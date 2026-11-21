package workbench

import (
	"bytes"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// One terminal can be shown by several viewers at once: desktop tabs, the
// phone, and local terminal applications through cage attach. Each viewer
// has its own window size, and each terminal emulator answers the queries a
// program writes (cursor position, attributes, colours). Two rules keep them
// from fighting:
//
//   - The size owner is the viewer that typed last. Its size is the PTY's.
//     Until someone types, or while the owner has gone, a resize applies at
//     once and makes its viewer the owner.
//   - Answers to queries reach the program only from the owner, so a query
//     is answered once.
//
// Viewers that do not identify themselves (older desktop clients, the phone)
// keep today's behaviour exactly: their resizes apply at once and their input
// is never filtered. They take part only as owners.

// terminalViewerPattern is a viewer identity chosen by the client. The same
// identity reconnecting keeps its size and ownership.
var terminalViewerPattern = regexp.MustCompile("^[A-Za-z0-9_-]{8,64}$")

// terminalViewerProtocol is announced to identified viewers in their first
// frame, so a client can tell this server from one that ignores the identity.
const terminalViewerProtocol = 1

// terminalViewers tells a viewer, before it creates a terminal, that this
// server applies the viewer rules; a server from before them answers 404,
// 405 or its page. cage attach asks it ahead of --new and --resume.
func (s *Server) terminalViewers(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, map[string]any{"protocol": terminalViewerProtocol})
}

// terminalOwnerGrace lets the owner reconnect (a network blip, a hidden tab
// resuming) before its ownership passes to another viewer.
var terminalOwnerGrace = 2 * time.Second

const (
	terminalPeerLimit = 32
	// terminalPhoneLive: the phone long-polls output for 25 seconds, so a
	// phone showing the terminal is seen at least this often.
	terminalPhoneLive = 30 * time.Second
	// terminalCursorQueryLive: how long after the output asked for the cursor
	// position an input shaped like the answer is taken as that answer. Shift+F3
	// in xterm's encoding has the same shape.
	terminalCursorQueryLive = 5 * time.Second
)

type terminalPeer struct {
	key        string
	legacy     bool // no identity: today's behaviour
	phone      bool // the phone's HTTP input and resize
	observe    bool // never types, never sizes
	cols, rows uint16
	live       int       // open streams of this identity
	seen       time.Time // phone: last request
	active     time.Time // last typing, else when it connected
}

type terminalSizing struct {
	peers      map[string]*terminalPeer
	owner      string // key of the size owner
	typist     bool   // the owner became owner by typing
	cols, rows uint16 // the PTY's size as last applied
	cursorAsk  time.Time
	tail       [4]byte // end of the previous output chunk, for split queries
	tailN      int
	sequence   uint64
	grace      *time.Timer
}

func (z *terminalSizing) init(cols, rows uint16) {
	z.peers, z.cols, z.rows = map[string]*terminalPeer{}, cols, rows
}

// liveLocked reports whether an identity still shows the terminal.
func (z *terminalSizing) liveLocked(key string, now time.Time) bool {
	p := z.peers[key]
	if p == nil {
		return false
	}
	if p.phone {
		return now.Sub(p.seen) < terminalPhoneLive
	}
	return p.live > 0
}

// openPeer registers one stream. id is empty for a viewer without identity.
func (t *terminalSession) openPeer(id string, observe bool) *terminalPeer {
	t.mu.Lock()
	defer t.mu.Unlock()
	z := &t.sizing
	if z.peers == nil {
		z.peers = map[string]*terminalPeer{}
	}
	now := time.Now()
	key := "v:" + id
	if id == "" {
		z.sequence++
		key = "s:" + strconv.FormatUint(z.sequence, 10)
	}
	p := z.peers[key]
	if p == nil {
		p = &terminalPeer{key: key, legacy: id == ""}
		z.peers[key] = p
		z.evictLocked()
	}
	p.observe = observe
	p.live++
	p.active = now
	if key == z.owner && z.grace != nil {
		z.grace.Stop()
		z.grace = nil
	}
	return p
}

