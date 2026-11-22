package termfilter

import (
	"bytes"
	"time"
)

// InputHold is how long an InputReader holds an escape sequence cut at the
// end of a read for its rest. A terminal application writes an answer to a
// query at once, so only a read boundary cuts one, and the rest is already
// in the kernel's buffer (locally it comes within a millisecond) or in the
// next packet. A lone Esc key waits this long, and it matters: it
// interrupts Claude Code and leaves insert mode in vim.
const InputHold = 25 * time.Millisecond

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// pasteLimit bounds a bracketed paste: past it the paste is over, and what
// follows is read as usual. Real pastes stay far under it.
const pasteLimit = 4 << 20

// InputReader classifies one window's input across reads, unit by unit as
// NextInput does. An escape sequence cut at the end of a read is held and
// completed by the next, so an answer to a query cut anywhere is still an
// answer and never a key: the program in the environment can ask for many
// answers, and they come back in reads cut at any byte. Held is shorter than
// any answer can be (replyLimit). An unfinished sequence longer than that is
// no answer: it is given as InputFragment at once, and so is the rest of it,
// as it comes, to its end. A complete sequence is NextInput's to classify;
// one shaped like an answer but refused by the answer grammar is
// InputFragment there, however long.
//
// Bracketed pastes are tracked: their text, end marker included, is
// InputPaste, whatever it holds. ESC [200~ starts one only while PasteOn
// reports bracketed paste on (a window without it sends pastes bare, and a
// pasted ESC [200~ is text); a paste is over at its end marker or after
// pasteLimit bytes.
//
// When the rest of a held sequence does not come within InputHold, the
// caller calls Release: the held bytes are given as InputFragment, and so is
// the rest of that sequence, at once as it comes, up to the longest answer's
// length. Nothing the reader gives up on counts as a key.
// The zero value is ready. It is not safe for concurrent use.
type InputReader struct {
	// PasteOn reports whether the window has bracketed paste on
	// (termfilter.Filter.BracketedPaste); nil: always.
	PasteOn func() bool

	held   []byte // the start of a unit cut at the end of the last read
	given  int    // of held, the bytes already given (Release)
	long   bool   // held is a sequence given up on for its length
	paste  bool   // inside a bracketed paste
	pasted int    // bytes of the open paste given
}

// Read calls each, in order, for every unit of data after what was held,
// and holds a unit cut at its end. The units' bytes are data's and the held
// ones, never more: bytes given are bytes read.
func (r *InputReader) Read(data []byte, each func(unit []byte, kind InputKind, reply Reply)) {
	skip, long := 0, r.long
	r.long = false
	if len(r.held) == 1 && r.given == 1 && !r.paste && len(data) > 0 && !answerIntroducer(data[0]) {
		// An Esc given up on and then something no answer goes on with:
		// the Esc key, and what follows starts afresh.
		r.held, r.given = nil, 0
	}
	if len(r.held) > 0 {
		data = append(r.held, data...)
		skip = r.given
		r.held, r.given = nil, 0
	}
	// emit gives data[from:to] but for what was given already.
	emit := func(from, to int, kind InputKind, reply Reply) {
		if to <= skip || from >= to {
			return
		}
		if from < skip {
			from = skip
			if kind != InputPaste {
				kind, reply = InputFragment, ReplyNone
			}
		}
		each(data[from:to], kind, reply)
	}
	for i := 0; i < len(data); {
		if r.paste {
			// To the end marker, or else to what may be its start.
			stop, ended := len(data)-markerStart(data[i:], pasteEnd), false
			if end := bytes.Index(data[i:], []byte(pasteEnd)); end >= 0 {
				stop, ended = i+end+len(pasteEnd), true
			}
			from := max(i, skip) // the bytes not given yet
			if room := pasteLimit - r.pasted; stop-from > room {
				// Longer than any real paste: over here.
				emit(i, from+room, InputPaste, ReplyNone)
				i = from + room
				r.paste, r.pasted = false, 0
				continue
			}
			emit(i, stop, InputPaste, ReplyNone)
			r.pasted += max(stop-from, 0)
			if !ended {
				r.hold(data[stop:], skip-stop)
				return
			}
			i, r.paste, r.pasted = stop, false, 0
			continue
		}
		if data[i] == 0x1b {
			n, done := seqEnd(data[i:])
			switch {
			case done && skip > i:
				// The rest of a sequence given up on: a fragment, to its
				// end, whatever NextInput would make of it. A paste start
				// is still one.
				if string(data[i:i+n]) == pasteStart && r.pasteOn() {
					r.paste, r.pasted = true, 0
				}
				emit(i, i+n, InputFragment, ReplyNone)
				i += n
				continue
			case done:
			case n >= replyLimit:
				// Longer than any answer: given up on at once.
				emit(i, len(data), InputFragment, ReplyNone)
				if long || skip <= i {
					// So is the rest of it, to its end; only what tells
					// where that is stays.
					r.long = true
					seq := compact(data[i:])
					r.hold(seq, len(seq))
				}
				// A start given up on for want of its rest (Esc, a pause,
				// then ] say) is not held past the longest answer: what
				// follows is read afresh.
				return
			case skip > i:
				// Part of it was given up on already: so is the rest,
				// as it comes.
				emit(i, len(data), InputFragment, ReplyNone)
				r.long = long
				r.hold(data[i:], len(data)-i)
				return
			default:
				r.hold(data[i:], 0)
				return
			}
		}
		n, kind, reply := NextInput(data[i:])
		if string(data[i:i+n]) == pasteStart && r.pasteOn() {
			r.paste, r.pasted = true, 0
		}
		emit(i, i+n, kind, reply)
		i += n
	}
}

