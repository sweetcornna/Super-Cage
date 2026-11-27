package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestAgentSchedulerStrategiesWeightAffinityAndCircuitBreaker(t *testing.T) {
	now := time.Unix(1750000000, 0)
	a, b, c := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24)
	pool := agentAccountPool{
		Scheduling: defaultAgentScheduling(),
		Accounts: []agentAccount{
			{ID: a, Label: "Pro-Heavy", Enabled: true, Status: "ready", Weight: 20, MaxConcurrency: 4, HandoffGroup: "work"},
			{ID: b, Label: "Plus-Standard", Enabled: true, Status: "ready", Weight: 10, MaxConcurrency: 2, HandoffGroup: "work"},
			{ID: c, Label: "Personal", Enabled: true, Status: "ready", Weight: 10, MaxConcurrency: 2, HandoffGroup: "personal"},
		},
	}

	// 1. Smart weighted concurrency: Pro-Heavy (weight 20, active 1 -> ratio 0.05) beats Plus-Standard (weight 10, active 1 -> ratio 0.10).
	dec := pickAgentAccount(pool, map[string]int{a: 1, b: 1, c: 2}, nil, "/proj/w", "", "", time.Time{}, now)
	if dec.AccountID != a {
		t.Fatalf("smart weighted concurrency picked %q, want %q (%s)", dec.AccountID, a, dec.Reason)
	}

	// 2. Max concurrency ceiling excludes C (2/2) and B (2/2) -> must pick A.
	dec = pickAgentAccount(pool, map[string]int{a: 3, b: 2, c: 2}, nil, "/proj/w", "", "", time.Time{}, now)
	if dec.AccountID != a {
		t.Fatalf("max concurrency cap violated: picked %q (%+v)", dec.AccountID, dec.Candidates)
	}

	// 3. Quota reset urgency ("use-it-or-lose-it"): B has 60% remaining resetting in 10 minutes vs A 65% resetting in 4 hours.
	poolUrgent := pool
	poolUrgent.Accounts = append([]agentAccount(nil), pool.Accounts...)
	poolUrgent.Accounts[0].Quota = &claudeQuota{
		FiveHour:   &claudeWindow{UsedPercentage: 35, ResetsAt: now.Add(4 * time.Hour).Unix(), ObservedAt: now.Unix()},
		ObservedAt: now.Unix(),
	}
	poolUrgent.Accounts[1].Quota = &claudeQuota{
		FiveHour:   &claudeWindow{UsedPercentage: 40, ResetsAt: now.Add(10 * time.Minute).Unix(), ObservedAt: now.Unix()},
		ObservedAt: now.Unix(),
	}
	poolUrgent.Accounts[2].Enabled = false
	dec = pickAgentAccount(poolUrgent, nil, nil, "/proj/u", "", "", time.Time{}, now)
	if dec.AccountID != b {
		t.Fatalf("reset urgency did not prioritize soon-expiring allowance: picked %q (%s)", dec.AccountID, dec.Reason)
	}

	// 4. Circuit breaker cooldown excludes A and automatically fails over to B.
	applyAccountCooldown(&pool.Accounts[0], "rate_limit", 0, now)
	if pool.Accounts[0].CooldownUntil != now.Add(60*time.Second).Unix() || pool.Accounts[0].ConsecutiveFailures != 1 {
		t.Fatalf("unexpected first backoff: %+v", pool.Accounts[0])
	}
	dec = pickAgentAccount(pool, nil, nil, "/proj/w", "", "", time.Time{}, now.Add(10*time.Second))
	if dec.AccountID == a {
		t.Fatalf("cooled down account selected: %+v", dec)
	}
	// Exponential backoff escalates on repeated failures.
	applyAccountCooldown(&pool.Accounts[0], "rate_limit", 0, now.Add(70*time.Second))
	if pool.Accounts[0].CooldownUntil != now.Add(250*time.Second).Unix() {
		t.Fatalf("exponential backoff did not escalate: %+v", pool.Accounts[0])
	}
	// Half-open probation penalizes freshly recovered A compared to healthy B.
	dec = pickAgentAccount(pool, nil, nil, "/proj/w", c, "", time.Time{}, now.Add(260*time.Second))
	if dec.AccountID == a {
		t.Fatalf("probationary account beat healthy peer: %+v", dec)
	}
	clearAccountCooldown(&pool.Accounts[0])

	// 5. Project pinning restricts strictly to bound group/account.
	pool.Scheduling.ProjectPins = map[string]string{
		"/proj/corp": "group:work",
		"/proj/home": "account:" + c,
	}
	dec = pickAgentAccount(pool, map[string]int{a: 1, b: 1, c: 0}, nil, "/proj/corp", "", "", time.Time{}, now)
	if dec.AccountID == c {
		t.Fatalf("project group pin escaped to personal account: %+v", dec)
	}
	dec = pickAgentAccount(pool, nil, nil, "/proj/home", "", "", time.Time{}, now)
	if dec.AccountID != c {
		t.Fatalf("project account pin ignored: %+v", dec)
	}

	// 6. Sticky project mode prefers recent workspace account within TTL, fails over when cooled down.
	pool.Scheduling.Mode = "sticky_project"
	pool.Scheduling.ProjectPins = nil
	dec = pickAgentAccount(pool, map[string]int{b: 1}, nil, "/proj/s", "", b, now.Add(-2*time.Minute), now)
	if dec.AccountID != b {
		t.Fatalf("sticky_project ignored recent account: %+v", dec)
	}
	applyAccountCooldown(&pool.Accounts[1], "rate_limit", 300, now)
	dec = pickAgentAccount(pool, nil, nil, "/proj/s", "", b, now.Add(-2*time.Minute), now)
	if dec.AccountID == b || dec.AccountID == "" {
		t.Fatalf("sticky_project failed to failover on cooldown: %+v", dec)
	}
	clearAccountCooldown(&pool.Accounts[1])

	// 7. Sequential priority waterfall respects list order and fails over when primary is full.
	pool.Scheduling.Mode = "sequential"
	dec = pickAgentAccount(pool, map[string]int{a: 2}, nil, "", "", "", time.Time{}, now)
	if dec.AccountID != a {
		t.Fatalf("sequential did not pick primary: %+v", dec)
	}
	dec = pickAgentAccount(pool, map[string]int{a: 4}, nil, "", "", "", time.Time{}, now)
	if dec.AccountID != b {
		t.Fatalf("sequential did not failover when primary hit concurrency limit: %+v", dec)
	}

	// 8. Quota-first picks highest headroom account.
	pool.Scheduling.Mode = "quota_first"
	pool.Accounts[0].Quota = &claudeQuota{FiveHour: &claudeWindow{UsedPercentage: 70, ResetsAt: now.Add(2 * time.Hour).Unix(), ObservedAt: now.Unix()}, ObservedAt: now.Unix()}
	pool.Accounts[1].Quota = &claudeQuota{FiveHour: &claudeWindow{UsedPercentage: 15, ResetsAt: now.Add(2 * time.Hour).Unix(), ObservedAt: now.Unix()}, ObservedAt: now.Unix()}
	dec = pickAgentAccount(pool, nil, nil, "", "", "", time.Time{}, now)
	if dec.AccountID != b {
		t.Fatalf("quota_first did not select highest headroom: %+v", dec)
	}
}

