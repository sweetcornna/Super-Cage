package workbench

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// guestVNC stands in for the guest's VNC server: it greets every connection
// with an RFB banner and reports what a client sends. It never uses the real
// port, which a running Cage on this machine may own.
type guestVNC struct {
	address  string
	received chan []byte
	closed   chan struct{}
}

func newGuestVNC(t *testing.T) *guestVNC {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &guestVNC{address: listener.Addr().String(), received: make(chan []byte, 16), closed: make(chan struct{}, 16)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "RFB 003.008\n")
				buffer := make([]byte, 4096)
				for {
					n, err := conn.Read(buffer)
					if n > 0 {
						g.received <- append([]byte(nil), buffer[:n]...)
					}
					if err != nil {
						g.closed <- struct{}{}
						return
					}
				}
			}()
		}
	}()
	previous := browserVNCAddress
	browserVNCAddress = g.address
	t.Cleanup(func() { browserVNCAddress = previous; listener.Close() })
	return g
}

type visibilityPost struct {
	Visible bool `json:"visible"`
	Held    int  `json:"held"`
}

// displayServer serves s.vnc over HTTP with a guest browser controller that
// records every visibility request and answers it with answer.
func displayServer(t *testing.T, s *Server, answer func() (*http.Response, error)) (*httptest.Server, chan visibilityPost) {
	t.Helper()
	posts := make(chan visibilityPost, 16)
	s.ready = true
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/visibility" {
			t.Errorf("unexpected guest request %s", r.URL.Path)
		}
		var post visibilityPost
		if err := json.NewDecoder(r.Body).Decode(&post); err != nil {
			t.Error(err)
		}
		posts <- post
		return answer()
	}))
	server := httptest.NewServer(http.HandlerFunc(s.vnc))
	s.origin = server.URL
	t.Cleanup(server.Close)
	return server, posts
}

func okAnswer() (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

func dialDisplay(t *testing.T, server *httptest.Server, protocols ...string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: protocols}
	ws, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{"Origin": {server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func readBanner(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil || string(data) != "RFB 003.008\n" {
		t.Fatalf("no picture: %q %v", data, err)
	}
	_ = ws.SetReadDeadline(time.Time{})
}

// A guest that cannot change its window state must not take the picture with
// it: the stream connects, and the failure is retried in the background.
func TestVNCConnectsWhenTheGuestVisibilityCallFails(t *testing.T) {
	newGuestVNC(t)
	s := testServer(t)
	shortDisplayRetry(&s.browserViews)
	server, posts := displayServer(t, s, func() (*http.Response, error) { return nil, errors.New("connection refused") })
	ws := dialDisplay(t, server)
	readBanner(t, ws)
	// It was tried, and tried again while the viewer is there.
	receive(t, posts, 3*time.Second, "the first visibility attempt")
	receive(t, posts, 3*time.Second, "a retry")
}

func TestVNCConnectsWhileTheGuestIsStillAnswering(t *testing.T) {
	newGuestVNC(t)
	s := testServer(t)
	unblock := make(chan struct{})
	server, posts := displayServer(t, s, func() (*http.Response, error) { <-unblock; return okAnswer() })
	t.Cleanup(func() { close(unblock) })
	ws := dialDisplay(t, server)
	readBanner(t, ws)
	if post := receive(t, posts, 3*time.Second, "the show"); !post.Visible {
		t.Fatal("the guest was told to hide")
	}
}

func TestVNCHiddenStreamStaysOpenUntilTheGraceEnds(t *testing.T) {
	g := newGuestVNC(t)
	previous := displayHeldGrace
	displayHeldGrace = 1200 * time.Millisecond
	t.Cleanup(func() { displayHeldGrace = previous })
	s := testServer(t)
	server, posts := displayServer(t, s, okAnswer)
	ws := dialDisplay(t, server, "binary", displayControlProtocol)
	if ws.Subprotocol() != displayControlProtocol {
		t.Fatalf("the server did not confirm display control: %q", ws.Subprotocol())
	}
	readBanner(t, ws)
	if post := receive(t, posts, 3*time.Second, "the show"); !post.Visible {
		t.Fatal("not shown")
	}
	// The pane is hidden: the guest is told to hide and how many streams the
	// client keeps open, and the stream itself carries on.
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"visibility","visible":false}`)); err != nil {
		t.Fatal(err)
	}
	if post := receive(t, posts, 3*time.Second, "the hide"); post.Visible || post.Held != 1 {
		t.Fatalf("hide request: %+v", post)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("input")); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, g.received, time.Second, "input while hidden"); string(got) != "input" {
		t.Fatalf("input while hidden: %q", got)
	}
	// Past the grace period the server closes it with its own close code.
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := ws.ReadMessage()
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != displayHeldCloseCode {
		t.Fatalf("hidden stream was not closed with its code: %v", err)
	}
	receive(t, g.closed, 3*time.Second, "the guest connection to close")
}

func TestVNCReturningViewerShowsTheGuestAgainAndKeepsTheStream(t *testing.T) {
	g := newGuestVNC(t)
	previous := displayHeldGrace
	displayHeldGrace = 1500 * time.Millisecond
	t.Cleanup(func() { displayHeldGrace = previous })
	s := testServer(t)
	server, posts := displayServer(t, s, okAnswer)
	ws := dialDisplay(t, server, "binary", displayControlProtocol)
	readBanner(t, ws)
	receive(t, posts, 3*time.Second, "the show")
	send := func(visible string) {
		t.Helper()
		if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"visibility","visible":`+visible+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	send("false")
	if post := receive(t, posts, 3*time.Second, "the hide"); post.Visible {
		t.Fatal("not hidden")
	}
	send("true")
	if post := receive(t, posts, 3*time.Second, "the show on return"); !post.Visible {
		t.Fatal("returning did not show the guest")
	}
	// Longer than the grace: showing must have cancelled it.
	time.Sleep(1700 * time.Millisecond)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("still here")); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, g.received, time.Second, "input after returning"); string(got) != "still here" {
		t.Fatalf("input after returning: %q", got)
	}
	// A repeated hide message does not extend the grace period.
	send("false")
	receive(t, posts, 3*time.Second, "the second hide")
	time.Sleep(900 * time.Millisecond)
	send("false")
	// The grace ends 1.5 s after the first hide, about 0.1 s from now. Had the
	// repeat restarted it, the stream would live until about 2.9 s.
	_ = ws.SetReadDeadline(time.Now().Add(1000 * time.Millisecond))
	_, _, err := ws.ReadMessage()
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != displayHeldCloseCode {
		t.Fatalf("a repeated hide message kept the stream open past its grace: %v", err)
	}
}

