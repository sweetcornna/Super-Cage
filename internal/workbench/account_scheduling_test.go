package workbench

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

func TestAccountPolicyNativeManualAndExhaust(t *testing.T) {
	now := time.Now()
	ids := []string{strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24)}
	a, b, c := ids[0], ids[1], ids[2]
	claude := claudeAccountList{}
	codex := codexAccountList{}
	for _, id := range ids {
		claude.Accounts = append(claude.Accounts, claudeAccount{ID: id, Email: id + "@example.test", Enabled: true, Status: "ready"})
		codex.Accounts = append(codex.Accounts, codexFixture(id, id+"@example.test"))
	}
	claude.Accounts[2].Quota = &claudeQuota{ObservedAt: now.Unix(), FiveHour: &claudeWindow{
		UsedPercentage: 100, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix(),
	}}
	codex.Accounts[2].Quota = exhaustedCodexQuota(now)
	for _, scenario := range []struct{ mode, selected, want string }{
		{"manual", "", ""},
		{"manual", b, b},
		{"manual", c, ""},
		{"exhaust", b, b},
		{"exhaust", c, a},
		{"sequential", c, a},
		{"smart", b, a},
	} {
		sched := claudeScheduling{Mode: scenario.mode, SelectedAccountID: scenario.selected}
		claude.Scheduling, codex.Scheduling = sched, sched
		load := map[string]int{b: 3}
		if got := pickClaudeAccount(claude, load, nil, "", "", now); got != scenario.want {
			t.Errorf("Claude %+v picked %q", scenario, got)
		}
		if got := pickCodexAccount(codex, load, nil, "", "", now); got != scenario.want {
			t.Errorf("Codex %+v picked %q", scenario, got)
		}
	}
}