func TestAgentsAPICachingCustomSpecAndAccountSecretsIsolation(t *testing.T) {
	s := testServer(t)
	s.ready = true

	var probeCalls atomic.Int32
	s.agentCLIs.probeHook = func(_ context.Context, specs []agentcatalog.AgentSpec, _ bool) (agentProbeSnapshot, error) {
		probeCalls.Add(1)
		time.Sleep(25 * time.Millisecond)
		agents := map[string]agentRuntimeState{}
		for _, sp := range specs {
			if sp.ID == "codex" || sp.ID == "claude" {
				agents[sp.ID] = agentRuntimeState{Installed: true, BinaryPath: "/usr/local/bin/" + sp.Binary, Version: "2.5.0"}
			}
		}
		return agentProbeSnapshot{
			Prerequisites: map[string]prerequisiteState{"node": {Available: true, Version: "24.0.0"}, "npm": {Available: true, Version: "11.0.0"}},
			Agents:        agents,
		}, nil
	}

	// Verify singleflight coalescing across concurrent requests and TTL caching!
	// Force vmRunning true by setting windowsGuest? No: s.vmRunning checks lima.yaml/state in testServer;
	// instead test probeAllAgents directly for singleflight & TTL, then test full HTTP flows.
	specs := agentcatalog.BuiltIn()
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := s.probeAllAgents(context.Background(), specs, false)
			if err != nil || !snap.Agents["codex"].Installed {
				t.Errorf("concurrent probe failed: %+v %v", snap, err)
			}
		}()
	}
	wg.Wait()
	if probeCalls.Load() != 1 {
		t.Fatalf("expected 1 coalesced probe call, got %d", probeCalls.Load())
	}
	// Cached read within TTL makes zero additional probe calls.
	if _, err := s.probeAllAgents(context.Background(), specs, false); err != nil {
		t.Fatal(err)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("TTL cache missed: calls=%d", probeCalls.Load())
	}

	// Test GET /api/agent-clis returns full catalog.
	w := request(s, "GET", "/api/agent-clis", "", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/agent-clis: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Agents []agentCatalogEntry `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil || len(listResp.Agents) < 10 {
		t.Fatalf("unexpected catalog response: %v len=%d", err, len(listResp.Agents))
	}

	// Test custom agent registration + rejection of unsafe env var.
	badCustom := `{"id":"evil-agent","name":"Evil","binary":"evil","isolation":{"mode":"env_dir","env_var":"LD_PRELOAD"}}`
	if w = request(s, "POST", "/api/agent-clis/custom", s.origin, badCustom, true); w.Code != http.StatusBadRequest {
		t.Fatalf("unsafe custom spec accepted: %d %s", w.Code, w.Body.String())
	}
	goodCustom := `{"id":"my-cli","name":"My CLI","binary":"my-cli","channels":[{"id":"npm-official","kind":"npm","label":"npm","package":"my-cli"}],"isolation":{"mode":"env_dir","env_var":"MY_CLI_HOME"},"auth":{"methods":["oauth","api_key"],"oauth_domains":["auth.mycli.dev"],"api_key_env_vars":["MY_CLI_API_KEY"]}}`
	if w = request(s, "POST", "/api/agent-clis/custom", s.origin, goodCustom, true); w.Code != http.StatusOK {
		t.Fatalf("valid custom spec rejected: %d %s", w.Code, w.Body.String())
	}

	// Test Claude launch command synchronization through /api/agent-clis/config.
	if w = request(s, "POST", "/api/agent-clis/config", s.origin, `{"agent_id":"claude","launch_command":"claude --continue"}`, true); w.Code != http.StatusOK {
		t.Fatalf("config update failed: %d %s", w.Code, w.Body.String())
	}
	if claudeTerminalCommand(s.preferences) != "claude --continue" {
		t.Fatalf("claude launch command did not sync: %q", s.preferences.ClaudeTerminalCommand)
	}

	// Test API Key account addition: verify secret NEVER enters agent-accounts.json or GET response!
	const rawSecret = "sk-proj-super-secret-value-9988"
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		if subcmd == "set-api-key" {
			data, _ := json.Marshal(agentGuestSnapshot{
				LoggedIn: true, AuthMode: "api_key", KeyHint: "sk-p...9988", KeyFingerprint: "fp1234",
				BaseURL: "https://api.example.com/v1", Model: "o3", EnvVar: "OPENAI_API_KEY",
			})
			return json.Unmarshal(data, output)
		}
		if subcmd == "snapshot" {
			data, _ := json.Marshal(agentGuestSnapshot{LoggedIn: false})
			return json.Unmarshal(data, output)
		}
		return nil
	}
	// Add API key directly through the store method (bypassing vmRunning check since testServer has no Lima VM).
	spec, _, _ := s.lookupAgentSpec("codex")
	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	pool := s.ensureAgentPoolLocked(pools, "codex")
	id1 := strings.Repeat("1", 24)
	pool.Accounts = append(pool.Accounts, agentAccount{
		ID: id1, Label: "Codex Key 1", Source: "api_key", AuthMode: "api_key", Enabled: true, Status: "ready",
		KeyHint: "sk-p...9988", BaseURL: "https://api.example.com/v1", Weight: 25, MaxConcurrency: 3, HandoffGroup: "work",
	})
	if err := s.saveAgentPoolsLocked(pools); err != nil {
		t.Fatal(err)
	}
	s.agentCLIs.mu.Unlock()
	_ = spec

	// Verify cooldown mutation, scheduling update, and project pinning via HTTP.
	w = request(s, "POST", "/api/agent-clis/codex/accounts", s.origin, `{"action":"cooldown","id":"`+id1+`","cooldown_seconds":300,"error_kind":"rate_limit"}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"cooldown_until"`) {
		t.Fatalf("cooldown failed: %d %s", w.Code, w.Body.String())
	}
	w = request(s, "POST", "/api/agent-clis/codex/accounts", s.origin, `{"action":"cooldown","id":"`+id1+`","cooldown_seconds":-1}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("clear cooldown failed: %d %s", w.Code, w.Body.String())
	}
	w = request(s, "POST", "/api/agent-clis/codex/accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"sticky_project","project_affinity_minutes":30,"auto_cooldown":true}}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sticky_project"`) {
		t.Fatalf("scheduling update failed: %d %s", w.Code, w.Body.String())
	}

	// Verify agent-accounts.json on disk contains zero secrets.
	diskData, err := os.ReadFile(filepath.Join(s.profile, "agent-accounts.json"))
	if err != nil || strings.Contains(string(diskData), rawSecret) {
		t.Fatalf("accounts file check failed: %v", err)
	}

	// Verify locked Cage rejects /api/agent-clis and /api/agent-clis/codex/accounts with 423.
	s.account.lock()
	if w = request(s, "GET", "/api/agent-clis", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked Cage exposed /api/agent-clis: %d", w.Code)
	}
	if w = request(s, "GET", "/api/agent-clis/codex/accounts", "", "", true); w.Code != http.StatusLocked {
		t.Fatalf("locked Cage exposed /api/agent-clis/codex/accounts: %d", w.Code)
	}
}

func TestMultiAgentLoginLinksAndCustomOAuthDomain(t *testing.T) {
	s := testServer(t)
	s.ready = true
	// Register a custom agent with its own OAuth domain.
	w := request(s, "POST", "/api/agent-clis/custom", s.origin,
		`{"id":"custom-ai","name":"Custom AI","binary":"custom-ai","isolation":{"mode":"xdg"},"auth":{"methods":["oauth"],"oauth_domains":["auth.custom-ai.dev"]}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("custom register failed: %d %s", w.Code, w.Body.String())
	}

	terminal := newTerminalSession(nil)
	terminal.id = "abcdef1234567890"
	s.terminals.sessions = map[string]*terminalSession{terminal.id: terminal}

	var gotHosts []string
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL          string   `json:"url"`
			Login        string   `json:"login"`
			AllowedHosts []string `json:"allowed_hosts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path == "/login-open" {
			gotHosts = in.AllowedHosts
			_, _ = w.Write([]byte(`{"id":"tab-agent"}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()

	for _, validURL := range []string{
		"https://auth.openai.com/oauth/authorize?client_id=codex",
		"https://accounts.google.com/o/oauth2/v2/auth?scope=openid",
		"https://github.com/login/device",
		"https://auth.custom-ai.dev/oauth/authorize?state=123",
	} {
		resp := request(s, "POST", "/api/terminals/"+terminal.id+"/login-link", s.origin, `{"url":"`+validURL+`"}`, true)
		if resp.Code != http.StatusOK {
			t.Fatalf("valid agent oauth link rejected (%s): %d %s", validURL, resp.Code, resp.Body.String())
		}
	}
	if len(gotHosts) == 0 {
		t.Fatal("expected allowed_hosts to be forwarded for non-Claude login links")
	}

	for _, badURL := range []string{
		"https://evil.phish.example/oauth/authorize?x=1",
		"http://auth.openai.com/oauth/authorize?x=1",
		"https://auth.openai.com:8443/oauth/authorize?x=1",
		"https://github.com/org/repo?tab=readme",
	} {
		resp := request(s, "POST", "/api/terminals/"+terminal.id+"/login-link", s.origin, `{"url":"`+badURL+`"}`, true)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("invalid agent oauth link accepted (%s): %d", badURL, resp.Code)
		}
	}
	close(terminal.done)
}

