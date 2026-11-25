package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func runningAgentTestServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	s.ready = true
	marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if !s.vmRunning() {
		t.Fatal("running VM fixture not active")
	}
	return s
}

type agentAccountsHTTPResponse struct {
	Accounts        []agentAccount   `json:"accounts"`
	SchedulePreview scheduleDecision `json:"schedule_preview"`
	DetectionStatus string           `json:"detection_status"`
	DetectionError  string           `json:"detection_error"`
	LoginNotice     string           `json:"login_notice"`
}

func decodeAgentAccountsResp(t *testing.T, code int, body []byte) agentAccountsHTTPResponse {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("unexpected HTTP status %d: %s", code, string(body))
	}
	var resp agentAccountsHTTPResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode agent accounts response: %v (body=%s)", err, string(body))
	}
	return resp
}

// TestAgentAccountEnabledStableWhenRuntimeFails verifies that failing
// runAgentRuntime() calls (e.g., Windows CreateProcess 32,767-char argv
// overflow) never cause an account's Enabled state or schedule_preview to flap
// across repeated GET/POST/catalog requests.
func TestAgentAccountEnabledStableWhenRuntimeFails(t *testing.T) {
	s := runningAgentTestServer(t)
	accID := strings.Repeat("a", 24)

	s.agentCLIs.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		s.agentCLIs.mu.Unlock()
		t.Fatal(err)
	}
	pool := s.ensureAgentPoolLocked(pools, "antigravity")
	pool.Accounts = []agentAccount{{
		ID:         accID,
		Label:      "Antigravity 默认登录",
		Source:     "default",
		AuthMode:   "oauth",
		Enabled:    true,
		Status:     "ready",
		Weight:     10,
		HealthEWMA: 1.0,
	}}
	if err := s.saveAgentPoolsLocked(pools); err != nil {
		s.agentCLIs.mu.Unlock()
		t.Fatal(err)
	}
	s.agentCLIs.mu.Unlock()

	runtimeErr := errors.New("exec: CreateProcess: The filename or extension is too long")
	s.agentCLIs.runtimeHook = func(_ context.Context, _ string, _ any, _ any) error {
		return runtimeErr
	}

	assertConsistent := func(wantEnabled bool, contextLabel string) {
		t.Helper()
		for i, path := range []string{
			"/api/agent-clis/antigravity/accounts",
			"/api/agent-clis/antigravity/accounts?refresh=1",
			"/api/agent-clis/antigravity/accounts",
		} {
			// Also hit /api/agent-clis between reads to ensure catalog refresh never mutates Enabled.
			if cw := request(s, "GET", "/api/agent-clis", "", "", true); cw.Code != http.StatusOK {
				t.Fatalf("%s[%d]: GET /api/agent-clis failed: %d", contextLabel, i, cw.Code)
			}
			w := request(s, "GET", path, "", "", true)
			resp := decodeAgentAccountsResp(t, w.Code, w.Body.Bytes())
			if len(resp.Accounts) != 1 {
				t.Fatalf("%s[%d]: want 1 account, got %d", contextLabel, i, len(resp.Accounts))
			}
			if resp.Accounts[0].Enabled != wantEnabled {
				t.Fatalf("%s[%d]: enabled flapped to %v, want %v", contextLabel, i, resp.Accounts[0].Enabled, wantEnabled)
			}
			if resp.DetectionStatus != "unavailable" {
				t.Fatalf("%s[%d]: detection_status=%q, want unavailable", contextLabel, i, resp.DetectionStatus)
			}
			if wantEnabled && resp.SchedulePreview.AccountID != accID {
				t.Fatalf("%s[%d]: enabled=true but preview picked %q (%s)", contextLabel, i, resp.SchedulePreview.AccountID, resp.SchedulePreview.Reason)
			}
			if !wantEnabled && resp.SchedulePreview.AccountID != "" {
				t.Fatalf("%s[%d]: enabled=false but preview still picked %q", contextLabel, i, resp.SchedulePreview.AccountID)
			}
		}
	}

	assertConsistent(true, "initial-enabled")

	// Toggle disabled.
	w := request(s, "POST", "/api/agent-clis/antigravity/accounts", s.origin,
		`{"action":"update","id":"`+accID+`","enabled":false}`, true)
	resp := decodeAgentAccountsResp(t, w.Code, w.Body.Bytes())
	if resp.Accounts[0].Enabled || resp.SchedulePreview.AccountID != "" {
		t.Fatalf("after disable: enabled=%v preview=%+v", resp.Accounts[0].Enabled, resp.SchedulePreview)
	}
	assertConsistent(false, "after-disable")

	// Toggle enabled again.
	w = request(s, "POST", "/api/agent-clis/antigravity/accounts", s.origin,
		`{"action":"update","id":"`+accID+`","enabled":true}`, true)
	resp = decodeAgentAccountsResp(t, w.Code, w.Body.Bytes())
	if !resp.Accounts[0].Enabled || resp.SchedulePreview.AccountID != accID {
		t.Fatalf("after re-enable: enabled=%v preview=%+v", resp.Accounts[0].Enabled, resp.SchedulePreview)
	}
	assertConsistent(true, "after-re-enable")
}

