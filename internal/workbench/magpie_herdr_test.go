package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestMagpieConfigLifecycleAndSecurityBoundaries(t *testing.T) {
	s := testServer(t)
	var openURLCalls atomic.Int32
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		if subcmd == "magpie-open-url" {
			openURLCalls.Add(1)
			return json.Unmarshal([]byte(`{"url":"http://127.0.0.1:3430/?k=secret-magpie-key"}`), output)
		}
		return nil
	}
	s.ready = true

	// Disabled by default with port 3430 and gateway port 3425.
	w := request(s, "GET", "/api/magpie", "", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/magpie: %d %s", w.Code, w.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["enabled"] != false || st["mode"] != "managed" || st["open_mode"] != "workbench" {
		t.Fatalf("unexpected default magpie status: %+v", st)
	}
	if st["port"] != float64(defaultMagpiePort) || st["gateway_port"] != float64(defaultMagpieGatewayPort) {
		t.Fatalf("unexpected default magpie ports: %+v", st)
	}
	if st["gateway_url"] != "http://127.0.0.1:3425" || st["gateway_only"] != false || st["focus_chrome"] != true {
		t.Fatalf("unexpected default magpie gateway/chrome flags: %+v", st)
	}
	if st["supports_managed"] != true || st["direct_available"] != true {
		t.Fatalf("expected supports_managed & direct_available true by default: %+v", st)
	}

	// Remote SSH client & native desktop WebView requests must see direct_available=false.
	remoteReq := httptest.NewRequest("GET", s.origin+"/api/magpie", nil)
	remoteReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	remoteReq.Header.Set("X-Cage-Remote-Client", "1")
	rw := httptest.NewRecorder()
	s.handler().ServeHTTP(rw, remoteReq)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"direct_available":false`) {
		t.Fatalf("remote client did not disable direct_available: %d %s", rw.Code, rw.Body.String())
	}
	desktopReq := httptest.NewRequest("GET", s.origin+"/api/magpie", nil)
	desktopReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	desktopReq.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	dw := httptest.NewRecorder()
	s.handler().ServeHTTP(dw, desktopReq)
	if dw.Code != http.StatusOK || !strings.Contains(dw.Body.String(), `"direct_available":false`) {
		t.Fatalf("desktop WebView did not disable direct_available: %d %s", dw.Code, dw.Body.String())
	}

	// Start/restart must reject disabled managed config.
	if sw := request(s, "POST", "/api/magpie/start", s.origin, `{}`, true); sw.Code != http.StatusConflict {
		t.Fatalf("disabled managed start accepted: %d %s", sw.Code, sw.Body.String())
	}

	// Reject reserved Magpie gateway port 3425 and privileged ports.
	if w = request(s, "POST", "/api/magpie/config", s.origin, `{"port":3425}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("reserved gateway port 3425 accepted: %d %s", w.Code, w.Body.String())
	}
	if w = request(s, "POST", "/api/magpie/config", s.origin, `{"port":80}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("privileged port accepted: %d %s", w.Code, w.Body.String())
	}

	// Reject non-loopback plain HTTP external URL and userinfo URLs.
	if w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"http://remote.example.com:3430"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("plain HTTP remote URL accepted: %d %s", w.Code, w.Body.String())
	}
	if w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"http://user:pass@127.0.0.1:3430"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("userinfo URL accepted: %d %s", w.Code, w.Body.String())
	}
	if w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"https://magpie.example.test//evil.com"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("protocol-relative external URL accepted: %d %s", w.Code, w.Body.String())
	}
	if clean := sanitizeMagpieDisplayURL("https://magpie.example.test/?k=secretkey12345678"); strings.Contains(clean, "secretkey") {
		t.Fatalf("sanitizeMagpieDisplayURL leaked key: %q", clean)
	}

	// Accept valid HTTPS external URL & persist settings.
	w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"https://magpie.example.test","open_mode":"direct","focus_chrome":true,"gateway_only":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("valid magpie config rejected: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), `"open_mode":"direct"`) || !strings.Contains(w.Body.String(), `"gateway_only":true`) {
		t.Fatalf("unexpected saved magpie config response: %s", w.Body.String())
	}

	// Verify workbench mode reuses ONLY exact-origin tab and does NOT consume auth URL when reusing.
	var activatedID, createdURL string
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tabs":
			_, _ = w.Write([]byte(`{"tabs":[{"id":"tab-other","kind":"web","url":"http://127.0.0.1:19999/"},{"id":"tab-magpie","kind":"web","url":"https://magpie.example.test/sessions/1"}]}`))
		case "/activate":
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			activatedID = body.ID
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/new":
			var body struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdURL = body.URL
			_, _ = w.Write([]byte(`{"id":"tab-new"}`))
		}
	}))
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()

	// Remote client requesting force_mode="direct" must fall back to workbench mode and reuse exact-origin tab.
	openReq := httptest.NewRequest("POST", s.origin+"/api/magpie/open", strings.NewReader(`{"force_mode":"direct"}`))
	openReq.Header.Set("Origin", s.origin)
	openReq.Header.Set("Content-Type", "application/json")
	openReq.Header.Set("X-Cage-Remote-Client", "1")
	openReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	ow := httptest.NewRecorder()
	s.handler().ServeHTTP(ow, openReq)
	if ow.Code != http.StatusOK || !strings.Contains(ow.Body.String(), `"mode":"workbench"`) || activatedID != "tab-magpie" {
		t.Fatalf("remote fallback / exact-origin tab reuse failed: %d %s activated=%q created=%q", ow.Code, ow.Body.String(), activatedID, createdURL)
	}
	if openURLCalls.Load() != 0 {
		t.Fatalf("reusing existing tab unexpectedly consumed open URL (%d calls)", openURLCalls.Load())
	}

	// Opening direct proxy against a live external target, then disabling Magpie must close the proxy immediately.
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ext.Close()
	w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"`+ext.URL+`","open_mode":"direct"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("switching external URL failed: %d %s", w.Code, w.Body.String())
	}
	openDirectResp := request(s, "POST", "/api/magpie/open", s.origin, `{"force_mode":"direct"}`, true)
	if openDirectResp.Code != http.StatusOK {
		t.Fatalf("direct open failed: %d %s", openDirectResp.Code, openDirectResp.Body.String())
	}
	s.magpie.mu.Lock()
	activeProxy := s.magpie.proxy
	s.magpie.mu.Unlock()
	if activeProxy == nil || activeProxy.isClosed() {
		t.Fatal("expected live Magpie proxy after direct open")
	}

	// Disabling Magpie closes the active proxy and persists enabled=false.
	w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":false}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("disabling magpie failed: %d %s", w.Code, w.Body.String())
	}
	s.magpie.mu.Lock()
	hasProxy := s.magpie.proxy != nil
	s.magpie.mu.Unlock()
	if hasProxy || !activeProxy.isClosed() {
		t.Fatal("disabling magpie left proxy active or unclosed")
	}

	// Verify consumeMagpieOpenURL queries guest runtime in managed mode.
	managedCfg := defaultMagpieConfig()
	managedCfg.Enabled = true
	gotOpenURL := s.consumeMagpieOpenURL(context.Background(), managedCfg)
	if gotOpenURL != "http://127.0.0.1:3430/?k=secret-magpie-key" || openURLCalls.Load() != 1 {
		t.Fatalf("consumeMagpieOpenURL = %q (calls=%d)", gotOpenURL, openURLCalls.Load())
	}

	// Verify magpieTUIScript for Linux and macOS guests.
	linuxScript := s.magpieTUIScript("/proj/my 'app'")
	if !strings.Contains(linuxScript, "/bin/bash -lic") ||
		!strings.Contains(linuxScript, "$HOME/.local/bin") ||
		!strings.Contains(linuxScript, "TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1") ||
		!strings.Contains(linuxScript, "magpie tui") ||
		!strings.Contains(linuxScript, shellescape.Quote("/proj/my 'app'")) {
		t.Fatalf("unexpected Linux magpieTUIScript: %s", linuxScript)
	}
	mac := &Server{macosGuest: &guestos.MacOSProfile{User: "cage"}}
	macScript := mac.magpieTUIScript("/Users/cage/work")
	if !strings.Contains(macScript, "/bin/zsh -lic") || !strings.Contains(macScript, "exec /bin/zsh -il") {
		t.Fatalf("unexpected macOS magpieTUIScript: %s", macScript)
	}

	// Locked Cage must block /api/magpie with 423.
	s.account.lock()
	if w = request(s, "GET", "/api/magpie", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked Cage exposed /api/magpie: %d", w.Code)
	}
}

func TestMagpieDirectProxyAuthCookiesCrossOriginWebSocketAndLockRevocation(t *testing.T) {
	var sawXFH, sawSecFetchSite, sawPostOrigin, sawPostCookie, sawWSOrigin string

	// Wrap mock upstream with Go's real http.NewCrossOriginProtection to match yetone/magpie's webOwnPage.
	cop := http.NewCrossOriginProtection()
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawCookie := r.Header.Get("Cookie")
		if strings.Contains(rawCookie, "cage_magpie_session_") || strings.Contains(rawCookie, "cage_t3_session_") {
			http.Error(w, "proxy cookie leaked upstream", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-Custom") != "" {
			http.Error(w, "untrusted forwarding header leaked upstream", http.StatusBadRequest)
			return
		}
		sawXFH = r.Header.Get("X-Forwarded-Host")
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			sawWSOrigin = r.Header.Get("Origin")
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			for {
				line, err := rw.ReadString('\n')
				if err != nil {
					return
				}
				_, _ = rw.WriteString("echo:" + line)
				_ = rw.Flush()
			}
		}
		if r.URL.Path == "/" && r.URL.Query().Get("k") == "magpie-one-time-key" {
			http.SetCookie(w, &http.Cookie{
				Name:     "magpie_web_session",
				Value:    "magpie-auth-ok",
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("MAGPIE_SIGNED_IN"))
			return
		}
		if r.Method == http.MethodPost {
			sawSecFetchSite = r.Header.Get("Sec-Fetch-Site")
			sawPostOrigin = r.Header.Get("Origin")
			sawPostCookie = rawCookie
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("MAGPIE_POST_OK"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("MAGPIE_ORIGINAL_WEB:" + r.URL.Path))
	})
	upstream := httptest.NewServer(cop.Handler(upstreamHandler))
	defer upstream.Close()

	s := testServer(t)
	s.ready = true
	defer s.closeMagpieProxy()

	w := request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"external","external_url":"`+upstream.URL+`/?k=magpie-one-time-key","open_mode":"direct"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("config failed: %d %s", w.Code, w.Body.String())
	}

	openOnce := func() string {
		t.Helper()
		resp := request(s, "POST", "/api/magpie/open", s.origin, `{"force_mode":"direct"}`, true)
		if resp.Code != http.StatusOK {
			t.Fatalf("open direct failed: %d %s", resp.Code, resp.Body.String())
		}
		var openResp struct {
			Mode      string `json:"mode"`
			DirectURL string `json:"direct_url"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &openResp); err != nil {
			t.Fatal(err)
		}
		if openResp.Mode != "direct" || !strings.Contains(openResp.DirectURL, "/_cage_magpie_auth?grant=") {
			t.Fatalf("unexpected open response: %+v", openResp)
		}
		return openResp.DirectURL
	}
	directURL1 := openOnce()
	unusedGrantURL := openOnce()

	parsed, err := url.Parse(directURL1)
	if err != nil {
		t.Fatal(err)
	}
	proxyBase := parsed.Scheme + "://" + parsed.Host
	wantCookieName := "cage_magpie_session_" + parsed.Port()

	// 1. Unauthenticated request directly to proxy must be rejected (401).
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(proxyBase + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy request status = %d, want 401", resp.StatusCode)
	}

	// 2. Redeem single-use grant -> sets cage_magpie_session_<port> HttpOnly SameSite=Strict cookie + 302 to /?k=...
	resp, err = noRedirect.Get(directURL1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/?k=magpie-one-time-key" {
		t.Fatalf("grant exchange failed: status=%d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != wantCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected proxy cookie: %+v (want name=%q HttpOnly SameSite=Strict)", cookies, wantCookieName)
	}
	proxyCookie := cookies[0]

	// 3. Replaying the same grant must fail with 401.
	replay, err := noRedirect.Get(directURL1)
	if err != nil {
		t.Fatal(err)
	}
	_ = replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed grant accepted: %d", replay.StatusCode)
	}

	// 4. Follow redirect /?k=magpie-one-time-key -> receives upstream magpie_web_session cookie!
	signInReq, _ := http.NewRequest("GET", proxyBase+resp.Header.Get("Location"), nil)
	signInReq.AddCookie(proxyCookie)
	signInResp, err := noRedirect.Do(signInReq)
	if err != nil {
		t.Fatal(err)
	}
	signInBody, _ := io.ReadAll(signInResp.Body)
	_ = signInResp.Body.Close()
	if signInResp.StatusCode != http.StatusOK || string(signInBody) != "MAGPIE_SIGNED_IN" {
		t.Fatalf("auto sign-in redirect failed: status=%d body=%q", signInResp.StatusCode, string(signInBody))
	}
	var magpieCookie *http.Cookie
	for _, c := range signInResp.Cookies() {
		if c.Name == "magpie_web_session" {
			magpieCookie = c
		}
	}
	if magpieCookie == nil || magpieCookie.Value != "magpie-auth-ok" {
		t.Fatalf("upstream magpie_web_session cookie missing: %+v", signInResp.Cookies())
	}

	// 5. Authenticated POST with Sec-Fetch-Site: same-origin, proxy cookie, stray t3 cookie, and magpie_web_session:
	//    - proxy & t3 cookies stripped; magpie_web_session preserved!
	//    - X-Forwarded-* spoofed headers stripped; canonical X-Forwarded-Host set!
	//    - Origin aligned to upstream + Sec-Fetch-Site preserved -> passes real http.NewCrossOriginProtection!
	postReq, _ := http.NewRequest("POST", proxyBase+"/api/chat", strings.NewReader(`{"msg":"hi"}`))
	postReq.Header.Set("Origin", proxyBase)
	postReq.Header.Set("Sec-Fetch-Site", "same-origin")
	postReq.Header.Set("X-Forwarded-For", "1.2.3.4")
	postReq.Header.Set("X-Forwarded-Custom", "spoofed")
	postReq.Header.Set("Forwarded", "for=1.2.3.4")
	postReq.AddCookie(proxyCookie)
	postReq.AddCookie(&http.Cookie{Name: "cage_t3_session_9999", Value: "leaked"})
	postReq.AddCookie(magpieCookie)
	postResp, err := noRedirect.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postBody, _ := io.ReadAll(postResp.Body)
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK || string(postBody) != "MAGPIE_POST_OK" {
		t.Fatalf("authenticated Magpie POST failed: status=%d body=%q", postResp.StatusCode, string(postBody))
	}
	if sawXFH != parsed.Host || sawSecFetchSite != "same-origin" || sawPostOrigin != upstream.URL {
		t.Fatalf("unexpected upstream headers: xfh=%q sec-fetch-site=%q origin=%q", sawXFH, sawSecFetchSite, sawPostOrigin)
	}
	if !strings.Contains(sawPostCookie, "magpie_web_session=magpie-auth-ok") || strings.Contains(sawPostCookie, "cage_magpie_session_") || strings.Contains(sawPostCookie, "cage_t3_session_") {
		t.Fatalf("unexpected upstream Cookie header: %q", sawPostCookie)
	}

	// 5b. Authenticated POST WITHOUT Sec-Fetch-Site still passes http.NewCrossOriginProtection via aligned Origin.
	postNoFetchReq, _ := http.NewRequest("POST", proxyBase+"/api/chat", strings.NewReader(`{"msg":"hi2"}`))
	postNoFetchReq.Header.Set("Origin", proxyBase)
	postNoFetchReq.AddCookie(proxyCookie)
	postNoFetchReq.AddCookie(magpieCookie)
	postNoFetchResp, err := noRedirect.Do(postNoFetchReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = postNoFetchResp.Body.Close()
	if postNoFetchResp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated Magpie POST without Sec-Fetch-Site failed: status=%d", postNoFetchResp.StatusCode)
	}

	// 6. Foreign Host & foreign Origin on POST must be rejected with 403.
	badHostReq, _ := http.NewRequest("GET", proxyBase+"/", nil)
	badHostReq.Host = "evil.rebind.test:" + parsed.Port()
	badHostReq.AddCookie(proxyCookie)
	badHostResp, err := noRedirect.Do(badHostReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badHostResp.Body.Close()
	if badHostResp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host accepted: %d", badHostResp.StatusCode)
	}

	badOriginReq, _ := http.NewRequest("POST", proxyBase+"/api/chat", strings.NewReader(`{}`))
	badOriginReq.Header.Set("Origin", "http://127.0.0.1:9999")
	badOriginReq.AddCookie(proxyCookie)
	badOriginResp, err := noRedirect.Do(badOriginReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badOriginResp.Body.Close()
	if badOriginResp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin on POST accepted: %d", badOriginResp.StatusCode)
	}

	// 7. WebSocket relay works with aligned Origin and closes immediately on account lock.
	wsReq, _ := http.NewRequest("GET", proxyBase+"/ws", nil)
	wsReq.Header.Set("Connection", "Upgrade")
	wsReq.Header.Set("Upgrade", "websocket")
	wsReq.Header.Set("Origin", proxyBase)
	wsReq.AddCookie(proxyCookie)
	wsReq.AddCookie(magpieCookie)
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := wsReq.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	wsResp, err := http.ReadResponse(br, wsReq)
	if err != nil || wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket upgrade failed: %v %+v", err, wsResp)
	}
	if sawWSOrigin != upstream.URL {
		t.Fatalf("upstream WS Origin = %q, want %q", sawWSOrigin, upstream.URL)
	}
	_, _ = conn.Write([]byte("ping-magpie\n"))
	echoLine, err := br.ReadString('\n')
	if err != nil || echoLine != "echo:ping-magpie\n" {
		t.Fatalf("websocket pipe mismatch: %q %v", echoLine, err)
	}

	// 8. Locking Cage immediately closes live WebSocket connection (not a timeout) and revokes proxy forever.
	s.account.lock()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, readErr := br.ReadByte()
	if readErr == nil {
		t.Fatal("established Magpie WebSocket survived Cage lock!")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("established Magpie WebSocket timed out instead of closing immediately on lock: %v", readErr)
	}

	// Re-unlocking with a fresh key must NOT revive old proxy, old cookie, or unused pre-lock grant.
	s.account.mu.Lock()
	s.account.setKey(make([]byte, 32))
	s.account.mu.Unlock()

	if respOld, err := noRedirect.Get(unusedGrantURL); err == nil {
		_ = respOld.Body.Close()
		t.Fatalf("unused pre-lock Magpie grant survived lock+unlock with status %d", respOld.StatusCode)
	}
	reqOldCookie, _ := http.NewRequest("GET", proxyBase+"/", nil)
	reqOldCookie.AddCookie(proxyCookie)
	if respOld, err := noRedirect.Do(reqOldCookie); err == nil {
		_ = respOld.Body.Close()
		t.Fatalf("pre-lock Magpie proxy cookie survived lock+unlock with status %d", respOld.StatusCode)
	}
}

