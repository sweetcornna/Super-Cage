package termfilter

import "strconv"

// What a program that relays a filtered stream to a local terminal writes
// around it: TitlePush before the first output, and on the way out End,
// which undoes what the guest left; ResetSequence, which undoes everything
// that can be undone without knowing what the guest did, is for a reset by
// hand. These names are stable for other packages.

// TitlePush saves the window's title and icon name before anything else is
// written: the reset pops them back.
const TitlePush = "\x1b[22;0t"

// resetStart ends whatever the application is in the middle of: a sequence
// or string in progress, a synchronized update.
const resetStart = "\x18\x1b\\" + // abort a sequence or string in progress, end a string
	"\x1b[?2026l" // end a synchronized update

// resetScreen sets what a shell expects, whatever the program left set. It
// sets only defaults, so it is harmless when nothing was changed.
const resetScreen = "" +
	// Back to the main screen with a whole-screen scroll region, the cursor
	// where it was: both the main screen's restore (when in the alternate
	// screen) and the region reset move it, and the saved cursor is per
	// screen.
	"\x1b7\x1b[?1049l\x1b[r\x1b8" +
	resetMouse +
	"\x1b[?1004l\x1b[?2004l\x1b[?2048l\x1b[?2031l" + // focus, bracketed paste, in-band resize, colour scheme reports
	"\x1b[<99u" + // pop every kitty keyboard level
	"\x1b[?1l\x1b[?66l\x1b>" + // normal cursor keys and keypad
	"\x1b[?7h\x1b[?45l\x1b[4l\x1b[20l" + // wrap, reverse wrap, insert and new line modes
	"\x1b(B\x1b)B\x0f" + // US ASCII in G0 and G1, G0 in use
	"\x1b[m" // character attributes

// resetMouse turns every mouse mode off.
const resetMouse = "\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l"

const (
	resetCursorStyle = "\x1b[0 q"                                         // the profile's cursor style
	cursorVisible    = "\x1b[?25h"                                        //
	resetBlink       = "\x1b[?12l"                                        // the cursor stops blinking
	resetProtection  = "\x1b[0\"q"                                        // DECSCA: characters erasable again
	resetPalette     = "\x1b]104\x07\x1b]110\x07\x1b]111\x07\x1b]112\x07" // palette and dynamic colours
	endLink          = "\x1b]8;;\x07"                                     // close an OSC 8 link
	endProgress      = "\x1b]9;4;0\x07"                                   // clear a progress report
	endCommand       = "\x1b]133;D\x07"                                   // end the command a mark said runs
	titlePop         = "\x1b[23;0t"                                       // the title and icon name TitlePush saved
)

// ResetSequence returns the local terminal to what a shell expects without
// knowing what the program in the Cage terminal did: cage attach --reset
// writes it after an attach that could not reset (killed). It sets only
// defaults, so it is harmless when nothing was changed, but it also resets
// the owner's palette, cursor style and blink, and tab stops up to column
// 256, and ends a progress report and a command mark; End resets those only
// when the guest changed them.
const ResetSequence = resetStart + resetScreen + defaultTabStops + resetCursorStyle + cursorVisible + resetBlink +
	resetProtection + resetPalette + endLink + endProgress + endCommand + titlePop

// ClearReset is what the filter writes for a full reset (ESC c), with the
// scrollback erase unless it is kept (Clear): a soft reset, spelled out as
// well for an emulator without one, the main screen, mouse and paste modes
// off, and a cleared screen with the cursor home. A full reset would also
// reset the window's own settings, in kitty its colour schemes. Everything
// in it passes the filter.
const ClearReset = "\x1b[!p" + // DECSTR
	"\x1b[?2026l\x1b[?1049l\x1b[r" + // no synchronized update, the main screen, a whole-screen region
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?2004l" + // mouse, bracketed paste
	"\x1b[?1l\x1b>\x1b[?7h\x1b[4l\x1b[20l" + // cursor keys, keypad, wrap, insert and new line modes
	"\x1b(B\x1b)B\x0f\x1b[m\x1b[?25h" + // US ASCII in G0 and G1, G0 in use, attributes, cursor
	"\x1b[H\x1b[2J"

