package workbench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedInputTerminal is a PTY whose input side does not read until it is
// opened, like a guest program that is busy or an SSH window that is full. It
// records what it is finally given.
type gatedInputTerminal struct {
	*controlledTerminal
	gate      chan struct{}
	started   chan struct{} // closed when the first write begins
	closed    chan struct{}
	startOnce sync.Once
	openOnce  sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	written   bytes.Buffer
}

func newGatedInputSession(t *testing.T) (*terminalSession, *gatedInputTerminal) {
	t.Helper()
	reader, writer := io.Pipe()
	file := &gatedInputTerminal{
		controlledTerminal: &controlledTerminal{reader: reader, writer: writer, killed: make(chan struct{}), waitStarted: make(chan struct{}), waitRelease: make(chan struct{})},
		gate:               make(chan struct{}), started: make(chan struct{}), closed: make(chan struct{}),
	}
	session := newTerminalSession(file)
	go session.run()
	t.Cleanup(func() {
		file.open()
		session.close()
	})
	return session, file
}

func (f *gatedInputTerminal) Write(data []byte) (int, error) {
	f.startOnce.Do(func() { close(f.started) })
	select {
	case <-f.gate:
	case <-f.closed:
		return 0, os.ErrClosed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.Write(data)
}

func (f *gatedInputTerminal) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return f.controlledTerminal.Close()
}

// Wait does not hold the session open: the tests end sessions by closing them.
func (f *gatedInputTerminal) Wait() error { return nil }

func (f *gatedInputTerminal) open() { f.openOnce.Do(func() { close(f.gate) }) }

func (f *gatedInputTerminal) received() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.String()
}

// awaitInput waits until the PTY has been given exactly want, in order.
func (f *gatedInputTerminal) awaitInput(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.received(); len(got) >= len(want) {
			if got != want {
				t.Fatalf("PTY input differs: got %d bytes, want %d bytes", len(got), len(want))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PTY received %d of %d input bytes", len(f.received()), len(want))
}

func setTerminalInputBudget(t *testing.T, budget int) {
	t.Helper()
	previous := terminalInputBudget
	terminalInputBudget = budget
	t.Cleanup(func() { terminalInputBudget = previous })
}

// awaitInputWaiter waits until a stream handler is parked for room in the
// session's input queue.
func awaitInputWaiter(t *testing.T, session *terminalSession) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		session.mu.Lock()
		waiting := session.inputSpace != nil
		session.mu.Unlock()
		if waiting {
			return
		}
	}
	t.Fatal("the stream did not wait for room in the input queue")
}

func awaitPong(t *testing.T, ws interface {
	ReadJSON(any) error
}) {
	t.Helper()
	for {
		var state struct {
			Type string `json:"type"`
		}
		if err := ws.ReadJSON(&state); err != nil {
			t.Fatalf("viewer was dropped: %v", err)
		}
		if state.Type == "pong" {
			return
		}
	}
}

// Typing or pasting while the guest does not read must queue, in order, and
// reach the program once it reads. It must not be refused after a few messages.
func TestTerminalInputQueuesInOrderWhilePTYDoesNotRead(t *testing.T) {
	session, file := newGatedInputSession(t)
	var want strings.Builder
	if err := session.enqueueInput(context.Background(), "first;"); err != nil {
		t.Fatal(err)
	}
	want.WriteString("first;")
	awaitTerminalSignal(t, file.started)
	for i := 0; i < 200; i++ {
		part := fmt.Sprintf("m%d;", i)
		if err := session.enqueueInput(context.Background(), part); err != nil {
			t.Fatalf("message %d was refused while the PTY was not reading: %v", i, err)
		}
		want.WriteString(part)
	}
	file.open()
	file.awaitInput(t, want.String())
}

// The queue is bounded by bytes, not by message count: many small messages
// fit, an overflowing one is refused without disturbing what is queued, and a
// message larger than the whole budget is still admitted into an empty queue.
func TestTerminalInputBudgetCountsBytes(t *testing.T) {
	setTerminalInputBudget(t, 4096)
	session, file := newGatedInputSession(t)
	ctx := context.Background()
	if err := session.enqueueInput(ctx, "head;"); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, file.started) // the writer holds it; the queue is empty
	want := "head;"
	chunk := strings.Repeat("x", 1000)
	for i := 0; i < 3; i++ {
		if err := session.enqueueInput(ctx, chunk); err != nil {
			t.Fatalf("chunk %d within the budget was refused: %v", i, err)
		}
		want += chunk
	}
	if err := session.enqueueInput(ctx, chunk); err != errTerminalInputFull {
		t.Fatalf("an overflowing chunk was not refused: %v", err)
	}
	// Small messages cost their bytes plus a fixed overhead, so they cannot
	// grow the queue past the budget either.
	refused := false
	for i := 0; i < 4096 && !refused; i++ {
		if err := session.enqueueInput(ctx, "k"); err == nil {
			want += "k"
		} else if err == errTerminalInputFull {
			refused = true
		} else {
			t.Fatal(err)
		}
	}
	if !refused {
		t.Fatal("tiny messages grew the queue without bound")
	}
	file.open()
	file.awaitInput(t, want)
	// Once drained, a message bigger than the budget is admitted.
	big := strings.Repeat("b", 3*terminalInputBudget)
	if err := session.enqueueInput(ctx, big); err != nil {
		t.Fatalf("a large message into an empty queue was refused: %v", err)
	}
	file.awaitInput(t, want+big)
}

