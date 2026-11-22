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
//     becomes a neutral name, and the owner's core id becomes a pseudonym
//     of this environment's (Pseudonym) wherever the owner's core writes it
//     on channel 0 (the hello, callers, parents); everything else passes.
//     The owner's core is trusted: it is the owner's own program.
//   - guest to owner: a reply or an event line holding an address of the
//     owner's own terminals ("<owner id>/t1", "<owner name>/t1") does not
//     reach the owner: Herdr's core reads those as its own (remote.rs
//     localize_in), so a forged "finished" would end hc wait t1 there.
//   - guest to owner: nothing structural the guest wrote reaches the owner
//     byte for byte. The hello reply and every channel-0 message are read
//     and written again as Go encodes them (canonical): the owner's core
//     reads JSON with its own parser, and wherever the two parsers disagree
//     the guest could choose what the owner's core reads (review 3, H1).
//   - guest to owner: one reply per call the owner made, and
//     $/cancelRequest, pass; every other JSON-RPC message the guest sends is
//     dropped, and a dropped request is answered "denied" to the guest. These are Herdr's call-backs
//     (terminal.send, get, read, wait, notes): without the filter a program in
//     the environment can type into a terminal on the owner's computer (E14).
//   - stream bytes reach the owner only from channels a reply announced for
//     the owner's own terminal.attach or events.subscribe call: a terminal's
//     through termfilter, as cage attach shows a Cage terminal; the events
//     stream as JSON lines, each read and written again like a channel-0
//     message. Bytes the
//     filter removes are credited back to the guest, or its flow-control
//     window would shrink until the stream stalled (wire.rs: credit counts
//     payload bytes).
//   - everything is bounded: lines, frames (Herdr's own 16 MiB), the banner
//     before the hello reply, and streams (one per announcing reply).
package herdrlink

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
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
	// MaxDepth bounds how deeply a JSON value from the guest nests: the
	// owner's JSON reader (serde_json) refuses 128 levels.
	MaxDepth = 64
	// MaxPending bounds the owner's calls waiting for their replies.
	MaxPending = 4096
	// callTTL and waitingTTL are how long a call waits for its reply before
	// a late one is dropped: Herdr gives a call its machine timeout plus
	// the wait it asks for, at most a day (remote.rs deadline_for).
	callTTL    = time.Hour
	waitingTTL = 25 * time.Hour
	// piece is how much of a stream frame is filtered at a time: a stream
	// frame is never held whole.
	piece = 32 << 10
)

// PeerName is what the environment's core calls the owner's core: the
// owner's host name never reaches it.
const PeerName = "super-cage"

// OwnerAddressMessage answers the owner's call in place of a reply that
// named one of the owner's own terminals.
const OwnerAddressMessage = "Super Cage dropped the environment's reply: it named a terminal on this computer"

// DeniedMessage answers a call-back the filter dropped.
const DeniedMessage = "Super Cage does not let the environment call back into this computer"

// Pseudonym is what the environment calls the owner's core whose id is id:
// the same for one key, unrelated across keys, shaped like a core id (32
// hexadecimal digits).
func Pseudonym(key []byte, id string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

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
	// IDKey is the environment's own key for the owner's core id: the
	// environment sees Pseudonym(IDKey, id) instead (none: a key of this
	// link's own).
	IDKey []byte
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
	key       []byte // IDKey, or this link's own

	mu          sync.Mutex
	helloID     string // the owner's hello id, canonical
	helloFramed bool
	// ownerID and ownerName are the owner's core id and name from its
	// hello; alias is the pseudonym the environment sees for ownerID.
	ownerID, ownerName, alias string
	// pending are the owner's calls still waiting for their reply, by
	// canonical id: one reply each reaches the owner.
	pending map[string]ownerCall
	streams map[uint32]*stream
	// closing are streams the owner closed: the guest's core answers with
	// a CLOSE of its own, which passes; nothing else on them does.
	closing map[uint32]bool

	framed     atomic.Bool // the guest answered a framed hello
	ownerEnded atomic.Bool
	guestEnded atomic.Bool
}

type streamKind uint8

