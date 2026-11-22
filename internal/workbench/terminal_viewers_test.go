package workbench

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// recordingPTY is a PTY whose program records every input and resize.
type recordingPTY struct {
	mu      sync.Mutex
	input   []string
	sizes   [][2]uint16
	reader  *io.PipeReader
	writer  *io.PipeWriter
	changed chan struct{}
	release chan struct{}
	once    sync.Once
	stuck   chan struct{} // when set, Resize waits for it: a program that stopped reading
}

func newRecordingSession(t *testing.T) (*terminalSession, *recordingPTY) {
	reader, writer := io.Pipe()
	file := &recordingPTY{reader: reader, writer: writer, changed: make(chan struct{}, 1), release: make(chan struct{})}
	session := newTerminalSession(file)
	session.id = "0123456789abcdef"
	go session.run()
	t.Cleanup(func() { file.once.Do(func() { close(file.release) }); session.close() })
	return session, file
}

func (f *recordingPTY) Read(data []byte) (int, error) { return f.reader.Read(data) }
func (f *recordingPTY) Write(data []byte) (int, error) {
	f.mu.Lock()
	f.input = append(f.input, string(data))
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
	return len(data), nil
}
func (f *recordingPTY) Resize(cols, rows uint16) error {
	if f.stuck != nil {
		<-f.stuck
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, [2]uint16{cols, rows})
	return nil
}
func (f *recordingPTY) Kill() error  { return nil }
func (f *recordingPTY) Wait() error  { <-f.release; return nil }
func (f *recordingPTY) Close() error { _ = f.writer.Close(); return f.reader.Close() }

func (f *recordingPTY) lastSize() [2]uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sizes) == 0 {
		return [2]uint16{}
	}
	return f.sizes[len(f.sizes)-1]
}

func (f *recordingPTY) resizes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sizes)
}

// received waits until the program has read want, then returns all input.
func (f *recordingPTY) received(t *testing.T, want string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		all := strings.Join(f.input, "")
		f.mu.Unlock()
		if strings.Contains(all, want) {
			return all
		}
		select {
		case <-f.changed:
		case <-deadline:
			t.Fatalf("program never read %q; it read %q", want, all)
		}
	}
}

// refusedSetting is a DECRQSS answer longer than any the viewers read as one.
var refusedSetting = "\x1bP1$r" + strings.Repeat("1;", 70) + "m\x1b\\"

