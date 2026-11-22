//go:build !windows

package workbench

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/controlwire"
)

// entryPausable is an ssh client's transport that stops reading while its
// gate is held, as a suspended ssh (~^Z) does: its proxy then stops reading
// the WebSocket, and the endpoint's writes wait.
type entryPausable struct {
	net.Conn
	gate sync.RWMutex
}

func (p *entryPausable) Read(b []byte) (int, error) {
	p.gate.RLock()
	p.gate.RUnlock()
	return p.Conn.Read(b)
}

// R3-4: a client that stops reading is a hang-up once a write to it waited
// entryWriteTimeout: the connection ends and its terminal is kept, under the
// kept rules (within entryKeptLimit, with the idle check); with no room left
// the terminal ends, as on any hang-up.
func TestEntryStalledClientKeepsItsTerminal(t *testing.T) {
	previous := entryWriteTimeout
	entryWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { entryWriteTimeout = previous })
	for _, c := range []struct {
		name  string
		limit int
		kept  bool
	}{{"kept", 4, true}, {"no room", 0, false}} {
		t.Run(c.name, func(t *testing.T) {
			previousLimit := entryKeptLimit
			entryKeptLimit = c.limit
			t.Cleanup(func() { entryKeptLimit = previousLimit })
			f := newEntryFixture(t, true)
			f.on()
			t.Cleanup(func() {
				for _, session := range f.s.terminals.keptSessions() {
					f.s.terminals.remove(session.id)
				}
				f.s.entryUnkept()
			})
			ended := make(chan *entryConn, 1)
			f.s.entry.registry.mu.Lock()
			f.s.entry.registry.observe = func(c *entryConn) { ended <- c }
			f.s.entry.registry.mu.Unlock()
			var pause *entryPausable
			client, err := f.dialThrough(f.device, func(ws *controlwire.Conn, conn net.Conn) net.Conn {
				// A small receive buffer, so that the way to the client fills
				// before the client's ssh window does.
				if tcp, ok := ws.NetConn().(*net.TCPConn); ok {
					_ = tcp.SetReadBuffer(16 << 10)
				}
				pause = &entryPausable{Conn: conn}
				return pause
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := session.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
				t.Fatal(err)
			}
			stdin, _ := session.StdinPipe()
			stdout, _ := session.StdoutPipe()
			if err := session.Shell(); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, "head -c 50000000 /dev/zero | tr '\\0' x; exec sleep 60\n"); err != nil {
				t.Fatal(err)
			}
			// Output flows while the client reads, then the client stops.
			var seen int
			buffer := make([]byte, 32<<10)
			for seen < 256<<10 {
				n, err := stdout.Read(buffer)
				if err != nil {
					t.Fatalf("after %d bytes: %v", seen, err)
				}
				seen += n
			}
			pause.gate.Lock()
			stopped := time.Now()
			var conn *entryConn
			select {
			case conn = <-ended:
			case <-time.After(10 * time.Second):
				pause.gate.Unlock()
				t.Fatal("the connection did not end while the client read nothing")
			}
			took := time.Since(stopped)
			if !conn.stalled.Load() || !errors.Is(context.Cause(conn.ctx), errEntryStalled) {
				t.Fatalf("the connection ended with %v, stalled %v", context.Cause(conn.ctx), conn.stalled.Load())
			}
			if c.kept {
				sessions := f.s.terminals.keptSessions()
				if len(sessions) != 1 || sessions[0].isClosed() || !f.s.entryKeptTimerRunning() {
					t.Fatalf("kept %d, idle check %v", len(sessions), f.s.entryKeptTimerRunning())
				}
			} else {
				entryAwait(t, 5*time.Second, "the terminal to end", func() bool { return len(entrySSHTerminals(f.s)) == 0 })
				if f.s.terminals.keptCount() != 0 || f.s.entryKeptTimerRunning() {
					t.Fatalf("kept %d, idle check %v", f.s.terminals.keptCount(), f.s.entryKeptTimerRunning())
				}
			}
			// The client, once it reads again, finds the connection gone.
			pause.gate.Unlock()
			waited := make(chan error, 1)
			go func() { waited <- client.Wait() }()
			select {
			case <-waited:
			case <-time.After(10 * time.Second):
				t.Fatal("the client's connection did not end")
			}
			t.Logf("%s: the connection ended %v after the client stopped reading (write timeout %v)", c.name, took.Round(10*time.Millisecond), entryWriteTimeout)
		})
	}
}
