package workbench

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	mobileTerminalChunk = 1 << 20
	mobileTerminalInput = 32 << 10
	mobileTerminalLimit = 12
)

// A phone read holds for less than the device link's response timeout.
var mobileTerminalHold = 25 * time.Second

var mobileTerminalID = regexp.MustCompile("^[0-9a-f]{16}$")

// Sign-in terminals carry account codes and stay on the computer.
func mobileTerminalKind(kind string) bool { return kind != "claude-login" && kind != "codex-login" }

func mobileTerminalSummary(t *terminalSession, item map[string]any) map[string]any {
	kind, _ := item["kind"].(string)
	if kind == "" {
		kind = "shell"
	}
	closed, _ := item["closed"].(bool)
	var exit any
	if code := t.exitStatus(); closed && code >= 0 {
		exit = code
	}
	// viewer: the phone may send a viewer identity with its input and resize.
	return map[string]any{"id": item["id"], "title": item["title"], "kind": kind, "closed": closed, "workspace": item["workspace"], "exit_code": exit, "viewer": terminalViewerProtocol}
}

// mobileTerminalKinds are the terminal kinds this guest can start.
func (s *Server) mobileTerminalKinds() []string {
	kinds := []string{"shell"}
	s.mu.Lock()
	command := claudeTerminalCommand(s.preferences)
	s.mu.Unlock()
	switch {
	case s.windowsGuest != nil && s.windowsGuest.Remote():
	case s.macosGuest != nil && !sharedCLILauncher(command):
	default:
		kinds = append(kinds, "claude")
	}
	if s.codexGuestSupported() {
		kinds = append(kinds, "codex")
	}
	return kinds
}

func (s *Server) mobileTerminals(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		sequence int
		view     map[string]any
	}
	var list []entry
	for _, item := range s.terminalSummaries() {
		id, _ := item["id"].(string)
		kind, _ := item["kind"].(string)
		session := s.terminals.get(id)
		if session == nil || !mobileTerminalKind(kind) {
			continue
		}
		list = append(list, entry{session.sequence, mobileTerminalSummary(session, item)})
	}
	slices.SortFunc(list, func(a, b entry) int { return a.sequence - b.sequence })
	terminals := []map[string]any{}
	for _, item := range list {
		terminals = append(terminals, item.view)
	}
	jsonReply(w, map[string]any{"terminals": terminals, "kinds": s.mobileTerminalKinds(), "limit": mobileTerminalLimit})
}

func (s *Server) mobileNewTerminal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Kind      string `json:"kind"`
		Workspace string `json:"workspace"`
	}
	if !readJSON(w, r, &input, 4<<10) {
		return
	}
	if !slices.Contains(s.mobileTerminalKinds(), input.Kind) {
		http.Error(w, "此环境不能打开这种终端", http.StatusBadRequest)
		return
	}
	if input.Workspace == "" || len(input.Workspace) > 4096 || !utf8.ValidString(input.Workspace) {
		http.Error(w, "请选择项目", http.StatusBadRequest)
		return
	}
	session, err := s.createTerminal(terminalRequest{Workspace: input.Workspace, Kind: input.Kind, requestContext: r.Context()})
	if err != nil {
		terminalFailure(w, err)
		return
	}
	jsonReply(w, mobileTerminalSummary(session, session.summary()))
}

// mobileTerminal finds a terminal the phone may use, or replies 404.
func (s *Server) mobileTerminal(w http.ResponseWriter, r *http.Request) *terminalSession {
	id := r.PathValue("id")
	if mobileTerminalID.MatchString(id) {
		if session := s.terminals.get(id); session != nil {
			session.mu.Lock()
			kind := session.kind
			session.mu.Unlock()
			if mobileTerminalKind(kind) {
				return session
			}
		}
	}
	http.Error(w, "终端已关闭或不存在", http.StatusNotFound)
	return nil
}

