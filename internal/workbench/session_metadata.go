package workbench

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"
)

type sessionMetadataInput struct {
	Title  *string `json:"title"`
	Pinned *bool   `json:"pinned"`
}

func (s *Server) updateSessionMetadata(id string, input sessionMetadataInput, requestContext context.Context) (mobileSession, error) {
	if !s.op.TryLock() {
		return mobileSession{}, errTerminalBusy
	}
	defer s.op.Unlock()
	if input.Title == nil && input.Pinned == nil {
		return mobileSession{}, errors.New("请选择要修改的会话信息")
	}
	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > 100 {
			return mobileSession{}, errors.New("会话名称需要 1 至 100 个字符")
		}
		input.Title = &title
	}
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	if requestContext != nil {
		s.account.mu.Lock()
		defer s.account.mu.Unlock()
		if !s.mobileAuthorized(requestContext) {
			return mobileSession{}, errMobileLocked
		}
	}
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.record(s.profile, id)
	if err != nil {
		return mobileSession{}, err
	}
	if record.Deleted {
		return mobileSession{}, os.ErrNotExist
	}
	previous := *record
	if input.Title != nil {
		record.CustomTitle = *input.Title
	}
	if input.Pinned != nil {
		record.Pinned = *input.Pinned
	}
	record.MetadataSet = true
	record.changed()
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		*record = previous
		return mobileSession{}, err
	}
	if m.catalog != nil {
		m.catalog[id] = record.summary()
	}
	return record.snapshot(), nil
}

// Archive state belongs to the shared catalog and survives terminal exits.
func (s *Server) setSessionArchived(id string, archived bool, requestContext context.Context) error {
	if requestContext != nil && requestContext.Err() != nil {
		return requestContext.Err()
	}
	wait := requestContext
	if wait == nil {
		wait = s.lifetimeContext()
	}
	m := &s.mobileCLI
	// Serialize with terminal creation and teardown, but let an explicit archive
	// wait for background network probes instead of rejecting the user's click.
	// An earlier archive's guest stop of this session runs after the lock
	// (below); a restore or another archive waits for it, so it never stops
	// a turn started after the restore.
	for {
		s.op.Lock()
		m.mu.Lock()
		pending := m.archiveStops[id]
		m.mu.Unlock()
		if pending == nil {
			break
		}
		s.op.Unlock()
		select {
		case <-pending:
		case <-wait.Done():
			return wait.Err()
		}
	}
	connection, stopping, err := s.setSessionArchivedLocked(id, archived, requestContext)
	if err != nil || stopping == nil {
		return err
	}
	defer func() {
		m.mu.Lock()
		delete(m.archiveStops, id)
		m.mu.Unlock()
		close(stopping)
	}()
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	// The guest's stop waits for Codex to exit (up to 8 s), so it runs after
	// the operation lock: the record is archived and its terminals are gone,
	// so neither a phone message nor a desktop open can start a turn on the
	// session meanwhile. The archive stands either way; a turn the guest
	// could not stop finishes on its own and no new one can start.
	if err := s.runCodexSessions(wait, "stop", map[string]any{"connection": connection, "reason": "archive"}, &result); err == nil && result.OK {
		m.mu.Lock()
		delete(m.codexOwner, id)
		delete(m.codexLive, id)
		m.mu.Unlock()
	}
	return nil
}

// setSessionArchivedLocked is setSessionArchived under the operation lock,
// which the caller holds and it releases. For a Codex session with a phone
// turn to stop in the guest, it returns the connection and a channel
// registered in archiveStops that the caller closes after the stop.
func (s *Server) setSessionArchivedLocked(id string, archived bool, requestContext context.Context) (string, chan struct{}, error) {
	defer s.op.Unlock()
	if requestContext != nil && requestContext.Err() != nil {
		return "", nil, requestContext.Err()
	}
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	m := &s.mobileCLI
	if requestContext != nil {
		s.account.mu.Lock()
		if !s.mobileAuthorized(requestContext) {
			s.account.mu.Unlock()
			return "", nil, errMobileLocked
		}
	}
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	if err != nil || record.Deleted {
		m.mu.Unlock()
		if requestContext != nil {
			s.account.mu.Unlock()
		}
		if err != nil {
			return "", nil, err
		}
		return "", nil, os.ErrNotExist
	}
	previous := *record
	if record.Deleting && !archived {
		m.mu.Unlock()
		if requestContext != nil {
			s.account.mu.Unlock()
		}
		return "", nil, errors.New("永久删除尚未完成，请先重试删除")
	}
	record.Archived = archived
	record.changed()
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		*record = previous
		m.mu.Unlock()
		if requestContext != nil {
			s.account.mu.Unlock()
		}
		return "", nil, err
	}
	if m.catalog != nil {
		m.catalog[id] = record.summary()
	}
	var done chan struct{}
	if run := m.active[id]; archived && run != nil && run.cancel != nil {
		run.cancel()
		done = run.done
	}
	terminalID := record.TerminalID
	// A phone turn of a Codex session runs in the guest; archiving stops it.
	// The guest is asked even when this server has not seen the turn (after
	// a restart); it stops only a turn of its own.
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	codexRun := archived && record.codex() && record.Connection != "" && ready
	connection := record.Connection
	var stopping chan struct{}
	if codexRun {
		stopping = make(chan struct{})
		if m.archiveStops == nil {
			m.archiveStops = map[string]chan struct{}{}
		}
		m.archiveStops[id] = stopping
	}
	m.mu.Unlock()
	if requestContext != nil {
		s.account.mu.Unlock()
	}
	if terminal := s.terminals.get(terminalID); archived && terminal != nil {
		if terminal.summary()["cli_session_id"] == id {
			s.terminals.remove(terminalID)
		}
	}
	for archived && record.codex() {
		terminal := s.terminals.boundCodex(id)
		if terminal == nil {
			break
		}
		s.terminals.remove(terminal.id)
	}
	if done != nil {
		<-done
	}
	return connection, stopping, nil
}

