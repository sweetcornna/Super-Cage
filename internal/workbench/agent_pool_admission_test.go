package workbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func TestAgentPoolLaunchMatchesPreview(t *testing.T) {
	now := time.Unix(1760000000, 0)
	a, b, c, d := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24), strings.Repeat("d", 24)
	pool := agentAccountPool{
		Scheduling: defaultAgentScheduling(),
		Accounts: []agentAccount{
			{ID: a, Enabled: true, Status: "ready", MaxConcurrency: 1, Weight: 20, LastAllocatedAt: now.Unix(), HandoffGroup: "team"},
			{ID: b, Enabled: true, Status: "ready", Weight: 5, HandoffGroup: "team", Quota: &claudeQuota{ObservedAt: now.Unix(),
				FiveHour: &claudeWindow{UsedPercentage: 34, ResetsAt: now.Add(time.Minute).Unix()}}},
			{ID: c, Enabled: true, Status: "ready", HandoffGroup: "others", ConsecutiveFailures: 1, CooldownUntil: now.Add(-time.Minute).Unix()},
			{ID: d, Enabled: true, Status: "ready", HandoffGroup: "team", Quota: &claudeQuota{ObservedAt: now.Unix(),
				SevenDay: &claudeWindow{UsedPercentage: 100, ResetsAt: now.Add(time.Hour).Unix()}}},
		},
	}
	for _, mode := range []string{"smart", "balanced", "sequential", "sticky_project", "quota_first", "drain"} {
		t.Run(mode, func(t *testing.T) {
			pool.Scheduling.Mode = mode
			for _, pin := range []string{"", "account:" + a, "group:team", "group:", "missing:legacy"} {
				pool.Scheduling.ProjectPins = map[string]string{"/work": pin}
				for _, active := range []map[string]int{nil, {a: 1, c: 3}} {
					full := pickAgentAccount(pool, active, map[string]bool{c: true}, "/work", b, a, now.Add(-time.Minute), now)
					fast, idx := chooseAgentAccount(pool, active, map[string]bool{c: true}, "/work", b, a, now.Add(-time.Minute), now, false)
					if len(fast.Candidates) != 0 || fast.AccountID != full.AccountID || fast.Score != full.Score || fast.Reason != full.Reason {
						t.Fatalf("pin %q active %v: fast=%+v preview=%+v", pin, active, fast, full)
					}
					if idx >= 0 && pool.Accounts[idx].ID != full.AccountID {
						t.Fatalf("wrong index %d for %+v", idx, full)
					}
					if pin == "group:" || pin == "missing:legacy" || pin == "account:"+a && active[a] > 0 {
						if idx >= 0 {
							t.Fatalf("invalid or blocked pin %q selected %+v", pin, full)
						}
					} else if idx < 0 || !full.Candidates[idx].Eligible || full.Candidates[idx].ActiveSessions != active[full.AccountID] {
						t.Fatalf("expected eligible selection with actual demand: %+v", full)
					}
				}
			}
		})
	}

	pool.Scheduling = defaultAgentScheduling()
	pool.Accounts = []agentAccount{{ID: a, Enabled: true, Status: "ready"}, {ID: b, Enabled: true, Status: "ready"}}
	last := a
	for _, want := range []string{b, a, b, a} {
		got := pickAgentAccount(pool, nil, nil, "", last, "", time.Time{}, now)
		if got.AccountID != want {
			t.Fatalf("tie rotation after %q picked %q, want %q", last, got.AccountID, want)
		}
		last = got.AccountID
	}
}

