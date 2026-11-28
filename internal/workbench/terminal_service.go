package workbench

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
)

var (
	errTerminalsClosed  = errors.New("后台正在退出，请重新打开 Super Cage。")
	errTerminalBusy     = errors.New("环境正在执行其他操作")
	errTerminalNotReady = errors.New("环境尚未就绪或正在重启")
	errTerminalLimit    = errors.New("最多保留 12 个终端，请先关闭不用的会话")
	errTerminalStart    = errors.New("无法创建终端连接")
)

type terminalRequest struct {
	Workspace       string `json:"workspace"`
	Kind            string `json:"kind"`
	ClaudeAccountID string `json:"claude_account_id"`
	CodexAccountID  string `json:"codex_account_id"`
	AgentID         string `json:"agent_id,omitempty"`
	AccountID       string `json:"account_id,omitempty"`
	LoginMode       string `json:"login_mode,omitempty"`
	sharedID        string
	recoverTerminal string
	recoverCLIID    string
	// codexSession: the client knows Codex sessions, so a Codex terminal from
	// the menu becomes one.
	codexSession bool
	// nativeCodexCommand is set only when a structured launch has already
	// checked this exact non-structured route and found it Magpie-owned.
	nativeCodexCommand string
	// codexProbed: the guest answered the Codex CLI probe completely just
	// before the operation lock (warmCodexProbe), so the open's check under
	// the lock may ask for it again: the guest answers from its cache.
	codexProbed    bool
	requestContext context.Context
}

// terminalManager owns host PTYs; subscriptions only borrow their output. The
// manager serializes process creation with closure so no process can be
// published after shutdown has collected the sessions it needs to close.
type terminalManager struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession
	sequence int
	closed   bool
	workers  sync.WaitGroup
	// Retain a login reservation while its closing callback verifies the
	// guest profile, even if the ended terminal has already been removed.
	agentLoginReservations map[agentLoginReservation]struct{}
	// kept are the ssh terminals kept past their connection (entryKeep),
	// which have their own cap instead of the shared limit.
	kept map[string]bool
	// imported are the viewers of hc terminals another Herdr client started
	// (herdr_terminals.go): their own cap too, so terminals the user opens
	// from another client never block new Cage terminals.
	imported map[string]bool
}

type agentLoginReservation struct {
	agentID   string
	accountID string
}

func (m *terminalManager) startSession(cmd *exec.Cmd, workspace, kind string, selection claudeSelection, finished func()) (*terminalSession, error) {
	return m.startSessionWithSetup(cmd, workspace, kind, selection, finished, func() (terminalPTY, error) { return startTerminal(cmd, 100, 30) }, nil)
}

func (m *terminalManager) startSessionWithPTY(cmd *exec.Cmd, workspace, kind string, selection claudeSelection, finished func(), start func() (terminalPTY, error)) (*terminalSession, error) {
	return m.startSessionWithSetup(cmd, workspace, kind, selection, finished, start, nil)
}

func (m *terminalManager) startSessionWithSetup(cmd *exec.Cmd, workspace, kind string, selection claudeSelection, finished func(), start func() (terminalPTY, error), setup func(*terminalSession, int)) (*terminalSession, error) {
	var ended func(*terminalSession)
	if finished != nil {
		ended = func(*terminalSession) { finished() }
	}
	return m.startSessionWith(cmd, workspace, kind, selection, codexSelection{}, ended, start, setup)
}

// finished receives the ended session, so it can read the exit status.
func (m *terminalManager) startCodexSession(cmd *exec.Cmd, workspace, kind string, selection codexSelection, finished func(*terminalSession)) (*terminalSession, error) {
	return m.startCodexSessionWithSetup(cmd, workspace, kind, selection, finished, nil)
}

func (m *terminalManager) startCodexSessionWithSetup(cmd *exec.Cmd, workspace, kind string, selection codexSelection, finished func(*terminalSession), setup func(*terminalSession, int)) (*terminalSession, error) {
	return m.startSessionWith(cmd, workspace, kind, claudeSelection{}, selection, finished, func() (terminalPTY, error) { return startTerminal(cmd, 100, 30) }, setup)
}

// Account bindings are published together with the session, so a concurrent
// allocation always counts a terminal that is already running.
func (m *terminalManager) startSessionWith(cmd *exec.Cmd, workspace, kind string, selection claudeSelection, codex codexSelection, finished func(*terminalSession), start func() (terminalPTY, error), setup func(*terminalSession, int)) (*terminalSession, error) {
	return m.startSessionAdmitted(false, cmd, workspace, kind, selection, codex, finished, start, setup)
}

// herdrViewerPTY starts a Herdr viewer's PTY; tests replace it.
var herdrViewerPTY = startTerminal

