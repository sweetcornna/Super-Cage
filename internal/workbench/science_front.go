package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/webfront"
)

// Claude Science phase 2a, the front (design §7.4 "Entry and front session"):
// a listener on 127.0.0.1 serving the daemon's web app on its own loopback
// origin http://localhost:Q, which the Windows shell frames inside the
// workbench. Only the Windows WebView2 shell may use it (see science_host.go).
//
// Contract (all on the front origin; exact Host "localhost:Q"):
//
//	GET  /_cage/enter#<ticket>   entry page; its script posts the ticket
//	POST /_cage/enter {ticket}    204 + probe cookie (Path=/_cage/, 2 min)
//	GET  /_cage/probe             identity probe (main, nested frame,
//	                              dedicated worker, blob worker)
//	POST /_cage/probe {reports}   204 + session cookie when every context
//	                              reports the guest's values; 403 + reason
//	POST /_cage/csp-report        CSP violation sink (counts only)
//	anything else                 the daemon, with the session cookie
const (
	scienceFrontFirstPort = 17840
	scienceFrontPortCount = 31
	scienceBodyLimit      = 8 << 20
	scienceProbeLife      = 2 * time.Minute
)

// listenScienceFront picks the port like listenController: the fixed range,
// then the remembered port, then any port (remembered for next time).
func listenScienceFront(profile string) (net.Listener, int, error) {
	for port := scienceFrontFirstPort; port < scienceFrontFirstPort+scienceFrontPortCount; port++ {
		if listener, err := listenLoopback(port); err == nil {
			return listener, port, nil
		}
	}
	path := filepath.Join(profile, "science-port.json")
	var record controllerPortRecord
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &record) == nil && record.Port >= 1024 && record.Port <= 65535 {
		if listener, err := listenLoopback(record.Port); err == nil {
			return listener, record.Port, nil
		}
	}
	listener, err := listenLoopback(0)
	if err != nil {
		return nil, 0, fmt.Errorf("无法在本机回环地址上监听 Claude Science 端口: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if port < 1024 {
		listener.Close()
		return nil, 0, fmt.Errorf("系统分配的 Claude Science 端口 %d 无效", port)
	}
	_ = writePrivate(path, controllerPortRecord{Port: port})
	return listener, port, nil
}

// scienceTicket is what an entry ticket was issued for.
type scienceTicket struct {
	epoch    context.Context
	identity scienceIdentity
	host     scienceHostIdentity
}

type scienceProbeSession struct {
	scienceTicket
	expires time.Time
}

// scienceFrontHooks connect the front to its workbench (tests replace them).
type scienceFrontHooks struct {
	// ready returns a Chinese reason while the environment or daemon is not
	// usable (409).
	ready func() error
	// ancestor is the origin allowed to frame the front (the desktop entry).
	ancestor func() string
	// upstream builds the upstream for the running daemon.
	upstream func() (*scienceUpstream, error)
	// signIn opens the daemon's sign-in in the guest browser (phase 1).
	signIn func() (claudeScienceTab, error)
}

type scienceFront struct {
	port     int
	origin   string
	host     string
	listener net.Listener
	server   *http.Server
	tickets  *webfront.Tickets[scienceTicket]
	hooks    scienceFrontHooks
	now      func() time.Time

	mu       sync.Mutex
	upstream *scienceUpstream
	probes   map[string]scienceProbeSession
	session  string
	current  scienceTicket
	audit    *scienceAudit
	state    scienceFrontState
	signing  time.Time
	// In-flight proxied requests (sockets and streams included) and the end
	// of the last one: an idle front forgets the daemon credential.
	active     int
	lastActive time.Time
}

// scienceIdleDrop is how long an idle front keeps the daemon credential. The
// session survives: the next request mints a new one.
const scienceIdleDrop = 2 * time.Minute

func (f *scienceFront) begin() {
	f.mu.Lock()
	f.active++
	f.mu.Unlock()
}

func (f *scienceFront) end() {
	f.mu.Lock()
	f.active--
	f.lastActive = f.now()
	f.mu.Unlock()
}

// dropIdle forgets the daemon credential when nothing has used it for
// scienceIdleDrop.
func (f *scienceFront) dropIdle() bool {
	f.mu.Lock()
	upstream := f.upstream
	idle := upstream != nil && f.active == 0 && f.now().Sub(f.lastActive) > scienceIdleDrop
	if idle {
		f.upstream = nil
	}
	f.mu.Unlock()
	if idle {
		upstream.close()
	}
	return idle
}

