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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestT3CodeAccountsStayBoundAndReleaseOnStop(t *testing.T) {
	t.Run("managed auto", func(t *testing.T) {
		testT3CodeAccountsStayBoundAndReleaseOnStop(t, "managed", "auto")
	})
	t.Run("registered explicit default", func(t *testing.T) {
		testT3CodeAccountsStayBoundAndReleaseOnStop(t, "default", "default")
	})
}

func testT3CodeAccountsStayBoundAndReleaseOnStop(t *testing.T, source, choice string) {
	t.Helper()
	s := testServer(t)
	workspace := t.TempDir()
	if err := writePrivate(filepath.Join(s.profile, "environment.json"), cage.EnvironmentRecord{Directory: workspace}); err != nil {
		t.Fatal(err)
	}
	instance := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, workspace))
	if err := os.MkdirAll(instance, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(instance, "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	const agentID = "opencode"
	assigned := strings.Repeat("a", 24)
	s.agentCLIs.mu.Lock()
	err := s.saveAgentPoolsLocked(map[string]*agentAccountPool{agentID: {
		Accounts: []agentAccount{{ID: assigned, Source: source, Enabled: true, Status: "ready", MaxConcurrency: 1}},
	}})
	s.agentCLIs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeAgents)
	cfg := defaultT3CodeConfig()
	cfg.Enabled = true
	cfg.AccountBindings[agentID] = choice
	s.t3code.mu.Lock()
	err = s.saveT3CodeConfigLocked(cfg)
	s.t3code.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	status := t3CodeGuestStatus{Port: cfg.Port}
	var starts, stops int
	failRestart := false
	s.agentCLIs.runtimeHook = func(_ context.Context, verb string, input, output any) error {
		switch verb {
		case "t3code-status":
			*output.(*t3CodeGuestStatus) = status
		case "t3code-start", "t3code-restart":
			if s.op.TryLock() {
				s.op.Unlock()
				t.Fatal("T3 was started outside the environment lock")
			}
			starts++
			binding := input.(map[string]any)["bindings"].(map[string]any)[agentID].(map[string]any)
			if binding["account_id"] != assigned ||
				(source == "default" && binding["mode"] != "default") ||
				(source != "default" && binding["guest_id"] != assigned) {
				t.Fatalf("wrong provider binding: %v", binding)
			}
			if failRestart {
				return errors.New("mock restart failed")
			}
			status = t3CodeGuestStatus{Running: true, Port: cfg.Port, PID: 1234, Accounts: map[string]string{agentID: assigned}}
			*output.(*t3CodeGuestStatus) = status
		case "t3code-stop":
			stops++
			status = t3CodeGuestStatus{Port: cfg.Port}
		}
		return nil
	}
	for _, restart := range []bool{false, false, true} {
		if _, _, err := s.ensureManagedT3CodeRunning(context.Background(), restart, ""); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 2 || s.activeT3CodeAccounts(agentID)[assigned] != 1 {
		t.Fatalf("overcount or extra launch: starts=%d active=%v", starts, s.activeT3CodeAccounts(agentID))
	}
	spec, _, _ := s.lookupAgentSpec(agentID)
	atCapacity := []string{"auto", assigned}
	if choice == "default" {
		atCapacity = append(atCapacity, "default")
	}
	for _, choice := range atCapacity {
		if _, _, err := s.selectAgentAccount(spec, choice, false, workspace); err == nil {
			t.Fatalf("selection %q reused the only slot held by T3", choice)
		}
	}
	s.t3code.mu.Lock()
	s.t3code.statusAt = time.Time{}
	s.t3code.statusCache = t3CodeGuestStatus{}
	s.t3code.mu.Unlock()
	if s.activeT3CodeAccounts(agentID)[assigned] != 1 {
		t.Fatal("controller did not recover the running service's account binding")
	}
	failRestart = true
	if _, _, err := s.ensureManagedT3CodeRunning(context.Background(), true, ""); err == nil ||
		s.activeT3CodeAccounts(agentID)[assigned] != 1 {
		t.Fatal("failed restart released the still-running service's reservation")
	}
	if err := s.stopManagedT3Code(context.Background(), cfg.Port); err != nil || stops != 1 || len(s.activeT3CodeAccounts(agentID)) != 0 {
		t.Fatalf("stop failed to release the reservation: stops=%d err=%v", stops, err)
	}
	if _, _, err := s.selectAgentAccount(spec, "auto", false, workspace); err != nil {
		t.Fatalf("stopped service still blocks terminal admission: %v", err)
	}
}

func TestT3CodeUnpooledDefaultCountsAfterRegistration(t *testing.T) {
	s := testServer(t)
	const agentID = "opencode"
	cfg := defaultT3CodeConfig()
	cfg.Enabled = true
	cfg.AccountBindings[agentID] = "default"
	s.t3code.mu.Lock()
	err := s.saveT3CodeConfigLocked(cfg)
	s.t3code.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	bindings, _, err := s.resolveT3CodeBindings(cfg, "")
	if err != nil {
		t.Fatalf("unpooled default was rejected: %v", err)
	}
	if binding := bindings[agentID].(map[string]any); binding["mode"] != "default" || binding["account_id"] != nil {
		t.Fatalf("unpooled default gained a registered account ID: %v", binding)
	}

	// The guest persists the default marker before the account is enrolled.
	s.agentCLIs.runtimeHook = func(_ context.Context, verb string, _, output any) error {
		if verb == "t3code-status" {
			*output.(*t3CodeGuestStatus) = t3CodeGuestStatus{
				Running: true, Port: cfg.Port, PID: 1234, Accounts: map[string]string{agentID: "default"},
			}
		}
		return nil
	}
	spec, _, _ := s.lookupAgentSpec(agentID)
	if sel, _, err := s.selectAgentAccount(spec, "default", false, ""); err != nil || sel.ID != "" || sel.GuestID != "default" {
		t.Fatalf("the unregistered default should remain available: %+v %v", sel, err)
	}
	registered := strings.Repeat("b", 24)
	s.agentCLIs.mu.Lock()
	err = s.saveAgentPoolsLocked(map[string]*agentAccountPool{agentID: {
		Accounts: []agentAccount{{ID: registered, Source: "default", Enabled: true, Status: "ready", MaxConcurrency: 1}},
	}})
	s.agentCLIs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range []string{"auto", "default", registered} {
		if _, _, err := s.selectAgentAccount(spec, choice, false, ""); err == nil {
			t.Fatalf("selection %q reused a registered default already held by T3", choice)
		}
	}
	replacement, _, err := s.resolveT3CodeLaunchBindings(cfg, "", map[string]string{agentID: "default"})
	if err != nil || replacement[agentID].(map[string]any)["account_id"] != registered {
		t.Fatalf("restart could not replace its original default seat: %v, %v", replacement, err)
	}
}

func TestT3CodePoolFailureRequiresExplicitDefault(t *testing.T) {
	s := testServer(t)
	claude, codex := strings.Repeat("a", 24), strings.Repeat("b", 24)
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{{ID: claude, Status: "signed_out"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{{ID: codex, Status: "signed_out"}}}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultT3CodeConfig()
	for _, id := range []string{"claude", "codex"} {
		cfg.AccountBindings = map[string]string{"claude": "default", "codex": "default", id: "auto"}
		if _, _, err := s.resolveT3CodeBindings(cfg, ""); err == nil {
			t.Fatalf("ineligible %s pool silently fell back", id)
		}
		cfg.AccountBindings[id] = "default"
		bindings, _, err := s.resolveT3CodeBindings(cfg, "")
		if err != nil || bindings[id].(map[string]any)["mode"] != "default" {
			t.Fatalf("explicit %s default was rejected: %v", id, err)
		}
	}
}

func TestT3CodeConfigLifecycleAndSecurityBoundaries(t *testing.T) {
	s := testServer(t)
	s.ready = true

	// Disabled by default (zero overhead until opted in).
	w := request(s, "GET", "/api/t3code", "", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/t3code: %d %s", w.Code, w.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["enabled"] != false || st["mode"] != "managed" || st["open_mode"] != "workbench" {
		t.Fatalf("unexpected default t3code status: %+v", st)
	}
	if st["supports_managed"] != true || st["direct_available"] != true {
		t.Fatalf("expected supports_managed & direct_available true by default: %+v", st)
	}

	// Remote SSH client & native desktop WebView requests must see direct_available=false.
	remoteReq := httptest.NewRequest("GET", s.origin+"/api/t3code", nil)
	remoteReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	remoteReq.Header.Set("X-Cage-Remote-Client", "1")
	rw := httptest.NewRecorder()
	s.handler().ServeHTTP(rw, remoteReq)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"direct_available":false`) {
		t.Fatalf("remote client did not disable direct_available: %d %s", rw.Code, rw.Body.String())
	}
	desktopReq := httptest.NewRequest("GET", s.origin+"/api/t3code", nil)
	desktopReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	desktopReq.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	dw := httptest.NewRecorder()
	s.handler().ServeHTTP(dw, desktopReq)
	if dw.Code != http.StatusOK || !strings.Contains(dw.Body.String(), `"direct_available":false`) {
		t.Fatalf("desktop WebView did not disable direct_available: %d %s", dw.Code, dw.Body.String())
	}

	// Start/restart must reject disabled managed config.
	if sw := request(s, "POST", "/api/t3code/start", s.origin, `{}`, true); sw.Code != http.StatusConflict {
		t.Fatalf("disabled managed start accepted: %d %s", sw.Code, sw.Body.String())
	}

	// Verify Linux SSH local forward args place PermitLocalCommand=yes & ClearAllForwardings=no FIRST.
	fwdCmd := s.sshLocalForwardCommand(context.Background(), "127.0.0.1:45678", 13773)
	if slices.Contains(fwdCmd.Args, "PermitLocalCommand=no") || !slices.Contains(fwdCmd.Args, "PermitLocalCommand=yes") {
		t.Fatalf("sshLocalForwardCommand has wrong PermitLocalCommand precedence: %v", fwdCmd.Args)
	}
	if slices.Contains(fwdCmd.Args, "ClearAllForwardings=yes") || !slices.Contains(fwdCmd.Args, "ClearAllForwardings=no") {
		t.Fatalf("sshLocalForwardCommand has wrong ClearAllForwardings precedence: %v", fwdCmd.Args)
	}

	// Reject non-loopback plain HTTP external URLs.
	w = request(s, "POST", "/api/t3code/config", s.origin, `{"enabled":true,"mode":"external","external_url":"http://remote.example.com:3773"}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("plain HTTP remote URL accepted: %d %s", w.Code, w.Body.String())
	}

	// Reject out-of-range port.
	w = request(s, "POST", "/api/t3code/config", s.origin, `{"port":80}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("privileged port accepted: %d %s", w.Code, w.Body.String())
	}

	// Accept valid HTTPS external URL & account bindings.
	w = request(s, "POST", "/api/t3code/config", s.origin, `{"enabled":true,"mode":"external","external_url":"https://t3.example.test","open_mode":"direct","focus_chrome":true,"account_bindings":{"claude":"auto","codex":"auto"}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("valid t3code config rejected: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), `"open_mode":"direct"`) {
		t.Fatalf("unexpected saved config response: %s", w.Body.String())
	}

	// Verify explicit account binding validation and Claude subject hash forwarding.
	badCfg := defaultT3CodeConfig()
	badCfg.AccountBindings = map[string]string{"claude": strings.Repeat("f", 24)}
	if _, _, err := s.resolveT3CodeBindings(badCfg, "/proj"); err == nil {
		t.Fatal("expected missing explicit Claude binding to fail loudly instead of silently falling back")
	}
	claudeID := strings.Repeat("a", 24)
	s.claudeAccounts.mu.Lock()
	_ = s.saveClaudeAccounts(claudeAccountList{
		Accounts: []claudeAccount{{
			ID:           claudeID,
			Label:        "Work Claude",
			Source:       "managed",
			Email:        "work@example.com",
			Organization: "acme",
			Enabled:      true,
			Status:       "ready",
		}},
		Scheduling: (claudeScheduling{}).defaults(),
	})
	s.claudeAccounts.mu.Unlock()
	goodCfg := defaultT3CodeConfig()
	goodCfg.AccountBindings = map[string]string{"claude": claudeID}
	bindings, labels, err := s.resolveT3CodeBindings(goodCfg, "/proj")
	if err != nil {
		t.Fatalf("valid Claude binding failed: %v", err)
	}
	claudeBind, _ := bindings["claude"].(map[string]any)
	wantSubj := claudeIdentityHash("work@example.com", "acme")
	if claudeBind["guest_id"] != claudeID || claudeBind["subject"] != wantSubj || labels["claude"] != "Work Claude" {
		t.Fatalf("unexpected resolved Claude binding: %+v labels=%+v", claudeBind, labels)
	}

	// Verify workbench mode reuses ONLY exact-origin tab and does NOT consume pair URL when reusing.
	var openURLCalls atomic.Int32
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		if subcmd == "t3code-open-url" {
			openURLCalls.Add(1)
			return json.Unmarshal([]byte(`{"url":"http://127.0.0.1:13773/pair#token=secret"}`), output)
		}
		return nil
	}
	var activatedID, createdURL string
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tabs":
			_, _ = w.Write([]byte(`{"tabs":[{"id":"tab-other","kind":"t3code","url":"http://127.0.0.1:19999/"},{"id":"tab-match","kind":"web","url":"https://t3.example.test/threads/1"}]}`))
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

	// Remote client requesting force_mode="direct" must be forced to workbench mode and reuse exact-origin tab!
	openReq := httptest.NewRequest("POST", s.origin+"/api/t3code/open", strings.NewReader(`{"force_mode":"direct"}`))
	openReq.Header.Set("Origin", s.origin)
	openReq.Header.Set("Content-Type", "application/json")
	openReq.Header.Set("X-Cage-Remote-Client", "1")
	openReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	ow := httptest.NewRecorder()
	s.handler().ServeHTTP(ow, openReq)
	if ow.Code != http.StatusOK || !strings.Contains(ow.Body.String(), `"mode":"workbench"`) || activatedID != "tab-match" {
		t.Fatalf("remote fallback / exact-origin tab reuse failed: %d %s activated=%q created=%q", ow.Code, ow.Body.String(), activatedID, createdURL)
	}
	if openURLCalls.Load() != 0 {
		t.Fatalf("reusing existing tab unexpectedly consumed open URL (%d calls)", openURLCalls.Load())
	}

	// Locked Cage must block /api/t3code with 423.
	s.account.lock()
	if w = request(s, "GET", "/api/t3code", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked Cage exposed /api/t3code: %d", w.Code)
	}
}

func TestT3CodeDirectProxyAuthCookieWebSocketAndLockRevocation(t *testing.T) {
	var sawXFH, sawWSOrigin string
	// Mock an original T3 Code server with HTTP + raw WebSocket echo that stays open.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ensure Cage's proxy auth cookie is stripped before reaching T3 Code.
		if strings.Contains(r.Header.Get("Cookie"), "cage_t3_session_") {
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
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("T3_ORIGINAL_WEB:" + r.URL.Path))
	}))
	defer upstream.Close()

	s := testServer(t)
	s.ready = true
	defer s.closeT3CodeProxy()

	w := request(s, "POST", "/api/t3code/config", s.origin, `{"enabled":true,"mode":"external","external_url":"`+upstream.URL+`","open_mode":"direct"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("config failed: %d %s", w.Code, w.Body.String())
	}

	// Mint two direct grants: one to redeem now, one to stay unused across lock/unlock.
	openOnce := func() string {
		t.Helper()
		resp := request(s, "POST", "/api/t3code/open", s.origin, `{"force_mode":"direct"}`, true)
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
		if openResp.Mode != "direct" || !strings.Contains(openResp.DirectURL, "/_cage_t3_auth?grant=") {
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
	wantCookieName := "cage_t3_session_" + parsed.Port()

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

	// 2. Redeem single-use grant -> receives unique-per-port HttpOnly SameSite=Strict cookie + 302 to /.
	resp, err = noRedirect.Get(directURL1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("grant exchange failed: status=%d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != wantCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected proxy cookie: %+v (want name=%q HttpOnly SameSite=Strict)", cookies, wantCookieName)
	}

	// 3. Replaying the same grant must fail with 401.
	replay, err := noRedirect.Get(directURL1)
	if err != nil {
		t.Fatal(err)
	}
	_ = replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed grant accepted: %d", replay.StatusCode)
	}

	// 4. Foreign Host (DNS rebinding) & foreign Origin on POST (loopback CSRF) must be rejected with 403.
	badHostReq, _ := http.NewRequest("GET", proxyBase+"/", nil)
	badHostReq.Host = "evil.rebind.test:" + parsed.Port()
	badHostReq.AddCookie(cookies[0])
	badHostResp, err := noRedirect.Do(badHostReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badHostResp.Body.Close()
	if badHostResp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host accepted: %d", badHostResp.StatusCode)
	}

	badOriginReq, _ := http.NewRequest("POST", proxyBase+"/api/mutate", strings.NewReader(`{}`))
	badOriginReq.Header.Set("Origin", "http://127.0.0.1:9999")
	badOriginReq.AddCookie(cookies[0])
	badOriginResp, err := noRedirect.Do(badOriginReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badOriginResp.Body.Close()
	if badOriginResp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin on POST accepted: %d", badOriginResp.StatusCode)
	}

	// 5. Authenticated request with cookie strips ALL untrusted X-Forwarded-* headers and sets canonical X-Forwarded-Host.
	req, _ := http.NewRequest("GET", proxyBase+"/threads/123", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Custom", "spoofed")
	req.Header.Set("Forwarded", "for=1.2.3.4")
	req.AddCookie(cookies[0])
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "T3_ORIGINAL_WEB:/threads/123" || sawXFH != parsed.Host {
		t.Fatalf("proxied HTTP mismatch: status=%d body=%q xfh=%q", resp.StatusCode, string(body), sawXFH)
	}

	// 6. WebSocket upgrade with foreign Origin is refused (403); exact proxy Origin succeeds (101) and aligns upstream Origin.
	badWSReq, _ := http.NewRequest("GET", proxyBase+"/ws", nil)
	badWSReq.Header.Set("Connection", "Upgrade")
	badWSReq.Header.Set("Upgrade", "websocket")
	badWSReq.Header.Set("Origin", "http://127.0.0.1:9999")
	badWSReq.AddCookie(cookies[0])
	badWSResp, err := noRedirect.Do(badWSReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badWSResp.Body.Close()
	if badWSResp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin on WS upgrade accepted: %d", badWSResp.StatusCode)
	}

	wsReq, _ := http.NewRequest("GET", proxyBase+"/ws", nil)
	wsReq.Header.Set("Connection", "Upgrade")
	wsReq.Header.Set("Upgrade", "websocket")
	wsReq.Header.Set("Origin", proxyBase)
	wsReq.AddCookie(cookies[0])
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
	_, _ = conn.Write([]byte("ping-t3\n"))
	echoLine, err := br.ReadString('\n')
	if err != nil || echoLine != "echo:ping-t3\n" {
		t.Fatalf("websocket pipe mismatch: %q %v", echoLine, err)
	}

	// 7. Locking Cage IMMEDIATELY closes the established live WebSocket connection (NOT a timeout)!
	s.account.lock()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, readErr := br.ReadByte()
	if readErr == nil {
		t.Fatal("established WebSocket survived Cage lock!")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("established WebSocket timed out instead of closing immediately on lock: %v", readErr)
	}

	// 8. Re-unlocking Cage with a new key MUST NOT revive the old proxy, old cookie, or unused pre-lock grant!
	s.account.mu.Lock()
	s.account.setKey(make([]byte, 32))
	s.account.mu.Unlock()

	if respOld, err := noRedirect.Get(unusedGrantURL); err == nil {
		_ = respOld.Body.Close()
		t.Fatalf("unused pre-lock grant survived lock+unlock with status %d!", respOld.StatusCode)
	}
	reqOldCookie, _ := http.NewRequest("GET", proxyBase+"/", nil)
	reqOldCookie.AddCookie(cookies[0])
	if respOld, err := noRedirect.Do(reqOldCookie); err == nil {
		_ = respOld.Body.Close()
		t.Fatalf("pre-lock proxy cookie survived lock+unlock with status %d!", respOld.StatusCode)
	}
}