// startHerdrViewer starts a viewer of an hc terminal (herdr_terminals.go).
// One another client started (imported) counts against its own cap.
func (m *terminalManager) startHerdrViewer(cmd *exec.Cmd, workspace, kind string, imported bool, setup func(*terminalSession, int)) (*terminalSession, error) {
	return m.startSessionAdmitted(imported, cmd, workspace, kind, claudeSelection{}, codexSelection{}, nil, func() (terminalPTY, error) { return herdrViewerPTY(cmd, 100, 30) }, setup)
}

func (m *terminalManager) startSessionAdmitted(imported bool, cmd *exec.Cmd, workspace, kind string, selection claudeSelection, codex codexSelection, finished func(*terminalSession), start func() (terminalPTY, error), setup func(*terminalSession, int)) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	admission := m.admissionErrorLocked
	if imported {
		admission = m.importedAdmissionLocked
	}
	if err := admission(); err != nil {
		return nil, err
	}
	file, err := start()
	if err != nil {
		return nil, errTerminalStart
	}
	m.sequence++
	title := fmt.Sprintf("终端 %d", m.sequence)
	if kind == "claude" {
		title = fmt.Sprintf("Claude %d", m.sequence)
	} else if kind == "claude-login" {
		title = "Claude 账号登录"
	} else if kind == "codex" {
		title = fmt.Sprintf("Codex %d", m.sequence)
	} else if kind == "codex-login" {
		title = "Codex 账号登录"
	} else if kind == "herdr" {
		title = "Herdr"
	} else if kind == "magpie-tui" {
		title = "Magpie TUI"
	}
	session := newTerminalSession(file)
	session.id, session.title, session.workspace, session.cmd = randomToken()[:16], title, workspace, cmd
	session.sequence = m.sequence
	session.kind, session.claudeAccountID = kind, selection.ID
	if kind == "claude-login" {
		session.loginGuestID = selection.GuestID
	}
	if selection.GuestID == "default" {
		session.claudeAccountSource = "default"
	} else if selection.ID != "" {
		session.claudeAccountSource = "managed"
	}
	if codex.ID != "" {
		session.codexAccountID, session.codexAccountSource = codex.ID, "managed"
	}
	if setup != nil {
		setup(session, m.sequence)
	}
	if m.sessions == nil {
		m.sessions = make(map[string]*terminalSession)
	}
	m.sessions[session.id] = session
	if imported {
		if m.imported == nil {
			m.imported = make(map[string]bool)
		}
		m.imported[session.id] = true
	}
	key := agentLoginReservation{agentID: session.agentID, accountID: session.accountID}
	reservedLogin := kind == "agent-login" && finished != nil && key.agentID != "" && key.accountID != ""
	if reservedLogin {
		if m.agentLoginReservations == nil {
			m.agentLoginReservations = make(map[agentLoginReservation]struct{})
		}
		m.agentLoginReservations[key] = struct{}{}
	}
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		if reservedLogin {
			defer func() {
				m.mu.Lock()
				delete(m.agentLoginReservations, key)
				m.mu.Unlock()
			}()
		}
		session.run()
		if finished != nil {
			finished(session)
		}
	}()
	return session, nil
}

func (m *terminalManager) admissionErrorLocked() error {
	if m.closed {
		return errTerminalsClosed
	}
	if len(m.sessions)-len(m.kept)-len(m.imported) >= 12 {
		return errTerminalLimit
	}
	return nil
}

func (m *terminalManager) importedAdmissionLocked() error {
	if m.closed {
		return errTerminalsClosed
	}
	if len(m.imported) >= herdrImportedLimit {
		return errHerdrTerminalLimit
	}
	return nil
}

func (m *terminalManager) canStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admissionErrorLocked()
}

func (m *terminalManager) snapshot() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.sessions))
	for _, session := range m.sessions {
		out = append(out, session.summary())
	}
	return out
}

func (m *terminalManager) hasLive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		session.mu.Lock()
		closed := session.closed
		session.mu.Unlock()
		if !closed {
			return true
		}
		if session.done != nil {
			select {
			case <-session.done:
			default:
				return true
			}
		}
	}
	return false
}

func (m *terminalManager) get(id string) *terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// boundCodex is the live Codex terminal of this server bound to a session.
// While it runs, the desktop owns the session's thread.
func (m *terminalManager) boundCodex(id string) *terminalSession {
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		session.mu.Lock()
		bound := session.kind == "codex" && session.sharedID == id && !session.closed
		session.mu.Unlock()
		if bound {
			return session
		}
	}
	return nil
}

// removeEndedCodex removes ended Codex terminals of a session before a new
// one opens, so they do not count toward the terminal limit.
func (m *terminalManager) removeEndedCodex(id string) {
	m.mu.Lock()
	var ended []string
	for key, session := range m.sessions {
		session.mu.Lock()
		if session.kind == "codex" && session.sharedID == id && session.closed {
			ended = append(ended, key)
		}
		session.mu.Unlock()
	}
	m.mu.Unlock()
	for _, key := range ended {
		m.remove(key)
	}
}

func (m *terminalManager) forConversation(connection string) *terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		session.mu.Lock()
		matches := session.connectionID == connection && !session.closed
		session.mu.Unlock()
		if matches {
			return session
		}
	}
	return nil
}

