package workbench

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Codex CLI sessions reuse the phone's shared-session records. The guest
// helper reads Codex's own rollout files and runs each phone message as a
// detached "codex exec" turn; the host keeps only public session state.
//
//go:embed codex_sessions.py
var codexSessionsHelper string

const codexProvider = "codex"

var codexSessionsSpec = newGuestScriptSpec(".local/share/cage/codex-session-code", 0o600, codexSessionsHelper)

// codexSessionsCommand returns a small, constant-size command targeting the
// SHA-256-pinned helper in ~/.local/share/cage/codex-session-code/<hash>.py so
// SSH argv stays well under Windows CreateProcessW's 32,767 UTF-16 limit.
// The helper file is versioned, so a running exec turn keeps its own copy.
func codexSessionsCommand(mode string) string { return codexSessionsInvocation(mode, "-") }

// codexSessionsInvocation passes the request as an argument instead of on
// stdin, which a terminal's Codex needs for itself.
func codexSessionsInvocation(mode, argument string) string {
	return codexSessionsSpec.shellCommand("python3", mode, argument)
}

func (s *Server) ensureCodexSessions(ctx context.Context) error {
	return s.ensureGuestScript(ctx, codexSessionsSpec, "python3")
}

type codexConversation struct {
	Error            string       `json:"error"`
	ThreadID         string       `json:"thread_id"`
	Account          string       `json:"account"`
	Connection       string       `json:"connection"`
	Workspace        string       `json:"workspace"`
	Title            string       `json:"title"`
	UpdatedAt        float64      `json:"updated_at"`
	Turns            []mobileTurn `json:"turns"`
	Running          bool         `json:"running"`
	Active           bool         `json:"active"`
	CanSend          bool         `json:"can_send"`
	HistoryTruncated bool         `json:"history_truncated"`
	ConnectionNotice string       `json:"connection_notice"`
	AccountRefused   bool         `json:"account_refused"`
	// Owner is the guest's view: "phone" (a phone turn runs), "desktop" (a
	// Codex TUI under a Cage lease), "external" (another process) or "".
	Owner string        `json:"owner"`
	Lease string        `json:"lease"`
	CLI   *codexCLICaps `json:"cli,omitempty"`
	// HistoryCompressed: the thread's history is stored compressed, which the
	// helper cannot read but the Codex TUI resumes.
	HistoryCompressed bool `json:"history_compressed"`
	HistoryFound      bool `json:"history_found"`
}

// codexCLICaps is what the guest's Codex CLI supports, from its own help.
type codexCLICaps struct {
	Installed      bool   `json:"installed"`
	Complete       bool   `json:"complete"`
	Version        string `json:"version"`
	Resume         bool   `json:"resume"`
	NoDaemon       bool   `json:"no_daemon"`
	ResumeNoDaemon bool   `json:"resume_no_daemon"`
	Delete         bool   `json:"delete"`
	WriterLocks    bool   `json:"writer_locks"`
}

const codexDesktopNotice = "这段会话正在电脑的 Codex 终端中打开，手机上可以查看和停止本轮。"

// A guest lease this server has no terminal for (one that outlived its
// terminal, or another workbench's) reads as another process.
const codexExternalNotice = "这段会话正在电脑上的 Codex 终端中打开。请先退出该终端中的 Codex，再从手机继续。"

var errCodexDesktopOwned = errors.New("这段会话正在电脑的 Codex 终端中打开。可在手机的终端页打开该终端继续，或等电脑上退出 Codex 后再发送。")

// codexOwner is who writes a Codex session's thread. A live terminal of this
// server bound to the session makes it the desktop's whatever the guest last
// said. A Cage lease without such a terminal is a TUI this server does not
// know (another workbench, or one that outlived a restart): to this server
// it is like any other process.
func codexOwner(bound bool, guest string) string {
	switch {
	case bound:
		return "desktop"
	case guest == "desktop":
		return "external"
	}
	return guest
}

// codexPhoneLoad counts the Codex sessions whose phone turn runs, by account.
func (m *mobileCLIStore) codexPhoneLoad() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	load := map[string]int{}
	for id, owner := range m.codexOwner {
		if account := m.catalog[id].CodexAccountID; owner == "phone" && account != "" {
			load[account]++
		}
	}
	return load
}

