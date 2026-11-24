package workbench

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
)

type t3CodeGrant struct {
	redirect string
	expires  time.Time
}

// t3CodeProxy provides a zero-VNC, ultra-low-latency authenticated loopback
// origin (http://127.0.0.1:<ephemeral>) dedicated exclusively to T3 Code Web.
//
// Security invariants:
//   - Bound strictly to the active localAccount unlock epoch: locking Cage
//     cancels the epoch, revokes all grants & cookies (even after later unlock),
//     and immediately closes every established HTTP and WebSocket connection.
//   - Strict Host (exact loopback authority) and Origin checks prevent DNS
//     rebinding and cross-port loopback CSRF/CSWSH.
//   - Unique per-proxy cookie name (`cage_t3_session_<port>`, HttpOnly,
//     SameSite=Strict) prevents cross-proxy cookie collisions across ports.
//   - Supplies canonical X-Forwarded-Host / X-Forwarded-Proto and aligned
//     Origin on WebSocket upgrades so T3 Code v0.1.11's origin validation passes.
type t3CodeProxy struct {
	server        *Server
	targetKey     string
	managedPort   int // >0 for guest-forwarded managed T3 Code
	upstreamURL   *url.URL
	listener      net.Listener
	authority     string // "127.0.0.1:<port>"
	origin        string // "http://127.0.0.1:<port>"
	cookieName    string // unique per proxy port
	httpServer    *http.Server
	reverseProxy  *httputil.ReverseProxy
	epoch         context.Context
	stopEpochHook func() bool

	mu            sync.Mutex
	sessionSecret string
	grants        map[string]t3CodeGrant
	activeConns   map[net.Conn]struct{}
	tunnelCancel  context.CancelFunc
	tunnelDone    chan struct{}
	tunnelAddr    string
	tunnelMu      sync.Mutex
	closed        bool
}

func (s *Server) closeT3CodeProxy() {
	s.t3code.mu.Lock()
	proxy := s.t3code.proxy
	s.t3code.proxy = nil
	s.t3code.mu.Unlock()
	if proxy != nil {
		proxy.close()
	}
}