// conversationOwners maps each open terminal's channel connection to the
// session it shows.
func (m *terminalManager) conversationOwners() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	owners := map[string]string{}
	for _, session := range m.sessions {
		session.mu.Lock()
		if session.connectionID != "" && session.sharedID != "" && !session.closed {
			owners[session.connectionID] = session.sharedID
		}
		session.mu.Unlock()
	}
	return owners
}

// boundTerminal is the terminal a record names, only while that terminal still
// shows the record's conversation: a /resume of another conversation inside
// the CLI moves the terminal to that conversation's record.
func (s *Server) boundTerminal(record *mobileRecord) *terminalSession {
	terminal := s.terminals.get(record.TerminalID)
	if terminal == nil {
		return nil
	}
	terminal.mu.Lock()
	shared := terminal.sharedID
	terminal.mu.Unlock()
	if shared != "" && shared != record.ID {
		return nil
	}
	return terminal
}

// Reattaching a repeated login request must not create a second OAuth flow.
// A profile can serve many normal terminals or a single exclusive login.
func (m *terminalManager) existingClaudeLogin(id string, login bool) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.claudeAccountID != id {
			continue
		}
		select {
		case <-session.done:
			continue
		default:
		}
		if session.kind == "claude-login" && login {
			return session, nil
		}
		if login || session.kind == "claude-login" {
			return nil, errors.New("请先结束此账号的登录或运行中的终端，再更改登录身份")
		}
	}
	return nil, nil
}

// A Codex account directory serves many terminals or a single device login.
func (m *terminalManager) existingCodexLogin(id string, login bool) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.codexAccountID != id {
			continue
		}
		select {
		case <-session.done:
			continue
		default:
		}
		if session.kind == "codex-login" && login {
			return session, nil
		}
		if login || session.kind == "codex-login" {
			return nil, errors.New("请先结束此 Codex 账号的登录或运行中的终端，再更改登录身份")
		}
	}
	return nil, nil
}

func (m *terminalManager) allAgentCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := map[string]int{}
	for _, session := range m.sessions {
		select {
		case <-session.done:
			continue
		default:
		}
		if session.agentID != "" {
			counts[session.agentID]++
		} else if session.kind == "claude" {
			counts["claude"]++
		} else if session.kind == "codex" {
			counts["codex"]++
		}
	}
	return counts
}

func (m *terminalManager) agentCounts(agentID string) (map[string]int, map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	active := map[string]int{}
	logins := map[string]bool{}
	for _, session := range m.sessions {
		if session.agentID != agentID {
			continue
		}
		select {
		case <-session.done:
			continue
		default:
		}
		if session.kind == "agent-login" {
			if session.accountID != "" {
				logins[session.accountID] = true
			}
		} else if session.kind == "agent" || session.accountID != "" {
			active[session.accountID]++
		}
	}
	for key := range m.agentLoginReservations {
		if key.agentID == agentID {
			logins[key.accountID] = true
		}
	}
	return active, logins
}

func (m *terminalManager) liveAgentLogin(agentID, accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.kind == "agent-login" && session.agentID == agentID && session.accountID == accountID {
			select {
			case <-session.done:
			default:
				return true
			}
		}
	}
	return false
}

func (m *terminalManager) existingAgentLogin(agentID, accountID string, login bool) (*terminalSession, error) {
	if agentID == "" || accountID == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.agentID != agentID || session.accountID != accountID {
			continue
		}
		select {
		case <-session.done:
			continue
		default:
		}
		if session.kind == "agent-login" && login {
			return session, nil
		}
		if login || session.kind == "agent-login" {
			return nil, errors.New("请先结束此账号的登录或运行中的终端，再更改登录身份")
		}
	}
	if _, settling := m.agentLoginReservations[agentLoginReservation{agentID: agentID, accountID: accountID}]; settling {
		return nil, errors.New("正在确认上一次登录，请稍后重试")
	}
	return nil, nil
}

func (m *terminalManager) remove(id string) {
	m.mu.Lock()
	session := m.sessions[id]
	m.mu.Unlock()
	if session != nil {
		// Keep its Claude account reserved until the old PTY has exited.
		session.close()
		m.mu.Lock()
		delete(m.sessions, id)
		delete(m.kept, id)
		delete(m.imported, id)
		m.mu.Unlock()
	}
}

// closeAll keeps ended sessions visible until explicitly removed and permits
// new terminals after the environment starts again. Environment operations
// serialize this call with createTerminal using Server.op.
func (m *terminalManager) closeAll() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}

func (m *terminalManager) close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.closeAll()
	// The final login-status observation also belongs to the session worker.
	m.workers.Wait()
}

