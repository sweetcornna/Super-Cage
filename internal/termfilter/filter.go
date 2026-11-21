// Package termfilter removes escape sequences a guest must not send to the
// owner's own terminal application, byte by byte, with no buffering beyond
// one unfinished sequence. It sits between a Cage terminal stream and the
// local application in cage attach.
//
// Every byte it emits is either ground text (valid UTF-8 without C1 control
// characters) or an escape sequence it parsed and allowed in full. A sequence
// it cannot parse within HoldLimit bytes, or does not know, is dropped: the
// filter fails closed. Long payloads it does allow (OSC 52 clipboard writes,
// sixel images, kitty images with direct data) are classified on a bounded
// header and then streamed through, restricted to their payload alphabet.
package termfilter

import "unicode/utf8"

// HoldLimit bounds the one unfinished sequence the filter keeps between
// chunks. A sequence still undecided at this size is dropped.
const HoldLimit = 4 << 10

// NoticeLimit bounds window titles and notification texts.
const NoticeLimit = 256

// ClipboardLimit bounds the base64 text of one OSC 52 clipboard write. It is
// what Cage's web terminal accepts: 2 MiB of decoded text.
const ClipboardLimit = 4 * ((2<<20 + 2) / 3)

// Options are the owner's per-computer choices.
type Options struct {
	// ClipboardWrite passes OSC 52 clipboard writes to the application,
	// like Cage's web terminal does. Clipboard reads are always dropped.
	ClipboardWrite bool
}

type state uint8

const (
	stGround     state = iota
	stUTF8             // inside a multi-byte character; its bytes are in u8
	stEsc              // after ESC, held
	stEscInter         // ESC and intermediates, held
	stCSI              // control sequence, held
	stCSIDrop          // dropping a control sequence until its final byte
	stString           // OSC, DCS or APC, held until it can be decided
	stStringPass       // an allowed string whose payload is streamed through
	stStringDrop       // dropping a string until its terminator
	stStrEsc           // ESC inside a string; strState says which
)

type stringKind uint8

const (
	kindOSC stringKind = iota
	kindDCS
	kindAPC
	kindOther // PM, SOS: always dropped
)

// alphabet restricts the bytes of a streamed payload.
type alphabet uint8

const (
	alphaBase64 alphabet = iota
	alphaSixel
)

// Filter is the state of one stream. It is not safe for concurrent use.
type Filter struct {
	opts     Options
	replay   bool
	st       state
	strState state      // the string state an ESC interrupted
	kind     stringKind // of the current string
	alpha    alphabet   // of the current streamed payload
	header   bool       // the DCS header is complete
	streamed int        // payload bytes streamed so far
	limit    int        // payload bound of the current streamed string
	u8       [4]byte
	u8n      int
	u8need   int
	n        int // held bytes
	hold     [HoldLimit]byte
}

// New returns a filter for one stream.
func New(opts Options) *Filter { return &Filter{opts: opts} }

// SetReplay marks the bytes that follow as replayed history: output the
// viewer has already seen elsewhere. Replay drops what would act again in the
// application: queries (their answers would be stale), clipboard writes,
// notifications and bells. Like Cage's web terminal, replayed output only
// draws.
func (f *Filter) SetReplay(on bool) { f.replay = on }

// Pending is the number of bytes held for an unfinished sequence.
func (f *Filter) Pending() int { return f.n + f.u8n }

// Reset forgets an unfinished sequence, as after a stream that restarts.
func (f *Filter) Reset() { f.st, f.n, f.u8n = stGround, 0, 0 }

// ground bytes that pass as they are; everything else goes through step.
var textClass = func() (table [256]bool) {
	for b := 0x20; b < 0x7f; b++ {
		table[b] = true
	}
	for _, b := range []byte{'\t', '\n', '\r', '\b'} {
		table[b] = true
	}
	return
}()

