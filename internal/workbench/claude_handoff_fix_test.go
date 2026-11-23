//go:build !windows

package workbench

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cliconversation"
)

// --- Finding 1: consent stays where it was given ---

func ungroupedLimited(t *testing.T, s *Server, id string, record *mobileRecord, scheduling claudeScheduling) {
	t.Helper()
	now := time.Now()
	list, _ := s.loadClaudeAccounts()
	for i := range list.Accounts {
		list.Accounts[i].HandoffGroup = ""
	}
	list.Accounts[0].Quota = exhaustedQuota(now, "seven_day", now.Add(30*time.Hour))
	list.Scheduling = scheduling
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	record.AccountFailure = "rate_limit"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
}

// Choosing automatic continuation in one session's menu is that session's yes
// to accounts without a group, never the others'.
func TestClaudeSessionConsentStaysInItsSession(t *testing.T) {
	s, id, record, _ := claudeSwitchGuest(t, handoffGuest)
	// 调度设置 says allow, limited to explicit groups.
	ungroupedLimited(t, s, id, record, claudeScheduling{Mode: "balanced", SwitchPolicy: "allow", SwitchAnswered: true})
	if response := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/account", s.origin, `{"action":"policy","policy":"allow"}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	list, _ := s.loadClaudeAccounts()
	if list.Scheduling.UngroupedHandoff {
		t.Fatal("one session's menu wrote the global consent")
	}
	chosen, _ := s.mobileCLI.record(s.profile, id)
	if !chosen.UngroupedHandoff || chosen.implicitConsent(list) != true {
		t.Fatal("the session's own yes was not kept on it")
	}
	b := list.Accounts[1].ID
	if target, _, _ := claudeAutomaticStep(list, chosen, nil, nil, "", time.Now()); target != b {
		t.Fatal("the session that said yes does not continue", target)
	}
	// Another session following 调度设置 (allow, groups only) stays put.
	other := *chosen
	other.SwitchPolicy, other.SwitchPolicySet, other.UngroupedHandoff = "", false, false
	if target, _, _ := claudeAutomaticStep(list, &other, nil, nil, "", time.Now()); target != "" {
		t.Fatal("another session moved on a yes it never got", target)
	}
	if state := claudeHandoffState(list, &other, time.Now()); state.Question == nil || state.Question.ID != b {
		t.Fatalf("the other session does not ask: %+v", state)
	}
	// A copied allow from an older version keeps its explicit-groups meaning,
	// whatever 调度设置 says.
	list.Scheduling.UngroupedHandoff = true
	legacy := other
	legacy.SwitchPolicy = "allow"
	if target, _, _ := claudeAutomaticStep(list, &legacy, nil, nil, "", time.Now()); target != "" {
		t.Fatal("a copied allow moved between ungrouped accounts", target)
	}
	if state := claudeHandoffState(list, &legacy, time.Now()); state.Question == nil {
		t.Fatalf("a copied allow does not ask between ungrouped accounts: %+v", state)
	}
	if target, _, _ := claudeAutomaticStep(list, &other, nil, nil, "", time.Now()); target != b {
		t.Fatal("a session following 调度设置 ignores its consent", target)
	}
}

// A refused session change saves nothing, the consent included.
func TestClaudeRefusedSessionRuleSavesNoConsent(t *testing.T) {
	s, id, record, _ := claudeSwitchGuest(t, handoffGuest)
	record.Switch = &claudeAccountSwitch{ID: uuid.NewString(), Phase: "preparing"}
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	if response := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/account", s.origin, `{"action":"policy","policy":"allow"}`, true); response.Code == 200 {
		t.Fatal("a policy change during a handoff was accepted")
	}
	list, _ := s.loadClaudeAccounts()
	stored, _ := s.mobileCLI.record(s.profile, id)
	if list.Scheduling.UngroupedHandoff || stored.UngroupedHandoff || stored.SwitchPolicySet {
		t.Fatal("a refused change saved consent")
	}
}

