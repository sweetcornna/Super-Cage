package workbench

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestSplitTerminalReplies(t *testing.T) {
	for _, row := range []struct {
		in, rest    string
		cursorAsked bool
		typing      bool
	}{
		{in: "ls\r", rest: "ls\r", typing: true},
		{in: "\x1b[A", rest: "\x1b[A", typing: true},                                             // arrow up
		{in: "\x1bOP", rest: "\x1bOP", typing: true},                                             // F1
		{in: "\x1b", rest: "\x1b", typing: true},                                                 // Escape key
		{in: "\x1bx", rest: "\x1bx", typing: true},                                               // Alt+x
		{in: "\x1b[<0;10;5M", rest: "\x1b[<0;10;5M", typing: true},                               // mouse press
		{in: "\x1b[97;5u", rest: "\x1b[97;5u", typing: true},                                     // kitty key
		{in: "\x1b[?1;2c", rest: ""},                                                             // DA1 answer
		{in: "\x1b[>0;276;0c", rest: ""},                                                         // DA2 answer
		{in: "\x1b[0n", rest: ""},                                                                // status report
		{in: "\x1b[?2004;1$y", rest: ""},                                                         // DECRPM
		{in: "\x1b[?15u", rest: ""},                                                              // kitty keyboard flags
		{in: "\x1b[8;40;120t", rest: ""},                                                         // size report
		{in: "\x1b[I", rest: ""},                                                                 // focus in
		{in: "\x1b]11;rgb:0000/0000/0000\x1b\\", rest: ""},                                       // OSC 11 answer
		{in: "\x1b]10;rgb:ffff/ffff/ffff\x07", rest: ""},                                         // OSC 10 answer, BEL
		{in: "\x1bP1$r0m\x1b\\", rest: ""},                                                       // DECRQSS answer
		{in: "\x1bP>|xterm(388)\x1b\\", rest: ""},                                                // XTVERSION answer
		{in: "\x1b_Gi=1;OK\x1b\\", rest: ""},                                                     // kitty graphics answer
		{in: "\x1b[12;40R", rest: "", cursorAsked: true},                                         // cursor position answer
		{in: "\x1b[1;2R", rest: "\x1b[1;2R", typing: true},                                       // Shift+F3: nothing asked
		{in: "\x1b[?1;2ca", rest: "a", typing: true},                                             // answer and a key together
		{in: "\x1b[200~\x1b[?1;2c\x1b[201~", rest: "\x1b[200~\x1b[?1;2c\x1b[201~", typing: true}, // pasted text
	} {
		typing, rest := splitTerminalReplies(row.in, row.cursorAsked)
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
