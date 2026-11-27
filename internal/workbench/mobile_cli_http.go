package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func (s *Server) mobileCLIRoutes(mux routeRegistrar) {
	// Desktop history follows the selected local or SSH host. Mobile routes
	// remain pinned to the paired computer by the remote gateway.
	mux.HandleFunc("GET /api/cli/sessions", s.mobileSessions)
	mux.HandleFunc("GET /api/cli/sessions/{id}", s.mobileSessionView)
	mux.HandleFunc("POST /api/cli/sessions/{id}/open", s.mobileOpenTerminal)
	mux.HandleFunc("POST /api/cli/sessions/{id}/metadata", s.mobileSessionMetadata)
	mux.HandleFunc("POST /api/cli/sessions/{id}/account", s.mobileSessionAccount)
	mux.HandleFunc("POST /api/cli/sessions/{id}/archive", s.mobileArchiveSession)
	mux.HandleFunc("POST /api/cli/sessions/{id}/restore", s.mobileRestoreSession)
	mux.HandleFunc("POST /api/cli/sessions/{id}/purge", s.mobilePurgeSession)
	mux.HandleFunc("GET /api/cli/sessions/{id}/earlier", s.mobileEarlierParts)
	mux.HandleFunc("GET /api/cli/sessions/{id}/earlier/{cli}", s.mobileEarlierPart)
	mux.HandleFunc("POST /api/cli/sessions/{id}/earlier/{cli}/separate", s.mobileSeparateEarlier)
	// The desktop stops a Codex turn it can only watch (a phone turn) here.
	mux.HandleFunc("POST /api/cli/sessions/{id}/stop", s.mobileStop)
	mux.HandleFunc("GET /api/mobile/cli/projects", s.mobileProjects)
	mux.HandleFunc("GET /api/mobile/cli/sessions", s.mobileSessions)
	mux.HandleFunc("POST /api/mobile/cli/sessions", s.mobileNewSession)
	mux.HandleFunc("GET /api/mobile/cli/sessions/{id}", s.mobileSessionView)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/media", s.mobileSessionMedia)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/messages", s.mobileMessage)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/stop", s.mobileStop)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/approval", s.mobileApprovalResponse)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/open", s.mobileOpenTerminal)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/metadata", s.mobileSessionMetadata)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/account", s.mobileSessionAccount)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/delete", s.mobileArchiveSession)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/archive", s.mobileArchiveSession)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/restore", s.mobileRestoreSession)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/purge", s.mobilePurgeSession)
	mux.HandleFunc("GET /api/mobile/cli/sessions/{id}/earlier", s.mobileEarlierParts)
	mux.HandleFunc("GET /api/mobile/cli/sessions/{id}/earlier/{cli}", s.mobileEarlierPart)
	mux.HandleFunc("POST /api/mobile/cli/sessions/{id}/earlier/{cli}/separate", s.mobileSeparateEarlier)
}