func TestSplitTerminalReplies(t *testing.T) {
	var z terminalSizing
	z.init(80, 24)
	for _, row := range []struct {
		in, rest string
		asked    bool // the viewer was just shown every kind of query
		typing   bool
	}{
		{in: "ls\r", rest: "ls\r", typing: true},
		{in: "\x1b[A", rest: "\x1b[A", typing: true, asked: true},               // arrow up
		{in: "\x1bOP", rest: "\x1bOP", typing: true, asked: true},               // F1
		{in: "\x1b", rest: "\x1b", typing: true, asked: true},                   // Escape key
		{in: "\x1bx", rest: "\x1bx", typing: true, asked: true},                 // Alt+x
		{in: "\x1bP", rest: "\x1bP", typing: true, asked: true},                 // Alt+P
		{in: "\x1b]", rest: "\x1b]", typing: true, asked: true},                 // Alt+]
		{in: "\x1b_", rest: "\x1b_", typing: true, asked: true},                 // Alt+_
		{in: "\x1b]ls\r", rest: "\x1b]ls\r", typing: true, asked: true},         // Esc, then ] typed fast
		{in: "\x1b[<0;10;5M", rest: "\x1b[<0;10;5M", typing: true, asked: true}, // mouse press
		{in: "\x1b[<35;10;5M", rest: ""},                                        // pointer motion, no button
		{in: "\x1b[<32;10;5M", rest: "\x1b[<32;10;5M", typing: true},            // drag with the left button
		{in: "\x1b[97;5u", rest: "\x1b[97;5u", typing: true, asked: true},       // kitty key
		{in: "\x1b[?1;2c", rest: "", asked: true},                               // DA1 answer
		{in: "\x1b[?1;2c", rest: "\x1b[?1;2c", typing: true},                    // the same, nothing asked
		{in: "\x1b[>0;276;0c", rest: "", asked: true},                           // DA2 answer
		{in: "\x1b[0n", rest: "", asked: true},                                  // status report
		{in: "\x1b[?2004;1$y", rest: "", asked: true},                           // DECRPM
		{in: "\x1b]11;rgb:0000/0000/0000\x1b\\", rest: "", asked: true},         // OSC 11 answer
		{in: "\x1b]10;rgb:ffff/ffff/ffff\x07", rest: "", asked: true},           // OSC 10 answer, BEL
		{in: "\x1bP1$r0m\x1b\\", rest: "", asked: true},                         // DECRQSS answer
		{in: "\x1b[12;40R", rest: "", asked: true},                              // cursor position answer
		// What xterm.js 6.0.0 (Cage's web terminal) answers by itself, captured
		// from a real Terminal: every form is an answer.
		{in: "\x1b[?1;1R", rest: "", asked: true},                                                             // to CSI ? 6 n
		{in: "\x1b[?9999;0$y", rest: "", asked: true},                                                         // DECRQM, unknown mode
		{in: "\x1bP0$r\x1b\\", rest: "", asked: true},                                                         // DECRQSS, unknown setting
		{in: "\x1bP1$r61;1\"p\x1b\\", rest: "", asked: true},                                                  // DECRQSS "p
		{in: "\x1b]10;rgb:ffff/ffff/ffff\x1b\\", rest: "", asked: true},                                       // OSC 10, ST
		{in: "\x1b[1;2R", rest: "\x1b[1;2R", typing: true},                                                    // Shift+F3: nothing asked
		{in: "\x1b[I", rest: ""},                                                                              // focus in
		{in: "\x1b[?1;2ca", rest: "a", typing: true, asked: true},                                             // answer and a key together
		{in: "\x1b[200~\x1b[?1;2c\x1b[201~", rest: "\x1b[200~\x1b[?1;2c\x1b[201~", typing: true, asked: true}, // pasted text
		// Shaped like answers but refused by the answer grammar
		// (termfilter.InputFragment): typing, passed on byte for byte.
		{in: "\x1b[?99999999999;0$y", rest: "\x1b[?99999999999;0$y", typing: true, asked: true},
		{in: "\x1b]4;1234;rgb:0/0/0\x07", rest: "\x1b]4;1234;rgb:0/0/0\x07", typing: true, asked: true},
		{in: refusedSetting, rest: refusedSetting, typing: true, asked: true},
		// Answers to what no viewer is ever asked (the filter and Cage's web
		// terminal ask none of these) are typing.
		{in: "\x1b[?15u", rest: "\x1b[?15u", typing: true, asked: true},                             // kitty keyboard flags
		{in: "\x1b[8;40;120t", rest: "\x1b[8;40;120t", typing: true, asked: true},                   // size report
		{in: "\x1bP>|xterm(388)\x1b\\", rest: "\x1bP>|xterm(388)\x1b\\", typing: true, asked: true}, // XTVERSION answer
	} {
		p := &terminalPeer{key: "v:viewer"}
		if row.asked {
			for kind := range termfilter.ReplyKinds {
				p.askLocked(termfilter.Reply(kind), 1, time.Now())
			}
		}
		typing, rest := z.splitReplies(p, row.in, time.Now())
		if typing != row.typing || rest != row.rest {
			t.Errorf("%q: typing %v rest %q, want %v %q", row.in, typing, rest, row.typing, row.rest)
		}
	}
}

