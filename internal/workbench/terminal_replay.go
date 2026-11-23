package workbench

import (
	"strconv"
	"time"
)

// A terminal keeps only the last terminalHistoryLimit bytes of its output. A
// viewer that is shown that tail without the bytes before it (a new page, the
// phone, a view whose offset is no longer kept) would start in the middle of
// an escape sequence and without the modes the program set at its start: a
// full-screen program would draw into the normal screen, its mouse wheel and
// paste would not reach it. terminalModes follows the output as it leaves the
// kept history, so the server can tell such a viewer what to set first.
//
// It reads only what xterm.js acts on and a replay must restore: the screen
// buffer, cursor keys and keypad, autowrap, cursor visibility and style,
// mouse tracking and its encoding, focus reports, bracketed paste and the
// line-drawing character set. Everything else the kept tail redraws itself.

type terminalParse uint8

const (
	parseGround terminalParse = iota
	parseEscape
	parseCharset // ESC ( ) * + and the designation still to come
	parseCSI
	parseString // OSC, DCS, APC, PM, SOS until BEL or ST
	parseStringEscape
)

// terminalModeParams bounds the parameters of one control sequence; more are
// ignored, as programs never set that many modes at once.
const terminalModeParams = 16

type terminalModes struct {
	parse   terminalParse
	utf8    uint8 // continuation bytes still owed by a multi-byte character
	g0      bool  // the charset being designated is G0
	private bool  // CSI with the ? prefix
	other   bool  // CSI with another prefix (<, =, >): not a mode
	space   bool  // CSI with a space intermediate (DECSCUSR)
	bang    bool  // CSI with a ! intermediate (DECSTR)
	inter   bool  // CSI with any other intermediate
	params  [terminalModeParams]uint16
	count   uint8 // parameters started
	value   uint32

	alt, cursorKeys, keypad, noWrap, hidden, focus, paste, lineDrawing bool
	mouse, encoding                                                    uint16
	cursorStyle                                                        uint8
}

// feed reads output bytes in order.
func (m *terminalModes) feed(data []byte) {
	for _, b := range data {
		m.step(b)
	}
}

// ground reports a boundary between sequences and characters: a replay may
// start after it.
func (m *terminalModes) ground() bool { return m.parse == parseGround && m.utf8 == 0 }

// finish reads the start of data until the sequence or character cut in half
// at its front is complete, and returns how many bytes that took. The modes
// that sequence sets count. A viewer is shown data from there.
func (m *terminalModes) finish(data []byte) int {
	n := 0
	for n < len(data) && !m.ground() {
		m.step(data[n])
		n++
	}
	return n
}

func (m *terminalModes) reset() {
	*m = terminalModes{parse: m.parse, utf8: m.utf8}
}

func (m *terminalModes) step(b byte) {
	switch b {
	case 0x18, 0x1a: // CAN, SUB end any sequence
		m.parse, m.utf8 = parseGround, 0
		return
	}
	switch m.parse {
	case parseGround:
		switch {
		case m.utf8 > 0 && b&0xc0 == 0x80:
			m.utf8--
		case b == 0x1b:
			m.parse, m.utf8 = parseEscape, 0
		case b >= 0xf0 && b < 0xf8:
			m.utf8 = 3
		case b >= 0xe0 && b < 0xf0:
			m.utf8 = 2
		case b >= 0xc0 && b < 0xe0:
			m.utf8 = 1
		default:
			m.utf8 = 0
		}
	case parseEscape:
		m.parse = parseGround
		switch b {
		case '[':
			m.parse, m.private, m.other, m.space, m.bang, m.inter, m.count, m.value = parseCSI, false, false, false, false, false, 0, 0
		case ']', 'P', '_', '^', 'X':
			m.parse = parseString
		case '(', ')', '*', '+':
			m.parse, m.g0 = parseCharset, b == '('
		case 'c': // RIS
			m.reset()
		case '=':
			m.keypad = true
		case '>':
			m.keypad = false
		case 0x1b:
			m.parse = parseEscape
		default:
			if b >= 0x20 && b <= 0x2f {
				// ESC with an intermediate: its final byte ends it.
				m.parse = parseCharset
				m.g0 = false
			}
		}
	case parseCharset:
		if b >= 0x20 && b <= 0x2f {
			return
		}
		if m.g0 {
			m.lineDrawing = b == '0'
		}
		m.parse = parseGround
		if b == 0x1b {
			m.parse = parseEscape
		}
	case parseCSI:
		switch {
		case b >= '0' && b <= '9':
			if m.count == 0 {
				m.count = 1
			}
			if m.value < 1<<16 {
				m.value = m.value*10 + uint32(b-'0')
			}
		case b == ';' || b == ':':
			m.param()
			m.count++
		case b >= '<' && b <= '?':
			if b == '?' {
				m.private = true
			} else {
				m.other = true
			}
		case b >= 0x20 && b <= 0x2f:
			switch b {
			case ' ':
				m.space = true
			case '!':
				m.bang = true
			default:
				m.inter = true
			}
		case b >= 0x40 && b <= 0x7e:
			m.param()
			m.dispatch(b)
			m.parse = parseGround
		case b == 0x1b:
			m.parse = parseEscape
		}
		// Other C0 controls inside a sequence act and leave it open.
	case parseString:
		switch b {
		case 0x07:
			m.parse = parseGround
		case 0x1b:
			m.parse = parseStringEscape
		}
	case parseStringEscape:
		if b == '\\' {
			m.parse = parseGround
			return
		}
		// Another sequence ends the string and starts itself.
		m.parse = parseEscape
		m.step(b)
	}
}

