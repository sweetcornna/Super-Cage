package workbench

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type claudeSwitchInput struct {
	Action    string `json:"action"`
	Policy    string `json:"policy"`
	Target    string `json:"target"`
	RequestID string `json:"request_id"`
	SourceID  string `json:"source_id"`
	Confirm   bool   `json:"confirm"`
	Continue  bool   `json:"continue"`
	// Force: the person confirmed stopping the source's current task.
	Force bool `json:"force"`
	// Answer to the first-time question: once, always, wait_once, wait_always.
	Answer string `json:"answer"`
	// wake: the wait for a reset ended and the source continues on itself.
	wake bool
}

type claudeSwitchOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// claudeSwitchTarget is an account a person can move a session to by hand,
// with what the person needs to see beside its name.
type claudeSwitchTarget struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Status    string  `json:"status"`
	Enabled   bool    `json:"enabled"`
	Exhausted string  `json:"exhausted,omitempty"`
	ResetsAt  int64   `json:"resets_at,omitempty"`
	Remaining float64 `json:"remaining"`
}

// claudeHandoffView is what a session shows about its account's quota.
type claudeHandoffView struct {
	// Window and ResetsAt: the source's confirmed exhaustion.
	Window   string `json:"window,omitempty"`
	ResetsAt int64  `json:"resets_at,omitempty"`
	// Question: the first-time (or every-time) question, with its suggestion.
	Question *claudeSwitchOption `json:"question,omitempty"`
	// Waiting is "source" while the session waits for its own account and
	// "all" when no account that may continue it has quota left; WaitLabel
	// resets first, at WaitUntil.
	Waiting   string `json:"waiting,omitempty"`
	WaitUntil int64  `json:"wait_until,omitempty"`
	WaitLabel string `json:"wait_label,omitempty"`
	// Automatic: the session's rule is to continue automatically.
	Automatic bool `json:"automatic"`
}

var errClaudeSwitchAnswered = errors.New("这个问题已在另一台设备上回答，界面已更新。")

func validSwitchPolicy(policy string) bool {
	return policy == "ask" || policy == "allow" || policy == "wait"
}

// switchPolicy is the session's own rule when the person set one, otherwise
// the rule in 调度设置.
//
// Sessions from before switch_policy_set carry a copy of the rule at their
// creation. A copied allow or wait was an explicit choice then and is kept; a
// copied ask was the default and now follows 调度设置.
func (r *mobileRecord) switchPolicy(list claudeAccountList) string {
	if (r.SwitchPolicySet || r.SwitchPolicy == "allow" || r.SwitchPolicy == "wait") && validSwitchPolicy(r.SwitchPolicy) {
		return r.SwitchPolicy
	}
	return list.Scheduling.defaults().SwitchPolicy
}

// claudeSwitchSource is the session's account while its identity is still
// the one the session was started with.
func claudeSwitchSource(list claudeAccountList, record *mobileRecord) *claudeAccount {
	source := list.find(record.Account.ID)
	if source == nil || source.guestID() != record.Account.GuestID || claudeIdentityHash(source.Email, source.Organization) != record.Account.Subject {
		return nil
	}
	return source
}

// The person's own choice is the authorization of a manual switch: every
// other account is listed, whatever its group, scheduling or quota, and
// one that is logged in can take the session.
func manualSwitchTargets(list claudeAccountList, record *mobileRecord, now time.Time) []claudeSwitchTarget {
	targets := []claudeSwitchTarget{}
	if claudeSwitchSource(list, record) == nil {
		return targets
	}
	for _, a := range list.Accounts {
		if a.ID == record.Account.ID {
			continue
		}
		room, _ := a.headroom(now)
		window, reset := a.exhaustedWindow(now)
		targets = append(targets, claudeSwitchTarget{a.ID, a.Label, a.Status, a.Enabled, window, reset, room})
	}
	return targets
}

// Automatic continuation moves a history only within its 接续分组. Accounts
// without one form an implicit group once the person said yes to that; an
// account given its own group stays apart.
func automaticSwitchTargets(list claudeAccountList, record *mobileRecord, implicit bool, now time.Time) []claudeSwitchOption {
	options := []claudeSwitchOption{}
	source := claudeSwitchSource(list, record)
	if source == nil || source.HandoffGroup == "" && !implicit {
		return options
	}
	for _, a := range list.Accounts {
		_, available := a.headroom(now)
		if a.ID != source.ID && a.Enabled && a.Status == "ready" && a.HandoffGroup == source.HandoffGroup && available {
			options = append(options, claudeSwitchOption{a.ID, a.Label})
		}
	}
	return options
}

