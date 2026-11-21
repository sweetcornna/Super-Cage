package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sylvinus/agent-vm/internal/webfront"
)

// Claude Science phase 2a, the front (design §7.4 "Entry and front session"):
// a listener on loopback serving the daemon's web app, which the Windows
// shell frames inside the workbench. Only the Windows WebView2 shell may use
// it (see science_host.go).
//
// Every session has its own origin, http://<label>.localhost:Q, where label
// is 128 random bits (32 hex digits) issued with the entry. The label is the
// session's only secret: no cookie is ever set, read or forwarded by the
// front. A new origin per session also gives each session its own storage,
// cache partition, site and renderer process: nothing one session's page
// leaves behind (or another environment's front on the same port) is seen
// by the next. The browser resolves *.localhost itself (no DNS) to 127.0.0.1
// and ::1; the front listens on both.
//
// Contract (exact Host "<label>.localhost:Q" for a label that is issued,
// entered or the active session; anything else 403):
//
//	GET  /_cage/enter            entry page while the label is issued (60 s);
//	                             Clear-Site-Data "cache", "storage"
//	POST /_cage/enter            204; the label enters its identity check
//	                             (2 min) and any earlier session ends
//	GET  /_cage/probe            identity probe (main, nested frame,
//	                             dedicated worker, blob worker)
//	POST /_cage/probe {reports}  204 and the label becomes the session when
//	                             every context reports the guest's values;
//	                             403 + reason
//	POST /_cage/csp-report       CSP violation sink (counts only; the label
//	                             must be checking or the session)
//	anything else                the daemon, for the session's label
//
// Unsafe requests and WebSocket upgrades need Origin http://<label>.localhost:Q.
// Sec-Fetch-Site, when sent, must be same-origin, except the frame's first
// navigation to /_cage/enter (cross-site, from the workbench document).
const (
	scienceFrontFirstPort = 17840
	scienceFrontPortCount = 31
	scienceBodyLimit      = 8 << 20
	scienceProbeLife      = 2 * time.Minute
	scienceEntryLife      = 60 * time.Second
	// The browser keeps up to 180 cookies of 4 KiB per host; the page can
	// fill its own origin's jar, and the front still reads every request
	// (it reads no cookie).
	scienceFrontHeaderLimit = 1 << 20
	// Per session: open sockets, and request body bytes held at once.
	scienceSocketCap  = 32
	scienceBodyBudget = 4 * scienceBodyLimit
)

// scienceLabelHost matches a session host; the port is checked separately.
var scienceLabelHost = regexp.MustCompile(`^([0-9a-f]{32})\.localhost:([0-9]{1,5})$`)

// scienceFrameSource is the workbench's frame-src for a front on port.
func scienceFrameSource(port int) string { return "http://*.localhost:" + strconv.Itoa(port) }

// listenScienceFront picks the port like listenController: the fixed range,
// then the remembered port, then any port (remembered for next time). The
// port must be free on 127.0.0.1 and on ::1: the browser may try either for
// a *.localhost name, and whoever listens on the other would get the label.
func listenScienceFront(profile string) (net.Listener, int, error) {
	for port := scienceFrontFirstPort; port < scienceFrontFirstPort+scienceFrontPortCount; port++ {
		if listener, err := listenScienceLoopback(port); err == nil {
			return listener, port, nil
		}
	}
	path := filepath.Join(profile, "science-port.json")
	var record controllerPortRecord
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &record) == nil && record.Port >= 1024 && record.Port <= 65535 {
		if listener, err := listenScienceLoopback(record.Port); err == nil {
			return listener, record.Port, nil
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		probe, err := listenLoopback(0)
		if err != nil {
			return nil, 0, fmt.Errorf("无法在本机回环地址上监听 Claude Science 端口: %w", err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		probe.Close()
		if port < 1024 {
			continue
		}
		if listener, err := listenScienceLoopback(port); err == nil {
			_ = writePrivate(path, controllerPortRecord{Port: port})
			return listener, port, nil
		}
	}
	return nil, 0, errors.New("无法在本机回环地址上找到 Claude Science 可用的端口")
}

// listenScienceLoopback listens on 127.0.0.1:port and, where the host has
// IPv6 loopback, on [::1]:port as well.
func listenScienceLoopback(port int) (net.Listener, error) {
	v4, err := listenLoopback(port)
	if err != nil {
		return nil, err
	}
	v6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			v4.Close()
			return nil, err
		}
		// No IPv6 loopback: the browser cannot reach ::1 either.
		return v4, nil
	}
	return newDualListener(v4, v6), nil
}

