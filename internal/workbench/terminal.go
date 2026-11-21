package workbench

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// A terminal stream crosses proxies, relays and tunnels. The server speaks at
// least every heartbeat interval so no hop sees an idle connection, and a
// viewer that stops answering is released after the read timeout. Browsers
// answer WebSocket pings themselves, including in hidden pages.
var (
	terminalHeartbeatInterval = 20 * time.Second
	terminalWriteTimeout      = 30 * time.Second
	terminalReadTimeout       = 90 * time.Second
)

// terminalFrameLimit bounds one binary frame, so a slow path still delivers
// a frame well within the write timeout and the viewer sees steady progress.
const terminalFrameLimit = 64 << 10

// terminalInputMessageLimit bounds one message from a viewer. A paste is one
// input message, JSON-escaped, so the limit must be well above the largest
// paste a person makes: a message over it closes the stream and the paste is
// lost. The session's input queue bounds what stays held, see
// terminalInputBudget.
const terminalInputMessageLimit = 16 << 20

var terminalOffsetPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)

// terminalResumeOffset reads the optional after=<byte offset> of a viewer that
// tracks offsets. A malformed or repeated value is refused.
func terminalResumeOffset(r *http.Request) (*uint64, bool) {
	values, present := r.URL.Query()["after"]
	if !present {
		return nil, true
	}
	if len(values) != 1 || !terminalOffsetPattern.MatchString(values[0]) {
		return nil, false
	}
	offset, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil {
		return nil, false
	}
	return &offset, true
}

func (s *Server) listTerminals(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, s.terminalSummaries())
}

// terminalsRevision changes whenever the list from listTerminals would, so a
// client polling the status reads the list only after it changed.
func (s *Server) terminalsRevision() string {
	list := s.terminalSummaries()
	slices.SortFunc(list, func(a, b map[string]any) int {
		left, _ := a["id"].(string)
		right, _ := b["id"].(string)
		return cmp.Compare(left, right)
	})
	data, _ := json.Marshal(list)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (s *Server) newTerminal(w http.ResponseWriter, r *http.Request) {
	var input terminalRequest
	if !readJSON(w, r, &input) {
		return
	}
	// A client that lists Codex sessions (?providers=claude,codex) gets a
	// session from the menu's Codex entry; older clients keep plain terminals.
	if values := r.URL.Query()["providers"]; values != nil {
		kinds, ok := sessionListFilter(false, values)
		if !ok {
			http.Error(w, "会话类型无效", http.StatusBadRequest)
			return
		}
		input.codexSession = kinds.codex
	}
	session, err := s.createTerminal(input)
	if err != nil {
		terminalFailure(w, err)
		return
	}
	jsonReply(w, session.summary())
}

// terminalFailure maps a terminal admission error for the desktop and phone.
func terminalFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTerminalsClosed):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errTerminalBusy), errors.Is(err, errTerminalNotReady), errors.Is(err, errTerminalLimit):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errTerminalStart):
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case errors.Is(err, errMobileLocked):
		http.Error(w, err.Error(), http.StatusLocked)
	default:
		requestError(w, err)
	}
}

func (s *Server) closeTerminal(w http.ResponseWriter, r *http.Request) {
	if err := s.closeTerminalID(r.PathValue("id")); err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

// A terminal sharing a Claude Code session is archived with its session;
// any other terminal ends and is removed.
func (s *Server) closeTerminalID(id string) error {
	if terminal := s.terminals.get(id); terminal != nil {
		summary := terminal.summary()
		if shared, _ := summary["cli_session_id"].(string); shared != "" {
			// A bound Codex terminal ends with its Codex; closing what is left
			// of it only removes the tab. Closing a live one archives.
			if summary["kind"] == "codex" && summary["closed"] == true {
				s.terminals.remove(id)
				return nil
			}
			return s.setSessionArchived(shared, true, nil)
		}
	}
	s.terminals.remove(id)
	s.entryUnkept()
	return nil
}

// terminalStream attaches a viewer. Without after it keeps the original
// framing: an optional history marker and one binary frame, then live output.
// With after, every history marker carries the offset of the bytes that
// follow, their size and whether the viewer must clear its screen first, so
// the viewer can resume after a reconnect from the last byte it received.
func (s *Server) terminalStream(w http.ResponseWriter, r *http.Request) {
	session := s.terminals.get(r.PathValue("id"))
	if session == nil {
		http.NotFound(w, r)
		return
	}
	after, valid := terminalResumeOffset(r)
	if !valid {
		http.Error(w, "终端输出位置无效", http.StatusBadRequest)
		return
	}
	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(terminalInputMessageLimit)
	_ = ws.SetReadDeadline(time.Now().Add(terminalReadTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(terminalReadTimeout)) })
	subscription := session.subscribeFrom(after)
	pongs, stop, done := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer ws.Close()
		writeTerminalStream(ws, subscription, after != nil, pongs, stop)
	}()
	defer func() {
		close(stop)
		subscription.Close()
		_ = ws.Close()
		<-done
	}()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(terminalReadTimeout))
		var input struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if json.Unmarshal(data, &input) != nil {
			continue
		}
		switch input.Type {
		case "ping":
			select {
			case pongs <- struct{}{}:
			default:
			}
		case "input":
			// A full queue holds this viewer here, unread, until the guest
			// takes input again; the sender is held back instead of the
			// input being refused.
			if err := session.enqueueInputWait(r.Context(), input.Data, done); err != nil {
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(terminalReadTimeout))
		case "resize":
			if validTerminalSize(input.Cols, input.Rows) {
				if err := session.file.Resize(input.Cols, input.Rows); err != nil {
					return
				}
			}
		}
	}
}

