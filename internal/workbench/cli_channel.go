package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
	"github.com/klauspost/compress/gzip"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

//go:embed cli_channel.py
var cliChannelHelper string

//go:embed cli_channel.resume.md
var cliResumePrompt string

//go:embed claude_account_resume.md
var claudeAccountResumePrompt string

// cliChannelInstalled is the helper as the bootstrap installs it in the guest.
var cliChannelInstalled = func() string {
	var helper strings.Builder
	for _, line := range strings.Split(cliChannelHelper, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			helper.WriteString(line + "\n")
		}
	}
	return helper.String()
}()

// cliChannelVersion names the installed helper files.
var cliChannelVersion = fmt.Sprintf("%x", sha256.Sum256([]byte(cliChannelHelper+cliResumePrompt+claudeAccountResumePrompt)))

var cliChannelEncoded = func() string {
	bundle := []byte(strings.Join([]string{cliChannelInstalled, cliResumePrompt, claudeAccountResumePrompt}, "\x00"))
	// Compact Python indentation before deflate to fit the Windows SSH command
	// budget. 0xff cannot occur in UTF-8 source or prompt text.
	bundle = bytes.ReplaceAll(bundle, []byte("    "), []byte{0xff})
	var compressed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	_, _ = writer.Write(bundle)
	_ = writer.Close()
	return cliTransport85(compressed.Bytes())
}()

// Python's RFC 1924 base85 alphabet has no single quote. It fits the compressed
// helper within Windows' command limit without expanding shell quoting.
func cliTransport85(data []byte) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"
	var out strings.Builder
	for len(data) > 0 {
		n := min(4, len(data))
		var value uint64
		for i := 0; i < 4; i++ {
			value <<= 8
			if i < n {
				value |= uint64(data[i])
			}
		}
		var block [5]byte
		for i := 4; i >= 0; i-- {
			block[i] = alphabet[value%85]
			value /= 85
		}
		out.Write(block[:n+1])
		data = data[n:]
	}
	return out.String()
}

type terminalConversation struct {
	NativeCCSUnverified bool         `json:"native_ccs_unverified"`
	AccountFailure      string       `json:"account_failure"`
	ResumePending       bool         `json:"resume_pending"`
	NativeHistory       bool         `json:"native_history"`
	CLIID               string       `json:"cli_id"`
	Config              string       `json:"config"`
	Connection          string       `json:"connection"`
	Workspace           string       `json:"workspace"`
	Title               string       `json:"title"`
	UpdatedAt           float64      `json:"updated_at"`
	Turns               []mobileTurn `json:"turns"`
	CanSend             bool         `json:"can_send"`
	ChannelReady        bool         `json:"channel_ready"`
	HasHistory          bool         `json:"has_history"`
	Running             bool         `json:"running"`
	ClaudeDesktop       bool         `json:"claude_desktop"`
	Active              bool         `json:"active"`
	ConnectionNotice    string       `json:"connection_notice"`
	HistoryTruncated    bool         `json:"history_truncated"`
	// Tasks comes with a view, RunningTasks with a discovered session; only the
	// resident helper (cli_discovery.py) sends them.
	Tasks        *mobileTasks `json:"tasks"`
	RunningTasks int          `json:"running_tasks"`
	// WatchKey comes only from the resident helper: view-wait answers when
	// what this view was read from changes.
	WatchKey string `json:"watch_key"`
}

func cliChannelCommand(mode, argument string) string {
	// Versioned helpers let an open terminal keep its original protocol while a
	// rebuilt host begins new sessions. No account configuration is overwritten.
	bootstrap := fmt.Sprintf(`import base64,gzip,os,pathlib,sys,tempfile
d=pathlib.Path.home()/".local/share/cage/cli-channel-code"
d.mkdir(parents=True,exist_ok=True,mode=0o700)
n="%s"
a=gzip.decompress(base64.b85decode("%s")).replace(b"\xff",b"    ").decode().split("\0")
for s,v in zip((".py",".resume.md",".account-resume.md"),a):
 p=d/(n+s)
 if not p.exists():
  with tempfile.NamedTemporaryFile(dir=d,delete=False) as f:
   f.write(v.encode())
   t=f.name
  os.replace(t,p)
p=d/(n+".py")
os.execv(sys.executable,[sys.executable,"-I",str(p),*sys.argv[1:]])
`, cliChannelVersion, cliChannelEncoded)
	return shellescape.QuoteCommand([]string{"python3", "-I", "-c", bootstrap, mode, argument})
}

