package controlwire

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// A header value with a line break or another control character could add a
// header to the request: the client refuses it before it connects.
func TestHeaderValuesWithControlsAreRefused(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	good := "localhost:" + strconv.Itoa(port)
	ctx := context.Background()
	for _, bad := range []string{"gen\r\nX-Injected: 1", "gen\n", "gen\x00", "gen\x7f"} {
		c := &Client{origin: "http://" + good, host: good, addr: ln.Addr().String(), ConnectionID: bad}
		var network *url.Error
		if _, _, err := c.Do(ctx, "GET", "/api/x", nil, false, 0); !errors.As(err, &network) {
			t.Errorf("Do with connection %q: %v", bad, err)
		}
		c.ConnectionID = ""
		var agent *controlclient.AgentError
		if _, err := c.Dial(ctx, controlclient.AgentTarget{Scope: "selected", Connection: bad}, "/api/x"); !errors.As(err, &agent) || agent.Code != "invalid_input" {
			t.Errorf("Dial with connection %q: %v", bad, err)
		}
	}
	c := &Client{origin: "http://" + good, host: good + "\r\nX-Injected: 1", addr: ln.Addr().String()}
	if _, _, err := c.Do(ctx, "GET", "/api/x", nil, false, 0); err == nil {
		t.Error("Do with a host holding a line break")
	}
	ln.Close()
	if n := connections.Load(); n != 0 {
		t.Fatalf("%d connections made for refused requests", n)
	}
}