// forgetCodexDesktop drops the guest's report of a desktop lease once this
// server's terminal for the session has ended; the next look says afresh.
func (m *mobileCLIStore) forgetCodexDesktop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.codexOwner[id] == "desktop" {
		delete(m.codexOwner, id)
	}
}

// guestCodexOwner reads the guest's owner; a running thread with no owner
// is taken as another process's, so nothing writes beside it.
func guestCodexOwner(item codexConversation) string {
	if item.Owner == "" && item.Running {
		return "external"
	}
	return item.Owner
}

func (m *mobileCLIStore) codexOwners() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	owners := make(map[string]string, len(m.codexOwner))
	for id, owner := range m.codexOwner {
		owners[id] = owner
	}
	return owners
}

// codexSessionsPossible: Codex terminals can be sessions. A custom Codex
// command (codex --search, a wrapper) stays a plain terminal, as a Claude
// command that cannot share its session does.
func (s *Server) codexSessionsPossible() bool {
	s.mu.Lock()
	command := strings.TrimSpace(codexTerminalCommand(s.preferences))
	s.mu.Unlock()
	return s.codexGuestSupported() && command == "codex"
}

func (m *mobileCLIStore) codexCLICaps() *codexCLICaps {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.codexCLI == nil {
		return nil
	}
	caps := *m.codexCLI
	return &caps
}