// 以后额度用完都自动接续 on a question between two accounts of one explicit
// group sets the rule, not the consent for accounts without a group.
func TestClaudeAlwaysWithinAGroupKeepsUngroupedApart(t *testing.T) {
	s, id, record, _ := claudeSwitchGuest(t, handoffGuest)
	now := time.Now()
	list, _ := s.loadClaudeAccounts() // both in group "personal"
	list.Accounts[0].Quota = exhaustedQuota(now, "seven_day", now.Add(30*time.Hour))
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	record.AccountFailure = "rate_limit"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "always", RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.loadClaudeAccounts()
	if list.Scheduling.SwitchPolicy != "allow" || list.Scheduling.UngroupedHandoff {
		t.Fatalf("always within a group: %+v", list.Scheduling)
	}
}

// The yes covers ungrouped accounts only when the account it moves to has
// no group either.
func TestClaudeAlwaysTowardsAGroupKeepsUngroupedApart(t *testing.T) {
	s, id, record, _ := claudeSwitchGuest(t, handoffGuest)
	now := time.Now()
	list, _ := s.loadClaudeAccounts()
	c := strings.Repeat("c", 24)
	list.Accounts[0].HandoffGroup, list.Accounts[1].HandoffGroup = "", "work"
	list.Accounts = append(list.Accounts, claudeAccount{ID: c, Label: "C", Email: "c@example.test", Enabled: true, Status: "ready"})
	list.Accounts[0].Quota = exhaustedQuota(now, "seven_day", now.Add(30*time.Hour))
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	record.AccountFailure = "rate_limit"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	if state := claudeHandoffState(list, record, now); state.Question == nil || state.Question.ID != c {
		t.Fatalf("question: %+v", state)
	}
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "always", Target: list.Accounts[1].ID, RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.loadClaudeAccounts(); list.Scheduling.SwitchPolicy != "allow" || list.Scheduling.UngroupedHandoff {
		t.Fatalf("always towards a grouped account: %+v", list.Scheduling)
	}
}

// 切换并继续 is the yes of that one session.
func TestClaudeOnceIsTheSessionsYes(t *testing.T) {
	s, id, record, _ := claudeSwitchGuest(t, handoffGuest)
	ungroupedLimited(t, s, id, record, claudeScheduling{Mode: "balanced", SwitchPolicy: "ask"})
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "once", RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.loadClaudeAccounts()
	stored, _ := s.mobileCLI.record(s.profile, id)
	if list.Scheduling.UngroupedHandoff || !stored.UngroupedHandoff || list.Scheduling.SwitchPolicy != "ask" {
		t.Fatalf("once: global %+v, session %v", list.Scheduling, stored.UngroupedHandoff)
	}
}

// The rule alone never implies the consent: an older client that sends only
// mode and switch_policy leaves it as it was.
func TestClaudeRuleAloneIsNoConsent(t *testing.T) {
	s := testServer(t)
	if response := request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"balanced","switch_policy":"allow"}}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if list, _ := s.loadClaudeAccounts(); list.Scheduling.UngroupedHandoff || list.Scheduling.SwitchPolicy != "allow" {
		t.Fatalf("%+v", list.Scheduling)
	}
	if response := request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"balanced","switch_policy":"allow","ungrouped_handoff":true}}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if list, _ := s.loadClaudeAccounts(); !list.Scheduling.UngroupedHandoff {
		t.Fatal("the explicit control was ignored")
	}
}

