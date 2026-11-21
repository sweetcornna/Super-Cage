// Package termfilter removes escape sequences a guest must not send to the
// owner's own terminal application, byte by byte, with no buffering beyond
// one unfinished sequence. It sits between a Cage terminal stream and the
// local application in cage attach.
//
// Every byte it emits is either ground text (valid UTF-8 without C1 control
// characters) or an escape sequence it parsed and allowed in full. A sequence
// it cannot parse within HoldLimit bytes, or does not know, is dropped: the
// filter fails closed. Control sequences, escape sequences and modes pass
// from allow-lists (allow.go): what real programs send that Cage's web
// terminal (xterm.js) also acts on. The one long payload it allows, an OSC 52
// clipboard write, is classified on a bounded header and then streamed
// through, restricted to base64.
package termfilter

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

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
	// ClipboardNow, when set, is asked as each clipboard write arrives: the
	// write passes only while it says the owner is at this window.
	ClipboardNow func() bool
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

// Filter is the state of one stream. It is not safe for concurrent use.
type Filter struct {
	opts     Options
	replay   bool
	st       state
	strState state      // the string state an ESC interrupted
	kind     stringKind // of the current string
	header   bool       // the DCS header is complete
	streamed int        // payload bytes streamed so far
	limit    int        // payload bound of the current streamed string
	u8       [4]byte
	u8n      int
	u8need   int
	n        int // held bytes
	hold     [HoldLimit]byte

	focus   bool           // the guest asked for focus reports (mode 1004)
	resets  int            // full and soft resets passed to the application
	dropped map[string]int // what was dropped, by kind
}

// GuestFocus reports whether the guest turned on focus reporting. The filter
// never passes mode 1004: the viewer owns the host terminal's focus reports.
func (f *Filter) GuestFocus() bool { return f.focus }

// Resets counts the full (RIS) and soft (DECSTR) resets that passed; a reset
// also turns off what the viewer itself set in the application.
func (f *Filter) Resets() int { return f.resets }

// Dropped returns how many sequences were dropped, by kind.
func (f *Filter) Dropped() map[string]int { return f.dropped }

// Report lists the drops one kind per line, most frequent first.
func (f *Filter) Report() string {
	kinds := make([]string, 0, len(f.dropped))
	for kind := range f.dropped {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		a, b := f.dropped[kinds[i]], f.dropped[kinds[j]]
		return a > b || a == b && kinds[i] < kinds[j]
	})
	var b strings.Builder
	for _, kind := range kinds {
		fmt.Fprintf(&b, "%6d  %q\n", f.dropped[kind], kind)
	}
	return b.String()
}

func (f *Filter) count(kind string) {
	if f.dropped == nil {
		f.dropped = map[string]int{}
	}
	f.dropped[kind]++
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
			f.count("PM/SOS")
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
	f.push(b)
	f.st = stGround
	if !allowEscape(f.hold[1:f.n]) {
		f.count("ESC " + string(f.hold[1:f.n]))
		f.n = 0
		return dst
	}
	if f.n == 2 && b == 'c' {
		f.resets++
	}
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
		if f.n > 0 {
			f.count(csiName(f.hold[2:f.n-1], b))
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
		if base64Byte(b) && f.streamed < f.limit {
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

func base64Byte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' || b == '='
}

// abortStream ends a streamed OSC 52 write that broke its alphabet or
// bound, so the application is not left inside a string, and drops the
// rest. The write is spoiled first so no application applies a truncated
// clipboard.
func (f *Filter) abortStream(dst []byte) []byte {
	dst = append(dst, '!', 0x07)
	f.count("OSC 52 malformed")
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
	}
	f.count("APC")
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
			f.count("OSC " + string(body))
			f.n, f.st = 0, stStringDrop
		}
		return dst
	}
	switch string(body[:semi]) {
	case "52":
		return f.classifyClipboard(dst, body[semi+1:])
	case "0", "1", "2", "4", "8", "9", "10", "11", "12", "99", "104", "110", "111", "112", "133", "777":
		return dst // decided at the terminator, within HoldLimit
	}
	f.count("OSC " + string(body[:semi]))
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
			f.count("OSC 52")
			f.n, f.st = 0, stStringDrop
		}
		return dst
	}
	data := rest[semi+1:]
	if len(data) == 0 {
		return dst
	}
	if !f.opts.ClipboardWrite || f.replay || data[0] == '?' || semi > 1 || semi == 1 && rest[0] != 'c' {
		f.count("OSC 52")
		f.n, f.st = 0, stStringDrop
		return dst
	}
	if f.opts.ClipboardNow != nil && !f.opts.ClipboardNow() {
		f.count("OSC 52 away") // not queued: the owner is not at this window
		f.n, f.st = 0, stStringDrop
		return dst
	}
	f.limit = ClipboardLimit
	if !base64Byte(data[0]) {
		f.n, f.st = 0, stStringDrop
		return dst
	}
	f.streamed, f.st = 1, stStringPass
	return f.emitHold(dst)
}