// scienceFrontState is what the pane may learn (status host_render fields).
type scienceFrontState struct {
	Refused      string               `json:"refused,omitempty"`
	RefusedAt    *time.Time           `json:"refused_at,omitempty"`
	Blocked      string               `json:"blocked,omitempty"`
	BlockedAt    *time.Time           `json:"blocked_at,omitempty"`
	BlockedCount int                  `json:"blocked_count,omitempty"`
	Audit        scienceAuditDistinct `json:"audit"`
	Session      bool                 `json:"session"`
	Transport    string               `json:"transport,omitempty"`
	SignIn       *scienceSignInEvent  `json:"signin,omitempty"`
	Violations   []scienceViolation   `json:"violations,omitempty"`
}

type scienceSignInEvent struct {
	At    time.Time         `json:"at"`
	Tab   *claudeScienceTab `json:"tab,omitempty"`
	Error string            `json:"error,omitempty"`
}

// scienceViolation is one CSP report, reduced to the directive and the
// blocked origin or scheme (never a path or query).
type scienceViolation struct {
	Directive string `json:"directive"`
	Blocked   string `json:"blocked"`
}

func newScienceFront(listener net.Listener, port int, hooks scienceFrontHooks) *scienceFront {
	f := &scienceFront{
		port: port, origin: "http://localhost:" + strconv.Itoa(port), host: "localhost:" + strconv.Itoa(port),
		listener: listener, hooks: hooks, now: time.Now, probes: map[string]scienceProbeSession{},
	}
	f.tickets = webfront.NewTickets[scienceTicket](60*time.Second, 16, func() time.Time { return f.now() })
	f.server = &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	return f
}

func (f *scienceFront) serve() { _ = f.server.Serve(f.listener) }

func (f *scienceFront) close() {
	_ = f.server.Close()
	f.reset("")
}

// reset drops every ticket, probe and session and the daemon credential.
// reason, when set, is shown to the pane as why the frame was closed.
func (f *scienceFront) reset(reason string) {
	f.tickets.Clear()
	f.mu.Lock()
	upstream := f.upstream
	f.upstream, f.session, f.current, f.audit = nil, "", scienceTicket{}, nil
	clear(f.probes)
	f.state.Session, f.state.Audit = false, scienceAuditDistinct{}
	if reason != "" {
		now := f.now()
		f.state.Refused, f.state.RefusedAt = reason, &now
	}
	f.mu.Unlock()
	if upstream != nil {
		upstream.close()
	}
}

// issue mints an entry ticket for a workbench session.
func (f *scienceFront) issue(ticket scienceTicket) string {
	f.mu.Lock()
	f.state.Refused, f.state.RefusedAt = "", nil
	f.mu.Unlock()
	return f.origin + "/_cage/enter#" + f.tickets.Issue(ticket)
}

func (f *scienceFront) snapshot() scienceFrontState {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.state
	state.Violations = append([]scienceViolation(nil), f.state.Violations...)
	if f.upstream != nil {
		state.Transport = f.upstream.dialer.Mode()
	}
	return state
}

func (f *scienceFront) cookieName() string { return "cage_science_" + strconv.Itoa(f.port) }
func (f *scienceFront) probeCookie() string {
	return "cage_science_probe_" + strconv.Itoa(f.port)
}

const sciencePermissionsPolicy = "accelerometer=(), autoplay=(), bluetooth=(), camera=(), display-capture=(), geolocation=(), gyroscope=(), hid=(), magnetometer=(), microphone=(), midi=(), payment=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), usb=(), xr-spatial-tracking=()"

// ancestors is the frame-ancestors source list: the workbench document,
// and the front itself for the app's own nested frames (every ancestor must
// match).
func (f *scienceFront) ancestors() string {
	if f.hooks.ancestor != nil {
		if origin := f.hooks.ancestor(); origin != "" {
			return "'self' " + origin
		}
	}
	return "'none'"
}

// lockdown is the policy appended to every daemon response. Policies
// intersect, so it only removes non-local sources from what the app needs.
func (f *scienceFront) lockdown() string {
	return "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' blob:; style-src 'self' 'unsafe-inline'; " +
		"connect-src 'self' ws://" + f.host + " blob: data:; img-src 'self' data: blob:; media-src 'self' data: blob:; font-src 'self' data:; " +
		"frame-src 'self'; child-src 'self' blob:; worker-src 'self' blob:; manifest-src 'self'; form-action 'self'; base-uri 'self'; object-src 'none'; " +
		"frame-ancestors " + f.ancestors() + "; report-uri /_cage/csp-report"
}

