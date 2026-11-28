package workbench

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestMagpieDesktopScopeReadsActualCommandAndOnlyMutatesReviewedRoute(t *testing.T) {
	s := testServer(t)
	s.ready = true
	s.preferences.CodexTerminalCommand = "codex -p magpie"
	const review = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	checked, changed := 0, 0
	s.agentCLIs.runtimeHook = func(_ context.Context, command string, input, output any) error {
		if command != "magpie-route" {
			return nil
		}
		payload := input.(map[string]any)
		actions := payload["agents"].(map[string][]string)
		if len(actions) != 1 || strings.Join(actions["codex"], " ") != "-p magpie" {
			t.Fatalf("wrong scope: %+v", payload)
		}
		var state magpieRoute
		if payload["action"] == "status" {
			checked++
			state = magpieRoute{State: "unknown", Scope: "Codex profile (magpie)", Review: review}
		} else {
			if payload["review"] != review {
				return errors.New("fixture: stale review")
			}
			changed++
			state = magpieRoute{State: "magpie", Confirmed: true, Review: review}
		}
		*output.(*map[string]magpieRoute) = map[string]magpieRoute{"codex": state}
		return nil
	}
	if native, err := s.magpieNewLaunch("codex", "codex -p magpie", ""); native || !errors.Is(err, errMagpieRouteUnknown) {
		t.Fatalf("ambiguous route selected an account: %v, %v", native, err)
	}
	url := "/api/magpie/route"
	w := request(s, "POST", url, s.origin, `{"agent":"codex","action":"status"}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), review) {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	w = request(s, "POST", url, s.origin, `{"agent":"codex","action":"magpie","review":"`+review+`"}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"confirmed":true`) || checked != 2 || changed != 1 {
		t.Fatalf("confirmation: %d %s (%d probes, %d mutations)", w.Code, w.Body.String(), checked, changed)
	}
}

func TestMagpieDecisionSkipsOnlyAutomaticClaudeTerminal(t *testing.T) {
	s := testServer(t)
	s.ready = true
	id := strings.Repeat("a", 24)
	account := claudeAccount{ID: id, Source: "managed", Status: "ready", Enabled: true, Email: "fixture@example.test"}
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{account}, Scheduling: claudeScheduling{Mode: "manual", SelectedAccountID: id}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.agentCLIs.runtimeHook = func(_ context.Context, verb string, input, output any) error {
		if verb == "magpie-route" {
			calls++
			*output.(*map[string]magpieRoute) = map[string]magpieRoute{"claude": {State: "magpie"}}
		}
		return nil
	}
	if s.magpieNativeAutoEligible("claude") {
		t.Fatal("manual account was treated as an automatic launch")
	}
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{account}, Scheduling: claudeScheduling{Mode: "smart"}}); err != nil {
		t.Fatal(err)
	}
	if sel, err := s.selectTerminalClaudeAccount("claude", "", false, "/project"); err != nil || sel != (claudeSelection{}) || calls != 1 {
		t.Fatalf("expected native launch without Cage identity: %+v %v (%d probes)", sel, err, calls)
	}
	if _, err := s.selectTerminalClaudeAccount("claude", id, false, "/project"); err == nil || calls != 1 {
		t.Fatalf("explicit account request bypassed validation: %v (%d probes)", err, calls)
	}
}