// hold keeps a copy of tail, of which given bytes were given already.
func (r *InputReader) hold(tail []byte, given int) {
	if len(tail) == 0 {
		return
	}
	r.held = append([]byte(nil), tail...)
	r.given = min(max(given, 0), len(tail))
}

// Held reports whether part of a unit waits for its rest: the caller calls
// Release if no more input comes within InputHold.
func (r *InputReader) Held() bool { return len(r.held) > r.given }

// Release gives what is held and not yet given: InputFragment, or
// InputPaste inside a paste. The rest of that unit, when it comes, is
// InputFragment too (InputPaste inside a paste), but for a lone Esc
// followed by what no answer goes on with: that was the Esc key.
func (r *InputReader) Release(each func(unit []byte, kind InputKind, reply Reply)) {
	if !r.Held() {
		return
	}
	kind := InputFragment
	if r.paste {
		kind = InputPaste
	}
	each(r.held[r.given:], kind, ReplyNone)
	if r.paste {
		r.pasted += len(r.held) - r.given
	}
	r.given = len(r.held)
}

// InPaste reports whether a bracketed paste is open.
func (r *InputReader) InPaste() bool { return r.paste }

func (r *InputReader) pasteOn() bool { return r.PasteOn == nil || r.PasteOn() }

// seqEnd measures the escape sequence at the start of data by its structure
// alone, and reports false when data ends before it does.
func seqEnd(data []byte) (int, bool) {
	if len(data) < 2 {
		return len(data), false
	}
	switch data[1] {
	case '[':
		i := 2
		for i < len(data) && data[i] >= 0x20 && data[i] <= 0x3f {
			i++
		}
		switch {
		case i == len(data):
			return i, false
		case data[i] < 0x40 || data[i] > 0x7e:
			return i, true // broken off: the program reads it as typed
		case data[i] == 'M' && i == 2: // X10 mouse: three bytes follow
			return min(len(data), 6), len(data) >= 6
		}
		return i + 1, true
	case ']', 'P':
		for i := 2; i < len(data); i++ {
			switch c := data[i]; {
			case c == 0x07 && data[1] == ']':
				return i + 1, true
			case c == 0x1b:
				if i+1 == len(data) {
					return len(data), false
				}
				if data[i+1] == '\\' {
					return i + 2, true
				}
				return i, true
			case c < 0x20 || c >= 0x7f:
				return i, true
			}
		}
		return len(data), false
	case 'O':
		return min(len(data), 3), len(data) >= 3
	}
	return 2, true
}

// answerIntroducer reports whether c after an Esc can begin an answer to a
// query: CSI, OSC or DCS.
func answerIntroducer(c byte) bool { return c == '[' || c == ']' || c == 'P' }

// compact keeps of an unfinished sequence given up on for its length only
// what tells where it ends: its introducer, and for a string an Esc at its
// end.
func compact(seq []byte) []byte {
	out := append([]byte(nil), seq[:2]...)
	switch {
	case seq[1] == '[':
		out = append(out, '0') // past the X10 mouse form
	case seq[len(seq)-1] == 0x1b:
		out = append(out, 0x1b)
	}
	return out
}

// markerStart is the length of the longest end of data that begins marker.
func markerStart(data []byte, marker string) int {
	for n := min(len(data), len(marker)-1); n > 0; n-- {
		if string(data[len(data)-n:]) == marker[:n] {
			return n
		}
	}
	return 0
}