func (s *Server) mobileOpenTerminal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Automatic       bool   `json:"automatic"`
		RecoverTerminal string `json:"recover_terminal_id"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.RecoverTerminal != "" && (input.Automatic || !mobileTerminalID.MatchString(input.RecoverTerminal)) {
		http.Error(w, "终端恢复请求无效", http.StatusBadRequest)
		return
	}
	s.mobileConnectMu.Lock()
	defer s.mobileConnectMu.Unlock()
	if view, terminal, ok := s.authorizedMobileSessionRecord(w, r); !ok {
		return
	} else if input.RecoverTerminal != "" && (!terminal || view.DesktopTerminalID != input.RecoverTerminal || view.Archived) {
		http.Error(w, errTerminalRecoveryUnavailable.Error(), http.StatusConflict)
		return
	} else if view.Provider == codexProvider {
		s.openCodexSession(w, r, view, input.RecoverTerminal)
		return
	}
	view, ok := s.authorizedMobileSession(w, r)
	if !ok {
		return
	}
	if !s.isTerminalConversation(view.ID) {
		if err := s.promoteMobileConversation(r.Context(), view.ID); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, view.ID)
	s.mobileCLI.mu.Unlock()
	if err != nil {
		http.Error(w, "无法读取会话", 500)
		return
	}
	existing := s.boundTerminal(record)
	needsTerminal := existing == nil || !existing.ownsConversation()
	if input.RecoverTerminal != "" {
		if !record.canRecoverTerminal(input.RecoverTerminal) || needsTerminal && view.nativeResumeID != record.CLIID {
			http.Error(w, errTerminalRecoveryUnavailable.Error(), http.StatusConflict)
			return
		}
		if !needsTerminal {
			view.DesktopTerminalID = existing.id
			jsonReply(w, view)
			return
		}
	}
	if needsTerminal {
		s.mu.Lock()
		launcher := claudeTerminalCommand(s.preferences)
		s.mu.Unlock()
		if s.windowsGuest == nil && record.Launcher != "" {
			launcher = record.Launcher
		}
		if !sharedCLILauncher(launcher) || record.Launcher != "" && launcher != record.Launcher {
			http.Error(w, "主机启动命令与原会话不一致，请恢复原配置后重试", 422)
			return
		}
		if problem := s.terminalAccountProblem(record); problem != "" {
			// The account handoff (claude_switch.go) can offer another account here.
			w.Header().Set("X-Cage-Recovery", "claude-account")
			http.Error(w, problem, 409)
			return
		}
		if view.missingHistory != "" && s.windowsGuest == nil && s.macosGuest == nil {
			// The CLI would say "No conversation found" and leave a shell.
			w.Header().Set("X-Cage-Recovery", "conversation-missing")
			http.Error(w, "电脑上找不到这段会话的 Claude Code 记录（"+view.missingHistory+" 中没有它），无法接续。Claude Code 会按保留期自动删除较早的会话记录，记录也可能已被删除。这个条目仍然保留，可以归档它，或新建会话继续工作。", 409)
			return
		}
	}
	if (record.Resume || record.Connection != "") && needsTerminal {
		var state struct {
			Running      bool   `json:"running"`
			ChannelReady bool   `json:"channel_ready"`
			Error        string `json:"error"`
		}
		mode := "active"
		if input.Automatic {
			if err := s.terminals.canStart(); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			mode = "handoff"
		}
		query := map[string]any{"cli_id": record.CLIID, "config": record.GuestConfig, "workspace": record.GuestWorkspace, "account": record.Account.GuestID}
		if err := s.windowsConversationInput(record, query); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err := s.runCLIChannel(r.Context(), mode, query, &state); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		if state.Error != "" {
			http.Error(w, state.Error, 409)
			return
		}
		if state.Running {
			if input.Automatic {
				if state.ChannelReady {
					var result struct {
						OK bool `json:"ok"`
					}
					resume := map[string]any{"cli_id": record.CLIID, "connection": record.Connection, "action": "resume"}
					if err := s.windowsConversationInput(record, resume); err != nil {
						http.Error(w, err.Error(), http.StatusConflict)
						return
					}
					if err := s.runCLIChannel(r.Context(), "rpc", resume, &result); err != nil || !result.OK {
						http.Error(w, "会话恢复暂未完成，请重试", 503)
						return
					}
				}
				view.ConnectionWaiting = true
				jsonReply(w, view)
				return
			}
			http.Error(w, "原会话仍在电脑终端运行，请先退出其中的 Claude Code，再接续同一段历史", 409)
			return
		}
	}
	terminal, err := s.createTerminal(terminalRequest{Workspace: view.Workspace, Kind: "claude", sharedID: view.ID, requestContext: r.Context(),
		recoverTerminal: input.RecoverTerminal, recoverCLIID: view.nativeResumeID})
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	view.DesktopTerminalID = terminal.id
	if input.Automatic {
		view, err = s.terminalConversationView(r.Context(), view.ID)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
	}
	jsonReply(w, view)
}

// openCodexSession opens a Codex session in its desktop terminal. Shipped
// phones call /open on their own for Claude sessions only, and older desktop
// UIs never list Codex rows; the phone route keeps refusing.
func (s *Server) openCodexSession(w http.ResponseWriter, r *http.Request, view mobileSession, recoverTerminal string) {
	if view.Archived {
		http.Error(w, "此会话已归档，请先在设置中的已归档会话撤销归档", http.StatusGone)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/mobile/") {
		http.Error(w, "Codex 会话请在手机上继续，暂不支持从手机打开电脑终端", 409)
		return
	}
	probed := s.warmCodexProbe(r.Context())
	terminal, err := s.createTerminal(terminalRequest{Workspace: view.Workspace, Kind: "codex", sharedID: view.ID, codexProbed: probed,
		requestContext: r.Context(), recoverTerminal: recoverTerminal})
	if err != nil {
		switch {
		case errors.Is(err, errMobileLocked):
			http.Error(w, err.Error(), http.StatusLocked)
		case errors.Is(err, errTerminalsClosed):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		case errors.Is(err, errTerminalStart):
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			http.Error(w, err.Error(), http.StatusConflict)
		}
		return
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, view.ID)
	if err == nil {
		view = record.snapshot()
	}
	s.mobileCLI.mu.Unlock()
	view.DesktopTerminalID, view.Owner, view.Running = terminal.id, "desktop", true
	jsonReply(w, view)
}

func (s *Server) mobileApprovalResponse(w http.ResponseWriter, r *http.Request) {
	view, ok := s.authorizedMobileSession(w, r)
	if !ok {
		return
	}
	var input struct {
		TurnID     string `json:"request_id"`
		ApprovalID string `json:"approval_id"`
		Allow      *bool  `json:"allow"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !mobileID(input.TurnID) || !mobileID(input.ApprovalID) || input.Allow == nil {
		http.Error(w, "需要本次操作的完整确认", 400)
		return
	}
	if s.isTerminalConversation(view.ID) {
		if len(view.Turns) == 0 || view.Turns[len(view.Turns)-1].ID != input.TurnID {
			http.Error(w, "审批轮次已变化，请重新读取会话", 409)
			return
		}
		result, err := s.terminalConversationAction(r.Context(), view.ID, map[string]any{"action": "approve", "approval_id": input.ApprovalID, "allow": *input.Allow})
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		jsonReply(w, result)
		return
	}
	// Serialize the actual decision with explicit account locking. Do not
	// retain either mutex while writing the HTTP response.
	s.account.mu.Lock()
	if !s.mobileAuthorized(r.Context()) {
		s.account.mu.Unlock()
		http.Error(w, "请先解锁 Super Cage", 423)
		return
	}
	m := &s.mobileCLI
	m.mu.Lock()
	run := m.active[view.ID]
	if run != nil && run.pending != nil && run.pending.ctx.Err() == nil {
		turn := &run.record.Turns[len(run.record.Turns)-1]
		if turn.ID == input.TurnID && turn.Status == "running" && turn.Approval != nil && turn.Approval.ID == input.ApprovalID {
			run.pending.reply <- *input.Allow
			run.pending = nil
			turn.Approval = nil
			run.record.changed()
			view = run.record.snapshot()
			m.mu.Unlock()
			s.account.mu.Unlock()
			jsonReply(w, view)
			return
		}
	}
	m.mu.Unlock()
	s.account.mu.Unlock()
	http.Error(w, "这项请求已结束或由其他设备处理，请重新读取会话", 409)
}
func (s *Server) mobileProjects(w http.ResponseWriter, r *http.Request) {
	if s.windowsGuest != nil {
		s.refreshWindowsComponents()
	}
	groups, err := s.projectGroups()
	if err != nil {
		http.Error(w, "无法读取已授权的项目", 500)
		return
	}
	projects := []map[string]any{}
	for _, group := range groups {
		projects = append(projects, map[string]any{"path": group.Workspace, "name": group.Name, "folders": group.Folders})
	}
	s.mu.Lock()
	ready := s.ready && !s.closing
	shared := sharedCLILauncher(claudeTerminalCommand(s.preferences))
	if s.windowsGuest != nil {
		available := s.windowsCapabilitiesLocked()["available"].(map[string]bool)
		shared = shared && available["cli"] && available["terminal"] && available["projects"]
	}
	codex := s.codexOfferedLocked()
	s.mu.Unlock()
	name, _ := os.Hostname()
	jsonReply(w, map[string]any{"projects": projects, "ready": ready, "host": name, "shared_sessions": shared, "codex": codex})
}

