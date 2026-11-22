package termfilter

import "math"

// The other direction: what a terminal application sends back. Besides the
// owner's keys and pastes it sends mouse and focus reports, and answers to
// the queries the program wrote. The viewer and the server both need to
// tell them apart: an answer must not count as the owner being at the
// window, and must not reach the program from a viewer that does not own
// the size. This grammar is strict: a sequence counts as an answer only
// when it is complete and well formed, so Alt+P, Alt+], Alt+_ and an Esc
// typed just before ] stay keys.

// InputKind is what one unit of input is.
type InputKind uint8

const (
	InputKey InputKind = iota // keys and pastes, and anything not recognised
	InputMouse
	InputFocusIn
	InputFocusOut
	InputReply
	// InputMotion is a mouse report of the pointer moving with no button
	// pressed: the owner did not act, the pointer only passed over.
	InputMotion
)

// Reply names the query an answer is for.
type Reply uint8

const (
	ReplyNone    Reply = iota
	ReplyDA1           // CSI ? Ps ; ... c, to CSI c
	ReplyDA2           // CSI > Ps ; Ps ; Ps c, to CSI > c
	ReplyStatus        // CSI 0 n, to CSI 5 n
	ReplyCursor        // CSI Pr ; Pc R or CSI ? Pr ; Pc [; Pp] R, to CSI 6 n or CSI ? 6 n
	ReplyMode          // CSI [?] Ps ; Pm $ y, to CSI [?] Ps $ p
	ReplySetting       // DCS 0|1 $ r ... ST, to DCS $ q ... ST
	ReplyColor         // OSC 4;n;rgb:... or OSC 10|11|12;rgb:..., to the same with ?
)

// replyLimit bounds an answer; anything longer is typing.
const replyLimit = 128

// NextInput measures the unit of input at the start of data and says what
// it is. n is at least 1 when data is not empty.
func NextInput(data []byte) (n int, kind InputKind, reply Reply) {
	if len(data) == 0 {
		return 0, InputKey, ReplyNone
	}
	if data[0] != 0x1b {
		i := 1
		for i < len(data) && data[i] != 0x1b {
			i++
		}
		return i, InputKey, ReplyNone
	}
	if len(data) == 1 {
		return 1, InputKey, ReplyNone
	}
	switch data[1] {
	case '[':
		return csiInput(data)
	case ']':
		if n, ok := colorReply(data); ok {
			return n, InputReply, ReplyColor
		}
	case 'P':
		if n, ok := settingReply(data); ok {
			return n, InputReply, ReplySetting
		}
	case 'O':
		if len(data) >= 3 {
			return 3, InputKey, ReplyNone // SS3: arrows, F1-F4 in application mode
		}
	}
	return 2, InputKey, ReplyNone // Alt with a key, or Esc and a key
}

func csiInput(data []byte) (int, InputKind, Reply) {
	i := 2
	for i < len(data) && data[i] >= 0x30 && data[i] <= 0x3f {
		i++
	}
	params := data[2:i]
	for i < len(data) && data[i] >= 0x20 && data[i] <= 0x2f {
		i++
	}
	inter := data[2+len(params) : i]
	if i >= len(data) || data[i] < 0x40 || data[i] > 0x7e {
		return i, InputKey, ReplyNone // unfinished: the program reads it as typed
	}
	final, n := data[i], i+1
	if final == 'M' && len(params) == 0 && len(inter) == 0 {
		if len(data) >= n+3 {
			return n + 3, mouseKind(int(data[n]) - 32), ReplyNone // X10 mouse
		}
		return n, InputKey, ReplyNone
	}
	var marker byte
	if len(params) > 0 && params[0] >= '<' && params[0] <= '?' {
		marker, params = params[0], params[1:]
	}
	// kitty ends its DA1 answer with an empty field: CSI ?62;c, ?62;52;c.
	if marker == '?' && len(inter) == 0 && final == 'c' && len(params) > 1 && params[len(params)-1] == ';' {
		params = params[:len(params)-1]
	}
	// A mode report echoes the mode asked for, which xterm.js reads up to
	// 2^31-1; other answers have short fields.
	digits := 5
	if string(inter) == "$" && final == 'y' {
		digits = 10
	}
	fields, ok := numbers(params, digits)
	switch {
	case !ok:
	case marker == '<' && len(inter) == 0 && (final == 'M' || final == 'm') && fields == 3:
		if final == 'm' {
			return n, InputMouse, ReplyNone // SGR release
		}
		return n, mouseKind(firstNumber(params)), ReplyNone // SGR mouse
	case marker == 0 && len(inter) == 0 && final == 'M' && fields == 3:
		return n, mouseKind(firstNumber(params) - 32), ReplyNone // urxvt mouse
	case marker == 0 && len(inter) == 0 && len(params) == 0 && final == 'I':
		return n, InputFocusIn, ReplyNone
	case marker == 0 && len(inter) == 0 && len(params) == 0 && final == 'O':
		return n, InputFocusOut, ReplyNone
	case marker == '?' && len(inter) == 0 && final == 'c' && fields >= 1:
		return n, InputReply, ReplyDA1
	case marker == '>' && len(inter) == 0 && final == 'c' && fields >= 1 && fields <= 3:
		return n, InputReply, ReplyDA2
	case marker == 0 && len(inter) == 0 && final == 'n' && (string(params) == "0" || string(params) == "3"):
		return n, InputReply, ReplyStatus
	case marker == 0 && len(inter) == 0 && final == 'R' && fields == 2,
		marker == '?' && len(inter) == 0 && final == 'R' && (fields == 2 || fields == 3):
		return n, InputReply, ReplyCursor
	case (marker == 0 || marker == '?') && string(inter) == "$" && final == 'y' && fields == 2:
		return n, InputReply, ReplyMode
	}
	return n, InputKey, ReplyNone
}

