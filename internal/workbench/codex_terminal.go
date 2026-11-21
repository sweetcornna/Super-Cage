package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
)

// Caller holds the environment operation lock through PTY publication.
func (s *Server) createCodexTerminalLocked(input terminalRequest) (*terminalSession, error) {
	if !s.codexGuestSupported() {
		return nil, errors.New("Codex 终端目前只支持 Linux 环境")
	}
	if input.Kind == "codex" && input.ClaudeAccountID == "" && (input.sharedID != "" || input.codexSession && s.codexSessionsPossible()) {
		return s.createCodexSessionLocked(input)
	}
	if input.ClaudeAccountID != "" || input.sharedID != "" {
		return nil, errors.New("Codex 终端不能指定 Claude 账号或会话")
	}
	s.mu.Lock()
	closing, sealed, componentUpdate := s.closing, s.appUpdateSealed, s.guestUpdateRequired
	command := codexTerminalCommand(s.preferences)
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if sealed {
		return nil, errAppUpdateSealed
	}
	if !s.vmRunning() || s.busy() {
		return nil, errTerminalNotReady
	}
	login := input.Kind == "codex-login"
	// Login runs from the guest home and needs no project share.
	workspace := s.workspace()
	if !login {
		if err := s.terminals.canStart(); err != nil {
			return nil, err
		}
		var err error
		if workspace, err = s.connectProject(input.Workspace, false); err != nil {
			return nil, err
		}
		directories, err := s.projectAdditionalDirectories(workspace)
		if err != nil {
			return nil, err
		}
		for _, directory := range directories {
			command += " --add-dir " + shellescape.Quote(directory)
		}
	}
	selection, err := s.selectCodexAccount(input.CodexAccountID, login, workspace)
	if err != nil {
		return nil, err
	}
	var finished func(*terminalSession)
	if selection.ID != "" {
		if componentUpdate {
			return nil, errGuestUpdateRequired
		}
		if !validClaudeID(selection.ID) || selection.GuestID != selection.ID {
			return nil, errors.New("Codex 账号 ID 无效")
		}
		if existing, conflict := s.terminals.existingCodexLogin(selection.ID, login); existing != nil || conflict != nil {
			return existing, conflict
		}
		if !login && selection.Subject == "" {
			return nil, errors.New("Codex 账号状态已变化，请重试")
		}
		finished = s.codexTerminalFinished(selection.ID, login)
	}
	script := codexTerminalScript(s.cliWorkspace(workspace), command, selection.GuestID, selection.Subject, login)
	cmd := s.sshCommand(s.lifetimeContext(), "-tt", s.sshTarget(), "--", script)
	if input.requestContext != nil {
		s.account.mu.Lock()
		if !s.mobileAuthorized(input.requestContext) {
			s.account.mu.Unlock()
			return nil, errMobileLocked
		}
	}
	session, err := s.terminals.startCodexSession(cmd, workspace, input.Kind, selection, finished)
	if input.requestContext != nil {
		s.account.mu.Unlock()
	}
	if err == nil && !login && selection.ID != "" {
		s.noteCodexSelection(selection.ID, workspace)
	}
	return session, err
}

var (
	errCodexPhoneRunning  = errors.New("手机发起的一轮仍在运行，完成后再在终端中继续。")
	errCodexOtherTerminal = errors.New("这段会话已在另一个电脑终端中打开。")
	errCodexOtherProcess  = errors.New("这段会话正在其他 Codex 进程中打开，请先退出该进程后再继续。")
	errCodexCommand       = errors.New("Codex 启动命令不是 codex，无法以会话方式打开。请在设置中恢复为 codex 后重试。")
	// The phone may watch, type into and stop a desktop Codex session, but
	// closing its terminal is the desktop's.
	errCodexDesktopTerminal = errors.New("这段会话正在电脑的 Codex 终端中打开，请在电脑上关闭或归档。")
)