// Session list protocol (GET /api/cli/sessions, GET /api/mobile/cli/sessions):
//
//   - Every answer carries "revision", an opaque string that changes whenever
//     anything else in the answer changes ("sessions" or "notice"). It is a
//     content hash; after a host restart every session's "generation"
//     changes, so the first answer is always a full list.
//   - ?after=<revision> answers 204 No Content while the list still equals
//     that revision, and 200 with the full list otherwise.
//   - ?after=<revision>&wait=<1..25> holds the request for up to that many
//     seconds while the list is unchanged and answers as soon as it changes
//     (200) or the time is up (204). Larger values are treated as 25. The
//     server may answer earlier. A client issues its next request at once
//     after an answer, but never sooner than 1 s after the previous request
//     started. It keeps its usual 5 s polling when an answer has no
//     "revision" (an older server), when a request with these parameters
//     fails with 400 or 404 (an older server or an older phone link filter),
//     and treats a 204 that arrives more than 1 s before its wait elapsed as
//     "not held" (wait its usual interval before the next request).
//   - A client that sends neither parameter gets today's behaviour: a fresh
//     guest observation and the full list on every request.
//
// "archived=true" combines with both parameters.
//
// Freshness: host changes (create, archive, restore, purge, rename, pin) are
// in every answer at once and reach a held request within 250 ms. Every new
// request starts from a guest observation made after it arrived, so a held
// answer is never older than its wait (at most 25 s); a guest too slow to
// answer within the request's time leaves it the newest finished observation
// (session_hold.go). On Linux guests the
// resident helper wakes it about 1 s after a guest change. The running flags
// may reuse the guest's claude agents --json listing for up to 30 s when the
// CLI's session registry shows no change. "updated_at" is part of the list,
// so during a running turn a held list answers about once a second.
func (s *Server) mobileSessions(w http.ResponseWriter, r *http.Request) {
	paths, err := s.projectPaths()
	if err != nil {
		http.Error(w, "无法读取已授权的项目", 500)
		return
	}
	granted := map[string]bool{}
	for _, path := range paths {
		granted[path] = true
	}
	query := r.URL.Query()
	archived := query.Get("archived") == "true"
	after, wait, ok := sessionListConditions(query.Get("after"), query.Get("wait"))
	if !ok {
		http.Error(w, "会话列表版本无效", 400)
		return
	}
	// The desktop lists Codex sessions only when it asks with
	// ?providers=claude,codex: an older desktop UI would open them as Claude
	// Code terminals. The phone lists both, as it always has.
	list, ok := s.sessionListKinds(strings.HasPrefix(r.URL.Path, "/api/mobile/"), query["providers"])
	if !ok {
		http.Error(w, "会话类型无效", 400)
		return
	}
	// The catalog must be loaded before discovery matches guest sessions to it.
	if _, err := s.mobileCLI.summaries(s.profile, granted, archived); err != nil {
		http.Error(w, "无法读取主机上的 CLI 会话记录", 500)
		return
	}
	arrival := time.Now()
	// The guest is looked at for no longer than the request may take; see
	// session_hold.go.
	limit := arrival.Add(sessionLookLimit)
	if wait > 0 {
		limit = arrival.Add(wait)
	}
	look, cancel := holdContext(r.Context(), limit)
	defer cancel()
	claudeErr, codexErr := s.observeSessions(look, paths, list, arrival)
	if r.Context().Err() != nil {
		return
	}
	answer, err := s.sessionListAnswer(granted, archived, list, claudeErr, codexErr)
	if err != nil {
		http.Error(w, "无法读取主机上的 CLI 会话记录", 500)
		return
	}
	if wait > 0 && answer.Revision == after {
		answer, err = s.awaitSessionList(r.Context(), arrival, arrival.Add(wait), after, paths, archived, list, claudeErr, codexErr)
		if r.Context().Err() != nil {
			return
		}
		if err != nil {
			http.Error(w, "无法读取主机上的 CLI 会话记录", 500)
			return
		}
	}
	if after != "" && answer.Revision == after {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	jsonReply(w, answer)
}

// awaitSessionList holds a list request while its answer equals after. The
// host catalog is checked every 250 ms; the guest is watched by the resident
// helper, or looked at every 5 s (the clients' own polling pace) when it
// cannot be; Codex sessions, when listed, are looked at every 5 s. observed is
// when the request's first look began.
func (s *Server) awaitSessionList(ctx context.Context, observed, deadline time.Time, after string, paths []string, archived bool, list sessionListKinds, claudeErr, codexErr error) (sessionListAnswer, error) {
	// Each look ends at the deadline; share then gives the newest finished
	// observation, and the answer follows at once.
	look, cancel := holdContext(ctx, deadline)
	defer cancel()
	changes, stop := s.subscribeGuestChanges()
	defer stop()
	host := time.NewTicker(250 * time.Millisecond)
	defer host.Stop()
	slow := time.NewTicker(5 * time.Second)
	defer slow.Stop()
	end := time.NewTimer(time.Until(deadline))
	defer end.Stop()
	looks := 0
	key := strings.Join(slices.Sorted(slices.Values(paths)), "\x00")
	run := func() (context.Context, context.CancelFunc) { return s.discoveryContext(look) }
	granted := func() map[string]bool {
		set := make(map[string]bool, len(paths))
		for _, path := range paths {
			set[path] = true
		}
		return set
	}
	// The watch replaces the channel it closes, so a change it saw while this
	// request was inside a discovery shows only in its time.
	seen, now := observed, make(chan struct{})
	close(now)
	for {
		changed, changedAt, watching := changes()
		claude, codex := false, false
		arrival := time.Now()
		if changedAt.After(seen) {
			changed = now
		}
		select {
		case <-ctx.Done():
			return sessionListAnswer{}, ctx.Err()
		case <-end.C:
			return s.sessionListAnswer(granted(), archived, list, claudeErr, codexErr)
		case <-changed:
			_, changedAt, _ = changes() // the change that closed it
			claude, arrival, seen = list.claude, changedAt, changedAt
		case <-slow.C:
			claude, codex = list.claude && !watching, list.codex
		case <-host.C:
			// A project change needs a new observation. Locking the account
			// or closing the server ends the request context instead.
			if looks++; looks%4 == 0 {
				if current, err := s.projectPaths(); err == nil && strings.Join(slices.Sorted(slices.Values(current)), "\x00") != key {
					paths, key = current, strings.Join(slices.Sorted(slices.Values(current)), "\x00")
					claude, codex = list.claude, list.codex
				}
			}
		}
		if claude {
			claudeErr = s.cliDiscovery.share(look, "claude\x00"+key, arrival, run, func(ctx context.Context) error { return s.discoverTerminalConversations(ctx, paths) })
		}
		if codex {
			codexErr = s.cliDiscovery.share(look, "codex\x00"+key, arrival, run, func(ctx context.Context) error { return s.discoverCodexConversations(ctx, paths) })
		}
		if ctx.Err() != nil {
			return sessionListAnswer{}, ctx.Err()
		}
		answer, err := s.sessionListAnswer(granted(), archived, list, claudeErr, codexErr)
		if err != nil || answer.Revision != after {
			return answer, err
		}
	}
}

type sessionListAnswer struct {
	Sessions []mobileSummary `json:"sessions"`
	Notice   string          `json:"notice"`
	// Capabilities is sent only to a desktop that lists Codex sessions.
	Capabilities *sessionCapabilities `json:"capabilities,omitempty"`
	Revision     string               `json:"revision"`
}

type sessionCapabilities struct {
	// CodexSessions: the menu's Codex entry creates a session, and Codex rows
	// open as bound terminals (a Linux guest with the plain "codex" command).
	CodexSessions bool `json:"codex_sessions"`
	// CodexCLI is the guest CLI's last reported support, when known.
	CodexCLI *codexCLICaps `json:"codex_cli,omitempty"`
}

// sessionListKinds is what a list request asks for.
type sessionListKinds struct{ phone, claude, codex bool }

// sessionListKinds is sessionListFilter with the phone's Codex rows following
// presence, as its Codex kind does: unknown and present list both, absent
// lists Claude only. The desktop names its providers itself.
func (s *Server) sessionListKinds(phone bool, values []string) (sessionListKinds, bool) {
	kinds, ok := sessionListFilter(phone, values)
	if ok && phone {
		s.mu.Lock()
		kinds.codex = s.codexOfferedLocked()
		s.mu.Unlock()
	}
	return kinds, ok
}

func sessionListFilter(phone bool, values []string) (sessionListKinds, bool) {
	if phone || values == nil {
		return sessionListKinds{phone: phone, claude: true, codex: phone}, true
	}
	var kinds sessionListKinds
	if len(values) != 1 {
		return kinds, false
	}
	for _, name := range strings.Split(values[0], ",") {
		switch name {
		case "claude":
			kinds.claude = true
		case codexProvider:
			kinds.codex = true
		default:
			return kinds, false
		}
	}
	return kinds, true
}

const sessionListWaitLimit = 25 * time.Second

func sessionListConditions(after, wait string) (string, time.Duration, bool) {
	if after != "" && (len(after) != 32 || strings.Trim(after, "0123456789abcdef") != "") {
		return "", 0, false
	}
	held, ok := sessionWait(wait)
	if !ok || held > 0 && after == "" {
		return "", 0, false
	}
	return after, held, true
}

// sessionWait reads a wait parameter in whole seconds; at most 25 are used.
func sessionWait(value string) (time.Duration, bool) {
	if value == "" {
		return 0, true
	}
	seconds, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, false
	}
	return min(time.Duration(seconds)*time.Second, sessionListWaitLimit), true
}

