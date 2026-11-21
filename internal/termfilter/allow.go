package termfilter

import "strconv"

// The allow-lists. A row is here because a real program sends it (the corpus
// in testdata: vim, tmux, less, top, and stand-ins for Claude Code and the
// Codex TUI) and Cage's web terminal, xterm.js 6, acts on it: what does not
// work in a Cage terminal in the browser is not needed in a local one.
// Queries pass only where the server's viewer rules gate their replies
// (splitTerminalReplies in internal/workbench), and never in replayed
// history. DA2 and XTVERSION are dropped although their replies are gated:
// they name the owner's emulator and its version.

// privateModes are the DEC private modes (CSI ? Pm h/l) that pass: what
// programs set that xterm.js's setModePrivate also handles. Cursor keys (1),
// wrap (7), cursor blink (12) and visibility (25), the mouse (1000, 1002,
// 1003 with the 1005, 1006 and 1015 encodings), the alternate screen (1049,
// and the 47, 1047 and 1048 forms other terminfo entries use), bracketed
// paste (2004) and synchronized output (2026). Origin mode (6) is dropped:
// no program here uses it, and setting or resetting it moves the cursor, so
// the viewer could not reset it without moving the owner's cursor. Mode 1004
// (focus reports) is the viewer's own and never passes; see Filter.GuestFocus.
var privateModes = map[int]bool{
	1: true, 7: true, 12: true, 25: true, 47: true,
	1000: true, 1002: true, 1003: true, 1005: true, 1006: true, 1015: true,
	1047: true, 1048: true, 1049: true, 2004: true, 2026: true,
}

// ansiModes are the ANSI modes (CSI Pm h/l): insert and new line.
var ansiModes = map[int]bool{4: true, 20: true}

// plainCSI are the final bytes of control sequences without a private marker
// or intermediate that pass with any numeric parameters: cursor movement,
// erasing, inserting, scrolling, tabs and character attributes.
var plainCSI = [128]bool{
	'@': true, 'A': true, 'B': true, 'C': true, 'D': true, 'E': true, 'F': true, 'G': true,
	'H': true, 'I': true, 'J': true, 'K': true, 'L': true, 'M': true, 'P': true, 'S': true,
	'X': true, 'Z': true, '`': true, 'a': true, 'b': true, 'd': true, 'e': true, 'f': true,
	'g': true, 'm': true, 'r': true,
}

// allowEscape decides an escape sequence other than CSI and strings from its
// intermediates and final byte: cursor save and restore, index, next line,
// reverse index, tab set, keypad modes, full reset, and the US, UK and line
// drawing sets for G0 and G1 (which SO and SI switch between). Leaving UTF-8
// (ESC % @), 8-bit controls (ESC SP G), DECID (ESC Z), locking shifts into G2
// and G3 (which would outlive the attach), single shifts, a stray ST and the
// rest are dropped.
func allowEscape(seq []byte) bool {
	if len(seq) == 1 {
		switch seq[0] {
		case '7', '8', 'D', 'E', 'H', 'M', '=', '>', 'c':
			return true
		}
		return false
	}
	if len(seq) == 2 && (seq[0] == '(' || seq[0] == ')') {
		switch seq[1] {
		case 'B', '0', 'A':
			return true
		}
	}
	return false
}