// Append filters src and appends the result to dst. Bytes of an unfinished
// sequence at the end of src are kept for the next call.
func (f *Filter) Append(dst, src []byte) []byte {
	i := 0
	for i < len(src) {
		if f.st == stGround {
			start := i
			for {
				for i < len(src) && textClass[src[i]] {
					i++
				}
				// A whole, valid UTF-8 character that is not a C1 control;
				// anything else takes the byte-wise path.
				if i < len(src) && src[i] >= 0x80 {
					if r, size := utf8.DecodeRune(src[i:]); size > 1 && r > 0x9f {
						i += size
						continue
					}
				}
				break
			}
			dst = append(dst, src[start:i]...)
			if i == len(src) {
				break
			}
		}
		dst = f.step(dst, src[i])
		i++
	}
	return dst
}

func (f *Filter) push(b byte) bool {
	if f.n >= HoldLimit {
		return false
	}
	f.hold[f.n] = b
	f.n++
	return true
}

func (f *Filter) emitHold(dst []byte) []byte {
	dst = append(dst, f.hold[:f.n]...)
	f.n = 0
	return dst
}

func (f *Filter) startEscape() { f.n, f.st = 0, stEsc; f.push(0x1b) }

func (f *Filter) step(dst []byte, b byte) []byte {
	switch f.st {
	case stGround:
		return f.ground(dst, b)
	case stUTF8:
		return f.utf8(dst, b)
	case stEsc, stEscInter:
		return f.escape(dst, b)
	case stCSI, stCSIDrop:
		return f.csi(dst, b)
	case stString, stStringPass, stStringDrop:
		return f.str(dst, b)
	case stStrEsc:
		return f.strEsc(dst, b)
	}
	return dst
}

func (f *Filter) ground(dst []byte, b byte) []byte {
	switch {
	case b == 0x1b:
		f.startEscape()
	case b == 0x05: // ENQ: the answerback can name the application
	case b == 0x07:
		if !f.replay {
			dst = append(dst, b)
		}
	case b < 0x20 || b == 0x7f:
		dst = append(dst, b)
	case b >= 0xc2 && b <= 0xf4:
		f.u8[0], f.u8n = b, 1
		switch {
		case b < 0xe0:
			f.u8need = 2
		case b < 0xf0:
			f.u8need = 3
		default:
			f.u8need = 4
		}
		f.st = stUTF8
	default:
		// A stray continuation byte or an invalid lead: an application that
		// does not decode UTF-8 could read 0x80-0x9f as a C1 control.
	}
	return dst
}

// utf8 holds a multi-byte character until it is complete and valid. Overlong
// forms, surrogates and C1 controls (U+0080-U+009F) are dropped.
func (f *Filter) utf8(dst []byte, b byte) []byte {
	lo, hi := byte(0x80), byte(0xbf)
	if f.u8n == 1 {
		switch f.u8[0] {
		case 0xc2:
			lo = 0xa0 // U+0080-U+009F are C1 controls
		case 0xe0:
			lo = 0xa0
		case 0xed:
			hi = 0x9f
		case 0xf0:
			lo = 0x90
		case 0xf4:
			hi = 0x8f
		}
	}
	if b < lo || b > hi {
		f.u8n, f.st = 0, stGround
		if b >= 0x80 && b <= 0xbf {
			return dst // the rest of the invalid character
		}
		return f.ground(dst, b)
	}
	f.u8[f.u8n] = b
	f.u8n++
	if f.u8n == f.u8need {
		dst = append(dst, f.u8[:f.u8n]...)
		f.u8n, f.st = 0, stGround
	}
	return dst
}

// control handles a C0 byte met inside an escape or control sequence, where
// an application executes it at once. It reports whether b was one.
func (f *Filter) control(dst *[]byte, b byte) bool {
	switch {
	case b == 0x1b:
		f.startEscape()
	case b == 0x18 || b == 0x1a: // CAN, SUB abort the sequence
		f.n, f.st = 0, stGround
	case b < 0x20:
		if b != 0x05 && (b != 0x07 || !f.replay) {
			*dst = append(*dst, b)
		}
	case b == 0x7f: // ignored inside sequences
	default:
		return false
	}
	return true
}