// TestConfirmAgentLoginErrorSurfacesNoticeAndPreservesPendingLogin verifies that
// when snapshot fails (e.g. Windows CreateProcess argv overflow), confirmAgentLogin
// surfaces an explicit error notice instead of silently dropping it or reporting
// "未检测到新登录凭据", keeps PendingLogin intact, and recovers once runtime works.
func TestConfirmAgentLoginErrorSurfacesNoticeAndPreservesPendingLogin(t *testing.T) {
	s := runningAgentTestServer(t)
	pendingID := strings.Repeat("b", 24)

	s.agentCLIs.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		s.agentCLIs.mu.Unlock()
		t.Fatal(err)
	}
	pool := s.ensureAgentPoolLocked(pools, "antigravity")
	pool.PendingLogin = pendingID
	if err := s.saveAgentPoolsLocked(pools); err != nil {
		s.agentCLIs.mu.Unlock()
		t.Fatal(err)
	}
	s.agentCLIs.mu.Unlock()

	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, _ any, _ any) error {
		if subcmd == "snapshot" {
			return errors.New("CreateProcess: command line too long")
		}
		return nil
	}

	if err := s.confirmAgentLogin(context.Background(), "antigravity", pendingID); err == nil {
		t.Fatal("expected confirmAgentLogin to return snapshot error")
	}

	// GET should show explicit credential-read error, NOT empty and NOT "未检测到新登录凭据".
	w := request(s, "GET", "/api/agent-clis/antigravity/accounts", "", "", true)
	resp := decodeAgentAccountsResp(t, w.Code, w.Body.Bytes())
	if !strings.Contains(resp.LoginNotice, "无法读取登录凭据") || !strings.Contains(resp.LoginNotice, "command line too long") {
		t.Fatalf("expected explicit read-failure notice, got %q", resp.LoginNotice)
	}
	if strings.Contains(resp.LoginNotice, "未检测到新登录凭据") {
		t.Fatalf("must not misreport runtime error as missing credentials: %q", resp.LoginNotice)
	}

	s.agentCLIs.mu.Lock()
	stillPending := s.agentCLIs.pools["antigravity"].PendingLogin
	s.agentCLIs.mu.Unlock()
	if stillPending != pendingID {
		t.Fatalf("PendingLogin was cleared on snapshot error: got %q, want %q", stillPending, pendingID)
	}

	// Once runAgentRuntime succeeds again, refreshing recovers the PendingLogin!
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		if subcmd == "snapshot" {
			m, _ := input.(map[string]any)
			if m["account_id"] == pendingID {
				data, _ := json.Marshal(agentGuestSnapshot{
					LoggedIn: true,
					AuthMode: "oauth",
					Email:    "dev@example.com",
					Plan:     "Google OAuth",
				})
				return json.Unmarshal(data, output)
			}
			data, _ := json.Marshal(agentGuestSnapshot{LoggedIn: false})
			return json.Unmarshal(data, output)
		}
		return nil
	}

	w = request(s, "GET", "/api/agent-clis/antigravity/accounts?refresh=1", "", "", true)
	resp = decodeAgentAccountsResp(t, w.Code, w.Body.Bytes())
	if len(resp.Accounts) != 1 || resp.Accounts[0].Email != "dev@example.com" {
		t.Fatalf("expected PendingLogin recovery to add dev@example.com, got accounts=%+v notice=%q", resp.Accounts, resp.LoginNotice)
	}
	s.agentCLIs.mu.Lock()
	afterPending := s.agentCLIs.pools["antigravity"].PendingLogin
	s.agentCLIs.mu.Unlock()
	if afterPending != "" {
		t.Fatalf("expected PendingLogin cleared after recovery, got %q", afterPending)
	}
}

