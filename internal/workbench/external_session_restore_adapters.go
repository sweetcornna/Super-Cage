package workbench

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/localorca"
	"github.com/sylvinus/agent-vm/internal/localterm"
)

type externalSessionResult struct {
	State   string   `json:"state"`
	Message string   `json:"message,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Dir     string   `json:"working_directory,omitempty"`
	Command string   `json:"command,omitempty"`
}

var externalTerminalHandle = regexp.MustCompile(`^term_[A-Za-z0-9_-]{4,128}$`)

func (s *Server) externalOrcaConnection(ctx context.Context, environment, expectedRuntime string) (*localorca.Client, string, error) {
	installation := s.detectLocalOrca()
	if !installation.Installed || installation.CLIPath == "" {
		return nil, "", errors.New("这台电脑未安装原 Orca 客户端")
	}
	client := s.localOrcaClient(installation)
	if environment == "local" {
		if expectedRuntime != "" {
			return nil, "", errors.New("原 Orca 连接已变化")
		}
		return client, "", nil
	}
	if !externalScopeID.MatchString(environment) {
		return nil, "", errors.New("请选择 Orca 中已有的连接")
	}
	saved, err := client.ListEnvironments(ctx)
	if err != nil {
		return nil, "", errors.New("无法读取这台电脑的 Orca 连接")
	}
	for _, candidate := range saved {
		if candidate.ID != environment {
			continue
		}
		if !externalScopeID.MatchString(candidate.RuntimeID) || expectedRuntime != "" && candidate.RuntimeID != expectedRuntime {
			return nil, "", errors.New("原 Orca 服务的身份已变化")
		}
		return client, candidate.RuntimeID, nil
	}
	return nil, "", errors.New("原 Orca 连接已离线、已移除或尚未配对")
}

type externalOrcaObservation struct {
	record externalSessionRecord
	probe  *externalOriginProbe
}

func (s *Server) captureExternalOrca(ctx context.Context, watch externalSessionWatch, register bool) (int, error) {
	client, identity, err := s.externalOrcaConnection(ctx, watch.Connection, watch.RuntimeID)
	if err != nil {
		return 0, err
	}
	watch.RuntimeID = identity
	return s.captureExternalOrcaWithClient(ctx, watch, register, client)
}

func (s *Server) captureExternalOrcaWithClient(ctx context.Context, watch externalSessionWatch, register bool, client *localorca.Client) (int, error) {
	placements, err := client.AgentPlacements(ctx, watch.Connection, watch.Workspace)
	if err != nil {
		return 0, err
	}
	var terminals []localorca.TerminalPlacement
	for _, row := range placements {
		if row.Kind == "terminal" && row.PaneKey != "" && (row.SessionID == "" || row.Provider == "codex") {
			terminals, _ = client.TerminalPlacements(ctx, watch.Connection, watch.Workspace)
			break
		}
	}
	observations := make([]externalOrcaObservation, 0, len(placements))
	for _, row := range placements {
		if !externalSessionText(row.WorktreeID, 1200) ||
			!externalScopeID.MatchString(row.Provider) || (row.Kind != "chat" && row.Kind != "terminal") ||
			(row.HostID != "" && !externalSessionText(row.HostID, 128)) {
			continue
		}
		r := externalSessionRecord{
			App: "orca", Guest: watch.Guest, CageInstance: watch.CageInstance, Connection: watch.Connection, RuntimeID: watch.RuntimeID,
			Workspace: watch.Workspace, WorktreeID: row.WorktreeID, Continuation: row.SessionID,
			HostID: externalOrcaHost(row.HostID), Kind: row.Kind, Provider: row.Provider,
		}
		var terminal *localorca.TerminalPlacement
		if row.Kind == "terminal" && externalSessionText(row.PaneKey, 256) {
			terminal = externalOrcaTerminal(r, row.PaneKey, terminals)
		}
		if terminal != nil {
			r.Handle, r.Incarnation = terminal.Handle, terminal.IncarnationID
		}
		if !externalOrcaID.MatchString(r.Continuation) {
			if r.Continuation != "" || terminal == nil {
				continue
			}
			r.PaneKey = row.PaneKey
			r.Continuation = "pane:" + row.PaneKey
		}
		r.ID = externalSessionKey(r)
		observation := externalOrcaObservation{record: r}
		if r.Provider == "codex" && terminal != nil {
			observation.probe = &externalOriginProbe{
				Handle: terminal.Handle, TabID: terminal.TabID, WorktreeID: r.WorktreeID,
				Workspace: r.Workspace, Provider: r.Provider,
			}
		}
		observations = append(observations, observation)
	}

	// Persist the placement before requesting provider provenance. An offline
	// Cage guest still leaves an exact live reattach target in the journal.
	count, pending, err := s.saveExternalOrcaObservations(ctx, watch, register, observations)
	if err != nil {
		return count, err
	}
	for _, observation := range pending {
		if ctx.Err() != nil {
			break
		}
		origin, err := s.externalOriginOnCage(ctx, watch.CageInstance, *observation.probe)
		if err != nil || !origin.verified() {
			continue
		}
		err = s.recordExternalOrcaOrigin(observation.record, origin)
		if err != nil && !errors.Is(err, errExternalSessionClosed) {
			return count, err
		}
	}
	return count, nil
}

func (s *Server) saveExternalOrcaObservations(ctx context.Context, watch externalSessionWatch, register bool, observations []externalOrcaObservation) (int, []externalOrcaObservation, error) {
	s.externalSessions.mu.Lock()
	defer s.externalSessions.mu.Unlock()
	if ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	data, err := s.loadExternalSessionJournal()
	if err != nil {
		return 0, nil, err
	}
	changed := register && !containsExternalWatch(data.Orca, watch)
	if changed {
		if len(data.Orca) >= externalSessionLimit {
			return 0, nil, errors.New("原 Orca 工作区记录已达上限")
		}
		data.Orca = append(data.Orca, watch)
	}
	known := make(map[string]externalSessionRecord, len(data.Records))
	launched := make(map[string]bool)
	for _, r := range data.Records {
		known[r.ID] = r
		if r.Guest == watch.Guest && r.CageInstance == watch.CageInstance && r.Connection == watch.Connection && r.RuntimeID == watch.RuntimeID &&
			r.Workspace == watch.Workspace && r.ResumedHandle != "" {
			launched[r.ResumedHandle] = true
		}
	}
	now := time.Now().Unix()
	count := 0
	var pending []externalOrcaObservation
	var result error
	for _, observation := range observations {
		r := observation.record
		if observation.probe != nil && launched[observation.probe.Handle] {
			continue
		}
		dirty, err := updateExternalSessionRecord(&data, r, now)
		if err != nil {
			if !errors.Is(err, errExternalSessionClosed) {
				result = errors.Join(result, err)
			}
			continue
		}
		changed = changed || dirty
		count++
		if observation.probe != nil && known[r.ID].Native == nil && len(pending) < 4 {
			pending = append(pending, observation)
		}
	}
	if changed {
		result = errors.Join(result, s.saveExternalSessionJournal(data))
	}
	return count, pending, result
}

func containsExternalWatch(list []externalSessionWatch, watch externalSessionWatch) bool {
	for _, existing := range list {
		if existing == watch {
			return true
		}
	}
	return false
}

func externalOrcaHost(value string) string {
	if value == "" {
		return "local"
	}
	return value
}

func externalOrcaTerminal(record externalSessionRecord, paneKey string, terminals []localorca.TerminalPlacement) *localorca.TerminalPlacement {
	var match *localorca.TerminalPlacement
	for _, t := range terminals {
		if t.TabID == "" || t.LeafID == "" || t.TabID+":"+t.LeafID != paneKey || t.WorktreeID != record.WorktreeID ||
			t.WorktreePath != "" && t.WorktreePath != record.Workspace ||
			externalOrcaHost(t.ExecutionHostID) != externalOrcaHost(record.HostID) ||
			!t.Connected || !t.Writable || t.Orphaned || !externalTerminalHandle.MatchString(t.Handle) {
			continue
		}
		if match != nil {
			return nil
		}
		match = &t
	}
	return match
}

func externalOrcaAwaiting(message string) externalSessionResult {
	return externalSessionResult{State: "awaiting_client", Message: message}
}

func (s *Server) recoverExternalOrca(ctx context.Context, r externalSessionRecord) (externalSessionResult, error) {
	client, _, err := s.externalOrcaConnection(ctx, r.Connection, r.RuntimeID)
	if err != nil {
		return externalSessionResult{}, err
	}
	var placements []localorca.AgentPlacement
	if r.PaneKey != "" {
		placements, err = client.AgentPlacements(ctx, r.Connection, r.Workspace)
	} else {
		var id string
		id, placements, err = client.AgentPlacement(ctx, r.Connection, r.Continuation)
		if err == nil && id != r.Continuation {
			err = errors.New("Orca returned another session")
		}
	}
	var original *localorca.AgentPlacement
	if err == nil {
		for _, place := range placements {
			if place.WorktreeID != r.WorktreeID || externalOrcaHost(place.HostID) != externalOrcaHost(r.HostID) ||
				place.Provider != r.Provider || place.Kind != r.Kind ||
				r.PaneKey != "" && place.PaneKey != r.PaneKey {
				continue
			}
			if original != nil {
				return externalSessionResult{State: "choose_pane", Message: "Orca 报告多个原会话窗格，请在原工作区选择。"}, nil
			}
			original = &place
		}
	}
	if r.Kind == "terminal" {
		terminals, err := client.TerminalPlacements(ctx, r.Connection, r.Workspace)
		if err != nil {
			return externalOrcaAwaiting("当前无法确认原 Orca 工作区的终端状态。请保持原连接并重试。"), nil
		}
		pane := r.PaneKey
		if pane == "" && original != nil {
			pane = original.PaneKey
		}
		if pane != "" {
			terminal := externalOrcaTerminal(r, pane, terminals)
			if terminal != nil && (r.PaneKey == "" ||
				r.Incarnation != "" && terminal.IncarnationID == r.Incarnation ||
				r.Incarnation == "" && terminal.Handle == r.Handle) {
				result := focusOrcaRecovery(ctx, client, r, terminal.Handle)
				if result.State == "reattached" || r.PaneKey != "" || r.Native != nil {
					return result, nil
				}
				return openOrcaRecovery(ctx, client)
			}
		}
		if r.RuntimeID != "" && r.Native != nil && r.Native.verified() {
			return s.recoverOrcaCodexCold(ctx, client, r, terminals, true)
		}
		if r.PaneKey != "" {
			return externalOrcaAwaiting("原 Orca 终端当前不可重接。尚未核实其原会话 ID 和账号，请在原客户端检查会话历史。"), nil
		}
	}
	return openOrcaRecovery(ctx, client)
}

func openOrcaRecovery(ctx context.Context, client *localorca.Client) (externalSessionResult, error) {
	if err := client.OpenDesktop(ctx); err != nil {
		return externalSessionResult{}, err
	}
	return externalSessionResult{State: "client_opened", Message: "已打开 Orca。请在原连接、工作区及账号下选择已保存的同一会话。"}, nil
}

func focusOrcaRecovery(ctx context.Context, client *localorca.Client, r externalSessionRecord, handle string) externalSessionResult {
	focused, err := client.FocusRecoveryTerminal(ctx, r.Connection, handle, r.WorktreeID)
	if err == nil && focused {
		return externalSessionResult{State: "reattached"}
	}
	return externalOrcaAwaiting("已找到原 Orca 终端，请在原客户端打开该窗格。")
}

func (s *Server) recoverOrcaCodexCold(ctx context.Context, client *localorca.Client, r externalSessionRecord, terminals []localorca.TerminalPlacement, focus bool) (externalSessionResult, error) {
	unknownLive := false
	for _, terminal := range terminals {
		if terminal.WorktreeID != r.WorktreeID || externalOrcaHost(terminal.ExecutionHostID) != externalOrcaHost(r.HostID) ||
			!terminal.Connected || terminal.Orphaned {
			continue
		}
		if terminal.Handle == r.Handle && r.ResumedHandle == "" &&
			(r.Incarnation == "" || terminal.IncarnationID == r.Incarnation) && !focus {
			return externalSessionResult{State: "existing_terminal"}, nil
		}
		if terminal.Writable && externalTerminalHandle.MatchString(terminal.Handle) && (focus || r.ResumeBoot != "") {
			origin, err := s.externalOriginOnCage(ctx, r.CageInstance, externalOriginProbe{
				Handle: terminal.Handle, TabID: terminal.TabID, WorktreeID: r.WorktreeID,
				Workspace: r.Workspace, Provider: r.Provider,
			})
			if err == nil && origin.verified() {
				if origin.sameIdentity(*r.Native) {
					if err := s.updateExternalOrca(r.ID, func(row *externalSessionRecord) bool {
						changed := row.Native.BootID != origin.BootID || row.ResumeBoot != "" || row.ResumedHandle != terminal.Handle
						row.Native, row.ResumeBoot, row.ResumedHandle = &origin, "", terminal.Handle
						return changed
					}); err != nil {
						return externalSessionResult{}, err
					}
					if focus {
						return focusOrcaRecovery(ctx, client, r, terminal.Handle), nil
					}
					return externalSessionResult{State: "existing_terminal"}, nil
				}
				continue // another positively identified Codex thread
			}
		}
		unknownLive = true
	}
	if unknownLive {
		return externalOrcaAwaiting("原 Orca 工作区有身份待确认的活动终端，请在 Orca 核对原会话。"), nil
	}
	if r.ResumeBoot != "" {
		return externalOrcaAwaiting("此前已请求恢复这条原会话。请在原 Orca 工作区核对已打开的终端。"), nil
	}
	origin, err := s.externalOriginOnCage(ctx, r.CageInstance, externalOriginProbe{
		Workspace: r.Workspace, Provider: r.Provider, WorktreeID: r.WorktreeID, Original: r.Native,
	})
	if err != nil || !origin.sameIdentity(*r.Native) {
		return externalOrcaAwaiting("原 Codex 账号或会话身份当前无法确认，请恢复原账号后重试。"), nil
	}
	if !focus && (!externalThreadID.MatchString(r.Native.BootID) || r.Native.BootID == origin.BootID) {
		return externalOrcaAwaiting("这一轮原环境尚未重新启动，未创建新终端。"), nil
	}
	verified, err := client.VerifyRecoveryWorkspace(ctx, r.Connection, r.WorktreeID, r.Workspace, externalOrcaHost(r.HostID))
	if err != nil || !verified {
		return externalOrcaAwaiting("当前未能确认原 Orca 工作区及执行主机。"), nil
	}
	if current, _ := s.externalSessionInstance(); current != r.CageInstance {
		return externalSessionResult{}, errors.New("原 Super Cage 连接已更改")
	}
	claim, err := s.externalOriginOnCage(ctx, r.CageInstance, externalOriginProbe{
		Workspace: r.Workspace, Provider: r.Provider, Original: r.Native, ClaimBoot: origin.BootID,
	})
	if err != nil || claim.BootID != origin.BootID || claim.State != "claimed" && claim.State != "already_claimed" {
		return externalOrcaAwaiting("未能核实原环境的恢复领取状态，请重连后重试。"), nil
	}
	if err := s.updateExternalOrca(r.ID, func(row *externalSessionRecord) bool {
		row.ResumeBoot, row.ResumedHandle = origin.BootID, ""
		return true
	}); err != nil {
		return externalSessionResult{}, err
	}
	if claim.State == "already_claimed" {
		return externalOrcaAwaiting("另一入口已领取这条原会话的恢复，请查看原 Orca 工作区。"), nil
	}
	// Orca's public CLI has no idempotency key. Reserve the attempt before
	// dispatch, and verify the exact provider thread before marking it attached.
	command := "exec env CAGE_CODEX_SUBJECT=" + origin.Subject + " /usr/local/bin/cage-codex-account run " + origin.Account + " resume " + origin.Thread
	terminal, err := client.StartRecoveryTerminal(ctx, r.Connection, r.WorktreeID, command, focus)
	if err != nil || !externalTerminalHandle.MatchString(terminal.Handle) || terminal.WorktreeID != r.WorktreeID ||
		externalOrcaHost(terminal.ExecutionHostID) != externalOrcaHost(r.HostID) {
		return externalOrcaAwaiting("已向原 Orca 发出恢复请求，当前无法核实结果。请在原工作区检查。"), nil
	}
	if err := s.updateExternalOrca(r.ID, func(row *externalSessionRecord) bool {
		row.ResumedHandle = terminal.Handle
		return true
	}); err != nil {
		return externalSessionResult{}, err
	}
	return externalSessionResult{State: "resume_started", Message: "已在原 Orca 工作区请求使用原账号恢复这条 Codex 会话。等待 Codex 确认。"}, nil
}

// Both the desktop and guest entry use the same capture and launch logic.
// The caller holds recovery while checking the inventory and reserving starts.
func (s *Server) reconcileExternalOrcaWithClient(ctx context.Context, watch externalSessionWatch, client *localorca.Client) error {
	s.externalSessions.mu.Lock()
	data, err := s.loadExternalSessionJournal()
	s.externalSessions.mu.Unlock()
	if err != nil {
		return err
	}
	var candidates []externalSessionRecord
	for _, r := range data.Records {
		if r.App == "orca" && r.RuntimeID != "" && !r.Closed && r.Guest == watch.Guest && r.CageInstance == watch.CageInstance &&
			r.Connection == watch.Connection && r.RuntimeID == watch.RuntimeID && r.Workspace == watch.Workspace &&
			r.Kind == "terminal" && r.Native != nil && r.Native.verified() && externalThreadID.MatchString(r.Native.BootID) {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	terminals, err := client.TerminalPlacements(ctx, watch.Connection, watch.Workspace)
	if err != nil {
		return err
	}
	var result error
	for _, r := range candidates {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		if r.ResumeBoot == "" && s.monitorExistingOrcaSession(client, r, terminals) {
			continue
		}
		recovery, err := s.recoverOrcaCodexCold(ctx, client, r, terminals, false)
		result = errors.Join(result, err)
		if recovery.State == "resume_started" || recovery.State == "awaiting_client" && r.ResumeBoot == "" {
			// Refresh before another launch. The receipt may have been
			// lost, and an unknown live terminal is treated conservatively.
			terminals, err = client.TerminalPlacements(ctx, watch.Connection, watch.Workspace)
			if err != nil {
				return errors.Join(result, err)
			}
		}
	}
	return result
}

func (s *Server) monitorExistingOrcaSession(client *localorca.Client, r externalSessionRecord, terminals []localorca.TerminalPlacement) bool {
	handle := r.Handle
	if r.ResumedHandle != "" {
		handle = r.ResumedHandle
	}
	for _, t := range terminals {
		if t.Handle == handle && t.WorktreeID == r.WorktreeID && externalOrcaHost(t.ExecutionHostID) == externalOrcaHost(r.HostID) &&
			t.Connected && t.Writable && !t.Orphaned &&
			(r.ResumedHandle != "" || r.Incarnation == "" || t.IncarnationID == r.Incarnation) {
			s.watchExternalOrcaExit(client, r, handle)
			return true
		}
	}
	return false
}

func (s *Server) watchExternalOrcaExit(client *localorca.Client, r externalSessionRecord, handle string) {
	m := &s.externalSessions
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.ID + ":" + handle
	if m.exits[key] || len(m.exits) >= 8 {
		return
	}
	ctx, release, err := s.beginActivity(context.Background())
	if err != nil {
		return
	}
	if m.exits == nil {
		m.exits = make(map[string]bool)
	}
	m.exits[key] = true
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.exits, key)
			m.mu.Unlock()
			release()
		}()
		exited, err := client.WaitForRecoveryExit(ctx, r.Connection, handle)
		if err != nil || !exited || ctx.Err() != nil {
			return
		}
		boot, err := s.externalOriginOnCage(ctx, r.CageInstance, externalOriginProbe{
			Workspace: r.Workspace, Provider: r.Provider, BootOnly: true,
		})
		if err != nil || boot.State != "boot" || boot.BootID != r.Native.BootID {
			return
		}
		m.recovery.Lock()
		defer m.recovery.Unlock()
		_ = s.updateExternalOrca(r.ID, func(row *externalSessionRecord) bool {
			if row.Native == nil || row.Native.BootID != boot.BootID || row.ResumeBoot != "" ||
				row.ResumedHandle != "" && row.ResumedHandle != handle ||
				row.ResumedHandle == "" && row.Handle != handle {
				return false
			}
			row.Closed = true
			return true
		})
	}()
}

func externalDshResume(r externalSessionRecord, confirmedAccount string) (externalSessionResult, error) {
	if confirmedAccount == "" || confirmedAccount != r.Account {
		return externalSessionResult{}, errors.New("请先在原应用核对并确认记录中的账号")
	}
	if r.App == "deepseek-desktop" {
		if runtime.GOOS != "darwin" {
			return externalSessionResult{State: "awaiting_client", Message: "请在原电脑打开 DeepSeek Harness，并在会话列表选择记录中的会话。"}, nil
		}
		return externalSessionResult{State: "needs_desktop_open"}, nil
	}
	argv := []string{"dsh", "--profile", r.Profile, "--resume", r.Continuation}
	quoted := make([]string, len(argv))
	for i, part := range argv {
		quoted[i] = localterm.ShellQuote(part)
	}
	return externalSessionResult{
		State: "native_command", Argv: argv, Dir: r.Workspace,
		Command: "cd " + localterm.ShellQuote(r.Workspace) + " && " + strings.Join(quoted, " "),
		Message: "请回到运行此会话的原终端或 SSH 连接，用原 DeepSeek 账号和原 DSH_HOME 运行。命令只恢复这条会话。",
	}, nil
}

var openExternalDeepSeek = func(ctx context.Context) error {
	// Use the vendor's registered application, not the handler of a user URL.
	return exec.CommandContext(ctx, "/usr/bin/open", "-b", "com.deepseek.dsh").Run()
}