func TestAgentPoolAdmissionCountsT3AndPendingLogin(t *testing.T) {
	s := testServer(t)
	spec, _ := agentcatalog.Lookup("opencode", nil)
	id, spare := strings.Repeat("a", 24), strings.Repeat("b", 24)
	pool := &agentAccountPool{
		Accounts: []agentAccount{
			{ID: id, Source: "managed", Enabled: true, Status: "ready", MaxConcurrency: 1},
			{ID: spare, Source: "managed", Enabled: true, Status: "ready", MaxConcurrency: 2},
		},
		Scheduling: agentScheduling{Mode: "sequential", ProjectPins: map[string]string{"/pinned": "account:" + id}},
	}
	s.agentCLIs.pools, s.agentCLIs.poolsLoaded = map[string]*agentAccountPool{spec.ID: pool}, true
	cfg := defaultT3CodeConfig()
	cfg.Enabled = true
	s.t3code.config, s.t3code.configLoaded = cfg, true
	s.t3code.statusCache = t3CodeGuestStatus{Running: true, PID: 42, Port: cfg.Port, Accounts: map[string]string{spec.ID: id}}
	s.t3code.statusAt = time.Now()

	s.op.Lock()
	defer s.op.Unlock()
	if sel, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || sel.ID != spare {
		t.Fatalf("active T3 binding should route auto to the spare: %+v, %v", sel, err)
	}
	for _, req := range []struct {
		id    string
		login bool
	}{{"auto", false}, {id, false}, {id, true}} {
		if sel, _, err := s.selectAgentAccount(spec, req.id, req.login, "/pinned"); err == nil {
			t.Fatalf("active T3 binding should block %q login=%t: %+v", req.id, req.login, sel)
		}
	}
	if sel, _, err := s.selectAgentAccountReplacing(spec, "auto", false, "/pinned", id); err != nil || sel.ID != id {
		t.Fatalf("restart should reuse its own seat: %+v, %v", sel, err)
	}
	if sel, _, err := s.selectAgentAccount(spec, spare, false, "/pinned"); err != nil || sel.ID != spare {
		t.Fatalf("explicit new launch should override project pin: %+v, %v", sel, err)
	}
	s.terminals.sessions = map[string]*terminalSession{
		"terminal": {agentID: spec.ID, accountID: id, kind: "agent", done: make(chan struct{})},
	}
	if _, _, err := s.selectAgentAccountReplacing(spec, id, false, "", id); err == nil {
		t.Fatal("replacing T3 incorrectly subtracted a terminal seat")
	}
	s.agentCLIs.confirmInflight = map[string]*agentConfirmCall{spec.ID + ":" + spare: {done: make(chan struct{})}}
	if _, _, err := s.selectAgentAccount(spec, spare, false, ""); err == nil {
		t.Fatal("normal launch entered a profile whose login is being confirmed")
	}
	if _, _, err := s.selectAgentAccount(spec, spare, true, ""); err == nil {
		t.Fatal("a second login entered a profile whose login is being confirmed")
	}
	delete(s.agentCLIs.confirmInflight, spec.ID+":"+spare)
	pool.Accounts[1].Enabled, pool.Accounts[1].Status = false, "signed_out"
	if sel, _, err := s.selectAgentAccount(spec, spare, true, ""); err != nil || sel.ID != spare {
		t.Fatalf("known account should still permit explicit recovery login: %+v, %v", sel, err)
	}
}

func TestAgentPoolConcurrentAdmissionAndRelease(t *testing.T) {
	s := testServer(t)
	spec, _ := agentcatalog.Lookup("opencode", nil)
	id := strings.Repeat("a", 24)
	s.agentCLIs.poolsLoaded = true
	s.agentCLIs.pools = map[string]*agentAccountPool{
		spec.ID: {Accounts: []agentAccount{{ID: id, Enabled: true, Status: "ready", MaxConcurrency: 2}}},
	}
	s.terminals.sessions = make(map[string]*terminalSession)
	results := make(chan bool, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			s.op.Lock()
			defer s.op.Unlock()
			sel, _, err := s.selectAgentAccount(spec, "auto", false, "")
			if err == nil {
				s.terminals.mu.Lock()
				s.terminals.sessions[fmt.Sprint(i)] = &terminalSession{
					agentID: spec.ID, accountID: sel.ID, kind: "agent", done: make(chan struct{}),
				}
				s.terminals.mu.Unlock()
			}
			results <- err == nil
		})
	}
	wg.Wait()
	close(results)
	var admitted int
	for ok := range results {
		if ok {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d launches, cap is 2", admitted)
	}
	s.op.Lock()
	defer s.op.Unlock()
	for _, session := range s.terminals.sessions {
		close(session.done)
		break
	}
	if sel, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || sel.ID != id {
		t.Fatalf("finished terminal should release a seat: %+v, %v", sel, err)
	}
}