// bestClaudeTarget applies the scheduling mode to the given targets.
func bestClaudeTarget(list claudeAccountList, options []claudeSwitchOption, active map[string]int, logins map[string]bool, last string, now time.Time) string {
	candidates := claudeAccountList{Scheduling: list.Scheduling}
	candidates.Scheduling.Preferred = ""
	for _, a := range list.Accounts {
		for _, option := range options {
			if option.ID == a.ID {
				candidates.Accounts = append(candidates.Accounts, a)
			}
		}
	}
	return pickClaudeAccount(candidates, active, logins, last, "", now)
}

// claudeHandoffState decides what the session shows at a quota limit. It
// reads only stored state: no query runs while nothing is limited.
func claudeHandoffState(list claudeAccountList, record *mobileRecord, now time.Time) claudeHandoffView {
	policy := record.switchPolicy(list)
	view := claudeHandoffView{Automatic: policy == "allow"}
	source := claudeSwitchSource(list, record)
	if source == nil || record.AccountFailure != "rate_limit" || record.Switch != nil && record.Switch.Phase != "completed" {
		return view
	}
	view.Window, view.ResetsAt = source.exhaustedWindow(now)
	if view.Window == "" {
		return view // an unconfirmed limit (model-specific, overload, unknown) is no exhaustion
	}
	if policy == "wait" || now.Unix() < record.HandoffWait {
		view.Waiting, view.WaitUntil, view.WaitLabel = "source", view.ResetsAt, source.Label
		return view
	}
	if policy == "allow" && len(automaticSwitchTargets(list, record, list.Scheduling.UngroupedHandoff, now)) > 0 {
		return view // the monitor continues it on another account
	}
	if suggested := automaticSwitchTargets(list, record, true, now); len(suggested) > 0 {
		pick := bestClaudeTarget(list, suggested, nil, nil, "", now)
		for i := range suggested {
			if suggested[i].ID == pick {
				view.Question = &suggested[i]
			}
		}
		return view
	}
	// No account that may continue it has quota left: name the earliest reset.
	view.Waiting, view.WaitUntil, view.WaitLabel = "all", view.ResetsAt, source.Label
	for _, a := range list.Accounts {
		if a.ID == source.ID || !a.Enabled || a.Status != "ready" || a.HandoffGroup != source.HandoffGroup {
			continue
		}
		if _, reset := a.exhaustedWindow(now); reset > 0 && (view.WaitUntil == 0 || reset < view.WaitUntil) {
			view.WaitUntil, view.WaitLabel = reset, a.Label
		}
	}
	return view
}

// claudeInterruptedNote tells the resumed model what the handoff stopped.
// It names only what the person sees: the latest turn's unfinished calls.
func claudeInterruptedNote(record *mobileRecord) string {
	var names []string
	if n := len(record.Turns); n > 0 {
		for _, tool := range record.Turns[n-1].Tools {
			if tool.Status == "called" {
				name := tool.Name
				if tool.Detail != "" {
					name = tool.Detail + " (" + tool.Name + ")"
				}
				names = append(names, "- "+name)
			}
		}
	}
	note := "\n\nThe previous execution was stopped for this account change. Background commands and subagents it had started were stopped with it; restart only what still matters, after checking what already took effect."
	if len(names) > 0 {
		note += "\nUnfinished when it stopped:\n" + strings.Join(names, "\n")
	}
	return note
}