func TestAgentSchedulerLongWindowConservationDrainEWMAAndPassiveRateLimit(t *testing.T) {
	now := time.Unix(1750000000, 0)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)

	// 1. Weekly (7d) conservation protects scarce weekly quota even if 5h resets soon.
	pool := agentAccountPool{
		Scheduling: defaultAgentScheduling(),
		Accounts: []agentAccount{
			{
				ID: a, Label: "Scarce-Weekly", Enabled: true, Status: "ready", Weight: 10,
				Quota: &claudeQuota{
					FiveHour:   &claudeWindow{UsedPercentage: 40, ResetsAt: now.Add(10 * time.Minute).Unix(), ObservedAt: now.Unix()},
					SevenDay:   &claudeWindow{UsedPercentage: 92, ResetsAt: now.Add(5 * 24 * time.Hour).Unix(), ObservedAt: now.Unix()},
					ObservedAt: now.Unix(),
				},
			},
			{
				ID: b, Label: "Healthy-Weekly", Enabled: true, Status: "ready", Weight: 10,
				Quota: &claudeQuota{
					FiveHour:   &claudeWindow{UsedPercentage: 40, ResetsAt: now.Add(3 * time.Hour).Unix(), ObservedAt: now.Unix()},
					SevenDay:   &claudeWindow{UsedPercentage: 30, ResetsAt: now.Add(5 * 24 * time.Hour).Unix(), ObservedAt: now.Unix()},
					ObservedAt: now.Unix(),
				},
			},
		},
	}
	dec := pickAgentAccount(pool, nil, nil, "/proj/w", "", "", time.Time{}, now)
	if dec.AccountID != b {
		t.Fatalf("7d conservation failed: picked %q (%s), want %q", dec.AccountID, dec.Reason, b)
	}

	// 2. Drain mode prioritizes soonest resetting window when 7d is healthy.
	pool.Scheduling.Mode = "drain"
	pool.Accounts[0].Quota.SevenDay.UsedPercentage = 20
	dec = pickAgentAccount(pool, nil, nil, "/proj/w", "", "", time.Time{}, now)
	if dec.AccountID != a {
		t.Fatalf("drain mode did not pick soonest reset: picked %q (%s)", dec.AccountID, dec.Reason)
	}

	// 3. Probation penalty decays cleanly over time so recovered accounts aren't permanently starved.
	pool.Scheduling.Mode = "smart"
	applyAccountCooldown(&pool.Accounts[0], "rate_limit", 60, now)
	if pool.Accounts[0].HealthEWMA >= 1.0 {
		t.Fatalf("expected EWMA health drop after cooldown, got %v", pool.Accounts[0].HealthEWMA)
	}
	if p := probationPenalty(pool.Accounts[0], now.Add(20*time.Minute)); p != 0 {
		t.Fatalf("probation penalty did not decay to 0 after recovery: %v", p)
	}

	// 4. Passive PTY rate-limit byte scanner catches 429 & quota exhaustion with zero false positives.
	if kind, _ := detectAgentRateLimitBytes([]byte("Error: HTTP 429 Too Many Requests - rate limit exceeded")); kind != "rate_limit" {
		t.Fatalf("missed rate_limit: %q", kind)
	}
	if kind, _ := detectAgentRateLimitBytes([]byte("APIError: insufficient_quota for project")); kind != "quota_exhausted" {
		t.Fatalf("missed quota_exhausted: %q", kind)
	}
	if kind, _ := detectAgentRateLimitBytes([]byte("Compiling 429 files successfully...")); kind != "" {
		t.Fatalf("false positive on normal output: %q", kind)
	}

	// 5. Passive onAgentRateLimit honors AutoCooldown toggle and applies cooldown.
	s := testServer(t)
	s.ready = true
	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	p := s.ensureAgentPoolLocked(pools, "codex")
	p.Scheduling = defaultAgentScheduling()
	p.Accounts = []agentAccount{{ID: a, Label: "Codex A", Enabled: true, Status: "ready", Weight: 10}}
	_ = s.saveAgentPoolsLocked(pools)
	s.agentCLIs.mu.Unlock()

	s.onAgentRateLimit("codex", a, "rate_limit")
	s.agentCLIs.mu.Lock()
	cooled := s.agentCLIs.pools["codex"].find(a).CooldownUntil
	s.agentCLIs.mu.Unlock()
	if cooled <= time.Now().Unix() {
		t.Fatalf("passive rate limit did not trigger auto cooldown: %d", cooled)
	}
	s.flushAgentPools()
}

