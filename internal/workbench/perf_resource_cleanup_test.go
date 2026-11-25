package workbench

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func TestStopEnvironmentClosesBrowserControlAndClearsGuestCaches(t *testing.T) {
	s := testServer(t)
	s.ready = true
	s.credentials = guestCredentials{Token: "guest-token"}

	guestHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer guestHTTP.Close()
	browser := localBrowserClient(t, s.lifetimeContext(), guestHTTP)
	s.browserControl = browser

	// Populate guest-scoped caches across agentCLIs, cliDiscovery, claudeAccounts, codexAccounts.
	s.agentCLIs.mu.Lock()
	s.agentCLIs.defaultCache = map[string]*agentDefaultProbeEntry{
		"antigravity": {checkedAt: time.Now()},
	}
	s.agentCLIs.pendingAttempted = map[string]time.Time{"p1": time.Now()}
	s.agentCLIs.loginNotice = map[string]string{"antigravity": "notice"}
	s.agentCLIs.lastPick = map[string]string{"antigravity": "acct-1"}
	s.agentCLIs.recent = map[string]map[string]claudeProjectPick{
		"antigravity": {"/proj": {id: "acct-1", at: time.Now()}},
	}
	s.agentCLIs.mu.Unlock()

	s.cliDiscovery.mu.Lock()
	s.cliDiscovery.viewKeys = map[string]residentView{"s1": {key: "k1"}}
	s.cliDiscovery.titles = map[string]string{"s1": "Title 1"}
	s.cliDiscovery.results = map[string]error{"list": nil}
	s.cliDiscovery.lastInput = map[string]any{"k": "v"}
	s.cliDiscovery.lastHash = "abc"
	s.cliDiscovery.installed = true
	s.cliDiscovery.mu.Unlock()

	s.claudeAccounts.mu.Lock()
	s.claudeAccounts.defaultChecked = time.Now()
	s.claudeAccounts.attempted = map[string]time.Time{"stale": time.Now()}
	s.claudeAccounts.mu.Unlock()

	s.codexAccounts.mu.Lock()
	s.codexAccounts.defaultChecked = time.Now()
	s.codexAccounts.attempted = map[string]time.Time{"stale": time.Now()}
	s.codexAccounts.mu.Unlock()

	if err := s.stopEnvironment(); err != nil {
		t.Fatalf("stopEnvironment: %v", err)
	}

	if browser.ctx.Err() == nil {
		t.Fatal("stopEnvironment left s.browserControl open")
	}
	s.mu.Lock()
	stillSet := s.browserControl != nil
	s.mu.Unlock()
	if stillSet {
		t.Fatal("stopEnvironment did not nil out s.browserControl")
	}

	s.agentCLIs.mu.Lock()
	if len(s.agentCLIs.defaultCache) != 0 || len(s.agentCLIs.pendingAttempted) != 0 || len(s.agentCLIs.loginNotice) != 0 || len(s.agentCLIs.recent) != 0 || len(s.agentCLIs.lastPick) != 0 {
		t.Fatalf("agentCLIs caches survived stopEnvironment: default=%d pending=%d notice=%d recent=%d lastPick=%d",
			len(s.agentCLIs.defaultCache), len(s.agentCLIs.pendingAttempted), len(s.agentCLIs.loginNotice), len(s.agentCLIs.recent), len(s.agentCLIs.lastPick))
	}
	s.agentCLIs.mu.Unlock()

	s.cliDiscovery.mu.Lock()
	if len(s.cliDiscovery.viewKeys) != 0 || len(s.cliDiscovery.titles) != 0 || len(s.cliDiscovery.results) != 0 || s.cliDiscovery.lastInput != nil || s.cliDiscovery.lastHash != "" || s.cliDiscovery.installed {
		t.Fatalf("cliDiscovery caches survived stopEnvironment: views=%d titles=%d results=%d hash=%q installed=%v",
			len(s.cliDiscovery.viewKeys), len(s.cliDiscovery.titles), len(s.cliDiscovery.results), s.cliDiscovery.lastHash, s.cliDiscovery.installed)
	}
	s.cliDiscovery.mu.Unlock()

	s.claudeAccounts.mu.Lock()
	if !s.claudeAccounts.defaultChecked.IsZero() || len(s.claudeAccounts.attempted) != 0 {
		t.Fatalf("claudeAccounts caches survived stopEnvironment: defaultChecked=%v attempted=%d",
			!s.claudeAccounts.defaultChecked.IsZero(), len(s.claudeAccounts.attempted))
	}
	s.claudeAccounts.mu.Unlock()

	s.codexAccounts.mu.Lock()
	if !s.codexAccounts.defaultChecked.IsZero() || len(s.codexAccounts.attempted) != 0 {
		t.Fatalf("codexAccounts caches survived stopEnvironment: defaultChecked=%v attempted=%d",
			!s.codexAccounts.defaultChecked.IsZero(), len(s.codexAccounts.attempted))
	}
	s.codexAccounts.mu.Unlock()
}