func (m *mobileCLIStore) noteCodexCLI(caps *codexCLICaps) {
	if caps == nil || !caps.Complete {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *caps
	m.codexCLI = &copied
}

// codexAccountLabels names Codex accounts as the account panel does. The
// accounts file is read again only when it is another file (every save
// replaces it) or its size or modification time changed.
func (s *Server) codexAccountLabels() map[string]string {
	c := &s.codexAccounts
	c.mu.Lock()
	defer c.mu.Unlock()
	info, statErr := os.Stat(filepath.Join(s.profile, "codex-accounts.json"))
	if statErr == nil && c.labelsFile != nil && os.SameFile(info, c.labelsFile) && info.Size() == c.labelsFile.Size() && info.ModTime().Equal(c.labelsFile.ModTime()) {
		return maps.Clone(c.labels)
	}
	c.labels, c.labelsFile = nil, nil
	list, err := s.loadCodexAccounts()
	labels := map[string]string{}
	if err != nil {
		return labels
	}
	for _, a := range list.Accounts {
		labels[a.ID] = a.Label
		if labels[a.ID] == "" {
			labels[a.ID] = a.Email
		}
	}
	if statErr == nil {
		c.labels, c.labelsFile = maps.Clone(labels), info
	}
	return labels
}

// codexRows fills in the live state of Codex rows of a list.
func (s *Server) codexRows(sessions []mobileSummary) {
	var owners, labels map[string]string
	for i := range sessions {
		row := &sessions[i]
		if row.Provider != codexProvider {
			continue
		}
		if owners == nil {
			owners, labels = s.mobileCLI.codexOwners(), s.codexAccountLabels()
		}
		row.Owner = codexOwner(s.terminals.boundCodex(row.ID) != nil, owners[row.ID])
		row.Running = row.Owner != ""
		row.Active = row.Active && row.Running
		row.AccountLabel = labels[row.CodexAccountID]
	}
}

func (record *mobileRecord) codex() bool { return record.Provider == codexProvider }

func (record *mobileRecord) codexGuestAccount() string {
	if record.CodexAccount == "" {
		return "default"
	}
	return record.CodexAccount
}

// A new session learns its Codex thread ID from its first turn. Until then
// CLIID is only a host placeholder and Resume is false.
func (record *mobileRecord) codexThread() string {
	if record.Resume {
		return record.CLIID
	}
	return ""
}

func (s *Server) runCodexSessions(ctx context.Context, mode string, input any, output any) error {
	started := time.Now()
	epoch, ok := s.account.access()
	if !ok {
		return errMobileLocked
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	if holdExpired(ctx) {
		return context.Cause(ctx)
	}
	if mode == "discover" && s.residentCodexDiscover(ctx, input, output) {
		return nil
	}
	loginScript := guestLoginScript(codexSessionsSpec.guardedShellBody("python3", mode, "-"))
	err := s.runWithGuestScript(ctx, codexSessionsSpec, "python3", func() (int, error) {
		return s.runGuestHelperLoginScript(ctx, started, "codex-sessions", mode, loginScript, input, output)
	})
	var ensureErr *guestScriptEnsureError
	if errors.As(err, &ensureErr) {
		failure, message := "guest_command", "电脑未能读取会话状态，聊天记录已保留。"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			failure, message = "timeout", "电脑读取运行环境超时，暂时无法获取会话状态。"
		} else if exitCodeOf(err) == 255 {
			failure, message = "ssh_connection", "电脑暂时无法连接运行环境，聊天记录已保留。"
		}
		_ = diagnostics.New(s.profile).Record("warn", "codex-sessions", fmt.Sprintf("operation=%s failure=%s exit=%d elapsed_ms=%d context=%v", mode, failure, exitCodeOf(err), time.Since(started).Milliseconds(), ctx.Err()))
		return errors.New(message)
	}
	return err
}

func (s *Server) discoverCodexConversations(ctx context.Context, paths []string) error {
	if !s.codexGuestSupported() {
		return nil
	}
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	if !ready {
		return nil
	}
	accounts := []string{"default"}
	s.codexAccounts.mu.Lock()
	list, err := s.loadCodexAccounts()
	s.codexAccounts.mu.Unlock()
	if err != nil {
		return err
	}
	for _, account := range list.Accounts {
		accounts = append(accounts, account.ID)
	}
	workspaces := make(map[string]string, len(paths))
	roots := make([]string, 0, len(paths))
	for _, path := range paths {
		workspaces[s.cliWorkspace(path)] = path
		roots = append(roots, s.cliWorkspace(path))
	}
	var found []codexConversation
	var failure struct {
		Error string `json:"error"`
	}
	var reply json.RawMessage
	if err := s.runCodexSessions(ctx, "discover", map[string]any{"accounts": accounts, "workspaces": roots}, &reply); err != nil {
		return err
	}
	if json.Unmarshal(reply, &found) != nil {
		_ = json.Unmarshal(reply, &failure)
		return errors.New(failure.Error)
	}
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	threads := map[string]*mobileRecord{}
	connections := map[string]*mobileRecord{}
	for id := range m.catalog {
		record, err := m.record(s.profile, id)
		if err != nil {
			return err
		}
		threads[record.CLIID] = record
		if record.codex() && record.Connection != "" {
			connections[record.Connection] = record
		}
	}
	running := map[string]bool{}
	owners := map[string]string{}
	for _, item := range found {
		var workspace, matched string
		for guest, path := range workspaces {
			if cliWorkspaceWithin(item.Workspace, guest, false) && len(guest) > len(matched) {
				workspace, matched = path, guest
			}
		}
		if workspace == "" || item.ThreadID != "" && !validCLIUUID(item.ThreadID) {
			continue
		}
		// The guest state a connection names is the session's own; a thread
		// match alone could be a second session made for the same thread.
		record := connections[item.Connection]
		if record == nil {
			record = threads[item.ThreadID]
		}
		if record != nil && (!record.codex() || record.Deleted || record.Archived || record.codexGuestAccount() != item.Account) {
			continue
		}
		if record == nil {
			if item.ThreadID == "" {
				continue
			}
			record = &mobileRecord{mobileSession: mobileSession{mobileSummary: mobileSummary{ID: uuid.NewString(), Provider: codexProvider, Workspace: workspace}, Turns: []mobileTurn{}}, Terminal: true}
			record.Generation = m.epochLocked()
			if item.Account != "default" {
				record.CodexAccount = item.Account
			}
		}
		before, _ := json.Marshal(record)
		if item.ThreadID != "" {
			record.CLIID, record.Resume = item.ThreadID, true
		}
		record.Workspace, record.Active = workspace, item.Active
		if item.Title != "" {
			record.Title = item.Title
		}
		if record.GuestWorkspace == "" {
			record.GuestWorkspace = item.Workspace
		}
		if item.UpdatedAt > 0 {
			record.UpdatedAt = time.UnixMilli(int64(item.UpdatedAt * 1000)).UTC()
		}
		if after, _ := json.Marshal(record); string(before) != string(after) {
			if err := writePrivate(mobileRecordPath(s.profile, record.ID), record); err != nil {
				return err
			}
			m.catalog[record.ID] = record.summary()
		}
		running[record.ID] = running[record.ID] || item.Running
		if owners[record.ID] == "" {
			owners[record.ID] = guestCodexOwner(item)
		}
		threads[record.CLIID] = record
	}
	m.codexLive = running
	m.codexOwner = owners
	return nil
}

// Caller holds conversationMu.
func (s *Server) codexConversationView(ctx context.Context, record *mobileRecord) (mobileSession, error) {
	id := record.ID
	root := s.cliWorkspace(record.Workspace)
	var current codexConversation
	input := map[string]any{"thread": record.codexThread(), "account": record.codexGuestAccount(), "connection": record.Connection, "workspace": root}
	if err := s.runCodexSessions(ctx, "view", input, &current); err != nil {
		return mobileSession{}, err
	}
	if current.Error != "" {
		return mobileSession{}, errors.New(current.Error)
	}
	label := ""
	if record.CodexAccount != "" {
		label = s.codexAccountLabels()[record.CodexAccount]
	}
	if current.ThreadID != "" && !validCLIUUID(current.ThreadID) || current.Workspace != "" && !cliWorkspaceWithin(current.Workspace, root, false) {
		return mobileSession{}, errors.New("Codex 会话的项目已变化，请重新读取会话列表")
	}
	if current.AccountRefused && record.CodexAccount != "" {
		s.recheckCodexAccount(record.CodexAccount)
	}
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reload after SSH so metadata written meanwhile is retained.
	record, err := m.record(s.profile, id)
	if err != nil {
		return mobileSession{}, err
	}
	if record.Deleted {
		return mobileSession{}, os.ErrNotExist
	}
	before, _ := json.Marshal(record)
	record.Turns = current.Turns
	if record.Turns == nil {
		record.Turns = []mobileTurn{}
	}
	if current.ThreadID != "" {
		record.CLIID, record.Resume = current.ThreadID, true
	}
	if current.Title != "" {
		record.Title = current.Title
	}
	if current.Workspace != "" {
		record.GuestWorkspace = current.Workspace
	}
	// The server's own bound terminal decides before the guest's report,
	// which can lag the terminal's start by a login shell.
	bound := s.terminals.boundCodex(id)
	owner := codexOwner(bound != nil, guestCodexOwner(current))
	// Keep the durable last binding even when this server has no PTY. Only
	// expose a live binding in the view sent to clients.
	if bound != nil {
		record.TerminalID = bound.id
	}
	canSend := current.CanSend && owner == ""
	record.ConnectionNotice = current.ConnectionNotice
	if owner == "desktop" {
		record.ConnectionNotice = codexDesktopNotice
	} else if owner == "external" && current.Owner == "desktop" {
		record.ConnectionNotice = codexExternalNotice
	}
	record.CanSend, record.HistoryTruncated = &canSend, current.HistoryTruncated
	record.Owner, record.Active = owner, current.Active && owner != ""
	if after, _ := json.Marshal(record); string(before) != string(after) {
		record.Revision++
		if current.UpdatedAt > 0 {
			record.UpdatedAt = time.UnixMilli(int64(current.UpdatedAt * 1000)).UTC()
		}
		if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
			return mobileSession{}, err
		}
		if m.catalog != nil {
			m.catalog[id] = record.summary()
		}
	}
	if m.codexLive != nil {
		m.codexLive[id] = owner != ""
	}
	if m.codexOwner == nil {
		m.codexOwner = map[string]string{}
	}
	m.codexOwner[id] = guestCodexOwner(current)
	view := record.snapshot()
	view.Running, view.AccountLabel = owner != "", label
	if bound == nil {
		view.DesktopTerminalID = ""
	}
	return view, nil
}