func TestTerminalSizeOwnerRules(t *testing.T) {
	session, file := newRecordingSession(t)
	web := session.openPeer("web-tab-identity", false)
	if err := session.peerResize(web, 120, 40); err != nil {
		t.Fatal(err)
	}
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("a sole viewer's size did not apply", file.sizes)
	}
	// A second viewer before anyone typed: its size applies at once.
	local := session.openPeer("local-app-identity", false)
	_ = session.peerResize(local, 200, 50)
	if file.lastSize() != [2]uint16{200, 50} {
		t.Fatal("no typist yet, size not applied", file.sizes)
	}
	// The web tab types: it owns the size.
	if data, _ := session.peerInput(web, "a"); data != "a" || file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("typing did not take the size", data, file.sizes)
	}
	// The local window resizes: remembered, not applied.
	_ = session.peerResize(local, 210, 52)
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("a non-owner resized the PTY", file.sizes)
	}
	// Answers: only the owner's reach the program, and they never move ownership.
	output(session, "\x1b[c")
	if data, _ := session.peerInput(local, "\x1b[?1;2c"); data != "" {
		t.Fatalf("non-owner answer passed: %q", data)
	}
	if data, _ := session.peerInput(web, "\x1b[?1;2c"); data != "\x1b[?1;2c" {
		t.Fatalf("owner answer dropped: %q", data)
	}
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("an answer moved ownership")
	}
	// The local window types: its remembered size applies.
	if data, _ := session.peerInput(local, "b"); data != "b" || file.lastSize() != [2]uint16{210, 52} {
		t.Fatal("typing in the local window did not take the size", file.sizes)
	}
	// Shift+F3 from the web tab with no cursor query waiting is typing.
	if data, _ := session.peerInput(web, "\x1b[1;2R"); data != "\x1b[1;2R" || file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("Shift+F3 was taken for an answer", data)
	}
	// With a cursor query in the output, the same bytes from a non-owner are an answer.
	session.mu.Lock()
	session.sizing.noteOutput([]byte("prompt\x1b["))
	session.sizing.noteOutput([]byte("6n"))
	session.mu.Unlock()
	if data, _ := session.peerInput(local, "\x1b[5;1R"); data != "" {
		t.Fatalf("a split cursor query was missed: %q", data)
	}
}