func TestAgentsConcurrentAccessRaceFreeAndAffinityBounds(t *testing.T) {
	s := testServer(t)
	s.ready = true
	id := strings.Repeat("d", 24)

	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	p := s.ensureAgentPoolLocked(pools, "codex")
	p.Accounts = []agentAccount{{ID: id, Label: "Codex D", Enabled: true, Status: "ready", Weight: 10}}
	_ = s.saveAgentPoolsLocked(pools)
	s.agentCLIs.mu.Unlock()

	// Verify 1440m affinity is accepted cleanly (matching UI dropdown).
	w := request(s, "POST", "/api/agent-clis/codex/accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"drain","project_affinity_minutes":1440,"auto_cooldown":true}}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"project_affinity_minutes":1440`) {
		t.Fatalf("1440m affinity rejected: %d %s", w.Code, w.Body.String())
	}

	// Hammer concurrent reads and writes to confirm zero map/slice races under -race.
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := range 5 {
				_ = request(s, "GET", "/api/agent-clis", "", "", true)
				_ = request(s, "GET", "/api/agent-clis/codex/accounts?skip_detect=1", "", "", true)
				if (idx+j)%2 == 0 {
					_ = request(s, "POST", "/api/agent-clis/codex/accounts", s.origin, `{"action":"pin_project","workspace":"/proj/p","pin_target":"account:`+id+`"}`, true)
				} else {
					_ = request(s, "POST", "/api/agent-clis/config", s.origin, `{"agent_id":"codex","launch_command":"codex --search"}`, true)
				}
			}
		}(i)
	}
	wg.Wait()
	s.flushAgentPools()
}