func (s *Server) mobileSessionAccount(w http.ResponseWriter, r *http.Request) {
	view, terminal, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return
	}
	var input claudeSwitchInput
	if !readJSON(w, r, &input) {
		return
	}
	if view.Provider == codexProvider {
		// Codex sessions continue in the account their thread was recorded in.
		if input.Action != "inspect" {
			http.Error(w, "Codex 会话不支持切换账号", 409)
			return
		}
		jsonReply(w, map[string]any{"session": view, "supported": false, "candidates": []claudeSwitchOption{}})
		return
	}
	if input.Action == "switch" || input.Action == "answer" || input.Action == "undo" {
		if !input.Confirm || !mobileID(input.RequestID) || !validCLIUUID(input.SourceID) {
			http.Error(w, "请确认本次账号接续并提供当前会话标识", 400)
			return
		}
		var err error
		switch input.Action {
		case "switch":
			err = s.switchClaudeAccount(r.Context(), view.ID, input, false)
		case "answer":
			err = s.answerClaudeQuestion(r.Context(), view.ID, input)
		default:
			err = s.undoClaudeSwitch(r.Context(), view.ID, input)
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	} else if input.Action == "cancel" {
		if err := s.cancelClaudeSwitch(r.Context(), view.ID, input.RequestID); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	} else if input.Action == "policy" {
		if !validSwitchPolicy(input.Policy) {
			http.Error(w, "切换授权无效", 400)
			return
		}
		if err := s.changeClaudeSession(r.Context(), view.ID, func(record *mobileRecord) error {
			if record.Switch != nil && record.Switch.Phase != "completed" {
				return errors.New("账号交接中，请等待完成后修改授权")
			}
			record.SwitchPolicy, record.SwitchPolicySet = input.Policy, true
			return nil
		}); err != nil {
			requestError(w, err)
			return
		}
	} else if input.Action != "inspect" {
		http.Error(w, "未知账号操作", 400)
		return
	}
	if terminal {
		if _, err := s.terminalConversationView(r.Context(), view.ID); err != nil && input.Action != "inspect" {
			http.Error(w, err.Error(), 503)
			return
		}
	}
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, view.ID)
	if err != nil {
		s.mobileCLI.mu.Unlock()
		requestError(w, err)
		return
	}
	if a := list.find(record.Account.ID); a != nil {
		record.AccountLabel = a.Label
	}
	now := time.Now()
	snapshot := record.snapshot()
	policy := record.switchPolicy(list)
	snapshot.SwitchPolicy = policy
	sourceID := record.CLIID
	targets := manualSwitchTargets(list, record, now)
	// An older client lists candidates and needs the 接续分组 no longer.
	options := []claudeSwitchOption{}
	for _, target := range targets {
		if target.Status == "ready" {
			options = append(options, claudeSwitchOption{target.ID, target.Label})
		}
	}
	handoff := claudeHandoffState(list, record, now)
	undo := claudeUndoTarget(list, record) != nil
	busy := record.Active && record.AccountFailure == ""
	supported := record.Terminal && record.Launcher == "claude" && record.Account.ID != ""
	s.mobileCLI.mu.Unlock()
	// handoff_api tells a client which fields and actions this server knows.
	jsonReply(w, map[string]any{"session": snapshot, "source_id": sourceID, "switch_policy": policy, "candidates": options, "supported": supported,
		"handoff_api": 2, "targets": targets, "source_busy": busy, "handoff": handoff, "undo": undo})
}

// changeClaudeSession applies change to a session record under the operation
// lock and the unlock check.
func (s *Server) changeClaudeSession(ctx context.Context, id string, change func(*mobileRecord) error) error {
	if !s.op.TryLock() {
		return errTerminalBusy
	}
	defer s.op.Unlock()
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if !s.mobileAuthorized(ctx) {
		return errMobileLocked
	}
	s.mobileCLI.mu.Lock()
	defer s.mobileCLI.mu.Unlock()
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		return err
	}
	if err := change(record); err != nil {
		return err
	}
	record.changed()
	return writePrivate(mobileRecordPath(s.profile, id), record)
}

// answerClaudeQuestion records the person's answer to "A's quota is used up;
// continue on B?". The answer lives on the server, so answering on either
// device answers it on both.
func (s *Server) answerClaudeQuestion(ctx context.Context, id string, input claudeSwitchInput) error {
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return err
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, id)
	s.mobileCLI.mu.Unlock()
	if err != nil {
		return err
	}
	if sw := record.Switch; sw != nil && sw.ID == input.RequestID {
		return s.switchClaudeAccount(ctx, id, input, false) // a retried answer
	}
	// Only an open question takes an answer: once it was answered on one
	// device, the other device's answer changes nothing.
	state := claudeHandoffState(list, record, time.Now())
	if state.Question == nil {
		return errClaudeSwitchAnswered
	}
	standing := map[string]string{"always": "allow", "wait_always": "wait"}[input.Answer]
	if standing != "" {
		s.claudeAccounts.mu.Lock()
		list, err = s.loadClaudeAccounts()
		if err == nil {
			list.Scheduling.SwitchPolicy, list.Scheduling.SwitchAnswered = standing, true
			list.Scheduling.UngroupedHandoff = list.Scheduling.UngroupedHandoff || standing == "allow"
			err = s.saveClaudeAccounts(list)
		}
		s.claudeAccounts.mu.Unlock()
		if err != nil {
			return err
		}
	}
	switch input.Answer {
	case "once", "always":
		if input.Target == "" {
			input.Target = state.Question.ID
		}
		if err := s.changeClaudeSession(ctx, id, func(record *mobileRecord) error {
			record.followStandingAnswer(standing)
			return nil
		}); err != nil {
			return err
		}
		input.Continue = true
		return s.switchClaudeAccount(ctx, id, input, false)
	case "wait_once", "wait_always":
		return s.changeClaudeSession(ctx, id, func(record *mobileRecord) error {
			record.HandoffWait = state.ResetsAt
			record.followStandingAnswer(standing)
			return nil
		})
	}
	return errors.New("未知回答")
}