func (f *scienceFront) ownPolicy() string {
	return "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; worker-src 'self' blob:; frame-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors " + f.ancestors()
}

func sciencePlain(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, message+"\n")
}

// scienceDetail answers in the daemon's own error shape, which its app shows.
func scienceDetail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": message})
}

func (f *scienceFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-DNS-Prefetch-Control", "off")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Permissions-Policy", sciencePermissionsPolicy)
	if r.Host != f.host {
		sciencePlain(w, http.StatusForbidden, "Host refused")
		return
	}
	if !webfront.ValidPath(r.URL) || r.Method == http.MethodTrace || r.Method == http.MethodConnect {
		sciencePlain(w, http.StatusBadRequest, "请求路径无效")
		return
	}
	upgrade := webfront.IsUpgrade(r)
	unsafe := (r.Method != http.MethodGet && r.Method != http.MethodHead) || upgrade
	if unsafe && r.Header.Get("Origin") != f.origin && r.URL.Path != "/_cage/csp-report" {
		sciencePlain(w, http.StatusForbidden, "Origin refused")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_cage/") {
		header.Set("Content-Security-Policy", f.ownPolicy())
		header.Set("Cache-Control", "no-store")
		f.serveOwn(w, r)
		return
	}
	// Neither kind of worker is reached by the shell's per-target overrides.
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest == "serviceworker" || dest == "sharedworker" || r.Header.Get("Service-Worker") != "" {
		sciencePlain(w, http.StatusForbidden, "本机渲染不支持共享 Worker 与 Service Worker")
		return
	}
	current, ok := f.authorized(r)
	if !ok {
		sciencePlain(w, http.StatusUnauthorized, "本机渲染会话已失效，请回到工作台重新打开 Claude Science。")
		return
	}
	if upgrade && !webfront.ValidWebSocketUpgrade(r) {
		sciencePlain(w, http.StatusBadRequest, "协议升级无效")
		return
	}
	if f.hooks.ready != nil {
		if err := f.hooks.ready(); err != nil {
			sciencePlain(w, http.StatusConflict, err.Error())
			return
		}
	}
	if f.intercept(w, r) {
		return
	}
	f.proxy(w, r, current)
}

// authorized checks the session cookie (any of several with that name) and
// that the workbench session it came from is still unlocked.
func (f *scienceFront) authorized(r *http.Request) (scienceTicket, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session == "" || !webfront.CookieMatches(r, f.cookieName(), f.session) {
		return scienceTicket{}, false
	}
	if f.current.epoch == nil || f.current.epoch.Err() != nil {
		return scienceTicket{}, false
	}
	return f.current, true
}

var scienceSignInHidden = map[string]bool{"/oauth/code/callback": true, "/api/auth/callback": true, "/api/auth/exchange": true, "/api/auth/broker-exchange": true}