// phonePeer is the identity of the phone's HTTP requests, marked as seen.
func (t *terminalSession) phonePeer() *terminalPeer {
	t.mu.Lock()
	defer t.mu.Unlock()
	z := &t.sizing
	if z.peers == nil {
		z.peers = map[string]*terminalPeer{}
	}
	p := z.peers["phone"]
	if p == nil {
		p = &terminalPeer{key: "phone", legacy: true, phone: true}
		z.peers["phone"] = p
		z.evictLocked()
	}
	p.seen = time.Now()
	return p
}

// evictLocked forgets the identities not shown anywhere, oldest first, past
// the bound.
func (z *terminalSizing) evictLocked() {
	now := time.Now()
	for len(z.peers) > terminalPeerLimit {
		var oldest *terminalPeer
		for _, p := range z.peers {
			if !z.liveLocked(p.key, now) && p.key != z.owner && (oldest == nil || p.active.Before(oldest.active)) {
				oldest = p
			}
		}
		if oldest == nil {
			return
		}
		delete(z.peers, oldest.key)
	}
}

// closePeer ends one stream. When the owner has no stream left, ownership
// passes to the most recently active viewer after a short grace.
func (t *terminalSession) closePeer(p *terminalPeer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	z := &t.sizing
	p.live--
	if p.legacy && p.live <= 0 {
		delete(z.peers, p.key)
	}
	if z.owner == p.key && !z.liveLocked(p.key, time.Now()) && !t.closed && z.grace == nil {
		z.grace = time.AfterFunc(terminalOwnerGrace, t.handOff)
	}
}

func (t *terminalSession) handOff() {
	t.sizeMu.Lock()
	defer t.sizeMu.Unlock()
	t.mu.Lock()
	z := &t.sizing
	z.grace = nil
	now := time.Now()
	if t.closed || z.liveLocked(z.owner, now) {
		t.mu.Unlock()
		return
	}
	var best *terminalPeer
	for _, p := range z.peers {
		if !p.observe && z.liveLocked(p.key, now) && (best == nil || p.active.After(best.active)) {
			best = p
		}
	}
	z.owner, z.typist = "", false
	if best == nil {
		t.mu.Unlock()
		return
	}
	z.owner = best.key
	t.mu.Unlock()
	t.applySizeOf(best)
}

// applySizeOf resizes the PTY to a peer's size. Caller holds sizeMu, not mu.
func (t *terminalSession) applySizeOf(p *terminalPeer) error {
	t.mu.Lock()
	cols, rows := p.cols, p.rows
	same := cols == t.sizing.cols && rows == t.sizing.rows
	t.mu.Unlock()
	if cols == 0 || same {
		return nil
	}
	if err := t.file.Resize(cols, rows); err != nil {
		return err
	}
	t.mu.Lock()
	t.sizing.cols, t.sizing.rows = cols, rows
	t.mu.Unlock()
	return nil
}

// peerResize records a viewer's size and applies it when that viewer decides
// the size. A viewer without identity always does, as before.
func (t *terminalSession) peerResize(p *terminalPeer, cols, rows uint16) error {
	t.sizeMu.Lock()
	defer t.sizeMu.Unlock()
	t.mu.Lock()
	z := &t.sizing
	p.cols, p.rows = cols, rows
	if p.phone {
		p.seen = time.Now()
	}
	decides := !p.observe && (p.legacy || z.owner == p.key || !z.typist || !z.liveLocked(z.owner, time.Now()))
	if decides && z.owner != p.key {
		z.owner, z.typist = p.key, false
	}
	t.mu.Unlock()
	if !decides {
		return nil
	}
	if p.legacy {
		// Exactly as before: every resize from such a viewer reaches the PTY.
		if err := t.file.Resize(cols, rows); err != nil {
			return err
		}
		t.mu.Lock()
		z.cols, z.rows = cols, rows
		t.mu.Unlock()
		return nil
	}
	return t.applySizeOf(p)
}

