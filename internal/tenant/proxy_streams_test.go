package tenant

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// openSockets dials count WebSockets to the workspace through the gateway and
// returns them. They stay open until the test ends.
type streamClient struct {
	t        *testing.T
	fixture  *portalFixture
	id       string
	cookie   *http.Cookie
	listener *pipeListener
}

func newStreamClient(t *testing.T) *streamClient {
	t.Helper()
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)
	return &streamClient{t: t, fixture: fixture, id: record.ID, cookie: cookie, listener: serveFixture(t, fixture.server)}
}

// socket opens one WebSocket and waits for the guest's first message. A
// refused handshake returns the HTTP status.
func (c *streamClient) socket() (*websocket.Conn, int) {
	c.t.Helper()
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.listener.dial(ctx)
	}, HandshakeTimeout: 3 * time.Second}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(vmOrigin(c.id), "https")+"/socket", http.Header{
		"Origin": {vmOrigin(c.id)}, "Cookie": {c.cookie.Name + "=" + c.cookie.Value},
	})
	if err != nil {
		if response != nil {
			return nil, response.StatusCode
		}
		c.t.Fatalf("WebSocket handshake failed: %v", err)
	}
	c.t.Cleanup(func() { conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		c.t.Fatalf("WebSocket did not reach the guest: %v", err)
	}
	return conn, http.StatusSwitchingProtocols
}

// events opens one server-sent event stream and waits for its first event.
func (c *streamClient) events(ctx context.Context) (io.Closer, int) {
	c.t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.listener.dial(ctx)
	}, DisableKeepAlives: true}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, vmOrigin(c.id)+"/stream", nil)
	if err != nil {
		c.t.Fatal(err)
	}
	request.URL.Scheme = "http"
	request.Header.Set("Accept", "text/event-stream")
	request.AddCookie(c.cookie)
	response, err := client.Do(request)
	if err != nil {
		c.t.Fatalf("event stream request failed: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, response.StatusCode
	}
	buffer := make([]byte, 64)
	if _, err := response.Body.Read(buffer); err != nil {
		c.t.Fatalf("event stream did not reach the guest: %v", err)
	}
	return response.Body, response.StatusCode
}

// get is an ordinary workbench request, answered within a deadline: a request
// that waits for a guest connection that never frees is as bad as a 429.
func (c *streamClient) get(path string) int {
	c.t.Helper()
	done := make(chan int, 1)
	go func() {
		response := c.fixture.request("GET", vmOrigin(c.id), path, "", c.cookie)
		done <- response.Code
	}()
	select {
	case status := <-done:
		return status
	case <-time.After(5 * time.Second):
		c.t.Fatalf("GET %s did not finish: the request waits behind open streams", path)
		return 0
	}
}

func TestTenantOpenWebSocketsDoNotUseTheRequestBudget(t *testing.T) {
	c := newStreamClient(t)
	for range maxRequestSlots {
		if _, status := c.socket(); status != http.StatusSwitchingProtocols {
			t.Fatalf("WebSocket refused with HTTP %d", status)
		}
	}
	if status := c.get("/"); status != http.StatusOK {
		t.Fatalf("GET with %d WebSockets open: HTTP %d, want 200", maxRequestSlots, status)
	}
	if status := c.get("/api/client/about"); status != http.StatusOK {
		t.Fatalf("status poll with %d WebSockets open: HTTP %d, want 200", maxRequestSlots, status)
	}
}

func TestTenantStreamBudgetIsLargerAndIndependent(t *testing.T) {
	if maxStreamSlots <= maxRequestSlots {
		t.Fatalf("stream budget %d must exceed the request budget %d", maxStreamSlots, maxRequestSlots)
	}
	c := newStreamClient(t)
	var sockets []*websocket.Conn
	for range maxStreamSlots {
		conn, status := c.socket()
		if status != http.StatusSwitchingProtocols {
			t.Fatalf("WebSocket %d refused with HTTP %d", len(sockets)+1, status)
		}
		sockets = append(sockets, conn)
	}
	if _, status := c.socket(); status != http.StatusTooManyRequests {
		t.Fatalf("WebSocket beyond the budget: HTTP %d, want 429", status)
	}
	if status := c.get("/"); status != http.StatusOK {
		t.Fatalf("GET with a full stream budget: HTTP %d, want 200", status)
	}
	// Ending one stream frees its place.
	sockets[0].Close()
	waitFixture(t, func() bool {
		conn, status := c.socket()
		if conn != nil {
			conn.Close()
		}
		return status == http.StatusSwitchingProtocols
	})
}

func TestTenantEventStreamsUseTheStreamBudget(t *testing.T) {
	c := newStreamClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for range maxRequestSlots {
		if _, status := c.events(ctx); status != http.StatusOK {
			t.Fatalf("event stream refused with HTTP %d", status)
		}
	}
	if status := c.get("/"); status != http.StatusOK {
		t.Fatalf("GET with %d event streams open: HTTP %d, want 200", maxRequestSlots, status)
	}
}