// dualListener accepts from both loopback listeners.
type dualListener struct {
	listeners []net.Listener
	conns     chan net.Conn
	done      chan struct{}
	once      sync.Once
}

func newDualListener(listeners ...net.Listener) *dualListener {
	d := &dualListener{listeners: listeners, conns: make(chan net.Conn), done: make(chan struct{})}
	for _, listener := range listeners {
		go func(listener net.Listener) {
			for {
				conn, err := listener.Accept()
				if err != nil {
					d.Close()
					return
				}
				select {
				case d.conns <- conn:
				case <-d.done:
					conn.Close()
					return
				}
			}
		}(listener)
	}
	return d
}

func (d *dualListener) Accept() (net.Conn, error) {
	select {
	case conn := <-d.conns:
		return conn, nil
	case <-d.done:
		return nil, net.ErrClosed
	}
}

func (d *dualListener) Close() error {
	d.once.Do(func() {
		close(d.done)
		for _, listener := range d.listeners {
			listener.Close()
		}
	})
	return nil
}

func (d *dualListener) Addr() net.Addr { return d.listeners[0].Addr() }

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
	listener net.Listener
	server   *http.Server
	hooks    scienceFrontHooks
	now      func() time.Time

	mu       sync.Mutex
	upstream *scienceUpstream
	// issued labels wait for their entry; checking labels run the identity
	// check; session is the label whose page may reach the daemon.
	issued   map[string]scienceProbeSession
	checking map[string]scienceProbeSession
	session  string
	current  scienceTicket
	jar      scienceJar
	audit    *scienceAudit
	state    scienceFrontState
	signing  time.Time
	// In-flight proxied requests (sockets and streams included) and the end
	// of the last one: an idle front forgets the daemon credential.
	active     int
	lastActive time.Time
	lastHeard  time.Time
	// bodies is the request body bytes held now (scienceBodyBudget);
	// sockets the open upgraded connections (scienceSocketCap).
	bodies  int64
	sockets int
	// generation changes on every reset; done ends what a generation
	// started (requests, sockets, credential).
	generation uint64
	done       context.Context
	cancel     context.CancelFunc
	// wake starts the watchdog when a session or credential appears.
	wake chan struct{}
}

// wakeWatch tells the watchdog there is something to watch.
func (f *scienceFront) wakeWatch() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// scienceIdleDrop is how long an idle front keeps the daemon credential. The
// session survives: the next request mints a new one.
const scienceIdleDrop = 2 * time.Minute

// scienceOrphanAfter: the pane polls every two seconds while its frame
// shows; a minimized window's timers may run once a minute.
const scienceOrphanAfter = 90 * time.Second

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

// heard records the pane's poll (or its ticket): its frame is still open.
func (f *scienceFront) heard() {
	f.mu.Lock()
	f.lastHeard = f.now()
	f.mu.Unlock()
}