func (s *Server) runCLIChannel(ctx context.Context, mode string, input any, output any) error {
	started := time.Now()
	epoch, ok := s.account.access()
	if !ok {
		return errMobileLocked
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	if s.windowsGuest != nil {
		return s.windowsCLIRequest(ctx, map[string]any{"action": "channel", "mode": mode, "input": input}, output)
	}
	if s.residentCLIChannel(ctx, mode, input, output) {
		return nil
	}
	if err := s.runGuestHelper(ctx, started, "cli-channel", mode, s.cliHelperCommand(mode, "-"), input, output); err != nil {
		return err
	}
	s.cliDiscovery.helperInstalled(s)
	return nil
}

// guestLoginScript runs body from the guest's login shell with the login
// shell's own output sent to stderr.
func guestLoginScript(body string) string {
	return "exec /bin/bash -c " + shellescape.Quote("exec 3>&1 1>&2; exec /bin/bash -lc "+shellescape.Quote("exec 1>&3 3>&-; "+body))
}

// guestLoginCommand runs helper from the guest's login shell with the login
// shell's own output sent to stderr.
func guestLoginCommand(helper string) string {
	return guestLoginScript("exec " + helper)
}

// runGuestHelper writes input as JSON to a helper started from the guest's
// login shell and decodes the helper's JSON reply. Startup banners cannot
// reach the reply because the login shell's output goes to stderr.
func (s *Server) runGuestHelper(ctx context.Context, started time.Time, source, mode, helper string, input any, output any) error {
	if holdExpired(ctx) {
		return context.Cause(ctx) // an abandoned look of a held read starts no login
	}
	if source == "codex-sessions" && mode == "discover" && s.residentCodexDiscover(ctx, input, output) {
		return nil
	}
	_, err := s.runGuestHelperLoginScript(ctx, started, source, mode, guestLoginCommand(helper), input, output)
	return err
}

func (s *Server) runGuestHelperLoginScript(ctx context.Context, started time.Time, source, mode, loginScript string, input any, output any) (int, error) {
	if holdExpired(ctx) {
		return -1, context.Cause(ctx) // an abandoned look of a held read starts no login
	}
	data, err := json.Marshal(input)
	if err != nil {
		return -1, err
	}
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", loginScript)
	cmd.Stdin = strings.NewReader(string(data))
	var stderr fileControlStderr
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	result, err := cmd.Output()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	if err != nil || len(result) > 4<<20 || json.Unmarshal(result, output) != nil {
		failure := "guest_response"
		message := "电脑未能读取会话状态，聊天记录已保留。"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			failure, message = "timeout", "电脑读取运行环境超时，暂时无法获取会话状态。"
		} else if err != nil {
			failure = "guest_command"
			if exit == 255 {
				failure, message = "ssh_connection", "电脑暂时无法连接运行环境，聊天记录已保留。"
			}
		}
		// Record transport and protocol evidence without terminal output,
		// prompts, account paths or raw SSH/Python error messages.
		_ = diagnostics.New(s.profile).Record("warn", source, fmt.Sprintf("operation=%s failure=%s exit=%d elapsed_ms=%d context=%v output_bytes=%d stderr_bytes=%d", mode, failure, exit, time.Since(started).Milliseconds(), ctx.Err(), len(result), len(stderr.data)))
		return exit, errors.New(message)
	}
	return 0, nil
}