// followStandingAnswer lets the session follow a standing answer just given
// in it, instead of its own earlier rule.
func (r *mobileRecord) followStandingAnswer(standing string) {
	if standing != "" {
		r.SwitchPolicy, r.SwitchPolicySet = "", false
	}
}

// claudeUndoTarget is the account a handoff at a quota limit came from, while
// the person can still go back to it.
func claudeUndoTarget(list claudeAccountList, record *mobileRecord) *claudeAccount {
	sw := record.Switch
	if sw == nil || sw.Phase != "completed" || sw.Reason != "five_hour" && sw.Reason != "seven_day" || len(record.Executions) == 0 || record.CLIID != sw.TargetID {
		return nil
	}
	previous := record.Executions[len(record.Executions)-1].Account
	a := list.find(previous.ID)
	if a == nil || a.Status != "ready" || a.guestID() != previous.GuestID || claudeIdentityHash(a.Email, a.Organization) != previous.Subject {
		return nil
	}
	return a
}

// undoClaudeSwitch moves the session back to the account it came from, where
// it waits for that account's reset.
func (s *Server) undoClaudeSwitch(ctx context.Context, id string, input claudeSwitchInput) error {
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return err
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, id)
	s.mobileCLI.mu.Unlock()
	if err != nil {
		return err
	}
	if sw := record.Switch; sw != nil && sw.ID == input.RequestID {
		return s.switchClaudeAccount(ctx, id, input, false)
	}
	back := claudeUndoTarget(list, record)
	if back == nil {
		return errors.New("这次交接已不能撤销：可以在账号菜单中直接选择要使用的账号。")
	}
	input.Target, input.Continue = back.ID, false
	if err := s.switchClaudeAccount(ctx, id, input, false); err != nil {
		return err
	}
	_, reset := back.exhaustedWindow(time.Now())
	return s.changeClaudeSession(ctx, id, func(record *mobileRecord) error {
		record.HandoffWait = reset
		return nil
	})
}