// Caller holds the operation lock. The guest admits a message durably before
// Codex reads it, so a retried request ID never starts a second turn.
func (s *Server) codexConversationAction(ctx context.Context, record *mobileRecord, input map[string]any) error {
	action, _ := input["action"].(string)
	payload := map[string]any{"request_id": input["request_id"], "connection": record.Connection}
	switch action {
	case "send":
		// The caller holds the operation lock, as terminal creation does, so
		// no desktop terminal can start between this check and the send.
		if s.terminals.boundCodex(record.ID) != nil {
			return errCodexDesktopOwned
		}
		if err := s.codexConnection(record); err != nil {
			return err
		}
		selection, err := s.pinnedCodexSelection(record)
		if err != nil {
			return err
		}
		subject := selection.Subject
		directories, err := s.projectAdditionalDirectories(record.Workspace)
		if err != nil {
			return err
		}
		root := s.cliWorkspace(record.Workspace)
		directory := record.GuestWorkspace
		if directory == "" {
			directory = root
		}
		if !cliWorkspaceWithin(directory, root, false) {
			return errors.New("会话目录不在已授权的项目内，请重新读取会话列表")
		}
		if directories == nil {
			directories = []string{}
		}
		payload["connection"] = record.Connection
		payload["text"], payload["thread"], payload["cwd"] = input["text"], record.codexThread(), directory
		payload["account"], payload["subject"], payload["additional_directories"] = record.codexGuestAccount(), subject, directories
	case "stop":
		if record.Connection == "" {
			return errors.New("这次运行不是从手机发起的，请在电脑终端停止")
		}
	case "approve":
		return errors.New("Codex 会话不使用审批，请重新读取会话")
	default:
		return errors.New("Codex 会话不支持此操作")
	}
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.runCodexSessions(ctx, action, payload, &response); err != nil {
		return err
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "电脑未能处理 Codex 会话请求，请稍后重试。"
		}
		return errors.New(response.Error)
	}
	return nil
}

