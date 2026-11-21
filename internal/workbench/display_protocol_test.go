package workbench

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// displayTestFrame is the smallest frame a native guest answers with.
const displayTestFrame = `{"id":"t","document":"d","width":1,"height":1,"target":"window"}`

// The test binary stands in for the Windows relay's browser and desktop
// connections: every request line is answered with one frame.
func init() {
	if os.Getenv("CAGE_DISPLAY_RELAY_TEST") != "1" {
		return
	}
	if len(os.Args) != 4 || os.Args[1] != "--relay-profile" || (os.Args[3] != "browser" && os.Args[3] != "desktop") {
		os.Exit(2)
	}
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		fmt.Println(displayTestFrame)
	}
	os.Exit(0)
}

// displayOffers are the subprotocol offers a display client makes: the
// current one, one that knows only the plain stream, and an older one.
var displayOffers = []struct {
	name   string
	offer  []string
	holdOK string // selected where the hold is supported
	plain  string // selected where it is not
}{
	{"binary and cage-display.v1", []string{"binary", displayControlProtocol}, displayControlProtocol, "binary"},
	{"binary only", []string{"binary"}, "binary", "binary"},
	{"none (older client)", nil, "", ""},
}

// handshakeProtocol dials the display handler and returns the subprotocol
// its handshake selected. A client that offered some and got none back is
// refused by Chrome; gorilla's dialer accepts it, so the test checks it.
func handshakeProtocol(t *testing.T, handler http.HandlerFunc, s *Server, offer []string) string {
	t.Helper()
	// A WebSocket handler outlives the server's Close (the connection is
	// hijacked); wait for it before the profile is removed.
	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		finished := make(chan struct{})
		go func() { handlers.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the display handler did not return after its client closed")
		}
	})
	s.origin = server.URL
	dialer := websocket.Dialer{Subprotocols: offer}
	ws, response, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{"Origin": {server.URL}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("handshake failed (status %d): %v", status, err)
	}
	defer ws.Close()
	return ws.Subprotocol()
}

func checkDisplayOffers(t *testing.T, hold bool, connect func(t *testing.T) (*Server, http.HandlerFunc)) {
	for _, tc := range displayOffers {
		t.Run(tc.name, func(t *testing.T) {
			s, handler := connect(t)
			want := tc.plain
			if hold {
				want = tc.holdOK
			}
			if got := handshakeProtocol(t, handler, s, tc.offer); got != want {
				t.Fatalf("offered %q, selected %q, want %q", tc.offer, got, want)
			}
		})
	}
}

func TestLinuxBrowserDisplaySelectsASubprotocol(t *testing.T) {
	checkDisplayOffers(t, true, func(t *testing.T) (*Server, http.HandlerFunc) {
		newGuestVNC(t)
		s := testServer(t)
		server, _ := displayServer(t, s, okAnswer)
		server.Close() // Only its guest controller is used.
		return s, s.vnc
	})
}

func TestLinuxClaudeDesktopDisplaySelectsASubprotocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Linux guest's display runs over the host's OpenSSH client")
	}
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("no ssh client to start")
	}
	checkDisplayOffers(t, true, func(t *testing.T) (*Server, http.HandlerFunc) {
		s := testServer(t)
		// A running environment whose SSH configuration does not exist: the
		// client starts and fails at once, after the display has connected.
		dir := s.instanceDir()
		if err := writePrivate(filepath.Join(dir, "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		return s, s.claudeDesktopVNC
	})
}

func TestMacOSBrowserDisplaySelectsASubprotocol(t *testing.T) {
	checkDisplayOffers(t, false, func(t *testing.T) (*Server, http.HandlerFunc) {
		s := testServer(t)
		s.ready = true
		s.macosGuest = &guestos.MacOSProfile{Instance: cage.InstanceName(s.profile, s.environmentDir())}
		s.windowsBrowserControl = scriptedBrowserClient(t, displayTestFrame)
		s.windowsBrowserControl.ownsBrowser = true
		return s, s.vnc
	})
}

func bindDisplayRelay(t *testing.T, s *Server) {
	t.Helper()
	binding := guestos.WindowsProfile{Version: 1, VMID: uuid.New(), GUIUserSID: "S-1-5-21-1-2-3-1001", GUIUser: "Guest",
		InstanceDirectory: filepath.Join(s.profile, "windows-vm"), PairingFile: filepath.Join(s.profile, "pair.dpapi"), RelayExecutable: os.Args[0]}
	if err := guestos.BindWindowsProfile(s.profile, binding); err != nil {
		t.Fatal(err)
	}
	s.windowsGuest = &binding
	t.Setenv("CAGE_DISPLAY_RELAY_TEST", "1")
}

func TestWindowsBrowserDisplaySelectsASubprotocol(t *testing.T) {
	checkDisplayOffers(t, false, func(t *testing.T) (*Server, http.HandlerFunc) {
		s := testServer(t)
		s.ready = true
		bindDisplayRelay(t, s)
		return s, s.vnc
	})
}

func TestWindowsClaudeDesktopDisplaySelectsASubprotocol(t *testing.T) {
	checkDisplayOffers(t, false, func(t *testing.T) (*Server, http.HandlerFunc) {
		s := testServer(t)
		bindDisplayRelay(t, s)
		return s, s.claudeDesktopVNC
	})
}
