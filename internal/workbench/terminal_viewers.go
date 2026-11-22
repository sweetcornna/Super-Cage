package workbench

import (
	"bytes"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/termfilter"
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
//     is answered once. So does pointer motion with no button held, which
//     a window sends as the pointer passes over it: it is not typing.
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
	// terminalQueryLive: how long after the output asked a viewer something
	// an input shaped like the answer is taken as that answer. Shift+F3 in
	// xterm's encoding has the shape of a cursor position answer.
	terminalQueryLive = 5 * time.Second
	// terminalAskSeconds: owed answers are counted per second, so each query
	// is owed for terminalQueryLive and less than a second more, however
	// many queries follow it.
	terminalAskSeconds = int(terminalQueryLive/time.Second) + 1
)

// terminalOwed counts the answers one viewer owes for the queries of one
// kind shown to it in one second.
type terminalOwed struct {
	second int64 // seconds since the viewer's first query
	n      uint16
}

type terminalPeer struct {
	key        string
	legacy     bool // no identity: today's behaviour
	phone      bool // the phone's HTTP input and resize
	observe    bool // never types, never sizes
	cols, rows uint16
	live       int       // open streams of this identity
	seen       time.Time // phone: last request
	active     time.Time // last typing, else when it connected
	// asked counts, per kind and second, the queries this viewer was shown
	// and has not answered yet; askedFrom is when the first one was asked.
	asked     [termfilter.ReplyKinds][terminalAskSeconds]terminalOwed
	askedFrom time.Time
}

type terminalSizing struct {
	peers      map[string]*terminalPeer
	owner      string             // key of the size owner
	typist     bool               // the owner became owner by typing
	cols, rows uint16             // the PTY's size as last applied
	scan       *termfilter.Filter // reads the output for queries
	scanned    []byte
	sequence   uint64
	grace      *time.Timer
}

