package termfilter

import (
	"strings"
	"testing"
)

// Cage's web terminal sees the output unfiltered and answers what xterm.js
// answers: it reads only the first parameter (clamped at 2^31-1) and
// ignores the rest. Every such query is counted, also one the filter drops
// for its size, and every answer is read as one.
func TestQueriesCountWhatTheWebTerminalAnswers(t *testing.T) {
	for _, row := range []struct {
		query string
		kind  Reply
	}{
		{"\x1b[?70000$p", ReplyMode},     // above the bound: dropped here, answered there
		{"\x1b[?1234567$p", ReplyMode},   // more digits than a mode has
		{"\x1b[?$p", ReplyMode},          // no mode: xterm.js reports mode 0
		{"\x1b[6;1n", ReplyCursor},       // later parameters ignored
		{"\x1b[000000006n", ReplyCursor}, // leading zeros
		{"\x1b[0;1c", ReplyDA1},          //
		{"\x1b[00000000c", ReplyDA1},     //
		{"\x1b[5:1n", ReplyStatus},       // a sub-parameter
	} {
		if counts := Queries([]byte(row.query)); counts[row.kind] != 1 {
			t.Errorf("%q not counted as %d: %v", row.query, row.kind, counts)
		}
	}
	for _, query := range []string{"\x1b[1c", "\x1b[7n", "\x1b[6<1n"} {
		if counts := Queries([]byte(query)); counts != [ReplyKinds]int{} {
			t.Errorf("%q counted: %v", query, counts)
		}
	}
	for _, reply := range []string{"\x1b[?70000;0$y", "\x1b[?1234567;0$y", "\x1b[?2147483647;0$y", "\x1b[0;0$y"} {
		if n, kind, which := NextInput([]byte(reply)); n != len(reply) || kind != InputReply || which != ReplyMode {
			t.Errorf("%q read as %d %d %d", reply, n, kind, which)
		}
	}
}

// What the guest left changed is remembered across an abort (a reconnect
// that starts over): End still undoes it.
func TestEndUndoesWhatWasLeftBeforeAnAbort(t *testing.T) {
	f := New(Options{})
	f.Append(nil, []byte("\x1b]11;rgb:1111/2222/3333\x07\x1b[5 q"))
	f.Abort(nil)
	f.Append(nil, []byte("after\r\n"))
	got := string(f.End(nil, 80))
	if !strings.Contains(got, resetPalette) || !strings.Contains(got, resetCursorStyle) {
		t.Fatalf("forgotten across an abort: %q", got)
	}
}

// The full-reset rewrite does not rely on the soft reset alone, which not
// every emulator implements: it spells out what a shell needs, and all of
// it passes the filter unchanged.
func TestClearResetSpellsOutTheSoftReset(t *testing.T) {
	for _, part := range []string{"\x1b[?2026l", "\x1b[r", "\x1b[?1l", "\x1b>", "\x1b[?7h", "\x1b[4l", "\x1b[m", "\x1b[?25h", "\x1b(B", "\x1b[H\x1b[2J"} {
		if !strings.Contains(ClearReset, part) {
			t.Errorf("%q missing", part)
		}
	}
	if got := string(New(Options{}).Append(nil, []byte(ClearReset))); got != ClearReset {
		t.Fatalf("the filter changes its own rewrite: %q", got)
	}
}