func (s *Server) createTerminal(input terminalRequest) (*terminalSession, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if !s.op.TryLock() {
			return nil, errTerminalBusy
		}
		session, err := s.createTerminalLocked(input)
		s.op.Unlock()
		if !errors.Is(err, errEnsureAgentRuntimeNeeded) {
			return session, err
		}
		ensureCtx, ensureCancel := context.WithTimeout(s.lifetimeContext(), coldEnsureTimeout)
		ensureErr := s.ensureAgentRuntime(ensureCtx)
		ensureCancel()
		if ensureErr != nil {
			if input.Kind == "codex" || input.Kind == "codex-login" {
				return nil, errors.New("无法在运行环境中准备 Herdr 辅助程序，请确认环境已启动")
			}
			return nil, errors.New("无法在运行环境中准备 Agent 辅助程序，请确认环境已启动")
		}
	}
	return nil, errors.New("无法在运行环境中准备 Agent 辅助程序，请确认环境已启动")
}

// Caller holds the environment operation lock through PTY publication.
func (s *Server) createTerminalLocked(input terminalRequest) (*terminalSession, error) {
	// The Agent CLI catalog uses the same native Claude account and launch
	// rules as the legacy terminal kind on every guest.
	if input.AgentID == "claude" && (input.Kind == "agent" || input.Kind == "agent-login") {
		if input.AccountID != "" {
			if input.ClaudeAccountID != "" && input.ClaudeAccountID != input.AccountID {
				return nil, errors.New("Claude 账号参数不一致")
			}
			input.ClaudeAccountID = input.AccountID
		}
		if input.LoginMode != "" && (input.Kind != "agent-login" || input.LoginMode != "oauth") {
			return nil, errors.New("Claude 终端不支持指定的登录方式")
		}
		if input.Kind == "agent-login" {
			input.Kind = "claude-login"
		} else {
			input.Kind = "claude"
		}
		input.AgentID, input.AccountID, input.LoginMode = "", "", ""
	}
	if input.Kind == "codex" || input.Kind == "codex-login" {
		return s.createCodexTerminalLocked(input)
	}
	if input.CodexAccountID != "" {
		return nil, errors.New("只有 Codex 终端可以指定 Codex 账号")
	}
	if input.recoverTerminal != "" {
		s.mobileCLI.mu.Lock()
		record, err := s.mobileCLI.record(s.profile, input.sharedID)
		s.mobileCLI.mu.Unlock()
		if err != nil || !record.canRecoverTerminal(input.recoverTerminal) || record.codex() ||
			!record.wasTerminalRunning() || !record.Resume || !validCLIUUID(input.recoverCLIID) ||
			record.CLIID != input.recoverCLIID || !record.canResumeClaudeWithoutInput() {
			return nil, errTerminalRecoveryUnavailable
		}
	}
	if s.windowsGuest != nil {
		return s.createWindowsTerminal(input)
	}
	var sharedRecord *mobileRecord
	if input.sharedID != "" {
		s.mobileCLI.mu.Lock()
		record, err := s.mobileCLI.record(s.profile, input.sharedID)
		s.mobileCLI.mu.Unlock()
		if err != nil {
			return nil, err
		}
		sharedRecord = record
		if record.codex() {
			return nil, errors.New("该会话需要通过 Codex 终端打开")
		}
		if record.Switch != nil && record.Switch.Phase == "preparing" {
			return nil, errors.New("账号交接尚未完成，请先重试或取消交接")
		}
		if record.Archived || record.Deleted {
			return nil, errors.New("此会话已归档或删除，请先从已归档会话恢复")
		}
		// The guest can be unreadable or not yet started while its host PTY
		// already owns this conversation. A stale snapshot cannot start a copy.
		if existing := s.boundTerminal(record); existing != nil && existing.ownsConversation() {
			return existing, nil
		}
		if existing := s.boundTerminal(record); existing != nil && existing.summary()["closed"] == true {
			s.terminals.remove(record.TerminalID)
		}
	}
	s.mu.Lock()
	closing, sealed := s.closing, s.appUpdateSealed
	componentUpdate := s.guestUpdateRequired
	command := claudeTerminalCommand(s.preferences)
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if sealed {
		return nil, errAppUpdateSealed
	}
	if sharedRecord != nil && sharedRecord.Launcher != "" {
		command = sharedRecord.Launcher
	}
	if sharedRecord != nil && !sharedCLILauncher(command) {
		return nil, errors.New("原会话的 Claude 启动命令不支持共享终端")
	}
	if s.macosGuest != nil {
		if input.Kind == "claude" && !sharedCLILauncher(command) {
			return nil, errors.New("macOS 客机支持 claude 或 ccs 配置名启动命令")
		}
		s.mu.Lock()
		ready := s.ready
		s.mu.Unlock()
		if !ready {
			return nil, errTerminalNotReady
		}
	}
	if !s.vmRunning() || s.busy() {
		return nil, errTerminalNotReady
	}
	// Reject a full manager before opening project shares. start checks again
	// atomically with process publication because shutdown can race this work.
	if input.Kind != "claude-login" && input.Kind != "agent-login" {
		if err := s.terminals.canStart(); err != nil {
			return nil, err
		}
	}
	if input.Kind != "" && input.Kind != "shell" && input.Kind != "claude" && input.Kind != "claude-login" && input.Kind != "agent" && input.Kind != "agent-login" && input.Kind != "herdr" && input.Kind != "magpie-tui" {
		return nil, errors.New("终端类型无效")
	}
	isAgentKind := input.Kind == "agent" || input.Kind == "agent-login"
	managed := input.Kind == "claude" || input.Kind == "claude-login"
	if !managed && input.ClaudeAccountID != "" {
		return nil, errors.New("普通终端不能指定 Claude 账号")
	}
	// Login works even when the last project is disconnected or has moved.
	// It always runs from the guest home, without loading project settings.
	workspace := s.workspace()
	var err error
	var selection claudeSelection
	selected := false // selection was made together with the project connection
	if input.Kind != "claude-login" && input.Kind != "agent-login" {
		if s.macOSLegacyWorkspace(sharedRecord) != "" {
			workspace, err = s.projectWorkspace(sharedRecord.Workspace)
		} else if managed && (sharedRecord == nil || !sharedRecord.Terminal) {
			// Opening the project's shares and choosing an account both wait on
			// guest reads, and neither needs the other's result.
			var connect func() error
			if workspace, connect, err = s.prepareProject(input.Workspace, false); err == nil {
				selection, err = connectAndSelect(connect, func() (claudeSelection, error) {
					return s.selectTerminalClaudeAccount(command, input.ClaudeAccountID, false, workspace)
				})
				selected = true
			}
		} else {
			workspace, err = s.connectProject(input.Workspace, false)
		}
		if err != nil {
			return nil, err
		}
	}
	herdrTakeover := s.shouldTakeoverWithHerdr()
	launchCommand := command
	if input.Kind == "claude" && (!sharedCLILauncher(command) || (input.sharedID == "" && herdrTakeover)) {
		directories, err := s.projectAdditionalDirectories(workspace)
		if err != nil {
			return nil, err
		}
		for _, directory := range directories {
			launchCommand += " --add-dir " + shellescape.Quote(directory)
		}
	}
	script := terminalScript(s.cliWorkspace(workspace), input.Kind, launchCommand)
	if s.macosGuest != nil {
		script = `cd -- "$HOME" && exec env TERM=xterm-256color COLORTERM=truecolor /bin/zsh -l`
		if input.Kind != "claude-login" && input.Kind != "agent-login" {
			script = "exec /usr/local/libexec/cage-macos-terminal " + shellescape.Quote(macOSProjectPath(s.macosGuest.User, workspace))
		}
	}
	var agentSel agentSelection
	var agentSpecName string
	var agentDomains []string
	var finished func()
	if isAgentKind {
		spec, cfg, ok := s.lookupAgentSpec(input.AgentID)
		if !ok {
			return nil, errors.New("未找到指定的 Agent CLI，请在「设置 → Agent CLI」中确认")
		}
		agentSpecName = spec.ShortName
		agentDomains = spec.Auth.OAuthDomains
		login := input.Kind == "agent-login"
		sel, _, selErr := s.selectAgentAccount(spec, input.AccountID, login, workspace)
		if selErr != nil {
			return nil, selErr
		}
		if sel.ID != "" {
			if existing, conflict := s.terminals.existingAgentLogin(spec.ID, sel.ID, login); existing != nil || conflict != nil {
				return existing, conflict
			}
		}
		agentSel = sel
		agentCmd := s.effectiveAgentLaunchCommand(spec, cfg)
		if !login && spec.Terminal.AddDirFlag != "" {
			if dirs, dirErr := s.projectAdditionalDirectories(workspace); dirErr == nil {
				for _, directory := range dirs {
					agentCmd += " " + spec.Terminal.AddDirFlag + " " + shellescape.Quote(directory)
				}
			}
		}
		if login && s.macosGuest != nil {
			ctx, cancel := context.WithTimeout(s.lifetimeContext(), 20*time.Second)
			_, browserErr := s.guestAPI(ctx, "", "GET", "/health", nil)
			cancel()
			if browserErr != nil {
				return nil, errors.New("客机登录浏览器尚未就绪，请恢复浏览器连接后重试")
			}
		}
		agentWorkspace := s.cliWorkspace(workspace)
		if s.macosGuest != nil && !login {
			agentWorkspace = macOSProjectPath(s.macosGuest.User, workspace)
		}
		script = managedAgentTerminalScript(agentWorkspace, spec, agentCmd, sel.GuestID, input.LoginMode, login, s.macosGuest != nil)
		if login {
			finished = func() {
				ctx, cancel := context.WithTimeout(s.lifetimeContext(), 12*time.Second)
				defer cancel()
				_ = s.confirmAgentLogin(ctx, spec.ID, sel.ID)
			}
		}
	}
	if managed {
		login := input.Kind == "claude-login"
		if sharedRecord != nil && sharedRecord.Terminal {
			selection = sharedRecord.Account
			if selection.ID != "" {
				s.claudeAccounts.mu.Lock()
				list, loadErr := s.loadClaudeAccounts()
				s.claudeAccounts.mu.Unlock()
				if loadErr != nil {
					return nil, loadErr
				}
				err = s.validateBoundClaudeAccount(list, selection)
			}
		} else if !selected {
			selection, err = s.selectTerminalClaudeAccount(command, input.ClaudeAccountID, login, workspace)
		}
		if err != nil {
			return nil, err
		}
		if selection.ID != "" {
			if componentUpdate {
				return nil, errGuestUpdateRequired
			}
			if !validClaudeID(selection.ID) || !validClaudeGuestID(selection.GuestID) || (login && selection.GuestID == "default") {
				return nil, errors.New("Claude 账号 ID 无效")
			}
			if login {
				if active, _ := s.claudeAccountLoad(); active[selection.ID] > 0 {
					return nil, errors.New("请先结束该账号正在运行的会话，再重新登录")
				}
			}
			if existing, conflict := s.terminals.existingClaudeLogin(selection.ID, login); existing != nil || conflict != nil {
				return existing, conflict
			}
			if !login && selection.Subject == "" {
				return nil, errors.New("Claude 账号状态已变化，请重试")
			}
			script = managedClaudeTerminalScript(s.cliWorkspace(workspace), launchCommand, selection.GuestID, selection.Subject, login)
			if login {
				if s.macosGuest != nil {
					ctx, cancel := context.WithTimeout(s.lifetimeContext(), 20*time.Second)
					_, browserErr := s.guestAPI(ctx, "", "GET", "/health", nil)
					cancel()
					if browserErr != nil {
						return nil, errors.New("客机登录浏览器尚未就绪，请恢复浏览器连接后重试")
					}
				}
				finished = func() {
					ctx, cancel := context.WithTimeout(s.lifetimeContext(), 12*time.Second)
					defer cancel()
					_ = s.confirmClaudeLogin(ctx, selection.ID)
				}
			}
		}
	}
	var conversation *mobileRecord
	// When Herdr takeover is active for a freshly created Claude terminal
	// (not resuming an existing mobile/history session), launch Claude directly
	// inside Herdr instead of binding a mobile conversation channel whose host
	// lifecycle would desync on Herdr detach.
	if input.Kind == "claude" && sharedCLILauncher(command) && !(input.sharedID == "" && herdrTakeover) {
		script, conversation, err = s.prepareTerminalConversation(workspace, selection, input.sharedID)
		if err != nil {
			return nil, err
		}
	}
	guestWorkspace := s.cliWorkspace(workspace)
	if s.macosGuest != nil && input.Kind != "claude-login" && input.Kind != "agent-login" {
		guestWorkspace = macOSProjectPath(s.macosGuest.User, workspace)
	}
	// With Herdr the terminal is an hc terminal named cage-<its id>, which
	// it views; the "herdr" kind opens hc's own picker.
	herdrBacked := false
	herdrID, herdrKind, herdrTitle := "", "", ""
	projectLabel := herdrProjectLabel(workspace)
	switch input.Kind {
	case "herdr":
		script = s.herdrAttachScript(herdrAttach{Mode: "picker", Workspace: guestWorkspace, Project: projectLabel, Title: "Herdr", Kind: "herdr"})
		herdrBacked = true
	case "magpie-tui":
		script = s.magpieTUIScript(guestWorkspace)
		if herdrTakeover {
			herdrKind, herdrTitle = "magpie-tui", "Magpie TUI"
		}
	case "", "shell", "claude", "agent":
		if herdrTakeover && conversation == nil {
			herdrKind, herdrTitle = input.Kind, "Shell"
			if herdrKind == "" {
				herdrKind = "shell"
			}
			switch herdrKind {
			case "claude":
				herdrTitle = "Claude"
			case "agent":
				herdrTitle = agentSpecName
				if herdrTitle == "" {
					herdrTitle = input.AgentID
				}
			}
		}
	}
	if herdrKind != "" {
		herdrID = randomToken()[:16]
		script = s.herdrAttachScript(herdrAttach{Mode: "create", Name: herdrTerminalName(herdrID), Workspace: guestWorkspace,
			Project: projectLabel, Title: herdrTitle, Kind: herdrKind, Inner: script})
		herdrBacked = true
	}
	if herdrBacked || (isAgentKind && (input.Kind == "agent-login" || (agentSel.GuestID != "" && agentSel.GuestID != "default"))) {
		if !s.agentRuntimeInstalled() {
			return nil, errEnsureAgentRuntimeNeeded
		}
	}
	if selection.ID != "" {
		settle := finished
		finished = func() {
			defer s.kickClaudeLaunchSchedule()
			if settle != nil {
				settle()
			}
		}
	}
	if agentSel.ID != "" {
		settle := finished
		finished = func() {
			defer s.kickAgentLaunchSchedule()
			if settle != nil {
				settle()
			}
		}
	}
	cmd := s.sshCommand(s.lifetimeContext(), "-tt", s.sshTarget(), "--", script)
	if input.requestContext != nil {
		s.account.mu.Lock()
		if !s.mobileAuthorized(input.requestContext) {
			s.account.mu.Unlock()
			return nil, errMobileLocked
		}
	}
	var setup func(*terminalSession, int)
	if isAgentKind || herdrBacked || input.recoverTerminal != "" {
		setup = func(session *terminalSession, seq int) {
			input.setupRecoveredTerminal(session, seq)
			session.herdrBacked = herdrBacked
			if herdrID != "" {
				session.id, session.herdrName = herdrID, herdrTerminalName(herdrID)
			}
			if isAgentKind {
				session.agentID = agentSel.AgentID
				session.agentName = agentSpecName
				session.accountID = agentSel.ID
				session.accountLabel = agentSel.Label
				session.accountAuthMode = agentSel.AuthMode
				session.scheduleReason = agentSel.Reason
				session.agentOAuthDomains = agentDomains
				if input.Kind == "agent-login" {
					session.title = agentSpecName + " 账号登录"
					session.loginGuestID = agentSel.GuestID
				} else {
					session.title = fmt.Sprintf("%s %d", agentSpecName, seq)
					if agentSel.ID != "" {
						aid, accID := agentSel.AgentID, agentSel.ID
						session.onRateLimit = func(kind string) {
							s.onAgentRateLimit(aid, accID, kind)
						}
					}
				}
			}
		}
	}
	session, err := s.terminals.startSessionWithSetup(cmd, workspace, input.Kind, selection, finished, func() (terminalPTY, error) { return startTerminal(cmd, 100, 30) }, setup)
	if input.requestContext != nil {
		s.account.mu.Unlock()
	}
	if err == nil && isAgentKind && input.Kind == "agent" && agentSel.ID != "" {
		s.noteAgentSelection(agentSel.AgentID, agentSel.ID, workspace)
	}
	if err == nil && agentSel.ID != "" {
		s.kickAgentLaunchSchedule()
	}
	if err == nil && herdrID != "" {
		s.rememberHerdrSession(session, herdrKind)
	}
	if err == nil && input.Kind == "herdr" {
		_ = s.noteWorkbenchHerdrUse()
	}
	if err == nil && conversation != nil {
		session.mu.Lock()
		session.sharedID = conversation.ID
		session.connectionID = conversation.Connection
		session.mu.Unlock()
		s.mobileCLI.mu.Lock()
		conversation.TerminalID = session.id
		if saveErr := writePrivate(mobileRecordPath(s.profile, conversation.ID), conversation); saveErr != nil {
			err = saveErr
		}
		s.mobileCLI.mu.Unlock()
		if err != nil {
			s.terminals.remove(session.id)
			return nil, err
		}
		s.prepareCLIConnection(session, conversation.ID)
		s.monitorClaudeAccount(session, conversation.ID)
	}
	if err == nil && input.Kind == "claude" && selection.ID != "" && (sharedRecord == nil || !sharedRecord.Terminal) {
		s.noteClaudeSelection(selection.ID, workspace)
	}
	if err == nil && selection.ID != "" {
		s.kickClaudeLaunchSchedule()
	}
	return session, err
}