func (s *Server) discoverTerminalConversations(ctx context.Context, paths []string) error {
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	if !ready {
		return nil
	}
	accounts := []string{"default"}
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return err
	}
	for _, account := range list.Accounts {
		if account.guestID() != "default" {
			accounts = append(accounts, account.guestID())
		}
	}
	workspaces := make(map[string]string, len(paths))
	for _, path := range paths {
		if s.windowsGuest != nil {
			if s.windowsGuest.Remote() {
				if home, err := s.remoteWindowsCLIWorkspace(path); err == nil {
					workspaces[home] = home
				}
				continue
			}
			binding, err := s.windowsProjectForHost(path)
			if err == nil {
				workspaces[binding.Path] = path
			}
		} else {
			workspaces[s.cliWorkspace(path)] = path
		}
	}
	var found []terminalConversation
	if err := s.runCLIChannel(ctx, "discover", map[string]any{"accounts": accounts, "workspaces": workspaces}, &found); err != nil {
		return err
	}
	m := &s.mobileCLI
	m.mu.Lock()
	loadChanged := false
	defer func() {
		m.mu.Unlock()
		if loadChanged {
			s.kickClaudeLaunchSchedule()
		}
	}()
	known := map[string]*mobileRecord{}
	running := map[string]bool{}
	desktop := map[string]bool{}
	shared := map[string]bool{}
	for id := range m.catalog {
		record, err := m.record(s.profile, id)
		if err != nil {
			return err
		}
		known[record.CLIID] = record
		for _, execution := range record.Executions {
			known[execution.CLIID] = record
		}
	}
	deferred := s.followClearedConversations(m, known, found)
	for _, item := range found {
		var workspace, matched string
		for guest, path := range workspaces {
			if cliWorkspaceWithin(item.Workspace, guest, s.windowsGuest != nil) && len(guest) > len(matched) {
				workspace, matched = path, guest
			}
		}
		if workspace == "" || !validCLIUUID(item.CLIID) {
			continue
		}
		record := known[item.CLIID]
		if record != nil && record.CLIID != item.CLIID {
			continue
		}
		if owner := deferred[item.CLIID]; owner != nil && record == nil {
			// The terminal's CLI has a new id without a transcript yet: no
			// entry for it, and the entry it continues stays running.
			running[owner.ID] = running[owner.ID] || item.Running
			if item.Running && item.Connection != "" {
				shared[owner.ID] = true
			}
			continue
		}
		if record != nil && (record.Deleted || record.Archived) {
			continue
		}
		if record != nil && !record.Terminal {
			continue
		}
		// Unchanged sessions are not rewritten: every list poll runs this, and
		// each write is a synced file replacement.
		var before []byte
		if record != nil {
			before, _ = json.Marshal(record)
		}
		if record == nil {
			record = &mobileRecord{mobileSession: mobileSession{mobileSummary: mobileSummary{ID: uuid.NewString(), Workspace: workspace}, Turns: []mobileTurn{}}, CLIID: item.CLIID, Terminal: true, Resume: true}
			record.Generation = m.epochLocked()
			record.Account.GuestID = "default"
			for _, account := range list.Accounts {
				config := strings.ReplaceAll(item.Config, "\\", "/")
				if strings.Contains(config, "/claude-accounts/"+account.guestID()+"/") || s.windowsGuest != nil && strings.HasSuffix(strings.ToLower(config), "/accounts/"+account.guestID()+"/config") {
					record.Account = claudeSelection{ID: account.ID, GuestID: account.guestID(), Subject: claudeIdentityHash(account.Email, account.Organization)}
				}
			}
		}
		record.Workspace = workspace
		record.Active = item.Active
		record.RunningTasks = item.RunningTasks
		title := clearedConversationTitle(item.Title)
		if title == clearedTitle && record.Title != "" {
			// The guest titles a conversation by its first command until a
			// name is set; after /clear the entry keeps its previous title.
			title = record.Title
		}
		record.Title, record.GuestConfig, record.Connection = title, item.Config, item.Connection
		s.cliDiscovery.named(record.ID, title)
		record.GuestWorkspace = item.Workspace
		if item.Connection != "" {
			if terminal := s.terminals.forConversation(item.Connection); terminal != nil {
				record.TerminalID = terminal.id
				terminal.mu.Lock()
				terminal.sharedID = record.ID
				terminal.mu.Unlock()
			}
		}
		record.UpdatedAt = time.UnixMilli(int64(item.UpdatedAt * 1000)).UTC()
		if after, _ := json.Marshal(record); !bytes.Equal(before, after) {
			if err := writePrivate(mobileRecordPath(s.profile, record.ID), record); err != nil {
				return err
			}
		}
		m.catalog[record.ID] = record.summary()
		running[record.ID] = running[record.ID] || item.Running
		desktop[record.ID] = desktop[record.ID] || item.Running && item.ClaudeDesktop
		if item.Running && item.Connection != "" && !item.ClaudeDesktop {
			shared[record.ID] = true
		}
		known[record.CLIID] = record
	}
	m.running = running
	m.desktop = desktop
	loadChanged = !maps.Equal(m.claudeShared, shared)
	m.claudeShared = shared
	return nil
}

// clearedTitle is how the guest titles a conversation that began with /clear:
// the CLI starts the new transcript with that command's row.
const clearedTitle = "/clear"

// clearedConversationTitle reads the title the guest gives a conversation that
// began with /clear: "/clear" from its transcript, or the command row itself
// when it is known from its channel state (its transcript has no folder yet).
func clearedConversationTitle(title string) string {
	if strings.HasPrefix(title, "<command-name>"+clearedTitle+"<") {
		return clearedTitle
	}
	return title
}

// untitledTitle is a conversation the guest knows only from its channel
// state: the CLI has its id but has not written its transcript yet.
const untitledTitle = "Claude Code"

