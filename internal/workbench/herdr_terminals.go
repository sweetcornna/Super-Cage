package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The hc terminals Cage lists (GET /api/terminals), and which of them are
// Cage's own.
//
//   - Cage's own: the workbench records every hc terminal it creates in
//     herdr-terminals.json (terminal id -> hc name cage-<id>). The record
//     decides, never a label: anything in the guest can write labels, which
//     are display hints only. A Cage terminal keeps its id across a
//     cage-serve restart and opens the same hc terminal again.
//   - Others: each live hc terminal of this machine without a record, listed
//     with the id h(address) and opened by its address. Another machine's
//     (machine/t7) is never listed or opened.
//
// A listed terminal without a session opens on its stream: the stream starts
// a viewer (hc attach) under the same id. One herdr-list per list request,
// reused for herdrListTTL, only while Herdr is enabled and installed:
// nothing polls and nothing watches while nobody lists terminals.

const (
	herdrListTTL       = 2 * time.Second
	herdrImportedLimit = 12
	herdrListMax       = 64
)

var (
	herdrNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	herdrAddressPattern = regexp.MustCompile(`^t[0-9a-z]{1,15}$`)
	herdrIDPattern      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	herdrLabelKeys      = []string{"cage", "cage.project", "cage.kind", "cage.title"}
)