func TestVNCMalformedControlFramesAreIgnored(t *testing.T) {
	newGuestVNC(t)
	s := testServer(t)
	server, posts := displayServer(t, s, okAnswer)
	ws := dialDisplay(t, server, "binary", displayControlProtocol)
	readBanner(t, ws)
	receive(t, posts, 3*time.Second, "the show")
	// Garbage control frames are ignored.
	for _, junk := range []string{`{"type":"other"}`, `not json`, `{"type":"visibility"}`, `{"type":"visibility","visible":"no"}`} {
		if err := ws.WriteMessage(websocket.TextMessage, []byte(junk)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case post := <-posts:
		t.Fatalf("a malformed control frame changed the guest: %+v", post)
	case <-time.After(700 * time.Millisecond):
	}
}

// A client that does not offer display control, like every client before
// this one, gets the stream exactly as before. One that offers the plain
// stream gets it selected, as Chrome requires of a handshake that offered
// subprotocols; one that offers none gets none.
func TestVNCClientWithoutDisplayControlIsUnchanged(t *testing.T) {
	newGuestVNC(t)
	s := testServer(t)
	server, posts := displayServer(t, s, okAnswer)
	if ws := dialDisplay(t, server); ws.Subprotocol() != "" {
		t.Fatalf("subprotocol %q selected for a client that offered none", ws.Subprotocol())
	} else {
		readBanner(t, ws)
		receive(t, posts, 3*time.Second, "the show")
		ws.Close()
		receive(t, posts, 3*time.Second, "the hide after disconnect")
	}
	ws := dialDisplay(t, server, "binary")
	if ws.Subprotocol() != "binary" {
		t.Fatalf("subprotocol %q selected for a client that offered only binary", ws.Subprotocol())
	}
	readBanner(t, ws)
	if post := receive(t, posts, 3*time.Second, "the show"); !post.Visible {
		t.Fatal("not shown")
	}
	ws.Close()
	if post := receive(t, posts, 3*time.Second, "the hide after disconnect"); post.Visible || post.Held != 0 {
		t.Fatalf("hide after disconnect: %+v", post)
	}
}