// dropOrphan ends the session and its credential when neither the pane nor
// the frame's page has been heard from for scienceOrphanAfter: the window
// switched to another environment (whose workload now gets the pane's
// polls), closed, or lost its renderer. Nothing on this workload sees a
// switch happen; silence is the signal.
func (f *scienceFront) dropOrphan() bool {
	f.mu.Lock()
	last := f.lastHeard
	if f.lastActive.After(last) {
		last = f.lastActive
	}
	orphan := f.session != "" && f.active == 0 && f.now().Sub(last) > scienceOrphanAfter
	f.mu.Unlock()
	if orphan {
		f.reset("")
	}
	return orphan
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
	Refused   string     `json:"refused,omitempty"`
	RefusedAt *time.Time `json:"refused_at,omitempty"`
	// The audit reports (F6): the last category a request or socket frame
	// carried, when, and how many so far. Nothing is refused for it.
	AuditHit   string               `json:"audit_hit,omitempty"`
	AuditHitAt *time.Time           `json:"audit_hit_at,omitempty"`
	AuditHits  int                  `json:"audit_hits,omitempty"`
	Audit      scienceAuditDistinct `json:"audit"`
	Session    bool                 `json:"session"`
	Transport  string               `json:"transport,omitempty"`
	SignIn     *scienceSignInEvent  `json:"signin,omitempty"`
	Violations []scienceViolation   `json:"violations,omitempty"`
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
		port: port, listener: listener, hooks: hooks, now: time.Now,
		issued: map[string]scienceProbeSession{}, checking: map[string]scienceProbeSession{},
		wake: make(chan struct{}, 1),
	}
	f.done, f.cancel = context.WithCancel(context.Background())
	f.server = &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: scienceFrontHeaderLimit, ErrorLog: log.New(io.Discard, "", 0)}
	return f
}

// labelOrigin is the origin of a session label.
func (f *scienceFront) labelOrigin(label string) string {
	return "http://" + label + ".localhost:" + strconv.Itoa(f.port)
}

// requestLabel is the label of the request's exact Host, or "" when the
// Host is not a label host of this front's port.
func (f *scienceFront) requestLabel(r *http.Request) string {
	match := scienceLabelHost.FindStringSubmatch(r.Host)
	if match == nil || match[2] != strconv.Itoa(f.port) {
		return ""
	}
	return match[1]
}

// labelState says what a label may do now: "issued", "checking", "session"
// or "" (unknown, expired or revoked). Expired entries are dropped.
func (f *scienceFront) labelState(label string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for known, entry := range f.issued {
		if !now.Before(entry.expires) || entry.epoch.Err() != nil {
			delete(f.issued, known)
		}
	}
	for known, entry := range f.checking {
		if !now.Before(entry.expires) || entry.epoch.Err() != nil {
			delete(f.checking, known)
		}
	}
	switch {
	case label == "":
		return ""
	case f.session != "" && webfront.Equal(label, f.session):
		return "session"
	}
	if _, ok := f.checking[label]; ok {
		return "checking"
	}
	if _, ok := f.issued[label]; ok {
		return "issued"
	}
	return ""
}

func (f *scienceFront) serve() { _ = f.server.Serve(f.listener) }

func (f *scienceFront) close() {
	_ = f.server.Close()
	f.reset("")
}

