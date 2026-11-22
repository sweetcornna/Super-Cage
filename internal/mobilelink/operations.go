// Package mobilelink connects a paired mobile client to the mobile
// operations of a single Cage host. It never exposes the full workbench API.
package mobilelink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxRequest  = 72 << 10
	maxResponse = 4 << 20
	linkOrigin  = "https://cage-mobile.invalid"

	// FrameMetaHeader carries the metadata of a binary frame reply as
	// unpadded base64url JSON next to an image/jpeg body. FrameStreamHeader is
	// "1" on frame replies of a host that also understands the negotiated read
	// (binary body, held read). Both are the only reply headers the link relays.
	FrameMetaHeader   = "X-Cage-Frame"
	FrameStreamHeader = "X-Cage-Frame-Stream"
	// MaxFrameMeta bounds the encoded metadata so the header list of one reply
	// stays far below the 16 KiB limit of the HTTP/2 link.
	MaxFrameMeta = 6 << 10
	maxFrameWait = 30000
)

var sessionPath = regexp.MustCompile(`^/api/mobile/cli/sessions/[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}(/messages|/media|/stop|/approval|/open|/metadata|/account|/delete|/archive|/restore|/purge)?$`)

var surfacePath = regexp.MustCompile(`^/api/mobile/(?:browser|desktop)/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}/(frame|action|close)$`)

var terminalPath = regexp.MustCompile(`^/api/mobile/terminals/[0-9a-f]{16}/(output|input|resize|close)$`)

var terminalAfter = regexp.MustCompile(`^after=(0|[1-9][0-9]{0,19})$`)

var epochID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var frameQuery = regexp.MustCompile(`^after=([0-9a-f]{32})$`)

// Held reads (?wait=) and the session list's revision (?after=); see the
// session list protocol in internal/workbench/mobile_cli_http.go.
var waitSeconds = regexp.MustCompile(`^[1-9][0-9]?$`)

var listRevision = regexp.MustCompile(`^[0-9a-f]{32}$`)

func sessionListQuery(query string) bool {
	fields := strings.Split(query, "&")
	if len(fields) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, field := range fields {
		key, value, _ := strings.Cut(field, "=")
		valid := key == "archived" && value == "true" || key == "after" && listRevision.MatchString(value) || key == "wait" && waitSeconds.MatchString(value)
		if !valid || seen[key] {
			return false
		}
		seen[key] = true
	}
	return !seen["wait"] || seen["after"]
}

var frameTag = regexp.MustCompile(`^[0-9a-f]{32}$`)

var frameWait = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