// eraseScrollback erases the window's scrollback (Options.KeepScrollback).
const eraseScrollback = "\x1b[3J"

// defaultTabStops sets a tab stop every 8 columns up to 256, the cursor
// kept: appendTabStops for a window whose width is not known.
const defaultTabStops = "\x1b7\x1b[3g" +
	"\x1b[9G\x1bH\x1b[17G\x1bH\x1b[25G\x1bH\x1b[33G\x1bH\x1b[41G\x1bH\x1b[49G\x1bH\x1b[57G\x1bH\x1b[65G\x1bH" +
	"\x1b[73G\x1bH\x1b[81G\x1bH\x1b[89G\x1bH\x1b[97G\x1bH\x1b[105G\x1bH\x1b[113G\x1bH\x1b[121G\x1bH\x1b[129G\x1bH" +
	"\x1b[137G\x1bH\x1b[145G\x1bH\x1b[153G\x1bH\x1b[161G\x1bH\x1b[169G\x1bH\x1b[177G\x1bH\x1b[185G\x1bH\x1b[193G\x1bH" +
	"\x1b[201G\x1bH\x1b[209G\x1bH\x1b[217G\x1bH\x1b[225G\x1bH\x1b[233G\x1bH\x1b[241G\x1bH\x1b[249G\x1bH" +
	"\x1b8"

// Clear appends what the filter writes for a full reset (ESC c): ClearReset
// and, unless KeepScrollback, the scrollback erase. A program relaying the
// stream writes it when the output it relays starts over.
func (f *Filter) Clear(dst []byte) []byte {
	f.paste.Store(false)
	dst = append(dst, ClearReset...)
	if f.opts.KeepScrollback {
		return dst
	}
	return append(dst, eraseScrollback...)
}

// End returns what to write to the local terminal when the stream ends: it
// spoils a string the application is inside (a clipboard write streaming
// through, Abort), then resets what a shell expects, and of the rest only
// what the guest changed: palette, cursor style, tab stops (every 8 columns
// up to cols, or 256 when it is not known), cursor blink, an open link,
// progress, a running command mark. The title pop is last. It appends to
// dst and forgets everything held and what the guest left.
func (f *Filter) End(dst []byte, cols int) []byte {
	left := f.left
	f.left = leftState{}
	f.paste.Store(false)
	dst = append(f.Abort(dst), resetStart...)
	dst = append(dst, resetScreen...)
	if left.tabs {
		// After leaving the alternate screen: kitty keeps tab stops per
		// screen.
		dst = appendTabStops(dst, cols)
	}
	if left.cursorStyle {
		dst = append(dst, resetCursorStyle...)
	}
	dst = append(dst, cursorVisible...)
	if left.blink {
		dst = append(dst, resetBlink...)
	}
	dst = append(dst, resetProtection...)
	if left.palette {
		dst = append(dst, resetPalette...)
	}
	if left.link {
		dst = append(dst, endLink...)
	}
	if left.progress {
		dst = append(dst, endProgress...)
	}
	if left.command {
		dst = append(dst, endCommand...)
	}
	return append(dst, titlePop...)
}

// appendTabStops sets the default tab stops, every 8 columns, with the
// cursor kept where it is.
func appendTabStops(dst []byte, cols int) []byte {
	if cols <= 0 {
		cols = 256
	}
	dst = append(dst, "\x1b7\x1b[3g"...)
	for col := 9; col <= cols; col += 8 {
		dst = append(dst, "\x1b["...)
		dst = strconv.AppendInt(dst, int64(col), 10)
		dst = append(dst, "G\x1bH"...)
	}
	return append(dst, "\x1b8"...)
}
