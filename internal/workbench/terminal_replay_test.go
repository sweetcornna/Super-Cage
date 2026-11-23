package workbench

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTerminalModesFollowOutput(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{"nothing set", "plain text\r\n\x1b[1;31mred\x1b[0m", ""},
		{"full-screen program", "\x1b[?1049h\x1b[?1h\x1b=\x1b[?25l\x1b[?1000;1006h\x1b[?1004h\x1b[?2004h\x1b[2 q\x1b(0",
			"\x1b[?1049h\x1b[?1h\x1b=\x1b[?25l\x1b[2 q\x1b[?1000h\x1b[?1006h\x1b[?1004h\x1b[?2004h\x1b(0"},
		{"left again", "\x1b[?1049h\x1b[?1003h\x1b[?1006h\x1b[?1049l\x1b[?1003l\x1b[?1006l", ""},
		{"any-motion mouse replaces button mouse", "\x1b[?1000h\x1b[?1002h\x1b[?1003h", "\x1b[?1003h"},
		{"resetting any tracking stops it", "\x1b[?1003h\x1b[?1000l", ""},
		{"full reset", "\x1b[?1049h\x1b[?2004h\x1bc", ""},
		{"soft reset keeps the screen", "\x1b[?1049h\x1b[?1h\x1b[?25l\x1b[!p", "\x1b[?1049h"},
		{"older alternate screens", "\x1b[?47h", "\x1b[?1049h"},
		{"line drawing ends", "\x1b(0qqq\x1b(B", ""},
		{"no autowrap", "\x1b[?7l", "\x1b[?7l"},
		// Queries, reports and other prefixes are not modes.
		{"not modes", "\x1b[?1049$p\x1b[>4;2m\x1b[>7u\x1b[?u\x1b[=1;1u\x1b[>1049h\x1b[1049h", ""},
		// Strings may hold anything until their terminator.
		{"strings", "\x1b]0;[?1049h title\x07\x1bP+q[?2004h;6d\x1b\\\x1b]52;c;YWJj\x1b\\\x1b_[?1h\x1b\\\x1b[?2004h", "\x1b[?2004h"},
		// As in xterm.js, an escape ends a string and starts a sequence.
		{"an escape ends a string", "\x1b]0;title\x1b[?2004h", "\x1b[?2004h"},
		{"cancel", "\x1b[?10\x18\x1b[?2004h", "\x1b[?2004h"},
		{"controls inside a sequence", "\x1b[?20\r04h", "\x1b[?2004h"},
	} {
		var m terminalModes
		m.feed([]byte(tc.output))
		if got := string(m.preamble()); got != tc.want {
			t.Errorf("%s: preamble %q, want %q", tc.name, got, tc.want)
		}
		if !m.ground() {
			t.Errorf("%s: not at a boundary", tc.name)
		}
	}
	// Byte by byte is the same as at once.
	all := "\x1b[?1049h\x1b]0;x\x07\x1b[?1000;1006h\x1b[?2004h中\x1b[?1h"
	var whole, split terminalModes
	whole.feed([]byte(all))
	for i := range len(all) {
		split.feed([]byte{all[i]})
	}
	if !bytes.Equal(whole.preamble(), split.preamble()) || !split.ground() {
		t.Fatalf("split %q, whole %q", split.preamble(), whole.preamble())
	}
}

