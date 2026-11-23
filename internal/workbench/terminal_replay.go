package workbench

import (
	"strconv"
	"strings"
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

// The states of xterm.js's parser (EscapeSequenceParser, the VT500 table).
const (
	parseGround terminalParse = iota
	parseEscape
	parseEscapeIntermediate
	parseCSIEntry
	parseCSIParam
	parseCSIIntermediate
	parseCSIIgnore
	parseOSC
	parseDCSEntry
	parseDCSParam
	parseDCSIntermediate
	parseDCSIgnore
	parseDCSPassthrough
	parseIgnoredString // SOS, PM, APC
)

// terminalModeParams is how many parameters of one sequence xterm.js keeps
// (Params.maxLength); the digits of later ones are dropped.
const terminalModeParams = 32

// terminalParamMax is the largest parameter value xterm.js stores.
const terminalParamMax = 0x7fffffff

type terminalModes struct {
	parse   terminalParse
	need    uint8  // UTF-8 continuation bytes still owed
	lead    uint8  // bytes of the character being decoded (2, 3 or 4)
	code    uint32 // the code point they complete
	collect uint32 // prefix and intermediates, collected as xterm.js does
	params  [terminalModeParams]uint32
	count   uint8 // parameters kept, at most terminalModeParams
	reject  bool  // more parameters than kept: their digits are dropped
	sub     bool  // digits go to a sub-parameter

	alt, cursorKeys, keypad, noWrap, hidden, focus, paste, lineDrawing bool
	mouse, encoding                                                    uint16
	cursorStyle                                                        uint8
}

// feed reads output bytes in order.
func (m *terminalModes) feed(data []byte) {
	for _, b := range data {
		m.byte(b)
	}
}

// ground reports a boundary between sequences and characters: a replay may
// start after it.
func (m *terminalModes) ground() bool { return m.parse == parseGround && m.need == 0 }

// finish reads the start of data until the sequence or character cut in half
// at its front is complete, and returns how many bytes that took; the modes
// that sequence sets count. A viewer is shown data from there. When the cut
// sequence does not end within data (a string left open, then plain output),
// ok is false and m is unchanged: the viewer is shown all of data, as
// before this tracker, rather than nothing.
func (m *terminalModes) finish(data []byte) (n int, ok bool) {
	probe := *m
	for n < len(data) && !probe.ground() {
		probe.byte(data[n])
		n++
	}
	if !probe.ground() {
		return 0, false
	}
	*m = probe
	return n, true
}

// reset is a full reset (RIS); the parser state is kept.
func (m *terminalModes) reset() {
	*m = terminalModes{parse: m.parse, need: m.need, lead: m.lead, code: m.code, collect: m.collect, params: m.params, count: m.count, reject: m.reject, sub: m.sub}
}

// byte decodes UTF-8 as xterm.js does (Utf8ToUtf32): a broken character is
// dropped and the byte that broke it read again; C1 controls arrive as
// U+0080..U+009F.
func (m *terminalModes) byte(b byte) {
	if m.need > 0 {
		if b&0xc0 == 0x80 {
			m.code = m.code<<6 | uint32(b&0x3f)
			if m.need--; m.need == 0 {
				switch c := m.code; {
				case m.lead == 2 && c >= 0x80,
					m.lead == 3 && c >= 0x800 && (c < 0xd800 || c > 0xdfff) && c != 0xfeff,
					m.lead == 4 && c >= 0x10000 && c <= 0x10ffff:
					m.step(c)
				}
			}
			return
		}
		m.need = 0
	}
	switch {
	case b < 0x80:
		m.step(uint32(b))
	case b&0xe0 == 0xc0:
		m.need, m.lead, m.code = 1, 2, uint32(b&0x1f)
	case b&0xf0 == 0xe0:
		m.need, m.lead, m.code = 2, 3, uint32(b&0x0f)
	case b&0xf8 == 0xf0:
		m.need, m.lead, m.code = 3, 4, uint32(b&0x07)
	}
}

// clear starts a new sequence: one parameter, 0, nothing collected.
func (m *terminalModes) clear() {
	m.collect, m.params[0], m.count, m.reject, m.sub = 0, 0, 1, false, false
}

// step reads one code point, with the transitions of xterm.js.
func (m *terminalModes) step(c uint32) {
	// Anywhere: CAN and SUB end what is open, ESC starts a sequence (and
	// ends a string), C1 controls act as their ESC forms.
	switch {
	case c == 0x18 || c == 0x1a:
		m.parse = parseGround
		return
	case c == 0x1b:
		m.parse = parseEscape
		m.clear()
		return
	case c >= 0x80 && c < 0xa0:
		switch c {
		case 0x90:
			m.parse = parseDCSEntry
			m.clear()
		case 0x9b:
			m.parse = parseCSIEntry
			m.clear()
		case 0x9d:
			m.parse = parseOSC
		case 0x98, 0x9e, 0x9f:
			m.parse = parseIgnoredString
		default: // ST and the C1 controls that execute
			m.parse = parseGround
		}
		return
	}
	// Code points past C1 are printable; a sequence that is not a string
	// ends on one (xterm.js's error transition).
	wide := c >= 0xa0
	control := c < 0x20
	switch m.parse {
	case parseGround:
	case parseEscape:
		switch {
		case control || c == 0x7f:
		case wide:
			m.parse = parseGround
		case c < 0x30:
			m.collect = m.collect<<8 | c
			m.parse = parseEscapeIntermediate
		case c == '[':
			m.parse = parseCSIEntry
			m.clear()
		case c == ']':
			m.parse = parseOSC
		case c == 'P':
			m.parse = parseDCSEntry
			m.clear()
		case c == 'X' || c == '^' || c == '_':
			m.parse = parseIgnoredString
		default:
			m.escape(c)
			m.parse = parseGround
		}
	case parseEscapeIntermediate:
		switch {
		case control || c == 0x7f:
		case wide:
			m.parse = parseGround
		case c < 0x30:
			m.collect = m.collect<<8 | c
		default:
			m.escape(c)
			m.parse = parseGround
		}
	case parseCSIEntry, parseCSIParam, parseCSIIntermediate:
		switch {
		case control || c == 0x7f:
		case wide:
			m.parse = parseGround
		case c >= 0x40:
			m.csi(c)
			m.parse = parseGround
		case c < 0x30:
			m.collect = m.collect<<8 | c
			m.parse = parseCSIIntermediate
		case m.parse == parseCSIIntermediate:
			m.parse = parseCSIIgnore
		case c >= 0x3c: // a prefix: first, or ignored
			if m.parse == parseCSIEntry {
				m.collect = m.collect<<8 | c
				m.parse = parseCSIParam
			} else {
				m.parse = parseCSIIgnore
			}
		default:
			m.param(c)
			m.parse = parseCSIParam
		}
	case parseCSIIgnore:
		if c >= 0x40 && c < 0x7f {
			m.parse = parseGround
		}
	case parseOSC:
		if c == 0x07 {
			m.parse = parseGround
		}
	case parseDCSEntry, parseDCSParam, parseDCSIntermediate:
		switch {
		case control || c == 0x7f:
		case wide:
			m.parse = parseGround
		case c >= 0x40:
			m.parse = parseDCSPassthrough
		case c < 0x30:
			m.parse = parseDCSIntermediate
		case m.parse == parseDCSIntermediate || c >= 0x3c && m.parse == parseDCSParam:
			m.parse = parseDCSIgnore
		default:
			m.parse = parseDCSParam
		}
	case parseDCSIgnore, parseDCSPassthrough:
		// Only ESC, ST, CAN and SUB end them, above.
	case parseIgnoredString:
		if wide {
			m.parse = parseGround
		}
	}
}

// param reads a digit, ";" or ":" of a CSI sequence, as xterm.js's Params.
func (m *terminalModes) param(c uint32) {
	switch c {
	case ';':
		m.sub = false
		if m.count >= terminalModeParams {
			m.reject = true
			return
		}
		m.params[m.count] = 0
		m.count++
	case ':':
		m.sub = true
	default:
		if m.reject || m.sub || m.count == 0 {
			return
		}
		v := &m.params[m.count-1]
		*v = uint32(min(uint64(*v)*10+uint64(c-'0'), terminalParamMax))
	}
}

// terminalCharsets are the character sets xterm.js designates (CHARSETS).
const terminalCharsets = "0AB4C5RQKYE6ZH7="

// escape dispatches ESC sequences that set modes.
func (m *terminalModes) escape(final uint32) {
	switch m.collect {
	case 0:
		switch final {
		case '=':
			m.keypad = true
		case '>':
			m.keypad = false
		case 'c': // RIS
			m.reset()
		}
	case '(':
		if strings.ContainsRune(terminalCharsets, rune(final)) {
			m.lineDrawing = final == '0'
		}
	case '%':
		if final == '@' || final == 'G' { // the default character set
			m.lineDrawing = false
		}
	}
}

// csi dispatches CSI sequences that set modes.
func (m *terminalModes) csi(final uint32) {
	switch {
	case m.collect == '?' && (final == 'h' || final == 'l'):
		for _, p := range m.params[:m.count] {
			m.set(p, final == 'h')
		}
	case m.collect == ' ' && final == 'q': // DECSCUSR
		if p := m.params[0]; p <= 6 {
			m.cursorStyle = uint8(p)
		}
	case m.collect == '!' && final == 'p': // DECSTR: xterm.js resets its DEC private modes
		m.cursorKeys, m.keypad, m.noWrap, m.hidden, m.focus, m.paste, m.lineDrawing, m.cursorStyle = false, false, false, false, false, false, false, 0
	}
}

// set applies one DEC private mode, the way xterm.js does.
func (m *terminalModes) set(mode uint32, on bool) {
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
			m.mouse = uint16(mode)
		}
	case 1006, 1016: // xterm.js has no 1005 and 1015
		m.encoding = 0
		if on {
			m.encoding = uint16(mode)
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
		// A string left open past the history (ok false) starts the replay
		// at the oldest kept byte, as before: never nothing.
		skip, _ = modes.finish(t.history)
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

// markShown records that output up to next was written to a viewer that
// answers its queries: a socket write that succeeded, or a phone that asks
// for what follows it.
func (t *terminalSession) markShown(next uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if next <= t.written {
		t.shown = max(t.shown, next)
	}
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