// ownerCall is one of the owner's calls waiting for its reply.
type ownerCall struct {
	opens streamKind // the stream its reply announces; 0 for none
	until time.Time
}

const (
	streamTerminal streamKind = iota + 1 // terminal.attach
	streamEvents                         // events.subscribe
)

// stream is one announced stream from the guest: a terminal's (filter)
// or the events stream (events, with held its unfinished line).
type stream struct {
	filter   *termfilter.Filter
	presence *Presence
	events   bool
	held     []byte
	// debt is what the guest spent that the owner will not credit (bytes
	// the filter removed or holds); credited is what this link credited.
	debt, credited int64
}

// New returns a link that writes toward the guest with toGuest.
func New(opts Options, toGuest io.Writer) *Link {
	key := opts.IDKey
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	return &Link{opts: opts, toGuest: toGuest, key: key, pending: map[string]ownerCall{}, streams: map[uint32]*stream{}, closing: map[uint32]bool{}}
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
		if err := l.ownerFrame(channel, kind, payload); err != nil {
			return err
		}
		if channel == 0 {
			payload = l.aliasID(payload)
		}
		if err := l.writeGuest(frame(channel, kind, payload)); err != nil {
			return err
		}
	}
}

// ownerLine handles one plain line from the owner: a hello gets the neutral
// name and the pseudonym; a framed hello ends plain mode on this side.
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
			var id, name string
			_ = json.Unmarshal(core["id"], &id)
			_ = json.Unmarshal(core["name"], &name)
			if _, named := core["name"]; named {
				core["name"], _ = json.Marshal(PeerName)
				l.count("hello:renamed")
			}
			l.mu.Lock()
			l.ownerName = name
			if len(id) >= minIDLength {
				l.ownerID, l.alias = id, Pseudonym(l.key, id)
				core["id"], _ = json.Marshal(l.alias)
				l.count("hello:pseudonym")
			}
			l.mu.Unlock()
			params["core"], _ = json.Marshal(core)
			message["params"], _ = json.Marshal(params)
		}
	}
	var on bool
	_ = json.Unmarshal(params["framed"], &on)
	l.mu.Lock()
	l.helloID, l.helloFramed = canonicalToken(message["id"]), on
	l.mu.Unlock()
	rewritten, err := json.Marshal(message)
	if err != nil {
		return on, line
	}
	return on, append(l.aliasID(rewritten), '\n')
}

// minIDLength is the shortest owner core id the filter replaces byte for
// byte (Herdr's are 32 hexadecimal digits, platform random_hex).
const minIDLength = 8

// aliasID gives the environment the pseudonym wherever the owner's core
// wrote its id: callers ("<id>/t3", remote.rs route), parents ($/orphaned,
// remote.rs orphaned_afar), waits on children (api/waiting.rs). Herdr
// writes JSON without escaping letters, so the id's own bytes are all its
// spellings.
func (l *Link) aliasID(payload []byte) []byte {
	l.mu.Lock()
	id, alias := l.ownerID, l.alias
	l.mu.Unlock()
	if id == "" || !bytes.Contains(payload, []byte(id)) {
		return payload
	}
	return bytes.ReplaceAll(payload, []byte(id), []byte(alias))
}

// addressKeys are the members Herdr's core reads addresses from
// (remote.rs map_addresses), and callers.
var addressKeys = map[string]bool{"address": true, "parent": true, "terminal": true, "from": true, "removed": true, "caller": true}

// namesOwner reports whether a value from the guest holds an address of the
// owner's own terminals: "<owner id>/…" anywhere, or "<owner name>/…" where
// Herdr reads an address. The environment knows neither (it has the
// pseudonym and PeerName), so only a forgery holds one.
func (l *Link) namesOwner(value any) bool {
	l.mu.Lock()
	id, name := l.ownerID, l.ownerName
	l.mu.Unlock()
	var walk func(value any, key string) bool
	walk = func(value any, key string) bool {
		switch v := value.(type) {
		case string:
			return id != "" && strings.HasPrefix(v, id+"/") || name != "" && addressKeys[key] && strings.HasPrefix(v, name+"/")
		case map[string]any:
			for k, item := range v {
				if id != "" && strings.HasPrefix(k, id+"/") || walk(item, k) {
					return true
				}
			}
		case []any:
			for _, item := range v {
				if walk(item, key) {
					return true
				}
			}
		}
		return false
	}
	return walk(value, "")
}