func TestAccountPolicyCodexAdoptsCurrentGuestCursor(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := codexAccountList{
		Scheduling: claudeScheduling{Mode: "exhaust", SwitchPolicy: "ask", SelectedAccountID: a, SelectedAccountRevision: "initial"},
		Accounts:   []codexAccount{codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test")},
	}
	if err := s.saveCodexAccounts(list); err != nil {
		t.Fatal(err)
	}
	s.agentCLIs.runtimeHook = func(_ context.Context, command string, input, out any) error {
		if command == "schedule-cursor" {
			if req, ok := input.(map[string]any); !ok || req["agent_id"] != "codex" || req["codex_native"] != true {
				t.Errorf("invalid cursor request %+v", input)
			}
			*out.(*guestSchedulingCursor) = guestSchedulingCursor{AccountID: b, SelectedAccountID: a, SelectedAccountRevision: "initial"}
		}
		return nil
	}
	noQuota := func(context.Context, []string) (map[string]codexGuestQuota, error) { return nil, nil }
	if picked, err := s.allocateCodexAccount(list, "", noQuota); err != nil || picked.ID != b {
		t.Fatalf("did not keep guest successor: %+v, %v", picked, err)
	}
	after, err := s.loadCodexAccounts()
	if err != nil || after.Scheduling.SelectedAccountID != b || after.Scheduling.SelectedAccountRevision == "initial" {
		t.Fatalf("guest successor was not durable: %+v, %v", after.Scheduling, err)
	}
	if err := s.selectSchedulingAccount("codex", a); err != nil {
		t.Fatal(err)
	}
	after, err = s.loadCodexAccounts()
	if err != nil || after.Scheduling.Mode != "exhaust" || after.Scheduling.SelectedAccountID != a {
		t.Fatalf("explicit switch did not repoint exhaust: %+v, %v", after.Scheduling, err)
	}
	if picked, err := s.allocateCodexAccount(after, "", noQuota); err != nil || picked.ID != a {
		t.Fatalf("previous guest cursor overrode explicit switch: %+v, %v", picked, err)
	}
}

func TestAccountPolicyAgentSelectionAPIAndPins(t *testing.T) {
	s := testServer(t)
	spec, _, ok := s.lookupAgentSpec("opencode")
	if !ok {
		t.Fatal("missing fixture agent")
	}
	a, b, c := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24)
	pool := &agentAccountPool{Scheduling: defaultAgentScheduling(), Accounts: []agentAccount{
		{ID: a, Enabled: true, Status: "ready"},
		{ID: b, Enabled: true, Status: "ready", MaxConcurrency: 1},
		{ID: c, Enabled: true, Status: "ready", AuthMode: "api_key", Source: "api_key", Provider: "example"},
	}}
	s.agentCLIs.poolsLoaded = true
	s.agentCLIs.pools = map[string]*agentAccountPool{spec.ID: pool}
	endpoint := "/api/agent-clis/opencode/accounts"
	if w := request(s, "POST", endpoint, s.origin, `{"action":"select","id":"`+b+`"}`, true); w.Code != 200 {
		t.Fatalf("select: %d %s", w.Code, w.Body)
	} else {
		var view struct {
			API        int             `json:"scheduling_api"`
			Scheduling agentScheduling `json:"scheduling"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.API != schedulingAPI ||
			view.Scheduling.Mode != "manual" || view.Scheduling.SelectedAccountID != b ||
			!view.Scheduling.AutoCooldown || view.Scheduling.ProjectAffinityMinutes != 120 {
			t.Fatalf("unexpected selection API response: %+v %v", view, err)
		}
	}
	s.terminals.sessions = map[string]*terminalSession{"busy": {agentID: spec.ID, accountID: b, done: make(chan struct{})}}
	if _, _, err := s.selectAgentAccount(spec, "auto", false, ""); err == nil || !strings.Contains(err.Error(), "手动") {
		t.Fatalf("manual did not report the locked account at capacity: %v", err)
	}
	if w := request(s, "POST", endpoint, s.origin, `{"action":"scheduling","scheduling":{"mode":"exhaust","project_pins":{"/pin":"account:`+a+`"}}}`, true); w.Code != 200 {
		t.Fatalf("configure exhaust: %d %s", w.Code, w.Body)
	}
	if got, _, err := s.selectAgentAccount(spec, "auto", false, "/pin"); err != nil || got.ID != a {
		t.Fatalf("project pin failed: %+v %v", got, err)
	}
	if got, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || got.ID != c || got.AuthMode != "api_key" {
		t.Fatalf("exhaust did not advance in pool order: %+v %v", got, err)
	}
	delete(s.terminals.sessions, "busy")
	if got, _, err := s.selectAgentAccount(spec, "auto", false, ""); err != nil || got.ID != c {
		t.Fatalf("exhaust did not retain successor with unknown quota: %+v %v", got, err)
	}
	schedule := agentLaunchScheduleOf(s.agentCLIs.pools, []agentcatalog.AgentSpec{spec}, nil, time.Now()).Agents[spec.ID]
	if schedule.Mode != "exhaust" || schedule.SelectedAccountID != c || schedule.SelectedAccountRevision == "" ||
		!slices.Equal(schedule.AccountOrder, []string{a, b, c}) {
		t.Fatalf("invalid published policy: %+v", schedule)
	}
}

func TestAccountPolicyManualEmptyDoesNotUseDefaultAndHistoryCanResume(t *testing.T) {
	s := testServer(t)
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{}, Scheduling: claudeScheduling{Mode: "manual"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectClaudeAccount("auto", false, ""); err == nil || !strings.Contains(err.Error(), "手动") {
		t.Fatalf("manual mode fell back to default: %v", err)
	}
	id := strings.Repeat("a", 24)
	claude := claudeAccount{ID: id, Email: "history@example.test", Status: "ready"}
	list := claudeAccountList{Accounts: []claudeAccount{claude}, Scheduling: claudeScheduling{Mode: "manual"}}
	history := claudeSelection{ID: id, GuestID: id, Subject: claudeIdentityHash(claude.Email, "")}
	if err := s.validateBoundClaudeAccount(list, history); err != nil {
		t.Fatalf("history could not resume: %v", err)
	}
}

func TestAccountPolicyClaudeManualKeepsSharedDefaultIdentity(t *testing.T) {
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	original := claudeAccount{ID: a, Source: "default", Email: "original@example.test", Enabled: true, Status: "ready"}
	current := claudeAccount{ID: b, Source: "default", Email: "current@example.test", Enabled: true, Status: "ready"}
	list := claudeAccountList{
		Accounts:   []claudeAccount{original, current},
		Scheduling: claudeScheduling{Mode: "manual", SelectedAccountID: a},
	}
	schedule := claudeLaunchScheduleOf(list)
	if schedule.SelectedAccountID != "default" || len(schedule.Accounts) != 1 ||
		schedule.Accounts[0].Subject != claudeIdentityHash(original.Email, "") {
		t.Fatalf("manual policy allowed a different identity under the same profile: %+v", schedule)
	}
}

func TestAccountPolicyUnverifiedQuotaIsNotExhaustion(t *testing.T) {
	now := time.Now()
	noObservation := &claudeWindow{UsedPercentage: 100, ResetsAt: now.Add(time.Hour).Unix()}
	windowQuota := &claudeQuota{FiveHour: noObservation}
	for name, account := range map[string]interface {
		headroom(time.Time) (float64, bool)
	}{
		"claude": claudeAccount{Quota: windowQuota},
		"codex": codexAccount{Quota: &codexQuota{Primary: &codexWindow{
			UsedPercentage: noObservation.UsedPercentage, ResetsAt: noObservation.ResetsAt,
		}}},
		"agent": agentAccount{Quota: windowQuota},
	} {
		if room, available := account.headroom(now); room != -1 || !available {
			t.Errorf("%s treated unknown quota as exhausted: %v %v", name, room, available)
		}
	}
}

func TestAccountPolicyNativeQuotaMergesKeepOldWindowTimestamp(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * claudeQuotaFresh).Unix()
	claude := claudeAccount{Quota: &claudeQuota{ObservedAt: old, FiveHour: &claudeWindow{UsedPercentage: 100}}}
	codex := codexAccount{Quota: &codexQuota{ObservedAt: old, Primary: &codexWindow{UsedPercentage: 100}}}
	if !claude.mergeQuota(&claudeQuota{ObservedAt: now.Unix(), SevenDay: &claudeWindow{UsedPercentage: 20}}, now) ||
		!codex.mergeQuota(&codexQuota{ObservedAt: now.Unix(), Secondary: &codexWindow{UsedPercentage: 20}}, now) {
		t.Fatal("did not update independently observed windows")
	}
	if room, available := claude.headroom(now); room != 80 || !available {
		t.Fatalf("new Claude sample refreshed a stale unrelated window: %v %v", room, available)
	}
	if room, available := codex.headroom(now); room != 80 || !available {
		t.Fatalf("new Codex sample refreshed a stale unrelated window: %v %v", room, available)
	}
}