// intercept answers the routes that must never be served from the host.
func (f *scienceFront) intercept(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	switch {
	case path == "/api/auth/login":
		f.requestSignIn()
		if r.Header.Get("Sec-Fetch-Mode") == "navigate" || strings.Contains(r.Header.Get("Accept"), "text/html") {
			// Leaves the document in place.
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		scienceDetail(w, http.StatusConflict, "请在 Super Cage 隔离浏览器中完成登录：登录页已在浏览器窗格中打开。")
		return true
	case scienceSignInHidden[path]:
		http.NotFound(w, r)
		return true
	case path == "/daemon/shutdown" || path == "/daemon/apply-update" || path == "/daemon/check-update":
		scienceDetail(w, http.StatusForbidden, "请在 Super Cage 工作台中停止或更新 Claude Science。")
		return true
	case strings.HasPrefix(path, "/api/mcp-servers/") && strings.HasSuffix(path, "/oauth/authorize"):
		scienceDetail(w, http.StatusConflict, "请在 Super Cage 浏览器中连接：本机渲染暂不支持连接器授权。")
		return true
	}
	return false
}

func (f *scienceFront) requestSignIn() {
	f.mu.Lock()
	if now := f.now(); now.Sub(f.signing) < 10*time.Second {
		f.mu.Unlock()
		return
	} else {
		f.signing = now
	}
	f.mu.Unlock()
	if f.hooks.signIn == nil {
		return
	}
	go func() {
		tab, err := f.hooks.signIn()
		event := &scienceSignInEvent{At: f.now()}
		if err != nil {
			event.Error = err.Error()
		} else {
			event.Tab = &tab
		}
		f.mu.Lock()
		f.state.SignIn = event
		f.mu.Unlock()
	}()
}

func (f *scienceFront) block(category string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.state.Blocked, f.state.BlockedAt = category, &now
	f.state.BlockedCount++
}

// scienceRequestHeaders is the tenant gateway's request allow-list without
// Sec-WebSocket-Extensions (compressed frames could not be audited),
// Accept-Language and Content-Encoding (set or refused here), plus what the
// daemon's app sends of its own.
var scienceRequestHeaders = []string{
	"Accept", "Accept-Encoding", "Content-Type", "Range", "If-Match", "If-None-Match", "If-Range",
	"If-Modified-Since", "If-Unmodified-Since", "Last-Event-ID", "Connection", "Upgrade",
	"Sec-WebSocket-Key", "Sec-WebSocket-Protocol", "Sec-WebSocket-Version", "X-Requested-With", "X-Csrf-Token", "Cache-Control",
}

var scienceResponseHeaders = []string{
	"Content-Type", "Content-Encoding", "Content-Length", "Content-Disposition", "Accept-Ranges", "Content-Range",
	"Location", "Allow", "Retry-After", "Cache-Control", "Etag", "Last-Modified", "Expires", "Vary", "Pragma", "X-Accel-Buffering",
}

func scienceOutboundHeaders(in http.Header, identity scienceIdentity, upstreamOrigin string) http.Header {
	out := webfront.SelectHeaders(in, scienceRequestHeaders...)
	for name, values := range in {
		if strings.HasPrefix(name, "X-Operon-") {
			out[name] = append([]string(nil), values...)
		}
	}
	// The front already checked the browser's Origin; the daemon sees its own.
	if in.Get("Origin") != "" {
		out.Set("Origin", upstreamOrigin)
	}
	var csrf []string
	for _, value := range in.Values("Cookie") {
		for _, part := range strings.Split(value, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name == "operon_csrf" {
				csrf = append(csrf, strings.TrimSpace(part))
			}
		}
	}
	if len(csrf) > 0 {
		out.Set("Cookie", strings.Join(csrf, "; "))
	}
	out.Set("Accept-Language", identity.UpstreamAcceptLanguage)
	out.Set("User-Agent", identity.UserAgent)
	return out
}

// auditRequest scans the URL, the outbound header values and the body.
func auditRequest(audit *scienceAudit, target *url.URL, headers http.Header, body []byte) string {
	if category := audit.scan([]byte(target.RawPath + " " + target.Path + "?" + target.RawQuery)); category != "" {
		return category
	}
	for name, values := range headers {
		if name == "Accept-Language" || name == "User-Agent" {
			continue
		}
		for _, value := range values {
			if category := audit.scan([]byte(value)); category != "" {
				return category
			}
		}
	}
	return audit.scan(body)
}

func (f *scienceFront) upstreamFor() (*scienceUpstream, error) {
	f.mu.Lock()
	upstream := f.upstream
	f.mu.Unlock()
	if upstream != nil {
		return upstream, nil
	}
	if f.hooks.upstream == nil {
		return nil, errors.New("Claude Science 尚未启动")
	}
	created, err := f.hooks.upstream()
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upstream != nil {
		created.close()
		return f.upstream, nil
	}
	f.upstream = created
	return created, nil
}

func (f *scienceFront) proxy(w http.ResponseWriter, r *http.Request, current scienceTicket) {
	f.begin()
	defer f.end()
	body, code, err := readScienceBody(r, scienceBodyLimit)
	if err != nil {
		if code == 0 {
			code = http.StatusBadRequest
		}
		scienceDetail(w, code, err.Error())
		return
	}
	f.mu.Lock()
	audit := f.audit
	f.mu.Unlock()
	upstream, err := f.upstreamFor()
	if err != nil {
		sciencePlain(w, http.StatusBadGateway, err.Error())
		return
	}
	outHeaders := scienceOutboundHeaders(r.Header, current.identity, upstream.origin())
	if category := auditRequest(audit, r.URL, outHeaders, body); category != "" {
		f.block(category)
		scienceDetail(w, http.StatusForbidden, scienceAuditMessages[category])
		return
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	proxy := &httputil.ReverseProxy{
		Transport:     scienceProxyTransport{upstream: upstream, audit: audit, onHit: f.block},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errScienceBearer) {
				sciencePlain(w, http.StatusBadGateway, err.Error())
				return
			}
			sciencePlain(w, http.StatusBadGateway, "与环境内 Claude Science 的连接中断，请重试。")
		},
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme, p.Out.URL.Host = "http", upstream.hostPort()
			p.Out.Host = upstream.hostPort()
			upgrade := p.Out.Header.Get("Upgrade")
			connection := p.Out.Header.Get("Connection")
			p.Out.Header = outHeaders.Clone()
			if upgrade != "" {
				p.Out.Header.Set("Upgrade", upgrade)
				p.Out.Header.Set("Connection", connection)
			} else {
				p.Out.Header.Del("Upgrade")
				p.Out.Header.Del("Connection")
			}
			p.Out.Trailer = nil
			if body != nil {
				p.Out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			}
		},
		ModifyResponse: func(response *http.Response) error { return f.rewriteResponse(response, upstream) },
	}
	proxy.ServeHTTP(w, r)
}

