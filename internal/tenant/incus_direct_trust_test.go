package tenant

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/vsockrelay"
)

// recordingConn keeps what the gateway wrote to a listener.
type recordingConn struct {
	net.Conn
	mu      *sync.Mutex
	written *bytes.Buffer
}

func (c *recordingConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	c.written.Write(data)
	c.mu.Unlock()
	return c.Conn.Write(data)
}

// Something else in the VM listens on the bridge's vsock port and answers
// HTTP like any server: the gateway must send it nothing, neither a probe nor
// the launcher token, and must not use it.
func TestIncusDirectPathSendsNothingToAListenerItDidNotStart(t *testing.T) {
	previous := vsockrelay.HandshakeTimeout
	vsockrelay.HandshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { vsockrelay.HandshakeTimeout = previous })
	f := newDirectFixture(t, true)
	foreign := serveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	var mu sync.Mutex
	var written bytes.Buffer
	f.runtime.vsockDial = func(ctx context.Context, cid, port uint32) (net.Conn, error) {
		conn, err := foreign.dial(ctx)
		if err != nil {
			return nil, err
		}
		return &recordingConn{Conn: conn, mu: &mu, written: &written}, nil
	}
	workspace := f.connect()
	requests(t, workspace, 2)
	mu.Lock()
	sent := written.String()
	mu.Unlock()
	if sent != "" {
		t.Fatalf("the gateway sent %d bytes to a listener it did not start: %q", len(sent), sent)
	}
	if f.execs.Load() == 0 || f.runtime.directAllowed(f.instance) {
		t.Fatalf("exec dials %d, direct allowed %v: want the workspace on exec and the instance skipped", f.execs.Load(), f.runtime.directAllowed(f.instance))
	}
}
