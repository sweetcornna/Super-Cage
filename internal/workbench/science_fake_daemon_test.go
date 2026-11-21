package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/webfront"
)

// fakeScienceDaemon replicates what design §2.1 lists as verified internals
// of the Claude Science daemon (0.1.72/0.1.73, read from the binary):
// the Host and Origin gates, the nonce -> operon_auth exchange, header or
// cookie bearer auth, the SPA document's CSP (nonce'd inline script,
// frame-ancestors 'none') with X-Frame-Options DENY and a report-only
// policy, and the /api/ws socket. Nothing here met the real daemon.
type fakeScienceDaemon struct {
	server   *httptest.Server
	mu       sync.Mutex
	nonces   map[string]bool
	tokens   map[string]bool
	requests []fakeScienceRequest
	results  []json.RawMessage
	frames   []string
	exchange int
	// page is the HTML body served at / (the fixture under test).
	page   func(nonce string) string
	assets map[string]string
	// rootHeaders replace the document's caching headers (a hostile daemon).
	rootHeaders map[string]string
	// refuseAll: every bearer is refused (a daemon that rejects the front).
	refuseAll bool
}

type fakeScienceRequest struct {
	Method, Path, Host, Origin, Authorization, Cookie, UserAgent, AcceptLanguage string
	Forwarded                                                                    bool
	Header                                                                       http.Header
}

var (
	fakeScienceHost   = regexp.MustCompile(`^(127\.0\.0\.1|localhost)(:\d+)?$`)
	fakeScienceOrigin = regexp.MustCompile(`^https?://(127\.0\.0\.1|localhost)(:\d+)?$`)
)

const fakeScienceSPAPolicy = "default-src 'self'; script-src 'self' 'nonce-%s' 'wasm-unsafe-eval' 'unsafe-eval' blob:; connect-src 'self' blob: data: ws: wss: https: http:; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https: http:; font-src 'self' data:; media-src 'self' blob: data:; worker-src 'self' blob:; frame-src 'self' http://localhost:4010 https://*.claudemcpcontent.com; manifest-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