func (s *Server) switchClaudeAccount(ctx context.Context, id string, input claudeSwitchInput, automatic bool) error {
	if !s.op.TryLock() {
		return errTerminalBusy
	}
	defer s.op.Unlock()
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	s.account.mu.Lock()
	allowed := s.mobileAuthorized(ctx)
	s.account.mu.Unlock()
	if !allowed {
		return errMobileLocked
	}
	s.mu.Lock()
	ready := s.ready && !s.closing && !s.guestUpdateRequired && claudeTerminalCommand(s.preferences) == "claude"
	s.mu.Unlock()
	if !ready {
		return errTerminalNotReady
	}
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return err
	}
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if record.Archived || record.Deleted || !record.Terminal || record.Launcher != "claude" {
		return errors.New("此会话不能进行账号接续")
	}
	if sw := record.Switch; sw != nil && sw.ID == input.RequestID && sw.Phase == "completed" {
		return nil
	}
	if old := record.Switch; old != nil && old.Phase != "completed" && old.ID != input.RequestID {
		if old.Phase != "preparing" {
			return errors.New("上一次交接已把会话历史交给 " + old.TargetLabel + "：点“重试账号交接”完成它。")
		}
		// A preparation that did not finish never froze the history. A new
		// choice replaces it; the person does not have to cancel it first.
		if err := s.dropClaudeSwitch(ctx, record); err != nil {
			return err
		}
	}
	if input.SourceID != record.CLIID && (record.Switch == nil || record.Switch.ID != input.RequestID || record.Switch.Source.CLIID != input.SourceID) {
		return errors.New("会话账号已变化，请刷新后确认")
	}
	fresh := record.Switch == nil || record.Switch.ID != input.RequestID
	if fresh {
		now := time.Now()
		source := claudeSwitchSource(list, record)
		if source == nil {
			return errors.New("原账号的登录身份已改变：在账号管理中检查 " + record.AccountLabel + "，重新登录后再切换。")
		}
		// A turn that ended on an account failure has nothing left to lose:
		// the source is stopped without asking.
		limited := record.AccountFailure != ""
		window, reset := source.exhaustedWindow(now)
		target := list.find(input.Target)
		if automatic {
			if record.switchPolicy(list) != "allow" || record.AccountFailure != "rate_limit" {
				return errors.New("当前会话需要手动确认切换")
			}
			if window == "" && !input.wake {
				return errors.New("尚未确认订阅额度耗尽，保留原账号")
			}
			eligible := input.wake && input.Target == source.ID
			for _, option := range automaticSwitchTargets(list, record, list.Scheduling.UngroupedHandoff, now) {
				eligible = eligible || option.ID == input.Target
			}
			if !eligible {
				return errors.New("目标账号不可用，请检查接续分组和额度")
			}
		} else {
			if target == nil || target.ID == source.ID {
				return errors.New("请选择另一个账号")
			}
			if target.Status != "ready" {
				return errors.New(target.Label + " 尚未登录：在账号管理中点“检查状态”或重新登录后再切换。")
			}
			if record.Active && !limited && !input.Force {
				return errors.New("当前任务仍在运行：确认“停止当前任务并切换到 " + target.Label + "”，或等本轮结束。")
			}
		}
		selection := claudeSelection{ID: target.ID, GuestID: target.guestID(), Subject: claudeIdentityHash(target.Email, target.Organization)}
		// Check the official CLI identity before stopping any existing work.
		status, probeErr := s.probeClaude(ctx, selection.GuestID)
		if probeErr != nil || status.subject() != selection.Subject {
			return errors.New("无法确认 " + target.Label + " 的登录：在账号管理中点“检查状态”，需要时重新登录。")
		}
		sourceStatus, sourceErr := s.probeClaude(ctx, record.Account.GuestID)
		if sourceErr != nil || sourceStatus.subject() != record.Account.Subject {
			return errors.New("原账号的登录身份已改变：在账号管理中检查 " + source.Label + "，重新登录后再切换。")
		}
		force := input.Force || automatic || limited
		sw := &claudeAccountSwitch{ID: input.RequestID, Phase: "preparing", Source: claudeExecution{record.CLIID, record.Account, record.GuestConfig}, Target: selection, TargetLabel: target.Label, TargetID: uuid.NewString(), Connection: record.Connection,
			Continue: input.Continue || force, Force: force, SourceLabel: source.Label, Automatic: automatic, At: now.Unix()}
		if limited && window != "" {
			sw.Reason, sw.ResetsAt = window, reset
		} else if input.wake {
			sw.Reason = "reset"
		}
		if force {
			sw.Note = claudeInterruptedNote(record)
		}
		record.Switch = sw
		if err := s.saveClaudeSwitch(record); err != nil {
			return err
		}
	}
	sw := record.Switch
	fail := func(err error) error { sw.Error = err.Error(); _ = s.saveClaudeSwitch(record); return err }
	// Nothing was frozen: the guest refused and lifted its own fence, or this
	// attempt failed before it reached the guest. The session stays as it was.
	undo := func(err error) error { record.Switch = nil; _ = s.saveClaudeSwitch(record); return err }
	before := func(err error) error {
		if fresh {
			return undo(err)
		}
		return fail(err) // an earlier attempt's outcome in the guest is unknown
	}
	if sw.Phase == "preparing" {
		// Authorization can be revoked or the target identity can change while
		// an interrupted preparation is waiting for a retry.
		target := list.find(sw.Target.ID)
		if claudeSwitchSource(list, record) == nil || target == nil || target.Status != "ready" || target.guestID() != sw.Target.GuestID || claudeIdentityHash(target.Email, target.Organization) != sw.Target.Subject {
			return before(errors.New(sw.TargetLabel + " 的登录已变化：在账号管理中检查它，再重新选择目标账号。"))
		}
		status, err := s.probeClaude(ctx, sw.Target.GuestID)
		if err != nil || status.subject() != sw.Target.Subject {
			return before(errors.New("无法确认 " + sw.TargetLabel + " 的登录：在账号管理中点“检查状态”，需要时重新登录。"))
		}
		var result struct {
			Transcript string          `json:"transcript"`
			Workspace  string          `json:"workspace"`
			Carry      []claudeCarried `json:"carry"`
			Error      string          `json:"error"`
		}
		if sw.Source.Config == "" || sw.Connection == "" {
			return before(errors.New("原会话连接尚未就绪：先打开这段会话，再切换。"))
		}
		// The session's project, not the folder its CLI is in now, decides
		// whether its history may continue here.
		root := s.cliConversationRoot(record)
		query := map[string]any{"cli_id": sw.Source.CLIID, "config": sw.Source.Config, "connection": sw.Connection, "workspace": record.GuestWorkspace, "root": root, "target_cli_id": sw.TargetID, "switch_id": sw.ID, "force": sw.Force}
		if err := s.windowsConversationInput(record, query); err != nil {
			return before(err)
		}
		if s.windowsGuest != nil {
			delete(query, "root") // the workspace is the registered project folder
		}
		err = s.runCLIChannel(ctx, "account-switch", query, &result)
		if err != nil {
			return fail(err)
		}
		if result.Error != "" {
			// The guest refused before freezing the history. Lift its fence
			// also when an older helper did not lift it itself.
			refusal := claudeSwitchRefusal(result.Error)
			if s.dropClaudeSwitch(ctx, record) != nil {
				return fail(refusal)
			}
			return refusal
		}
		if !strings.HasPrefix(result.Transcript, "/") && !(s.windowsGuest != nil && windowsCLIAbsolutePath(result.Transcript)) {
			return fail(errors.New("原会话历史未确认，未启动目标账号"))
		}
		if old := s.terminals.get(record.TerminalID); old != nil {
			s.terminals.remove(old.id)
		}
		record.Executions = append(record.Executions, sw.Source)
		record.CLIID, record.Account, record.AccountLabel = sw.TargetID, sw.Target, sw.TargetLabel
		record.GuestConfig, record.Connection, record.TerminalID = "", "", ""
		record.Resume, record.TerminalRunning, record.Active = false, false, false
		record.ResumeTranscript = result.Transcript
		if result.Workspace != "" && cliWorkspaceWithin(result.Workspace, root, s.windowsGuest != nil) {
			record.GuestWorkspace = result.Workspace // the target starts in the session's own folder
		}
		record.AccountFailure = ""
		record.HandoffWait, record.WaitingReset = 0, 0
		// Each message the source never took gets its own request id once, so
		// the target channel delivers it a single time whatever is retried.
		if sw.Carry == nil {
			sw.Carry = []claudeCarried{}
			for _, message := range result.Carry {
				sw.Carry = append(sw.Carry, claudeCarried{ID: uuid.NewString(), Text: message.Text})
			}
		}
		sw.Phase, sw.Error = "prepared", ""
		if err := s.saveClaudeSwitch(record); err != nil {
			return err
		}
	}
	if sw.Phase == "prepared" {
		// Treat a previous launch with unknown HTTP outcome as owned until its
		// PTY or the guest registry proves otherwise. The connection UUID is
		// deterministic for this switch, and guest launch also rejects duplicates.
		var state struct {
			Running bool `json:"running"`
		}
		query := map[string]any{"cli_id": record.CLIID, "config": record.GuestConfig, "account": record.Account.GuestID}
		if err := s.windowsConversationInput(record, query); err != nil {
			return fail(err)
		}
		if err := s.runCLIChannel(ctx, "active", query, &state); err != nil {
			return fail(err)
		}
		if state.Running {
			sw.Phase = "starting"
			return s.saveClaudeSwitch(record)
		}
		status, err := s.probeClaude(ctx, record.Account.GuestID)
		if err != nil || status.subject() != record.Account.Subject {
			return fail(errors.New("目标账号身份无法确认，保留待恢复历史"))
		}
		_, err = s.createTerminalLocked(terminalRequest{Workspace: record.Workspace, Kind: "claude", sharedID: id, requestContext: ctx})
		if err != nil {
			return fail(err)
		}
		// A launched process may outlive the HTTP request. Never launch it again
		// while its published PTY or guest registry can still own this execution.
		m.mu.Lock()
		latest, loadErr := m.record(s.profile, id)
		m.mu.Unlock()
		if loadErr != nil {
			return loadErr
		}
		record = latest
		record.Switch.Phase = "starting"
		if err := s.saveClaudeSwitch(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) cancelClaudeSwitch(ctx context.Context, id, requestID string) error {
	if !s.op.TryLock() {
		return errTerminalBusy
	}
	defer s.op.Unlock()
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	s.account.mu.Lock()
	allowed := s.mobileAuthorized(ctx)
	s.account.mu.Unlock()
	if !allowed {
		return errMobileLocked
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, id)
	s.mobileCLI.mu.Unlock()
	if err != nil {
		return err
	}
	if record.Switch == nil {
		return nil
	}
	if sw := record.Switch; sw.ID != requestID || sw.Phase != "preparing" {
		return errors.New("目标账号已接管历史，请在当前会话继续")
	}
	return s.dropClaudeSwitch(ctx, record)
}

// dropClaudeSwitch lifts the guest fence of a preparation that has not frozen
// the history and forgets it. The caller holds the operation lock.
func (s *Server) dropClaudeSwitch(ctx context.Context, record *mobileRecord) error {
	sw := record.Switch
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	query := map[string]any{"cli_id": sw.Source.CLIID, "connection": sw.Connection, "switch_id": sw.ID}
	if err := s.windowsConversationInput(record, query); err != nil {
		return err
	}
	if sw.Connection != "" {
		if err := s.runCLIChannel(ctx, "account-switch-cancel", query, &response); err != nil {
			return err
		}
		if !response.OK {
			return claudeSwitchRefusal(response.Error)
		}
	}
	record.Switch = nil
	return s.saveClaudeSwitch(record)
}

// The guest helper names a refusal with a short code, which keeps its
// embedded command within Windows' command-line limit. The host says it in
// one sentence that names the thing and the next step, the same on every device.
var claudeSwitchRefusals = map[string]string{
	"switch:reused":  "这次交接的编号已用于另一项操作：刷新会话后重新选择目标账号。",
	"switch:changed": "原会话的身份或项目已变化：刷新会话后再切换。",
	"switch:busy":    "原会话还有任务、待发消息或审批：确认“停止当前任务并切换”，或等本轮结束。",
	"switch:pending": "这段会话还有一次账号交接没有完成：点“重试账号交接”。",
	"switch:running": "原会话的 Claude Code 仍在执行本轮任务：确认“停止当前任务并切换”，或等它结束。",
	"switch:process": "无法确认原会话的 Claude Code 进程：在原终端退出 Claude Code 后再切换。",
	"switch:exit":    "原进程尚未退出，未切换：稍后点“重试账号交接”。",
	"switch:history": "找不到这段会话的历史文件，未切换：会话仍在原账号，可以继续使用。",
	"switch:project": "这段会话的历史属于另一个项目文件夹：从那个项目打开它后再切换。",
	"switch:cancel":  "原会话的交接已变化：刷新会话后再操作。",
}

func claudeSwitchRefusal(message string) error {
	if sentence, ok := claudeSwitchRefusals[message]; ok {
		return errors.New(sentence)
	}
	return errors.New(message)
}

// Preserve metadata written concurrently while committing execution ownership.
func (s *Server) saveClaudeSwitch(record *mobileRecord) error {
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.record(s.profile, record.ID)
	if err != nil {
		return err
	}
	record.CustomTitle, record.Pinned, record.Archived, record.Deleted = current.CustomTitle, current.Pinned, current.Archived, current.Deleted
	record.SwitchPolicy, record.SwitchPolicySet = current.SwitchPolicy, current.SwitchPolicySet
	record.Revision = current.Revision
	record.changed()
	return writePrivate(mobileRecordPath(s.profile, record.ID), record)
}

func (s *Server) monitorClaudeAccount(terminal *terminalSession, id string) {
	ctx, cancel := context.WithCancel(s.lifetimeContext())
	s.activities.Add(1)
	go func() {
		defer s.activities.Done()
		defer cancel()
		var lastUsageCheck time.Time
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-terminal.done:
				return
			case <-ticker.C:
			}
			epoch, ok := s.account.access()
			if !ok {
				continue
			}
			// Locking pauses observation; unlocking resumes it for the same
			// terminal. Cancel an in-flight attempt if its access epoch ends.
			attempt, finish := context.WithCancel(context.WithValue(ctx, accountEpochKey{}, epoch))
			stop := context.AfterFunc(epoch, finish)
			s.maybeSwitchClaudeAccount(attempt, id, &lastUsageCheck)
			stop()
			finish()
		}
	}()
}