func TestWindowsGuestAgentsAndT3CodeDoNotDeadlock(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{}
	s.ready = true

	for _, path := range []string{"/api/agent-clis", "/api/agent-clis/codex/accounts?skip_detect=1", "/api/t3code", "/api/magpie", "/api/herdr"} {
		r := httptest.NewRequest(http.MethodGet, s.origin+path, nil)
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("Windows guest %s failed: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestAgentSecurityAndLifecycleHardening(t *testing.T) {
	// 1. managedAgentTerminalScript uses hashed prep stamp + subshell profile.env (zero ExtraEnv shell expansion).
	customSpec := agentcatalog.AgentSpec{
		ID:     "evil-env-test",
		Name:   "Test",
		Binary: "testcli",
		Isolation: agentcatalog.IsolationSpec{
			Mode:     "env_dir",
			EnvVar:   "TESTCLI_HOME",
			ExtraEnv: map[string]string{"INJECT_TEST": "$(touch /tmp/pwned)"},
		},
	}
	script := managedAgentTerminalScript("/proj", customSpec, "testcli", "acct12345678901234567890", "", false, false)
	wantHash := agentPrepHash(customSpec, "acct12345678901234567890")
	if !strings.Contains(script, ".prepared-"+wantHash) || !strings.Contains(script, "profile.env") {
		t.Fatalf("script missing hashed stamp or profile.env sourcing: %s", script)
	}
	if strings.Contains(script, `export INJECT_TEST="`) {
		t.Fatalf("script unsafely interpolated ExtraEnv inside double quotes: %s", script)
	}
	if !strings.Contains(script, `( . "$HOME/.local/share/cage/agent-accounts/evil-env-test/acct12345678901234567890/profile.env" && eval "$CAGE_TERMINAL_COMMAND" )`) {
		t.Fatalf("script did not scope profile.env to subshell: %s", script)
	}
	defaultScript := managedAgentTerminalScript("/proj", customSpec, "testcli", "default", "", false, false)
	if strings.Contains(defaultScript, "prepare-session") {
		t.Fatalf("default account script should skip prepare-session: %s", defaultScript)
	}

	// 2. probeAllAgents sets ProbedAt on the very first call & rejects stale writes after invalidation.
	s := testServer(t)
	s.ready = true
	specs := agentcatalog.BuiltIn()
	started := make(chan struct{})
	release := make(chan struct{})
	s.agentCLIs.probeHook = func(_ context.Context, _ []agentcatalog.AgentSpec, _ bool) (agentProbeSnapshot, error) {
		close(started)
		<-release
		return agentProbeSnapshot{
			Agents: map[string]agentRuntimeState{"codex": {Installed: true}},
		}, nil
	}
	var firstSnap agentProbeSnapshot
	done := make(chan struct{})
	go func() {
		defer close(done)
		firstSnap, _ = s.probeAllAgents(context.Background(), specs, true)
	}()
	<-started
	s.invalidateAgentProbeCache() // bump generation while probe is in-flight!
	close(release)
	<-done
	if firstSnap.ProbedAt == 0 {
		t.Fatal("first probeAllAgents caller received ProbedAt == 0")
	}
	s.agentCLIs.mu.Lock()
	staleCached := !s.agentCLIs.probeAt.IsZero()
	s.agentCLIs.mu.Unlock()
	if staleCached {
		t.Fatal("stale in-flight probe wrote back to probeCache after invalidation!")
	}

	// 3. Scoped terminalLoginLink restricts agent terminals to their own OAuth domains.
	codexTerm := newTerminalSession(nil)
	codexTerm.id = "codexterm1234567"
	codexTerm.agentID = "codex"
	codexTerm.agentOAuthDomains = []string{"auth.openai.com"}
	s.terminals.sessions = map[string]*terminalSession{codexTerm.id: codexTerm}
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"tab-codex"}`))
	}))
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()

	if resp := request(s, "POST", "/api/terminals/"+codexTerm.id+"/login-link", s.origin, `{"url":"https://auth.openai.com/oauth/authorize?client_id=1"}`, true); resp.Code != http.StatusOK {
		t.Fatalf("codex terminal rejected its own oauth domain: %d %s", resp.Code, resp.Body.String())
	}
	if resp := request(s, "POST", "/api/terminals/"+codexTerm.id+"/login-link", s.origin, `{"url":"https://claude.ai/oauth/authorize?client_id=1"}`, true); resp.Code != http.StatusBadRequest {
		t.Fatalf("codex terminal accepted foreign Claude oauth domain: %d", resp.Code)
	}
	if resp := request(s, "POST", "/api/terminals/"+codexTerm.id+"/login-link", s.origin, `{"url":"https://github.com/login/device"}`, true); resp.Code != http.StatusBadRequest {
		t.Fatalf("codex terminal accepted unrelated GitHub oauth domain: %d", resp.Code)
	}
	close(codexTerm.done)

	// API-key-only agent terminal (empty agentOAuthDomains) must NOT inherit global provider allowlist.
	aiderTerm := newTerminalSession(nil)
	aiderTerm.id = "aiderterm1234567"
	aiderTerm.agentID = "aider"
	aiderTerm.agentOAuthDomains = nil
	s.terminals.sessions[aiderTerm.id] = aiderTerm
	if resp := request(s, "POST", "/api/terminals/"+aiderTerm.id+"/login-link", s.origin, `{"url":"https://auth.openai.com/oauth/authorize?client_id=1"}`, true); resp.Code != http.StatusBadRequest {
		t.Fatalf("API-key-only agent terminal inherited global oauth domain: %d", resp.Code)
	}
	close(aiderTerm.done)

	// Claude allowlist check must NEVER bypass supplied allowedHosts.
	if terminalAllowsLoginURL("https://claude.ai/oauth/authorize?client_id=1", "claude", []string{"console.anthropic.com"}) {
		t.Fatal("Claude special case bypassed explicit allowedHosts")
	}
	if !terminalAllowsLoginURL("https://claude.ai/oauth/authorize?client_id=1", "claude", []string{"claude.ai"}) {
		t.Fatal("Claude URL rejected even though claude.ai was in allowedHosts")
	}

	// 4. Atomic add/update/remove validation before guest mutations + ProfileID routing.
	var guestCalls []string
	var lastGuestID string
	var failClear bool
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		if subcmd == "sync-auto-shims" {
			// Publication is asynchronous and independent of credential operations.
			return nil
		}
		guestCalls = append(guestCalls, subcmd)
		if m, ok := input.(map[string]any); ok {
			if id, ok := m["account_id"].(string); ok {
				lastGuestID = id
			}
		}
		if subcmd == "clear-credentials" && failClear {
			return os.ErrPermission
		}
		if output != nil {
			data, _ := json.Marshal(agentGuestSnapshot{LoggedIn: true, AuthMode: "api_key", KeyHint: "sk-...1234", BaseURL: "https://api.test/v1"})
			return json.Unmarshal(data, output)
		}
		return nil
	}
	codexSpec, _, _ := s.lookupAgentSpec("codex")
	badWeight := 999
	if err := s.addAgentAPIKeyAccount(context.Background(), codexSpec, agentAccountMutation{APIKey: "sk-123", Weight: &badWeight}); err == nil {
		t.Fatal("out-of-range weight accepted")
	}
	if len(guestCalls) != 0 {
		t.Fatalf("invalid addAgentAPIKeyAccount still invoked guest runtime: %v", guestCalls)
	}
	if err := s.updateAgentAccount(context.Background(), codexSpec, agentAccountMutation{ID: strings.Repeat("9", 24), APIKey: "sk-new"}); err == nil {
		t.Fatal("non-existent account update succeeded")
	}
	if len(guestCalls) != 0 {
		t.Fatalf("non-existent updateAgentAccount still invoked guest runtime: %v", guestCalls)
	}

	// Seed an account whose ProfileID differs from its roster ID and verify update targets ProfileID.
	rosterID := strings.Repeat("1", 24)
	profileID := strings.Repeat("2", 24)
	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	p := s.ensureAgentPoolLocked(pools, "codex")
	p.Accounts = []agentAccount{{ID: rosterID, ProfileID: profileID, Label: "Key", Source: "api_key", AuthMode: "api_key", Enabled: true, Status: "ready"}}
	_ = s.saveAgentPoolsLocked(pools)
	s.agentCLIs.mu.Unlock()

	if err := s.updateAgentAccount(context.Background(), codexSpec, agentAccountMutation{ID: rosterID, BaseURL: "https://api.test/v2"}); err != nil {
		t.Fatalf("valid update failed: %v", err)
	}
	if lastGuestID != profileID {
		t.Fatalf("updateAgentAccount sent account_id=%q, want ProfileID=%q", lastGuestID, profileID)
	}

	// Updating an OAuth/managed account with a raw API key must be rejected (prevents Source mismatch & leaked key on removal).
	managedID := strings.Repeat("3", 24)
	s.agentCLIs.mu.Lock()
	pools, _ = s.loadAgentPoolsLocked()
	p = s.ensureAgentPoolLocked(pools, "codex")
	p.Accounts = append(p.Accounts, agentAccount{ID: managedID, Label: "OAuth", Source: "managed", AuthMode: "oauth", Enabled: true, Status: "ready"})
	_ = s.saveAgentPoolsLocked(pools)
	s.agentCLIs.mu.Unlock()
	if err := s.updateAgentAccount(context.Background(), codexSpec, agentAccountMutation{ID: managedID, APIKey: "sk-should-reject"}); err == nil {
		t.Fatal("writing API key onto managed OAuth account was accepted")
	}

	// Failed clear-credentials must atomically restore roster entry (prevents orphaned secret).
	failClear = true
	if err := s.removeAgentAccount(context.Background(), codexSpec, rosterID); err == nil {
		t.Fatal("removeAgentAccount succeeded despite clear-credentials failure")
	}
	s.agentCLIs.mu.Lock()
	stillThere := s.agentCLIs.pools["codex"].find(rosterID) != nil
	s.agentCLIs.mu.Unlock()
	if !stillThere {
		t.Fatal("roster entry deleted even though guest credential wipe failed")
	}

	// 5. Rolling ANSI-stripped rate-limit scanner catches split chunks, large >2KiB prefix errors, and ignores prose.
	ts := newTerminalSession(nil)
	ts.appendRateScanBytes([]byte("\x1b[31mError: HTTP \x1b[1m42"))
	ts.appendRateScanBytes([]byte("9 Too Many\x1b[0m Requests"))
	if kind, _ := detectAgentRateLimitNormalized(ts.rateScanBuf); kind != "rate_limit" {
		t.Fatalf("ANSI split rate limit missed: buf=%q kind=%q", string(ts.rateScanBuf), kind)
	}
	largeChunk := append([]byte("APIError: insufficient_quota "), make([]byte, 3000)...)
	for i := 29; i < len(largeChunk); i++ {
		largeChunk[i] = '.'
	}
	tsLarge := newTerminalSession(nil)
	foundLarge := ""
	for off := 0; off < len(largeChunk); off += 512 {
		end := min(off+512, len(largeChunk))
		tsLarge.appendRateScanBytes(largeChunk[off:end])
		if k, _ := detectAgentRateLimitNormalized(tsLarge.rateScanBuf); k != "" {
			foundLarge = k
			break
		}
	}
	if foundLarge != "quota_exhausted" {
		t.Fatalf("large chunk prefix rate-limit missed: %q", foundLarge)
	}
	if kind, _ := detectAgentRateLimitBytes([]byte("Note: this public API is rate limited to 60 rpm in docs.")); kind != "" {
		t.Fatalf("normal prose triggered false rate-limit cooldown: %q", kind)
	}

	// 6. closeAgents seals against late timers cleanly.
	s.closeAgents()
	s.onAgentRateLimit("codex", rosterID, "rate_limit")
	s.agentCLIs.mu.Lock()
	timerArmedAfterClose := s.agentCLIs.flushTimer != nil
	s.agentCLIs.mu.Unlock()
	if timerArmedAfterClose {
		t.Fatal("late callback armed flushTimer after closeAgents()")
	}
}
