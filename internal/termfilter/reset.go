package termfilter

// What a program that relays a filtered stream to a local terminal writes
// around it: TitlePush before the first output, and on the way out End
// (Abort, then ResetSequence). These names are stable for other packages.

// TitlePush saves the window's title and icon name before anything else is
// written: ResetSequence pops them back.
const TitlePush = "\x1b[22;0t"

// ResetSequence returns the local terminal to what a shell expects, whatever
// the program in the Cage terminal left set. A program that shows a Cage
// stream on a local terminal (cage attach, the SSH entry) writes it on every
// way out after its first output, after Filter.Abort (or as Filter.End).
// It sets only defaults, so it is harmless when nothing was changed.
const ResetSequence = "\x18\x1b\\" + // abort a sequence or string in progress, end a string
	"\x1b[?2026l" + // end a synchronized update
	// Back to the main screen with a whole-screen scroll region, the cursor
	// where it was: both the main screen's restore (when in the alternate
	// screen) and the region reset move it, and the saved cursor is per
	// screen.
	"\x1b7\x1b[?1049l\x1b[r\x1b8" +
	"\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" + // every mouse mode
	"\x1b[?1004l\x1b[?2004l\x1b[?2048l\x1b[?2031l" + // focus, bracketed paste, in-band resize, colour scheme reports
	"\x1b[<99u" + // pop every kitty keyboard level
	"\x1b[?1l\x1b[?66l\x1b>" + // normal cursor keys and keypad
	"\x1b[?7h\x1b[?45l\x1b[4l\x1b[20l" + // wrap, reverse wrap, insert and new line modes
	"\x1b(B\x1b)B\x0f" + // US ASCII in G0 and G1, G0 in use
	"\x1b[m\x1b[0 q\x1b[?25h" + // character attributes, cursor style, visible cursor
	"\x1b]104\x07\x1b]110\x07\x1b]111\x07\x1b]112\x07" + // palette and dynamic colours
	"\x1b[23;0t" // the title and icon name TitlePush saved

// End returns what to write to the local terminal when the stream ends, in
// the order that matters: Abort spoils a string the application is inside
// (a clipboard write streaming through), then ResetSequence. It appends to
// dst and forgets everything held.
func (f *Filter) End(dst []byte) []byte {
	return append(f.Abort(dst), ResetSequence...)
}
