//go:build !windows

package workbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cliconversation"
)

func handoffAccounts(now time.Time) (claudeAccountList, *mobileRecord) {
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := claudeAccountList{Accounts: []claudeAccount{
		{ID: a, Label: "A", Email: "a@example.test", Enabled: true, Status: "ready", Quota: exhaustedQuota(now, "seven_day", now.Add(30*time.Hour))},
		{ID: b, Label: "B", Email: "b@example.test", Enabled: true, Status: "ready"},
	}}
	list.Scheduling = list.Scheduling.defaults()
	record := &mobileRecord{Account: claudeSelection{ID: a, GuestID: a, Subject: claudeIdentityHash("a@example.test", "")}}
	record.AccountFailure = "rate_limit"
	return list, record
}

func exhaustedQuota(now time.Time, window string, reset time.Time) *claudeQuota {
	used := &claudeWindow{UsedPercentage: 100, ResetsAt: reset.Unix(), ObservedAt: now.Unix()}
	open := &claudeWindow{UsedPercentage: 20, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}
	if window == "five_hour" {
		return &claudeQuota{FiveHour: used, SevenDay: open}
	}
	return &claudeQuota{FiveHour: open, SevenDay: used}
}

// The first-time question comes only at a confirmed exhaustion with another
// usable account, and only while no rule answers it.
func TestClaudeHandoffQuestionAppearsAtConfirmedExhaustion(t *testing.T) {
	now := time.Now()
	list, record := handoffAccounts(now)
	state := claudeHandoffState(list, record, now)
	if state.Question == nil || state.Question.Label != "B" || state.Window != "seven_day" || state.ResetsAt != now.Add(30*time.Hour).Unix() || state.Automatic {
		t.Fatalf("question at a weekly limit: %+v", state)
	}
	for name, change := range map[string]func(*claudeAccountList, *mobileRecord){
		"overload is not a quota limit": func(_ *claudeAccountList, r *mobileRecord) { r.AccountFailure = "overloaded" },
		"unknown error":                 func(_ *claudeAccountList, r *mobileRecord) { r.AccountFailure = "unknown" },
		// A model-specific limit leaves both windows open.
		"model-specific limit": func(l *claudeAccountList, _ *mobileRecord) {
			l.Accounts[0].Quota = &claudeQuota{FiveHour: &claudeWindow{UsedPercentage: 40, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}}
		},
		"no reading yet": func(l *claudeAccountList, _ *mobileRecord) { l.Accounts[0].Quota = nil },
	} {
		l, r := handoffAccounts(now)
		change(&l, r)
		if got := claudeHandoffState(l, r, now); got.Question != nil || got.Window != "" || got.Waiting != "" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// A disabled or exhausted second account cannot continue it: the session
	// names the first reset instead of asking.
	l, r := handoffAccounts(now)
	l.Accounts[1].Quota = exhaustedQuota(now, "five_hour", now.Add(2*time.Hour))
	if got := claudeHandoffState(l, r, now); got.Question != nil || got.Waiting != "all" || got.WaitLabel != "B" || got.WaitUntil != now.Add(2*time.Hour).Unix() {
		t.Fatalf("all used up: %+v", got)
	}
	l, r = handoffAccounts(now)
	l.Accounts[1].Enabled = false
	if got := claudeHandoffState(l, r, now); got.Question != nil || got.Waiting != "all" || got.WaitLabel != "A" {
		t.Fatalf("disabled account suggested: %+v", got)
	}
	// An account with its own group stays apart from the implicit group.
	l, r = handoffAccounts(now)
	l.Accounts[0].HandoffGroup = "work"
	if got := claudeHandoffState(l, r, now); got.Question != nil {
		t.Fatalf("a grouped account was offered an ungrouped one: %+v", got)
	}
	// Standing answers and the session's own rule.
	l, r = handoffAccounts(now)
	l.Scheduling.SwitchPolicy = "wait"
	if got := claudeHandoffState(l, r, now); got.Question != nil || got.Waiting != "source" || got.WaitLabel != "A" {
		t.Fatalf("wait rule: %+v", got)
	}
	r.SwitchPolicy, r.SwitchPolicySet = "ask", true
	if got := claudeHandoffState(l, r, now); got.Question == nil {
		t.Fatalf("the session's own rule must win: %+v", got)
	}
	// A session from an older version carries a copy of the rule: a copied
	// ask follows 调度设置, a copied allow or wait is kept.
	r.SwitchPolicySet = false
	if got := claudeHandoffState(l, r, now); got.Waiting != "source" {
		t.Fatalf("a copied default overrode 调度设置: %+v", got)
	}
	l.Scheduling.SwitchPolicy, r.SwitchPolicy = "ask", "wait"
	if got := claudeHandoffState(l, r, now); got.Waiting != "source" || got.Question != nil {
		t.Fatalf("an explicit stored choice was dropped: %+v", got)
	}
	r.SwitchPolicy = "ask"
	r.HandoffWait = now.Add(time.Hour).Unix()
	if got := claudeHandoffState(l, r, now); got.Question != nil || got.Waiting != "source" {
		t.Fatalf("a chosen wait asks again: %+v", got)
	}
	l, r = handoffAccounts(now)
	l.Scheduling.SwitchPolicy = "allow"
	if got := claudeHandoffState(l, r, now); got.Question == nil || !got.Automatic {
		t.Fatalf("an old allow without the implicit group must still ask for ungrouped accounts: %+v", got)
	}
	l.Scheduling.UngroupedHandoff = true
	if got := claudeHandoffState(l, r, now); got.Question != nil || !got.Automatic {
		t.Fatalf("after a yes the question never returns: %+v", got)
	}
}

// Nothing moves before a yes; after it, a confirmed limit moves the session,
// an exhausted group waits for its first reset, and a source that resets
// first continues on itself.
func TestClaudeAutomaticContinuationRules(t *testing.T) {
	now := time.Now()
	list, record := handoffAccounts(now)
	b := list.Accounts[1].ID
	for _, policy := range []string{"ask", "wait"} {
		list.Scheduling.SwitchPolicy = policy
		if target, _, _ := claudeAutomaticStep(list, record, nil, nil, "", now); target != "" {
			t.Fatal(policy, "moved without a yes")
		}
	}
	list.Scheduling.SwitchPolicy = "allow"
	if target, _, _ := claudeAutomaticStep(list, record, nil, nil, "", now); target != "" {
		t.Fatal("ungrouped accounts continued without the implicit group")
	}
	list.Scheduling.UngroupedHandoff = true
	if target, wake, _ := claudeAutomaticStep(list, record, nil, nil, "", now); target != b || wake {
		t.Fatal("confirmed limit did not move", target)
	}
	if target, _, _ := claudeAutomaticStep(list, record, nil, map[string]bool{b: true}, "", now); target != "" {
		t.Fatal("an account in the middle of a login was chosen")
	}
	record.AccountFailure = "overloaded"
	if target, _, _ := claudeAutomaticStep(list, record, nil, nil, "", now); target != "" {
		t.Fatal("an overload moved the session")
	}
	record.AccountFailure = "rate_limit"
	list.Accounts[0].Quota = &claudeQuota{SevenDay: &claudeWindow{UsedPercentage: 60, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}}
	if target, _, _ := claudeAutomaticStep(list, record, nil, nil, "", now); target != "" {
		t.Fatal("a model-specific limit counted as the weekly window")
	}
	// Both used up: wait for the first reset (A at +1h, B at +3h).
	list.Accounts[0].Quota = exhaustedQuota(now, "five_hour", now.Add(time.Hour))
	list.Accounts[1].Quota = exhaustedQuota(now, "five_hour", now.Add(3*time.Hour))
	target, _, until := claudeAutomaticStep(list, record, nil, nil, "", now)
	if target != "" || until != now.Add(time.Hour).Unix() {
		t.Fatal("all exhausted", target, until)
	}
	record.WaitingReset = until
	later := now.Add(time.Hour + time.Minute)
	if target, wake, _ := claudeAutomaticStep(list, record, nil, nil, "", later); target != list.Accounts[0].ID || !wake {
		t.Fatal("the source reset first and did not continue on itself", target, wake)
	}
	list.Scheduling.SwitchPolicy = "ask"
	if target, _, _ := claudeAutomaticStep(list, record, nil, nil, "", later); target != "" {
		t.Fatal("the wake-up applies only under the automatic rule")
	}
}

func readGuestCalls(t *testing.T, home string) []map[string]any {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(home, "calls.jsonl"))
	calls := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var call map[string]any
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func guestCalls(t *testing.T, home, mode string) []map[string]any {
	found := []map[string]any{}
	for _, call := range readGuestCalls(t, home) {
		if call["mode"] == mode {
			found = append(found, call)
		}
	}
	return found
}

// A guest whose preparation succeeds, returns one message the source never
// took, and reports the launched target as running.
const handoffGuest = `if mode=='account-switch':
    print(json.dumps({'transcript':'/guest/frozen/'+request['switch_id']+'/history.jsonl','workspace':request.get('workspace'),'carry':[{'id':'admitted','text':'waiting behind the limit'}]}))
elif mode=='active':
    print(json.dumps({'running':True}))
else:
    print(json.dumps({'ok':True}))
`

func limitedSession(t *testing.T, s *Server, id string, record *mobileRecord) {
	t.Helper()
	now := time.Now()
	list, _ := s.loadClaudeAccounts()
	for i := range list.Accounts {
		list.Accounts[i].HandoffGroup = ""
	}
	list.Accounts[0].Quota = exhaustedQuota(now, "seven_day", now.Add(30*time.Hour))
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	record.AccountFailure, record.Active = "rate_limit", true
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeQuestionAnswers(t *testing.T) {
	answer := func(t *testing.T, answer string) (*Server, string, string, error) {
		s, id, record, home := claudeSwitchGuest(t, handoffGuest)
		limitedSession(t, s, id, record)
		err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: answer, RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true})
		return s, id, home, err
	}
	t.Run("wait once", func(t *testing.T) {
		s, id, home, err := answer(t, "wait_once")
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := s.mobileCLI.record(s.profile, id)
		list, _ := s.loadClaudeAccounts()
		if stored.Account.ID != list.Accounts[0].ID || stored.HandoffWait != list.Accounts[0].Quota.SevenDay.ResetsAt || list.Scheduling.SwitchPolicy != "ask" || list.Scheduling.SwitchAnswered {
			t.Fatalf("wait once: %+v %+v", stored.HandoffWait, list.Scheduling)
		}
		if len(guestCalls(t, home, "account-switch")) != 0 {
			t.Fatal("waiting moved the history")
		}
		if state := claudeHandoffState(list, stored, time.Now()); state.Question != nil || state.Waiting != "source" {
			t.Fatalf("the question returned during the chosen wait: %+v", state)
		}
	})
	t.Run("wait always", func(t *testing.T) {
		s, _, _, err := answer(t, "wait_always")
		list, _ := s.loadClaudeAccounts()
		if err != nil || list.Scheduling.SwitchPolicy != "wait" || !list.Scheduling.SwitchAnswered || list.Scheduling.UngroupedHandoff {
			t.Fatal(err, list.Scheduling)
		}
	})
	t.Run("switch once", func(t *testing.T) {
		s, id, home, err := answer(t, "once")
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := s.mobileCLI.record(s.profile, id)
		list, _ := s.loadClaudeAccounts()
		if stored.Account.ID != list.Accounts[1].ID || list.Scheduling.SwitchPolicy != "ask" || list.Scheduling.UngroupedHandoff {
			t.Fatalf("once: %s %+v", stored.Account.ID, list.Scheduling)
		}
		calls := guestCalls(t, home, "account-switch")
		if len(calls) != 1 || calls[0]["force"] != true {
			t.Fatal("a limited source is stopped without a second question:", calls)
		}
		sw := stored.Switch
		if sw.Reason != "seven_day" || sw.SourceLabel != "A" || sw.Automatic || !sw.Continue || len(sw.Carry) != 1 || sw.Carry[0].ID == "admitted" || sw.Carry[0].Text != "waiting behind the limit" {
			t.Fatalf("handoff record: %+v", sw)
		}
	})
	t.Run("always", func(t *testing.T) {
		s, id, _, err := answer(t, "always")
		stored, _ := s.mobileCLI.record(s.profile, id)
		list, _ := s.loadClaudeAccounts()
		if err != nil || stored.Account.ID != list.Accounts[1].ID || list.Scheduling.SwitchPolicy != "allow" || !list.Scheduling.SwitchAnswered || !list.Scheduling.UngroupedHandoff {
			t.Fatal(err, list.Scheduling)
		}
	})
	t.Run("answer from the other device", func(t *testing.T) {
		s, id, home, err := answer(t, "wait_once")
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := s.mobileCLI.record(s.profile, id)
		late := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "once", RequestID: uuid.NewString(), SourceID: stored.CLIID, Confirm: true})
		if late != errClaudeSwitchAnswered {
			t.Fatal("a second answer was applied:", late)
		}
		again, _ := s.mobileCLI.record(s.profile, id)
		if again.Account.ID != stored.Account.ID || len(guestCalls(t, home, "account-switch")) != 0 {
			t.Fatal("the late answer moved the session")
		}
		s2, id2, home2, err := answer(t, "once")
		if err != nil {
			t.Fatal(err)
		}
		moved, _ := s2.mobileCLI.record(s2.profile, id2)
		if late := s2.answerClaudeQuestion(claudeSwitchContext(s2), id2, claudeSwitchInput{Answer: "wait_always", RequestID: uuid.NewString(), SourceID: moved.CLIID, Confirm: true}); late != errClaudeSwitchAnswered {
			t.Fatal("a wait after the switch was applied:", late)
		}
		if list, _ := s2.loadClaudeAccounts(); list.Scheduling.SwitchPolicy != "ask" || len(guestCalls(t, home2, "account-switch")) != 1 {
			t.Fatal("the late answer changed the rule", list.Scheduling)
		}
	})
}