// connectAndSelect runs the project connection and the account selection at
// the same time and returns when both are done, so the caller still holds the
// environment lock until neither is running. A project error wins over a
// selection error, as it did when they ran one after the other.
func connectAndSelect(connect func() error, selectAccount func() (claudeSelection, error)) (claudeSelection, error) {
	connected := make(chan error, 1)
	go func() { connected <- connect() }()
	selection, selectErr := selectAccount()
	if err := <-connected; err != nil {
		return claudeSelection{}, err
	}
	return selection, selectErr
}

func validClaudeGuestID(id string) bool { return id == "default" || validClaudeID(id) }

func (s *Server) selectTerminalClaudeAccount(command, requested string, login bool, workspace string) (claudeSelection, error) {
	parts, valid := mobileLaunchArgs(command, claudeSelection{})
	if !login && valid == nil && len(parts) == 2 && parts[0] == "ccs" {
		if requested != "" && requested != "auto" {
			return claudeSelection{}, errors.New("CCS 使用自己的配置，不能指定 Super Cage 账号")
		}
		return claudeSelection{}, nil
	}
	if !login && (requested == "" || requested == "auto") && s.magpieNativeAutoEligible("claude") {
		if native, err := s.magpieNewLaunch("claude", command, workspace); native || err != nil {
			return claudeSelection{}, err
		}
	}
	return s.selectClaudeAccount(requested, login, workspace)
}