func (s *Server) mobileTerminalOutput(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	value := query.Get("after")
	after, err := strconv.ParseUint(value, 10, 64)
	if len(query) != 1 || len(query["after"]) != 1 || err != nil || strconv.FormatUint(after, 10) != value {
		http.Error(w, "读取位置无效", http.StatusBadRequest)
		return
	}
	session := s.mobileTerminal(w, r)
	if session == nil {
		return
	}
	// A phone reading the output shows the terminal: it stays a viewer for
	// the size owner rules.
	peer := session.phonePeer()
	hold := time.NewTimer(mobileTerminalHold)
	defer hold.Stop()
	for {
		out := session.outputAfter(after, mobileTerminalChunk)
		if len(out.Data) > 0 || out.Reset || out.Closed {
			session.showQueries(peer, out.Data)
			jsonReply(w, map[string]any{"data": out.Data, "next": out.Next, "reset": out.Reset, "closed": out.Closed, "exit_code": out.ExitCode})
			return
		}
		select {
		case <-out.changed:
		case <-hold.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) mobileTerminalInput(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Data   string `json:"data"`
		Viewer string `json:"viewer"`
	}
	if !readJSON(w, r, &input, 72<<10) || !mobileViewerValid(w, input.Viewer) {
		return
	}
	if input.Data == "" || len(input.Data) > mobileTerminalInput || !utf8.ValidString(input.Data) {
		http.Error(w, "输入内容无效或超过 32 KiB", http.StatusBadRequest)
		return
	}
	session := s.mobileTerminal(w, r)
	if session == nil {
		return
	}
	// The account decides whether the phone may type; reading the input and
	// queueing it happen after, so a program that stopped reading cannot
	// hold the account for everyone.
	peer := session.phonePeer(input.Viewer)
	s.account.mu.Lock()
	authorized := s.mobileAuthorized(r.Context())
	s.account.mu.Unlock()
	err := errMobileLocked
	if authorized {
		var data string
		if data, err = session.peerInput(peer, input.Data); err == nil {
			err = session.enqueueInput(r.Context(), data)
		}
	}
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errTerminalInputFull):
		http.Error(w, "终端暂未处理之前的输入，请稍后重试", http.StatusTooManyRequests)
	case errors.Is(err, os.ErrClosed):
		http.Error(w, "终端已结束", http.StatusConflict)
	default:
		http.Error(w, errMobileLocked.Error(), http.StatusLocked)
	}
}

func (s *Server) mobileTerminalResize(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Cols   uint16 `json:"cols"`
		Rows   uint16 `json:"rows"`
		Viewer string `json:"viewer"`
	}
	if !readJSON(w, r, &input, 1<<10) || !mobileViewerValid(w, input.Viewer) {
		return
	}
	if !validTerminalSize(input.Cols, input.Rows) {
		http.Error(w, "终端尺寸无效", http.StatusBadRequest)
		return
	}
	session := s.mobileTerminal(w, r)
	if session == nil {
		return
	}
	peer := session.phonePeer(input.Viewer)
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed || session.peerResize(peer, input.Cols, input.Rows) != nil {
		http.Error(w, "终端已结束", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mobileCloseTerminal(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}, 1<<10) {
		return
	}
	session := s.mobileTerminal(w, r)
	if session == nil {
		return
	}
	if !s.mobileAllowed(w, r) {
		return
	}
	session.mu.Lock()
	bound := session.kind == "codex" && session.sharedID != "" && !session.closed
	session.mu.Unlock()
	if bound {
		http.Error(w, errCodexDesktopTerminal.Error(), http.StatusConflict)
		return
	}
	if err := s.closeTerminalID(session.id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mobileViewerValid checks the optional viewer identity of a phone's input
// or resize. A server from before the viewer rules refuses the field, so the
// phone sends it only when the terminal summary has viewer.
func mobileViewerValid(w http.ResponseWriter, viewer string) bool {
	if viewer != "" && !terminalViewerPattern.MatchString(viewer) {
		http.Error(w, "终端查看者标识无效", http.StatusBadRequest)
		return false
	}
	return true
}