// Picking a target switches: any group, a disabled or used-up account, with
// one confirm only when the source is really working.
func TestClaudeManualSwitchHasOneConfirm(t *testing.T) {
	setup := func(t *testing.T, change func(*claudeAccountList)) (*Server, string, *mobileRecord, string) {
		s, id, record, home := claudeSwitchGuest(t, handoffGuest)
		list, _ := s.loadClaudeAccounts()
		list.Accounts[1].HandoffGroup = "work"
		change(&list)
		if err := s.saveClaudeAccounts(list); err != nil {
			t.Fatal(err)
		}
		return s, id, record, home
	}
	b := strings.Repeat("b", 24)
	for name, change := range map[string]func(*claudeAccountList){
		"another group": func(*claudeAccountList) {},
		"disabled":      func(l *claudeAccountList) { l.Accounts[1].Enabled = false },
		"used up": func(l *claudeAccountList) {
			l.Accounts[1].Quota = exhaustedQuota(time.Now(), "five_hour", time.Now().Add(time.Hour))
		},
		"unknown quota":       func(l *claudeAccountList) { l.Accounts[1].Quota = nil },
		"source ungrouped":    func(l *claudeAccountList) { l.Accounts[0].HandoffGroup = "" },
		"global wait":         func(l *claudeAccountList) { l.Scheduling.SwitchPolicy = "wait" },
		"never answered rule": func(l *claudeAccountList) { l.Scheduling.SwitchPolicy = "ask" },
	} {
		t.Run(name, func(t *testing.T) {
			s, id, record, home := setup(t, change)
			if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true}, false); err != nil {
				t.Fatal(err)
			}
			stored, _ := s.mobileCLI.record(s.profile, id)
			if stored.Account.ID != b || len(guestCalls(t, home, "account-switch")) != 1 {
				t.Fatal("not switched", stored.Account.ID)
			}
		})
	}
	t.Run("source working", func(t *testing.T) {
		s, id, record, home := setup(t, func(*claudeAccountList) {})
		record.Active = true
		record.Turns = []mobileTurn{{ID: "t", Tools: []cliconversation.ToolActivity{{ID: "x", Name: "Agent", Detail: "Review the patch", Status: "called"}, {ID: "y", Name: "Read", Status: "done"}}}}
		if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
			t.Fatal(err)
		}
		err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true}, false)
		if err == nil || !strings.Contains(err.Error(), "停止当前任务") || len(readGuestCalls(t, home)) != 0 {
			t.Fatal("a working source needs the one confirm:", err)
		}
		if stored, _ := s.mobileCLI.record(s.profile, id); stored.Switch != nil {
			t.Fatal("the refusal left a handoff behind")
		}
		if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true, Force: true}, false); err != nil {
			t.Fatal(err)
		}
		calls := guestCalls(t, home, "account-switch")
		stored, _ := s.mobileCLI.record(s.profile, id)
		if len(calls) != 1 || calls[0]["force"] != true || !stored.Switch.Continue || !strings.Contains(stored.Switch.Note, "Review the patch (Agent)") || strings.Contains(stored.Switch.Note, "Read") {
			t.Fatalf("confirmed stop: %v %+v", calls, stored.Switch)
		}
	})
	t.Run("target not logged in", func(t *testing.T) {
		s, id, record, home := setup(t, func(l *claudeAccountList) { l.Accounts[1].Status = "signed_out" })
		err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true, Force: true}, false)
		if err == nil || !strings.Contains(err.Error(), "B 尚未登录：") || len(readGuestCalls(t, home)) != 0 {
			t.Fatal(err)
		}
	})
	t.Run("from the phone", func(t *testing.T) {
		s, id, record, home := setup(t, func(l *claudeAccountList) { l.Accounts[1].Enabled = false })
		inspect := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/account", s.origin, `{"action":"inspect"}`, true)
		var view struct {
			API     int                  `json:"handoff_api"`
			Targets []claudeSwitchTarget `json:"targets"`
			Old     []claudeSwitchOption `json:"candidates"`
		}
		if err := json.Unmarshal(inspect.Body.Bytes(), &view); err != nil || view.API != 2 || len(view.Targets) != 1 || view.Targets[0].Enabled || len(view.Old) != 1 {
			t.Fatal(inspect.Code, inspect.Body.String())
		}
		body := `{"action":"switch","target":"` + b + `","request_id":"` + uuid.NewString() + `","source_id":"` + record.CLIID + `","confirm":true}`
		response := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/account", s.origin, body, true)
		stored, _ := s.mobileCLI.record(s.profile, id)
		if stored.Account.ID != b || len(guestCalls(t, home, "account-switch")) != 1 {
			t.Fatal(response.Code, response.Body.String())
		}
	})
}