// followClearedConversations keeps one entry per terminal conversation when the
// CLI changes its session id in place. /clear ends the old id and starts a new
// one in the same process, so the same channel connection reports it. A new
// id on a connection owned by another record's terminal joins that record when
// its transcript began with /clear; the old id becomes an earlier part. An id
// with no transcript yet is held back (returned) so it never makes an entry of
// its own. Anything else, such as /resume of another conversation, keeps the
// usual rules. Caller holds m.mu.
func (s *Server) followClearedConversations(m *mobileCLIStore, known map[string]*mobileRecord, found []terminalConversation) map[string]*mobileRecord {
	byID := map[string]*mobileRecord{}
	for _, record := range known {
		byID[record.ID] = record
	}
	eligible := func(record *mobileRecord) bool {
		return record != nil && record.Terminal && !record.codex() && !record.Deleted && !record.Archived
	}
	// Owners are taken before discovery rewrites records: the old id's item no
	// longer carries the connection, and an open terminal outlives it.
	owners := map[string]*mobileRecord{}
	for _, record := range byID {
		if eligible(record) && record.Connection != "" {
			owners[record.Connection] = record
		}
	}
	for connection, id := range s.terminals.conversationOwners() {
		if record := byID[id]; eligible(record) {
			owners[connection] = record
		}
	}
	present := make(map[string]bool, len(found))
	for _, item := range found {
		present[item.CLIID] = true
	}
	deferred := map[string]*mobileRecord{}
	for _, item := range found {
		owner := owners[item.Connection]
		if item.Connection == "" || owner == nil || owner.CLIID == item.CLIID || !validCLIUUID(item.CLIID) {
			continue
		}
		if sw := owner.Switch; sw != nil && sw.Phase != "completed" {
			// An account handoff in progress decides this entry's id: a new id
			// on its terminal meanwhile makes no entry and is linked, if it is
			// a /clear, once the handoff is settled.
			if known[item.CLIID] == nil {
				deferred[item.CLIID] = owner
			}
			continue
		}
		if known[item.CLIID] != nil {
			// Only a running CLI resumes: a reopened entry's first launch leaves
			// its closed connection on the id it ran, which is now a part.
			if known[item.CLIID] == owner && owner.clearedPart(item.CLIID) != nil && item.Running {
				s.resumeEarlierPart(m, owner, item.CLIID, present)
			}
			continue
		}
		switch clearedConversationTitle(item.Title) {
		case untitledTitle:
			// Held back only while it has no history (the resident helper
			// says so): an untitled conversation with some gets its own entry.
			if !item.HasHistory {
				deferred[item.CLIID] = owner
			}
		case clearedTitle:
			executions, cleared, previous, resume := owner.Executions, owner.Cleared, owner.CLIID, owner.Resume
			owner.Executions = append(slices.Clip(executions), claudeExecution{CLIID: previous, Account: owner.Account, Config: owner.GuestConfig})
			owner.Cleared = append(slices.Clip(cleared), clearedPart{CLIID: previous, ClearedAt: time.Now().UTC(), Empty: !present[previous]})
			if sw := owner.Switch; !present[previous] && sw != nil && sw.Phase == "completed" && sw.TargetID == previous && owner.clearedPart(sw.Source.CLIID) == nil {
				// A handoff's target writes its copy of the history only with its
				// first prompt: before that, the conversation is the source's.
				owner.Cleared = append(owner.Cleared, clearedPart{CLIID: sw.Source.CLIID, ClearedAt: time.Now().UTC()})
			}
			owner.CLIID, owner.Resume = item.CLIID, true
			owner.changed()
			if err := writePrivate(mobileRecordPath(s.profile, owner.ID), owner); err != nil {
				// Unsaved, the link is retried by the next discovery.
				owner.Executions, owner.Cleared, owner.CLIID, owner.Resume = executions, cleared, previous, resume
				deferred[item.CLIID] = owner
				continue
			}
			known[item.CLIID] = owner
			if m.catalog != nil {
				m.catalog[owner.ID] = owner.summary()
			}
		}
	}
	return deferred
}

// resumeEarlierPart: /resume of an earlier part inside the entry's own
// terminal makes it the entry's conversation again, and the conversation the
// terminal left becomes an earlier part. Unsaved, it is retried by the next
// discovery. Caller holds m.mu.
func (s *Server) resumeEarlierPart(m *mobileCLIStore, owner *mobileRecord, id string, present map[string]bool) {
	index := slices.IndexFunc(owner.Executions, func(e claudeExecution) bool { return e.CLIID == id })
	if index < 0 {
		return
	}
	executions, cleared, previous := owner.Executions, owner.Cleared, owner.CLIID
	owner.Executions = append(slices.Delete(slices.Clone(executions), index, index+1), claudeExecution{CLIID: previous, Account: owner.Account, Config: owner.GuestConfig})
	owner.Cleared = append(slices.DeleteFunc(slices.Clone(cleared), func(part clearedPart) bool { return part.CLIID == id }),
		clearedPart{CLIID: previous, ClearedAt: time.Now().UTC(), Empty: !present[previous]})
	owner.CLIID, owner.Resume = id, true
	owner.changed()
	if err := writePrivate(mobileRecordPath(s.profile, owner.ID), owner); err != nil {
		owner.Executions, owner.Cleared, owner.CLIID = executions, cleared, previous
		return
	}
	if m.catalog != nil {
		m.catalog[owner.ID] = owner.summary()
	}
}