func TestTerminalModesFinishCompletesCutSequence(t *testing.T) {
	for _, tc := range []struct {
		before, after string
		skip          int
		ok            bool
		want          string
	}{
		{"\x1b[38;2;1", "2;3mX", 4, true, ""},
		{"\x1b[?10", "49hX", 3, true, "\x1b[?1049h"},
		{"\x1b", "[?2004hX", 7, true, "\x1b[?2004h"},
		{"\xe4", "\xb8\xadX", 2, true, ""},
		{"\x1b]0;long tit", "le\x07X", 3, true, ""},
		{"\x1bP+q", "\x1b\\X", 2, true, ""},
		{"X", "\x1b[1mY", 0, true, ""},
		// The history ends inside the sequence: it is shown from its first
		// byte, as before the tracker, and the modes are those before it.
		{"\x1b[?10", "49", 0, false, ""},
		{"\x1b]52;c;QUFB", "QUFB\r\nyyy\r\n", 0, false, ""},
	} {
		var m terminalModes
		m.feed([]byte(tc.before))
		if skip, ok := m.finish([]byte(tc.after)); skip != tc.skip || ok != tc.ok {
			t.Errorf("%q|%q: skip %d ok %v, want %d %v", tc.before, tc.after, skip, ok, tc.skip, tc.ok)
		}
		if got := string(m.preamble()); got != tc.want {
			t.Errorf("%q|%q: preamble %q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
}

// The tracker reads output as xterm.js's parser does (EscapeSequenceParser
// and Params in @xterm/xterm 6): the viewer is told the modes its terminal
// would be in, and a replay starts where xterm.js would be between
// sequences.
func TestTerminalModesParseAsXtermJS(t *testing.T) {
	params := func(n int) string { return "\x1b[?" + strings.Repeat("0;", n) + "1049h" }
	for _, tc := range []struct {
		name, output, want string
		ground             bool
	}{
		// xterm.js keeps 32 parameters and drops the digits of later ones;
		// the sequence still acts on the 32 it kept.
		{"1049 as parameter 21", params(20), "\x1b[?1049h", true},
		{"1049 as parameter 32", params(31), "\x1b[?1049h", true},
		{"1049 as parameter 33", params(32), "", true},
		{"1049 as parameter 257", params(256), "", true},
		{"modes kept past the limit", "\x1b[?1049;" + strings.Repeat("0;", 40) + "2004h", "\x1b[?1049h", true},
		// A sub-parameter belongs to the parameter before it.
		{"sub-parameter", "\x1b[?1049:1h", "\x1b[?1049h", true},
		// A prefix counts only first; elsewhere the sequence is ignored.
		{"prefix after a digit", "\x1b[1?1049h", "", true},
		{"prefix after an intermediate", "\x1b[ ?1049h", "", true},
		// C1 controls (U+0080..U+009F, sent as UTF-8) act as their ESC forms.
		{"C1 CSI", "\xc2\x9b?1049h", "\x1b[?1049h", true},
		{"C1 OSC", "\xc2\x9d0;title", "", false},
		{"C1 ST ends an OSC", "\xc2\x9d0;title\xc2\x9c[?1h", "", true},
		{"C1 ST ends a DCS", "\x1bP+q\xc2\x9c[?1h", "", true},
		{"C1 DCS", "\xc2\x90+q", "", false},
		{"C1 APC", "\xc2\x9fG", "", false},
		{"other C1 controls end a sequence", "\x1b[?10\xc2\x8549h", "", true},
		{"a raw C1 byte is no character", "\x9b?1049h", "", true},
		// Only OSC ends at BEL.
		{"BEL ends an OSC", "\x1b]0;title\x07[?1h", "", true},
		{"BEL does not end a DCS", "\x1bP+q\x07[?1h", "", false},
		{"BEL does not end an APC", "\x1b_Ga=q\x07[?1h", "", false},
		{"BEL does not end a PM", "\x1b^note\x07", "", false},
		// Outside OSC and DCS, a character past C1 ends the sequence.
		{"a character ends an APC", "\x1b_G中[?1h", "", true},
		{"a character ends a CSI", "\x1b[?10中49h", "", true},
		{"a DCS keeps characters", "\x1bP+q中", "", false},
		{"an OSC keeps characters", "\x1b]0;中", "", false},
		{"ESC ends a DCS", "\x1bP+q\x07\x1b[?2004h", "\x1b[?2004h", true},
		// Charsets xterm.js does not know leave the line-drawing set alone.
		{"unknown charset", "\x1b(0\x1b(z", "\x1b(0", true},
		{"default charset", "\x1b(0\x1b%G", "", true},
	} {
		var whole, split terminalModes
		whole.feed([]byte(tc.output))
		for i := range len(tc.output) {
			split.feed([]byte{tc.output[i]})
		}
		if got := string(whole.preamble()); got != tc.want || whole.ground() != tc.ground {
			t.Errorf("%s: preamble %q ground %v, want %q %v", tc.name, got, whole.ground(), tc.want, tc.ground)
		}
		if !bytes.Equal(split.preamble(), whole.preamble()) || split.ground() != whole.ground() {
			t.Errorf("%s: byte by byte %q %v, at once %q %v", tc.name, split.preamble(), split.ground(), whole.preamble(), whole.ground())
		}
	}
}

// A string left open before the kept history (a clipboard write never ended,
// then plain output for more than the history keeps) does not end within it:
// the replay is the whole kept history from its oldest byte, as before the
// modes were followed, never nothing.
func TestTerminalReplayShowsTailOfStringNeverEnded(t *testing.T) {
	f := newTerminalStreamFixture(t)
	f.write([]byte("\x1b[?2004h\x1b]52;c;"+strings.Repeat("QUFB", 64)), 1<<10)
	f.write(bytes.Repeat([]byte(strings.Repeat("y", 78)+"\r\n"), 2*terminalHistoryLimit/80), 16<<10)
	f.write([]byte("flood-done\r\n"), 64)
	f.session.mu.Lock()
	oldest, history := f.session.oldestLocked(), append([]byte(nil), f.session.history...)
	f.session.mu.Unlock()
	if oldest == 0 {
		t.Fatal("history was not trimmed")
	}
	out := f.session.outputAfter(0, 1<<20)
	if start := out.Next - uint64(len(out.Data)); !out.Reset || start != oldest || !bytes.Equal(out.Data, history) {
		t.Fatalf("replay [%d,+%d), want the kept history [%d,+%d)", start, len(out.Data), oldest, len(history))
	}
	if string(out.State) != "\x1b[?2004h" {
		t.Fatalf("state %q", out.State)
	}
}

// codexLikeOutput is a program that enters the alternate screen with the
// mouse and bracketed paste, then redraws coloured cells for ever: more than
// the kept history, so the start leaves it in the middle of a sequence.
func codexLikeOutput() (start, frames []byte) {
	start = []byte("\x1b[?2004h\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[1;1H\x1b[JWelcome menu")
	var b bytes.Buffer
	for b.Len() < terminalHistoryLimit+(64<<10)+7 {
		b.WriteString("\x1b[?2026h\x1b[3;5H\x1b[38;2;135;134;132;49m\xe2\xa3\xbf\x1b[?2026l")
	}
	return start, b.Bytes()
}

func TestTerminalReplayAfterTrimStartsAtBoundaryWithModes(t *testing.T) {
	f := newTerminalStreamFixture(t)
	start, frames := codexLikeOutput()
	f.write(start, len(start))
	// Not trimmed yet: the whole output, no state, nothing to repaint.
	out := f.session.outputAfter(0, 1<<20)
	if out.Reset || len(out.State) != 0 || out.Repaint || !bytes.Equal(out.Data, start) {
		t.Fatalf("untrimmed history changed: %+v", out)
	}
	f.write(frames, 16<<10)
	f.session.mu.Lock()
	written, oldest := f.session.written, f.session.oldestLocked()
	f.session.mu.Unlock()
	if oldest == 0 {
		t.Fatal("history was not trimmed")
	}
	all := append(append([]byte(nil), start...), frames...)
	wantState := "\x1b[?1049h\x1b[?1003h\x1b[?1006h\x1b[?2004h"
	check := func(name string, data []byte, offset uint64, state string) {
		t.Helper()
		if state != wantState {
			t.Fatalf("%s: state %q, want %q", name, state, wantState)
		}
		if offset < oldest || offset+uint64(len(data)) != written || !bytes.Equal(data, all[offset:]) {
			t.Fatalf("%s: replay [%d,+%d) is not the kept tail [%d,%d)", name, offset, len(data), oldest, written)
		}
		// It starts where a sequence starts and holds no half sequence.
		var m terminalModes
		m.feed(all[:offset])
		if !m.ground() || data[0] != 0x1b {
			t.Fatalf("%s: replay starts inside a sequence: %q", name, data[:16])
		}
	}
	out = f.session.outputAfter(0, 1<<20)
	if !out.Reset || !out.Repaint {
		t.Fatalf("trimmed read: reset=%v repaint=%v", out.Reset, out.Repaint)
	}
	check("output", out.Data, out.Next-uint64(len(out.Data)), string(out.State))

	// A viewer with offsets gets the state in its history marker.
	ws := f.dial("?after=0")
	view := &terminalView{}
	var state string
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for len(view.markers) == 0 || view.replay > 0 {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.TextMessage && strings.Contains(string(data), "history") {
			var marker struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(data, &marker)
			state = marker.State
		}
		view.frame(t, kind, data)
	}
	if !view.markers[0].Reset {
		t.Fatal("a trimmed replay was not a reset")
	}
	check("stream", view.data, view.start, state)
	_ = ws.Close()

	// A viewer without offsets gets the state in front of its one frame.
	legacy := f.dial("")
	if _, _, err := legacy.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	_, frame, err := legacy.ReadMessage()
	if err != nil || !bytes.HasPrefix(frame, []byte(wantState)) {
		t.Fatalf("legacy replay does not start with the state: %q %v", frame[:min(len(frame), 40)], err)
	}
	check("legacy", frame[len(wantState):], written-uint64(len(frame)-len(wantState)), wantState)

	// The phone gets it as a field beside the data.
	f.server.terminals.sessions = map[string]*terminalSession{"0123456789abcdef": f.session}
	w := request(f.server, "GET", "/api/mobile/terminals/0123456789abcdef/output?after=0", "", "", true)
	var reply struct {
		Data  []byte `json:"data"`
		Next  uint64 `json:"next"`
		Reset bool   `json:"reset"`
		State string `json:"state"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil || !reply.Reset {
		t.Fatalf("phone read: %d %s", w.Code, w.Body.String()[:min(w.Body.Len(), 200)])
	}
	check("phone", reply.Data, reply.Next-uint64(len(reply.Data)), reply.State)
}

// Viewers are told which replayed output reached a viewer before, so that
// they answer only queries nobody was shown.
func TestTerminalReplayTellsWhatWasAnswered(t *testing.T) {
	f := newTerminalStreamFixture(t)
	f.write([]byte("before anyone\x1b[c"), 64)
	first := f.dial("?after=0")
	view := &terminalView{}
	var answered []uint64
	read := func(ws *websocket.Conn, v *terminalView, done func() bool) {
		t.Helper()
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		for !done() {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if kind == websocket.TextMessage && strings.Contains(string(data), "history") {
				var marker struct {
					Answered *uint64 `json:"answered"`
				}
				if json.Unmarshal(data, &marker) != nil || marker.Answered == nil {
					t.Fatalf("marker without answered: %s", data)
				}
				answered = append(answered, *marker.Answered)
			}
			v.frame(t, kind, data)
		}
	}
	read(first, view, func() bool { return len(view.markers) == 1 && view.replay == 0 })
	if answered[0] != 0 {
		t.Fatalf("output no viewer saw was reported answered: %d", answered[0])
	}
	shown := uint64(len("before anyone\x1b[c") + len("live\x1b[c"))
	f.write([]byte("live\x1b[c"), 64)
	read(first, view, func() bool { return view.offset == shown })
	_ = first.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.session.mu.Lock()
		viewers := len(f.session.clients)
		f.session.mu.Unlock()
		if viewers == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the viewer did not leave")
		}
	}
	f.write([]byte("while away\x1b[c"), 64)
	second := f.dial("?after=0")
	again := &terminalView{}
	read(second, again, func() bool { return len(again.markers) == 1 && again.replay == 0 })
	if answered[1] != shown {
		t.Fatalf("answered %d, want %d", answered[1], shown)
	}
}

// A viewer shown a trimmed history of a full-screen program gets its whole
// screen: one row less, then the size the terminal has. Nothing else
// resizes, and a normal-screen program is left alone.
func TestTerminalRepaintsFullScreenProgramAfterTrimmedReplay(t *testing.T) {
	gap := terminalRepaintGap
	terminalRepaintGap = 20 * time.Millisecond
	t.Cleanup(func() { terminalRepaintGap = gap })
	f, file := viewerStreams(t)
	desktop := f.session.openPeer("desktop-tab-0001", false)
	_ = f.session.peerResize(desktop, 120, 40)
	feed := func(data []byte) {
		f.session.mu.Lock()
		target := f.session.written + uint64(len(data))
		f.session.mu.Unlock()
		for len(data) > 0 {
			n := min(len(data), 16<<10)
			if _, err := file.writer.Write(data[:n]); err != nil {
				t.Fatal(err)
			}
			data = data[n:]
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
			f.session.mu.Lock()
			done := f.session.written >= target
			f.session.mu.Unlock()
			if done {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("output not recorded")
			}
		}
	}
	sizes := func() [][2]uint16 {
		file.mu.Lock()
		defer file.mu.Unlock()
		return append([][2]uint16(nil), file.sizes...)
	}
	// A normal-screen program with a trimmed history: no repaint.
	feed(bytes.Repeat([]byte("line of a long log\r\n"), terminalHistoryLimit/16))
	before := len(sizes())
	ws := f.dial("?after=0")
	view := &terminalView{}
	view.read(t, ws, func() bool { return len(view.markers) == 1 && view.replay == 0 })
	time.Sleep(100 * time.Millisecond)
	if got := sizes(); len(got) != before {
		t.Fatalf("a normal-screen program was resized: %v", got[before:])
	}
	_ = ws.Close()
	// A full-screen program: one row less, then 120x40 again.
	start, frames := codexLikeOutput()
	feed(start)
	feed(frames)
	before = len(sizes())
	ws = f.dial("?after=0")
	view = &terminalView{}
	view.read(t, ws, func() bool { return len(view.markers) == 1 && view.replay == 0 })
	for deadline := time.Now().Add(5 * time.Second); len(sizes()) < before+2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no repaint: %v", sizes()[before:])
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := sizes()[before:]; len(got) != 2 || got[0] != [2]uint16{120, 39} || got[1] != [2]uint16{120, 40} {
		t.Fatalf("repaint resizes: %v", got)
	}
	// A viewer that resumes where it was continues: no repaint.
	before = len(sizes())
	resume := f.dial("?after=" + strconv.FormatUint(view.offset, 10))
	again := &terminalView{offset: view.offset}
	again.read(t, resume, func() bool { return len(again.markers) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := sizes(); len(got) != before {
		t.Fatalf("a resuming viewer repainted: %v", got[before:])
	}
	// Ownership did not move.
	f.session.mu.Lock()
	owner := f.session.sizing.owner
	f.session.mu.Unlock()
	if owner != desktop.key {
		t.Fatalf("repaint moved the owner to %q", owner)
	}
}

// Output counts as answered only once it was written to a viewer whose
// terminal answers it: not when a viewer is registered or the PTY is read,
// not for an observer, not for a phone reply that may be lost.
func TestTerminalAnsweredOnlyWhatReachedAViewer(t *testing.T) {
	f := newTerminalStreamFixture(t)
	query := []byte("query\x1b[c")
	answered := func() uint64 {
		t.Helper()
		f.session.mu.Lock()
		defer f.session.mu.Unlock()
		return f.session.shown
	}
	waitAnswered := func(want uint64) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); answered() != want; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("answered %d, want %d", answered(), want)
			}
		}
	}
	// A viewer that never took the output: its socket dropped first.
	idle := f.session.subscribeFrom(nil)
	f.write(query, 64)
	if got := f.session.outputAfter(0, 1<<20).Answered; got != 0 {
		t.Fatalf("a query no viewer was written was answered up to %d", got)
	}
	idle.Close()
	// An observer is written it, but its answers never reach the program.
	watch := f.dial("?after=0&viewer=watch-tab-0001&observe=1")
	view := &terminalView{}
	view.read(t, watch, func() bool { return len(view.markers) == 1 && view.replay == 0 })
	time.Sleep(50 * time.Millisecond)
	if got := answered(); got != 0 {
		t.Fatalf("an observer answered up to %d", got)
	}
	// A viewer that was written the replay, then live output, answered both.
	ws := f.dial("?after=0")
	view = &terminalView{}
	view.read(t, ws, func() bool { return len(view.markers) == 1 && view.replay == 0 })
	waitAnswered(uint64(len(query)))
	f.write(query, 64)
	view.read(t, ws, func() bool { return view.offset == uint64(2*len(query)) })
	waitAnswered(uint64(2 * len(query)))
	_ = ws.Close()
	_ = watch.Close()

	// The phone has drawn a reply when it asks for what follows it.
	f.server.terminals.sessions = map[string]*terminalSession{"0123456789abcdef": f.session}
	read := func(after uint64) (next, answered uint64) {
		t.Helper()
		w := request(f.server, "GET", "/api/mobile/terminals/0123456789abcdef/output?after="+strconv.FormatUint(after, 10), "", "", true)
		var reply struct {
			Next     uint64 `json:"next"`
			Answered uint64 `json:"answered"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("phone read: %d %s", w.Code, w.Body.String())
		}
		return reply.Next, reply.Answered
	}
	before := answered()
	f.write(query, 64)
	next, _ := read(before)
	// Its reply was lost: it reads from the same place again, and that
	// output is not answered yet.
	if _, got := read(before); got != before {
		t.Fatalf("a phone reply that may be lost was answered: %d, want %d", got, before)
	}
	f.write(query, 64)
	if _, got := read(next); got != next {
		t.Fatalf("the phone's next read: answered %d, want %d", got, next)
	}
}