// Carried messages reach the target once, under the id each got when the
// history was frozen, whatever the monitor retries.
func TestClaudeCarriedMessageIsDeliveredOnce(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, handoffGuest)
	limitedSession(t, s, id, record)
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "once", RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	carried := stored.Switch.Carry[0].ID
	view := stored.snapshot()
	if view.AccountSwitch.carry {
		t.Fatal("delivered before the target took over")
	}
	stored.Switch.Phase, stored.Connection = "completed", uuid.NewString()
	if err := writePrivate(mobileRecordPath(s.profile, id), stored); err != nil {
		t.Fatal(err)
	}
	ready := true
	for range 3 {
		latest, _ := s.mobileCLI.record(s.profile, id)
		view := latest.snapshot()
		view.CanSend, view.Active = &ready, false
		s.deliverClaudeCarry(claudeSwitchContext(s), id, view)
	}
	sent := []map[string]any{}
	for _, call := range guestCalls(t, home, "rpc") {
		if call["request"] == carried {
			sent = append(sent, call)
		}
	}
	if len(sent) != 1 || sent[0]["text"] != "waiting behind the limit" {
		t.Fatal("carried message deliveries:", sent)
	}
	if latest, _ := s.mobileCLI.record(s.profile, id); !latest.Switch.Carry[0].Delivered || latest.snapshot().AccountSwitch.carry {
		t.Fatal("delivery not recorded")
	}
}

