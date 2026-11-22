// Package herdrlink filters one Herdr machine link: the byte stream between
// the owner's Herdr core (which dialled through Super Cage's SSH entry) and
// the core in the environment. It runs in the entry endpoint, outside the
// guest, so that nothing in the environment can reach around it.
//
// Herdr's protocol (herdr-core 0.1.21, wire.rs, remote.rs, server.rs): plain
// JSON-RPC lines until a hello with framed: true is answered, then frames
// [channel u32 LE][kind u8][len u32 LE][payload]. Channel 0 carries JSON-RPC;
// other channels are streams opened by a reply's result.channel.
//
// What the filter keeps:
//   - owner to guest: the hello's params.core.name (the owner's host name)
//     becomes a neutral name; everything else passes. The owner's core is
//     trusted: it is the owner's own program.
//   - guest to owner: replies, $/cancelRequest and stream bytes pass;
//     every other JSON-RPC message the guest sends is dropped, and a dropped
//     request is answered "denied" to the guest. These are Herdr's call-backs
//     (terminal.send, get, read, wait, notes): without the filter a program in
//     the environment can type into a terminal on the owner's computer (E14).
//   - stream bytes reach the owner only from channels a reply announced for
//     the owner's own terminal.attach or events.subscribe call, and only
//     through termfilter, as cage attach shows a Cage terminal. Bytes the
//     filter removes are credited back to the guest, or its flow-control
//     window would shrink until the stream stalled (wire.rs: credit counts
//     payload bytes).
//   - everything is bounded: lines, frames (Herdr's own 16 MiB), the banner
//     before the hello reply, and streams (one per announcing reply).
package herdrlink

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// Frame kinds (herdr-core wire.rs).
const (
	KindData           = 0
	KindCredit         = 1
	KindClose          = 2
	KindResize         = 3
	KindFocus          = 4
	KindFacts          = 5
	KindAutomaticInput = 6
)

const (
	// HeaderSize is a frame header's size.
	HeaderSize = 9
	// MaxPayload is Herdr's own frame limit; a larger header ends the link.
	MaxPayload = 16 << 20
	// MaxLine bounds one plain JSON line in either direction.
	MaxLine = 1 << 20
	// MaxBanner bounds what the guest may print before its hello reply (a
	// login banner, Herdr's own words); it is dropped.
	MaxBanner = 64 << 10
	// MaxClose bounds a CLOSE frame's reason.
	MaxClose = 64 << 10
	// MaxStreams bounds the streams open at once.
	MaxStreams = 1024
	// piece is how much of a stream frame is filtered at a time: a stream
	// frame is never held whole.
	piece = 32 << 10
)

// PeerName is what the environment's core calls the owner's core: the
// owner's host name never reaches it.
const PeerName = "super-cage"

// DeniedMessage answers a call-back the filter dropped.
const DeniedMessage = "Super Cage does not let the environment call back into this computer"

// ErrProtocol ends a link that broke the protocol's bounds.
var ErrProtocol = errors.New("herdr link: protocol violation")

// Options are the owner's choices for this link.
type Options struct {
	// ClipboardWrite passes OSC 52 clipboard writes from the environment's
	// terminals to the owner's Herdr panes, as the same setting does for
	// cage attach and ssh.
	ClipboardWrite bool
	// Window is how long after the last key or mouse press in a pane that
	// never reported focus the owner counts as at that pane
	// (attach.ClipboardWindow).
	Window time.Duration
	// Count hears of what the filter did, by kind; never content.
	Count func(event string)
}

// Result says how a link ended.
type Result struct {
	// OwnerEnded: the owner's side ended (hung up) before the guest's.
	OwnerEnded bool
	// Framed: the guest answered a framed hello (a machine link, not a probe).
	Framed bool
}

// Link is one link's state, shared by its two pumps.
type Link struct {
	opts Options

	toGuestMu sync.Mutex
	toGuest   io.Writer

	mu          sync.Mutex
	helloID     string // the owner's hello id, as a JSON token
	helloFramed bool
	pending     map[string]streamKind // owner calls whose reply opens a stream
	streams     map[uint32]*stream
	// closing are streams the owner closed: the guest's core answers with
	// a CLOSE of its own, which passes; nothing else on them does.
	closing map[uint32]bool

	framed     atomic.Bool // the guest answered a framed hello
	ownerEnded atomic.Bool
	guestEnded atomic.Bool
}