type scienceProxyTransport struct {
	upstream *scienceUpstream
	audit    *scienceAudit
	onHit    func(string)
}

func (t scienceProxyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.upstream.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		stream, ok := response.Body.(io.ReadWriteCloser)
		if !ok {
			response.Body.Close()
			return nil, errors.New("upgrade without a stream")
		}
		response.Body = scienceSocket{ReadWriteCloser: stream, audit: &wsAudit{dst: stream, audit: t.audit, onHit: t.onHit}}
	}
	return response, nil
}

// rewriteResponse applies the response half of design §7.4.
func (f *scienceFront) rewriteResponse(response *http.Response, upstream *scienceUpstream) error {
	if response.StatusCode == http.StatusSwitchingProtocols {
		if !webfront.ValidSwitchingResponse(response.Request, response) {
			return errors.New("science protocol upgrade refused")
		}
		response.Header = webfront.SelectHeaders(response.Header, "Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Protocol")
		return nil
	}
	headers := webfront.SelectHeaders(response.Header, scienceResponseHeaders...)
	for _, cookie := range response.Cookies() {
		// The app may read its CSRF token; every credential cookie stays out.
		if cookie.Name == "operon_csrf" && cookie.Value != "" {
			kept := &http.Cookie{Name: cookie.Name, Value: cookie.Value, Path: "/", MaxAge: cookie.MaxAge, Expires: cookie.Expires, SameSite: http.SameSiteStrictMode}
			if value := kept.String(); value != "" {
				headers.Add("Set-Cookie", value)
			}
		}
	}
	if values := headers.Values("Location"); len(values) > 0 {
		location, err := url.Parse(values[0])
		if len(values) != 1 || err != nil {
			return errors.New("invalid science redirect")
		}
		location = response.Request.URL.ResolveReference(location)
		if location.Scheme != "http" || (location.Host != upstream.hostPort() && location.Host != "127.0.0.1:"+strconv.Itoa(upstream.port)) || location.User != nil || !webfront.ValidPath(location) {
			return errors.New("science redirect refused")
		}
		location.Scheme, location.Host = "http", f.host
		headers.Set("Location", location.String())
	}
	ancestors := strings.Fields(f.ancestors())
	for _, policy := range webfront.ParsePolicies(response.Header.Values("Content-Security-Policy")) {
		policy = policy.Without("report-uri", "report-to").Set("frame-ancestors", ancestors...)
		headers.Add("Content-Security-Policy", policy.String())
	}
	headers.Add("Content-Security-Policy", f.lockdown())
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("X-DNS-Prefetch-Control", "off")
	headers.Set("Cross-Origin-Resource-Policy", "same-origin")
	headers.Set("Permissions-Policy", sciencePermissionsPolicy)
	if response.StatusCode == http.StatusUnauthorized {
		f.mu.Lock()
		f.state.Session = f.session != ""
		f.mu.Unlock()
	}
	response.Header = headers
	return nil
}