func TestAgentPoolDefaultTerminalAndSettlingLogin(t *testing.T) {
	s := testServer(t)
	spec, _ := agentcatalog.Lookup("opencode", nil)
	id := strings.Repeat("a", 24)
	pool := &agentAccountPool{
		Accounts: []agentAccount{{ID: id, Source: "default", Enabled: true, Status: "ready", MaxConcurrency: 1}},
	}
	s.agentCLIs.pools, s.agentCLIs.poolsLoaded = map[string]*agentAccountPool{spec.ID: pool}, true
	defaultSession := &terminalSession{agentID: spec.ID, kind: "agent", done: make(chan struct{})}
	s.terminals.sessions = map[string]*terminalSession{"existing": defaultSession}
	if _, _, err := s.selectAgentAccount(spec, "auto", false, ""); err == nil {
		t.Fatal("joining the default roster ignored an existing default terminal")
	}
	if _, _, err := s.selectAgentAccount(spec, "default", false, ""); err == nil {
		t.Fatal("explicit default bypassed the same default profile capacity")
	}
	close(defaultSession.done)
	pool.Accounts[0].Source = "managed"

	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() {
		releaseOnce.Do(func() { close(release) })
		s.terminals.workers.Wait()
	}
	t.Cleanup(finish)
	session, err := s.terminals.startSessionWithSetup(nil, "", "agent-login", claudeSelection{}, func() {
		<-release
	}, func() (terminalPTY, error) {
		return idlePTY{}, nil
	}, func(session *terminalSession, _ int) {
		session.agentID, session.accountID = spec.ID, id
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("login PTY did not stop")
	}
	s.terminals.remove(session.id)
	if s.terminals.liveAgentLogin(spec.ID, id) {
		t.Fatal("ended login still reported as a running PTY")
	}
	if _, _, err := s.selectAgentAccount(spec, "auto", false, ""); err == nil {
		t.Fatal("removed terminal freed the profile before login verification finished")
	}
	if existing, err := s.terminals.existingAgentLogin(spec.ID, id, true); existing != nil || err == nil {
		t.Fatal("a settling login was reattached or could be replaced")
	}
	finish()
	if sel, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || sel.ID != id {
		t.Fatalf("login verification did not release the profile: %+v, %v", sel, err)
	}
}

func TestAgentPoolFailedRosterSavePreservesLiveState(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.closeAgents)
	spec, _ := agentcatalog.Lookup("opencode", nil)
	id := strings.Repeat("a", 24)
	pool := &agentAccountPool{
		Accounts:   []agentAccount{{ID: id, Enabled: true, Status: "ready"}},
		Scheduling: defaultAgentScheduling(),
	}
	s.agentCLIs.pools, s.agentCLIs.poolsLoaded = map[string]*agentAccountPool{spec.ID: pool}, true
	mode := "drain"
	if err := s.updateAgentScheduling(spec.ID, agentAccountMutation{Scheduling: &agentSchedulingInput{Mode: &mode}, Order: []string{"bad"}}); err == nil {
		t.Fatal("invalid order was accepted")
	}
	if pool.Scheduling.Mode != "smart" {
		t.Fatalf("failed order changed live scheduling to %q", pool.Scheduling.Mode)
	}

	roster := filepath.Join(s.profile, "agent-accounts.json")
	if err := os.Mkdir(roster, 0700); err != nil {
		t.Fatal(err)
	}
	s.noteAgentSelection(spec.ID, id, "/")
	if err := s.setAgentAccountCooldown(spec.ID, agentAccountMutation{ID: id, CooldownSeconds: 60}); err == nil {
		t.Fatal("expected roster write failure")
	}
	s.flushAgentPools()
	s.agentCLIs.mu.Lock()
	preserved := s.agentCLIs.poolsDirty && s.agentCLIs.flushTimer != nil && s.agentCLIs.pools[spec.ID].Accounts[0].CooldownUntil == 0
	s.agentCLIs.mu.Unlock()
	if !preserved {
		t.Fatal("failed write changed the live pool or discarded the pending flush")
	}
	if err := os.Rename(roster, filepath.Join(t.TempDir(), "blocked-roster")); err != nil {
		t.Fatal(err)
	}
	s.flushAgentPools()
	reopened := &Server{profile: s.profile}
	saved, err := reopened.loadAgentPoolsLocked()
	if err != nil || saved[spec.ID] == nil || saved[spec.ID].Accounts[0].TotalAllocations != 1 {
		t.Fatalf("allocation not recovered after write failure: %v, %v", saved, err)
	}
}