func (s *Server) readClaudeGuestSnapshot(ctx context.Context, id string) (claudeGuestSnapshot, error) {
	if s.windowsGuest != nil {
		return s.readWindowsClaudeAccount(ctx, "snapshot", id, "")
	}
	// A snapshot only reads, and the accounts pages and every launch ask for
	// it, so it shares the guest's control connection.
	return s.readClaudeGuestReportVia(ctx, id, claudeSnapshotCommand(id), s.guestQuick)
}

func (s *Server) readClaudeGuestUsage(ctx context.Context, id, subject string) (claudeGuestSnapshot, error) {
	if _, err := hex.DecodeString(subject); err != nil || len(subject) != 64 {
		return claudeGuestSnapshot{}, errors.New("Claude 账号身份无效")
	}
	if s.windowsGuest != nil {
		return s.readWindowsClaudeAccount(ctx, "usage", id, subject)
	}
	return s.readClaudeGuestReport(ctx, id, claudeGuestReportCommand(id, subject))
}

// readClaudeGuestReport runs command over a connection of its own. Usage and
// login recovery use it: they can run long or change the account, which a
// shared connection is not for.
func (s *Server) readClaudeGuestReport(ctx context.Context, id, command string) (claudeGuestSnapshot, error) {
	return s.readClaudeGuestReportVia(ctx, id, command, s.guest)
}