func (f *Filter) escape(dst []byte, b byte) []byte {
	if f.control(&dst, b) {
		return dst
	}
	if b >= 0x80 {
		f.n, f.st = 0, stGround
		return f.ground(dst, b)
	}
	if f.st == stEsc {
		switch b {
		case '[':
			f.push(b)
			f.st = stCSI
			return dst
		case ']':
			return f.beginString(dst, b, kindOSC)
		case 'P':
			return f.beginString(dst, b, kindDCS)
		case '_':
			return f.beginString(dst, b, kindAPC)
		case '^', 'X':
			f.n, f.kind, f.st = 0, kindOther, stStringDrop
			return dst
		}
	}
	if b < 0x30 { // intermediate
		if !f.push(b) {
			f.n, f.st = 0, stGround
			return dst
		}
		f.st = stEscInter
		return dst
	}
	// Final byte. ESC Z (DECID) asks for the primary attributes.
	if f.st == stEsc && b == 'Z' && f.replay {
		f.n, f.st = 0, stGround
		return dst
	}
	f.push(b)
	f.st = stGround
	return f.emitHold(dst)
}

func (f *Filter) beginString(dst []byte, b byte, kind stringKind) []byte {
	f.push(b)
	f.kind, f.st, f.header = kind, stString, false
	return dst
}

func (f *Filter) csi(dst []byte, b byte) []byte {
	if f.control(&dst, b) {
		return dst
	}
	if f.st == stCSIDrop {
		if b >= 0x40 && b <= 0x7e || b >= 0x80 {
			f.st = stGround
		}
		return dst
	}
	switch {
	case b >= 0x80:
		f.n, f.st = 0, stGround
		return f.ground(dst, b)
	case b >= 0x40:
		if !f.push(b) {
			f.n, f.st = 0, stGround
			return dst
		}
		f.st = stGround
		if f.allowCSI(f.hold[2:f.n-1], b) {
			return f.emitHold(dst)
		}
		f.n = 0
		return dst
	case b >= 0x30 && f.n > 2 && f.hold[f.n-1] < 0x30:
		// A parameter after an intermediate: the application ignores the
		// whole sequence, and so does the filter.
		f.n, f.st = 0, stCSIDrop
		return dst
	}
	if !f.push(b) {
		f.n, f.st = 0, stCSIDrop
	}
	return dst
}

// allowCSI decides a complete control sequence from its parameter and
// intermediate bytes and its final byte.
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
	switch {
	case marker == '>' && (final == 'q' || final == 'c'): // XTVERSION, DA2
		return false
	case marker == '=' && final == 'c': // DA3: the unit id
		return false
	case marker == 0 && len(inter) == 0 && final == 't':
		return f.allowWindowOp(params)
	}
	if f.replay && csiQuery(marker, params, inter, final) {
		return false
	}
	return true
}