// peerInput returns what of a viewer's input reaches the program. Typing
// makes the viewer the owner. Answers to queries pass only from the owner,
// or while no owner shows the terminal; they never move ownership.
func (t *terminalSession) peerInput(p *terminalPeer, data string) (string, error) {
	if p.observe {
		return "", nil
	}
	t.mu.Lock()
	cursorAsked := time.Since(t.sizing.cursorAsk) < terminalCursorQueryLive
	t.mu.Unlock()
	typing, rest := splitTerminalReplies(data, cursorAsked)
	t.sizeMu.Lock()
	defer t.sizeMu.Unlock()
	t.mu.Lock()
	z := &t.sizing
	now := time.Now()
	if p.phone {
		p.seen = now
	}
	if typing {
		p.active = now
		claim := z.owner != p.key
		z.owner, z.typist = p.key, true
		t.mu.Unlock()
		if claim {
			if err := t.applySizeOf(p); err != nil {
				return "", err
			}
		}
		return data, nil
	}
	answers := p.legacy || z.owner == p.key || !z.liveLocked(z.owner, now)
	t.mu.Unlock()
	if answers {
		return data, nil
	}
	return rest, nil
}

// noteOutput watches the program's output for cursor position queries.
// Caller holds mu.
func (z *terminalSizing) noteOutput(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	found := bytes.Contains(chunk, []byte("\x1b[6n")) || bytes.Contains(chunk, []byte("\x1b[?6n"))
	if !found && z.tailN > 0 {
		var joined [9]byte
		n := copy(joined[:], z.tail[:z.tailN])
		n += copy(joined[n:], chunk[:min(len(chunk), 5)])
		found = bytes.Contains(joined[:n], []byte("\x1b[6n")) || bytes.Contains(joined[:n], []byte("\x1b[?6n"))
	}
	if found {
		z.cursorAsk = time.Now()
	}
	z.tailN = copy(z.tail[:], chunk[max(0, len(chunk)-4):])
}

// splitTerminalReplies reads a viewer's input. typing is false when it holds
// only what a terminal emulator sends by itself: answers to queries and focus
// reports. rest is the input without them.
func splitTerminalReplies(data string, cursorAsked bool) (typing bool, rest string) {
	if strings.IndexByte(data, 0x1b) < 0 {
		return data != "", data
	}
	var kept strings.Builder
	dropped := false
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			j := i + 1
			for j < len(data) && data[j] != 0x1b {
				j++
			}
			typing = true
			kept.WriteString(data[i:j])
			i = j
			continue
		}
		end, reply, paste := terminalInputSequence(data[i:], cursorAsked)
		if paste {
			// A bracketed paste is typing, whatever it contains.
			closing := strings.Index(data[i+end:], "\x1b[201~")
			if closing < 0 {
				end = len(data) - i
			} else {
				end += closing + len("\x1b[201~")
			}
		}
		if reply {
			dropped = true
		} else {
			typing = true
			kept.WriteString(data[i : i+end])
		}
		i += end
	}
	if !dropped {
		return typing, data
	}
	return typing, kept.String()
}

// terminalInputSequence measures the escape sequence at the start of s and
// reports whether a terminal emulator sends it by itself.
func terminalInputSequence(s string, cursorAsked bool) (n int, reply, paste bool) {
	if len(s) < 2 {
		return len(s), false, false
	}
	switch s[1] {
	case '[':
		i := 2
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i >= len(s) || s[i] < 0x40 || s[i] > 0x7e {
			return i, false, false // unfinished: a key the program reads itself
		}
		body, final := s[2:i], s[i]
		var marker byte
		if body != "" && body[0] >= '<' && body[0] <= '?' {
			marker = body[0]
		}
		switch {
		case body == "200" && final == '~':
			return i + 1, false, true
		case final == 'R':
			// Cursor position report, or Shift+F3 (CSI 1;2R) when no query
			// is waiting for one.
			return i + 1, cursorAsked && strings.Trim(body, "0123456789;?") == "", false
		case final == 'c' && (marker == '?' || marker == '>'), // DA1, DA2 answers
			final == 'n', // status reports
			final == 'y' && strings.HasSuffix(body, "$"), // DECRPM
			final == 'u' && marker == '?',                // kitty keyboard flags
			final == 't',                                 // window reports
			(final == 'I' || final == 'O') && body == "": // focus in and out
			return i + 1, true, false
		}
		return i + 1, false, false
	case ']', 'P', '_':
		// OSC, DCS and APC from a terminal are answers: colours, settings,
		// version, graphics status.
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 && s[1] == ']' {
				return i + 1, true, false
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2, true, false
			}
		}
		return len(s), true, false
	case 'O':
		return min(3, len(s)), false, false
	}
	return 2, false, false
}