var frameMeta = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// FrameAfter accepts a single opaque frame tag in its canonical wire form.
func FrameAfter(query string) (string, bool) {
	match := frameQuery.FindStringSubmatch(query)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// FrameQuery is a parsed frame read. A bare "after=<tag>" is the original
// conditional read and keeps its meaning exactly; only a query that also
// carries wait or fmt negotiates anything new, so older hosts refuse it.
type FrameQuery struct {
	After  string        // opaque frame tag the phone already shows
	Wait   time.Duration // longest time the host may hold the read for a newer frame
	Binary bool          // answer with the JPEG as the body (fmt=jpeg)
	Stream bool          // wait or fmt was present
}

// ParseFrameQuery accepts each of after, wait (milliseconds) and fmt (jpeg)
// once, in any order, with canonical values only. A held read needs a tag.
func ParseFrameQuery(query string) (FrameQuery, bool) {
	var q FrameQuery
	if query == "" {
		return q, false
	}
	seen := map[string]bool{}
	for _, field := range strings.Split(query, "&") {
		key, value, ok := strings.Cut(field, "=")
		if !ok || seen[key] {
			return FrameQuery{}, false
		}
		seen[key] = true
		switch key {
		case "after":
			if !frameTag.MatchString(value) {
				return FrameQuery{}, false
			}
			q.After = value
		case "wait":
			ms, err := strconv.Atoi(value)
			if !frameWait.MatchString(value) || err != nil || ms > maxFrameWait {
				return FrameQuery{}, false
			}
			q.Wait = time.Duration(ms) * time.Millisecond
		case "fmt":
			if value != "jpeg" {
				return FrameQuery{}, false
			}
			q.Binary = true
		default:
			return FrameQuery{}, false
		}
	}
	q.Stream = seen["wait"] || seen["fmt"]
	if q.Wait > 0 && q.After == "" {
		return FrameQuery{}, false
	}
	return q, true
}

// ValidFrameMeta reports whether a metadata header value is usable: canonical
// base64url alphabet only, within the header budget.
func ValidFrameMeta(value string) bool {
	return len(value) > 0 && len(value) <= MaxFrameMeta && frameMeta.MatchString(value)
}

func allowed(method string, u *url.URL) bool {
	if u == nil || u.RawPath != "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.ForceQuery {
		return false
	}
	if u.Path == "/api/mobile/cli/sessions" && u.RawQuery != "" {
		return method == http.MethodGet && sessionListQuery(u.RawQuery)
	}
	if u.RawQuery == "" {
		switch u.Path {
		case "/api/mobile/browser/open", "/api/mobile/desktop/open", "/api/mobile/device",
			"/api/mobile/environment/start", "/api/mobile/environment/stop", "/api/mobile/environment/restart",
			"/api/mobile/environment/proxy/select", "/api/mobile/environment/proxy/delay",
			"/api/mobile/files/list", "/api/mobile/files/read", "/api/mobile/files/mkdir",
			"/api/mobile/files/upload/start", "/api/mobile/files/upload/chunk", "/api/mobile/files/upload/finish", "/api/mobile/files/upload/abort":
			return method == http.MethodPost
		case "/api/mobile/cli/projects", "/api/mobile/environment", "/api/mobile/environment/proxy":
			return method == http.MethodGet
		case "/api/mobile/cli/sessions", "/api/mobile/browser/favorites", "/api/mobile/terminals":
			return method == http.MethodGet || method == http.MethodPost
		}
	}
	if match := terminalPath.FindStringSubmatch(u.Path); match != nil {
		if match[1] == "output" {
			after := terminalAfter.FindStringSubmatch(u.RawQuery)
			if after == nil || method != http.MethodGet {
				return false
			}
			_, err := strconv.ParseUint(after[1], 10, 64)
			return err == nil
		}
		return method == http.MethodPost && u.RawQuery == ""
	}
	if match := surfacePath.FindStringSubmatch(u.Path); match != nil {
		if u.RawQuery == "" {
			return (match[1] == "frame" && method == http.MethodGet) || (match[1] != "frame" && method == http.MethodPost)
		}
		q, ok := ParseFrameQuery(u.RawQuery)
		// The bare conditional read only ever existed for the browser. The
		// negotiated forms are new and may be used by both surfaces.
		return ok && match[1] == "frame" && method == http.MethodGet &&
			(q.Stream || strings.HasPrefix(u.Path, "/api/mobile/browser/"))
	}
	match := sessionPath.FindStringSubmatch(u.Path)
	if match == nil {
		return false
	}
	if match[1] != "" {
		return method == http.MethodPost && u.RawQuery == ""
	}
	if method != http.MethodGet {
		return false
	}
	if u.RawQuery == "" {
		return true
	}
	fields := strings.Split(u.RawQuery, "&")
	if len(fields) > 3 {
		return false
	}
	var after string
	var hasEpoch, hasWait bool
	for _, field := range fields {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "wait":
			if hasWait || !waitSeconds.MatchString(value) {
				return false
			}
			hasWait = true
		case "after":
			if after != "" || value == "" {
				return false
			}
			after = value
		case "epoch":
			if hasEpoch || !epochID.MatchString(value) {
				return false
			}
			hasEpoch = true
		default:
			return false
		}
	}
	if after == "" {
		return false
	}
	for _, digit := range after {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(after, 10, 64)
	return err == nil
}

// Reply carries only application data. Browser cookies, redirects, hop headers
// and upstream authentication fields never cross the device link.
type Reply struct {
	Status      int
	ContentType string
	Body        []byte
	// Header holds only FrameMetaHeader and FrameStreamHeader, both validated.
	Header http.Header
}