func TestTerminalOwnerLeavesAndReturns(t *testing.T) {
	grace := terminalOwnerGrace
	terminalOwnerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminalOwnerGrace = grace })
	session, file := newRecordingSession(t)
	web := session.openPeer("web-tab-identity", false)
	_ = session.peerResize(web, 120, 40)
	local := session.openPeer("local-app-identity", false)
	_ = session.peerResize(local, 200, 50)
	_, _ = session.peerInput(local, "x")
	// The owner reconnects within the grace (a dropped stream): nothing changes.
	session.closePeer(local)
	local = session.openPeer("local-app-identity", false)
	time.Sleep(4 * terminalOwnerGrace)
	if file.lastSize() != [2]uint16{200, 50} {
		t.Fatal("a quick reconnect lost ownership", file.sizes)
	}
	// While the owner is gone, the other viewer's answers pass at once.
	session.closePeer(local)
	if data, _ := session.peerInput(web, "\x1b[?1;2c"); data != "\x1b[?1;2c" {
		t.Fatal("no one answered while the owner was away")
	}
	// After the grace, the remaining viewer's size applies.
	deadline := time.Now().Add(2 * time.Second)
	for file.lastSize() != [2]uint16{120, 40} {
		if time.Now().After(deadline) {
			t.Fatal("ownership never passed on", file.sizes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTerminalLegacyAndObservingViewers(t *testing.T) {
	session, file := newRecordingSession(t)
	legacy := session.openPeer("", false)
	attach := session.openPeer("local-app-identity", false)
	watcher := session.openPeer("observer-identity", true)
	_, _ = session.peerInput(attach, "x")
	// A viewer without identity resizes at once, as before, every time.
	before := file.resizes()
	_ = session.peerResize(legacy, 80, 24)
	_ = session.peerResize(legacy, 80, 24)
	if file.resizes() != before+2 || file.lastSize() != [2]uint16{80, 24} {
		t.Fatal("legacy resize changed", file.sizes)
	}
	// Its input is never filtered.
	if data, _ := session.peerInput(legacy, "\x1b[?1;2c"); data != "\x1b[?1;2c" {
		t.Fatal("legacy answer filtered")
	}
	// An observer never types and never sizes.
	if data, _ := session.peerInput(watcher, "rm -rf /\r"); data != "" {
		t.Fatal("observer typed")
	}
	before = file.resizes()
	_ = session.peerResize(watcher, 30, 10)
	if file.resizes() != before {
		t.Fatal("observer resized")
	}
	// The phone keeps today's behaviour too.
	phone := session.phonePeer()
	_ = session.peerResize(phone, 50, 20)
	if file.lastSize() != [2]uint16{50, 20} {
		t.Fatal("phone resize not applied")
	}
	if data, _ := session.peerInput(phone, "\x1b[?1;2c"); data != "\x1b[?1;2c" {
		t.Fatal("phone input filtered")
	}
}

// viewerStreams serves one recording session over the real stream handler.
func viewerStreams(t *testing.T) (*terminalStreamFixture, *recordingPTY) {
	s := testServer(t)
	s.origin = "http://workbench.test"
	session, file := newRecordingSession(t)
	session.id = "viewers"
	s.terminals.sessions = map[string]*terminalSession{session.id: session}
	listener := &terminalPipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	httpServer := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return &terminalStreamFixture{t: t, server: s, session: session, listener: listener}, file
}

func sendTerminal(t *testing.T, ws *websocket.Conn, value map[string]any) {
	t.Helper()
	data, _ := json.Marshal(value)
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

// Two terminal emulators showing one terminal both answer every query. This
// is what happens today with a desktop tab and the phone, or two tabs: the
// program reads each answer twice. Old clients keep that; identified
// viewers answer once.
func TestTerminalStreamsAnswerOnceOnlyForIdentifiedViewers(t *testing.T) {
	f, file := viewerStreams(t)
	answer := "\x1b[?62;22c"
	// Today: two viewers without identity.
	a, b := f.dial("?after=0"), f.dial("?after=0")
	sendTerminal(t, a, map[string]any{"type": "input", "data": answer})
	sendTerminal(t, a, map[string]any{"type": "input", "data": "mark-a"})
	sendTerminal(t, b, map[string]any{"type": "input", "data": answer})
	sendTerminal(t, b, map[string]any{"type": "input", "data": "mark-b"})
	file.received(t, "mark-a")
	if got := file.received(t, "mark-b"); strings.Count(got, answer) != 2 {
		t.Fatalf("old clients changed: %q", got)
	}
	_ = a.Close()
	_ = b.Close()
	// New clients: the owner (typed last) answers, the other does not.
	c, d := f.dial("?after=0&viewer=desktop-tab-0001"), f.dial("?after=0&viewer=local-window-01")
	for _, ws := range []*websocket.Conn{c, d} {
		_, first, err := ws.ReadMessage()
		if err != nil || !strings.Contains(string(first), "\"type\":\"viewer\"") {
			t.Fatalf("no viewer frame: %q %v", first, err)
		}
	}
	sendTerminal(t, d, map[string]any{"type": "input", "data": "k"})
	file.received(t, "k")
	// The program asks; both applications see the question.
	if _, err := file.writer.Write([]byte("\x1b[c")); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []*websocket.Conn{c, d} {
		for {
			_, frame, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(frame), "\x1b[c") {
				break
			}
		}
	}
	sendTerminal(t, c, map[string]any{"type": "input", "data": answer})
	sendTerminal(t, d, map[string]any{"type": "input", "data": answer})
	sendTerminal(t, d, map[string]any{"type": "input", "data": "mark-2"})
	got := file.received(t, "mark-2")
	if strings.Count(got[strings.Index(got, "mark-b")+len("mark-b"):], answer) != 1 {
		t.Fatalf("identified viewers answered twice: %q", got)
	}
	// An old and a new client side by side: the old one is unchanged.
	e := f.dial("?after=0")
	sendTerminal(t, e, map[string]any{"type": "resize", "cols": 90, "rows": 30})
	sendTerminal(t, e, map[string]any{"type": "input", "data": answer + "mark-3"})
	if got := file.received(t, "mark-3"); !strings.HasSuffix(got, answer+"mark-3") {
		t.Fatalf("old client input changed: %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for file.lastSize() != [2]uint16{90, 30} {
		if time.Now().After(deadline) {
			t.Fatal("old client resize not applied", file.sizes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A malformed identity is refused before the upgrade.
	if _, err := f.dialErr("?viewer=no"); err == nil {
		t.Fatal("short identity accepted")
	}
	if _, err := f.dialErr("?observe=1"); err == nil {
		t.Fatal("observe without identity accepted")
	}
}

// TestTerminalMixedViewersStillAnswerTwice records what an identified viewer
// cannot fix alone: a viewer without identity (today's web tab and phone)
// keeps answering as before, so with an identified owner the program reads
// two answers until those views send an identity too. When the viewer
// without identity owns the terminal, the identified one stays silent.
func TestTerminalMixedViewersStillAnswerTwice(t *testing.T) {
	session, _ := newRecordingSession(t)
	web := session.openPeer("", false)
	attach := session.openPeer("attach-identity-1", false)
	answer := "\x1b[?62;22c"
	count := func() int {
		output(session, "[c")
		n := 0
		for _, p := range []*terminalPeer{web, attach} {
			if data, _ := session.peerInput(p, answer); data == answer {
				n++
			}
		}
		return n
	}
	_, _ = session.peerInput(attach, "x")
	if n := count(); n != 2 {
		t.Fatalf("identified owner with a legacy viewer: %d answers", n)
	}
	_, _ = session.peerInput(web, "y")
	if n := count(); n != 1 {
		t.Fatalf("legacy owner with an identified viewer: %d answers", n)
	}
}

// A program that stops reading its terminal (a Windows guest whose resize
// goes through a pipe) stalls no one: a resize request returns within its
// deadline, and input from any viewer and the account lock stay free.
func TestStuckResizeStallsNoOne(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	reader, writer := io.Pipe()
	file := &recordingPTY{reader: reader, writer: writer, changed: make(chan struct{}, 1), release: make(chan struct{}), stuck: make(chan struct{})}
	t.Cleanup(func() { close(file.stuck); file.once.Do(func() { close(file.release) }) })
	session, err := s.terminals.startSessionWithPTY(exec.Command("true"), "/project", "shell", claudeSelection{}, nil, func() (terminalPTY, error) { return file, nil })
	if err != nil {
		t.Fatal(err)
	}
	within := func(what string, limit time.Duration, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
		case <-time.After(limit):
			t.Fatalf("%s stalled", what)
		}
	}
	base := "/api/mobile/terminals/" + session.id
	// The phone's resize decides the size; the PTY never takes it.
	within("a resize", 2*time.Second, func() {
		if w := request(s, "POST", base+"/resize", s.origin, `{"cols":90,"rows":30}`, true); w.Code != http.StatusNoContent {
			t.Errorf("resize: %d %s", w.Code, w.Body.String())
		}
	})
	within("the account lock", time.Second, func() { s.account.mu.Lock(); s.account.mu.Unlock() })
	// Typing from the phone claims the size: another stuck resize.
	within("phone input", 2*time.Second, func() {
		if w := request(s, "POST", base+"/input", s.origin, `{"data":"x"}`, true); w.Code != http.StatusNoContent {
			t.Errorf("input: %d %s", w.Code, w.Body.String())
		}
	})
	within("the account lock", time.Second, func() { s.account.mu.Lock(); s.account.mu.Unlock() })
	// Another viewer's typing and resizing go on.
	desktop := session.openPeer("desktop-tab", false)
	within("another viewer's resize", 2*time.Second, func() { _ = session.peerResize(desktop, 120, 40) })
	within("another viewer's input", 2*time.Second, func() {
		data, err := session.peerInput(desktop, "y")
		if err == nil {
			err = session.enqueueInput(context.Background(), data)
		}
		if err != nil || data != "y" {
			t.Errorf("%q %v", data, err)
		}
	})
	file.received(t, "xy")
}

// output feeds program output to the viewer rules, as the PTY reader does.
func output(session *terminalSession, text string) {
	session.mu.Lock()
	session.sizing.noteOutput([]byte(text))
	session.mu.Unlock()
}

// Input counts as an answer only when it is a complete, well-formed answer
// to a query the program sent to that viewer recently; everything else is
// typing, so no key is ever lost and no answer moves ownership.
func TestTerminalAnswersOnlyToQueriesAsked(t *testing.T) {
	session, file := newRecordingSession(t)
	owner := session.openPeer("owner-identity-1", false)
	other := session.openPeer("other-identity-1", false)
	_ = session.peerResize(other, 100, 30)
	_ = session.peerResize(owner, 120, 40)
	if data, _ := session.peerInput(owner, "x"); data != "x" {
		t.Fatal(data)
	}
	// Keys that start like an answer reach the program from anyone.
	for _, key := range []string{"\x1bP", "\x1b]", "\x1b_", "\x1b]ls\r", "\x1bPq"} {
		if data, _ := session.peerInput(other, key); data != key {
			t.Errorf("non-owner key %q became %q", key, data)
		}
		_, _ = session.peerInput(owner, "x")
	}
	// An answer nobody asked for is typing.
	if data, _ := session.peerInput(other, "\x1b[?62;22c"); data != "\x1b[?62;22c" {
		t.Fatalf("unasked answer dropped: %q", data)
	}
	_, _ = session.peerInput(owner, "x")
	// The program asks for the cursor in a form the old check missed.
	output(session, "\x1b[06n")
	if data, _ := session.peerInput(other, "\x1b[3;4R"); data != "" || file.lastSize() != [2]uint16{120, 40} {
		t.Fatalf("a non-owner's answer passed or moved ownership: %q %v", data, file.lastSize())
	}
	// One query, one answer per viewer: the same bytes again are typing.
	if data, _ := session.peerInput(other, "\x1b[3;4R"); data != "\x1b[3;4R" {
		t.Fatalf("a second answer to one query was dropped: %q", data)
	}
	_, _ = session.peerInput(owner, "x")
	// Answers in the forms a terminal application sends them, to their queries.
	for query, answer := range map[string]string{
		"\x1b[c":           "\x1b[?62;22c",
		"\x1b[0c":          "\x1b[?1;2c",
		"\x1b[>c":          "\x1b[>0;276;0c",
		"\x1b[5n":          "\x1b[0n",
		"\x1b[?6n":         "\x1b[?3;4;1R",
		"\x1b[?2026$p":     "\x1b[?2026;2$y",
		"\x1bP$qm\x1b\\":   "\x1bP1$r0m\x1b\\",
		"\x1b]11;?\x07":    "\x1b]11;rgb:1e1e/1e1e/1e1e\x07",
		"\x1b]4;1;?\x1b\\": "\x1b]4;1;rgb:cd/00/00\x1b\\",
		// As xterm.js 6.0.0 answers them.
		"\x1b[?9999$p":       "\x1b[?9999;0$y",
		"\x1bP$qzz\x1b\\":    "\x1bP0$r\x1b\\",
		"\x1b]4;1;?;2;?\x07": "\x1b]4;1;rgb:cccc/0000/0000\x1b\\\x1b]4;2;rgb:4e4e/9a9a/0606\x1b\\",
	} {
		output(session, query)
		if data, _ := session.peerInput(other, answer); data != "" {
			t.Errorf("answer %q to %q passed from a non-owner", answer, query)
		}
		if data, _ := session.peerInput(owner, answer); data != "" && data != answer {
			t.Errorf("owner answer %q became %q", answer, data)
		}
	}
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("an answer moved ownership", file.lastSize())
	}
}

// A viewer without identity keeps the size behaviour from before the
// viewer rules exactly: its every resize applies, its typing never resizes,
// and its input is never filtered.
func TestTerminalLegacyTypingNeverResizes(t *testing.T) {
	session, file := newRecordingSession(t)
	legacy := session.openPeer("", false)
	attach := session.openPeer("local-app-identity", false)
	_ = session.peerResize(legacy, 80, 24)
	_ = session.peerResize(attach, 120, 40)
	_, _ = session.peerInput(attach, "x")
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal(file.sizes)
	}
	before := file.resizes()
	if data, _ := session.peerInput(legacy, "typed"); data != "typed" {
		t.Fatal(data)
	}
	if file.resizes() != before {
		t.Fatal("typing in a viewer without identity resized the PTY", file.sizes)
	}
	phone := session.phonePeer()
	_ = session.peerResize(phone, 50, 20)
	before = file.resizes()
	_, _ = session.peerInput(phone, "p")
	if file.resizes() != before {
		t.Fatal("typing on a phone without identity resized the PTY", file.sizes)
	}
}

// A phone that sends a viewer identity in its input and resize (the server
// says it may in every terminal summary) takes part in the rules: its
// typing takes the size, its answers to what it was shown count once. One
// without an identity keeps the phone's earlier behaviour.
func TestPhoneViewerIdentity(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	reader, writer := io.Pipe()
	file := &recordingPTY{reader: reader, writer: writer, changed: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { file.once.Do(func() { close(file.release) }) })
	session, err := s.terminals.startSessionWithPTY(exec.Command("true"), "/project", "shell", claudeSelection{}, nil, func() (terminalPTY, error) { return file, nil })
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/mobile/terminals/" + session.id
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		return request(s, method, base+path, s.origin, body, true)
	}
	if w := request(s, "GET", "/api/mobile/terminals", s.origin, "", true); !strings.Contains(w.Body.String(), "\"viewer\":1") {
		t.Fatalf("summaries do not offer the viewer rules: %s", w.Body.String())
	}
	desktop := session.openPeer("desktop-tab-0001", false)
	_ = session.peerResize(desktop, 120, 40)
	_, _ = session.peerInput(desktop, "x")
	// The program asks; the phone is shown the question by its next read.
	if _, err := writer.Write([]byte("\x1b[c")); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/output?after=0", ""); !strings.Contains(w.Body.String(), "\"G1tj\"") { // base64 of the query
		t.Fatalf("phone read: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/resize", "{\"cols\":50,\"rows\":20,\"viewer\":\"phone-viewer-0001\"}"); w.Code != http.StatusNoContent || file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("a named phone that does not own the size resized", w.Code, file.sizes)
	}
	answer := "\x1b[?62;22c"
	if w := call("POST", "/input", "{\"data\":\"\\u001b[?62;22c\",\"viewer\":\"phone-viewer-0001\"}"); w.Code != http.StatusNoContent {
		t.Fatal(w.Code, w.Body.String())
	}
	data, _ := session.peerInput(desktop, "mark")
	_ = session.enqueueInput(context.Background(), data)
	if got := file.received(t, "mark"); strings.Contains(got, answer) || file.lastSize() != [2]uint16{120, 40} {
		t.Fatalf("the named phone's answer passed or took the size: %q %v", got, file.sizes)
	}
	if w := call("POST", "/input", "{\"data\":\"p\",\"viewer\":\"phone-viewer-0001\"}"); w.Code != http.StatusNoContent || file.lastSize() != [2]uint16{50, 20} {
		t.Fatal("typing on a named phone did not take the size", w.Code, file.sizes)
	}
	if w := call("POST", "/input", "{\"data\":\"p\",\"viewer\":\"no\"}"); w.Code != http.StatusBadRequest {
		t.Fatal("a malformed phone identity was accepted", w.Code)
	}
	// Without an identity the phone is as before: its size always applies.
	if w := call("POST", "/resize", "{\"cols\":60,\"rows\":22}"); w.Code != http.StatusNoContent || file.lastSize() != [2]uint16{60, 22} {
		t.Fatal("a phone without identity lost its behaviour", w.Code, file.sizes)
	}
}

// A viewer that opens after the program asked is shown the question in the
// replayed history, and its terminal answers it: that answer is an answer,
// not typing, so it neither reaches the program nor takes the size.
func TestTerminalAnswersToReplayedQueries(t *testing.T) {
	f, file := viewerStreams(t)
	owner := f.dial("?after=0&viewer=owner-tab-0001")
	sendTerminal(t, owner, map[string]any{"type": "resize", "cols": 120, "rows": 40})
	sendTerminal(t, owner, map[string]any{"type": "input", "data": "k"})
	file.received(t, "k")
	if _, err := file.writer.Write([]byte("\x1b[c")); err != nil {
		t.Fatal(err)
	}
	until := func(ws *websocket.Conn, want string) {
		t.Helper()
		for {
			_, frame, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(frame), want) {
				return
			}
		}
	}
	until(owner, "\x1b[c")
	late := f.dial("?after=0&viewer=late-tab-00001")
	until(late, "\x1b[c")
	sendTerminal(t, late, map[string]any{"type": "resize", "cols": 200, "rows": 50})
	answer := "\x1b[?62;22c"
	sendTerminal(t, late, map[string]any{"type": "input", "data": answer})
	sendTerminal(t, late, map[string]any{"type": "ping"})
	until(late, "\"pong\"")
	sendTerminal(t, owner, map[string]any{"type": "input", "data": "mark"})
	got := file.received(t, "mark")
	if strings.Contains(got, answer) || file.lastSize() != [2]uint16{120, 40} {
		t.Fatalf("an answer to a replayed query passed or took the size: %q %v", got, file.sizes)
	}
}

// A burst of queries past the count kept per kind (a TUI starting up, or a
// history replay) still has every answer taken as an answer.
func TestTerminalAnswersPastTheQueryCount(t *testing.T) {
	session, file := newRecordingSession(t)
	owner := session.openPeer("owner-identity-1", false)
	other := session.openPeer("other-identity-1", false)
	_ = session.peerResize(other, 100, 30)
	_ = session.peerResize(owner, 120, 40)
	_, _ = session.peerInput(owner, "x")
	for range 300 {
		output(session, "\x1b[c")
	}
	for i := range 300 {
		if data, _ := session.peerInput(other, "\x1b[?62;22c"); data != "" {
			t.Fatalf("answer %d passed from a non-owner", i+1)
		}
	}
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("an answer took the size", file.sizes)
	}
}

