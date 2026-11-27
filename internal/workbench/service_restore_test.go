package workbench

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func makeToolRestoreEnvironment(t *testing.T, s *Server) string {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	s.preferences.Workspace = workspace
	s.ready = true
	return workspace
}

func reopenToolRestoreController(t *testing.T, s *Server) *Server {
	t.Helper()
	next := testServer(t)
	next.profile, next.configPath = s.profile, s.configPath
	next.preferences.Workspace = s.workspace()
	next.agentCLIs.runtimeHook = s.agentCLIs.runtimeHook
	next.ready = true
	t.Cleanup(next.closeAgents)
	return next
}

func TestWorkbenchToolRestorePreservesT3AccountAndLiveService(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.closeAgents)
	workspace := makeToolRestoreEnvironment(t, s)
	first, next := strings.Repeat("a", 24), strings.Repeat("b", 24)
	cfg := defaultT3CodeConfig()
	cfg.Enabled = true
	cfg.AccountBindings["opencode"] = first
	s.t3code.mu.Lock()
	err := s.saveT3CodeConfigLocked(cfg)
	s.t3code.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	status := t3CodeGuestStatus{Port: cfg.Port}
	var launches []string
	s.agentCLIs.runtimeHook = func(_ context.Context, verb string, in, out any) error {
		mu.Lock()
		defer mu.Unlock()
		switch verb {
		case "t3code-status":
			*out.(*t3CodeGuestStatus) = status
		case "t3code-start", "t3code-restart":
			payload := in.(map[string]any)
			if payload["workspace"] != s.cliWorkspace(workspace) {
				return errors.New("wrong workspace")
			}
			assigned := payload["bindings"].(map[string]any)["opencode"].(map[string]any)["account_id"].(string)
			launches = append(launches, assigned)
			status = t3CodeGuestStatus{Port: cfg.Port, Running: true, PID: 1234, Accounts: map[string]string{"opencode": assigned}}
			*out.(*t3CodeGuestStatus) = status
		case "t3code-stop":
			status = t3CodeGuestStatus{Port: cfg.Port}
		}
		return nil
	}
	s.agentCLIs.mu.Lock()
	err = s.saveAgentPoolsLocked(map[string]*agentAccountPool{"opencode": {
		Accounts: []agentAccount{
			{ID: first, Source: "managed", Status: "ready", Enabled: true, MaxConcurrency: 1},
			{ID: next, Source: "managed", Status: "ready", Enabled: true},
		},
	}})
	s.agentCLIs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if w := request(s, "POST", "/api/t3code/start", s.origin, "{}", true); w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/t3code/config", s.origin, `{"account_bindings":{"opencode":"`+next+`"}}`, true); w.Code != http.StatusOK {
		t.Fatalf("change next binding: %d %s", w.Code, w.Body.String())
	}
	restarted := reopenToolRestoreController(t, s)
	if err := restarted.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatalf("reuse live T3: %v", err)
	}
	mu.Lock()
	before := len(launches)
	status = t3CodeGuestStatus{Port: cfg.Port}
	mu.Unlock()
	if before != 1 {
		t.Fatalf("restoring a live T3 started another process: %d starts", before)
	}
	if err := restarted.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatalf("restore cold T3: %v", err)
	}
	mu.Lock()
	actual := append([]string(nil), launches...)
	mu.Unlock()
	if strings.Join(actual, ",") != first+","+first {
		t.Fatalf("restoration picked a new identity: %v", actual)
	}
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tabs":
			_, _ = fmt.Fprintf(w, `{"tabs":[{"id":"saved-t3","url":"http://127.0.0.1:%d/threads/old"}]}`, cfg.Port)
		case "/activate":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer browser.Close()
	restarted.browserControl = localBrowserClient(t, restarted.lifetimeContext(), browser)
	defer restarted.browserControl.close()
	if w := request(restarted, "POST", "/api/t3code/open", restarted.origin, `{"force_mode":"workbench"}`, true); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"tab_id":"saved-t3"`) {
		t.Fatalf("existing workbench tab was not reused: %d %s", w.Code, w.Body.String())
	}
	if w := request(restarted, "POST", "/api/t3code/stop", restarted.origin, "{}", true); w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	afterStop := reopenToolRestoreController(t, s)
	if err := afterStop.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatalf("restore after stop: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(launches) != 2 {
		t.Fatalf("explicitly stopped T3 came back: %v", launches)
	}
}

func TestWorkbenchToolRestoreRejectsAChangedT3Profile(t *testing.T) {
	s := testServer(t)
	account := strings.Repeat("a", 24)
	s.agentCLIs.mu.Lock()
	err := s.saveAgentPoolsLocked(map[string]*agentAccountPool{"opencode": {
		Accounts: []agentAccount{{ID: account, ProfileID: strings.Repeat("b", 24), Source: "managed", Status: "ready", Enabled: true}},
	}})
	s.agentCLIs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	run := &t3CodeRunIntent{Bindings: map[string]t3CodeRestoreBinding{"opencode": {AccountID: account, GuestID: account}}}
	if _, _, err := s.resolveT3CodeRun(run, nil); err == nil || !strings.Contains(err.Error(), "身份已变化") {
		t.Fatalf("saved T3 binding accepted a replacement profile: %v", err)
	}
}

func TestWorkbenchToolRestoreDefersAndReusesMagpieAndHerdr(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.closeAgents)
	makeToolRestoreEnvironment(t, s)
	s.magpie.mu.Lock()
	magpie := defaultMagpieConfig()
	magpie.Enabled, magpie.GatewayOnly = true, true
	err := s.saveMagpieConfigLocked(magpie)
	s.magpie.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.herdr.mu.Lock()
	herdr := defaultHerdrConfig()
	herdr.Enabled = true
	err = s.saveHerdrConfigLocked(herdr)
	s.herdr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.t3code.mu.Lock()
	t3 := defaultT3CodeConfig()
	oldRun := snapshotT3CodeRun("", map[string]any{"claude": map[string]any{"mode": "default"}}, nil)
	t3.Enabled, t3.Mode, t3.ExternalURL, t3.Run = true, "external", "https://t3.example.test/", oldRun
	err = s.saveT3CodeConfigLocked(t3)
	s.t3code.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls []string
	var magpieRunning, herdrRunning bool
	failMagpieStop := false
	s.agentCLIs.runtimeHook = func(_ context.Context, verb string, in, out any) error {
		mu.Lock()
		defer mu.Unlock()
		if verb == "sync-auto-shims" {
			return nil
		}
		calls = append(calls, verb)
		switch verb {
		case "magpie-start":
			if in.(map[string]any)["gateway_only"] != true {
				return errors.New("restoration lost gateway mode")
			}
			magpieRunning = true
			fallthrough
		case "magpie-status":
			*out.(*magpieGuestStatus) = magpieGuestStatus{Running: magpieRunning, GatewayOnly: true, Port: magpie.Port}
		case "magpie-stop":
			if failMagpieStop {
				return errors.New("mock stop failed")
			}
			magpieRunning = false
		case "herdr-start":
			herdrRunning = true
			fallthrough
		case "herdr-status":
			*out.(*herdrGuestStatus) = herdrGuestStatus{Installed: true, ServerRunning: herdrRunning}
		case "herdr-stop":
			herdrRunning = false
			*out.(*herdrGuestStatus) = herdrGuestStatus{Installed: true}
		}
		return nil
	}
	resetCalls := func() {
		mu.Lock()
		calls = nil
		mu.Unlock()
	}
	assertCalls := func(expected string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if got := strings.Join(calls, ","); got != expected {
			t.Fatalf("service calls = %q, want %q", got, expected)
		}
	}
	if err := s.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCalls("")
	for _, service := range []string{"magpie", "herdr"} {
		if w := request(s, "POST", "/api/"+service+"/start", s.origin, "{}", true); w.Code != http.StatusOK {
			t.Fatalf("start %s: %d %s", service, w.Code, w.Body.String())
		}
	}
	restarted := reopenToolRestoreController(t, s)
	restarted.ready = false
	resetCalls()
	if err := restarted.restoreWorkbenchToolServices(context.Background()); !errors.Is(err, errTerminalNotReady) {
		t.Fatalf("restore before ready: %v", err)
	}
	assertCalls("")
	restarted.ready = true
	previous, _ := restarted.account.access()
	restarted.account.lock()
	if err := restarted.restoreWorkbenchToolServices(context.Background()); !errors.Is(err, errClaudeAccessLost) {
		t.Fatalf("restore while locked: %v", err)
	}
	assertCalls("")
	restarted.account.setKey(make([]byte, 32))
	stale := context.WithValue(context.Background(), accountEpochKey{}, previous)
	if err := restarted.restoreWorkbenchToolServices(stale); !errors.Is(err, errClaudeAccessLost) {
		t.Fatalf("stale epoch restored services: %v", err)
	}
	restarted.networkPaused = true // The managed web and terminal services are local.
	if err := restarted.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatalf("reuse local tools with network paused: %v", err)
	}
	assertCalls("magpie-status,herdr-status")
	mu.Lock()
	magpieRunning, herdrRunning = false, false
	mu.Unlock()
	resetCalls()
	if err := restarted.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatalf("cold restore local tools: %v", err)
	}
	assertCalls("magpie-status,magpie-start,herdr-status,herdr-start")
	mu.Lock()
	failMagpieStop = true
	mu.Unlock()
	if w := request(restarted, "POST", "/api/magpie/stop", restarted.origin, "{}", true); w.Code != http.StatusConflict {
		t.Fatalf("failed stop was accepted: %d %s", w.Code, w.Body.String())
	}
	check := reopenToolRestoreController(t, s)
	resetCalls()
	if err := check.restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatalf("restore after a failed stop: %v", err)
	}
	assertCalls("magpie-status,herdr-status")
	mu.Lock()
	failMagpieStop = false
	mu.Unlock()
	for _, service := range []string{"magpie", "herdr"} {
		if w := request(restarted, "POST", "/api/"+service+"/stop", restarted.origin, "{}", true); w.Code != http.StatusOK {
			t.Fatalf("stop %s: %d %s", service, w.Code, w.Body.String())
		}
	}
	resetCalls()
	if err := reopenToolRestoreController(t, s).restoreWorkbenchToolServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCalls("")
}