type performFunc func(context.Context, string, string, []byte) (*Reply, error)

type transportError struct{ err error }

func (e transportError) Error() string { return e.err.Error() }
func (e transportError) Unwrap() error { return e.err }

// IsTransportError reports whether err indicates a broken underlying transport
// rather than an application-level rejection, unexpected status, or size limit.
func IsTransportError(err error) bool {
	var target transportError
	return errors.As(err, &target)
}

func limited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, transportError{errors.New("payload could not be read")}
	}
	if int64(len(data)) > limit {
		return nil, errors.New("payload exceeds limit")
	}
	return data, nil
}

// Operations serves the mobile operation allowlist through client, as the
// phone's local bridge does. A desktop that opened a joined computer uses it
// for that computer's link; the caller authenticates the window first.
func Operations(client OperationClient) http.Handler { return operations(client.Do) }

// operations checks the method and route before reading data. Callers establish
// the peer identity or the local browser session before invoking this handler.
func operations(do performFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !allowed(r.Method, r.URL) {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if kind != "application/json" {
				http.Error(w, "JSON is required", http.StatusUnsupportedMediaType)
				return
			}
		}
		body, err := limited(r.Body, maxRequest)
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Method == http.MethodGet && len(body) != 0 {
			http.Error(w, "GET must not have a body", http.StatusBadRequest)
			return
		}
		reply, err := do(r.Context(), r.Method, r.URL.RequestURI(), body)
		if err != nil {
			http.Error(w, "与电脑的连接已中断。请检查手机网络和电脑上的 Super Cage，再重新读取会话。", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", reply.ContentType)
		for _, name := range []string{FrameMetaHeader, FrameStreamHeader} {
			if value := reply.Header.Get(name); value != "" {
				w.Header().Set(name, value)
			}
		}
		w.WriteHeader(reply.Status)
		if reply.Status != http.StatusNoContent {
			_, _ = w.Write(reply.Body)
		}
	})
}

func request(ctx context.Context, client http.RoundTripper, origin, method, path string, body []byte, localOrigin bool) (*Reply, error) {
	u, err := url.ParseRequestURI(path)
	if err != nil || u.IsAbs() || u.Host != "" || !allowed(method, u) || len(body) > maxRequest || (method == http.MethodGet && len(body) > 0) {
		return nil, errors.New("mobile operation refused")
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid mobile operation")
	}
	req.GetBody = nil // A mutation with a lost response is never replayed here.
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if localOrigin {
		req.Header.Set("Origin", origin)
	}
	res, err := client.RoundTrip(req)
	if err != nil {
		return nil, transportError{errors.New("mobile operation interrupted")}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || (res.StatusCode >= 300 && res.StatusCode < 400) {
		return nil, errors.New("unexpected mobile response status")
	}
	data, err := limited(res.Body, maxResponse)
	if err != nil {
		return nil, err
	}
	contentType := "text/plain; charset=utf-8"
	var header http.Header
	frame := surfacePath.MatchString(u.Path) && strings.HasSuffix(u.Path, "/frame")
	switch kind, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); {
	case kind == "application/json":
		contentType = "application/json"
	case kind == "image/jpeg" && frame && res.StatusCode == http.StatusOK:
		// A binary frame is only meaningful with its metadata. Anything else
		// claiming to be an image stays opaque text, as before.
		meta := res.Header.Get(FrameMetaHeader)
		if !ValidFrameMeta(meta) {
			return nil, errors.New("invalid frame reply")
		}
		contentType = "image/jpeg"
		header = http.Header{FrameMetaHeader: {meta}}
	}
	if frame && res.StatusCode == http.StatusOK && res.Header.Get(FrameStreamHeader) == "1" {
		if header == nil {
			header = http.Header{}
		}
		header.Set(FrameStreamHeader, "1")
	}
	return &Reply{Status: res.StatusCode, ContentType: contentType, Body: data, Header: header}, nil
}