// observeSessions refreshes the guest's view of the sessions a list shows,
// sharing runs with concurrent list requests; the desktop and the phone share
// one Codex discovery.
func (s *Server) observeSessions(ctx context.Context, paths []string, list sessionListKinds, arrival time.Time) (claudeErr, codexErr error) {
	key := strings.Join(slices.Sorted(slices.Values(paths)), "\x00")
	run := func() (context.Context, context.CancelFunc) { return s.discoveryContext(ctx) }
	var wg sync.WaitGroup
	if list.codex {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codexErr = s.cliDiscovery.share(ctx, "codex\x00"+key, arrival, run, func(ctx context.Context) error { return s.discoverCodexConversations(ctx, paths) })
		}()
	}
	if list.claude {
		claudeErr = s.cliDiscovery.share(ctx, "claude\x00"+key, arrival, run, func(ctx context.Context) error { return s.discoverTerminalConversations(ctx, paths) })
	}
	wg.Wait()
	return claudeErr, codexErr
}

func (s *Server) sessionListAnswer(granted map[string]bool, archived bool, list sessionListKinds, claudeErr, codexErr error) (sessionListAnswer, error) {
	sessions, err := s.mobileCLI.summaries(s.profile, granted, archived)
	if err != nil {
		return sessionListAnswer{}, err
	}
	var notices []string
	for _, err := range []error{claudeErr, codexErr} {
		if err != nil && !slices.Contains(notices, err.Error()) {
			notices = append(notices, err.Error())
		}
	}
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	for i := range sessions {
		failed := claudeErr
		if sessions[i].Provider == codexProvider {
			failed = codexErr
		}
		if !ready || failed != nil {
			sessions[i].Active = false
		}
	}
	s.codexRows(sessions)
	sessions = slices.DeleteFunc(sessions, func(item mobileSummary) bool {
		if item.Provider != codexProvider {
			return !list.claude
		}
		// A session whose Codex closed before it made a thread has nothing to
		// resume: the desktop hides it, and the phone keeps only its own.
		return !list.codex || item.placeholder && item.Owner == "" && (!list.phone || item.menuPlaceholder)
	})
	// Equal pins and times keep a fixed order, so equal lists hash equally.
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID < b.ID
	})
	answer := sessionListAnswer{Sessions: sessions, Notice: strings.Join(notices, "\n")}
	if list.codex && !list.phone {
		answer.Capabilities = &sessionCapabilities{CodexSessions: s.codexSessionsPossible(), CodexCLI: s.mobileCLI.codexCLICaps()}
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return sessionListAnswer{}, err
	}
	sum := sha256.Sum256(data)
	answer.Revision = hex.EncodeToString(sum[:16])
	return answer, nil
}
func (s *Server) mobileNewSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID        string `json:"id"`
		Workspace string `json:"workspace"`
		Shared    bool   `json:"shared"` // Accepted for old clients; the host always creates a common session.
		Provider  string `json:"provider"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !mobileID(input.ID) || input.Workspace == "" {
		http.Error(w, "需要项目和有效的创建编号", 400)
		return
	}
	if input.Provider != "" && input.Provider != "claude" && input.Provider != codexProvider {
		http.Error(w, "不支持的会话类型", 400)
		return
	}
	workspace, err := s.projectWorkspace(input.Workspace)
	if err != nil {
		http.Error(w, "项目未授权或已不可用", 403)
		return
	}
	if input.Provider == codexProvider {
		view, err := s.createCodexConversation(r.Context(), input.ID, workspace)
		if errors.Is(err, errMobileLocked) {
			http.Error(w, err.Error(), http.StatusLocked)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		jsonReply(w, view)
		return
	}
	s.mu.Lock()
	shared := sharedCLILauncher(claudeTerminalCommand(s.preferences))
	s.mu.Unlock()
	if !shared {
		http.Error(w, "当前启动命令无法连接手机，请在主机使用 claude 或 ccs 配置名", 409)
		return
	}
	s.account.mu.Lock()
	if !s.mobileAuthorized(r.Context()) {
		s.account.mu.Unlock()
		http.Error(w, errMobileLocked.Error(), 423)
		return
	}
	view, err := s.mobileCLI.create(s.profile, workspace, input.ID)
	s.account.mu.Unlock()
	if err != nil {
		http.Error(w, "无法创建会话，请检查编号、磁盘和目录权限", 409)
		return
	}
	// Older phones do not send shared. The host chooses the common session
	// implementation, so updating only the computer still fixes new sessions.
	if len(view.Turns) == 0 {
		if _, err := s.createTerminal(terminalRequest{Workspace: workspace, Kind: "claude", sharedID: view.ID, requestContext: r.Context()}); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		s.mobileCLI.mu.Lock()
		record, err := s.mobileCLI.record(s.profile, view.ID)
		if err == nil {
			view = record.snapshot()
		}
		s.mobileCLI.mu.Unlock()
	}
	jsonReply(w, view)
}
func (s *Server) authorizedMobileSession(w http.ResponseWriter, r *http.Request) (mobileSession, bool) {
	view, terminal, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return view, false
	}
	if view.Archived {
		http.Error(w, "此会话已归档，请先在设置中的已归档会话撤销归档", http.StatusGone)
		return view, false
	}
	if terminal {
		var err error
		view, err = s.terminalConversationView(r.Context(), view.ID)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return view, false
		}
	}
	return view, true
}

// List operations need only the host record; an ended guest terminal need not
// be reachable to rename, pin, or remove it.
func (s *Server) authorizedMobileSessionRecord(w http.ResponseWriter, r *http.Request) (mobileSession, bool, bool) {
	id := r.PathValue("id")
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, id)
	var view mobileSession
	if err == nil {
		if record.Deleted {
			err = os.ErrNotExist
		}
		view = record.snapshot()
	}
	m.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return view, false, false
	}
	if err != nil {
		http.Error(w, "无法读取主机上的 CLI 会话记录", 500)
		return view, false, false
	}
	if _, err := s.projectWorkspace(view.Workspace); err != nil {
		http.Error(w, "项目未授权或已不可用", 403)
		return view, false, false
	}
	return view, record.Terminal, true
}

// Session read protocol (GET /api/[mobile/]cli/sessions/{id}):
//
//   - ?after=<revision>[&epoch=<generation>] answers 204 while the session's
//     revision and generation are unchanged (as before).
//   - Adding &wait=<1..25> holds the request while it is unchanged and
//     answers as soon as it changes (200) or the time is up (204). An older
//     server ignores wait and answers 204 at once, so a client sends wait
//     only to a server whose session list answers carry "revision", and the
//     session list's pacing and fallback rules apply: at most one request
//     per second, and on 400, 404 or an early 204 it keeps its 1 s / 4 s
//     polling. The 1 s floor stays because each read re-parses the whole
//     transcript and each answer carries the whole session (up to 448 KiB
//     of turns); while a reply streams, its state changes with every hook,
//     so reads still arrive about once a second. What waiting adds is the
//     first change after a quiet period within about 250 ms, and no polling
//     while nothing happens.
func (s *Server) mobileSessionView(w http.ResponseWriter, r *http.Request) {
	arrival := time.Now()
	// A held read answers by its time whatever the guest does; its first look
	// gets at least heldReadFirstLook (session_hold.go).
	var view mobileSession
	ok, reached := false, true
	if held, valid := sessionWait(r.URL.Query().Get("wait")); valid && held > 0 && r.URL.Query().Get("after") != "" {
		view, ok, reached = s.readSessionBy(w, r, arrival.Add(max(held, heldReadFirstLook)))
	} else {
		view, ok = s.authorizedMobileSession(w, r)
	}
	if r.Context().Err() != nil || !ok && reached {
		return
	}
	if value := r.URL.Query().Get("after"); value != "" {
		revision, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			http.Error(w, "会话版本无效", 400)
			return
		}
		epoch := r.URL.Query().Get("epoch")
		if epoch != "" && !mobileID(epoch) {
			http.Error(w, "服务标识无效", 400)
			return
		}
		wait, ok := sessionWait(r.URL.Query().Get("wait"))
		if !ok {
			http.Error(w, "等待时间无效", 400)
			return
		}
		if !reached {
			// The first look did not finish in time: nothing is known to
			// have changed.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		unchanged := func() bool { return revision == view.Revision && (epoch == "" || epoch == view.Generation) }
		// Hold the request while the session is unchanged; no lock is held
		// while waiting, and every look is a full authorized read that ends
		// at the deadline. The request context ends with the server, the
		// account lock and the phone link, which ends a held read too.
		for deadline := arrival.Add(wait); unchanged() && s.awaitSessionView(r.Context(), view, deadline); {
			next, ok, reached := s.readSessionBy(w, r, deadline)
			if !reached {
				break
			}
			if !ok {
				return
			}
			view = next
		}
		if r.Context().Err() != nil {
			return
		}
		if unchanged() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	jsonReply(w, view)
}
func (s *Server) mobileMessage(w http.ResponseWriter, r *http.Request) {
	view, ok := s.authorizedMobileSession(w, r)
	if !ok {
		return
	}
	var input struct {
		ID   string `json:"request_id"`
		Text string `json:"text"`
	}
	if !readJSON(w, r, &input, 72<<10) {
		return
	}
	if !mobileID(input.ID) || strings.TrimSpace(input.Text) == "" || len(input.Text) > mobilePromptLimit || !utf8.ValidString(input.Text) {
		http.Error(w, "需要有效的发送编号和 32 KiB 以内的消息", 400)
		return
	}
	if s.isTerminalConversation(view.ID) {
		result, err := s.terminalConversationAction(r.Context(), view.ID, map[string]any{"action": "send", "request_id": input.ID, "text": input.Text})
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		jsonReply(w, result)
		return
	}
	result, err := s.submitMobileTurn(r.Context(), view.ID, input.ID, input.Text)
	if err != nil {
		if errors.Is(err, errTerminalBusy) {
			http.Error(w, "主机正在处理其他请求，请保留发送编号重试", http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, errMobileLocked) {
			http.Error(w, err.Error(), http.StatusLocked)
			return
		}
		http.Error(w, err.Error(), 409)
		return
	}
	jsonReply(w, result)
}
func (s *Server) mobileStop(w http.ResponseWriter, r *http.Request) {
	view, ok := s.authorizedMobileSession(w, r)
	if !ok {
		return
	}
	var input struct {
		ID string `json:"request_id"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !mobileID(input.ID) {
		http.Error(w, "执行编号无效", 400)
		return
	}
	if s.isTerminalConversation(view.ID) {
		result, err := s.stopTerminalConversation(r.Context(), view, input.ID)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		jsonReply(w, result)
		return
	}
	s.account.mu.Lock()
	if !s.mobileAuthorized(r.Context()) {
		s.account.mu.Unlock()
		http.Error(w, errMobileLocked.Error(), http.StatusLocked)
		return
	}
	result, err := s.stopMobileTurn(view.ID, input.ID)
	s.account.mu.Unlock()
	if err != nil {
		http.Error(w, "未找到这次执行", 404)
		return
	}
	jsonReply(w, result)
}