// TestListAgentAccountsWaitsForInflightConfirmLogin verifies that when the UI
// triggers GET /api/agent-clis/<id>/accounts immediately upon terminal close
// (before confirmAgentLogin finishes its guest snapshot), the GET waits for the
// in-flight confirmation and returns the newly added account instead of stale state.
func TestListAgentAccountsWaitsForInflightConfirmLogin(t *testing.T) {
	s := runningAgentTestServer(t)
	pendingID := strings.Repeat("c", 24)

	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	pool := s.ensureAgentPoolLocked(pools, "antigravity")
	pool.PendingLogin = pendingID
	if err := s.saveAgentPoolsLocked(pools); err != nil {
		s.agentCLIs.mu.Unlock()
		t.Fatal(err)
	}
	s.agentCLIs.mu.Unlock()

	started := make(chan struct{})
	release := make(chan struct{})
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		if subcmd == "snapshot" {
			m, _ := input.(map[string]any)
			if m["account_id"] == pendingID {
				close(started)
				<-release
				data, _ := json.Marshal(agentGuestSnapshot{
					LoggedIn: true,
					AuthMode: "oauth",
					Email:    "race@example.com",
					Plan:     "Google OAuth",
				})
				return json.Unmarshal(data, output)
			}
			data, _ := json.Marshal(agentGuestSnapshot{LoggedIn: false})
			return json.Unmarshal(data, output)
		}
		return nil
	}

	confirmDone := make(chan error, 1)
	go func() {
		confirmDone <- s.confirmAgentLogin(context.Background(), "antigravity", pendingID)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmAgentLogin did not start snapshot")
	}

	getDone := make(chan agentAccountsHTTPResponse, 1)
	go func() {
		w := request(s, "GET", "/api/agent-clis/antigravity/accounts", "", "", true)
		getDone <- decodeAgentAccountsResp(t, w.Code, w.Body.Bytes())
	}()

	time.Sleep(30 * time.Millisecond)
	close(release)

	if err := <-confirmDone; err != nil {
		t.Fatalf("confirmAgentLogin failed: %v", err)
	}
	resp := <-getDone
	if len(resp.Accounts) != 1 || resp.Accounts[0].Email != "race@example.com" {
		t.Fatalf("GET returned stale state before confirmAgentLogin completed: accounts=%+v notice=%q", resp.Accounts, resp.LoginNotice)
	}
}

// TestConfirmAgentLoginDoesNotClearPendingWhileLoginTerminalActive ensures a
// premature confirmation check while an agent-login terminal is still open does
// not wipe PendingLogin or post "登录终端已关闭，未检测到新登录凭据".
func TestConfirmAgentLoginDoesNotClearPendingWhileLoginTerminalActive(t *testing.T) {
	s := runningAgentTestServer(t)
	pendingID := strings.Repeat("d", 24)

	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	pool := s.ensureAgentPoolLocked(pools, "antigravity")
	pool.PendingLogin = pendingID
	if err := s.saveAgentPoolsLocked(pools); err != nil {
		s.agentCLIs.mu.Unlock()
		t.Fatal(err)
	}
	s.agentCLIs.mu.Unlock()

	session := newTerminalSession(nil)
	session.id = "login-session-active"
	session.kind = "agent-login"
	session.agentID = "antigravity"
	session.accountID = pendingID
	s.terminals.mu.Lock()
	s.terminals.sessions = map[string]*terminalSession{session.id: session}
	s.terminals.mu.Unlock()

	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, _ any, output any) error {
		if subcmd == "snapshot" {
			data, _ := json.Marshal(agentGuestSnapshot{LoggedIn: false})
			return json.Unmarshal(data, output)
		}
		return nil
	}

	if err := s.confirmAgentLogin(context.Background(), "antigravity", pendingID); err != nil {
		t.Fatalf("confirmAgentLogin returned error: %v", err)
	}

	s.agentCLIs.mu.Lock()
	gotPending := s.agentCLIs.pools["antigravity"].PendingLogin
	gotNotice := s.agentCLIs.loginNotice["antigravity"]
	s.agentCLIs.mu.Unlock()
	if gotPending != pendingID {
		t.Fatalf("PendingLogin cleared while login terminal still open: got %q, want %q", gotPending, pendingID)
	}
	if strings.Contains(gotNotice, "登录终端已关闭") {
		t.Fatalf("notice falsely claimed login terminal closed: %q", gotNotice)
	}

	// Once the login terminal actually closes, empty credentials should clear PendingLogin and set notice.
	close(session.done)
	if err := s.confirmAgentLogin(context.Background(), "antigravity", pendingID); err != nil {
		t.Fatalf("confirmAgentLogin after close returned error: %v", err)
	}
	s.agentCLIs.mu.Lock()
	gotPending = s.agentCLIs.pools["antigravity"].PendingLogin
	gotNotice = s.agentCLIs.loginNotice["antigravity"]
	s.agentCLIs.mu.Unlock()
	if gotPending != "" {
		t.Fatalf("expected PendingLogin cleared after terminal closed with no creds, got %q", gotPending)
	}
	if gotNotice != "登录终端已关闭，未检测到新登录凭据" {
		t.Fatalf("unexpected notice after close: %q", gotNotice)
	}
}