func (s *Server) readClaudeGuestReportVia(ctx context.Context, id, command string, run func(context.Context, string) ([]byte, error)) (claudeGuestSnapshot, error) {
	var snapshot claudeGuestSnapshot
	if !validClaudeGuestID(id) {
		return snapshot, errors.New("Claude 账号 ID 无效")
	}
	data, err := run(ctx, command)
	if err != nil || json.Unmarshal(data, &snapshot) != nil {
		return snapshot, errors.New("无法读取账号状态，请确认环境已启动并同步环境组件")
	}
	return snapshot, nil
}

func claudeSnapshotCommand(id string) string {
	return claudeGuestReportCommand(id, "")
}

func claudeGuestReportCommand(id, subject string) string {
	command := "exec /usr/local/bin/cage-claude-account snapshot " + id
	if subject != "" {
		command = "exec /usr/local/bin/cage-claude-account usage " + id + " " + shellescape.Quote(subject)
	}
	if id == "default" {
		// Use the same login shell a regular terminal opens, including its
		// configured API/provider environment. Isolate startup banners from
		// the JSON output and keep the probe outside every shared project.
		command = `set -o pipefail; cd "$HOME" && env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 /bin/bash -lic ` +
			shellescape.Quote(`printf '\nCAGE_CLAUDE_SNAPSHOT '; `+command) +
			` | sed -n 's/^CAGE_CLAUDE_SNAPSHOT //p'`
	}
	return command
}