func TestAgentPoolConfirmedLoginUsesGuestProfileAndProtectsIdentity(t *testing.T) {
	s := testServer(t)
	spec, _ := agentcatalog.Lookup("opencode", nil)
	id, guestID := strings.Repeat("a", 24), strings.Repeat("b", 24)
	s.agentCLIs.pools, s.agentCLIs.poolsLoaded = map[string]*agentAccountPool{
		spec.ID: {Accounts: []agentAccount{{ID: id, ProfileID: guestID, Email: "expect@example.test", Enabled: true, Status: "ready"}}},
	}, true
	s.agentCLIs.runtimeHook = func(_ context.Context, cmd string, input, output any) error {
		if cmd == "sync-auto-shims" {
			return nil
		}
		if cmd != "snapshot" || input.(map[string]any)["account_id"] != guestID {
			return errors.New("probe received the roster ID instead of the guest profile ID")
		}
		*output.(*agentGuestSnapshot) = agentGuestSnapshot{LoggedIn: true, Email: "changed@example.test"}
		return nil
	}
	if err := s.confirmAgentLogin(context.Background(), spec.ID, id); err != nil {
		t.Fatal(err)
	}
	s.agentCLIs.mu.Lock()
	a := *s.agentCLIs.pools[spec.ID].find(id)
	s.agentCLIs.mu.Unlock()
	if a.Status != "changed" || a.Email != "expect@example.test" {
		t.Fatalf("observation changed account identity: %+v", a)
	}
	if _, _, err := s.selectAgentAccount(spec, id, false, ""); err == nil {
		t.Fatal("sticky roster ID launched a different identity")
	}
}

func TestAgentPoolQuotaObservationDoesNotRefreshOtherWindows(t *testing.T) {
	now := time.Unix(1760000000, 0)
	old := now.Add(-2 * time.Hour).Unix()
	a := agentAccount{Quota: &claudeQuota{ObservedAt: old, SevenDay: &claudeWindow{UsedPercentage: 100}}}
	if _, ok := a.headroom(now); !ok {
		t.Fatal("stale exhausted quota with unknown reset should not block")
	}
	fresh := &claudeQuota{ObservedAt: now.Unix(), FiveHour: &claudeWindow{UsedPercentage: 20}}
	if !a.mergeQuota(fresh, now) || a.Quota.SevenDay.ObservedAt != old {
		t.Fatalf("merge did not preserve the old observation: %+v", a.Quota.SevenDay)
	}
	if room, ok := a.headroom(now); !ok || room != 80 {
		t.Fatalf("stale seven-day observation masked fresh short quota: %v, %v", room, ok)
	}
	if a.mergeQuota(&claudeQuota{ObservedAt: now.Unix(), FiveHour: &claudeWindow{UsedPercentage: 100, ObservedAt: now.Unix() + 61}}, now) {
		t.Fatal("out-of-range future observation was accepted")
	}
	a.Quota = &claudeQuota{ObservedAt: old, SevenDay: &claudeWindow{UsedPercentage: 100, ResetsAt: now.Add(time.Minute).Unix()}}
	if _, ok := a.headroom(now); ok {
		t.Fatal("confirmed exhausted window with a future reset was ignored")
	}
	if room, ok := a.headroom(now.Add(time.Minute)); !ok || room != -1 {
		t.Fatalf("expired window did not release the account: %v, %v", room, ok)
	}
}