// Answers pay the queries they answer: after ten cursor queries and ten
// answers nothing is owed, and xterm's Shift+F3, shaped like a cursor
// answer, is typing.
func TestTerminalOwedAnswersArePaidDown(t *testing.T) {
	var z terminalSizing
	z.init(80, 24)
	p := &terminalPeer{key: "v:viewer"}
	now := time.Now()
	p.askLocked(termfilter.ReplyCursor, 10, now)
	if typing, rest := z.splitReplies(p, strings.Repeat("\x1b[1;1R", 10), now); typing || rest != "" {
		t.Fatalf("an owed answer was typing: %v %q", typing, rest)
	}
	if typing, _ := z.splitReplies(p, "\x1b[1;2R", now.Add(time.Second)); !typing {
		t.Fatal("Shift+F3 swallowed after every query was answered")
	}
}

// Each query is owed for terminalQueryLive (and less than a second more),
// however many queries of any kind follow it.
func TestTerminalOwedAnswersExpireOneByOne(t *testing.T) {
	var z terminalSizing
	z.init(80, 24)
	p := &terminalPeer{key: "v:viewer"}
	start := time.Now()
	p.askLocked(termfilter.ReplyCursor, 1, start) // never answered
	for s := 2; s <= 8; s += 2 {
		now := start.Add(time.Duration(s) * time.Second)
		p.askLocked(termfilter.ReplyDA1, 1, now)
		if typing, _ := z.splitReplies(p, "\x1b[?62;22c", now); typing {
			t.Fatal("an owed answer was typing")
		}
	}
	if typing, _ := z.splitReplies(p, "\x1b[1;2R", start.Add(8*time.Second)); !typing {
		t.Fatal("a cursor query 8 seconds old still swallowed Shift+F3")
	}
	asked := start.Add(9 * time.Second)
	p.askLocked(termfilter.ReplyCursor, 1, asked)
	if typing, _ := z.splitReplies(p, "\x1b[1;1R", asked.Add(terminalQueryLive-time.Millisecond)); typing {
		t.Fatal("an answer within terminalQueryLive was typing")
	}
	p.askLocked(termfilter.ReplyCursor, 1, asked)
	if typing, _ := z.splitReplies(p, "\x1b[1;2R", asked.Add(terminalQueryLive+time.Second)); !typing {
		t.Fatal("a query was owed past terminalQueryLive and a second")
	}
}

