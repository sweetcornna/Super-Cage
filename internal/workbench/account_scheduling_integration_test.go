package workbench

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func TestAccountPolicyNoGuestReadWithoutCandidates(t *testing.T) {
	spec, _ := agentcatalog.Lookup("opencode", nil)
	paused, signedOut, ready := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24)
	for _, mode := range []string{"smart", "exhaust", "manual"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			s.orca.config = defaultOrcaConfig(s.profile)
			s.orca.config.Enabled, s.orca.configLoaded = true, true
			s.t3code.config = defaultT3CodeConfig()
			s.t3code.config.Enabled, s.t3code.configLoaded = true, true
			s.agentCLIs.runtimeHook = func(_ context.Context, command string, _, _ any) error {
				t.Errorf("unavailable schedule queried the guest: %s", command)
				return nil
			}

			generic := []agentAccount{
				{ID: paused, Status: "ready", MaxConcurrency: 1},
				{ID: signedOut, Enabled: true, Status: "signed_out"},
			}
			claude := []claudeAccount{
				{ID: paused, Status: "ready", Email: "paused@example.test"},
				{ID: signedOut, Enabled: true, Status: "signed_out", Email: "signed-out@example.test"},
			}
			codex := []codexAccount{codexFixture(paused, "paused@example.test"), codexFixture(signedOut, "signed-out@example.test")}
			codex[0].Enabled, codex[1].Status = false, "signed_out"
			if mode == "manual" {
				// An otherwise ready account cannot take a manual launch from
				// the chosen, paused account.
				generic = append(generic, agentAccount{ID: ready, Enabled: true, Status: "ready"})
				claude = append(claude, claudeAccount{ID: ready, Enabled: true, Status: "ready", Email: "ready@example.test"})
				codex = append(codex, codexFixture(ready, "ready@example.test"))
			}
			s.agentCLIs.poolsLoaded = true
			s.agentCLIs.pools = map[string]*agentAccountPool{spec.ID: {
				Scheduling: agentScheduling{Mode: mode, SelectedAccountID: paused},
				Accounts:   generic,
			}}
			policy := claudeScheduling{Mode: mode, SelectedAccountID: paused}
			claudeList := claudeAccountList{Accounts: claude, Scheduling: policy}
			codexList := codexAccountList{Accounts: codex, Scheduling: policy}
			if err := s.saveClaudeAccounts(claudeList); err != nil {
				t.Fatal(err)
			}
			if err := s.saveCodexAccounts(codexList); err != nil {
				t.Fatal(err)
			}
			if got, _, err := s.selectAgentAccount(spec, "auto", false, "/fixture"); err == nil || got.GuestID != "" {
				t.Errorf("unavailable generic selection = %+v, %v", got, err)
			}
			if got, err := s.allocateClaudeAccount(claudeList, "", func(context.Context, []string) (map[string]claudeGuestQuota, error) {
				t.Error("unavailable Claude schedule read guest quotas")
				return nil, nil
			}); err == nil || got.GuestID != "" {
				t.Errorf("unavailable Claude selection = %+v, %v", got, err)
			}
			if got, err := s.allocateCodexAccount(codexList, "", func(context.Context, []string) (map[string]codexGuestQuota, error) {
				t.Error("unavailable Codex schedule read guest quotas")
				return nil, nil
			}); err == nil || got.GuestID != "" {
				t.Errorf("unavailable Codex selection = %+v, %v", got, err)
			}
		})
	}
}

func TestAccountPolicyEmptyExhaustDoesNotReadGuest(t *testing.T) {
	s := testServer(t)
	spec, _ := agentcatalog.Lookup("opencode", nil)
	s.agentCLIs.poolsLoaded = true
	s.agentCLIs.pools = map[string]*agentAccountPool{spec.ID: {Scheduling: agentScheduling{Mode: "exhaust"}}}
	s.agentCLIs.runtimeHook = func(_ context.Context, command string, _, _ any) error {
		t.Errorf("empty schedule queried the guest: %s", command)
		return nil
	}
	if got, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || got.GuestID != "default" {
		t.Fatalf("unconfigured exhaust selection = %+v, %v", got, err)
	}
}

func TestAccountPolicyClaudeRetainsPausedLegacyPreferred(t *testing.T) {
	s := testServer(t)
	id, profile := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := claudeAccountList{
		Scheduling: claudeScheduling{Mode: "manual", Preferred: id},
		Accounts:   []claudeAccount{{ID: id, ProfileID: profile, Status: "ready", Email: "preferred@example.test"}},
	}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	called := false
	got, err := s.allocateClaudeAccount(list, "", func(_ context.Context, ids []string) (map[string]claudeGuestQuota, error) {
		called = true
		if len(ids) != 1 || ids[0] != profile {
			t.Errorf("legacy preferred quota request = %v", ids)
		}
		return nil, nil
	})
	if err != nil || !called || got.ID != id || got.GuestID != profile {
		t.Fatalf("paused legacy preferred selection = %+v, %v, quota queried %t", got, err, called)
	}
}