// serveOwn answers /_cage/*: the entry, the probe and the report sink.
func (f *scienceFront) serveOwn(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		sciencePlain(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	path := r.URL.Path
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		page, kind, needsProbe := scienceOwnAssets[path], "", false
		switch {
		case strings.HasSuffix(path, ".js"):
			kind = "text/javascript; charset=utf-8"
		case strings.HasSuffix(path, ".css"):
			kind = "text/css; charset=utf-8"
		default:
			kind = "text/html; charset=utf-8"
		}
		needsProbe = strings.HasPrefix(path, "/_cage/probe")
		if page == "" {
			http.NotFound(w, r)
			return
		}
		if needsProbe {
			if _, ok := f.probeSession(r); !ok {
				sciencePlain(w, http.StatusUnauthorized, "身份核对已过期，请回到工作台重新打开 Claude Science。")
				return
			}
		}
		w.Header().Set("Content-Type", kind)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, page)
		}
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	switch path {
	case "/_cage/enter":
		f.enter(w, r)
	case "/_cage/probe":
		f.probe(w, r)
	case "/_cage/csp-report":
		f.cspReport(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *scienceFront) probeSession(r *http.Request) (scienceProbeSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for token, session := range f.probes {
		if !now.Before(session.expires) || session.epoch.Err() != nil {
			delete(f.probes, token)
		}
	}
	for _, value := range webfront.CookieValues(r, f.probeCookie()) {
		for token, session := range f.probes {
			if webfront.Equal(value, token) {
				return session, true
			}
		}
	}
	return scienceProbeSession{}, false
}

func (f *scienceFront) enter(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Ticket string `json:"ticket"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" || decoder.Decode(&input) != nil {
		sciencePlain(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	ticket, ok := f.tickets.Consume(input.Ticket)
	if !ok || ticket.epoch == nil || ticket.epoch.Err() != nil {
		sciencePlain(w, http.StatusUnauthorized, "入口已失效，请回到工作台重新打开 Claude Science。")
		return
	}
	token := webfront.Secret(32)
	f.mu.Lock()
	// A new entry revokes the session of any earlier document.
	f.session, f.current, f.audit = "", scienceTicket{}, nil
	f.state.Session = false
	clear(f.probes)
	f.probes[token] = scienceProbeSession{scienceTicket: ticket, expires: f.now().Add(scienceProbeLife)}
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: f.probeCookie(), Value: token, Path: "/_cage/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(scienceProbeLife.Seconds())})
	w.WriteHeader(http.StatusNoContent)
}

func (f *scienceFront) probe(w http.ResponseWriter, r *http.Request) {
	session, ok := f.probeSession(r)
	if !ok {
		sciencePlain(w, http.StatusUnauthorized, "身份核对已过期，请回到工作台重新打开 Claude Science。")
		return
	}
	var input struct {
		Reports []scienceProbeReport `json:"reports"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" || decoder.Decode(&input) != nil || len(input.Reports) > 8 {
		sciencePlain(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	reason := checkScienceProbe(session.identity, input.Reports, f.now())
	f.mu.Lock()
	for token, candidate := range f.probes {
		if candidate.expires == session.expires && candidate.epoch == session.epoch {
			delete(f.probes, token)
		}
	}
	if reason != "" {
		now := f.now()
		f.state.Refused, f.state.RefusedAt = "身份核对未通过："+reason+"。已改用隔离浏览器打开。", &now
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: f.probeCookie(), Value: "", Path: "/_cage/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		sciencePlain(w, http.StatusForbidden, reason)
		return
	}
	token := webfront.Secret(32)
	f.session, f.current = token, session.scienceTicket
	f.audit = newScienceAudit(session.host, session.identity, f.now())
	f.state.Session, f.state.Audit = true, f.audit.Distinct
	f.state.Refused, f.state.RefusedAt = "", nil
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: f.probeCookie(), Value: "", Path: "/_cage/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: f.cookieName(), Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (f *scienceFront) cspReport(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var report struct {
		Body struct {
			Effective string `json:"effective-directive"`
			Violated  string `json:"violated-directive"`
			Blocked   string `json:"blocked-uri"`
		} `json:"csp-report"`
	}
	if json.Unmarshal(data, &report) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	directive := report.Body.Effective
	if directive == "" {
		directive, _, _ = strings.Cut(report.Body.Violated, " ")
	}
	blocked := report.Body.Blocked
	if u, err := url.Parse(blocked); err == nil && u.Host != "" {
		blocked = u.Scheme + "://" + u.Host
	} else if i := strings.IndexAny(blocked, ":/"); i > 0 {
		blocked = blocked[:i]
	}
	if len(directive) > 64 || len(blocked) > 256 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.state.Violations = append(f.state.Violations, scienceViolation{Directive: directive, Blocked: blocked})
	if len(f.state.Violations) > 64 {
		f.state.Violations = f.state.Violations[len(f.state.Violations)-64:]
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