// 新会话使用此账号 does not make the account a continuation target.
func TestClaudePreferredAccountIsNotAContinuationTarget(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := claudeAccountList{Accounts: []claudeAccount{{ID: a, Label: "A", Enabled: true, Status: "ready", Email: "a@example.test"}, {ID: b, Label: "B", Status: "ready", Email: "b@example.test"}}}
	list.Scheduling = claudeScheduling{SwitchPolicy: "allow", UngroupedHandoff: true}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	if response := request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"prefer","id":"`+b+`"}`, true); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	loaded, _ := s.loadClaudeAccounts()
	if loaded.Accounts[1].Enabled || loaded.Scheduling.Preferred != b {
		t.Fatal("prefer turned on 参与分配")
	}
	if id := pickClaudeAccount(loaded, nil, nil, "", "", time.Now()); id != b {
		t.Fatal("a preferred account outside scheduling is not used for new sessions", id)
	}
	now := time.Now()
	loaded.Accounts[0].Quota = exhaustedQuota(now, "five_hour", now.Add(time.Hour))
	record := &mobileRecord{Account: claudeSelection{ID: a, GuestID: a, Subject: claudeIdentityHash("a@example.test", "")}}
	record.AccountFailure = "rate_limit"
	if got := automaticSwitchTargets(loaded, record, true, now); len(got) != 0 {
		t.Fatal("the preferred account became a continuation target", got)
	}
}

// --- Finding 2: nothing is stopped unless confirmed or at a confirmed limit ---

func TestClaudeBackgroundTasksNeedTheConfirm(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, handoffGuest)
	b := strings.Repeat("b", 24)
	record.Tasks = &mobileTasks{Count: 1, Running: []cliconversation.ToolActivity{{ID: "bg", Name: "Bash", Detail: "Long build", Status: "called"}}}
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	var view struct {
		Busy  bool `json:"source_busy"`
		Tasks int  `json:"source_tasks"`
	}
	response := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/account", s.origin, `{"action":"inspect"}`, true)
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || !view.Busy || view.Tasks != 1 {
		t.Fatal("a running background task does not count as work:", response.Body.String())
	}
	err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true}, false)
	if err == nil || !strings.Contains(err.Error(), "停止当前任务") || !strings.Contains(err.Error(), "后台任务") || len(guestCalls(t, home, "account-switch")) != 0 {
		t.Fatal("a running background task was stopped without the confirm:", err)
	}
	if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true, Force: true}, false); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	calls := guestCalls(t, home, "account-switch")
	if len(calls) != 1 || calls[0]["force"] != true || !strings.Contains(stored.Switch.Note, "Long build (Bash), running in the background") || !strings.Contains(stored.Switch.Note, "may still be running") {
		t.Fatalf("confirmed stop: %v %q", calls, stored.Switch.Note)
	}
}

// An overload or a server error is not a confirmed quota limit: an idle
// source moves without stopping anything, and a working one asks first.
func TestClaudeOtherFailuresDoNotForce(t *testing.T) {
	for _, failure := range []string{"overloaded", "server_error", "rate_limit"} {
		t.Run(failure, func(t *testing.T) {
			s, id, record, home := claudeSwitchGuest(t, handoffGuest)
			b := strings.Repeat("b", 24)
			record.AccountFailure = failure // rate_limit without a used-up reading
			if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
				t.Fatal(err)
			}
			if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true}, false); err != nil {
				t.Fatal(err)
			}
			stored, _ := s.mobileCLI.record(s.profile, id)
			calls := guestCalls(t, home, "account-switch")
			if len(calls) != 1 || calls[0]["force"] == true || stored.Switch.Note != "" || stored.Switch.Force {
				t.Fatalf("%s forced the switch: %v %+v", failure, calls, stored.Switch)
			}
		})
	}
}

// --- Finding 4: undo keeps its wait and never continues on the used-up account ---

const flakyGuest = `counter=pathlib.Path.home()/'attempts'
if mode=='account-switch' and not counter.exists():
    counter.write_text('1')
    print('not json')
elif mode=='account-switch':
    print(json.dumps({'transcript':'/guest/frozen/'+request['switch_id']+'/history.jsonl','workspace':request.get('workspace'),'carry':[{'id':'b-unsent','text':'typed on B'}]}))
elif mode=='active':
    print(json.dumps({'running':True}))
else:
    print(json.dumps({'ok':True}))
