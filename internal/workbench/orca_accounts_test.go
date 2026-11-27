package workbench

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func TestOrcaFixedBindingsAdmissionLifecycle(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.closeAgents)
	ctx := context.Background()
	spec, ok := agentcatalog.Lookup("opencode", nil)
	if !ok {
		t.Fatal("missing opencode spec")
	}
	a, b, guestID, dynamicID := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24), strings.Repeat("d", 24)
	first := agentAccount{ID: a, ProfileID: guestID, Enabled: true, Status: "ready", Source: "managed", MaxConcurrency: 1}
	second := agentAccount{ID: b, Enabled: true, Status: "ready", Source: "managed", MaxConcurrency: 1}
	pool := &agentAccountPool{Accounts: []agentAccount{first, second}}
	s.agentCLIs.poolsLoaded = true
	s.agentCLIs.pools = map[string]*agentAccountPool{
		spec.ID:       pool,
		"antigravity": {Accounts: []agentAccount{{ID: dynamicID, Enabled: true, Status: "ready"}}},
	}
	cfg := defaultOrcaConfig(s.profile)
	cfg.Enabled, cfg.AutoSyncProjects, cfg.HostTunnelPort = true, false, defaultOrcaPort
	cfg.AccountBindings = map[string]string{spec.ID: a, "antigravity": "auto"}
	s.orca.config, s.orca.configLoaded = cfg, true

	var guestMu sync.Mutex
	guest := orcaGuestStatus{Port: cfg.GuestPort}
	var starts, stops, plain int
	var failStop, failProbe bool
	s.agentCLIs.runtimeHook = func(_ context.Context, op string, input, output any) error {
		guestMu.Lock()
		defer guestMu.Unlock()
		switch op {
		case "orca-status":
			if failProbe {
				return errors.New("guest temporarily unavailable")
			}
			*output.(*orcaGuestStatus) = guest
		case "plain-launches":
			*output.(*map[string]int) = map[string]int{guestID: plain}
		case "orca-start", "orca-restart":
			args := input.(map[string]any)
			bindings := args["bindings"].(map[string]any)
			target := bindings[spec.ID].(map[string]any)
			if target["account_id"] == a && target["guest_id"] != guestID {
				return errors.New("roster ID was used as the guest profile")
			}
			if target["account_id"] == a && s.op.TryLock() {
				s.op.Unlock()
				return errors.New("Orca started outside account admission lock")
			}
			if dynamic := bindings["antigravity"].(map[string]any); dynamic["mode"] != "auto" || dynamic["account_id"] != nil {
				return errors.New("dynamic provider incorrectly fixed to one account")
			}
			starts++
			guest = orcaGuestStatus{Port: cfg.GuestPort, Running: true, PID: 400 + starts,
				ConfigKey: args["config_key"].(string), Accounts: orcaFixedAccounts(bindings)}
			*output.(*orcaGuestStatus) = guest
		case "orca-stop":
			if failStop {
				return errors.New("could not confirm stop")
			}
			stops++
			guest = orcaGuestStatus{Port: cfg.GuestPort}
		}
		return nil
	}

	_, firstStart, err := s.ensureManagedOrcaRunning(ctx, false, "")
	if err != nil || firstStart.Accounts[spec.ID] != a {
		t.Fatalf("first start = %+v, %v", firstStart, err)
	}
	if sel, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || sel.ID != b {
		t.Fatalf("remaining terminal seat = %+v, %v", sel, err)
	}
	if _, _, err := s.selectAgentAccount(spec, a, true, ""); err == nil {
		t.Fatal("Orca reservation did not prevent relogin")
	}
	if err := s.removeAgentAccount(ctx, spec, a); err == nil {
		t.Fatal("Orca reservation did not prevent removing the profile")
	}
	if active := s.activeServiceAccounts("antigravity"); len(active) != 0 {
		t.Fatalf("Orca process claimed a seat for a dynamic provider: %v", active)
	}
	s.agentCLIs.mu.Lock()
	pool.Accounts[0].MaxConcurrency = 2
	s.agentCLIs.mu.Unlock()
	if _, _, err := s.selectAgentAccount(spec, a, false, ""); err != nil {
		t.Fatalf("fixed Orca seat was counted twice: %v", err)
	}
	guestMu.Lock()
	plain = 1
	guestMu.Unlock()
	if _, _, err := s.selectAgentAccount(spec, a, false, ""); err == nil {
		t.Fatal("distinct dynamic child lease was not added to the fixed Orca seat")
	}
	guestMu.Lock()
	plain = 0
	guestMu.Unlock()
	s.agentCLIs.mu.Lock()
	pool.Accounts[0].MaxConcurrency = 1
	s.agentCLIs.mu.Unlock()
	s.orca.mu.Lock()
	s.orca.statusCache = orcaGuestStatus{}
	s.orca.statusAt = time.Time{}
	s.orca.mu.Unlock()
	if active := s.activeServiceAccounts(spec.ID); active[a] != 1 {
		t.Fatalf("cold controller did not recover the reservation: %v", active)
	}
	guestMu.Lock()
	failProbe = true
	guestMu.Unlock()
	s.invalidateOrcaStatus()
	if active := s.activeServiceAccounts(spec.ID); active[a] != 1 {
		t.Fatalf("probe failure dropped the last verified seat: %v", active)
	}
	guestMu.Lock()
	failProbe = false
	guestMu.Unlock()

	s.agentCLIs.mu.Lock()
	pool.Accounts[0].Enabled = false
	s.agentCLIs.mu.Unlock()
	if _, st, err := s.ensureManagedOrcaRunning(ctx, false, ""); err != nil || st.PID != firstStart.PID {
		t.Fatalf("reattach re-admitted the live profile: %+v, %v", st, err)
	}
	s.agentCLIs.mu.Lock()
	pool.Accounts[0].Enabled = true
	s.agentCLIs.mu.Unlock()

	t3 := defaultT3CodeConfig()
	t3.Enabled = true
	s.t3code.config, s.t3code.configLoaded = t3, true
	s.t3code.statusCache = t3CodeGuestStatus{PID: 77, Port: t3.Port, Accounts: map[string]string{spec.ID: a}}
	s.t3code.statusAt = time.Now()
	if _, _, err := s.ensureManagedOrcaRunning(ctx, true, ""); err == nil {
		t.Fatal("Orca restart borrowed the T3 Code seat")
	}
	if active := s.activeServiceAccounts(spec.ID); active[a] != 2 {
		t.Fatalf("failed restart lost one service reservation: %v", active)
	}
	s.t3code.statusCache = t3CodeGuestStatus{Port: t3.Port}
	_, restarted, err := s.ensureManagedOrcaRunning(ctx, true, "")
	if err != nil || restarted.PID == firstStart.PID || s.activeServiceAccounts(spec.ID)[a] != 1 {
		t.Fatalf("same-account restart = %+v, %v", restarted, err)
	}

	s.orca.mu.Lock()
	s.orca.config.AccountBindings[spec.ID] = b
	s.orca.mu.Unlock()
	if active := s.activeServiceAccounts(spec.ID); active[a] != 1 || active[b] != 0 {
		t.Fatalf("preferences changed the running PID's account: %v", active)
	}
	_, replaced, err := s.ensureManagedOrcaRunning(ctx, false, "")
	if err != nil || replaced.PID == restarted.PID || replaced.Accounts[spec.ID] != b {
		t.Fatalf("replacement = %+v, %v", replaced, err)
	}
	guestMu.Lock()
	failStop = true
	guestMu.Unlock()
	if err := s.stopManagedOrca(ctx, cfg.GuestPort, "orca-stop"); err == nil || s.activeServiceAccounts(spec.ID)[b] != 1 {
		t.Fatalf("failed stop released a live reservation: %v", err)
	}
	guestMu.Lock()
	failStop = false
	guestMu.Unlock()
	if err := s.stopManagedOrca(ctx, cfg.GuestPort, "orca-stop"); err != nil || len(s.activeServiceAccounts(spec.ID)) != 0 {
		t.Fatalf("confirmed stop did not release the account: %v", err)
	}
	if starts != 3 || stops != 1 {
		t.Fatalf("unexpected guest transitions: %d starts, %d stops", starts, stops)
	}
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	if pool.Accounts[0].TotalAllocations != 2 || pool.Accounts[1].TotalAllocations != 1 {
		t.Fatalf("reattach or failed restart double-counted an allocation: %+v", pool.Accounts)
	}
}