// writeTerminalStream is the only writer of a terminal stream. Live output is
// coalesced into frames of at most terminalFrameLimit bytes; the PTY never
// waits for this viewer, which falls back to the kept history if its queue
// overflows.
func writeTerminalStream(ws *websocket.Conn, subscription *terminalSubscription, resume bool, pongs, stop <-chan struct{}) {
	writeJSON := func(value any) bool {
		_ = ws.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
		return ws.WriteJSON(value) == nil
	}
	writeBinary := func(data []byte) bool {
		for len(data) > 0 {
			n := min(len(data), terminalFrameLimit)
			_ = ws.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
			if ws.WriteMessage(websocket.BinaryMessage, data[:n]) != nil {
				return false
			}
			data = data[n:]
		}
		return true
	}
	// Replayed output must render without repeating clipboard side effects.
	replay := func(data []byte, offset uint64, reset bool) bool {
		if !resume {
			if len(data) == 0 {
				return true
			}
			if !writeJSON(map[string]string{"type": "history"}) {
				return false
			}
			_ = ws.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
			return ws.WriteMessage(websocket.BinaryMessage, data) == nil
		}
		return writeJSON(map[string]any{"type": "history", "offset": offset, "reset": reset, "size": len(data)}) && writeBinary(data)
	}
	if subscription.CLIStartup != nil && !writeJSON(map[string]any{"type": "cli-startup", "pending": subscription.CLIStarting}) {
		return
	}
	if !replay(subscription.History, subscription.HistoryOffset, subscription.HistoryReset) {
		return
	}
	subscription.History = nil
	startup := subscription.CLIStartup
	if !subscription.CLIStarting {
		startup = nil
	}
	heartbeat := time.NewTicker(terminalHeartbeatInterval)
	defer heartbeat.Stop()
	started := func() bool {
		startup = nil
		return writeJSON(map[string]any{"type": "cli-startup", "pending": false})
	}
	beat := func() bool {
		return ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(terminalWriteTimeout)) == nil &&
			writeJSON(map[string]string{"type": "heartbeat"})
	}
	pong := func() bool { return writeJSON(map[string]string{"type": "pong"}) }
	for {
		// Control replies are never starved by a continuous stream of output.
		select {
		case <-stop:
			return
		case <-startup:
			if !started() {
				return
			}
		case <-pongs:
			if !pong() {
				return
			}
		case <-heartbeat.C:
			if !beat() {
				return
			}
		default:
		}
		batch := subscription.next(terminalFrameLimit)
		switch {
		case batch.Resync && !resume && batch.Reset:
			// An original viewer cannot clear its screen. Reconnecting gives
			// it the kept history, as it did before resynchronisation.
			return
		case batch.Resync && resume:
			if !replay(batch.Data, batch.Offset, batch.Reset) {
				return
			}
			continue
		case len(batch.Data) > 0:
			if !writeBinary(batch.Data) {
				return
			}
			continue
		case batch.Done:
			if batch.Ended && writeJSON(map[string]string{"type": "exit"}) {
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			}
			return
		}
		select {
		case <-stop:
			return
		case <-subscription.Wake():
		case <-startup:
			if !started() {
				return
			}
		case <-pongs:
			if !pong() {
				return
			}
		case <-heartbeat.C:
			if !beat() {
				return
			}
		}
	}
}

func validTerminalSize(cols, rows uint16) bool {
	return cols > 1 && cols < 600 && rows > 1 && rows < 400
}