// allowWindowOp keeps the XTWINOPS a program needs to size its output and
// its title stack, and drops the ones that move, resize, raise or iconify the
// owner's window, or read the title back into the guest.
func (f *Filter) allowWindowOp(params []byte) bool {
	n := firstParam(params)
	switch n {
	case 22, 23: // push and pop the title
		return true
	case 14, 16, 18: // text area and cell size in pixels, size in characters
		return !f.replay
	}
	return false
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

// csiQuery reports control sequences that make the application answer.
func csiQuery(marker byte, params, inter []byte, final byte) bool {
	switch {
	case final == 'c' || final == 'n' || final == 'x': // DA, DSR and CPR, DECREQTPARM
		return true
	case final == 'p' && len(inter) == 1 && inter[0] == '$': // DECRQM
		return true
	case marker == '?' && (final == 'u' || final == 'm' || final == 'S'): // kitty flags, XTQMODKEYS, XTSMGRAPHICS
		return true
	}
	return false
}

// str handles the bytes of an OSC, DCS, APC, PM or SOS string.
func (f *Filter) str(dst []byte, b byte) []byte {
	switch b {
	case 0x1b:
		f.strState, f.st = f.st, stStrEsc
		return dst
	case 0x07:
		if f.kind == kindOSC {
			return f.endString(dst, f.st, []byte{0x07})
		}
	case 0x18, 0x1a:
		if f.st == stStringPass {
			dst = append(dst, b) // the application aborts what it was given
		}
		f.n, f.st = 0, stGround
		return dst
	}
	switch f.st {
	case stStringDrop:
		return dst
	case stStringPass:
		if b < 0x20 && f.alpha == alphaSixel {
			return dst // ignored inside sixel data by every application
		}
		if f.payloadByte(b) && f.streamed < f.limit {
			f.streamed++
			return append(dst, b)
		}
		return f.abortStream(dst)
	}
	if b < 0x20 || b == 0x7f || b >= 0x80 && f.kind != kindOSC {
		// Only OSC text may hold UTF-8; it is validated when decided.
		f.n, f.st = 0, stStringDrop
		return dst
	}
	if !f.push(b) {
		f.n, f.st = 0, stStringDrop
		return dst
	}
	return f.classify(dst)
}

func (f *Filter) payloadByte(b byte) bool {
	if f.alpha == alphaSixel {
		return b >= 0x21 && b <= 0x7e
	}
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' || b == '='
}

// abortStream ends a streamed payload that broke its alphabet or bound, so
// the application is not left inside a string, and drops the rest. An OSC 52
// write is spoiled first so no application applies a truncated clipboard.
func (f *Filter) abortStream(dst []byte) []byte {
	if f.kind == kindOSC {
		dst = append(dst, '!', 0x07)
	} else {
		dst = append(dst, 0x1b, '\\')
	}
	f.n, f.st = 0, stStringDrop
	return dst
}

func (f *Filter) strEsc(dst []byte, b byte) []byte {
	if b == '\\' {
		return f.endString(dst, f.strState, []byte{0x1b, '\\'})
	}
	// Any other ESC ends the string in the application too; the byte
	// starts a new escape sequence.
	dst = f.endString(dst, f.strState, []byte{0x1b, '\\'})
	f.startEscape()
	return f.escape(dst, b)
}

// endString finishes a string that was in state from with terminator term.
func (f *Filter) endString(dst []byte, from state, term []byte) []byte {
	f.st = stGround
	switch from {
	case stStringPass:
		return append(dst, term...)
	case stStringDrop:
		f.n = 0
		return dst
	}
	if f.n > 0 && f.decide(true) {
		dst = f.emitHold(dst)
		return append(dst, term...)
	}
	f.n = 0
	return dst
}

// classify runs after each held string byte: it may stream or drop early.
func (f *Filter) classify(dst []byte) []byte {
	switch f.kind {
	case kindOSC:
		return f.classifyOSC(dst)
	case kindDCS:
		return f.classifyDCS(dst)
	case kindAPC:
		return f.classifyAPC(dst)
	}
	f.n, f.st = 0, stStringDrop
	return dst
}

// held string content after "ESC ]", "ESC P" or "ESC _".
func (f *Filter) body() []byte { return f.hold[2:f.n] }

func (f *Filter) classifyOSC(dst []byte) []byte {
	body := f.body()
	semi := -1
	for i, c := range body {
		if c == ';' {
			semi = i
			break
		}
	}
	if semi < 0 {
		if len(body) > 0 && (body[len(body)-1] < '0' || body[len(body)-1] > '9' || len(body) > 5) {
			f.n, f.st = 0, stStringDrop
		}
		return dst
	}
	switch string(body[:semi]) {
	case "52":
		return f.classifyClipboard(dst, body[semi+1:])
	case "0", "1", "2", "4", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19",
		"22", "99", "104", "105", "110", "111", "112", "113", "114", "115", "116", "117", "118", "119", "133", "777":
		return dst // decided at the terminator, within HoldLimit
	}
	f.n, f.st = 0, stStringDrop
	return dst
}

// classifyClipboard streams an allowed OSC 52 write once its selection and
// first data byte are known.
func (f *Filter) classifyClipboard(dst []byte, rest []byte) []byte {
	semi := -1
	for i, c := range rest {
		if c == ';' {
			semi = i
			break
		}
	}
	if semi < 0 {
		if len(rest) > 1 || len(rest) == 1 && rest[0] != 'c' {
			f.n, f.st = 0, stStringDrop
		}
		return dst
	}
	data := rest[semi+1:]
	if len(data) == 0 {
		return dst
	}
	if !f.opts.ClipboardWrite || f.replay || data[0] == '?' || semi > 1 || semi == 1 && rest[0] != 'c' {
		f.n, f.st = 0, stStringDrop
		return dst
	}
	f.alpha, f.limit = alphaBase64, ClipboardLimit
	if !f.payloadByte(data[0]) {
		f.n, f.st = 0, stStringDrop
		return dst
	}
	f.streamed, f.st = 1, stStringPass
	return f.emitHold(dst)
}

func (f *Filter) classifyDCS(dst []byte) []byte {
	body := f.body()
	c := body[len(body)-1]
	if f.header {
		return dst // data of an allowed request, held until its end
	}
	if c < 0x40 {
		if len(body) > 64 || len(body) > 1 && c >= 0x30 && body[len(body)-2] < 0x30 {
			// Too long, or a parameter after an intermediate.
			f.n, f.st = 0, stStringDrop
		}
		return dst
	}
	{
		// c is the final byte of the header.
		f.header = true
		header := body[:len(body)-1]
		var inter []byte
		for i, h := range header {
			if h < 0x30 {
				inter = header[i:]
				header = header[:i]
				break
			}
		}
		switch {
		case c == 'q' && len(inter) == 0 && (len(header) == 0 || header[0] >= '0' && header[0] <= ';'): // sixel
			f.alpha, f.limit, f.streamed, f.st = alphaSixel, int(^uint(0)>>1), 0, stStringPass
			return f.emitHold(dst)
		case c == 'q' && len(inter) == 1 && inter[0] == '$' && len(header) == 0: // DECRQSS
			if f.replay {
				f.n, f.st = 0, stStringDrop
			}
			return dst
		case c == 's' && len(inter) == 0 && (string(header) == "=1" || string(header) == "=2"): // synchronized update
			return dst
		}
		// XTGETTCAP (+q) answers with the application's terminfo name; tmux
		// passthrough, soft fonts, key definitions and the rest are unknown.
		f.n, f.st = 0, stStringDrop
		return dst
	}
}

func (f *Filter) classifyAPC(dst []byte) []byte {
	body := f.body()
	if body[0] != 'G' {
		f.n, f.st = 0, stStringDrop
		return dst
	}
	if body[len(body)-1] != ';' {
		return dst // control data continues; decided at ';' or the end
	}
	if !f.allowGraphics(body[1 : len(body)-1]) {
		f.n, f.st = 0, stStringDrop
		return dst
	}
	f.alpha, f.limit, f.streamed, f.st = alphaBase64, int(^uint(0)>>1), 0, stStringPass
	return f.emitHold(dst)
}

// allowGraphics reads kitty graphics control data: key=value pairs separated
// by commas. Only direct data (t=d, the default) passes: the other
// transmission media make the application read the owner's files or memory.
func (f *Filter) allowGraphics(control []byte) bool {
	for len(control) > 0 {
		pair, rest := control, []byte(nil)
		for i, c := range control {
			if c == ',' {
				pair, rest = control[:i], control[i+1:]
				break
			}
		}
		control = rest
		if len(pair) < 3 || pair[1] != '=' || !letter(pair[0]) {
			return false
		}
		for _, c := range pair[2:] {
			if !letter(c) && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
		switch pair[0] {
		case 't':
			if string(pair[2:]) != "d" {
				return false
			}
		case 'a':
			if f.replay && string(pair[2:]) == "q" {
				return false
			}
		}
	}
	return true
}

// decide is called with a complete held string. It may rewrite the hold.
func (f *Filter) decide(_ bool) bool {
	switch f.kind {
	case kindDCS:
		return f.header // only allowed requests are still held after their header
	case kindAPC:
		body := f.body()
		return len(body) > 0 && body[0] == 'G' && f.allowGraphics(body[1:])
	case kindOSC:
		return f.decideOSC()
	}
	return false
}

func (f *Filter) decideOSC() bool {
	body := f.body()
	if !utf8.Valid(body) || hasC1(body) {
		return false
	}
	semi := -1
	for i, c := range body {
		if c == ';' {
			semi = i
			break
		}
	}
	if semi < 0 {
		// Colour resets take no parameter.
		switch string(body) {
		case "104", "105", "110", "111", "112", "113", "114", "115", "116", "117", "118", "119":
			return true
		}
		return false
	}
	text := body[semi+1:]
	switch string(body[:semi]) {
	case "0", "1", "2":
		return len(text) <= NoticeLimit
	case "8":
		return allowLink(text)
	case "9":
		return !f.replay && len(text) <= NoticeLimit
	case "777":
		return !f.replay && len(text) <= NoticeLimit && hasPrefix(text, "notify;")
	case "99":
		return !f.replay && f.rewriteKittyNotification(semi)
	case "4", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "104", "105", "110", "111", "112", "113", "114", "115", "116", "117", "118", "119":
		return !f.replay || !contains(text, '?')
	case "22", "133":
		return true
	}
	return false
}

// allowLink keeps OSC 8 hyperlinks to http and https only; the empty target
// ends a link. Other schemes (file:, ssh:, x-man-page: …) open local things.
func allowLink(text []byte) bool {
	semi := -1
	for i, c := range text {
		if c == ';' {
			semi = i
			break
		}
	}
	if semi < 0 {
		return false
	}
	uri := text[semi+1:]
	if len(uri) == 0 {
		return true
	}
	return hasPrefixFold(uri, "http://") || hasPrefixFold(uri, "https://")
}

// rewriteKittyNotification keeps a kitty notification (OSC 99) with plain
// metadata and drops its actions: a=report makes the application type into
// the guest when the notification is clicked. Queries (p=?) are dropped.
func (f *Filter) rewriteKittyNotification(semi int) bool {
	start := 2 + semi + 1 // first metadata byte in hold
	end := start
	for end < f.n && f.hold[end] != ';' {
		end++
	}
	if end == f.n || f.n-end-1 > NoticeLimit {
		return false
	}
	out := start
	for i := start; i < end; {
		j := i
		for j < end && f.hold[j] != ':' {
			j++
		}
		pair := f.hold[i:j]
		keep := true
		if len(pair) < 2 || pair[1] != '=' {
			return false
		}
		switch pair[0] {
		case 'a':
			keep = false
		case 'p':
			if v := string(pair[2:]); v != "title" && v != "body" {
				return false
			}
		case 'i', 'd', 'u', 'o', 'e':
		default:
			return false
		}
		if keep {
			if out > start {
				f.hold[out] = ':'
				out++
			}
			out += copy(f.hold[out:], pair)
		}
		i = j + 1
	}
	n := copy(f.hold[out:], f.hold[end:f.n])
	f.n = out + n
	return true
}

func letter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func hasC1(text []byte) bool {
	for i := 0; i+1 < len(text); i++ {
		if text[i] == 0xc2 && text[i+1] >= 0x80 && text[i+1] <= 0x9f {
			return true
		}
	}
	return false
}

func contains(text []byte, c byte) bool {
	for _, b := range text {
		if b == c {
			return true
		}
	}
	return false
}

func hasPrefix(text []byte, prefix string) bool {
	return len(text) >= len(prefix) && string(text[:len(prefix)]) == prefix
}

func hasPrefixFold(text []byte, prefix string) bool {
	if len(text) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := text[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}