type streamKind uint8

const (
	streamTerminal streamKind = iota + 1 // terminal.attach
	streamEvents                         // events.subscribe
)

// stream is one announced stream from the guest.
type stream struct {
	filter   *termfilter.Filter
	presence *Presence
	// debt is what the guest spent that the owner will not credit (bytes
	// the filter removed or holds); credited is what this link credited.
	debt, credited int64
}

// New returns a link that writes toward the guest with toGuest.
func New(opts Options, toGuest io.Writer) *Link {
	return &Link{opts: opts, toGuest: toGuest, pending: map[string]streamKind{}, streams: map[uint32]*stream{}, closing: map[uint32]bool{}}
}

func (l *Link) count(event string) {
	if l.opts.Count != nil {
		l.opts.Count(event)
	}
}

// Run pumps both ways until the guest's side ends or either side breaks the
// protocol. When the owner's side ends first it closes toGuest (if it is a
// Closer) so the guest's core sees the end, and keeps relaying what the guest
// still sends. After Run returns the caller closes both sides, which ends a
// pump still reading; a cut is the same: the caller closes both sides.
func Run(opts Options, owner io.Reader, toOwner io.Writer, guest io.Reader, toGuest io.Writer) (Result, error) {
	l := New(opts, toGuest)
	up := make(chan error, 1)
	go func() {
		err := l.Up(owner)
		if err == nil && !l.guestEnded.Load() {
			l.ownerEnded.Store(true)
			if closer, ok := toGuest.(interface{ CloseWrite() error }); ok {
				_ = closer.CloseWrite()
			} else if closer, ok := toGuest.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		up <- err
	}()
	down := make(chan error, 1)
	go func() {
		err := l.Down(guest, toOwner)
		l.guestEnded.Store(true)
		down <- err
	}()
	result := func() Result { return Result{OwnerEnded: l.ownerEnded.Load(), Framed: l.framed.Load()} }
	for {
		select {
		case err := <-up:
			if err != nil {
				return result(), err
			}
			up = nil
		case err := <-down:
			return result(), err
		}
	}
}

// --- owner to guest ---

// Up copies what the owner's core sends to the guest, rewriting the hello
// and learning which calls open streams and where the owner's panes are.
// It returns nil at the owner's end of stream.
func (l *Link) Up(owner io.Reader) error {
	r := bufio.NewReaderSize(owner, 64<<10)
	for {
		line, err := readLine(r)
		if err == io.EOF && len(line) == 0 {
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		framed, out := l.ownerLine(line)
		if err := l.writeGuest(out); err != nil {
			return err
		}
		if err == io.EOF {
			return nil
		}
		if framed {
			break
		}
	}
	for {
		channel, kind, payload, err := readFrame(r, MaxPayload)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		l.ownerFrame(channel, kind, payload)
		if err := l.writeGuest(frame(channel, kind, payload)); err != nil {
			return err
		}
	}
}

// ownerLine handles one plain line from the owner: a hello gets the neutral
// name; a framed hello ends plain mode on this side.
func (l *Link) ownerLine(line []byte) (framed bool, out []byte) {
	trimmed := bytes.TrimSpace(line)
	var message map[string]json.RawMessage
	if json.Unmarshal(trimmed, &message) != nil {
		return false, line
	}
	var method string
	if json.Unmarshal(message["method"], &method) != nil || method != "hello" {
		return false, line
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(message["params"], &params) != nil {
		return false, line
	}
	if raw, ok := params["core"]; ok {
		var core map[string]json.RawMessage
		if json.Unmarshal(raw, &core) == nil {
			if _, named := core["name"]; named {
				core["name"], _ = json.Marshal(PeerName)
				params["core"], _ = json.Marshal(core)
				message["params"], _ = json.Marshal(params)
				l.count("hello:renamed")
			}
		}
	}
	var on bool
	_ = json.Unmarshal(params["framed"], &on)
	l.mu.Lock()
	l.helloID, l.helloFramed = token(message["id"]), on
	l.mu.Unlock()
	rewritten, err := json.Marshal(message)
	if err != nil {
		return on, line
	}
	return on, append(rewritten, '\n')
}

// ownerFrame learns from a frame the owner sends: its stream-opening calls,
// and its panes' focus and typing.
func (l *Link) ownerFrame(channel uint32, kind byte, payload []byte) {
	if channel == 0 {
		if kind != KindData {
			return
		}
		fields, ok := topLevel(payload)
		if !ok {
			return
		}
		var method string
		if json.Unmarshal(fields["method"], &method) != nil {
			return
		}
		id := token(fields["id"])
		if id == "" {
			return
		}
		var opens streamKind
		switch method {
		case "terminal.attach":
			opens = streamTerminal
		case "events.subscribe":
			opens = streamEvents
		default:
			return
		}
		l.mu.Lock()
		if len(l.pending) < MaxStreams {
			l.pending[id] = opens
		}
		l.mu.Unlock()
		return
	}
	l.mu.Lock()
	s := l.streams[channel]
	if kind == KindClose && s != nil {
		delete(l.streams, channel)
		if len(l.closing) < MaxStreams {
			l.closing[channel] = true
		}
	}
	l.mu.Unlock()
	if s == nil || s.presence == nil {
		return
	}
	switch kind {
	case KindFocus:
		s.presence.Focus(len(payload) > 0 && payload[0] == 1)
	case KindData:
		s.presence.Input(payload)
	}
}

// --- guest to owner ---

// Down copies what the guest's core sends to the owner, through the rules
// in the package comment. It returns nil at the guest's end of stream.
func (l *Link) Down(guest io.Reader, toOwner io.Writer) error {
	r := bufio.NewReaderSize(guest, 64<<10)
	banner := 0
	answered := false
	for {
		line, err := readLine(r)
		if len(line) > 0 {
			out, reply := l.guestLine(line, answered)
			switch {
			case reply:
				answered = true
			case !answered:
				banner += len(line)
				if banner > MaxBanner {
					return ErrProtocol
				}
			}
			if len(out) > 0 {
				if _, werr := toOwner.Write(out); werr != nil {
					return werr
				}
			}
			if l.framed.Load() {
				break
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	pieceBuf := make([]byte, piece)
	var out []byte
	for {
		var header [HeaderSize]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		channel := binary.LittleEndian.Uint32(header[0:4])
		kind := header[4]
		size := binary.LittleEndian.Uint32(header[5:9])
		if size > MaxPayload {
			return ErrProtocol
		}
		if channel == 0 {
			payload := make([]byte, size)
			if _, err := io.ReadFull(r, payload); err != nil {
				return eofIsUnexpected(err)
			}
			if kind != KindData {
				l.count("dropped:control-kind")
				continue
			}
			if err := l.guestMessage(payload, toOwner); err != nil {
				return err
			}
			continue
		}
		l.mu.Lock()
		s := l.streams[channel]
		answer := s == nil && kind == KindClose && l.closing[channel]
		l.mu.Unlock()
		switch {
		case answer:
			if size > MaxClose {
				return ErrProtocol
			}
			payload := make([]byte, size)
			if _, err := io.ReadFull(r, payload); err != nil {
				return eofIsUnexpected(err)
			}
			l.mu.Lock()
			delete(l.closing, channel)
			l.mu.Unlock()
			if _, err := toOwner.Write(frame(channel, KindClose, plainReason(payload))); err != nil {
				return err
			}
		case s == nil:
			l.count("dropped:unannounced")
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				return eofIsUnexpected(err)
			}
		case kind == KindData:
			remaining := int64(size)
			in, emitted := int64(0), int64(0)
			for remaining > 0 {
				n := min(remaining, piece)
				if _, err := io.ReadFull(r, pieceBuf[:n]); err != nil {
					return eofIsUnexpected(err)
				}
				remaining -= n
				in += n
				out = s.filter.Append(out[:0], pieceBuf[:n])
				if len(out) > 0 {
					if _, err := toOwner.Write(frame(channel, KindData, out)); err != nil {
						return err
					}
					emitted += int64(len(out))
				}
			}
			if err := l.creditBack(channel, s, in-emitted); err != nil {
				return err
			}
		case kind == KindCredit || kind == KindResize || kind == KindClose:
			limit := uint32(MaxClose)
			if kind != KindClose {
				limit = 4
			}
			if size > limit {
				return ErrProtocol
			}
			payload := make([]byte, size)
			if _, err := io.ReadFull(r, payload); err != nil {
				return eofIsUnexpected(err)
			}
			if kind == KindClose {
				l.mu.Lock()
				delete(l.streams, channel)
				l.mu.Unlock()
				payload = plainReason(payload)
			}
			if _, err := toOwner.Write(frame(channel, kind, payload)); err != nil {
				return err
			}
		default:
			// FOCUS, FACTS and AUTOMATIC_INPUT go from a viewer to a core,
			// never from the guest's core to the owner's.
			l.count("dropped:stream-kind")
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				return eofIsUnexpected(err)
			}
		}
	}
}

// plainReason keeps the printable text of a CLOSE frame's reason: the
// owner's hc may print it on a terminal.
func plainReason(reason []byte) []byte {
	out := reason[:0]
	for len(reason) > 0 {
		r, n := utf8.DecodeRune(reason)
		if r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f) && r != utf8.RuneError {
			out = append(out, reason[:n]...)
		}
		reason = reason[n:]
	}
	return out
}

// creditBack gives the guest the credit for bytes the owner will not see.
func (l *Link) creditBack(channel uint32, s *stream, removed int64) error {
	s.debt += removed
	if s.debt <= s.credited {
		return nil
	}
	give := s.debt - s.credited
	l.count("stream:filtered")
	for give > 0 {
		n := min(give, int64(^uint32(0)))
		var payload [4]byte
		binary.LittleEndian.PutUint32(payload[:], uint32(n))
		if err := l.writeGuest(frame(channel, KindCredit, payload[:])); err != nil {
			return err
		}
		give -= n
		s.credited += n
	}
	return nil
}

// guestLine handles one plain line from the guest: before the hello reply
// everything else is a banner and is dropped; after it, replies pass and
// messages with a method are dropped.
func (l *Link) guestLine(line []byte, answered bool) (out []byte, reply bool) {
	fields, ok := topLevel(bytes.TrimSpace(line))
	if !ok {
		if !answered {
			l.count("dropped:banner")
		} else {
			l.count("dropped:plain")
		}
		return nil, false
	}
	if _, has := fields["method"]; has {
		l.count("dropped:plain-method")
		return nil, false
	}
	if answered {
		return line, false
	}
	l.mu.Lock()
	helloID, helloFramed := l.helloID, l.helloFramed
	l.mu.Unlock()
	if helloID == "" || token(fields["id"]) != helloID {
		l.count("dropped:banner")
		return nil, false
	}
	if _, failed := fields["error"]; !failed && helloFramed {
		if _, ok := fields["result"]; ok {
			l.framed.Store(true)
		}
	}
	return line, true
}

// guestMessage handles one JSON-RPC message from the guest on channel 0.
func (l *Link) guestMessage(payload []byte, toOwner io.Writer) error {
	fields, ok := topLevel(payload)
	if !ok {
		l.count("dropped:malformed")
		return nil
	}
	if raw, has := fields["method"]; has {
		var method string
		if json.Unmarshal(raw, &method) == nil && method == "$/cancelRequest" {
			_, err := toOwner.Write(frame(0, KindData, payload))
			return err
		}
		l.count("dropped:" + knownMethod(method))
		if id, has := fields["id"]; has && token(id) != "null" {
			reply := []byte(`{"jsonrpc":"2.0","id":`)
			reply = append(reply, bytes.TrimSpace(id)...)
			reply = append(reply, `,"error":{"code":-32000,"message":`...)
			message, _ := json.Marshal(DeniedMessage)
			reply = append(reply, message...)
			reply = append(reply, `,"data":{"kind":"denied","hint":""}}}`...)
			return l.writeGuest(frame(0, KindData, reply))
		}
		return nil
	}
	// A reply: one that answers the owner's attach or subscribe call
	// announces the stream it opened.
	if id := token(fields["id"]); id != "" {
		l.mu.Lock()
		opens, pending := l.pending[id]
		delete(l.pending, id)
		l.mu.Unlock()
		if pending {
			if channel, ok := resultChannel(fields["result"]); ok && channel != 0 {
				l.open(channel, opens)
			}
		}
	}
	_, err := toOwner.Write(frame(0, KindData, payload))
	return err
}

// open starts filtering a stream a reply announced.
func (l *Link) open(channel uint32, kind streamKind) {
	s := &stream{}
	if kind == streamTerminal {
		s.presence = &Presence{window: l.opts.Window}
		s.filter = termfilter.New(termfilter.Options{ClipboardWrite: l.opts.ClipboardWrite, ClipboardNow: s.presence.Here})
	} else {
		// Events are JSON lines: nothing in them is a terminal's to act on.
		s.filter = termfilter.New(termfilter.Options{})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, open := l.streams[channel]; !open && len(l.streams) >= MaxStreams {
		l.count("dropped:too-many-streams")
		return
	}
	l.streams[channel] = s
	delete(l.closing, channel)
}

func (l *Link) writeGuest(data []byte) error {
	l.toGuestMu.Lock()
	defer l.toGuestMu.Unlock()
	_, err := l.toGuest.Write(data)
	return err
}

// --- presence ---

// Presence is "the owner is at this pane" for one Herdr stream, the rule cage
// attach applies to its window: Herdr's viewer reports focus with FOCUS
// frames, and the last one decides; a pane that never reported focus counts
// as the owner's for Window after a key or mouse press typed into it.
// Terminal replies (and focus reports) do not count as typing.
type Presence struct {
	window    time.Duration
	focus     atomic.Int32 // 0 never reported, 1 in, 2 out
	lastInput atomic.Int64
}

// Focus records a FOCUS frame.
func (p *Presence) Focus(in bool) {
	if in {
		p.focus.Store(1)
	} else {
		p.focus.Store(2)
	}
}

// Input records what the owner's pane sent as DATA.
func (p *Presence) Input(data []byte) {
	for i := 0; i < len(data); {
		n, kind, _ := termfilter.NextInput(data[i:])
		if n <= 0 {
			break
		}
		i += n
		if kind == termfilter.InputKey || kind == termfilter.InputMouse {
			p.lastInput.Store(time.Now().UnixNano())
			return
		}
	}
}

// Here reports whether the owner is at this pane now.
func (p *Presence) Here() bool {
	switch p.focus.Load() {
	case 1:
		return true
	case 2:
		return false
	}
	last := p.lastInput.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < p.window
}

// --- wire helpers ---

func frame(channel uint32, kind byte, payload []byte) []byte {
	out := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint32(out[0:4], channel)
	out[4] = kind
	binary.LittleEndian.PutUint32(out[5:9], uint32(len(payload)))
	copy(out[HeaderSize:], payload)
	return out
}

// Frame encodes one frame (tests and tools).
func Frame(channel uint32, kind byte, payload []byte) []byte { return frame(channel, kind, payload) }

func readFrame(r *bufio.Reader, limit uint32) (uint32, byte, []byte, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if err == io.EOF {
			return 0, 0, nil, io.EOF
		}
		return 0, 0, nil, err
	}
	size := binary.LittleEndian.Uint32(header[5:9])
	if size > limit {
		return 0, 0, nil, ErrProtocol
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, 0, nil, eofIsUnexpected(err)
	}
	return binary.LittleEndian.Uint32(header[0:4]), header[4], payload, nil
}

// readLine reads up to and including a newline, at most MaxLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxLine || len(line) >= MaxLine && err == bufio.ErrBufferFull {
			return nil, ErrProtocol
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func eofIsUnexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// topLevel reads a JSON object's members exactly: keys are matched as
// written (Go's struct decoding would ignore case), and a key that appears
// twice makes the message invalid, since the owner's parser may take the
// other one.
func topLevel(data []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		if _, twice := fields[name]; twice {
			return nil, false
		}
		fields[name] = value
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

// resultChannel reads result.channel of a stream-opening reply.
func resultChannel(raw json.RawMessage) (uint32, bool) {
	fields, ok := topLevel(raw)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(string(bytes.TrimSpace(fields["channel"])), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// token is a JSON value's text, the form ids are compared in.
func token(raw json.RawMessage) string { return string(bytes.TrimSpace(raw)) }

// knownMethod names a dropped method for the counters; anything else the
// guest made up counts as "other".
func knownMethod(method string) string {
	switch method {
	case "terminal.send", "terminal.get", "terminal.read", "terminal.wait", "notes", "$/orphaned", "hello",
		"terminal.run", "terminal.attach", "terminal.keys", "terminal.put", "machine.exec", "events.subscribe":
		return method
	}
	return "other"
}