// A viewer that types fast into a stalled guest stays attached and loses
// nothing: every message arrives, in order, once the guest reads again.
func TestTerminalViewerKeepsInputWhilePTYDoesNotRead(t *testing.T) {
	s := testServer(t)
	session, file := newGatedInputSession(t)
	session.id = "stalled"
	s.terminals.sessions = map[string]*terminalSession{session.id: session}
	t.Cleanup(s.terminals.close)
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, session.id)
	var want strings.Builder
	send := func(part string) {
		t.Helper()
		if err := ws.WriteJSON(map[string]string{"type": "input", "data": part}); err != nil {
			t.Fatal(err)
		}
		want.WriteString(part)
	}
	send("first;")
	awaitTerminalSignal(t, file.started)
	for i := 0; i < 40; i++ {
		send(fmt.Sprintf("m%d;", i))
	}
	// The viewer is still served: a ping is answered.
	if err := ws.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	awaitPong(t, ws)
	file.open()
	file.awaitInput(t, want.String())
	_ = ws.Close()
}

// A paste is one input message. A multi-megabyte one must reach the program
// whole instead of closing the stream for exceeding a frame limit.
func TestTerminalViewerPasteOfSeveralMegabytesArrivesWhole(t *testing.T) {
	s := testServer(t)
	session, file := newGatedInputSession(t)
	session.id = "paste"
	s.terminals.sessions = map[string]*terminalSession{session.id: session}
	t.Cleanup(s.terminals.close)
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, session.id)
	paste := strings.Repeat("paste line of text\n", (3<<20)/19)
	if err := ws.WriteJSON(map[string]string{"type": "input", "data": paste}); err != nil {
		t.Fatalf("sending the paste: %v", err)
	}
	file.open()
	file.awaitInput(t, paste)
	if err := ws.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	awaitPong(t, ws)
	_ = ws.Close()
}

// With the queue over its byte budget the stream holds the viewer back
// instead of refusing its input: nothing is lost, order is kept, and the
// viewer is still attached and answered afterwards.
func TestTerminalStreamWaitsForInputRoom(t *testing.T) {
	setTerminalInputBudget(t, 2048)
	s := testServer(t)
	session, file := newGatedInputSession(t)
	session.id = "full"
	s.terminals.sessions = map[string]*terminalSession{session.id: session}
	t.Cleanup(s.terminals.close)
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, session.id)
	var want strings.Builder
	send := func(part string) {
		t.Helper()
		if err := ws.WriteJSON(map[string]string{"type": "input", "data": part}); err != nil {
			t.Fatal(err)
		}
		want.WriteString(part)
	}
	send("first;")
	awaitTerminalSignal(t, file.started)
	for i := 0; i < 30; i++ {
		send(fmt.Sprintf("%02d%s;", i, strings.Repeat("p", 500)))
	}
	// The handler is parked on a full queue, not dropping or refusing.
	awaitInputWaiter(t, session)
	session.mu.Lock()
	queued := session.inputBytes
	session.mu.Unlock()
	if queued > terminalInputBudget {
		t.Fatalf("queue grew past its budget: %d > %d", queued, terminalInputBudget)
	}
	file.open()
	file.awaitInput(t, want.String())
	if err := ws.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	awaitPong(t, ws)
	_ = ws.Close()
}

// A viewer parked on a full queue is released by whatever ends it: the viewer
// leaving, the account being locked, or the session ending.
func TestTerminalStreamWaitForInputRoomEnds(t *testing.T) {
	for _, action := range []string{"disconnect", "account-lock", "session-close"} {
		t.Run(action, func(t *testing.T) {
			setTerminalInputBudget(t, 1)
			previous := terminalHeartbeatInterval
			terminalHeartbeatInterval = 50 * time.Millisecond
			t.Cleanup(func() { terminalHeartbeatInterval = previous })
			s := testServer(t)
			session, file := newGatedInputSession(t)
			session.id = "parked"
			s.terminals.sessions = map[string]*terminalSession{session.id: session}
			t.Cleanup(s.terminals.close)
			finished := make(chan struct{})
			handler := s.handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			s.origin = server.URL
			ws := connectTerminalViewer(t, s, server, session.id)
			for _, part := range []string{"blocked", "queued", "waiting"} {
				if err := ws.WriteJSON(map[string]string{"type": "input", "data": part}); err != nil {
					t.Fatal(err)
				}
				if part == "blocked" {
					awaitTerminalSignal(t, file.started)
				}
			}
			awaitInputWaiter(t, session)
			switch action {
			case "disconnect":
				_ = ws.Close()
			case "account-lock":
				s.account.lock()
			case "session-close":
				go session.close()
			}
			awaitTerminalSignal(t, finished)
			session.mu.Lock()
			clients := len(session.clients)
			session.mu.Unlock()
			if clients != 0 {
				t.Fatal("a released viewer stayed attached")
			}
		})
	}
}