// reset drops every ticket, probe and session and the daemon credential.
// reason, when set, is shown to the pane as why the frame was closed.
func (f *scienceFront) reset(reason string) {
	f.mu.Lock()
	upstream := f.upstream
	f.upstream, f.session, f.current, f.audit, f.jar = nil, "", scienceTicket{}, nil, scienceJar{}
	clear(f.issued)
	clear(f.checking)
	// Requests and sockets of the old generation end with it (F9).
	f.cancel()
	f.generation++
	f.done, f.cancel = context.WithCancel(context.Background())
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

// resetGeneration resets only if no reset happened since generation.
func (f *scienceFront) resetGeneration(generation uint64, reason string) {
	f.mu.Lock()
	current := f.generation == generation
	f.mu.Unlock()
	if current {
		f.reset(reason)
	}
}

// issue mints an entry for a workbench session: a new label, usable for
// scienceEntryLife to enter (once).
func (f *scienceFront) issue(ticket scienceTicket) string {
	label := webfront.Secret(16)
	f.mu.Lock()
	// The pane waits for the session of the document this entry opens.
	f.state.Refused, f.state.RefusedAt, f.state.Session = "", nil, false
	if len(f.issued) >= 16 {
		clear(f.issued)
	}
	f.issued[label] = scienceProbeSession{scienceTicket: ticket, expires: f.now().Add(scienceEntryLife)}
	f.mu.Unlock()
	return f.labelOrigin(label) + "/_cage/enter"
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
func (f *scienceFront) lockdown(host string) string {
	return "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' blob:; style-src 'self' 'unsafe-inline'; " +
		"connect-src 'self' ws://" + host + " blob: data:; img-src 'self' data: blob:; media-src 'self' data: blob:; font-src 'self' data:; " +
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
	header.Set("Cache-Control", "no-store")
	// The exact Host names a label this front issued: DNS rebinding cannot
	// produce it (no DNS name resolves to *.localhost but itself).
	label := f.requestLabel(r)
	state := f.labelState(label)
	if state == "" {
		sciencePlain(w, http.StatusForbidden, "Host refused")
		return
	}
	if !webfront.ValidPath(r.URL) || r.Method == http.MethodTrace || r.Method == http.MethodConnect {
		sciencePlain(w, http.StatusBadRequest, "请求路径无效")
		return
	}
	upgrade := webfront.IsUpgrade(r)
	unsafe := (r.Method != http.MethodGet && r.Method != http.MethodHead) || upgrade
	if unsafe && r.Header.Get("Origin") != f.labelOrigin(label) {
		sciencePlain(w, http.StatusForbidden, "Origin refused")
		return
	}
	// Browsers send fetch metadata to loopback (not on WebSocket handshakes,
	// which the Origin check covers). Only the label's own documents and
	// workers may call it, and the workbench may only open the entry.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		entry := r.Method == http.MethodGet && r.URL.Path == "/_cage/enter" && site == "cross-site" && r.Header.Get("Sec-Fetch-Mode") == "navigate"
		if !entry {
			sciencePlain(w, http.StatusForbidden, "请求来源被拒绝")
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/_cage/") {
		header.Set("Content-Security-Policy", f.ownPolicy())
		f.serveOwn(w, r, label, state)
		return
	}
	// Neither kind of worker is reached by the shell's per-target overrides.
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest == "serviceworker" || dest == "sharedworker" || r.Header.Get("Service-Worker") != "" {
		sciencePlain(w, http.StatusForbidden, "本机渲染不支持共享 Worker 与 Service Worker")
		return
	}
	current, generation, ok := f.authorized(label)
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
	f.proxy(w, r, current, generation)
}

// authorized checks that the label is the session's and that the workbench
// session it came from is still unlocked.
func (f *scienceFront) authorized(label string) (scienceTicket, uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session == "" || label == "" || !webfront.Equal(label, f.session) {
		return scienceTicket{}, 0, false
	}
	if f.current.epoch == nil || f.current.epoch.Err() != nil {
		return scienceTicket{}, 0, false
	}
	return f.current, f.generation, true
}

var scienceSignInHidden = map[string]bool{"/oauth/code/callback": true, "/api/auth/callback": true, "/api/auth/exchange": true, "/api/auth/broker-exchange": true}

// intercept answers the routes that must never be served from the host.
func (f *scienceFront) intercept(w http.ResponseWriter, r *http.Request) bool {
	// The daemon's router may take a trailing slash for the same route.
	path := strings.TrimRight(r.URL.Path, "/")
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

// auditHit records what the audit saw: a tripwire for a page whose
// overrides failed, not a barrier (a hostile page has many other ways).
func (f *scienceFront) auditHit(category string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.state.AuditHit, f.state.AuditHitAt = category, &now
	f.state.AuditHits++
}

// scienceRequestHeaders is the tenant gateway's request allow-list without
// Sec-WebSocket-Extensions (compressed frames could not be audited),
// Accept-Language and Content-Encoding (set or refused here), the
// conditional headers (nothing the front serves may be kept, so nothing is
// revalidated), Cookie and X-Csrf-Token (the front holds the daemon's
// cookies), plus what the daemon's app sends of its own.
var scienceRequestHeaders = []string{
	"Accept", "Accept-Encoding", "Content-Type", "Range", "Last-Event-ID", "Connection", "Upgrade",
	"Sec-WebSocket-Key", "Sec-WebSocket-Protocol", "Sec-WebSocket-Version", "X-Requested-With", "Cache-Control",
}

// scienceResponseHeaders leaves out every validator and freshness header:
// each response is no-store.
var scienceResponseHeaders = []string{
	"Content-Type", "Content-Encoding", "Content-Length", "Content-Disposition", "Accept-Ranges", "Content-Range",
	"Location", "Allow", "Retry-After", "X-Accel-Buffering",
}

func scienceOutboundHeaders(in http.Header, identity scienceIdentity, upstreamOrigin string, jar *scienceJar) http.Header {
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
	// The daemon's own cookies, held here; the page can neither read nor set
	// them. Its CSRF token goes with them (header-authenticated requests skip
	// the daemon's double-submit check; the front's Origin check is the
	// guard either way).
	if cookie := jar.header(); cookie != "" {
		out.Set("Cookie", cookie)
	}
	if csrf := jar.value("operon_csrf"); csrf != "" {
		out.Set("X-Csrf-Token", csrf)
	}
	out.Set("Accept-Language", identity.UpstreamAcceptLanguage)
	out.Set("User-Agent", identity.UserAgent)
	return out
}

// scienceJar holds the daemon's cookies for one session, bounded.
type scienceJar struct {
	cookies map[string]string
}

const (
	scienceJarCookies = 32
	scienceJarBytes   = 16 << 10
)

func (j *scienceJar) value(name string) string {
	if j == nil {
		return ""
	}
	return j.cookies[name]
}

func (j *scienceJar) header() string {
	if j == nil || len(j.cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(j.cookies))
	for name := range j.cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+j.cookies[name])
	}
	return strings.Join(parts, "; ")
}

// keep stores what the daemon set; deletions and expired cookies leave.
func (j *scienceJar) keep(cookies []*http.Cookie, now time.Time) {
	for _, cookie := range cookies {
		if cookie.Name == "" || strings.ContainsAny(cookie.Value, " ;\r\n") {
			continue
		}
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(now)) || cookie.Value == "" {
			delete(j.cookies, cookie.Name)
			continue
		}
		if j.cookies == nil {
			j.cookies = map[string]string{}
		}
		if _, ok := j.cookies[cookie.Name]; !ok && len(j.cookies) >= scienceJarCookies {
			continue
		}
		previous, had := j.cookies[cookie.Name]
		j.cookies[cookie.Name] = cookie.Value
		if len(j.header()) > scienceJarBytes {
			if had {
				j.cookies[cookie.Name] = previous
			} else {
				delete(j.cookies, cookie.Name)
			}
		}
	}
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

var errScienceReset = errors.New("本机渲染会话已结束，请回到工作台重新打开 Claude Science。")

// upstreamFor returns the generation's upstream, creating it on first use.
// A reset since the request's session check refuses (F9): nothing is
// created or kept for a generation that ended.
func (f *scienceFront) upstreamFor(generation uint64) (*scienceUpstream, error) {
	f.mu.Lock()
	upstream, create := f.upstream, f.hooks.upstream
	current := f.generation
	f.mu.Unlock()
	if current != generation {
		return nil, errScienceReset
	}
	if upstream != nil {
		return upstream, nil
	}
	if create == nil {
		return nil, errors.New("Claude Science 尚未启动")
	}
	created, err := create()
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generation != generation {
		created.close()
		return nil, errScienceReset
	}
	if f.upstream != nil {
		created.close()
		return f.upstream, nil
	}
	f.upstream = created
	f.wakeWatch()
	return created, nil
}

// reserve takes room for a request body (F7); release gives it back.
func (f *scienceFront) reserve(r *http.Request) (int64, bool) {
	size := int64(0)
	if r.Body != nil && r.Body != http.NoBody {
		size = r.ContentLength
		if size < 0 || size > scienceBodyLimit {
			size = scienceBodyLimit
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if size > 0 && f.bodies+size > scienceBodyBudget {
		return 0, false
	}
	f.bodies += size
	return size, true
}

func (f *scienceFront) release(size int64) {
	f.mu.Lock()
	f.bodies -= size
	f.mu.Unlock()
}

func (f *scienceFront) proxy(w http.ResponseWriter, r *http.Request, current scienceTicket, generation uint64) {
	f.begin()
	defer f.end()
	// The request (and an upgraded socket) ends with the workbench session
	// it came from and with the front's generation.
	f.mu.Lock()
	done := f.done
	stale := f.generation != generation
	f.mu.Unlock()
	if stale {
		sciencePlain(w, http.StatusUnauthorized, errScienceReset.Error())
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(current.epoch, cancel)()
	defer context.AfterFunc(done, cancel)()
	r = r.WithContext(ctx)
	if webfront.IsUpgrade(r) {
		f.mu.Lock()
		full := f.sockets >= scienceSocketCap
		if !full {
			f.sockets++
		}
		f.mu.Unlock()
		if full {
			sciencePlain(w, http.StatusTooManyRequests, "Claude Science 页面打开的连接太多，请关闭一些页面后重试。")
			return
		}
		defer func() {
			f.mu.Lock()
			f.sockets--
			f.mu.Unlock()
		}()
	}
	reserved, ok := f.reserve(r)
	if !ok {
		sciencePlain(w, http.StatusServiceUnavailable, "正在发送给 Claude Science 的内容太多，请稍后重试。")
		return
	}
	defer f.release(reserved)
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
	jar := scienceJar{cookies: maps.Clone(f.jar.cookies)}
	f.mu.Unlock()
	upstream, err := f.upstreamFor(generation)
	if errors.Is(err, errScienceReset) {
		sciencePlain(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		sciencePlain(w, http.StatusBadGateway, err.Error())
		return
	}
	outHeaders := scienceOutboundHeaders(r.Header, current.identity, upstream.origin(), &jar)
	if category := auditRequest(audit, r.URL, outHeaders, body); category != "" {
		f.auditHit(category)
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	proxy := &httputil.ReverseProxy{
		Transport:     scienceProxyTransport{upstream: upstream, audit: audit, onHit: f.auditHit},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errScienceRefused) {
				// F10: the daemon refused every fresh credential.
				f.resetGeneration(generation, err.Error())
				sciencePlain(w, http.StatusBadGateway, err.Error())
				return
			}
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
		ModifyResponse: func(response *http.Response) error { return f.rewriteResponse(response, upstream, r.Host) },
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
		response.Body = scienceSocket{ReadWriteCloser: stream, audit: newScienceSocketAudit(stream, t.audit, t.onHit)}
	}
	return response, nil
}

// rewriteResponse applies the response half of design §7.4. host is the
// session's label host.
func (f *scienceFront) rewriteResponse(response *http.Response, upstream *scienceUpstream, host string) error {
	if response.StatusCode == http.StatusSwitchingProtocols {
		if !webfront.ValidSwitchingResponse(response.Request, response) {
			return errors.New("science protocol upgrade refused")
		}
		response.Header = webfront.SelectHeaders(response.Header, "Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Protocol")
		return nil
	}
	headers := webfront.SelectHeaders(response.Header, scienceResponseHeaders...)
	// Nothing the front serves is kept by the browser.
	headers.Set("Cache-Control", "no-store")
	// The daemon's cookies stay here, for this session only.
	if cookies := response.Cookies(); len(cookies) > 0 {
		f.mu.Lock()
		if f.current.epoch != nil {
			f.jar.keep(cookies, f.now())
		}
		f.mu.Unlock()
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
		location.Scheme, location.Host = "http", host
		headers.Set("Location", location.String())
	}
	ancestors := strings.Fields(f.ancestors())
	for _, policy := range webfront.ParsePolicies(response.Header.Values("Content-Security-Policy")) {
		policy = policy.Without("report-uri", "report-to").Set("frame-ancestors", ancestors...)
		headers.Add("Content-Security-Policy", policy.String())
	}
	headers.Add("Content-Security-Policy", f.lockdown(host))
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
// state is the label's (issued, checking or session).
func (f *scienceFront) serveOwn(w http.ResponseWriter, r *http.Request, label, state string) {
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
		switch {
		case needsProbe && state != "checking":
			sciencePlain(w, http.StatusUnauthorized, "身份核对已过期，请回到工作台重新打开 Claude Science。")
			return
		case strings.HasPrefix(path, "/_cage/enter") && state != "issued":
			sciencePlain(w, http.StatusUnauthorized, "入口已失效，请回到工作台重新打开 Claude Science。")
			return
		}
		if path == "/_cage/enter" {
			// A fresh origin has nothing to clear; this keeps it so should a
			// label ever come back.
			w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
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
	switch {
	case path == "/_cage/enter" && state == "issued":
		f.enter(w, r, label)
	case path == "/_cage/probe" && state == "checking":
		f.probe(w, r, label)
	case path == "/_cage/csp-report" && (state == "checking" || state == "session"):
		f.cspReport(w, r)
	case path == "/_cage/enter" || path == "/_cage/probe" || path == "/_cage/csp-report":
		sciencePlain(w, http.StatusUnauthorized, "入口已失效，请回到工作台重新打开 Claude Science。")
	default:
		http.NotFound(w, r)
	}
}

// enter starts the identity check for an issued label (once) and ends any
// earlier session.
func (f *scienceFront) enter(w http.ResponseWriter, r *http.Request, label string) {
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
		sciencePlain(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	var input struct{}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		sciencePlain(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	f.mu.Lock()
	entry, ok := f.issued[label]
	delete(f.issued, label)
	if !ok || entry.epoch == nil || entry.epoch.Err() != nil {
		f.mu.Unlock()
		sciencePlain(w, http.StatusUnauthorized, "入口已失效，请回到工作台重新打开 Claude Science。")
		return
	}
	upstream := f.upstream
	// A new entry ends the session of any earlier document.
	f.session, f.current, f.audit, f.jar, f.upstream = "", scienceTicket{}, nil, scienceJar{}, nil
	f.state.Session = false
	clear(f.checking)
	f.checking[label] = scienceProbeSession{scienceTicket: entry.scienceTicket, expires: f.now().Add(scienceProbeLife)}
	f.mu.Unlock()
	if upstream != nil {
		upstream.close()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *scienceFront) probe(w http.ResponseWriter, r *http.Request, label string) {
	var input struct {
		Reports []scienceProbeReport `json:"reports"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" || decoder.Decode(&input) != nil || len(input.Reports) > 8 {
		sciencePlain(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	f.mu.Lock()
	session, ok := f.checking[label]
	delete(f.checking, label)
	f.mu.Unlock()
	if !ok || session.epoch.Err() != nil || !f.now().Before(session.expires) {
		sciencePlain(w, http.StatusUnauthorized, "身份核对已过期，请回到工作台重新打开 Claude Science。")
		return
	}
	reason := checkScienceProbe(session.identity, input.Reports, f.now())
	f.mu.Lock()
	if reason != "" {
		now := f.now()
		f.state.Refused, f.state.RefusedAt = "身份核对未通过："+reason+"。已改用隔离浏览器打开。", &now
		f.mu.Unlock()
		sciencePlain(w, http.StatusForbidden, reason)
		return
	}
	f.session, f.current, f.jar = label, session.scienceTicket, scienceJar{}
	f.audit = newScienceAudit(session.host, session.identity, f.now())
	f.state.Session, f.state.Audit = true, f.audit.Distinct
	f.state.Refused, f.state.RefusedAt = "", nil
	f.lastHeard = f.now()
	f.mu.Unlock()
	f.wakeWatch()
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