// conversationFollowPace is the least time between two follow-up looks (the
// clients' own list pace): a CLI that sits after /clear costs at most that.
var conversationFollowPace = 5 * time.Second

// followConversationID looks at the guest's sessions once more when a view
// finds that the record's terminal no longer runs the record's id (a /clear or
// a /resume inside the CLI): a phone inside a conversation polls only its view,
// so the list's discovery would not link it. One look runs at a time.
func (s *Server) followConversationID() {
	if last := s.conversationFollowAt.Load(); last != 0 && time.Since(time.Unix(0, last)) < conversationFollowPace {
		return
	}
	if !s.conversationFollow.CompareAndSwap(false, true) {
		return
	}
	s.conversationFollowAt.Store(time.Now().UnixNano())
	go func() {
		defer s.conversationFollow.Store(false)
		paths, err := s.projectPaths()
		if err != nil {
			return
		}
		// As a list request does: discovery works on the loaded catalog.
		granted := make(map[string]bool, len(paths))
		for _, path := range paths {
			granted[path] = true
		}
		if _, err := s.mobileCLI.summaries(s.profile, granted, false); err != nil {
			return
		}
		_, _ = s.observeSessions(s.lifetimeContext(), paths, sessionListKinds{claude: true}, time.Now())
	}()
}