func (s *Server) mobileSessionMetadata(w http.ResponseWriter, r *http.Request) {
	view, _, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return
	}
	var input sessionMetadataInput
	if !readJSON(w, r, &input) {
		return
	}
	updated, err := s.updateSessionMetadata(view.ID, input, r.Context())
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, updated)
}

func (s *Server) mobileArchiveSession(w http.ResponseWriter, r *http.Request) {
	s.changeSessionArchive(w, r, true)
}

func (s *Server) mobileRestoreSession(w http.ResponseWriter, r *http.Request) {
	s.changeSessionArchive(w, r, false)
}

func (s *Server) changeSessionArchive(w http.ResponseWriter, r *http.Request, archived bool) {
	view, _, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return
	}
	var input struct{}
	if !readJSON(w, r, &input) {
		return
	}
	// The phone may watch and stop a desktop Codex session, never close it.
	if archived && view.Provider == codexProvider && strings.HasPrefix(r.URL.Path, "/api/mobile/") && s.terminals.boundCodex(view.ID) != nil {
		http.Error(w, errCodexDesktopTerminal.Error(), http.StatusConflict)
		return
	}
	if err := s.setSessionArchived(view.ID, archived, r.Context()); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

func (s *Server) mobilePurgeSession(w http.ResponseWriter, r *http.Request) {
	// A separate endpoint keeps old clients' "delete" requests reversible.
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !input.Confirm {
		http.Error(w, "需要确认永久删除该会话及其原始历史", 400)
		return
	}
	if err := s.purgeSession(r.Context(), r.PathValue("id")); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

func (s *Server) purgeSession(ctx context.Context, id string) error {
	if !s.op.TryLock() {
		return errTerminalBusy
	}
	defer s.op.Unlock()
	s.conversationMu.Lock()
	defer s.conversationMu.Unlock()
	s.account.mu.Lock()
	authorized := s.mobileAuthorized(ctx)
	s.account.mu.Unlock()
	if !authorized {
		return errMobileLocked
	}
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := s.projectWorkspace(record.Workspace); err != nil {
		return err
	}
	if record.Purged {
		return nil
	}
	if s.windowsGuest != nil && !record.Terminal && (record.Resume || len(record.Turns) > 0) {
		return errWindowsLegacyMobile
	}
	if !record.Archived {
		return errors.New("请先归档会话，再从已归档会话中永久删除")
	}
	// Once deletion is admitted, a retry must finish it before restoration can
	// reopen a transcript that may already be partly removed from the guest.
	record.Deleting = true
	record.changed()
	m.mu.Lock()
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.catalog != nil {
		m.catalog[id] = record.summary()
	}
	m.mu.Unlock()
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	executions := append([]claudeExecution{}, record.Executions...)
	executions = append(executions, claudeExecution{record.CLIID, record.Account, record.GuestConfig})
	if record.codex() {
		if err := s.purgeCodexConversation(ctx, record); err != nil {
			return err
		}
		executions = nil
	}
	for _, execution := range executions {
		input := map[string]any{
			"cli_id": execution.CLIID, "config": execution.Config,
			"account": execution.Account.GuestID, "workspace": guestPath(record.Workspace),
			"expect_history": execution.CLIID != record.CLIID || record.Resume,
		}
		if err := s.windowsConversationInput(record, input); err != nil {
			return err
		}
		if err := s.runCLIChannel(ctx, "purge", input, &result); err != nil {
			return err
		}
		if !result.OK {
			return errors.New(result.Error)
		}
	}
	// Keep only the identity needed to make retries and discovery idempotent.
	// If this write fails, the archived record can safely retry guest cleanup.
	tombstone := &mobileRecord{mobileSession: mobileSession{mobileSummary: mobileSummary{
		ID: record.ID, Provider: record.Provider, Workspace: record.Workspace, Deleted: true, Purged: true,
		Revision: record.Revision, Generation: record.Generation,
	}, Turns: []mobileTurn{}}, CLIID: record.CLIID}
	for _, execution := range record.Executions {
		tombstone.Executions = append(tombstone.Executions, claudeExecution{CLIID: execution.CLIID})
	}
	tombstone.changed()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := writePrivate(mobileRecordPath(s.profile, id), tombstone); err != nil {
		return err
	}
	delete(m.active, id)
	if m.catalog != nil {
		m.catalog[id] = tombstone.summary()
	}
	return nil
}

func (s *Server) terminalSessionMetadata(w http.ResponseWriter, r *http.Request) {
	terminal := s.terminals.get(r.PathValue("id"))
	if terminal == nil {
		http.NotFound(w, r)
		return
	}
	id, _ := terminal.summary()["cli_session_id"].(string)
	if id == "" {
		http.Error(w, "此终端没有绑定会话", 409)
		return
	}
	var input sessionMetadataInput
	if !readJSON(w, r, &input) {
		return
	}
	view, err := s.updateSessionMetadata(id, input, nil)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, view)
}

func (s *Server) terminalSummaries() []map[string]any {
	list := s.terminals.snapshot()
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range list {
		id, _ := item["cli_session_id"].(string)
		if id == "" {
			continue
		}
		record, err := m.record(s.profile, id)
		if err != nil {
			continue
		}
		item["custom_title"], item["pinned"], item["metadata_set"] = record.CustomTitle, record.Pinned, record.MetadataSet
		item["title"] = record.snapshot().Title
		if s.macOSLegacyWorkspace(record) != "" {
			item["workspace_notice"] = macOSLegacyWorkspaceNotice
		}
	}
	return list
}