func TestClaudeUndoReturnsToTheExhaustedAccountAndWaits(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, handoffGuest)
	limitedSession(t, s, id, record)
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "once", RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	moved, _ := s.mobileCLI.record(s.profile, id)
	moved.Switch.Phase, moved.Connection, moved.GuestConfig, moved.Active = "completed", uuid.NewString(), "/guest/accounts/b/config", false
	if err := writePrivate(mobileRecordPath(s.profile, id), moved); err != nil {
		t.Fatal(err)
	}
	list, _ := s.loadClaudeAccounts()
	if back := claudeUndoTarget(list, moved); back == nil || back.ID != list.Accounts[0].ID {
		t.Fatal("undo unavailable after a limit handoff")
	}
	if err := s.undoClaudeSwitch(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: moved.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	back, _ := s.mobileCLI.record(s.profile, id)
	if back.Account.ID != list.Accounts[0].ID || back.HandoffWait != list.Accounts[0].Quota.SevenDay.ResetsAt || back.Switch.Continue || len(guestCalls(t, home, "account-switch")) != 2 {
		t.Fatalf("undo: %+v", back.Switch)
	}
	// Back on A it hits the limit again: it waits instead of asking or moving.
	back.AccountFailure, back.Switch.Phase = "rate_limit", "completed"
	if state := claudeHandoffState(list, back, time.Now()); state.Question != nil || state.Waiting != "source" {
		t.Fatalf("ping-pong after undo: %+v", state)
	}
	if err := s.undoClaudeSwitch(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: back.CLIID, Confirm: true}); err == nil {
		t.Fatal("a manual return was undone again")
	}
}