func TestOrcaLateStatusDoesNotRestoreStoppedBinding(t *testing.T) {
	s := testServer(t)
	cfg := defaultOrcaConfig(s.profile)
	cfg.Enabled = true
	s.orca.config, s.orca.configLoaded = cfg, true
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	id := strings.Repeat("a", 24)
	s.agentCLIs.runtimeHook = func(_ context.Context, op string, _ any, output any) error {
		if op == "orca-status" {
			close(started)
			<-release
			*output.(*orcaGuestStatus) = orcaGuestStatus{Port: cfg.GuestPort, PID: 42, Running: true, Accounts: map[string]string{"opencode": id}}
		}
		return nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.probeOrcaStatus(context.Background(), cfg.GuestPort, true)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("status probe did not start")
	}
	if err := s.stopManagedOrca(context.Background(), cfg.GuestPort, "orca-stop"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if err := <-result; err == nil {
		t.Fatal("pre-stop probe returned stale account data")
	}
	s.orca.mu.Lock()
	defer s.orca.mu.Unlock()
	if s.orca.statusCache.PID != 0 || len(s.orca.statusCache.Accounts) != 0 {
		t.Fatalf("late pre-stop status restored a reservation: %+v", s.orca.statusCache)
	}
}

func TestOrcaReservationFeedsNativePoolsAndDefault(t *testing.T) {
	s := testServer(t)
	cfg := defaultOrcaConfig(s.profile)
	cfg.Enabled = true
	s.orca.config, s.orca.configLoaded = cfg, true
	claudeID, codexID, defaultID := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24)
	s.orca.statusCache = orcaGuestStatus{
		Port: cfg.GuestPort, PID: 42,
		Accounts: map[string]string{"claude": claudeID, "codex": codexID, "opencode": ""},
	}
	s.orca.statusAt = time.Now()
	if counts, _ := s.claudeAccountLoad(); counts[claudeID] != 1 {
		t.Fatalf("Claude ignored a live but unhealthy Orca PID: %v", counts)
	}
	if counts, _ := s.codexAccountLoad(); counts[codexID] != 1 {
		t.Fatalf("Codex ignored Orca: %v", counts)
	}
	if err := s.removeCodexAccount(codexID); err == nil || !strings.Contains(err.Error(), "托管服务") {
		t.Fatalf("Codex removal ignored Orca: %v", err)
	}
	spec, _ := agentcatalog.Lookup("opencode", nil)
	s.agentCLIs.poolsLoaded = true
	s.agentCLIs.pools = map[string]*agentAccountPool{
		spec.ID: {Accounts: []agentAccount{{ID: defaultID, Source: "default", Enabled: true, Status: "ready", MaxConcurrency: 1}}},
	}
	if _, _, err := s.selectAgentAccount(spec, "auto", false, ""); err == nil {
		t.Fatal("newly joined default did not inherit the Orca reservation")
	}
}