func (s *Server) claudeNow() time.Time {
	if s.claudeAccounts.now != nil {
		return s.claudeAccounts.now()
	}
	return time.Now()
}

// maybeSwitchClaudeAccount runs on the terminal's existing ten-second check.
// While nothing is limited it only reads the view that check already reads.
func (s *Server) maybeSwitchClaudeAccount(ctx context.Context, id string, lastUsageCheck *time.Time) {
	view, err := s.terminalConversationView(ctx, id)
	if err != nil {
		return
	}
	s.deliverClaudeCarry(ctx, id, view)
	if view.AccountFailure != "rate_limit" || view.AccountSwitch != nil && view.AccountSwitch.Phase != "completed" {
		return
	}
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, id)
	s.mobileCLI.mu.Unlock()
	if err != nil || record.switchPolicy(list) != "allow" || claudeSwitchSource(list, record) == nil {
		return // nothing moves before the person said yes
	}
	now := s.claudeNow()
	if now.Unix() < record.HandoffWait {
		return // the person chose to wait for this account
	}
	// One bounded quota refresh on the failure path. The CLI owns this
	// query and its tokens; no model prompt is generated for probing.
	if time.Since(*lastUsageCheck) >= claudeUsageInterval {
		*lastUsageCheck = time.Now()
		_ = s.refreshClaudeUsage(ctx, claudeAccountInput{Action: "refresh", ID: record.Account.ID})
		s.claudeAccounts.mu.Lock()
		list, err = s.loadClaudeAccounts()
		s.claudeAccounts.mu.Unlock()
		if err != nil {
			return
		}
	}
	active, logins := s.mobileCLI.activeAccounts(), map[string]bool{}
	for _, terminal := range s.terminals.snapshot() {
		if account, _ := terminal["claude_account_id"].(string); terminal["closed"] != true {
			if terminal["kind"] == "claude-login" {
				logins[account] = true
			} else {
				active[account]++
			}
		}
	}
	s.claudeAccounts.mu.Lock()
	last := s.claudeAccounts.lastPick
	s.claudeAccounts.mu.Unlock()
	target, wake, waitUntil := claudeAutomaticStep(list, record, active, logins, last, now)
	if waitUntil != record.WaitingReset {
		_ = s.changeClaudeSession(ctx, id, func(record *mobileRecord) error {
			record.WaitingReset = waitUntil
			return nil
		})
	}
	if target != "" {
		_ = s.switchClaudeAccount(ctx, id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: target, wake: wake}, true)
	}
}