// param stores the parameter being read, if there is room.
func (m *terminalModes) param() {
	if m.count > 0 && int(m.count) <= terminalModeParams {
		m.params[m.count-1] = uint16(min(m.value, 1<<16-1))
	}
	m.value = 0
}

func (m *terminalModes) dispatch(final byte) {
	n := min(int(m.count), terminalModeParams)
	switch {
	case m.bang && final == 'p' && !m.private && !m.other: // DECSTR
		m.cursorKeys, m.keypad, m.noWrap, m.hidden, m.lineDrawing = false, false, false, false, false
	case m.space && final == 'q' && !m.private && !m.other && !m.inter: // DECSCUSR
		m.cursorStyle = 0
		if n > 0 {
			m.cursorStyle = uint8(min(m.params[0], 6))
		}
	case m.private && !m.other && !m.space && !m.bang && !m.inter && (final == 'h' || final == 'l'):
		for _, p := range m.params[:n] {
			m.set(p, final == 'h')
		}
	}
}

// set applies one DEC private mode, the way xterm.js does.
func (m *terminalModes) set(mode uint16, on bool) {
	switch mode {
	case 1:
		m.cursorKeys = on
	case 7:
		m.noWrap = !on
	case 25:
		m.hidden = !on
	case 47, 1047, 1049:
		m.alt = on
	case 66:
		m.keypad = on
	case 9, 1000, 1002, 1003:
		m.mouse = 0
		if on {
			m.mouse = mode
		}
	case 1005, 1006, 1015, 1016:
		m.encoding = 0
		if on {
			m.encoding = mode
		}
	case 1004:
		m.focus = on
	case 2004:
		m.paste = on
	}
}

// preamble is what a viewer writes after clearing its screen and before the
// kept output, so that output lands in the screen and modes it was written
// for. Empty when everything is at its default.
func (m *terminalModes) preamble() []byte {
	var out []byte
	decset := func(mode uint16) {
		out = append(out, "\x1b[?"...)
		out = strconv.AppendUint(out, uint64(mode), 10)
		out = append(out, 'h')
	}
	if m.alt {
		decset(1049)
	}
	if m.cursorKeys {
		decset(1)
	}
	if m.keypad {
		out = append(out, "\x1b="...)
	}
	if m.noWrap {
		out = append(out, "\x1b[?7l"...)
	}
	if m.hidden {
		out = append(out, "\x1b[?25l"...)
	}
	if m.cursorStyle != 0 {
		out = append(out, "\x1b["...)
		out = strconv.AppendUint(out, uint64(m.cursorStyle), 10)
		out = append(out, " q"...)
	}
	if m.mouse != 0 {
		decset(m.mouse)
	}
	if m.encoding != 0 {
		decset(m.encoding)
	}
	if m.focus {
		decset(1004)
	}
	if m.paste {
		decset(2004)
	}
	if m.lineDrawing {
		out = append(out, "\x1b(0"...)
	}
	return out
}

// terminalReplay is the kept output a viewer is shown from its oldest byte.
type terminalReplay struct {
	data   []byte // starts at a sequence boundary
	offset uint64 // absolute offset of data[0]
	state  []byte // modes to set before data; empty when the history was never trimmed
	alt    bool   // after data, the program is in the alternate screen
}

// repaint reports a trimmed replay that leaves the viewer in a full-screen
// program, whose screen it shows only in part.
func (r terminalReplay) repaint() bool { return len(r.state) > 0 && r.alt }

// replayFromOldestLocked is the kept history for a viewer that cannot
// continue what it shows. Caller holds t.mu.
func (t *terminalSession) replayFromOldestLocked(limit int) terminalReplay {
	oldest := t.oldestLocked()
	modes := t.trimmed
	skip := 0
	if oldest > 0 {
		skip = modes.finish(t.history)
	}
	end := min(len(t.history), skip+limit)
	replay := terminalReplay{data: append(make([]byte, 0, end-skip), t.history[skip:end]...), offset: oldest + uint64(skip)}
	if oldest > 0 {
		replay.state = modes.preamble()
		after := modes
		after.feed(t.history[skip:])
		replay.alt = after.alt
	}
	return replay
}

// terminalRepaintGap is how long a full-screen program has to draw itself at
// one row less before it is given its size back.
var terminalRepaintGap = 120 * time.Millisecond

// markShown records that output up to next reached a viewer.
func (t *terminalSession) markShown(next uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.shown = max(t.shown, next)
}

// repaint makes a full-screen program draw its whole screen again, for a
// viewer that was shown a trimmed history: cells drawn before the kept tail
// are otherwise missing from its screen until the program redraws them. The
// program is given one row less, then the size the terminal has by then.
// Ownership and the recorded size do not change.
func (t *terminalSession) repaint() {
	t.sizeMu.Lock()
	t.mu.Lock()
	cols, rows, closed := t.sizing.cols, t.sizing.rows, t.closed
	// Viewers that attach together share one repaint.
	recent := time.Since(t.repaintAt) < 2*terminalRepaintGap
	if !recent {
		t.repaintAt = time.Now()
	}
	t.mu.Unlock()
	if closed || recent || cols == 0 || rows < 3 {
		t.sizeMu.Unlock()
		return
	}
	t.resize(cols, rows-1)
	t.sizeMu.Unlock()
	time.AfterFunc(terminalRepaintGap, func() {
		t.sizeMu.Lock()
		defer t.sizeMu.Unlock()
		t.mu.Lock()
		cols, rows, closed := t.sizing.cols, t.sizing.rows, t.closed
		t.mu.Unlock()
		if !closed && cols != 0 {
			t.resize(cols, rows)
		}
	})
}