func TestClaudeSchedulingKeepsStandingAnswersAndPreferredAccount(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := claudeAccountList{Accounts: []claudeAccount{{ID: a, Label: "A", Enabled: true, Status: "ready", Email: "a@example.test"}, {ID: b, Label: "B", Status: "ready", Email: "b@example.test"}}}
	list.Scheduling = claudeScheduling{SwitchPolicy: "allow", UngroupedHandoff: true}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := s.loadClaudeAccounts(); !loaded.Scheduling.answered() {
		t.Fatal("a stored explicit choice must count as answered")
	}
	if response := request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"prefer","id":"`+b+`"}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	// An older client sends only mode and switch_policy.
	if response := request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"balanced","switch_policy":"allow"}}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	loaded, _ := s.loadClaudeAccounts()
	if loaded.Scheduling.Preferred != b || !loaded.Accounts[1].Enabled || !loaded.Scheduling.UngroupedHandoff || !loaded.Scheduling.SwitchAnswered {
		t.Fatalf("scheduling lost fields: %+v", loaded.Scheduling)
	}
	if id := pickClaudeAccount(loaded, map[string]int{b: 5}, nil, "", "", time.Now()); id != b {
		t.Fatal("preferred account not used for new sessions", id)
	}
	loaded.Accounts[1].Quota = exhaustedQuota(time.Now(), "five_hour", time.Now().Add(time.Hour))
	if id := pickClaudeAccount(loaded, nil, nil, "", "", time.Now()); id != a {
		t.Fatal("a used-up preferred account was used", id)
	}
	if response := request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"balanced","switch_policy":"ask","ungrouped_handoff":false,"preferred":""}}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if loaded, _ = s.loadClaudeAccounts(); loaded.Scheduling.Preferred != "" || loaded.Scheduling.UngroupedHandoff || loaded.Scheduling.SwitchPolicy != "ask" {
		t.Fatalf("%+v", loaded.Scheduling)
	}
}