func (p *t3CodeProxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *t3CodeProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.sessionSecret = ""
	clear(p.grants)
	conns := make([]net.Conn, 0, len(p.activeConns))
	for c := range p.activeConns {
		conns = append(conns, c)
	}
	clear(p.activeConns)
	stopHook := p.stopEpochHook
	p.stopEpochHook = nil
	p.mu.Unlock()

	if stopHook != nil {
		stopHook()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	p.stopTunnel()
	if p.httpServer != nil {
		_ = p.httpServer.Close()
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
}

func (p *t3CodeProxy) stopTunnel() {
	p.tunnelMu.Lock()
	cancel := p.tunnelCancel
	done := p.tunnelDone
	p.tunnelCancel = nil
	p.tunnelDone = nil
	p.tunnelAddr = ""
	p.tunnelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *t3CodeProxy) ensureLiveTunnel(ctx context.Context) (string, error) {
	if p.managedPort <= 0 {
		return "", nil
	}
	p.tunnelMu.Lock()
	defer p.tunnelMu.Unlock()
	if p.isClosed() {
		return "", errors.New("T3 Code 本地代理已关闭")
	}
	if p.tunnelAddr != "" && p.tunnelDone != nil {
		select {
		case <-p.tunnelDone:
			// Prior SSH tunnel died; tear down and transparently reconnect while
			// preserving the exact public loopback origin!
			if p.tunnelCancel != nil {
				p.tunnelCancel()
			}
			p.tunnelCancel = nil
			p.tunnelDone = nil
			p.tunnelAddr = ""
		default:
			return p.tunnelAddr, nil
		}
	}
	addr, cancel, done, err := p.server.startGuestLocalForward(ctx, p.managedPort)
	if err != nil {
		return "", err
	}
	p.tunnelAddr, p.tunnelCancel, p.tunnelDone = addr, cancel, done
	return addr, nil
}

func (p *t3CodeProxy) trackConn(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || (p.epoch != nil && p.epoch.Err() != nil) {
		return false
	}
	if p.activeConns == nil {
		p.activeConns = map[net.Conn]struct{}{}
	}
	p.activeConns[c] = struct{}{}
	return true
}

func (p *t3CodeProxy) untrackConn(c net.Conn) {
	p.mu.Lock()
	delete(p.activeConns, c)
	p.mu.Unlock()
}

func (p *t3CodeProxy) issueGrant(redirectTarget string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || (p.epoch != nil && p.epoch.Err() != nil) {
		return "", errors.New("Cage 已锁定或代理已关闭")
	}
	now := time.Now()
	for k, g := range p.grants {
		if now.After(g.expires) {
			delete(p.grants, k)
		}
	}
	grant := randomToken()
	if redirectTarget == "" {
		redirectTarget = "/"
	}
	p.grants[grant] = t3CodeGrant{
		redirect: redirectTarget,
		expires:  now.Add(t3CodeDirectGrantTTL),
	}
	return grant, nil
}

func (p *t3CodeProxy) consumeGrant(grant string) (string, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.sessionSecret == "" || (p.epoch != nil && p.epoch.Err() != nil) {
		return "", "", false
	}
	g, ok := p.grants[grant]
	delete(p.grants, grant)
	if !ok || time.Now().After(g.expires) {
		return "", "", false
	}
	return g.redirect, p.sessionSecret, true
}

func (p *t3CodeProxy) checkSession(cookieVal string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.sessionSecret == "" || (p.epoch != nil && p.epoch.Err() != nil) {
		return false
	}
	return equalSecret(cookieVal, p.sessionSecret)
}

func (p *t3CodeProxy) dialUpstream(ctx context.Context, network, addr string) (net.Conn, error) {
	target := addr
	if p.managedPort > 0 {
		tunnelAddr, err := p.ensureLiveTunnel(ctx)
		if err != nil {
			return nil, err
		}
		target = tunnelAddr
	}
	dialer := &net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		// If managed tunnel connection was refused, force a one-time reconnect.
		if p.managedPort > 0 {
			p.stopTunnel()
			if retryAddr, rErr := p.ensureLiveTunnel(ctx); rErr == nil {
				conn, err = dialer.DialContext(ctx, "tcp", retryAddr)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return conn, nil
}

func (p *t3CodeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Strict account epoch & closed check.
	if p.isClosed() || (p.epoch != nil && p.epoch.Err() != nil) {
		http.Error(w, "Cage 已锁定，请先解锁工作台后重新打开 T3 Code。", http.StatusLocked)
		return
	}
	if p.server.account != nil {
		if epoch, unlocked := p.server.account.access(); !unlocked || (p.epoch != nil && epoch != p.epoch) {
			p.close()
			http.Error(w, "Cage 已锁定，请先解锁工作台后重新打开 T3 Code。", http.StatusLocked)
			return
		}
	}

	// 2. Strict Host authority validation (blocks DNS rebinding).
	if r.Host != p.authority {
		http.Error(w, "Host refused", http.StatusForbidden)
		return
	}

	// 3. Single-use grant exchange -> unique-per-proxy HttpOnly SameSite=Strict cookie.
	if r.URL.Path == "/_cage_t3_auth" && r.Method == http.MethodGet {
		grant := strings.TrimSpace(r.URL.Query().Get("grant"))
		redirectTarget, secret, ok := p.consumeGrant(grant)
		if !ok {
			http.Error(w, "T3 Code 直连授权已过期或无效，请从 Cage 重新打开。", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     p.cookieName,
			Value:    secret,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, redirectTarget, http.StatusFound)
		return
	}

	// 4. Strict Origin validation on state-changing requests and WebSocket upgrades
	// (prevents cross-port loopback CSRF and Cross-Site WebSocket Hijacking).
	isUpgrade := isWebSocketUpgrade(r)
	if isUpgrade || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != p.origin {
			http.Error(w, "Origin refused", http.StatusForbidden)
			return
		}
	}

	// 5. Session cookie verification bound to current unlock epoch.
	cookie, err := r.Cookie(p.cookieName)
	if err != nil || !p.checkSession(cookie.Value) {
		http.Error(w, "请通过 Cage 工作台授权打开 T3 Code。", http.StatusUnauthorized)
		return
	}

	if isUpgrade {
		p.serveWebSocket(w, r)
		return
	}
	if p.epoch != nil {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(p.epoch, cancel)
		defer func() { stop(); cancel() }()
		r = r.WithContext(ctx)
	}
	p.reverseProxy.ServeHTTP(w, r)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (p *t3CodeProxy) dialWebSocketUpstream(ctx context.Context) (net.Conn, error) {
	hostPort := p.upstreamURL.Host
	if _, _, err := net.SplitHostPort(hostPort); err != nil {
		port := "80"
		if p.upstreamURL.Scheme == "https" {
			port = "443"
		}
		hostPort = net.JoinHostPort(p.upstreamURL.Hostname(), port)
	}
	conn, err := p.dialUpstream(ctx, "tcp", hostPort)
	if err != nil {
		return nil, err
	}
	if p.upstreamURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: p.upstreamURL.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return conn, nil
}

func sanitizeUpstreamRequestHeaders(h http.Header, proxyCookieName, proxyAuthority, upstreamOrigin string) {
	// Strip ALL untrusted client forwarding headers (including arbitrary X-Forwarded-*)
	// and Cage's own proxy cookie before forwarding upstream.
	for name := range h {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-forwarded-") || lower == "x-real-ip" || lower == "forwarded" {
			h.Del(name)
		}
	}
	// Supply canonical forwarding headers for T3 Code v0.1.11 origin verification.
	h.Set("X-Forwarded-Host", proxyAuthority)
	h.Set("X-Forwarded-Proto", "http")
	if origin := strings.TrimSpace(h.Get("Origin")); origin != "" && upstreamOrigin != "" {
		// Proxy already verified Origin == p.origin; align Origin with upstream
		// authority so T3 Code's /ws & POST origin checks succeed even without
		// Sec-Fetch-Site headers.
		h.Set("Origin", upstreamOrigin)
	}
	if rawCookies := h.Values("Cookie"); len(rawCookies) > 0 {
		h.Del("Cookie")
		var kept []string
		for _, line := range rawCookies {
			for _, part := range strings.Split(line, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				name, _, _ := strings.Cut(part, "=")
				name = strings.TrimSpace(name)
				if name == proxyCookieName || strings.HasPrefix(name, "cage_t3_session_") || strings.HasPrefix(name, cookieName+"_") {
					continue
				}
				kept = append(kept, part)
			}
		}
		if len(kept) > 0 {
			h.Set("Cookie", strings.Join(kept, "; "))
		}
	}
}

// serveWebSocket performs bounded-handshake, epoch-tracked, full-duplex
// half-close-safe WebSocket relaying with TCP_NODELAY for sub-ms streaming.
func (p *t3CodeProxy) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket hijacking unsupported", http.StatusInternalServerError)
		return
	}
	dialCtx, dialCancel := context.WithTimeout(r.Context(), 10*time.Second)
	upstream, err := p.dialWebSocketUpstream(dialCtx)
	dialCancel()
	if err != nil {
		http.Error(w, "无法连接到 T3 Code 实时通道", http.StatusBadGateway)
		return
	}

	clientConn, clientRW, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if !p.trackConn(clientConn) || !p.trackConn(upstream) {
		p.untrackConn(clientConn)
		p.untrackConn(upstream)
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	defer func() {
		p.untrackConn(clientConn)
		p.untrackConn(upstream)
		_ = clientConn.Close()
		_ = upstream.Close()
	}()
	if tc, ok := clientConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	reqCopy := r.Clone(r.Context())
	reqCopy.Host = p.upstreamURL.Host
	reqCopy.RequestURI = ""
	upstreamOrigin := p.upstreamURL.Scheme + "://" + p.upstreamURL.Host
	sanitizeUpstreamRequestHeaders(reqCopy.Header, p.cookieName, p.authority, upstreamOrigin)

	// Bound the WebSocket handshake write+read phase to 10 seconds.
	_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
	if err := reqCopy.Write(upstream); err != nil {
		return
	}
	br := bufio.NewReaderSize(upstream, 32<<10)
	resp, err := http.ReadResponse(br, reqCopy)
	if err != nil {
		return
	}
	_ = upstream.SetDeadline(time.Time{})
	_ = clientConn.SetDeadline(time.Time{})

	// Forward the upstream handshake status + headers faithfully to the client.
	if err := resp.Write(clientConn); err != nil {
		_ = resp.Body.Close()
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		return
	}

	// Full-duplex relay: propagate half-close cleanly via CloseWrite() and wait
	// for BOTH directions so normal EOF on one side never truncates peer data.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if clientRW != nil && clientRW.Reader.Buffered() > 0 {
			_, _ = io.CopyN(upstream, clientRW.Reader, int64(clientRW.Reader.Buffered()))
		}
		_, _ = io.Copy(upstream, clientConn)
		closeWriteConn(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, br)
		closeWriteConn(clientConn)
	}()
	wg.Wait()
}

func closeWriteConn(c net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}

// startGuestLocalForward spawns a single persistent multiplexed SSH local port
// forward (`ssh -N -L 127.0.0.1:<local>:127.0.0.1:<guestPort>`) with OpenSSH's
// authenticated `LocalCommand=echo 1` readiness marker (matching macOS Screen
// Sharing) and a single reaper goroutine so there are never zombie children.
func (s *Server) startGuestLocalForward(parent context.Context, guestPort int) (string, context.CancelFunc, chan struct{}, error) {
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	localPort := probeLn.Addr().(*net.TCPAddr).Port
	_ = probeLn.Close()

	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	life := s.lifetimeContext()
	cmdCtx, cancel := context.WithCancel(life)

	var cmd *exec.Cmd
	if s.macosGuest != nil {
		args, err := guestos.MacOSLocalForwardSSHArgs(s.profile, s.macosGuest.User, localPort, guestPort)
		if err != nil {
			cancel()
			return "", nil, nil, err
		}
		args = append(args, "cage-macos")
		cmd = exec.CommandContext(cmdCtx, sshExecutable(), args...)
		cmd.Env = sshEnvironment()
		host.HideCommandWindow(cmd)
	} else {
		cmd = s.sshLocalForwardCommand(cmdCtx, localAddr, guestPort)
	}
	if cmd.Err != nil {
		cancel()
		return "", nil, nil, cmd.Err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, nil, fmt.Errorf("无法建立到 T3 Code 的极速本地通道: %w", err)
	}

	bound := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var marker [1]byte
		_, readErr := io.ReadFull(stdout, marker[:])
		bound <- readErr == nil && marker[0] == '1'
		_ = cmd.Wait()
	}()

	waitCtx, waitCancel := context.WithTimeout(parent, 8*time.Second)
	defer waitCancel()
	select {
	case ok := <-bound:
		if ok {
			return localAddr, cancel, done, nil
		}
	case <-waitCtx.Done():
	case <-life.Done():
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return "", nil, nil, errors.New("T3 Code 极速本地通道建立失败或超时，请重试或改用工作台内置标签")
}

func (s *Server) ensureT3CodeProxy(ctx context.Context, cfg t3CodeConfig) (*t3CodeProxy, error) {
	var epoch context.Context
	if s.account != nil {
		var unlocked bool
		epoch, unlocked = s.account.access()
		if !unlocked {
			return nil, errors.New("Cage 已锁定，请先解锁工作台")
		}
	}
	targetKey := fmt.Sprintf("%s:%d:%s", cfg.Mode, cfg.Port, cfg.ExternalURL)

	s.t3code.mu.Lock()
	if p := s.t3code.proxy; p != nil && p.targetKey == targetKey && !p.isClosed() && (p.epoch == nil || p.epoch == epoch) {
		s.t3code.mu.Unlock()
		if p.managedPort > 0 {
			if _, err := p.ensureLiveTunnel(ctx); err != nil {
				return nil, err
			}
		}
		return p, nil
	}
	old := s.t3code.proxy
	s.t3code.proxy = nil
	s.t3code.mu.Unlock()
	if old != nil {
		old.close()
	}

	var upstream *url.URL
	managedPort := 0
	if cfg.Mode == "external" {
		u, err := url.Parse(cfg.ExternalURL)
		if err != nil || u.Host == "" {
			return nil, errors.New("外部 T3 Code 地址无效")
		}
		upstream = u
	} else {
		if s.windowsGuest != nil {
			return nil, errors.New("Windows 原生客机暂不支持托管 T3 Code 本地隧道，请使用外部地址模式")
		}
		upstream = &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", cfg.Port)}
		managedPort = cfg.Port
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxyPort := ln.Addr().(*net.TCPAddr).Port
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(proxyPort))
	origin := "http://" + authority
	upstreamOrigin := upstream.Scheme + "://" + upstream.Host

	p := &t3CodeProxy{
		server:        s,
		targetKey:     targetKey,
		managedPort:   managedPort,
		upstreamURL:   upstream,
		listener:      ln,
		authority:     authority,
		origin:        origin,
		cookieName:    fmt.Sprintf("cage_t3_session_%d", proxyPort),
		epoch:         epoch,
		sessionSecret: randomToken(),
		grants:        map[string]t3CodeGrant{},
		activeConns:   map[net.Conn]struct{}{},
	}

	transport := &http.Transport{
		DialContext:           p.dialUpstream,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			sanitizeUpstreamRequestHeaders(pr.Out.Header, p.cookieName, p.authority, upstreamOrigin)
		},
		FlushInterval: -1,
	}
	p.reverseProxy = rp
	p.httpServer = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if managedPort > 0 {
		if _, err := p.ensureLiveTunnel(ctx); err != nil {
			p.close()
			return nil, err
		}
	}

	// Register epoch cancellation hook AFTER httpServer is initialized and guard
	// stopEpochHook under p.mu so an already-cancelled epoch cannot race p.close().
	if epoch != nil {
		stop := context.AfterFunc(epoch, func() {
			p.close()
			s.t3code.mu.Lock()
			if s.t3code.proxy == p {
				s.t3code.proxy = nil
			}
			s.t3code.mu.Unlock()
		})
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			stop()
		} else {
			p.stopEpochHook = stop
			p.mu.Unlock()
		}
	}

	go func() {
		_ = p.httpServer.Serve(ln)
	}()

	var displaced *t3CodeProxy
	s.t3code.mu.Lock()
	if p.isClosed() || (epoch != nil && epoch.Err() != nil) {
		s.t3code.mu.Unlock()
		p.close()
		return nil, errors.New("Cage 已锁定，请先解锁工作台")
	}
	if existing := s.t3code.proxy; existing != nil && !existing.isClosed() {
		if existing.targetKey == targetKey && (existing.epoch == nil || existing.epoch == epoch) {
			s.t3code.mu.Unlock()
			p.close()
			return existing, nil
		}
		displaced = existing
	}
	s.t3code.proxy = p
	s.t3code.mu.Unlock()
	if displaced != nil {
		displaced.close()
	}
	return p, nil
}

func (p *t3CodeProxy) issueDirectURL(openURL string) (string, error) {
	redirectTarget := "/"
	if u, err := url.Parse(openURL); err == nil {
		path := u.EscapedPath()
		if path == "" {
			path = "/"
		}
		if u.RawQuery != "" {
			path += "?" + u.RawQuery
		}
		if u.Fragment != "" {
			path += "#" + u.Fragment
		}
		redirectTarget = path
	}
	grant, err := p.issueGrant(redirectTarget)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/_cage_t3_auth?grant=%s", p.origin, grant), nil
}

func (s *Server) issueT3CodeDirectURL(ctx context.Context, cfg t3CodeConfig, openURL string) (string, error) {
	p, err := s.ensureT3CodeProxy(ctx, cfg)
	if err != nil {
		return "", err
	}
	return p.issueDirectURL(openURL)
}