`

func TestClaudeUndoKeepsItsWaitAcrossARetry(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, flakyGuest)
	if err := os.WriteFile(home+"/attempts", []byte("1"), 0600); err != nil { // the first handoff succeeds
		t.Fatal(err)
	}
	limitedSession(t, s, id, record)
	if err := s.answerClaudeQuestion(claudeSwitchContext(s), id, claudeSwitchInput{Answer: "once", RequestID: uuid.NewString(), SourceID: record.CLIID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	moved, _ := s.mobileCLI.record(s.profile, id)
	moved.Switch.Phase, moved.Connection, moved.GuestConfig, moved.Active = "completed", uuid.NewString(), "/guest/accounts/b/config", true
	for i := range moved.Switch.Carry {
		moved.Switch.Carry[i].Delivered = true
	}
	if err := writePrivate(mobileRecordPath(s.profile, id), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(home + "/attempts"); err != nil { // the undo's first preparation fails
		t.Fatal(err)
	}
	list, _ := s.loadClaudeAccounts()
	reset := list.Accounts[0].Quota.SevenDay.ResetsAt
	undo := claudeSwitchInput{RequestID: uuid.NewString(), SourceID: moved.CLIID, Confirm: true, Force: true} // B was working
	if err := s.undoClaudeSwitch(claudeSwitchContext(s), id, undo); err == nil {
		t.Fatal("the flaky preparation did not fail")
	}
	if pending, _ := s.mobileCLI.record(s.profile, id); pending.Switch == nil || pending.Switch.Phase != "preparing" {
		t.Fatal("the failed undo was not kept for a retry")
	}
	// 重试账号交接 repeats the same request.
	if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: undo.RequestID, SourceID: moved.CLIID, Confirm: true}, false); err != nil {
		t.Fatal(err)
	}
	back, _ := s.mobileCLI.record(s.profile, id)
	if back.Account.ID != list.Accounts[0].ID || back.HandoffWait != reset || back.Switch.Continue {
		t.Fatalf("retried undo: wait %d (want %d), continue %v", back.HandoffWait, reset, back.Switch.Continue)
	}
	// What B never took waits for A's reset instead of hitting A's limit.
	back.Switch.Phase, back.Connection = "completed", uuid.NewString()
	if err := writePrivate(mobileRecordPath(s.profile, id), back); err != nil {
		t.Fatal(err)
	}
	ready := true
	view := back.snapshot()
	view.CanSend, view.Active = &ready, false
	s.deliverClaudeCarry(claudeSwitchContext(s), id, view)
	if len(guestCalls(t, home, "rpc")) != 0 {
		t.Fatal("B's unsent message was sent to A before its reset")
	}
	back.HandoffWait = time.Now().Add(-time.Second).Unix()
	if err := writePrivate(mobileRecordPath(s.profile, id), back); err != nil {
		t.Fatal(err)
	}
	view = back.snapshot()
	view.CanSend, view.Active = &ready, false
	s.deliverClaudeCarry(claudeSwitchContext(s), id, view)
	if calls := guestCalls(t, home, "rpc"); len(calls) != 1 || calls[0]["text"] != "typed on B" {
		t.Fatal("after the reset the message was not delivered once:", calls)
	}
}

// --- Finding 5: a refused automatic continuation is not repeated every check ---

// A refusal that usually passes by itself (the source CLI still exiting) is
// tried again on the next checks, a few times, and then waits like the others:
// a slow guest does not hold the continuation back for the whole backoff.
func TestClaudeTransientRefusalIsTriedAgainOnTheNextChecks(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, strings.Replace(refusingGuest, "switch:history", "switch:exit", 1))
	ungroupedLimited(t, s, id, record, claudeScheduling{Mode: "balanced", SwitchPolicy: "allow", SwitchAnswered: true, UngroupedHandoff: true})
	clock := time.Now()
	s.claudeAccounts.now = func() time.Time { return clock }
	monitor := &claudeMonitor{lastUsageCheck: time.Now(), started: true}
	tick := func() {
		s.maybeSwitchClaudeAccount(claudeSwitchContext(s), id, monitor)
		clock = clock.Add(10 * time.Second)
	}
	for range 3 {
		tick()
	}
	if n := len(guestCalls(t, home, "account-switch")); n != 3 {
		t.Fatalf("a refusal that passes by itself was tried %d times in three checks", n)
	}
	for range 15 { // it never passes: the attempts are bounded
		tick()
	}
	if n := len(guestCalls(t, home, "account-switch")); n != claudeTransientAttempts {
		t.Fatalf("tried %d times, want %d", n, claudeTransientAttempts)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	list, _ := s.loadClaudeAccounts()
	if state := claudeHandoffState(list, stored, clock); state.Waiting != "refused" || !strings.Contains(state.Refusal, "还没有退出") {
		t.Fatalf("the refusal is not shown: %+v", state)
	}
	clock = clock.Add(16 * time.Minute)
	tick()
	if n := len(guestCalls(t, home, "account-switch")); n != claudeTransientAttempts+1 {
		t.Fatal("the backoff did not allow a new attempt:", n)
	}
}

const refusingGuest = `if mode=='account-switch':
    print(json.dumps({'error':'switch:history'}))
