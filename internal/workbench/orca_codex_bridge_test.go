package workbench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrcaCodexReconcilesStandaloneSelectionAndSkipsUnchanged(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.closeAgents)
	ctx := context.Background()
	a := codexAccount{ID: strings.Repeat("a", 24), Enabled: true, Status: "ready", Email: "owner@example.test", Organization: "a"}
	b := codexAccount{ID: strings.Repeat("b", 24), Enabled: true, Status: "ready", Email: a.Email, Organization: "b"}
	list := codexAccountList{Accounts: []codexAccount{a, b}, Scheduling: claudeScheduling{Mode: "manual"}}
	list.Scheduling.selectAccount(a.ID)
	path := filepath.Join(s.profile, "codex-accounts.json")
	if err := writePrivate(path, list); err != nil {
		t.Fatal(err)
	}
	var selected, stamp = "system", "initial"
	var probes, switches int
	s.agentCLIs.runtimeHook = func(_ context.Context, op string, input, output any) error {
		if op != "orca-codex-accounts" {
			return nil
		}
		in := input.(map[string]any)
		out := output.(*orcaCodexSnapshot)
		if in["managed_port"] != 0 {
			t.Fatal("standalone discovery required an enabled Cage-managed server")
		}
		if in["fingerprint"] == stamp && in["target"] == nil {
			*out = orcaCodexSnapshot{Available: true, Unchanged: true, Fingerprint: stamp}
			return nil
		}
		probes++
		switch in["target"] {
		case a.ID:
			if in["expected_runtime_id"] != "fixture-runtime" || in["expected_orca_account_id"] != selected {
				t.Fatalf("bad optimistic switch: %#v", in)
			}
			selected, stamp = "orca-a", "switch-to-a"
			switches++
		case b.ID:
			t.Fatal("native selection was echoed back")
		}
		*out = orcaCodexSnapshot{Available: true, RuntimeID: "fixture-runtime", OrcaAccountID: selected, Fingerprint: stamp, RegistrationSupported: true}
		if selected == "orca-a" {
			out.CageAccountID, out.Subject = a.ID, a.subject()
		} else if selected == "orca-b" {
			out.CageAccountID, out.Subject = b.ID, b.subject()
		}
		return nil
	}

	cfg := defaultOrcaConfig(s.profile)
	var seen orcaCodexObservation
	if view := s.reconcileOrcaCodex(ctx, cfg, &seen); view.AccountID != a.ID || !view.RegistrationSupported || switches != 1 {
		t.Fatalf("Cage selection did not reach Orca: %#v", view)
	}
	firstProbes := probes
	for range 3 {
		if view := s.reconcileOrcaCodex(ctx, cfg, &seen); view.AccountID != a.ID {
			t.Fatalf("unchanged observation lost selection: %#v", view)
		}
	}
	if probes != firstProbes {
		t.Fatalf("steady state made %d additional native probes", probes-firstProbes)
	}

	selected, stamp = "orca-b", "switch-to-b"
	if view := s.reconcileOrcaCodex(ctx, cfg, &seen); view.AccountID != b.ID {
		t.Fatalf("Orca selection did not resolve to Cage: %#v", view)
	}
	got, err := s.loadCodexAccounts()
	if err != nil || got.Scheduling.SelectedAccountID != b.ID || got.Scheduling.SelectedAccountRevision == list.Scheduling.SelectedAccountRevision {
		t.Fatalf("native switch did not change Cage's scheduler selection: %#v, %v", got.Scheduling, err)
	}
	if switches != 1 {
		t.Fatalf("native switch caused %d Cage writes to Orca", switches-1)
	}
}