func TestHerdrConfigTakeoverQuotingAndSecurityBoundaries(t *testing.T) {
	s := testServer(t)
	s.ready = true

	// Default status: disabled, takeover_terminals=true, no theme (the TUI's).
	w := request(s, "GET", "/api/herdr", "", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/herdr: %d %s", w.Code, w.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if _, theme := st["theme"]; st["enabled"] != false || st["takeover_terminals"] != true || theme {
		t.Fatalf("unexpected default herdr status: %+v", st)
	}
	if st["installed"] != false || st["server_running"] != false || st["supports_managed"] != true {
		t.Fatalf("unexpected default herdr runtime fields: %+v", st)
	}

	// A theme from an older client is accepted and ignored.
	if w = request(s, "POST", "/api/herdr/config", s.origin, `{"theme":"tokyo-night"}`, true); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "theme") {
		t.Fatalf("old client's herdr theme: %d %s", w.Code, w.Body.String())
	}

	// Disabled start must fail with 409 Conflict; unknown action must 404.
	if w = request(s, "POST", "/api/herdr/start", s.origin, `{}`, true); w.Code != http.StatusConflict {
		t.Fatalf("disabled herdr start accepted: %d %s", w.Code, w.Body.String())
	}
	if w = request(s, "POST", "/api/herdr/unknown-op", s.origin, `{}`, true); w.Code != http.StatusNotFound {
		t.Fatalf("unknown herdr action status = %d, want 404", w.Code)
	}

	// Valid config updates (enable, takeover_terminals toggle).
	w = request(s, "POST", "/api/herdr/config", s.origin, `{"enabled":true,"takeover_terminals":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("valid herdr config rejected: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), `"takeover_terminals":false`) {
		t.Fatalf("unexpected saved herdr config response: %s", w.Body.String())
	}

	// shouldTakeoverWithHerdr() behavior across all state combinations.
	// 1. Enabled + takeover=false -> false
	if s.shouldTakeoverWithHerdr() {
		t.Fatal("expected shouldTakeoverWithHerdr=false when takeover_terminals=false")
	}

	// 2. Enabled + takeover=true + InstalledHint=false -> false
	w = request(s, "POST", "/api/herdr/config", s.origin, `{"takeover_terminals":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("enabling takeover_terminals failed: %d %s", w.Code, w.Body.String())
	}
	if s.shouldTakeoverWithHerdr() {
		t.Fatal("expected shouldTakeoverWithHerdr=false when uninstalled")
	}

	// 3. Enabled + takeover=true + InstalledHint=true -> true (zero SSH)
	s.herdr.mu.Lock()
	cfg, _ := s.loadHerdrConfigLocked()
	cfg.InstalledHint = true
	_ = s.saveHerdrConfigLocked(cfg)
	s.herdr.mu.Unlock()
	if !s.shouldTakeoverWithHerdr() {
		t.Fatal("expected shouldTakeoverWithHerdr=true when enabled + takeover + InstalledHint")
	}

	// 4. Fresh probe saying installed=false overrides stale InstalledHint.
	s.herdr.mu.Lock()
	s.herdr.statusCache = herdrGuestStatus{Installed: false}
	s.herdr.statusAt = time.Now()
	s.herdr.mu.Unlock()
	if s.shouldTakeoverWithHerdr() {
		t.Fatal("expected fresh uninstalled probe to override InstalledHint")
	}

	// 5. Fresh probe saying installed=true enables takeover.
	s.herdr.mu.Lock()
	s.herdr.statusCache = herdrGuestStatus{Installed: true}
	s.herdr.statusAt = time.Now()
	s.herdr.mu.Unlock()
	if !s.shouldTakeoverWithHerdr() {
		t.Fatal("expected shouldTakeoverWithHerdr=true with fresh installed probe")
	}

	// 6. Disabled -> false even when installed.
	w = request(s, "POST", "/api/herdr/config", s.origin, `{"enabled":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("disabling herdr failed: %d %s", w.Code, w.Body.String())
	}
	if s.shouldTakeoverWithHerdr() {
		t.Fatal("expected shouldTakeoverWithHerdr=false when disabled")
	}

	// 7. Windows guest -> always false even when enabled + installed + takeover.
	w = request(s, "POST", "/api/herdr/config", s.origin, `{"enabled":true,"takeover_terminals":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enabling herdr failed: %d %s", w.Code, w.Body.String())
	}
	s.windowsGuest = &guestos.WindowsProfile{}
	if s.shouldTakeoverWithHerdr() {
		t.Fatal("expected shouldTakeoverWithHerdr=false on Windows guest")
	}
	// Verify Windows guest status payload does NOT deadlock (vmRunning called before s.mu).
	if payload, err := s.herdrStatusPayload(nil, false); err != nil || payload["supports_managed"] != false {
		t.Fatalf("Windows guest herdrStatusPayload unexpected: err=%v payload=%+v", err, payload)
	}
	if payload, err := s.magpieStatusPayload(nil, false); err != nil || payload["supports_managed"] != false {
		t.Fatalf("Windows guest magpieStatusPayload unexpected: err=%v payload=%+v", err, payload)
	}
	s.windowsGuest = nil

	// herdrAttachScript quoting safety & environment variables (Linux + macOS).
	hostileWorkspace := `/proj/evil'; rm -rf /; $(whoami) ` + "`id`" + ` "x"`
	hostileProject := `proj'$(touch /tmp/pwn)`
	hostileTab := `Claude #1; echo pwn`
	hostileInner := `cd '/proj' && eval "echo 'hi'" ; $(false)`
	linuxAttach := s.herdrAttachScript(herdrAttach{Mode: "create", Name: "cage-0123456789abcdef", Workspace: hostileWorkspace,
		Project: hostileProject, Title: hostileTab, Kind: "claude", Inner: hostileInner})
	for _, want := range []string{
		"exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 ",
		"CAGE_HERDR_MODE=create ",
		"CAGE_HERDR_NAME=cage-0123456789abcdef ",
		"CAGE_HERDR_WORKSPACE=" + shellescape.Quote(hostileWorkspace),
		"CAGE_HERDR_PROJECT_LABEL=" + shellescape.Quote(hostileProject),
		"CAGE_HERDR_TAB_LABEL=" + shellescape.Quote(hostileTab),
		"CAGE_HERDR_KIND=" + shellescape.Quote("claude"),
		"CAGE_HERDR_INNER_CMD=" + shellescape.Quote(hostileInner),
		"CAGE_HERDR_SHELL=" + shellescape.Quote("/bin/bash"),
		"herdr-attach",
	} {
		if !strings.Contains(linuxAttach, want) {
			t.Fatalf("Linux herdrAttachScript missing %q:\n%s", want, linuxAttach)
		}
	}
	// Ensure raw unquoted injection payload does not leak into the command line.
	if strings.Contains(linuxAttach, "CAGE_HERDR_WORKSPACE="+hostileWorkspace) {
		t.Fatalf("unquoted workspace leaked into herdrAttachScript: %s", linuxAttach)
	}

	s.macosGuest = &guestos.MacOSProfile{User: "cage"}
	macAttach := s.herdrAttachScript(herdrAttach{Mode: "create", Name: "cage-0123456789abcdef", Workspace: "/Users/cage/proj", Project: "proj", Title: "Shell 1", Kind: "shell", Inner: "exec /bin/zsh -l"})
	s.macosGuest = nil
	if !strings.Contains(macAttach, "CAGE_HERDR_SHELL="+shellescape.Quote("/bin/zsh")) ||
		!strings.Contains(macAttach, "/usr/local/libexec/cage-cli/bin/python3") {
		t.Fatalf("unexpected macOS herdrAttachScript: %s", macAttach)
	}

	// Locked Cage must block /api/herdr with 423.
	s.account.lock()
	if w = request(s, "GET", "/api/herdr", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked Cage exposed /api/herdr: %d", w.Code)
	}
}

func TestMagpieTakeoverDisablesAndRestoresAllAccountScheduling(t *testing.T) {
	s := testServer(t)
	s.ready = true

	claudeID := strings.Repeat("a", 24)
	codexID := strings.Repeat("b", 24)
	ocID := strings.Repeat("c", 24)

	s.claudeAccounts.mu.Lock()
	_ = s.saveClaudeAccounts(claudeAccountList{
		Accounts:   []claudeAccount{{ID: claudeID, Label: "Claude Pro", Source: "managed", Email: "c@example.test", Enabled: true, Status: "ready"}},
		Scheduling: (claudeScheduling{}).defaults(),
	})
	s.claudeAccounts.mu.Unlock()
	s.claudeAccounts.probe = func(_ context.Context, id string) (claudeGuestSnapshot, error) {
		if id == claudeID {
			return claudeGuestSnapshot{LoggedIn: true, Email: "c@example.test"}, nil
		}
		return claudeGuestSnapshot{}, nil
	}

	s.codexAccounts.mu.Lock()
	_ = s.saveCodexAccounts(codexAccountList{
		Accounts:   []codexAccount{{ID: codexID, Label: "Codex Plus", Email: "x@example.test", Enabled: true, Status: "ready"}},
		Scheduling: (claudeScheduling{}).defaults(),
	})
	s.codexAccounts.mu.Unlock()
	s.codexAccounts.probe = func(_ context.Context, id string) (codexGuestSnapshot, error) {
		if id == codexID {
			return codexGuestSnapshot{LoggedIn: true, Email: "x@example.test"}, nil
		}
		return codexGuestSnapshot{}, nil
	}

	s.agentCLIs.mu.Lock()
	_ = s.saveAgentPoolsLocked(map[string]*agentAccountPool{
		"opencode": {
			Scheduling: defaultAgentScheduling(),
			Accounts:   []agentAccount{{ID: ocID, Label: "OpenCode Key", Source: "api_key", AuthMode: "api_key", Enabled: true, Status: "ready", Weight: 10, HealthEWMA: 1.0}},
		},
	})
	s.agentCLIs.mu.Unlock()

	var syncMu sync.Mutex
	var syncCalls []map[string]any
	resetSyncCalls := func() {
		syncMu.Lock()
		syncCalls = nil
		syncMu.Unlock()
	}
	snapshotSyncCalls := func() []map[string]any {
		syncMu.Lock()
		defer syncMu.Unlock()
		out := make([]map[string]any, len(syncCalls))
		copy(out, syncCalls)
		return out
	}
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, _ any) error {
		if subcmd == "sync-auto-shims" {
			if m, ok := input.(map[string]any); ok {
				cp := make(map[string]any, len(m))
				for k, v := range m {
					cp[k] = v
				}
				syncMu.Lock()
				syncCalls = append(syncCalls, cp)
				syncMu.Unlock()
			}
		}
		return nil
	}

	ocSpec, _, ok := s.lookupAgentSpec("opencode")
	if !ok {
		t.Fatal("opencode built-in spec missing")
	}

	// 1. Before Magpie is enabled, Cage schedules accounts normally.
	if sel, _, err := s.selectAgentAccount(ocSpec, "", false, "/proj"); err != nil || sel.ID != ocID {
		t.Fatalf("pre-magpie selectAgentAccount = %+v err=%v, want %s", sel, err, ocID)
	}
	if gb, labels, err := s.resolveToolAccountBindings(map[string]string{
		"claude":   claudeID,
		"codex":    codexID,
		"opencode": "auto",
	}, "/proj", "Orca Server"); err != nil {
		t.Fatalf("pre-magpie resolveToolAccountBindings failed: %v", err)
	} else if labels["claude"] != "Claude Pro" || labels["codex"] != "Codex Plus" || gb["opencode"].(map[string]any)["mode"] != "auto" {
		t.Fatalf("unexpected pre-magpie tool bindings: gb=%+v labels=%+v", gb, labels)
	}

	// 2. Enabling Magpie immediately clears guest scheduling and hands over ALL harnesses.
	resetSyncCalls()
	w := request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":true,"mode":"managed"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("enabling magpie failed: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"scheduling_disabled":true`) {
		t.Fatalf("magpie status missing scheduling_disabled=true: %s", w.Body.String())
	}
	calls := snapshotSyncCalls()
	sawDisable := false
	for _, call := range calls {
		if call["disable_all"] == true && call["magpie_takeover"] == true {
			sawDisable = true
			break
		}
	}
	if !sawDisable {
		t.Fatalf("enabling magpie did not invoke sync-auto-shims disable_all+magpie_takeover: %+v", calls)
	}
	if !s.allLaunchPublishedMagpie() {
		t.Fatal("expected allLaunchPublishedMagpie=true after enabling magpie")
	}

	// Claude, Codex, and generic Agent CLI selection all bypass scheduling while preserving explicit login.
	if sel, err := s.selectClaudeAccount("", false, "/proj"); err != nil || sel != (claudeSelection{}) {
		t.Fatalf("magpie selectClaudeAccount = %+v err=%v, want empty", sel, err)
	}
	if sel, err := s.selectTerminalClaudeAccount("claude", "", false, "/proj"); err != nil || sel != (claudeSelection{}) {
		t.Fatalf("magpie selectTerminalClaudeAccount = %+v err=%v, want empty", sel, err)
	}
	if sel, err := s.selectCodexAccount("", false, "/proj"); err != nil || sel != (codexSelection{}) {
		t.Fatalf("magpie selectCodexAccount = %+v err=%v, want empty", sel, err)
	}
	if sel, dec, err := s.selectAgentAccount(ocSpec, ocID, false, "/proj"); err != nil || sel.ID != "" || sel.GuestID != "default" || dec.Strategy != "magpie" {
		t.Fatalf("magpie selectAgentAccount = %+v dec=%+v err=%v, want default+magpie", sel, dec, err)
	}
	if loginSel, _, err := s.selectAgentAccount(ocSpec, "", true, "/proj"); err != nil || loginSel.ID == "" {
		t.Fatalf("explicit agent login broken under magpie: %+v err=%v", loginSel, err)
	}

	// T3 Code & Orca Server tool bindings switch to mode="magpie" (even for previously pinned IDs).
	gb, labels, err := s.resolveToolAccountBindings(map[string]string{
		"claude":   claudeID,
		"codex":    codexID,
		"opencode": "auto",
		"gemini":   "default",
	}, "/proj", "Orca Server")
	if err != nil {
		t.Fatalf("magpie resolveToolAccountBindings failed: %v", err)
	}
	for _, agent := range []string{"claude", "codex", "opencode", "gemini"} {
		m, _ := gb[agent].(map[string]any)
		if m["mode"] != "magpie" || labels[agent] != magpieTakeoverLabel {
			t.Fatalf("magpie binding[%s] = %+v label=%q, want mode=magpie label=%q", agent, m, labels[agent], magpieTakeoverLabel)
		}
	}

	// All status APIs report magpie_takeover=true and preserve saved accounts untouched.
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/claude-accounts", `"magpie_takeover":true`},
		{"/api/codex-accounts", `"magpie_takeover":true`},
		{"/api/agent-clis/opencode/accounts?skip_detect=1", `"magpie_takeover":true`},
		{"/api/agent-clis/opencode/accounts?skip_detect=1", `"strategy":"magpie"`},
		{"/api/t3code", `"magpie_takeover":true`},
		{"/api/orca", `"magpie_takeover":true`},
	} {
		resp := request(s, "GET", tc.path, "", "", true)
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), tc.want) {
			t.Fatalf("GET %s = %d %s, want %s", tc.path, resp.Code, resp.Body.String(), tc.want)
		}
	}

	// Cold-start stale-file protection: resetting launchPublished to "" while Magpie is enabled
	// must still run disableGuestAccountScheduling once across all 3 publishers.
	resetSyncCalls()
	s.markAllLaunchPublished("")
	s.publishClaudeLaunchSchedule()
	s.publishCodexLaunchSchedule()
	s.publishAgentLaunchSchedule()
	calls = snapshotSyncCalls()
	if len(calls) != 1 || calls[0]["disable_all"] != true {
		t.Fatalf("cold-start publish under magpie ran %d sync calls (%+v), want 1 coalesced disable_all", len(calls), calls)
	}

	// 3. Disabling Magpie clears takeover marker and immediately restores Cage scheduling over preserved accounts.
	resetSyncCalls()
	w = request(s, "POST", "/api/magpie/config", s.origin, `{"enabled":false}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"scheduling_disabled":false`) {
		t.Fatalf("disabling magpie failed: %d %s", w.Code, w.Body.String())
	}
	calls = snapshotSyncCalls()
	sawClear := false
	for _, call := range calls {
		if call["magpie_takeover"] == false {
			sawClear = true
			break
		}
	}
	if !sawClear {
		t.Fatalf("disabling magpie did not clear guest takeover marker: %+v", calls)
	}
	if sel, _, err := s.selectAgentAccount(ocSpec, "", false, "/proj"); err != nil || sel.ID != ocID {
		t.Fatalf("post-magpie selectAgentAccount = %+v err=%v, want restored %s", sel, err, ocID)
	}
	if gb, labels, err := s.resolveToolAccountBindings(map[string]string{
		"claude":   claudeID,
		"codex":    codexID,
		"opencode": "auto",
	}, "/proj", "T3 Code"); err != nil {
		t.Fatalf("post-magpie resolveToolAccountBindings failed: %v", err)
	} else if labels["claude"] != "Claude Pro" || labels["codex"] != "Codex Plus" || gb["opencode"].(map[string]any)["mode"] != "auto" {
		t.Fatalf("post-magpie bindings not restored: gb=%+v labels=%+v", gb, labels)
	}
}