func managedClaudeTerminalScript(workspace, command, accountID, subject string, login bool) string {
	if login {
		return "cd \"$HOME\" && exec /usr/local/bin/cage-claude-account login " + shellescape.Quote(accountID)
	}
	// The interactive shell still interprets the user's saved command and
	// remains useful when Claude exits. The shim scopes subsequent invocations
	// in this shell to the same official CLI profile.
	env := `export CAGE_CLAUDE_ACCOUNT_ID=` + shellescape.Quote(accountID) +
		` CAGE_CLAUDE_SUBJECT=` + shellescape.Quote(subject) + "; "
	if accountID != "default" {
		env += `export CLAUDE_CONFIG_DIR="$HOME/.local/share/cage/claude-accounts/$CAGE_CLAUDE_ACCOUNT_ID/config"; `
	}
	env += `export PATH="/usr/local/lib/cage-claude-bin:$PATH"; `
	env += `unalias claude 2>/dev/null || :; claude() { command /usr/local/bin/cage-claude-account run "$CAGE_CLAUDE_ACCOUNT_ID" "$@"; }; `
	body := env + `eval "$CAGE_TERMINAL_COMMAND"; unset CAGE_TERMINAL_COMMAND; exec bash -i`
	return "cd " + shellescape.Quote(workspace) +
		" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 CAGE_TERMINAL_COMMAND=" +
		shellescape.Quote(command) + " bash -lic " + shellescape.Quote(body)
}

// The saved command is interpreted only by the guest's interactive shell.
// Reconnecting a viewer never executes it again; exiting Claude leaves a shell.
func terminalScript(workspace, kind, command string) string {
	prefix := "cd " + shellescape.Quote(workspace) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 "
	if kind != "claude" {
		return prefix + "bash -l"
	}
	return prefix + "CAGE_TERMINAL_COMMAND=" + shellescape.Quote(command) + " bash -lic " + shellescape.Quote(
		"export CAGE_CLAUDE_ACCOUNT_ID=default CAGE_ACCOUNT_SCHEDULING=off; eval \"$CAGE_TERMINAL_COMMAND\"; unset CAGE_TERMINAL_COMMAND CAGE_CLAUDE_ACCOUNT_ID CAGE_ACCOUNT_SCHEDULING; exec bash -il")
}

func herdrProjectLabel(workspace string) string {
	cleaned := strings.TrimRight(strings.TrimSpace(workspace), "/\\")
	if cleaned == "" {
		return "Super Cage"
	}
	base := filepath.Base(cleaned)
	if base == "" || base == "." || base == "/" || base == "\\" {
		return "Super Cage"
	}
	return base
}