// classifyDCS keeps only DECRQSS, a query for one of the settings Cage's
// web terminal reports; its reply is gated by the viewer rules. Sixel
// images, synchronized update by DCS, tmux passthrough, XTGETTCAP, soft
// fonts and key definitions are dropped.
func (f *Filter) classifyDCS(dst []byte) []byte {
	body := f.body()
	if f.header {
		if len(body) > 6 {
			f.count("DCS $q")
			f.n, f.st = 0, stStringDrop
		}
		return dst // the setting asked for, decided at the end
	}
	c := body[len(body)-1]
	if c < 0x40 {
		if string(body) != "$" {
			f.count("DCS")
			f.n, f.st = 0, stStringDrop
		}
		return dst
	}
	if c != 'q' || string(body) != "$q" || f.replay {
		f.count("DCS " + string(body))
		f.n, f.st = 0, stStringDrop
		return dst
	}
	f.header = true
	return dst
}

// decide is called with a complete held string. It may rewrite the hold.
func (f *Filter) decide(_ bool) bool {
	switch f.kind {
	case kindDCS:
		// DECRQSS for the settings xterm.js reports: SGR, scroll region,
		// cursor style, protection and conformance level.
		if !f.header {
			return false
		}
		switch string(f.body()[2:]) {
		case "m", "r", " q", "\"q", "\"p":
			return true
		}
		f.count("DCS $q")
		return false
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
		case "104", "110", "111", "112":
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
		return !f.replay && allowOSC9(text)
	case "777":
		return !f.replay && len(text) <= NoticeLimit && hasPrefix(text, "notify;")
	case "99":
		return !f.replay && f.rewriteKittyNotification(semi)
	case "4", "10", "11", "12", "104", "110", "111", "112":
		return !f.replay || !contains(text, '?')
	case "133":
		return allowMark(text)
	}
	return false
}

// allowOSC9 keeps the two meanings of OSC 9 that stay in the window: a
// notification (iTerm2, Ghostty, WezTerm) and ConEmu's progress report
// 9;4;state[;percent]. Every other ConEmu sub-command is a number first:
// 9;9 sets the working directory of the next local tab in Ghostty and
// Windows Terminal, 9;2 opens a message box, 9;6 runs a GUI macro. So a
// notification whose first field is a number is dropped.
func allowOSC9(text []byte) bool {
	if len(text) > NoticeLimit {
		return false
	}
	first, rest, more := cut(text, ';')
	if !number(first) {
		return true // notification text
	}
	if string(first) != "4" || !more {
		return false
	}
	state, percent, hasPercent := cut(rest, ';')
	if !number(state) || len(state) != 1 || state[0] > '4' {
		return false
	}
	if !hasPercent {
		return true
	}
	return number(percent) && len(percent) <= 3 && atoi(percent) <= 100
}

// allowMark keeps the shell integration marks of OSC 133 alone: prompt,
// input and output start (A, B, C), and the end of a command (D) with its
// exit status. Their key=value extras ask for more (click_events moves the
// cursor on click, cmdline_url carries a URL) and are dropped.
func allowMark(text []byte) bool {
	mark, status, hasStatus := cut(text, ';')
	switch string(mark) {
	case "A", "B", "C":
		return !hasStatus
	case "D":
		return !hasStatus || number(status) && len(status) <= 5
	}
	return false
}

// cut splits text at the first sep.
func cut(text []byte, sep byte) (before, after []byte, found bool) {
	for i, c := range text {
		if c == sep {
			return text[:i], text[i+1:], true
		}
	}
	return text, nil, false
}

// number reports whether text is one or more decimal digits.
func number(text []byte) bool {
	if len(text) == 0 {
		return false
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoi(text []byte) int {
	n := 0
	for _, c := range text {
		n = n*10 + int(c-'0')
	}
	return n
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