// herdrRecord is what the workbench knows about an hc terminal it created.
// Address is learned from the first list that shows the name: a later
// terminal under the same name at another address is not this one.
type herdrRecord struct {
	Name      string `json:"name"`
	Address   string `json:"address,omitempty"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Workspace string `json:"workspace"`
	AgentID   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
	Created   int64  `json:"created"`
}

// herdrEntry is one live hc terminal as agent-runtime.py herdr-list reports
// it. Everything in it is written by the guest.
type herdrEntry struct {
	Address   string            `json:"address"`
	Name      string            `json:"name"`
	Title     string            `json:"title"`
	Program   string            `json:"program"`
	Cwd       string            `json:"cwd"`
	CreatedMS int64             `json:"created_ms"`
	Labels    map[string]string `json:"labels"`
}

type herdrList struct {
	Running   bool         `json:"running"`
	Terminals []herdrEntry `json:"terminals"`
}

func herdrTerminalName(id string) string { return "cage-" + id }

// herdrImportedID is the terminal id of an hc terminal another client
// started: stable for its address, in the shape of every terminal id.
func herdrImportedID(address string) string {
	sum := sha256.Sum256([]byte("herdr:" + address))
	return hex.EncodeToString(sum[:8])
}

// herdrText strips control and format characters (bidi overrides too) from
// a guest-written string and caps it at limit characters.
func herdrText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	clean := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value))
	if utf8.RuneCountInString(clean) > limit {
		clean = strings.TrimSpace(string([]rune(clean)[:limit]))
	}
	return clean
}

// cleanHerdrList keeps what Cage may show or use from a guest's list.
func cleanHerdrList(list herdrList) herdrList {
	out := herdrList{Running: list.Running}
	for _, entry := range list.Terminals {
		if !herdrAddressPattern.MatchString(entry.Address) {
			continue
		}
		clean := herdrEntry{
			Address:   entry.Address,
			Title:     herdrText(entry.Title, 80),
			Program:   herdrText(entry.Program, 40),
			Cwd:       herdrText(entry.Cwd, 1024),
			CreatedMS: max(entry.CreatedMS, 0),
			Labels:    map[string]string{},
		}
		if herdrNamePattern.MatchString(entry.Name) {
			clean.Name = entry.Name
		}
		for _, key := range herdrLabelKeys {
			if value, ok := entry.Labels[key]; ok {
				clean.Labels[key] = herdrText(value, 80)
			}
		}
		out.Terminals = append(out.Terminals, clean)
		if len(out.Terminals) >= herdrListMax {
			break
		}
	}
	return out
}

// herdrRecordsLocked loads the records once; a record that does not name
// cage-<its id> is dropped. Caller holds s.herdr.listMu.
func (s *Server) herdrRecordsLocked() map[string]herdrRecord {
	m := &s.herdr
	if m.recordsOK {
		return m.records
	}
	m.records = map[string]herdrRecord{}
	var saved map[string]herdrRecord
	if data, err := os.ReadFile(filepath.Join(s.profile, "herdr-terminals.json")); err == nil && json.Unmarshal(data, &saved) == nil {
		for id, record := range saved {
			if herdrIDPattern.MatchString(id) && record.Name == herdrTerminalName(id) {
				m.records[id] = record
			}
		}
	}
	m.recordsOK = true
	return m.records
}

func (s *Server) saveHerdrRecordsLocked() {
	_ = writePrivate(filepath.Join(s.profile, "herdr-terminals.json"), s.herdr.records)
}

// rememberHerdrTerminal records an hc terminal Cage created.
func (s *Server) rememberHerdrTerminal(id string, record herdrRecord) {
	s.herdr.listMu.Lock()
	defer s.herdr.listMu.Unlock()
	s.herdrRecordsLocked()[id] = record
	s.saveHerdrRecordsLocked()
	s.herdr.listAt = time.Time{}
}

// rememberHerdrSession records the hc terminal a new session created.
func (s *Server) rememberHerdrSession(session *terminalSession, kind string) {
	session.mu.Lock()
	record := herdrRecord{Name: session.herdrName, Title: session.title, Kind: kind, Workspace: session.workspace,
		AgentID: session.agentID, AgentName: session.agentName, Created: time.Now().Unix()}
	id := session.id
	session.mu.Unlock()
	s.rememberHerdrTerminal(id, record)
}

func (s *Server) herdrRecord(id string) (herdrRecord, bool) {
	s.herdr.listMu.Lock()
	defer s.herdr.listMu.Unlock()
	record, ok := s.herdrRecordsLocked()[id]
	return record, ok
}

func (s *Server) forgetHerdrTerminal(id string) {
	s.herdr.listMu.Lock()
	defer s.herdr.listMu.Unlock()
	records := s.herdrRecordsLocked()
	if _, ok := records[id]; ok {
		delete(records, id)
		s.saveHerdrRecordsLocked()
	}
}

func (s *Server) invalidateHerdrList() {
	s.herdr.listMu.Lock()
	s.herdr.listAt = time.Time{}
	s.herdr.listMu.Unlock()
}

// reconcileHerdrRecordsLocked drops the records of terminals a running core
// no longer has, or whose name now belongs to another address. A core that
// is down keeps them: it may come back with its terminals (hc keeps them
// across a core crash).
func (s *Server) reconcileHerdrRecordsLocked(list herdrList) {
	if !list.Running {
		return
	}
	byName := map[string]string{}
	for _, entry := range list.Terminals {
		if entry.Name != "" {
			byName[entry.Name] = entry.Address
		}
	}
	records := s.herdrRecordsLocked()
	changed := false
	for id, record := range records {
		address, live := byName[record.Name]
		switch {
		case !live || (record.Address != "" && record.Address != address):
			delete(records, id)
			changed = true
		case record.Address == "":
			record.Address = address
			records[id] = record
			changed = true
		}
	}
	if changed {
		s.saveHerdrRecordsLocked()
	}
}

// herdrListing reports whether listing terminals reads hc's: Herdr enabled,
// installed, and the environment up. No guest call decides it.
func (s *Server) herdrListing() bool {
	if s.windowsGuest != nil {
		return false
	}
	cfg, err := s.herdrConfig()
	if err != nil || !cfg.Enabled {
		return false
	}
	s.herdr.mu.Lock()
	installed := cfg.InstalledHint
	if !s.herdr.statusAt.IsZero() && time.Since(s.herdr.statusAt) < herdrStatusTTL {
		installed = s.herdr.statusCache.Installed
	}
	s.herdr.mu.Unlock()
	if !installed || !s.vmRunning() {
		return false
	}
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	return ready
}

// herdrTerminals is the guest's live hc terminals, read at most once per
// herdrListTTL; concurrent readers wait for the one read.
func (s *Server) herdrTerminals(ctx context.Context) (herdrList, error) {
	s.herdr.fetchMu.Lock()
	defer s.herdr.fetchMu.Unlock()
	s.herdr.listMu.Lock()
	if !s.herdr.listAt.IsZero() && time.Since(s.herdr.listAt) < herdrListTTL {
		list := s.herdr.list
		s.herdr.listMu.Unlock()
		return list, nil
	}
	s.herdr.listMu.Unlock()
	var raw herdrList
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := s.runAgentRuntime(ctx, "herdr-list", map[string]any{}, &raw)
	cancel()
	if err != nil {
		return herdrList{}, err
	}
	list := cleanHerdrList(raw)
	s.herdr.listMu.Lock()
	s.reconcileHerdrRecordsLocked(list)
	s.herdr.list, s.herdr.listAt = list, time.Now()
	s.herdr.listMu.Unlock()
	return list, nil
}

// herdrView is how one listed hc terminal appears in the terminal list, and
// what opening it attaches to.
type herdrView struct {
	ID, Target, Title, Kind, Workspace string
	AgentID, AgentName                 string
	Imported                           bool
}

// herdrViews pairs the live hc terminals with Cage's records.
func (s *Server) herdrViews(list herdrList) []herdrView {
	s.herdr.listMu.Lock()
	records := s.herdrRecordsLocked()
	own := make(map[string]string, len(records))
	for id, record := range records {
		own[record.Name] = id
	}
	views := make([]herdrView, 0, len(list.Terminals))
	for _, entry := range list.Terminals {
		if id, ok := own[entry.Name]; ok && entry.Name != "" {
			record := records[id]
			views = append(views, herdrView{ID: id, Target: record.Name, Title: record.Title, Kind: record.Kind,
				Workspace: record.Workspace, AgentID: record.AgentID, AgentName: record.AgentName})
			continue
		}
		views = append(views, herdrView{ID: herdrImportedID(entry.Address), Target: entry.Address,
			Title: herdrImportedTitle(entry), Kind: "shell", Workspace: entry.Cwd, Imported: true})
	}
	s.herdr.listMu.Unlock()
	for i := range views {
		if views[i].Imported {
			views[i].Workspace = s.herdrWorkspaceFor(views[i].Workspace)
		}
	}
	return views
}

// herdrImportedTitle names another client's terminal: its own title, its
// name, its program, or its address.
func herdrImportedTitle(entry herdrEntry) string {
	for _, title := range []string{entry.Title, entry.Name, entry.Program} {
		if title != "" {
			return herdrText(title, 48)
		}
	}
	return "Herdr " + entry.Address
}

// herdrWorkspaceFor is the project whose guest folder holds cwd, or the
// current project.
func (s *Server) herdrWorkspaceFor(cwd string) string {
	best, bestLen := "", 0
	if paths, err := s.projectPaths(); err == nil {
		for _, path := range paths {
			guest := strings.TrimRight(s.cliWorkspace(path), "/")
			if guest == "" || len(guest) <= bestLen {
				continue
			}
			if cwd == guest || strings.HasPrefix(cwd, guest+"/") {
				best, bestLen = path, len(guest)
			}
		}
	}
	if best == "" {
		return s.workspace()
	}
	return best
}

// withHerdrTerminals adds the live hc terminals that have no running
// session to list, and marks an ended session whose hc terminal lives on.
func (s *Server) withHerdrTerminals(ctx context.Context, list []map[string]any) []map[string]any {
	if !s.herdrListing() {
		return list
	}
	hc, err := s.herdrTerminals(ctx)
	if err != nil || !hc.Running {
		return list
	}
	byID := make(map[string]map[string]any, len(list))
	for _, item := range list {
		if id, _ := item["id"].(string); id != "" {
			byID[id] = item
		}
	}
	for _, view := range s.herdrViews(hc) {
		if item, ok := byID[view.ID]; ok {
			if item["closed"] == true {
				item["herdr_live"] = true
			}
			continue
		}
		entry := map[string]any{"id": view.ID, "title": view.Title, "closed": false, "workspace": view.Workspace, "kind": view.Kind,
			"claude_account_id": "", "claude_account_source": "", "cli_session_id": "", "codex_account_id": "", "codex_account_source": "",
			"cli_starting": false, "herdr_backed": true, "herdr_detached": true}
		if view.Imported {
			entry["herdr_imported"] = true
		}
		if view.AgentID != "" {
			entry["agent_id"], entry["agent_name"] = view.AgentID, view.AgentName
		}
		list = append(list, entry)
	}
	return list
}

// herdrViewFor is what id opens: the hc terminal its ended session viewed,
// its record, or a listed terminal of another client. ok is false when id
// is no live hc terminal.
func (s *Server) herdrViewFor(ctx context.Context, id string, old *terminalSession) (herdrView, bool) {
	if !s.herdrListing() {
		return herdrView{}, false
	}
	hc, err := s.herdrTerminals(ctx)
	if err != nil || !hc.Running {
		return herdrView{}, false
	}
	for _, view := range s.herdrViews(hc) {
		if view.ID != id {
			continue
		}
		if old != nil {
			old.mu.Lock()
			name, title := old.herdrName, old.title
			old.mu.Unlock()
			if name != view.Target {
				return herdrView{}, false
			}
			view.Title = title
		}
		return view, true
	}
	return herdrView{}, false
}

// openHerdrViewer gives the stream of id a running session: the session
// itself while it runs; otherwise, when id is a live hc terminal (its
// session's ssh dropped, cage-serve restarted, or another client started
// it), a new viewer of that terminal under the same id. It returns the
// session as it was when id is no Herdr terminal.
func (s *Server) openHerdrViewer(ctx context.Context, id string) (*terminalSession, error) {
	old := s.terminals.get(id)
	if old != nil {
		old.mu.Lock()
		viewing := old.herdrName != ""
		old.mu.Unlock()
		if !old.finished() || !viewing {
			return old, nil
		}
	} else if !herdrIDPattern.MatchString(id) {
		return nil, nil
	}
	view, ok := s.herdrViewFor(ctx, id, old)
	if !ok {
		return old, nil
	}
	if !s.op.TryLock() {
		return nil, errTerminalBusy
	}
	defer s.op.Unlock()
	if current := s.terminals.get(id); current != nil && !current.finished() {
		return current, nil
	} else if current != nil {
		s.terminals.remove(id)
	}
	s.mu.Lock()
	closing, ready := s.closing, s.ready
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if !ready || !s.vmRunning() || s.busy() {
		return nil, errTerminalNotReady
	}
	workspace := view.Workspace
	if workspace == "" {
		workspace = s.workspace()
	}
	guestWorkspace := s.cliWorkspace(workspace)
	script := s.herdrAttachScript(herdrAttach{Mode: "attach", Target: view.Target, Workspace: guestWorkspace,
		Project: herdrProjectLabel(workspace), Title: view.Title, Kind: view.Kind})
	ensureCtx, ensureCancel := context.WithTimeout(s.lifetimeContext(), 15*time.Second)
	ensureErr := s.ensureAgentRuntime(ensureCtx)
	ensureCancel()
	if ensureErr != nil {
		return nil, errors.New("无法在运行环境中准备 Herdr 辅助程序，请确认环境已启动")
	}
	cmd := s.sshCommand(s.lifetimeContext(), "-tt", s.sshTarget(), "--", script)
	return s.terminals.startHerdrViewer(cmd, workspace, view.Kind, view.Imported, func(session *terminalSession, _ int) {
		session.id = id
		if view.Title != "" {
			session.title = view.Title
		}
		session.herdrBacked, session.herdrName, session.herdrImported = true, view.Target, view.Imported
		session.agentID, session.agentName = view.AgentID, view.AgentName
		// Far past any offset a viewer of an earlier session holds, so a
		// stale resume restarts from the kept history (terminalOutput).
		session.written = herdrOffsetBase()
	})
}

// herdrOffsetBase grows with the clock (1 KiB per millisecond), so each new
// viewer session starts past every offset an earlier one could have
// reached, and stays well inside a JavaScript number.
func herdrOffsetBase() uint64 { return uint64(time.Now().UnixMilli()) << 10 }

// endHerdrTerminal ends the hc terminal behind a terminal the user closed:
// kill, then rm, in the guest. A dropped viewer never comes here.
func (s *Server) endHerdrTerminal(id string, session *terminalSession) {
	target := ""
	if session != nil {
		session.mu.Lock()
		target = session.herdrName
		session.mu.Unlock()
	}
	if record, ok := s.herdrRecord(id); ok && target == "" {
		target = record.Name
	}
	if target == "" && s.herdrListing() {
		s.herdr.listMu.Lock()
		list := s.herdr.list
		s.herdr.listMu.Unlock()
		for _, entry := range list.Terminals {
			if herdrImportedID(entry.Address) == id {
				target = entry.Address
			}
		}
	}
	s.forgetHerdrTerminal(id)
	if target == "" || !(herdrNamePattern.MatchString(target) || herdrAddressPattern.MatchString(target)) || !s.vmRunning() {
		return
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 10*time.Second)
	_ = s.runAgentRuntime(ctx, "herdr-close", map[string]any{"terminal": target}, nil)
	cancel()
	s.invalidateHerdrList()
}

var errHerdrTerminalLimit = errors.New("最多同时打开 12 个其他 Herdr 客户端的终端，请先关闭不用的")