// createCodexSessionLocked opens a Codex session in a terminal bound to its
// record: the Codex TUI on the session's pinned account, resumed on its
// thread, under a lease in the guest. The menu's Codex entry makes the record
// first. The caller holds the operation lock through PTY publication; phone
// messages take it too, so none starts between the owner check here and the
// terminal becoming the session's.
func (s *Server) createCodexSessionLocked(input terminalRequest) (*terminalSession, error) {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if !s.vmRunning() || s.busy() {
		return nil, errTerminalNotReady
	}
	if !s.codexSessionsPossible() {
		return nil, errCodexCommand
	}
	var record *mobileRecord
	if input.sharedID != "" {
		s.mobileCLI.mu.Lock()
		loaded, err := s.mobileCLI.record(s.profile, input.sharedID)
		s.mobileCLI.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !loaded.codex() {
			return nil, errors.New("此会话不是 Codex 会话")
		}
		if loaded.Archived || loaded.Deleted {
			return nil, errors.New("此会话已归档或删除，请先从已归档会话恢复")
		}
		// Opening again shows the terminal that already owns the thread.
		if existing := s.terminals.boundCodex(loaded.ID); existing != nil {
			return existing, nil
		}
		s.terminals.removeEndedCodex(loaded.ID)
		record = loaded
	}
	if err := s.terminals.canStart(); err != nil {
		return nil, err
	}
	project := input.Workspace
	if record != nil {
		project = record.Workspace
	}
	workspace, err := s.connectProject(project, false)
	if err != nil {
		return nil, err
	}
	directories, err := s.projectAdditionalDirectories(workspace)
	if err != nil {
		return nil, err
	}
	if directories == nil {
		directories = []string{}
	}
	var selection codexSelection
	if record == nil {
		selection, err = s.selectCodexAccount(input.CodexAccountID, false, workspace)
	} else {
		selection, err = s.pinnedCodexSelection(record)
	}
	if err != nil {
		return nil, err
	}
	if selection.ID != "" {
		if !validClaudeID(selection.ID) || selection.Subject == "" {
			return nil, errors.New("Codex 账号状态已变化，请重试")
		}
		if _, conflict := s.terminals.existingCodexLogin(selection.ID, false); conflict != nil {
			return nil, conflict
		}
	}
	root := s.cliWorkspace(workspace)
	thread := ""
	if record == nil {
		view, err := s.mobileCLI.createProvider(s.profile, workspace, uuid.NewString(), codexProvider, func(created *mobileRecord) {
			closed := false
			created.Terminal, created.CodexAccount, created.DesktopMenu = true, selection.ID, true
			created.Connection, created.GuestWorkspace = uuid.NewString(), root
			created.CanSend = &closed
		})
		if err != nil {
			return nil, err
		}
		s.mobileCLI.mu.Lock()
		record, err = s.mobileCLI.record(s.profile, view.ID)
		s.mobileCLI.mu.Unlock()
		if err != nil {
			return nil, err
		}
	} else {
		if err := s.codexConnection(record); err != nil {
			return nil, err
		}
		if thread, err = s.checkCodexOpen(input.requestContext, record, root); err != nil {
			return nil, err
		}
	}
	directory := record.GuestWorkspace
	if directory == "" {
		directory = root
	}
	if !cliWorkspaceWithin(directory, root, false) {
		return nil, errors.New("会话目录不在已授权的项目内，请重新读取会话列表")
	}
	lease := uuid.NewString()
	payload, _ := json.Marshal(map[string]any{"connection": record.Connection, "lease": lease, "thread": thread, "account": record.codexGuestAccount(),
		"subject": selection.Subject, "cwd": directory, "additional_directories": directories})
	cmd := s.sshCommand(s.lifetimeContext(), "-tt", s.sshTarget(), "--", codexSessionTerminalScript(directory, string(payload)))
	if input.requestContext != nil {
		s.account.mu.Lock()
		if !s.mobileAuthorized(input.requestContext) {
			s.account.mu.Unlock()
			return nil, errMobileLocked
		}
	}
	session, err := s.terminals.startCodexSession(cmd, workspace, "codex", selection, s.codexSessionFinished(record.ID, selection.ID))
	if err == nil {
		session.mu.Lock()
		session.sharedID = record.ID
		session.mu.Unlock()
	}
	if input.requestContext != nil {
		s.account.mu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	m := &s.mobileCLI
	m.mu.Lock()
	current, err := m.record(s.profile, record.ID)
	if err == nil {
		current.TerminalID, current.CodexLease, current.Owner = session.id, lease, "desktop"
		if err = writePrivate(mobileRecordPath(s.profile, current.ID), current); err == nil && m.catalog != nil {
			m.catalog[current.ID] = current.summary()
		}
	}
	m.mu.Unlock()
	if err != nil {
		s.terminals.remove(session.id)
		return nil, err
	}
	if input.sharedID == "" && selection.ID != "" {
		s.noteCodexSelection(selection.ID, workspace)
	}
	return session, nil
}

// checkCodexOpen asks the guest who holds a session's thread before a
// terminal resumes it, so a refusal reaches the desktop as an answer instead
// of a terminal that ends at once. It returns the thread to resume.
func (s *Server) checkCodexOpen(ctx context.Context, record *mobileRecord, root string) (string, error) {
	if ctx == nil {
		ctx = s.lifetimeContext()
	}
	var current codexConversation
	input := map[string]any{"thread": record.codexThread(), "account": record.codexGuestAccount(), "connection": record.Connection, "workspace": root, "probe": true}
	if err := s.runCodexSessions(ctx, "view", input, &current); err != nil {
		return "", err
	}
	if current.Error != "" {
		return "", errors.New(current.Error)
	}
	s.mobileCLI.noteCodexCLI(current.CLI)
	switch guestCodexOwner(current) {
	case "phone":
		return "", errCodexPhoneRunning
	case "desktop":
		return "", errCodexOtherTerminal
	case "external":
		return "", errCodexOtherProcess
	}
	thread := record.codexThread()
	if current.ThreadID != "" {
		if !validCLIUUID(current.ThreadID) || thread != "" && current.ThreadID != thread {
			return "", errors.New("会话身份已变化，请重新读取会话列表")
		}
		thread = current.ThreadID
	}
	if current.Workspace != "" && !cliWorkspaceWithin(current.Workspace, root, false) {
		return "", errors.New("Codex 会话的项目已变化，请重新读取会话列表")
	}
	if thread != "" && current.CLI != nil && current.CLI.Complete && !current.CLI.Resume {
		return "", errors.New("电脑上的 Codex 版本不支持继续会话，请更新 Codex 后重试。")
	}
	if thread != "" && len(current.Turns) == 0 && current.ConnectionNotice != "" && !current.CanSend && !current.HistoryCompressed {
		return "", errors.New(current.ConnectionNotice)
	}
	return thread, nil
}

// codexSessionTerminalScript runs the Codex helper's tui-launch from the
// guest's login shell. Every step execs, so the terminal is Codex itself: it
// ends when Codex does, its status is Codex's (or 75 when the launch was
// refused, 78 when the account was), and the lease records Codex's process.
func codexSessionTerminalScript(directory, payload string) string {
	body := "exec " + codexSessionsInvocation("tui-launch", payload)
	return "cd " + shellescape.Quote(directory) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 bash -lic " + shellescape.Quote(body)
}

func (s *Server) codexSessionFinished(id, account string) func(*terminalSession) {
	started := time.Now()
	return func(session *terminalSession) {
		// Codex's own failures end this terminal too; only 78 is the account
		// helper refusing the account.
		if account != "" && session.exitStatus() == 78 && time.Since(started) < codexQuickExit {
			s.recheckCodexAccount(account)
		}
		s.mobileCLI.forgetCodexDesktop(id)
	}
}

// codexQuickExit bounds how soon after start a failed exit is taken as the
// guest helper refusing the account rather than Codex ending normally.
const codexQuickExit = 20 * time.Second

func (s *Server) codexTerminalFinished(id string, login bool) func(*terminalSession) {
	if login {
		return func(*terminalSession) {
			ctx, cancel := context.WithTimeout(s.lifetimeContext(), 25*time.Second)
			defer cancel()
			_ = s.confirmCodexLogin(ctx, id)
		}
	}
	started := time.Now()
	return func(session *terminalSession) {
		// The helper ends the terminal when the account is no longer signed in
		// as selected. Re-check it in the background so allocation stops
		// choosing it; the terminal's teardown never waits for the probe.
		if session.exitStatus() > 0 && time.Since(started) < codexQuickExit {
			s.recheckCodexAccount(id)
		}
	}
}

// The saved command is interpreted only by the guest's interactive shell and
// leaves a shell when Codex exits. With a managed account, the shell keeps
// CODEX_HOME and the shim, so later codex invocations stay on that account.
// When the helper refuses the account (status 78), the terminal ends instead,
// so the host learns that the account needs to be checked.
func codexTerminalScript(workspace, command, accountID, subject string, login bool) string {
	if login {
		return "cd \"$HOME\" && exec /usr/local/bin/cage-codex-account login " + shellescape.Quote(accountID)
	}
	body := `eval "$CAGE_TERMINAL_COMMAND"; unset CAGE_TERMINAL_COMMAND; exec bash -il`
	if accountID != "" {
		body = `export CAGE_CODEX_ACCOUNT_ID=` + shellescape.Quote(accountID) + ` CAGE_CODEX_SUBJECT=` + shellescape.Quote(subject) + "; " +
			`export CODEX_HOME="$HOME/.local/share/cage/codex-accounts/$CAGE_CODEX_ACCOUNT_ID/home"; (umask 077; mkdir -p "$CODEX_HOME"); ` +
			`export PATH="/usr/local/lib/cage-codex-bin:$PATH"; eval "$CAGE_TERMINAL_COMMAND"; cage_status=$?; unset CAGE_TERMINAL_COMMAND; ` +
			`if [ "$cage_status" = 78 ]; then exit 78; fi; unset cage_status; exec bash -i`
	}
	return "cd " + shellescape.Quote(workspace) +
		" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 CAGE_TERMINAL_COMMAND=" +
		shellescape.Quote(command) + " bash -lic " + shellescape.Quote(body)
}