func (s *Server) terminalConversationView(ctx context.Context, id string) (mobileSession, error) {
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	m.mu.Unlock()
	if err != nil {
		return mobileSession{}, err
	}
	if record.codex() {
		return s.codexConversationView(ctx, record)
	}
	var current terminalConversation
	input := map[string]any{"cli_id": record.CLIID, "config": record.GuestConfig, "connection": record.Connection, "account": record.Account.GuestID}
	if s.windowsGuest == nil && s.macosGuest == nil {
		// The resident helper keeps a session the CLI filed under this project
		// in it when its latest folder lies elsewhere (cli_discovery.py).
		input["project"] = s.cliWorkspace(record.Workspace)
	}
	if err := s.windowsConversationInput(record, input); err != nil {
		return mobileSession{}, err
	}
	if err := s.runCLIChannel(ctx, "view", input, &current); err != nil {
		return mobileSession{}, err
	}
	s.cliDiscovery.noteView(id, input, current.WatchKey)
	terminal := s.boundTerminal(record)
	bound, open := "", false
	if terminal != nil {
		terminal.mu.Lock()
		bound, open = terminal.connectionID, !terminal.closed
		terminal.mu.Unlock()
	}
	// A reopened entry's first launch leaves its closed connection on this id.
	if current.Connection == "" || !current.Running && bound != "" && bound != current.Connection {
		// The guest has no live connection on this id while the entry's
		// terminal still runs a conversation: its CLI moved to another id
		// (/clear, or a /resume inside it). Checked on every view, as the new
		// id's transcript appears only with the next prompt.
		// The open terminal's own connection decides. A look can be paced
		// away, and this view then clears the record's connection, perhaps
		// after an earlier view saw the old id end: the next view asks again.
		if open && bound != "" || record.Connection != "" && s.terminals.forConversation(record.Connection) != nil || terminal != nil && terminal.ownsConversation() {
			s.followConversationID()
		}
	}
	root := s.cliConversationRoot(record)
	if current.Workspace == "" && !current.HasHistory && record.TerminalID != "" {
		if record.Switch != nil && record.Switch.Phase == "starting" && !current.Running {
			if owner := s.terminals.get(record.TerminalID); owner == nil || !owner.ownsConversation() {
				m.mu.Lock()
				record.Switch.Phase, record.Switch.Error = "prepared", "目标账号未建立会话，请重试交接"
				record.changed()
				err = writePrivate(mobileRecordPath(s.profile, id), record)
				m.mu.Unlock()
				if err != nil {
					return mobileSession{}, err
				}
			}
		}
		return record.snapshot(), nil
	}
	if !cliWorkspaceWithin(current.Workspace, root, s.windowsGuest != nil) {
		return mobileSession{}, errors.New("终端的项目已变化，请重新读取会话列表")
	}
	if terminal := s.terminals.get(record.TerminalID); terminal != nil && current.Connection != "" && current.Workspace != "" {
		terminal.mu.Lock()
		loadChanged := false
		if terminal.connectionID == current.Connection {
			loadChanged = terminal.claudeAccountID != "" && terminal.conversationEnded == current.Running
			terminal.conversationEnded = !current.Running
		}
		terminal.mu.Unlock()
		if loadChanged {
			s.kickClaudeLaunchSchedule()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reload after SSH so another request's identity and terminal attachment are
	// retained. The guest owns transcript content, the host owns the public ID.
	record, err = m.record(s.profile, id)
	if err != nil {
		return mobileSession{}, err
	}
	if record.Deleted {
		return mobileSession{}, os.ErrNotExist
	}
	before, _ := json.Marshal(record)
	record.Turns = adoptedMobileTurns(record.LegacyTurns, current.Turns)
	if record.Turns == nil {
		record.Turns = []mobileTurn{}
	}
	if !s.cliDiscovery.discoveredTitle(id, record.Title) && !(record.Title != "" && strings.HasPrefix(current.Title, "<command-name>"+clearedTitle+"<")) {
		record.Title = current.Title
	}
	record.ConnectionNotice = current.ConnectionNotice
	if s.macOSLegacyWorkspace(record) != "" {
		record.ConnectionNotice = strings.TrimSpace(record.ConnectionNotice + "\n" + macOSLegacyWorkspaceNotice)
	}
	record.CanSend, record.HistoryTruncated = &current.CanSend, current.HistoryTruncated
	record.GuestConfig, record.Connection = current.Config, current.Connection
	record.GuestWorkspace = current.Workspace
	record.Resume = record.Resume || current.HasHistory
	// A view after controller restart must not clear the last active
	// binding before recovery can examine it. A report from its own live
	// viewer can still record that Claude exited into the surrounding shell.
	if terminal != nil && bound != "" && current.Connection == bound {
		record.TerminalRunning = current.Running
	}
	record.Active = current.Active
	record.Tasks = current.Tasks
	record.AccountFailure = current.AccountFailure
	if current.NativeHistory {
		record.ResumeTranscript = ""
	}
	if record.Switch != nil && record.Switch.Phase == "starting" && current.CLIID == record.Switch.TargetID {
		if current.ChannelReady && !current.ResumePending || current.HasHistory && !current.Running {
			record.Switch.Phase, record.Switch.Error = "completed", ""
			if current.NativeHistory {
				record.ResumeTranscript = ""
			}
		} else if !current.Running {
			if owner := s.terminals.get(record.TerminalID); owner == nil || !owner.ownsConversation() {
				record.Switch.Phase, record.Switch.Error = "prepared", "目标账号未建立会话，请重试交接"
			}
		}
	}
	after, _ := json.Marshal(record)
	if string(before) != string(after) {
		record.Revision++
		record.UpdatedAt = time.UnixMilli(int64(current.UpdatedAt * 1000)).UTC()
		if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
			return mobileSession{}, err
		}
		if m.catalog != nil {
			m.catalog[id] = record.summary()
		}
	}
	view := record.snapshot()
	if record.Resume && !current.HasHistory && !current.NativeHistory && current.Config != "" && record.ResumeTranscript == "" && record.Switch == nil {
		view.missingHistory = current.Config
	}
	if current.NativeHistory && current.HasHistory && current.CLIID == record.CLIID && validCLIUUID(current.CLIID) {
		view.nativeResumeID = current.CLIID
	}
	return view, nil
}

// cliConversationRoot is the guest folder of the project a terminal
// conversation is bound to; its CLI may work in a folder below it.
func (s *Server) cliConversationRoot(record *mobileRecord) string {
	if s.windowsGuest != nil {
		return record.GuestWorkspace
	}
	if legacy := s.macOSLegacyWorkspace(record); legacy != "" {
		return legacy
	}
	return s.cliWorkspace(record.Workspace)
}

func (s *Server) terminalConversationAction(ctx context.Context, id string, input map[string]any) (mobileSession, error) {
	if !s.op.TryLock() {
		return mobileSession{}, errTerminalBusy
	}
	defer s.op.Unlock()
	s.account.mu.Lock()
	authorized := s.mobileAuthorized(ctx)
	s.account.mu.Unlock()
	if !authorized {
		return mobileSession{}, errMobileLocked
	}
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	m.mu.Unlock()
	if err != nil {
		return mobileSession{}, err
	}
	if record.Deleted {
		return mobileSession{}, os.ErrNotExist
	}
	if input["action"] == "send" {
		for _, previous := range record.LegacyTurns {
			if previous.ID == input["request_id"] {
				if previous.Prompt != input["text"] {
					return mobileSession{}, errors.New("同一发送编号已用于另一条消息")
				}
				view := record.snapshot()
				view.AcknowledgedRequestID = previous.ID
				return view, nil
			}
		}
	}
	if record.Archived {
		return mobileSession{}, errors.New("此会话已归档，请先撤销归档")
	}
	if record.codex() {
		err = s.codexConversationAction(ctx, record, input)
	} else {
		err = s.claudeConversationRPC(ctx, record, input)
	}
	if err != nil {
		return mobileSession{}, err
	}
	if input["action"] == "send" {
		// The guest already committed this message. Return its receipt now;
		// fetching the transcript is a separate read, not part of admission.
		s.account.mu.Lock()
		defer s.account.mu.Unlock()
		if !s.mobileAuthorized(ctx) {
			return mobileSession{}, errMobileLocked
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		record, err = m.record(s.profile, id)
		if err != nil {
			return mobileSession{}, err
		}
		view := record.snapshot()
		requestID, _ := input["request_id"].(string)
		if findMobileTurn(record, requestID) == nil {
			text, _ := input["text"].(string)
			view.Turns = append(view.Turns, mobileTurn{ID: requestID, Prompt: text, Status: "queued"})
		}
		view.AcknowledgedRequestID = requestID
		return view, nil
	}
	return s.terminalConversationView(ctx, id)
}

func (s *Server) claudeConversationRPC(ctx context.Context, record *mobileRecord, input map[string]any) error {
	if record.Connection == "" {
		return errors.New("会话连接尚未就绪，请稍后重试")
	}
	input["cli_id"], input["connection"] = record.CLIID, record.Connection
	if err := s.windowsConversationInput(record, input); err != nil {
		return err
	}
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.runCLIChannel(ctx, "rpc", input, &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Error)
	}
	return nil
}

func (s *Server) prepareTerminalConversation(workspace string, selection claudeSelection, existing string) (string, *mobileRecord, error) {
	directories, err := s.projectAdditionalDirectories(workspace)
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	launcher := claudeTerminalCommand(s.preferences)
	s.mu.Unlock()
	s.claudeAccounts.mu.Lock()
	list, loadErr := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if loadErr != nil {
		return "", nil, loadErr
	}
	m := &s.mobileCLI
	id := existing
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := m.create(s.profile, workspace, id); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.record(s.profile, id)
	if err != nil {
		return "", nil, err
	}
	if s.windowsGuest == nil && record.Terminal && record.Launcher != "" {
		launcher = record.Launcher
	}
	legacyWorkspace := ""
	if existing != "" {
		legacyWorkspace = s.macOSLegacyWorkspace(record)
	}
	if selection.ID == "" && selection.Subject == "" {
		selection.GuestID = ""
	}
	prefix, err := mobileLaunchArgs(launcher, selection)
	if err != nil {
		return "", nil, err
	}
	record.Terminal, record.Account, record.Launcher = true, selection, launcher
	record.TerminalApplication = "cage"
	// A new session follows the rule in 调度设置 until the person sets its own.
	if record.AccountLabel == "" {
		if a := list.find(selection.ID); a != nil {
			record.AccountLabel = a.Label
		}
	}
	record.TerminalRunning, record.TerminalClosed = true, false
	record.Connection = uuid.NewString()
	if record.ResumeTranscript != "" && record.Switch != nil {
		record.Connection = record.Switch.ID
	}
	payload := map[string]any{"id": record.Connection, "session_id": record.CLIID, "prefix": prefix, "config": record.GuestConfig}
	if len(directories) > 0 {
		payload["additional_directories"] = directories
	}
	if record.Resume {
		payload["resume"] = record.CLIID
	}
	if record.ResumeTranscript != "" {
		payload["resume_transcript"] = record.ResumeTranscript
		payload["resume"] = ""
		payload["switch_id"] = record.Switch.ID
		payload["continue_after_switch"] = record.Switch.Continue
		payload["note"] = record.Switch.Note
	}
	if s.macosGuest != nil && record.GuestWorkspace != "" {
		root := s.cliWorkspace(workspace)
		if legacyWorkspace != "" {
			root = legacyWorkspace
		}
		if !cliWorkspaceWithin(record.GuestWorkspace, root, false) {
			return "", nil, errors.New("会话目录不在已授权的 macOS 项目内")
		}
		payload["project_subdir"] = strings.TrimPrefix(strings.TrimPrefix(record.GuestWorkspace, root), "/")
	}
	data, _ := json.Marshal(payload)
	record.CanSend = new(bool)
	record.ConnectionNotice = ""
	if legacyWorkspace != "" {
		record.ConnectionNotice = macOSLegacyWorkspaceNotice
	}
	if s.windowsGuest != nil {
		if s.windowsGuest.Remote() {
			home, err := s.remoteWindowsCLIWorkspace(workspace)
			if err != nil {
				return "", nil, err
			}
			record.GuestWorkspace = home
			if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
				return "", nil, err
			}
			return string(data), record, nil
		}
		binding, err := s.windowsProjectForHost(workspace)
		if err != nil {
			return "", nil, err
		}
		record.GuestWorkspace = binding.Path
		if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
			return "", nil, err
		}
		return string(data), record, nil
	}
	// Only status 1: what every refusal gives (the CLI, the account wrapper, the
	// helper); a CLI ended by a signal (a hand-over: 143) or left (0) says nothing.
	body := s.cliHelperCommand("launch", string(data)) + "; [ $? -ne 1 ] || printf '\\n%s\\n' " + shellescape.Quote(terminalLaunchFailed) + "; exec bash -i"
	root := s.cliWorkspace(workspace)
	directory := record.GuestWorkspace
	if directory == "" {
		directory = root
	}
	record.GuestWorkspace = directory
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		return "", nil, err
	}
	if s.macosGuest != nil {
		if legacyWorkspace != "" {
			return "cd -- " + shellescape.Quote(legacyWorkspace) + " && exec " + s.cliHelperCommand("macos-launch", string(data)), record, nil
		}
		return s.macOSClaudeLaunch(workspace, string(data)), record, nil
	}
	script := terminalDirectory(directory, root) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 bash -lic " + shellescape.Quote(body)
	return script, record, nil
}