// mouseKind tells pointer motion with no button pressed (motion bit 32,
// button code 3, no wheel bit 64; modifier bits allowed) from the rest.
func mouseKind(button int) InputKind {
	if button >= 0 && button&32 != 0 && button&3 == 3 && button&64 == 0 {
		return InputMotion
	}
	return InputMouse
}

// firstNumber reads the first field of a parameter list numbers accepted.
func firstNumber(params []byte) int {
	n := 0
	for _, c := range params {
		if c == ';' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// numbers counts the fields of a parameter list made only of numbers of at
// most the given digits.
func numbers(params []byte, most int) (int, bool) {
	if len(params) == 0 {
		return 0, true
	}
	fields, digits := 1, 0
	for _, c := range params {
		switch {
		case c >= '0' && c <= '9':
			digits++
			if digits > most {
				return 0, false
			}
		case c == ';' && digits > 0:
			fields, digits = fields+1, 0
		default:
			return 0, false
		}
	}
	return fields, digits > 0
}

// stringEnd finds the end of an answer string: BEL (OSC only) or ST.
func stringEnd(data []byte, bel bool) (end, n int, ok bool) {
	for i := 2; i < len(data) && i < replyLimit; i++ {
		switch {
		case data[i] == 0x07 && bel:
			return i, i + 1, true
		case data[i] == 0x1b:
			if i+1 < len(data) && data[i+1] == '\\' {
				return i, i + 2, true
			}
			return 0, 0, false
		case data[i] < 0x20 || data[i] >= 0x7f:
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// colorReply accepts OSC 4;n;rgb:h/h/h and OSC 10|11|12;rgb:h/h/h.
func colorReply(data []byte) (int, bool) {
	end, n, ok := stringEnd(data, true)
	if !ok {
		return 0, false
	}
	body := string(data[2:end])
	var spec string
	switch {
	case len(body) > 2 && body[:2] == "4;":
		rest := body[2:]
		i := 0
		for i < len(rest) && i < 3 && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(rest) || rest[i] != ';' {
			return 0, false
		}
		spec = rest[i+1:]
	case len(body) > 3 && (body[:3] == "10;" || body[:3] == "11;" || body[:3] == "12;"):
		spec = body[3:]
	default:
		return 0, false
	}
	if len(spec) < 4 || spec[:4] != "rgb:" {
		return 0, false
	}
	parts, digits := 1, 0
	for _, c := range spec[4:] {
		switch {
		case c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			digits++
			if digits > 4 {
				return 0, false
			}
		case c == '/' && digits > 0:
			parts, digits = parts+1, 0
		default:
			return 0, false
		}
	}
	return n, parts == 3 && digits > 0
}

// settingReply accepts DCS 0|1 $ r Pt ST with printable Pt.
func settingReply(data []byte) (int, bool) {
	if len(data) < 5 || data[2] != '0' && data[2] != '1' || data[3] != '$' || data[4] != 'r' {
		return 0, false
	}
	_, n, ok := stringEnd(data, false)
	return n, ok
}

// csiQuery names the answer a control sequence asks for, reading its
// parameter as a number the way terminal applications do (06n is 6n).
func csiQuery(body []byte, final byte) Reply {
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
	// As xterm.js reads it: the first parameter decides, clamped at
	// 2^31-1, and the others (and sub-parameters) are ignored.
	n, first := 0, true
	for _, c := range params {
		switch {
		case c >= '0' && c <= '9':
			if first {
				n = min(n*10+int(c-'0'), math.MaxInt32)
			}
		case c == ';' || c == ':':
			first = false
		default:
			return ReplyNone
		}
	}
	switch {
	case final == 'c' && len(inter) == 0 && marker == 0 && n == 0:
		return ReplyDA1
	case final == 'c' && len(inter) == 0 && marker == '>' && n == 0:
		return ReplyDA2
	case final == 'n' && len(inter) == 0 && marker == 0 && n == 5:
		return ReplyStatus
	case final == 'n' && len(inter) == 0 && (marker == 0 || marker == '?') && n == 6:
		return ReplyCursor
	case final == 'p' && string(inter) == "$" && (marker == 0 || marker == '?'):
		return ReplyMode
	}
	return ReplyNone
}

// ReplyKinds bounds the Reply values, for tables indexed by them.
const ReplyKinds = int(ReplyColor) + 1

// Queries counts the queries in a stretch of output by the answer each asks
// for, as OnQuery would hear them. The stretch is read on its own.
func Queries(data []byte) (counts [ReplyKinds]int) {
	f := New(Options{OnQuery: func(kind Reply) { counts[kind]++ }})
	f.Append(make([]byte, 0, len(data)), data)
	return counts
}