// Moving the pointer over a window is not typing: it never takes the size,
// and reaches the program only from the owner. A click does both.
func TestTerminalPointerMotionIsNotTyping(t *testing.T) {
	session, file := newRecordingSession(t)
	owner := session.openPeer("owner-identity-1", false)
	other := session.openPeer("other-identity-1", false)
	_ = session.peerResize(other, 100, 30)
	_ = session.peerResize(owner, 120, 40)
	_, _ = session.peerInput(owner, "x")
	// SGR, X10 and urxvt encodings of motion with no button held.
	for _, motion := range []string{"\x1b[<35;10;5M", "\x1b[MC*%", "\x1b[67;10;5M"} {
		if data, _ := session.peerInput(other, motion); data != "" {
			t.Fatalf("%q passed from a non-owner", motion)
		}
		if data, _ := session.peerInput(owner, motion); data != motion {
			t.Fatalf("%q did not pass from the owner", motion)
		}
	}
	if file.lastSize() != [2]uint16{120, 40} {
		t.Fatal("pointer motion took the size", file.sizes)
	}
	if data, _ := session.peerInput(other, "\x1b[<0;10;5M"); data != "\x1b[<0;10;5M" || file.lastSize() != [2]uint16{100, 30} {
		t.Fatal("a click did not pass or take the size", data, file.sizes)
	}
}