// terminalLaunchFailed follows a launch that ended with status 1 (the CLI exits
// 0 when the person leaves it): its own message is just above.
const terminalLaunchFailed = "Super Cage：Claude Code 未能启动或接续这段会话，原因见上方。会话仍在列表中，可以再次打开；这个终端现在是普通命令行。"

// terminalDirectory enters the folder the conversation last worked in, or the
// project when that folder is gone (a removed worktree): the CLI finds the
// transcript by its id from any folder, so the resume goes on and says where.
func terminalDirectory(directory, root string) string {
	if directory == root {
		return "cd " + shellescape.Quote(root)
	}
	notice := shellescape.Quote("会话上次所在的文件夹已不存在，已在项目文件夹中继续：" + root)
	return "{ cd " + shellescape.Quote(directory) + " 2>/dev/null || { cd " + shellescape.Quote(root) + " && printf '%s\n' " + notice + "; }; }"
}

func (s *Server) isTerminalConversation(id string) bool {
	s.mobileCLI.mu.Lock()
	defer s.mobileCLI.mu.Unlock()
	record, err := s.mobileCLI.record(s.profile, id)
	return err == nil && record.Terminal
}

func (s *Server) stopTerminalConversation(ctx context.Context, view mobileSession, turnID string) (mobileSession, error) {
	if len(view.Turns) == 0 || view.Turns[len(view.Turns)-1].ID != turnID || view.Turns[len(view.Turns)-1].Status != "running" {
		return view, errors.New("本轮运行状态已变化，请重新读取")
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, view.ID)
	s.mobileCLI.mu.Unlock()
	if err != nil {
		return view, err
	}
	// The view was read from the guest for this request and shows turnID
	// running. One Esc per turn: a repeated or concurrent Stop before the
	// interrupt reaches the transcript sends none, since a second Esc on an
	// idle composer would open "edit previous message".
	escape := func(terminal *terminalSession) error {
		s.account.mu.Lock()
		defer s.account.mu.Unlock()
		if !s.mobileAuthorized(ctx) {
			return errMobileLocked
		}
		at := time.Now()
		s.mobileCLI.mu.Lock()
		claimed := s.mobileCLI.claimTerminalStopLocked(view.ID, turnID, at)
		s.mobileCLI.mu.Unlock()
		if !claimed {
			return nil
		}
		err := terminal.enqueueInput(ctx, "\x1b")
		if err != nil {
			s.mobileCLI.mu.Lock()
			s.mobileCLI.releaseTerminalStopLocked(view.ID, turnID, at)
			s.mobileCLI.mu.Unlock()
		}
		return err
	}
	if record.codex() {
		// A turn of the desktop's Codex TUI stops as it would at the keyboard:
		// Esc interrupts it (and declines an open approval).
		if terminal := s.terminals.boundCodex(record.ID); terminal != nil {
			if err := escape(terminal); err != nil {
				return view, err
			}
			return s.terminalConversationView(ctx, view.ID)
		}
		if view.Owner == "external" {
			return view, errors.New("这一轮不在 Super Cage 的终端中运行，请在运行它的终端中停止")
		}
		return s.terminalConversationAction(ctx, view.ID, map[string]any{"action": "stop", "request_id": turnID})
	}
	terminal := s.terminals.get(record.TerminalID)
	if terminal == nil {
		return view, errors.New("请在电脑终端停止这次运行")
	}
	if err := escape(terminal); err != nil {
		return view, err
	}
	return s.terminalConversationAction(ctx, view.ID, map[string]any{"action": "stop"})
}