// claudeAutomaticStep decides, under automatic continuation, what a session
// limited by its account's quota does now: move to target, continue on its
// own account after a reset (wake), or wait until waitUntil, the first
// reset of the accounts that may take it.
func claudeAutomaticStep(list claudeAccountList, record *mobileRecord, active map[string]int, logins map[string]bool, last string, now time.Time) (target string, wake bool, waitUntil int64) {
	source := claudeSwitchSource(list, record)
	if source == nil || record.switchPolicy(list) != "allow" || record.AccountFailure != "rate_limit" || now.Unix() < record.HandoffWait {
		return "", false, record.WaitingReset
	}
	window, _ := source.exhaustedWindow(now)
	options := automaticSwitchTargets(list, record, list.Scheduling.UngroupedHandoff, now)
	switch {
	case window != "" && len(options) > 0:
		return bestClaudeTarget(list, options, active, logins, last, now), false, 0
	case window != "":
		// Every account that may continue it is used up. This same check
		// continues it at the first reset.
		return "", false, claudeHandoffState(list, record, now).WaitUntil
	case record.WaitingReset > 0 && now.Unix() >= record.WaitingReset:
		// The source reset first. Its CLI waits in its limit menu, so the
		// history continues in a fresh CLI on the same account.
		return source.ID, true, record.WaitingReset
	}
	return "", false, record.WaitingReset // an unconfirmed limit is not an exhaustion
}

// deliverClaudeCarry gives the target, once it is idle, the messages the
// source never took: one per check, under the request id each got when the
// history was frozen. The target channel acknowledges a repeated id without
// delivering it again, so a retry after a lost reply is harmless.
func (s *Server) deliverClaudeCarry(ctx context.Context, id string, view mobileSession) {
	if view.AccountSwitch == nil || !view.AccountSwitch.carry || view.CanSend == nil || !*view.CanSend || view.Active {
		return // the usual case reads nothing more
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, id)
	s.mobileCLI.mu.Unlock()
	if err != nil || record.Switch == nil || record.Switch.Phase != "completed" || record.CLIID != record.Switch.TargetID {
		return
	}
	for _, message := range record.Switch.Carry {
		if message.Delivered {
			continue
		}
		if _, err := s.terminalConversationAction(ctx, id, map[string]any{"action": "send", "request_id": message.ID, "text": message.Text}); err != nil {
			return
		}
		_ = s.changeClaudeSession(ctx, id, func(record *mobileRecord) error {
			for i := range record.Switch.Carry {
				if record.Switch.Carry[i].ID == message.ID {
					record.Switch.Carry[i].Delivered = true
				}
			}
			return nil
		})
		return
	}
}