func TestAccountPolicyConcurrentSelectRejectsStaleGuestCursor(t *testing.T) {
	ids := []string{strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24)}
	for _, agentID := range []string{"opencode", "claude", "codex"} {
		t.Run(agentID, func(t *testing.T) {
			s := testServer(t)
			spec, _ := agentcatalog.Lookup(agentID, nil)
			first, stale, selected := ids[0], ids[1], ids[2]
			const revision = "fixture-before-select"
			policy := claudeScheduling{Mode: "exhaust", SelectedAccountID: first, SelectedAccountRevision: revision}
			claude := claudeAccountList{Scheduling: policy}
			codex := codexAccountList{Scheduling: policy}
			pool := &agentAccountPool{Scheduling: agentScheduling{Mode: "exhaust", SelectedAccountID: first, SelectedAccountRevision: revision}}
			for _, id := range ids {
				claude.Accounts = append(claude.Accounts, claudeAccount{ID: id, Enabled: true, Status: "ready", Email: id + "@example.test"})
				codex.Accounts = append(codex.Accounts, codexFixture(id, id+"@example.test"))
				pool.Accounts = append(pool.Accounts, agentAccount{ID: id, Enabled: true, Status: "ready"})
			}
			switch agentID {
			case "opencode":
				s.agentCLIs.poolsLoaded = true
				s.agentCLIs.pools = map[string]*agentAccountPool{agentID: pool}
			case "claude":
				if err := s.saveClaudeAccounts(claude); err != nil {
					t.Fatal(err)
				}
			case "codex":
				if err := s.saveCodexAccounts(codex); err != nil {
					t.Fatal(err)
				}
			}
			reading, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			s.agentCLIs.runtimeHook = func(ctx context.Context, command string, _, out any) error {
				if command == "schedule-cursor" {
					close(reading)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					*out.(*guestSchedulingCursor) = guestSchedulingCursor{AccountID: stale, SelectedAccountID: first, SelectedAccountRevision: revision}
				}
				return nil
			}
			result := make(chan error, 1)
			go func() {
				var id string
				var err error
				switch agentID {
				case "opencode":
					got, _, launchErr := s.selectAgentAccount(spec, "auto", false, "")
					id, err = got.ID, launchErr
				case "claude":
					got, launchErr := s.allocateClaudeAccount(claude, "", func(context.Context, []string) (map[string]claudeGuestQuota, error) { return nil, nil })
					id, err = got.ID, launchErr
				case "codex":
					got, launchErr := s.allocateCodexAccount(codex, "", func(context.Context, []string) (map[string]codexGuestQuota, error) { return nil, nil })
					id, err = got.ID, launchErr
				}
				if err == nil && id != selected {
					err = fmt.Errorf("picked %q, want %q", id, selected)
				}
				result <- err
			}()
			select {
			case <-reading:
			case err := <-result:
				t.Fatalf("launch returned before reading the cursor: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("launch did not reach the guest cursor")
			}
			if err := s.selectSchedulingAccount(agentID, selected); err != nil {
				t.Fatal(err)
			}
			release <- struct{}{}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("launch did not return after the cursor was released")
			}
		})
	}
}

func BenchmarkUnavailableAgentScheduling(b *testing.B) {
	spec, _ := agentcatalog.Lookup("opencode", nil)
	for _, mode := range []string{"smart", "exhaust"} {
		b.Run(mode, func(b *testing.B) {
			account := &localAccount{record: &accountRecord{Version: 1, Username: "Fixture"}}
			account.setKey(make([]byte, 32))
			b.Cleanup(account.lock)
			s := &Server{account: account, profile: b.TempDir()}
			pool := &agentAccountPool{Scheduling: agentScheduling{Mode: mode}}
			for i := range 16 {
				pool.Accounts = append(pool.Accounts, agentAccount{ID: fmt.Sprintf("%024x", i+1), Status: "ready", MaxConcurrency: 1})
			}
			s.agentCLIs.poolsLoaded = true
			s.agentCLIs.pools = map[string]*agentAccountPool{spec.ID: pool}
			calls := 0
			s.agentCLIs.runtimeHook = func(_ context.Context, _ string, _, _ any) error {
				calls++
				return nil
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := s.selectAgentAccount(spec, "auto", false, ""); err == nil {
					b.Fatal("paused roster unexpectedly admitted a launch")
				}
			}
			b.ReportMetric(float64(calls)/float64(b.N), "guest-calls/op")
		})
	}
}