// allowCSI decides a complete control sequence from its parameter and
// intermediate bytes and its final byte. It may rewrite the held sequence,
// and empties it (f.n == 0) for one it takes out without counting a drop.
// Window operations (CSI Ps t) are all dropped: Cage's web terminal sets no
// window options, so xterm.js ignores even the title stack, and the viewer
// pushes and pops the title itself.
func (f *Filter) allowCSI(body []byte, final byte) bool {
	var marker byte
	if len(body) > 0 && body[0] >= '<' && body[0] <= '?' {
		marker, body = body[0], body[1:]
	}
	params, inter := body, []byte(nil)
	for i, c := range body {
		if c < 0x30 {
			params, inter = body[:i], body[i:]
			break
		}
	}
	for _, c := range params {
		if c == ':' && final == 'm' && marker == 0 {
			continue // SGR sub-parameters
		}
		if (c < '0' || c > '9') && c != ';' {
			return false
		}
	}
	if len(inter) > 1 {
		return false
	}
	var in byte
	if len(inter) == 1 {
		in = inter[0]
	}
	switch {
	case marker == 0 && in == 0:
		switch final {
		case 'h', 'l':
			return everyParam(params, func(n int) bool { return ansiModes[n] })
		case 'c': // DA1
			return !f.replay && (len(params) == 0 || string(params) == "0")
		case 'n': // status and cursor position reports
			return !f.replay && (string(params) == "5" || string(params) == "6")
		case 's', 'u': // save and restore the cursor
			return len(params) == 0
		case 'T': // scroll down; five parameters start mouse highlight tracking
			return count(params, ';') == 0
		}
		return final < 128 && plainCSI[final]
	case marker == '?' && in == 0:
		switch final {
		case 'h', 'l':
			return f.privateModes(params, final)
		case 'J', 'K': // selective erase
			return true
		case 'n': // cursor position report
			return !f.replay && string(params) == "6"
		}
	case marker == 0 && in == ' ':
		switch final {
		case 'q': // cursor style
			n := firstParam(params)
			return n >= 0 && n <= 6 && count(params, ';') == 0
		case '@', 'A': // scroll left and right
			return true
		}
	case marker == 0 && in == '!' && final == 'p': // soft reset
		if len(params) == 0 {
			f.resets++
			return true
		}
	case marker == 0 && in == '"' && final == 'q': // character protection
		return count(params, ';') == 0
	case marker == 0 && in == '\'' && (final == '}' || final == '~'): // insert and delete columns
		return true
	case (marker == 0 || marker == '?') && in == '$' && final == 'p': // DECRQM
		return !f.replay && count(params, ';') == 0 && len(params) > 0
	}
	return false
}

// privateModes passes DECSET and DECRST for the allowed modes. Mode 1004 is
// taken out and remembered; a sequence with any other mode is dropped.
func (f *Filter) privateModes(params []byte, final byte) bool {
	if len(params) == 0 {
		return false
	}
	focus := false
	ok := everyParam(params, func(n int) bool {
		if n == 1004 {
			focus = true
			return true
		}
		return privateModes[n]
	})
	if !ok {
		return false
	}
	if !focus {
		return true
	}
	f.focus.Store(final == 'h')
	// Rebuild "ESC [ ? modes final" without 1004.
	kept := make([]byte, 0, len(params))
	for _, field := range splitParams(params) {
		if string(trimZeros(field)) == "1004" {
			continue
		}
		if len(kept) > 0 {
			kept = append(kept, ';')
		}
		kept = append(kept, field...)
	}
	if len(kept) == 0 {
		f.n = 0
		return false
	}
	f.n = 0
	f.push(0x1b)
	f.push('[')
	f.push('?')
	for _, c := range kept {
		f.push(c)
	}
	f.push(final)
	return true
}

// everyParam reports whether params is a non-empty list of numbers that all
// satisfy ok.
func everyParam(params []byte, ok func(int) bool) bool {
	if len(params) == 0 {
		return false
	}
	for _, field := range splitParams(params) {
		if len(field) == 0 || len(field) > 5 {
			return false
		}
		n, _ := strconv.Atoi(string(field))
		if !ok(n) {
			return false
		}
	}
	return true
}

func splitParams(params []byte) [][]byte {
	var fields [][]byte
	start := 0
	for i, c := range params {
		if c == ';' {
			fields = append(fields, params[start:i])
			start = i + 1
		}
	}
	return append(fields, params[start:])
}

func trimZeros(field []byte) []byte {
	for len(field) > 1 && field[0] == '0' {
		field = field[1:]
	}
	return field
}

func count(text []byte, c byte) int {
	n := 0
	for _, b := range text {
		if b == c {
			n++
		}
	}
	return n
}

func firstParam(params []byte) int {
	n := 0
	for i, c := range params {
		if c < '0' || c > '9' {
			if i == 0 {
				return 0
			}
			break
		}
		n = n*10 + int(c-'0')
		if n > 1<<16 {
			return -1
		}
	}
	return n
}

// csiName names a dropped control sequence for the counts, as it was sent
// when its parameters are short.
func csiName(body []byte, final byte) string {
	if len(body) > 16 {
		body = append(append(body[:1:1], "…"...), body[len(body)-1])
	}
	return "CSI " + string(body) + string(final)
}
