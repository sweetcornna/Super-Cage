package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var errTerminalRecoveryUnavailable = errors.New("无法恢复原终端。请在会话列表中确认原会话和登录状态")

func syncTerminalStateDirectory(path string) error {
	// Windows does not provide POSIX-style fsync for an open directory.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// A recovery request names the last PTY and its durable CLI conversation.
// Only the server's record can associate the two. Plain shells and external
// application sessions have no Cage CLI binding.
func (r *mobileRecord) canRecoverTerminal(terminal string) bool {
	return r != nil && r.Terminal && r.TerminalID == terminal && mobileTerminalID.MatchString(terminal) &&
		!r.TerminalClosed && !r.Archived && !r.Deleted && !r.Deleting &&
		(r.TerminalApplication == "" || r.TerminalApplication == "cage") &&
		(r.Provider == "" || r.Provider == "claude" || r.codex())
}

func (r *mobileRecord) wasTerminalRunning() bool {
	return r.TerminalRunning || r.codex() && r.Owner == "desktop"
}

func (r *mobileRecord) canResumeClaudeWithoutInput() bool {
	if r.ResumeTranscript != "" {
		return false
	}
	if r.Switch == nil {
		return true
	}
	if r.Switch.Phase != "completed" {
		return false
	}
	for _, message := range r.Switch.Carry {
		if !message.Delivered {
			return false
		}
	}
	return true
}

func (input terminalRequest) setupRecoveredTerminal(session *terminalSession, _ int) {
	if input.recoverTerminal != "" {
		session.id = input.recoverTerminal
		// The replacement PTY has a different stream. Old viewers must reset
		// their output offsets before they draw or answer any of its output.
		session.written = herdrOffsetBase()
	}
}

// An ended Codex terminal can be dismissed without archiving its conversation.
// Preserve that choice independently of terminal state or a client's cache.
func (s *Server) dismissTerminalRecovery(id, terminal string) error {
	s.op.Lock()
	defer s.op.Unlock()
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.record(s.profile, id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || record.TerminalID != terminal {
		return err
	}
	record.TerminalClosed = true
	return writePrivate(mobileRecordPath(s.profile, id), record)
}

// A close can arrive from a viewer that still knows the terminal while the
// replacement controller has not attached it yet.
func (s *Server) dismissDetachedTerminalRecovery(terminal string) error {
	if !mobileTerminalID.MatchString(terminal) {
		return nil
	}
	s.op.Lock()
	defer s.op.Unlock()
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := os.ReadDir(filepath.Dir(mobileRecordPath(s.profile, "")))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !mobileID(id) {
			continue
		}
		record, err := m.record(s.profile, id)
		if err == nil && record.canRecoverTerminal(terminal) {
			record.TerminalClosed = true
			return writePrivate(mobileRecordPath(s.profile, id), record)
		}
	}
	return nil
}

// restoreWorkbenchSessions runs after a controller brings the guest back
// under an unlocked account. Herdr PTYs can already be attached by their
// durable terminal ID. Only Cage CLI terminals with a prior active binding
// and verified native history can start a replacement PTY.
func (s *Server) restoreWorkbenchSessions(ctx context.Context) error {
	epoch, unlocked := s.account.access()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	if !unlocked || !ready {
		return nil
	}
	ctx, cancel := context.WithCancel(context.WithValue(ctx, accountEpochKey{}, epoch))
	stop := context.AfterFunc(epoch, cancel)
	defer stop()
	defer cancel()
	paths, err := s.projectPaths()
	if err != nil {
		return err
	}
	granted := make(map[string]bool, len(paths))
	for _, path := range paths {
		granted[path] = true
	}
	m := &s.mobileCLI
	summaries, err := m.summaries(s.profile, granted, false)
	if err != nil {
		return err
	}
	var problems []error
	for _, summary := range summaries {
		if ctx.Err() != nil {
			return errors.Join(append(problems, ctx.Err())...)
		}
		if err := s.restoreWorkbenchTerminal(ctx, summary.ID); err != nil && !errors.Is(err, errTerminalRecoveryUnavailable) {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func (s *Server) restoreWorkbenchTerminal(ctx context.Context, id string) error {
	s.mobileConnectMu.Lock()
	defer s.mobileConnectMu.Unlock()
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if !record.canRecoverTerminal(record.TerminalID) || !record.wasTerminalRunning() {
		return nil
	}
	// A listed terminal keeps its existing viewer path, even if its Claude
	// process left the surrounding shell running.
	if s.terminals.get(record.TerminalID) != nil {
		return nil
	}
	input := terminalRequest{Workspace: record.Workspace, Kind: "claude", sharedID: id,
		requestContext: ctx, recoverTerminal: record.TerminalID}
	if record.codex() {
		input.Kind = "codex"
	} else {
		if !record.canResumeClaudeWithoutInput() {
			return nil
		}
		view, err := s.terminalConversationView(ctx, id)
		if err != nil {
			return err
		}
		if view.Running || view.nativeResumeID == "" {
			return nil
		}
		var active struct {
			Running bool   `json:"running"`
			Error   string `json:"error"`
		}
		query := map[string]any{"cli_id": view.nativeResumeID, "config": record.GuestConfig,
			"workspace": record.GuestWorkspace, "account": record.Account.GuestID}
		if err := s.windowsConversationInput(record, query); err != nil {
			return err
		}
		if err := s.runCLIChannel(ctx, "active", query, &active); err != nil {
			return err
		}
		if active.Error != "" {
			return errors.New(active.Error)
		}
		if active.Running {
			return nil
		}
		input.recoverCLIID = view.nativeResumeID
	}
	_, err = s.createTerminal(input)
	return err
}
