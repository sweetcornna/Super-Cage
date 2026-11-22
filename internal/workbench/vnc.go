package workbench

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// browserVNCAddress is where the guest's browser display listens, seen from
// the host. A variable so that tests never touch a real guest's port.
var browserVNCAddress = "127.0.0.1:15900"

func (s *Server) vnc(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		http.Error(w, "环境尚未就绪", 409)
		return
	}
	if s.macosGuest != nil || s.windowsGuest != nil {
		s.nativeBrowserVNC(w, r)
		return
	}
	// The guest's window state follows in the background. The picture never
	// waits for it, and a failed change never refuses the picture.
	viewer := s.joinBrowserView()
	defer viewer.close()
	address := browserVNCAddress
	if containerenv.Configured(s.profile) {
		environment, err := containerenv.Load(s.profile)
		if err != nil {
			requestError(w, err)
			return
		}
		state, err := environment.State(r.Context())
		if err != nil || !state.Ready {
			http.Error(w, "容器画面连接尚未就绪", 409)
			return
		}
		address = state.VNCAddress
	}
	remote, e := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(r.Context(), "tcp", address)
	if e != nil {
		http.Error(w, "画面连接暂时不可用", 502)
		return
	}
	defer remote.Close()
	s.streamDisplay(w, r, remote, remote, func() { _ = remote.Close() }, viewer)
}

func (s *Server) streamVNC(w http.ResponseWriter, r *http.Request, reader io.Reader, writer io.Writer, closeTransport func()) {
	s.streamDisplay(w, r, reader, writer, closeTransport, nil)
}

// A display stream that has a viewer accepts display control messages in
// WebSocket text frames, next to the RFB bytes in binary frames:
//
//	{"type":"visibility","visible":false}   the pane is hidden; keep the stream
//	{"type":"visibility","visible":true}    the pane is shown again
//
// The client may send them when the server selected the WebSocket subprotocol
// displayControlProtocol; a server that did not select it ignores text frames,
// so the client must then close the stream when its pane is hidden, as before.
// A stream is born shown. While it is hidden it demands nothing from the
// guest, which can then pause the window. A stream hidden for longer than
// displayHeldGrace is closed with displayHeldCloseCode, and the client
// connects again when its pane is next shown. The server sends no text frame.
const (
	displayControlProtocol = "cage-display.v1"
	displayHeldCloseCode   = 4001
)

var displayHeldGrace = 60 * time.Second

// displayStreamProtocol is the plain RFB stream, which every display path
// serves; clients offer it next to displayControlProtocol.
const displayStreamProtocol = "binary"

// upgradeDisplay selects a subprotocol whenever the client offers one it
// knows: Chrome fails a handshake that offered subprotocols and got none
// back. A stream with a viewer prefers displayControlProtocol (it holds a
// hidden stream); every stream takes displayStreamProtocol otherwise, the
// native macOS and Windows displays included.
func (s *Server) upgradeDisplay(w http.ResponseWriter, r *http.Request, control bool) (*websocket.Conn, error) {
	protocols := []string{displayStreamProtocol}
	if control {
		protocols = []string{displayControlProtocol, displayStreamProtocol}
	}
	// The same origin rule and buffers as upgrade, plus the subprotocols.
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == s.origin }, ReadBufferSize: 16384, WriteBufferSize: 16384, Subprotocols: protocols}
	ws, err := up.Upgrade(w, r, nil)
	if err == nil {
		context.AfterFunc(r.Context(), func() { _ = ws.Close() })
	}
	return ws, err
}

func (s *Server) streamDisplay(w http.ResponseWriter, r *http.Request, reader io.Reader, writer io.Writer, closeTransport func(), viewer *displayViewer) {
	var once sync.Once
	closeRemote := func() { once.Do(closeTransport) }
	defer closeRemote()
	ws, e := s.upgradeDisplay(w, r, viewer != nil)
	if e != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(4 << 20)
	closeStreams := func() { _ = ws.Close(); closeRemote() }
	stop := context.AfterFunc(r.Context(), closeStreams)
	done := make(chan struct{})
	// A menu capture can briefly pause the guest while pointer events continue.
	// Bound both count and bytes; four tiny mouse packets must not tear down a
	// healthy display. No protocol bytes are discarded or reordered.
	input := make(chan []byte, 64)
	var queuedBytes atomic.Int64
	written := make(chan struct{})
	defer func() {
		stop()
		closeStreams()
		close(input)
		<-done
		<-written
	}()
	// Keep reading WebSocket close/control frames even while the guest stops
	// accepting input. Overflow closes this display connection without dropping
	// bytes into an otherwise live RFB stream.
	go func() {
		defer close(written)
		defer closeStreams()
		for data := range input {
			if _, err := writer.Write(data); err != nil {
				return
			}
			queuedBytes.Add(-int64(len(data)))
		}
	}()
	go func() {
		defer close(done)
		defer ws.Close()
		b := make([]byte, 64<<10)
		for {
			n, e := reader.Read(b)
			if n > 0 {
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteMessage(websocket.BinaryMessage, b[:n]) != nil {
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	var hidden *time.Timer
	defer func() {
		if hidden != nil {
			hidden.Stop()
		}
	}()
	for {
		kind, b, e := ws.ReadMessage()
		if e != nil {
			return
		}
		if kind == websocket.TextMessage && viewer != nil && len(b) <= 256 {
			var control struct {
				Type    string `json:"type"`
				Visible *bool  `json:"visible"`
			}
			if json.Unmarshal(b, &control) != nil || control.Type != "visibility" || control.Visible == nil {
				continue
			}
			viewer.setVisible(*control.Visible)
			if *control.Visible && hidden != nil {
				hidden.Stop()
				hidden = nil
			}
			// The grace runs from the first hide message, not from a repeat.
			if !*control.Visible && hidden == nil {
				hidden = time.AfterFunc(displayHeldGrace, func() {
					_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(displayHeldCloseCode, "hidden-timeout"), time.Now().Add(time.Second))
					closeStreams()
				})
			}
			continue
		}
		if kind == websocket.BinaryMessage {
			if queuedBytes.Add(int64(len(b))) > 8<<20 {
				diagnostics.Log("warn", "display-transport", "input byte limit reached")
				return
			}
			// Allow short input bursts to drain. A stalled guest still has a
			// bounded queue and cannot hide a client disconnect indefinitely.
			timer := time.NewTimer(time.Second)
			select {
			case input <- b:
				timer.Stop()
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
				diagnostics.Log("warn", "display-transport", "input stalled: bounded queue did not drain")
				return
			}
		}
	}
}