// Discovered sessions get their guest state key on the first phone message.
func (s *Server) codexConnection(record *mobileRecord) error {
	if record.Connection != "" {
		return nil
	}
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.record(s.profile, record.ID)
	if err != nil {
		return err
	}
	if current.Connection == "" {
		current.Connection = uuid.NewString()
		if err := writePrivate(mobileRecordPath(s.profile, current.ID), current); err != nil {
			return err
		}
	}
	record.Connection = current.Connection
	return nil
}

// A new Codex session receives an account like a new Codex terminal and runs
// nothing until its first message.
func (s *Server) createCodexConversation(ctx context.Context, id, workspace string) (mobileSession, error) {
	if !s.codexGuestSupported() {
		return mobileSession{}, errors.New("Codex 会话目前只支持 Linux 环境")
	}
	s.mobileCLI.mu.Lock()
	_, err := s.mobileCLI.record(s.profile, id)
	s.mobileCLI.mu.Unlock()
	var selection codexSelection
	if errors.Is(err, os.ErrNotExist) {
		if s.magpieNativeAutoEligible("codex") {
			native, routeErr := s.magpieNewLaunch("codex", "codex", workspace)
			if routeErr != nil {
				return mobileSession{}, routeErr
			}
			if native {
				return mobileSession{}, errors.New("此 Codex 配置由 Magpie 路由；结构化移动会话暂不支持，请使用终端")
			}
		}
		if selection, err = s.selectCodexAccount("", false, workspace); err != nil {
			return mobileSession{}, err
		}
	} else if err != nil {
		return mobileSession{}, err
	}
	created := false
	sendable := true
	s.account.mu.Lock()
	if !s.mobileAuthorized(ctx) {
		s.account.mu.Unlock()
		return mobileSession{}, errMobileLocked
	}
	view, err := s.mobileCLI.createProvider(s.profile, workspace, id, codexProvider, func(record *mobileRecord) {
		created = true
		record.Terminal, record.CodexAccount = true, selection.ID
		record.CodexSubject = selection.Subject
		record.Connection, record.GuestWorkspace = uuid.NewString(), s.cliWorkspace(workspace)
		record.CanSend = &sendable
	})
	s.account.mu.Unlock()
	if err != nil {
		return mobileSession{}, err
	}
	if created && selection.ID != "" {
		s.noteCodexSelection(selection.ID, workspace)
	}
	return view, nil
}

func (m *mobileCLIStore) hasCodexSession(folders []string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, summary := range m.catalog {
		if summary.Provider == codexProvider && !summary.Deleted && slices.Contains(folders, summary.Workspace) {
			return true
		}
	}
	return false
}

// Caller holds the operation and conversation locks.
func (s *Server) purgeCodexConversation(ctx context.Context, record *mobileRecord) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	input := map[string]any{"thread": record.codexThread(), "account": record.codexGuestAccount(), "connection": record.Connection, "workspace": s.cliWorkspace(record.Workspace)}
	if err := s.runCodexSessions(ctx, "purge", input, &result); err != nil {
		return err
	}
	if !result.OK {
		return errors.New(result.Error)
	}
	return nil
}