func newFakeScienceDaemon(t *testing.T) *fakeScienceDaemon {
	t.Helper()
	d := &fakeScienceDaemon{nonces: map[string]bool{}, tokens: map[string]bool{}, assets: map[string]string{}}
	d.page = func(nonce string) string {
		return "<!doctype html><title>Claude Science</title><script nonce=\"" + nonce + "\">window.inlineRan = true;</script><p>fake</p>"
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || fakeScienceOrigin.MatchString(origin)
	}}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.requests = append(d.requests, fakeScienceRequest{
			Method: r.Method, Path: r.URL.Path, Host: r.Host, Origin: r.Header.Get("Origin"), Authorization: r.Header.Get("Authorization"),
			Cookie: r.Header.Get("Cookie"), UserAgent: r.Header.Get("User-Agent"), AcceptLanguage: r.Header.Get("Accept-Language"),
			Forwarded: r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Forwarded") != "",
			Header:    r.Header.Clone(),
		})
		d.mu.Unlock()
		if !fakeScienceHost.MatchString(r.Host) {
			http.Error(w, "host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !fakeScienceOrigin.MatchString(origin) {
				http.Error(w, "origin", http.StatusForbidden)
				return
			}
		}
		if r.URL.Path == "/api/auth/nonce" && r.Method == http.MethodPost {
			_ = r.ParseForm()
			d.mu.Lock()
			valid := d.nonces[r.PostForm.Get("nonce")]
			delete(d.nonces, r.PostForm.Get("nonce"))
			token := webfront.Secret(24)
			if valid {
				d.tokens[token] = true
				d.exchange++
			}
			d.mu.Unlock()
			if !valid {
				http.Error(w, "nonce", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "operon_auth", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.SetCookie(w, &http.Cookie{Name: "operon_csrf", Value: "csrf-" + token[:6], Path: "/"})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if !d.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/":
			nonce := webfront.Secret(12)
			w.Header().Set("Content-Security-Policy", fmt.Sprintf(fakeScienceSPAPolicy, nonce))
			w.Header().Add("Content-Security-Policy-Report-Only", "connect-src 'self' https://api.anthropic.com; report-uri https://reports.example/csp")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Report-To", "{\"url\":\"https://reports.example/r\"}")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			for name, value := range d.rootHeaders {
				w.Header().Set(name, value)
			}
			_, _ = io.WriteString(w, d.page(nonce))
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			body, ok := d.assets[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			kind := "text/javascript; charset=utf-8"
			if strings.HasSuffix(r.URL.Path, ".css") {
				kind = "text/css; charset=utf-8"
			} else if strings.HasSuffix(r.URL.Path, ".wasm") {
				kind = "application/wasm"
			} else if strings.HasSuffix(r.URL.Path, ".html") {
				kind = "text/html; charset=utf-8"
			}
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			_, _ = io.WriteString(w, body)
		case r.URL.Path == "/api/auth/status":
			http.SetCookie(w, &http.Cookie{Name: "operon_session", Value: "jwt-secret", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "operon_csrf", Value: "csrf-status", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"authenticated":false}`)
		case r.URL.Path == "/api/echo":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case r.URL.Path == "/api/test/results":
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			d.mu.Lock()
			d.results = append(d.results, json.RawMessage(body))
			d.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: tick-%d\n\n", i)
				flusher.Flush()
				time.Sleep(20 * time.Millisecond)
			}
		case r.URL.Path == "/redirect-internal":
			http.Redirect(w, r, "http://localhost:47800/next?x=1", http.StatusFound)
		case r.URL.Path == "/redirect-external":
			http.Redirect(w, r, "https://claude.ai/oauth", http.StatusFound)
		case r.URL.Path == "/api/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				d.mu.Lock()
				d.frames = append(d.frames, string(data))
				d.mu.Unlock()
				if conn.WriteMessage(kind, data) != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(d.server.Close)
	return d
}

func (d *fakeScienceDaemon) authorized(r *http.Request) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refuseAll {
		return false
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && d.tokens[token] {
		return true
	}
	if c, err := r.Cookie("operon_auth"); err == nil && d.tokens[c.Value] {
		return true
	}
	return false
}

// link mints what "claude-science url" prints.
func (d *fakeScienceDaemon) link(context.Context) (string, error) {
	nonce := webfront.Secret(16)
	d.mu.Lock()
	d.nonces[nonce] = true
	d.mu.Unlock()
	return "http://localhost:47800/?nonce=" + nonce, nil
}

// rotate forgets every bearer, as a daemon restart does.
func (d *fakeScienceDaemon) rotate() {
	d.mu.Lock()
	clear(d.tokens)
	d.mu.Unlock()
}

func (d *fakeScienceDaemon) seen() []fakeScienceRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]fakeScienceRequest(nil), d.requests...)
}

func (d *fakeScienceDaemon) bearers() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var tokens []string
	for token := range d.tokens {
		tokens = append(tokens, token)
	}
	return tokens
}

// tcpDialer dials the fake daemon directly (the SSH carriage is tested
// separately in science_ssh_test.go).
type tcpDialer struct{ address string }

func (d tcpDialer) DialContext(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.address)
}
func (tcpDialer) Mode() string { return "test" }
func (tcpDialer) Close() error { return nil }

var testScienceIdentity = scienceIdentity{
	Timezone: "Asia/Tokyo", Locale: "ja-JP", Language: "ja-JP", Languages: []string{"ja-JP"},
	AcceptLanguage: "ja-JP", UpstreamAcceptLanguage: "ja-JP,ja;q=0.9",
	UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36",
	Platform:  "Linux x86_64",
	Metadata:  scienceUAMetadata{Platform: "Linux", Architecture: "x86", Bitness: "64", Brands: []scienceUABrand{{Brand: "Chromium", Version: "157"}}, FullVersionList: []scienceUABrand{}},
}

func testHostIdentity() scienceHostIdentity {
	offset, _ := jsOffset("America/Los_Angeles", time.Now())
	return scienceHostIdentity{Timezone: "America/Los_Angeles", Offset: &offset, Languages: []string{"fr-FR", "fr"}}
}

func goodProbeReports(identity scienceIdentity) []scienceProbeReport {
	offset, _ := jsOffset(identity.Timezone, time.Now())
	platform := identity.Metadata.Platform
	var reports []scienceProbeReport
	for _, ctx := range scienceProbeContexts {
		reports = append(reports, scienceProbeReport{Context: ctx, Timezone: identity.Timezone, Offset: offset, Language: identity.Language, Languages: identity.Languages, UserAgent: identity.UserAgent, Platform: identity.Platform, UAPlatform: &platform})
	}
	none := 0
	reports[0].RTC = &none
	return reports
}

type testScienceFront struct {
	front   *scienceFront
	daemon  *fakeScienceDaemon
	signins int
	mu      sync.Mutex
	epoch   context.Context
	lock    context.CancelFunc
	// label is the last entry's (the session's once entered).
	label string
}

// newTestScienceFront runs a real front on a loopback port in front of a
// fake daemon. The workbench (ancestor) is http://localhost:17777.
func newTestScienceFront(t *testing.T) *testScienceFront {
	t.Helper()
	return newTestScienceFrontAt(t, 0)
}

// newTestScienceFrontAt listens on the given loopback port (0: any).
func newTestScienceFrontAt(t *testing.T, port int) *testScienceFront {
	t.Helper()
	daemon := newFakeScienceDaemon(t)
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	port = listener.Addr().(*net.TCPAddr).Port
	env := &testScienceFront{daemon: daemon}
	env.epoch, env.lock = context.WithCancel(context.Background())
	t.Cleanup(env.lock)
	address := strings.TrimPrefix(daemon.server.URL, "http://")
	env.front = newScienceFront(listener, port, scienceFrontHooks{
		ancestor: func() string { return "http://localhost:17777" },
		upstream: func() (*scienceUpstream, error) {
			return newScienceUpstream(tcpDialer{address: address}, 47800, daemon.link), nil
		},
		signIn: func() (claudeScienceTab, error) {
			env.mu.Lock()
			env.signins++
			env.mu.Unlock()
			return claudeScienceTab{ID: "tab-1", Kind: "science", Title: "Claude Science"}, nil
		},
	})
	go env.front.serve()
	t.Cleanup(env.front.close)
	return env
}

// base is the origin of the last entry's label.
func (env *testScienceFront) base() string { return env.front.labelOrigin(env.label) }

// host is its Host.
func (env *testScienceFront) host() string { return strings.TrimPrefix(env.base(), "http://") }

// sessionOrigin is the origin the frame's requests carry.
func (env *testScienceFront) sessionOrigin() string { return env.base() }

// scienceTestFrameSources is the workbench's frame-src for this front.
func scienceTestFrameSources(env *testScienceFront) string { return scienceFrameSource(env.front.port) }

// dial reaches the front on 127.0.0.1 for any name of its port.
func (env *testScienceFront) dial(ctx context.Context, network, address string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil || port != fmt.Sprint(env.front.port) {
		return nil, fmt.Errorf("test client refused %s", address)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp4", fmt.Sprintf("127.0.0.1:%d", env.front.port))
}

// client is a browser stand-in without cookies (the front uses none).
func (env *testScienceFront) client(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{DialContext: env.dial}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (env *testScienceFront) do(t *testing.T, client *http.Client, method, path, origin, body string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, env.base()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ticket issues an entry and makes its label the one requests use.
func (env *testScienceFront) ticket(t *testing.T) string {
	t.Helper()
	entry := env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()})
	parsed, _ := url.Parse(entry)
	match := scienceLabelHost.FindStringSubmatch(parsed.Host)
	if parsed.Path != "/_cage/enter" || parsed.Scheme != "http" || parsed.Fragment != "" || match == nil || match[2] != fmt.Sprint(env.front.port) {
		t.Fatalf("entry URL: %s", entry)
	}
	env.label = match[1]
	return env.label
}

// enter performs the entry and the probe as the frame would, returning a
// client and the probe's status code; env.label is then the session's.
func (env *testScienceFront) enter(t *testing.T, reports []scienceProbeReport) (*http.Client, int) {
	t.Helper()
	client := env.client(t)
	env.ticket(t)
	if r := env.do(t, client, "POST", "/_cage/enter", env.base(), "{}", map[string]string{"Content-Type": "application/json"}); r.StatusCode != 204 {
		t.Fatalf("enter: %d %s", r.StatusCode, readBody(t, r))
	}
	payload, _ := json.Marshal(map[string]any{"reports": reports})
	r := env.do(t, client, "POST", "/_cage/probe", env.base(), string(payload), map[string]string{"Content-Type": "application/json"})
	return client, r.StatusCode
}