func (z *terminalSizing) init(cols, rows uint16) {
	z.peers, z.cols, z.rows = map[string]*terminalPeer{}, cols, rows
	z.scan = termfilter.New(termfilter.Options{OnQuery: z.noteQuery})
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
// The phone's reads carry no identity, so every phone shares this one, as
// before. viewer is the identity a phone sends with its input and resize: a
// phone that sends one takes part in the viewer rules, one that does not (an
// app from before them) keeps the phone's earlier behaviour.
func (t *terminalSession) phonePeer(viewer ...string) *terminalPeer {
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
	if len(viewer) > 0 {
		p.legacy = viewer[0] == ""
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
	if cols != 0 && !same {
		t.sizing.cols, t.sizing.rows = cols, rows
	}
	t.mu.Unlock()
	if cols == 0 || same {
		return nil
	}
	t.resize(cols, rows)
	return nil
}

// terminalResizeWait is how long a request waits for its size to reach the
// PTY. A program that stopped reading (a Windows guest takes sizes through
// a pipe) can keep a resize from returning; then the request goes on
// without it, and the size is applied when the program reads again.
var terminalResizeWait = 250 * time.Millisecond

// terminalResizer applies sizes to the PTY off the request path, one at a
// time, the latest wanted size winning. Guarded by its own mutex: nothing
// else is held while the PTY takes a size.
type terminalResizer struct {
	mu      sync.Mutex
	cols    uint16
	rows    uint16
	pending bool
	busy    bool
	waiters []chan struct{}
}

// resize asks for a PTY size and waits for it, at most terminalResizeWait,
// and not at all while an earlier size is still being applied.
func (t *terminalSession) resize(cols, rows uint16) {
	r := &t.resizer
	done := make(chan struct{})
	r.mu.Lock()
	r.cols, r.rows, r.pending = cols, rows, true
	r.waiters = append(r.waiters, done)
	stuck := r.busy
	if !r.busy {
		r.busy = true
		go t.applyResizes()
	}
	r.mu.Unlock()
	if stuck {
		return
	}
	timer := time.NewTimer(terminalResizeWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (t *terminalSession) applyResizes() {
	r := &t.resizer
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.pending {
		cols, rows, waiters := r.cols, r.rows, r.waiters
		r.pending, r.waiters = false, nil
		r.mu.Unlock()
		_ = t.file.Resize(cols, rows)
		r.mu.Lock()
		// Idle again before the waiters go on, so a request made as soon
		// as one returns waits for its own size.
		r.busy = r.pending
		for _, done := range waiters {
			close(done)
		}
	}
	r.busy = false
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
	legacy := p.legacy // a phone's request can change it
	decides := !p.observe && (legacy || z.owner == p.key || !z.typist || !z.liveLocked(z.owner, time.Now()))
	if decides && z.owner != p.key {
		z.owner, z.typist = p.key, false
	}
	t.mu.Unlock()
	if !decides {
		return nil
	}
	if legacy {
		// As before: every resize from such a viewer reaches the PTY.
		t.mu.Lock()
		z.cols, z.rows = cols, rows
		t.mu.Unlock()
		t.resize(cols, rows)
		return nil
	}
	return t.applySizeOf(p)
}

// peerInput returns what of a viewer's input reaches the program. Typing
// makes the viewer the owner. Answers to queries pass only from the owner,
// or while no owner shows the terminal; they never move ownership. A viewer
// without identity keeps its behaviour from before these rules: its input
// passes whole and its typing never resizes.
func (t *terminalSession) peerInput(p *terminalPeer, data string) (string, error) {
	now := time.Now()
	t.mu.Lock()
	if p.observe {
		t.mu.Unlock()
		return "", nil
	}
	typing, rest := t.sizing.splitReplies(p, data, now)
	t.mu.Unlock()
	t.sizeMu.Lock()
	defer t.sizeMu.Unlock()
	t.mu.Lock()
	z := &t.sizing
	if p.phone {
		p.seen = now
	}
	if typing {
		p.active = now
		claim := z.owner != p.key && !p.legacy
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

// noteOutput reads the program's output for queries. Caller holds mu.
func (z *terminalSizing) noteOutput(chunk []byte) {
	if len(chunk) == 0 || z.scan == nil {
		return
	}
	z.scanned = z.scan.Append(z.scanned[:0], chunk)
	if cap(z.scanned) > 64<<10 {
		z.scanned = nil
	}
}

// noteQuery records a query for every viewer showing the terminal now: each
// may answer it once. The phone is told of it by the read that returns it.
// Caller holds mu.
func (z *terminalSizing) noteQuery(kind termfilter.Reply) {
	now := time.Now()
	for _, p := range z.peers {
		if !p.observe && !p.phone && z.liveLocked(p.key, now) {
			p.askLocked(kind, 1, now)
		}
	}
}

// askLocked records n queries of one kind shown to this viewer. Caller
// holds mu.
func (p *terminalPeer) askLocked(kind termfilter.Reply, n int, now time.Time) {
	if p.askedFrom.IsZero() {
		p.askedFrom = now
	}
	second := p.askSecond(now)
	owed := &p.asked[kind][second%int64(terminalAskSeconds)]
	if owed.second != second {
		*owed = terminalOwed{second: second}
	}
	owed.n = uint16(min(int(owed.n)+n, math.MaxUint16))
}

// payLocked takes an answer of one kind for the oldest query of that kind
// still owed, and reports whether there was one. A second that saw more
// queries than its count holds stays owed until it expires: past that
// flood every answer is taken as one. Caller holds mu.
func (p *terminalPeer) payLocked(kind termfilter.Reply, now time.Time) bool {
	if p.askedFrom.IsZero() {
		return false
	}
	second := p.askSecond(now)
	var oldest *terminalOwed
	for i := range p.asked[kind] {
		owed := &p.asked[kind][i]
		if owed.n > 0 && second-owed.second < int64(terminalAskSeconds) && (oldest == nil || owed.second < oldest.second) {
			oldest = owed
		}
	}
	if oldest == nil {
		return false
	}
	if oldest.n < math.MaxUint16 {
		oldest.n--
	}
	return true
}

// askSecond is the second of the viewer's queries now falls in.
func (p *terminalPeer) askSecond(now time.Time) int64 {
	return max(0, int64(now.Sub(p.askedFrom)/time.Second))
}

// showQueries records the queries in output a viewer is shown apart from the
// live stream: the history replayed when it opens, or a phone's read. A
// viewer without identity is skipped, its input passes whole anyway; not
// the phone, which may send its identity only after its first read.
func (t *terminalSession) showQueries(p *terminalPeer, data []byte) {
	t.mu.Lock()
	skip := p.legacy && !p.phone || p.observe
	t.mu.Unlock()
	if skip || bytes.IndexByte(data, 0x1b) < 0 {
		return
	}
	counts := termfilter.Queries(data)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for kind, n := range counts {
		if n > 0 {
			p.askLocked(termfilter.Reply(kind), n, now)
		}
	}
}

// splitReplies reads a viewer's input. typing is false when it holds only
// what a terminal application sends by itself: answers to queries this
// viewer was asked, focus reports and pointer motion with no button held.
// rest is the input without them.
// Anything else is typing, whatever it looks like: a pasted answer, an
// answer nobody asked for, Alt+P. Caller holds mu.
func (z *terminalSizing) splitReplies(p *terminalPeer, data string, now time.Time) (typing bool, rest string) {
	if strings.IndexByte(data, 0x1b) < 0 {
		return data != "", data
	}
	var kept strings.Builder
	dropped := false
	for i := 0; i < len(data); {
		if strings.HasPrefix(data[i:], "\x1b[200~") {
			// A bracketed paste is typing, whatever it contains.
			end := strings.Index(data[i:], "\x1b[201~")
			if end < 0 {
				end = len(data) - i
			} else {
				end += len("\x1b[201~")
			}
			typing = true
			kept.WriteString(data[i : i+end])
			i += end
			continue
		}
		n, kind, reply := termfilter.NextInput([]byte(data[i:]))
		unit := data[i : i+n]
		i += n
		switch {
		case kind == termfilter.InputFocusIn || kind == termfilter.InputFocusOut:
			dropped = true
			continue
		case kind == termfilter.InputMotion:
			// The pointer passing over a window, no button held.
			dropped = true
			continue
		case kind == termfilter.InputReply && p.payLocked(reply, now):
			dropped = true
			continue
		}
		typing = true
		kept.WriteString(unit)
	}
	if !dropped {
		return typing, data
	}
	return typing, kept.String()
}
