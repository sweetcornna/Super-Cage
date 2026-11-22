//go:build !windows

package workbench

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// R3-2: an internal error in an ssh terminal's output or input ends that
// session only. Its window gets the exit reset and Super Cage's line; the
// connection's other sessions and the controller go on, and new sessions
// open.
func TestEntryInternalErrorEndsOnlyThatSession(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	frame, note := entryFrame, entryNote
	t.Cleanup(func() { entryFrame, entryNote = frame, note })
	entryFrame = func(o *entryOutput, out, data []byte, replay, restart bool) []byte {
		if bytes.Contains(data, []byte("PANIC-OUT")) {
			panic("test: output")
		}
		return frame(o, out, data, replay, restart)
	}
	entryNote = func(p *entryPresence, data []byte, focus func() bool) []byte {
		if bytes.Contains(data, []byte("\x07\x07\x07")) {
			panic("test: input")
		}
		return note(p, data, focus)
	}
	client, err := f.dial(f.device)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a := entryOpenShell(t, client, 100, 30, "")
	b := entryOpenShell(t, client, 100, 30, "")
	for _, shell := range []*entryShell{a, b} {
		shell.send("printf 'R=%s\\n' ready\n")
		shell.expect("R=")
	}
	ended := func(shell *entryShell) {
		t.Helper()
		entryAwait(t, 10*time.Second, "the session's end", func() bool { return strings.Contains(shell.text(), entryInternalError) })
		text := shell.text()
		reset := strings.LastIndex(text, entryTitlePop)
		if reset < 0 || reset > strings.LastIndex(text, entryInternalError) {
			t.Fatalf("no exit reset before Super Cage's line: %q", text[max(0, len(text)-300):])
		}
		if err := shell.session.Wait(); err == nil {
			t.Fatal("the session ended well")
		}
	}
	// The output step fails in a.
	a.send("printf 'PANIC-%s\\n' OUT\n")
	ended(a)
	if strings.Contains(a.text(), "PANIC-OUT") {
		t.Fatal("the output that failed reached the window")
	}
	b.send("printf 'S=%s\\n' still\n")
	b.expect("S=")
	// The input step fails in b.
	b.send("\x07\x07\x07")
	ended(b)
	// The connection and the controller go on.
	c := entryOpenShell(t, client, 100, 30, "")
	c.send("printf 'N=%s\\n' new\n")
	c.expect("N=")
	entryAwait(t, 10*time.Second, "the two terminals' end", func() bool { return len(entrySSHTerminals(f.s)) == 1 })
}