elif mode=='view':
    print(json.dumps({'cli_id':request['cli_id'],'config':request['config'],'connection':request['connection'],'workspace':request.get('project',''),'has_history':True,'running':True,'account_failure':'rate_limit'}))
else:
    print(json.dumps({'ok':True}))
`

func TestClaudeRefusedAutomaticContinuationWaitsForAChange(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, refusingGuest)
	ungroupedLimited(t, s, id, record, claudeScheduling{Mode: "balanced", SwitchPolicy: "allow", SwitchAnswered: true, UngroupedHandoff: true})
	clock := time.Now()
	s.claudeAccounts.now = func() time.Time { return clock }
	monitor := &claudeMonitor{lastUsageCheck: time.Now(), started: true}
	tick := func() {
		s.maybeSwitchClaudeAccount(claudeSwitchContext(s), id, monitor)
		clock = clock.Add(10 * time.Second)
	}
	for range 7 { // a minute of checks
		tick()
	}
	if n := len(guestCalls(t, home, "account-switch")); n != 1 {
		t.Fatalf("a persistent refusal was tried %d times in a minute", n)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	list, _ := s.loadClaudeAccounts()
	if state := claudeHandoffState(list, stored, clock); state.Waiting != "refused" || !strings.Contains(state.Refusal, "找不到这段会话的历史文件") {
		t.Fatalf("the refusal is not shown: %+v", state)
	}
	// Something it depends on changes: one new attempt.
	list.Accounts[1].Label = "B renamed" // not a dependency
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	tick()
	if n := len(guestCalls(t, home, "account-switch")); n != 1 {
		t.Fatal("a label change retried the refused attempt")
	}
	list.Accounts = append(list.Accounts, claudeAccount{ID: strings.Repeat("c", 24), Label: "C", Email: "c@example.test", Enabled: true, Status: "ready"})
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	tick()
	if n := len(guestCalls(t, home, "account-switch")); n != 2 {
		t.Fatal("a new account did not bring a new attempt:", n)
	}
	// Nothing changes: the slow backoff allows one more after 15 minutes.
	clock = clock.Add(16 * time.Minute)
	tick()
	tick()
	if n := len(guestCalls(t, home, "account-switch")); n != 3 {
		t.Fatal("the backoff did not allow exactly one retry:", n)
	}
}

// --- Finding 11 and the carry guard ---

func TestClaudeGuestErrorsBecomeSentences(t *testing.T) {
	if got := claudeSwitchRefusal("switch:outside:/work/other").Error(); !strings.Contains(got, "/work/other") || !strings.Contains(got, "退出 Claude Code") || strings.Contains(got, "确认") {
		t.Fatal("outside:", got)
	}
	for _, raw := range []string{"[Errno 28] No space left on device", "Expecting value: line 1 column 1 (char 0)"} {
		if got := claudeSwitchRefusal(raw).Error(); got != claudeSwitchGeneric {
			t.Fatal("a raw guest error reached the person:", got)
		}
	}
	if got := claudeSwitchRefusal("历史项目与当前项目不一致").Error(); got != "历史项目与当前项目不一致" {
		t.Fatal("an older helper's sentence was replaced", got)
	}
}

func TestClaudeCarryMarkSurvivesAMissingSwitch(t *testing.T) {
	record := &mobileRecord{}
	record.markCarried("anything") // the switch was replaced meanwhile
	record.Switch = &claudeAccountSwitch{Carry: []claudeCarried{{ID: "x"}, {ID: "y"}}}
	record.markCarried("y")
	if record.Switch.Carry[0].Delivered || !record.Switch.Carry[1].Delivered {
		t.Fatal(record.Switch.Carry)
	}
}

// The refusal travels the whole way as a sentence: the switch is dropped and
// no raw guest text reaches the session.
func TestClaudeGuestRefusalReachesThePersonAsASentence(t *testing.T) {
	for _, scenario := range []struct{ reply, want string }{
		{"{'error':'switch:outside:/work/other'}", "/work/other"},
		{"{'error':\"Traceback: KeyError 'cwd'\"}", claudeSwitchGeneric},
	} {
		s, id, record, _ := claudeSwitchGuest(t, "if mode=='account-switch':\n    print(json.dumps("+scenario.reply+"))\nelse:\n    print(json.dumps({'ok':True}))\n")
		err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: strings.Repeat("b", 24), Confirm: true}, false)
		stored, _ := s.mobileCLI.record(s.profile, id)
		if err == nil || !strings.Contains(err.Error(), scenario.want) || strings.Contains(err.Error(), "Traceback") || stored.Switch != nil || stored.Account.ID != record.Account.ID {
			t.Fatalf("%s: %v, switch %+v", scenario.reply, err, stored.Switch)
		}
	}
}

// Messages an undone handoff still holds go on with the next handoff, and
// stay when that one is refused.
func TestClaudeUndeliveredMessagesSurviveTheNextSwitch(t *testing.T) {
	held := claudeCarried{ID: "held", Text: "typed on B"}
	start := func(t *testing.T, script string) (*Server, string, *mobileRecord, string) {
		s, id, record, home := claudeSwitchGuest(t, script)
		record.Switch = &claudeAccountSwitch{ID: uuid.NewString(), Phase: "completed", TargetID: record.CLIID, Carry: []claudeCarried{{ID: "sent", Text: "old", Delivered: true}, held}}
		record.HandoffWait = time.Now().Add(time.Hour).Unix()
		if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
			t.Fatal(err)
		}
		return s, id, record, home
	}
	s, id, record, _ := start(t, refusingGuest)
	if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: strings.Repeat("b", 24), Confirm: true}, false); err == nil {
		t.Fatal("the refusing guest did not refuse")
	}
	if stored, _ := s.mobileCLI.record(s.profile, id); stored.Switch == nil || len(stored.Switch.Carry) != 2 || stored.Switch.Carry[1] != held {
		t.Fatalf("a refused switch lost the held message: %+v", stored.Switch)
	}
	s, id, record, _ = start(t, handoffGuest)
	if err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: strings.Repeat("b", 24), Confirm: true}, false); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	if carry := stored.Switch.Carry; len(carry) != 2 || carry[0] != held || carry[1].Text != "waiting behind the limit" || stored.Switch.Earlier != nil || stored.HandoffWait != 0 {
		t.Fatalf("the next switch: %+v, wait %d", stored.Switch, stored.HandoffWait)
	}
}

// A refusal after the channel moved to another conversation (a /clear or
// /cd in the terminal) leaves no handoff behind, though the extra cancel is
// refused: the next choice is not blocked by it.
func TestClaudeRefusalAfterTheChannelMovedLeavesNoHandoff(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, "if mode=='account-switch':\n    print(json.dumps({'error':'switch:process'}))\nelif mode=='account-switch-cancel':\n    print(json.dumps({'error':'switch:cancel'}))\nelse:\n    print(json.dumps({'ok':True}))\n")
	err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: strings.Repeat("b", 24), Confirm: true}, false)
	stored, _ := s.mobileCLI.record(s.profile, id)
	if err == nil || err.Error() != claudeSwitchRefusals["switch:process"] || stored.Switch != nil || len(guestCalls(t, home, "account-switch-cancel")) != 1 {
		t.Fatalf("%v, switch %+v", err, stored.Switch)
	}
}

var _ = context.Background