func TestCachesAreBoundedUnderLongUptime(t *testing.T) {
	s := testServer(t)

	// cliDiscovery.viewKeys and titles must stay bounded across many sessions.
	for i := range 1000 {
		id := fmt.Sprintf("session-%04d", i)
		s.cliDiscovery.noteView(id, map[string]any{"id": id}, "k")
		s.cliDiscovery.named(id, "title-"+id)
	}
	s.cliDiscovery.mu.Lock()
	views, titles := len(s.cliDiscovery.viewKeys), len(s.cliDiscovery.titles)
	s.cliDiscovery.mu.Unlock()
	if views > 256 {
		t.Fatalf("cliDiscovery.viewKeys grew to %d, want <= 256", views)
	}
	if titles > 512 {
		t.Fatalf("cliDiscovery.titles grew to %d, want <= 512", titles)
	}

	// claudeAccounts.attempted and codexAccounts.attempted must stay bounded.
	s.claudeAccounts.mu.Lock()
	for i := range 500 {
		id := fmt.Sprintf("draft-%04d", i)
		s.claudeAccounts.claim(id)
		delete(s.claudeAccounts.refreshing, id)
	}
	claudeAttempted := len(s.claudeAccounts.attempted)
	s.claudeAccounts.mu.Unlock()
	if claudeAttempted > 128 {
		t.Fatalf("claudeAccounts.attempted grew to %d, want <= 128", claudeAttempted)
	}

	s.codexAccounts.mu.Lock()
	for i := range 500 {
		id := fmt.Sprintf("draft-%04d", i)
		s.codexAccounts.claim(id)
		delete(s.codexAccounts.refreshing, id)
	}
	codexAttempted := len(s.codexAccounts.attempted)
	s.codexAccounts.mu.Unlock()
	if codexAttempted > 128 {
		t.Fatalf("codexAccounts.attempted grew to %d, want <= 128", codexAttempted)
	}
}

func TestCloseServiceWaitsForBackgroundAgentProbeAndReturnsGoroutinesToBaseline(t *testing.T) {
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	before := runtime.NumGoroutine()

	s := testServer(t)
	started := make(chan struct{})
	exited := make(chan struct{})
	s.agentCLIs.probeHook = func(ctx context.Context, _ []agentcatalog.AgentSpec, _ bool) (agentProbeSnapshot, error) {
		close(started)
		defer close(exited)
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond)
		return agentProbeSnapshot{}, ctx.Err()
	}

	s.startMonitors()
	s.triggerBackgroundAgentProbe(agentcatalog.BuiltIn())

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background agent probe did not start")
	}

	// closeService must wait for the background agent probe goroutine before returning.
	_ = s.closeService()
	select {
	case <-exited:
	default:
		t.Fatal("closeService returned while triggerBackgroundAgentProbe goroutine was still running")
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+1 {
		t.Fatalf("goroutines leaked after start/close cycle: before=%d after=%d", before, after)
	}
}
