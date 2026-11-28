package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTerminalLoginLinkUsesGuestPrivateBrowser(t *testing.T) {
	const address = "https://claude.com/cai/oauth/authorize?state=fixture&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback"
	s := testServer(t)
	s.ready = true
	terminal := newTerminalSession(nil)
	terminal.id = "1234567890abcdef"
	s.terminals.sessions = map[string]*terminalSession{terminal.id: terminal}
	finished := make(chan struct{})
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ URL, Login string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Login != terminal.id {
			t.Error("incorrect login request")
		}
		switch r.URL.Path {
		case "/login-open":
			if in.URL != address {
				t.Error("lost authorization parameters")
			}
			_, _ = w.Write([]byte(`{"id":"tab","url":"private"}`))
		case "/login-finish":
			if in.URL != "" {
				t.Error("cleanup exposed URL")
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
			close(finished)
		default:
			t.Error("unexpected browser route")
		}
	}))
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()
	w := request(s, "POST", "/api/terminals/"+terminal.id+"/login-link", s.origin, `{"url":"`+address+`"}`, true)
	if w.Code != 200 || w.Body.String() != "{\"id\":\"tab\"}\n" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	close(terminal.done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the terminal retained its login tab")
	}
}

func TestTerminalLoginLinkReportsOnlyKnownGuestNavigationFailure(t *testing.T) {
	const address = "https://claude.ai/oauth/authorize?state=private-fixture"
	for _, tc := range []struct{ name, guestError, want string }{
		{"navigation", "授权页导航失败", "Chromium 未能加载授权页，请检查环境网络后重试"},
		{"unknown", "Navigation failed: " + address, "Cage 浏览器未能打开授权页，请检查浏览器连接并重试"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			s.ready = true
			terminal := newTerminalSession(nil)
			terminal.id = "1234567890abcdef"
			terminal.loginGuestID = "aaaaaaaaaaaaaaaaaaaaaaaa"
			s.terminals.sessions = map[string]*terminalSession{terminal.id: terminal}
			guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.guestError})
			}))
			defer guest.Close()
			s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
			defer s.browserControl.close()
			w := request(s, "POST", "/api/terminals/"+terminal.id+"/login-link", s.origin, `{"url":"`+address+`"}`, true)
			if w.Code != http.StatusBadGateway || w.Body.String() != tc.want+"\n" {
				t.Fatalf("unexpected response: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestClaudeAuthorizationURL(t *testing.T) {
	for _, valid := range []string{
		"https://claude.ai/oauth/authorize?x=1",
		"https://claude.com/cai/oauth/authorize/?x=1",
	} {
		if !claudeAuthorizationURL(valid) {
			t.Fatalf("rejected %q", valid)
		}
	}
	for _, invalid := range []string{
		"https://example.org/cai/oauth/authorize?x=1",
		"https://claude.com/extra/cai/oauth/authorize?x=1",
		"https://claude.com/cai/oauth/authorize/next?x=1",
		"http://claude.ai/oauth/authorize?x=1",
		"https://claude.ai:90/oauth/authorize?x=1",
		"https://claude.ai@elsewhere.test/oauth/authorize?x=1",
		"https://claude.ai/redirect?x=1",
		"https://claude.ai/oauth/authorize",
	} {
		if claudeAuthorizationURL(invalid) {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