// ownerFrame learns from a frame the owner sends: its calls (which open
// streams), and its panes' focus and typing. Too many calls waiting for
// replies the guest withholds end the link.
func (l *Link) ownerFrame(channel uint32, kind byte, payload []byte) error {
	if channel == 0 {
		if kind != KindData {
			return nil
		}
		fields, ok := topLevel(payload)
		if !ok {
			return nil
		}
		var method string
		if json.Unmarshal(fields["method"], &method) != nil {
			return nil
		}
		id := canonicalToken(fields["id"])
		if id == "" || id == "null" {
			return nil
		}
		now := time.Now()
		c := ownerCall{until: now.Add(callTTL)}
		switch method {
		case "terminal.attach":
			c.opens = streamTerminal
		case "events.subscribe":
			c.opens = streamEvents
		case "terminal.wait", "terminal.run", "terminal.send", "terminal.put", "machine.exec":
			c.until = now.Add(waitingTTL)
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if len(l.pending) >= MaxPending {
			for id, waiting := range l.pending {
				if now.After(waiting.until) {
					delete(l.pending, id)
				}
			}
			if len(l.pending) >= MaxPending {
				return ErrProtocol
			}
		}
		l.pending[id] = c
		return nil
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
		return nil
	}
	switch kind {
	case KindFocus:
		s.presence.Focus(len(payload) > 0 && payload[0] == 1)
	case KindData:
		s.presence.Input(payload)
	}
	return nil
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
			out, reply, lerr := l.guestLine(line, answered)
			if lerr != nil {
				return lerr
			}
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
		case kind == KindData && s.events:
			remaining := int64(size)
			in, emitted := int64(0), int64(0)
			for remaining > 0 {
				n := min(remaining, piece)
				if _, err := io.ReadFull(r, pieceBuf[:n]); err != nil {
					return eofIsUnexpected(err)
				}
				remaining -= n
				in += n
				var err error
				out, err = l.eventLines(s, out[:0], pieceBuf[:n])
				if err != nil {
					return err
				}
				if len(out) > 0 {
					if _, err := toOwner.Write(frame(channel, KindData, out)); err != nil {
						return err
					}
					emitted += int64(len(out))
				}
			}
			// A line held whole is credited now: one longer than the
			// guest's window would otherwise stall it. Its bytes are
			// credited again when the owner reads them; Herdr adds
			// credit without a cap (wire.rs credit).
			if err := l.creditBack(channel, s, in-emitted); err != nil {
				return err
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

// eventLines reads the events stream the way the owner's core does, as JSON
// lines (remote.rs mirror_events), and passes each whole line written again
// (canonical), unless it does not read or names the owner's own terminals.
// An unfinished line longer than MaxLine ends the link.
func (l *Link) eventLines(s *stream, out, data []byte) ([]byte, error) {
	s.held = append(s.held, data...)
	rest := s.held
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		line := rest[:i]
		rest = rest[i+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		value, canon, err := canonical(line)
		switch {
		case err != nil:
			l.count("dropped:event")
		case l.namesOwner(value):
			l.count("dropped:owner-address")
		default:
			out = append(append(out, canon...), '\n')
		}
	}
	if len(rest) > MaxLine {
		return out, ErrProtocol
	}
	if len(rest) == 0 {
		s.held = nil
	} else if len(rest) < len(s.held) {
		s.held = append([]byte(nil), rest...)
	}
	return out, nil
}

// plainReason keeps the printable text of a CLOSE frame's reason: the
// owner's core hands it to the viewer as words, and hc may print it on a
// terminal. No control or format character (a bidirectional override)
// passes.
func plainReason(reason []byte) []byte {
	out := reason[:0]
	for len(reason) > 0 {
		r, n := utf8.DecodeRune(reason)
		if r != utf8.RuneError && unicode.IsPrint(r) {
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

// guestLine handles one plain line from the guest. Before the hello reply
// every other line is a banner and is dropped. The reply reaches the owner
// only as this filter writes it again, alone on its line: the owner's core
// takes the first line its own JSON reader accepts, so with the guest's own
// bytes the two could disagree about where frames start (review 3, H1). A
// line with the hello's id that is not a well-formed reply ends the link.
// After a reply that does not switch to frames (a probe) nothing more
// passes: the owner hangs up.
func (l *Link) guestLine(line []byte, answered bool) (out []byte, reply bool, err error) {
	if answered {
		l.count("dropped:plain")
		return nil, false, nil
	}
	l.mu.Lock()
	helloID, helloFramed := l.helloID, l.helloFramed
	l.mu.Unlock()
	fields, ok := topLevel(bytes.TrimSpace(line))
	if !ok || helloID == "" || canonicalToken(fields["id"]) != helloID {
		l.count("dropped:banner")
		return nil, false, nil
	}
	value, out, err := canonical(bytes.TrimSpace(line))
	if err != nil {
		l.count("ended:hello")
		return nil, false, ErrProtocol
	}
	message := value.(map[string]any)
	for key := range message {
		switch key {
		case "jsonrpc", "id", "result", "error":
		default:
			l.count("ended:hello")
			return nil, false, ErrProtocol
		}
	}
	result, hasResult := message["result"]
	failure, hasError := message["error"]
	_, resultObject := result.(map[string]any)
	_, errorObject := failure.(map[string]any)
	if hasResult == hasError || hasResult && !resultObject || hasError && !errorObject {
		l.count("ended:hello")
		return nil, false, ErrProtocol
	}
	if hasResult && helloFramed {
		l.framed.Store(true)
	}
	return append(out, '\n'), true, nil
}

// guestMessage handles one JSON-RPC message from the guest on channel 0.
// The owner gets it only as Go writes it again; one that does not read
// ends the link. A reply passes once, to a call the owner made; a reply to
// the owner's terminal.attach or events.subscribe announces its stream.
func (l *Link) guestMessage(payload []byte, toOwner io.Writer) error {
	value, out, err := canonical(payload)
	if err != nil {
		l.count("ended:malformed")
		return ErrProtocol
	}
	message, ok := value.(map[string]any)
	if !ok {
		// A batch or a bare value: Herdr's core reads it as a reply
		// without an id, and drops it.
		l.count("dropped:malformed")
		return nil
	}
	if raw, has := message["method"]; has {
		method, _ := raw.(string)
		if method == "$/cancelRequest" {
			_, err := toOwner.Write(frame(0, KindData, out))
			return err
		}
		l.count("dropped:" + knownMethod(method))
		if id, has := message["id"]; has && id != nil {
			idText, _ := encode(id)
			reply := []byte(`{"jsonrpc":"2.0","id":`)
			reply = append(reply, idText...)
			reply = append(reply, `,"error":{"code":-32000,"message":`...)
			denied, _ := json.Marshal(DeniedMessage)
			reply = append(reply, denied...)
			reply = append(reply, `,"data":{"kind":"denied","hint":""}}}`...)
			return l.writeGuest(frame(0, KindData, reply))
		}
		return nil
	}
	id := ""
	if raw, has := message["id"]; has {
		idText, _ := encode(raw)
		id = string(idText)
	}
	l.mu.Lock()
	asked, pending := l.pending[id]
	delete(l.pending, id)
	l.mu.Unlock()
	if !pending {
		// Herdr's core would take a second reply to its events.subscribe
		// call for a new mirror channel (remote.rs hold): only the first
		// reply to a call passes.
		l.count("dropped:unasked")
		return nil
	}
	if l.namesOwner(message) {
		l.count("dropped:owner-address")
		reply, _ := encode(map[string]any{"jsonrpc": "2.0", "id": message["id"], "error": map[string]any{
			"code": -32000, "message": OwnerAddressMessage, "data": map[string]any{"kind": "denied", "hint": ""},
		}})
		_, err := toOwner.Write(frame(0, KindData, reply))
		return err
	}
	if asked.opens != 0 {
		if result, ok := message["result"].(map[string]any); ok {
			if raw, has := result["channel"]; has {
				channel, ok := streamChannel(raw)
				if !ok {
					l.count("ended:channel")
					return ErrProtocol
				}
				if err := l.open(channel, asked.opens); err != nil {
					return err
				}
			}
		}
	}
	_, err = toOwner.Write(frame(0, KindData, out))
	return err
}

// streamChannel is a stream's channel as a reply announces it: an integer
// from 1 to 2^32-1 (Herdr's core cuts a larger one to 32 bits).
func streamChannel(raw any) (uint32, bool) {
	n, ok := raw.(float64)
	if !ok || n < 1 || n > math.MaxUint32 || n != math.Trunc(n) {
		return 0, false
	}
	return uint32(n), true
}

// open starts filtering a stream a reply announced. A channel already open,
// or closed by the owner and not yet answered, ends the link: the owner's
// core would give it to the new stream, the filter to the old one.
func (l *Link) open(channel uint32, kind streamKind) error {
	s := &stream{events: kind == streamEvents}
	if kind == streamTerminal {
		s.presence = &Presence{window: l.opts.Window}
		s.filter = termfilter.New(termfilter.Options{ClipboardWrite: l.opts.ClipboardWrite, ClipboardNow: s.presence.Here})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, open := l.streams[channel]; open || l.closing[channel] {
		l.count("ended:channel")
		return ErrProtocol
	}
	if len(l.streams) >= MaxStreams {
		l.count("dropped:too-many-streams")
		return nil
	}
	l.streams[channel] = s
	return nil
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
// Terminal replies (and focus reports) do not count as typing. Input is
// read as ssh and attach windows read theirs (termfilter.InputReader): a
// reply cut across DATA frames is still a reply.
type Presence struct {
	window    time.Duration
	focus     atomic.Int32 // 0 never reported, 1 in, 2 out
	lastInput atomic.Int64
	// input is the owner-side pump's alone.
	input termfilter.InputReader
}

// Focus records a FOCUS frame.
func (p *Presence) Focus(in bool) {
	if in {
		p.focus.Store(1)
	} else {
		p.focus.Store(2)
	}
}

// Input records what the owner's pane sent as DATA. A sequence cut at the
// end of a frame waits for the next; one that never completes never counts.
func (p *Presence) Input(data []byte) {
	typed := false
	p.input.Read(data, func(_ []byte, kind termfilter.InputKind, _ termfilter.Reply) {
		typed = typed || kind == termfilter.InputKey || kind == termfilter.InputMouse || kind == termfilter.InputPaste
	})
	if typed {
		p.lastInput.Store(time.Now().UnixNano())
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

// canonical reads one JSON value the guest wrote and writes it again as Go
// encodes it: compact, valid UTF-8, no raw line break, no lone surrogate,
// numbers within float64's range, keys in order. The owner's core reads
// only these bytes, never the guest's own, so its JSON reader and Go's
// cannot disagree about them (review 3, H1). A value nested deeper than
// MaxDepth, a number out of range, or an encoding over MaxPayload is
// refused.
func canonical(data []byte) (any, []byte, error) {
	if depth(data) > MaxDepth {
		return nil, nil, ErrProtocol
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, nil, ErrProtocol
	}
	out, err := encode(value)
	if err != nil || len(out) > MaxPayload {
		return nil, nil, ErrProtocol
	}
	return value, out, nil
}

// encode writes a decoded value compactly, without HTML escapes.
func encode(value any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// depth is how deeply a JSON text nests, strings aside.
func depth(data []byte) int {
	deepest, level := 0, 0
	inString, escaped := false, false
	for _, c := range data {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			level++
			deepest = max(deepest, level)
		case c == '}' || c == ']':
			level--
		}
	}
	return deepest
}

// canonicalToken is a JSON value's canonical text, the form ids are
// compared in; "" for one that does not read.
func canonicalToken(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	_, out, err := canonical(raw)
	if err != nil {
		return ""
	}
	return string(out)
}

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
