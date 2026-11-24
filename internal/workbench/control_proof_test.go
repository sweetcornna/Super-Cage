package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/entryproxy"
)

// ipv6LoopbackUsable reports whether this system can bind [::1] at all.
func ipv6LoopbackUsable() bool {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ephemeralTwinPort returns a listener on 127.0.0.1 at a port the system
// assigned, whose [::1] twin is free. It never returns a port of the
// installed app's range (17777..17807): a test must not take the [::1] twin
// of a real controller.
func ephemeralTwinPort(t *testing.T) net.Listener {
	t.Helper()
	if !ipv6LoopbackUsable() {
		t.Skip("no IPv6 loopback on this system: nothing can listen on [::1]")
	}
	for range 20 {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		if port >= controllerFirstPort && port < controllerFirstPort+controllerPortCount {
			l.Close()
			continue
		}
		probe, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
		if err != nil {
			l.Close()
			continue
		}
		probe.Close()
		return l
	}
	t.Fatal("no ephemeral port with a free [::1] twin")
	return nil
}

// squatter stands for another local user listening on [::1]:<port>. It keeps
// every byte a client sends it and answers like a busy controller.
type squatter struct {
	mu    sync.Mutex
	conns int
	data  bytes.Buffer
}

func newSquatter(t *testing.T, port int) *squatter {
	t.Helper()
	if port >= controllerFirstPort && port < controllerFirstPort+controllerPortCount {
		t.Fatalf("refusing to squat a real controller port %d", port)
	}
	l, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	q := &squatter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			q.mu.Lock()
			q.conns++
			q.mu.Unlock()
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				buffer := make([]byte, 64<<10)
				n, _ := conn.Read(buffer)
				q.mu.Lock()
				q.data.Write(buffer[:n])
				q.mu.Unlock()
				_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); <-done })
	return q
}

func (q *squatter) seen() (int, string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.conns, q.data.String()
}

// holdControllerLock makes profile look owned by a live controller, as
// connectController requires before it dials.
func holdControllerLock(t *testing.T, profile string) {
	t.Helper()
	lock, err := openControllerLock(profile, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err = lockController(lock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockController(lock); lock.Close() })
}

// recordingHandler keeps the path and body of every request a test server
// receives.
type recordingHandler struct {
	mu   sync.Mutex
	seen []string
	next http.Handler
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	h.mu.Lock()
	h.seen = append(h.seen, r.Method+" "+r.URL.Path+" "+string(body))
	h.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.next.ServeHTTP(w, r)
}

func (h *recordingHandler) log() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.seen, "\n")
}

// startTestController serves s on listener and publishes record as its
// control.json under a held profile lock.
func startTestController(t *testing.T, s *Server, listener net.Listener, record func(port int) controlFile) *recordingHandler {
	t.Helper()
	identity, err := currentController()
	if err != nil {
		t.Fatal(err)
	}
	s.identity = identity
	s.instanceID = randomToken()
	port := listener.Addr().(*net.TCPAddr).Port
	s.origin = "http://localhost:" + strconv.Itoa(port)
	recording := &recordingHandler{next: s.handler()}
	server := httptest.NewUnstartedServer(recording)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	holdControllerLock(t, s.profile)
	if err := writePrivate(filepath.Join(s.profile, "control.json"), record(port)); err != nil {
		t.Fatal(err)
	}
	return recording
}

func legacyRecord(s *Server) func(int) controlFile {
	return func(port int) controlFile {
		return controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: s.identity}
	}
}

// Every client of a controller dials 127.0.0.1 itself. A squatter on the
// [::1] twin of the port, which "localhost" resolves to first on macOS, gets
// no connection and so no token, ticket or cookie.
func TestControllerClientsSendNothingToIPv6Squatter(t *testing.T) {
	s := testServer(t)
	s.proofKey = randomToken()
	listener := ephemeralTwinPort(t)
	port := listener.Addr().(*net.TCPAddr).Port
	startTestController(t, s, listener, s.controlRecord)
	q := newSquatter(t, port)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var failures []string
	note := func(step string, err error) {
		if err != nil {
			failures = append(failures, step+": "+err.Error())
		}
	}
	c, err := connectProvenController(s.profile)
	note("connectProvenController", err)
	if err == nil {
		c.close()
	}
	local := &desktopLocal{profile: s.profile}
	if c, err = openController(s.profile); err == nil {
		_, err = c.localControl(ctx, "status", desktopLocalRequest{})
		note("localControl", err)
		session, err := local.localSession(ctx, ctx, c, desktopLocalReply{Identity: s.identity, InstanceID: s.instanceID})
		note("localSession", err)
		if err == nil {
			response, err := session.Client.Get(s.origin + "/api/settings")
			note("local session request", err)
			if err == nil {
				response.Body.Close()
			}
			session.Close()
		}
		c.close()
	} else {
		note("openController", err)
	}
	_, err = AgentRequest(ctx, s.configPath, AgentTarget{}, "GET", "/api/settings", nil, "", "")
	note("AgentRequest", err)
	_ = AgentSocket(ctx, s.configPath, AgentTarget{}, "/api/terminals/none/stream", io.NopCloser(strings.NewReader("")), io.Discard)
	// The local SSH entry's proxy (cage entry proxy): refused,
	// the entry is off, but over the proven connection.
	if ws, _, err := entryproxy.Dial(ctx, s.configPath, "none", agentSameBuild); err == nil {
		ws.Close()
	}
	if conns, data := q.seen(); conns != 0 {
		t.Fatalf("the squatter on [::1]:%d got %d connections; it read:\n%s", port, conns, data)
	}
	if len(failures) != 0 {
		t.Fatalf("clients failed against the real controller:\n%s", strings.Join(failures, "\n"))
	}
}

// impostor stands for whoever holds 127.0.0.1:<port> when the profile's
// controller is not listening there. It answers every request like a
// controller would and keeps what it receives.
func impostorController(t *testing.T, answer http.HandlerFunc) (*recordingHandler, int) {
	t.Helper()
	recording := &recordingHandler{next: answer}
	server := httptest.NewServer(recording)
	t.Cleanup(server.Close)
	return recording, server.Listener.Addr().(*net.TCPAddr).Port
}

func impostorLaunch(port int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case controllerProofPath:
			jsonReply(w, map[string]any{"version": controlAuth, "proof": strings.Repeat("0", 64)})
		case "/api/launch":
			jsonReply(w, launchResponse{URL: "http://localhost:" + strconv.Itoa(port) + "/#" + randomToken()})
		default:
			jsonReply(w, map[string]bool{"ok": true})
		}
	}
}

func TestControllerProofKeepsTokenFromImpostor(t *testing.T) {
	profile := t.TempDir()
	holdControllerLock(t, profile)
	var port int
	recording, port := impostorController(t, func(w http.ResponseWriter, r *http.Request) { impostorLaunch(port)(w, r) })
	token := randomToken()
	record := controlFile{Port: port, Token: token, PID: os.Getpid(), Auth: controlAuth, ProofKey: randomToken()}
	if err := writePrivate(filepath.Join(profile, "control.json"), record); err != nil {
		t.Fatal(err)
	}
	_, connectErr := connectProvenController(profile)
	var localErr error
	if c, err := openController(profile); err == nil {
		_, localErr = c.localControl(t.Context(), "status", desktopLocalRequest{})
		c.close()
	} else {
		localErr = err
	}
	if seen := recording.log(); strings.Contains(seen, token) || strings.Contains(seen, "/api/launch ") || strings.Contains(seen, "/api/desktop-local/") {
		t.Fatalf("the impostor received a credential:\n%s", seen)
	}
	if !errors.Is(connectErr, errControllerUnproven) || localErr == nil {
		t.Fatalf("connect %v, local control %v", connectErr, localErr)
	}
}

// A server that relays the challenge to the real controller over its own
// connection gets an answer bound to that connection, not to the client's.
func TestControllerProofRejectsRelayedAnswer(t *testing.T) {
	s := testServer(t)
	s.proofKey = randomToken()
	real := httptest.NewServer(http.NotFoundHandler())
	realPort := real.Listener.Addr().(*net.TCPAddr).Port
	s.origin = "http://localhost:" + strconv.Itoa(realPort)
	real.Config.Handler = s.handler()
	t.Cleanup(real.Close)
	profile := t.TempDir()
	holdControllerLock(t, profile)
	var port int
	relay := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != controllerProofPath {
			impostorLaunch(port)(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		forward, _ := http.NewRequest("POST", real.URL+controllerProofPath, bytes.NewReader(body))
		forward.Host = "localhost:" + strconv.Itoa(realPort)
		forward.Header.Set("Origin", "http://"+forward.Host)
		forward.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}
	recording, port := impostorController(t, relay)
	record := controlFile{Port: port, Token: s.token, PID: os.Getpid(), Auth: controlAuth, ProofKey: s.proofKey}
	if err := writePrivate(filepath.Join(profile, "control.json"), record); err != nil {
		t.Fatal(err)
	}
	_, err := connectProvenController(profile)
	if seen := recording.log(); strings.Contains(seen, s.token) || strings.Contains(seen, "/api/launch ") {
		t.Fatalf("the relay received the token:\n%s", seen)
	}
	if !errors.Is(err, errControllerUnproven) {
		t.Fatalf("connect: %v", err)
	}
}

func agentErrorMessage(err error) string {
	var agentErr *AgentError
	if errors.As(err, &agentErr) {
		return agentErr.Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// The private record decides the protocol. A record from the release before
// the proof is refused by automation with a plain update message, before
// anything is sent, and still reaches the window's own paths over 127.0.0.1.
// A record from a newer release asks for this client to be updated.
func TestControllerVersionNegotiation(t *testing.T) {
	t.Run("older controller", func(t *testing.T) {
		s := testServer(t)
		s.proofKey = ""
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		recording := startTestController(t, s, listener, legacyRecord(s))
		_, provenErr := connectProvenController(s.profile)
		_, requestErr := AgentRequest(t.Context(), s.configPath, AgentTarget{}, "GET", "/api/settings", nil, "", "")
		socketErr := AgentSocket(t.Context(), s.configPath, AgentTarget{}, "/api/terminals/none/stream", io.NopCloser(strings.NewReader("")), io.Discard)
		if seen := recording.log(); strings.Contains(seen, s.token) || strings.Contains(seen, "/api/launch ") {
			t.Fatalf("automation sent the token to an unproven controller:\n%s", seen)
		}
		if !errors.Is(provenErr, errControllerOutdated) {
			t.Fatalf("connectProvenController: %v", provenErr)
		}
		for name, err := range map[string]error{"AgentRequest": requestErr, "AgentSocket": socketErr} {
			if !strings.Contains(agentErrorMessage(err), "需要更新") {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, openErr := openController(s.profile); !errors.Is(openErr, errControllerOutdated) {
			t.Fatalf("openController accepted an older controller: %v", openErr)
		}
		if seen := recording.log(); strings.Contains(seen, s.token) || strings.Contains(seen, "/api/launch ") {
			t.Fatalf("a client sent the token to an unproven controller:\n%s", seen)
		}
	})
	t.Run("newer controller", func(t *testing.T) {
		s := testServer(t)
		s.proofKey = randomToken()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		recording := startTestController(t, s, listener, func(port int) controlFile {
			record := s.controlRecord(port)
			record.Auth = controlAuth + 1
			return record
		})
		_, openErr := openController(s.profile)
		_, requestErr := AgentRequest(t.Context(), s.configPath, AgentTarget{}, "GET", "/api/settings", nil, "", "")
		_, entryErr := ServerEntry(s.configPath)
		if seen := recording.log(); seen != "" {
			t.Fatalf("a client spoke a protocol it does not know:\n%s", seen)
		}
		if !errors.Is(openErr, errClientOutdated) {
			t.Fatalf("openController: %v", openErr)
		}
		for name, err := range map[string]error{"AgentRequest": requestErr, "ServerEntry": entryErr} {
			if !strings.Contains(agentErrorMessage(err), "需要更新") {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("older client", func(t *testing.T) {
		s := testServer(t)
		s.proofKey = randomToken()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		startTestController(t, s, listener, s.controlRecord)
		body, _ := json.Marshal(map[string]string{"token": s.token})
		request, _ := http.NewRequest("POST", "http://"+listener.Addr().String()+"/api/launch", bytes.NewReader(body))
		request.Host = strings.TrimPrefix(s.origin, "http://")
		request.Header.Set("Origin", s.origin)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultTransport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("a client from before the proof was refused: %d", response.StatusCode)
		}
	})
}

// The controller holds the [::1] twin of its port, so a desktop window that
// loads http://localhost:<port> reaches it whichever address it tries first.
func TestListenLoopbackHoldsIPv6Twin(t *testing.T) {
	if !ipv6LoopbackUsable() {
		t.Skip("no IPv6 loopback on this system: nothing can listen on [::1]")
	}
	listener, err := listenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if other, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port)); err == nil {
		other.Close()
		t.Fatalf("[::1]:%d was left for anyone to take", port)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
		accepted <- err
	}()
	conn, err := net.DialTimeout("tcp6", fmt.Sprintf("[::1]:%d", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a connection to [::1] did not reach the controller")
	}
}

// A port whose [::1] twin someone else holds counts as taken.
func TestListenLoopbackTreatsHeldIPv6TwinAsTaken(t *testing.T) {
	probe := ephemeralTwinPort(t)
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	newSquatter(t, port)
	listener, err := listenLoopback(port)
	if err == nil {
		listener.Close()
		t.Fatalf("the controller listened on %d next to a squatter on [::1]", port)
	}
	again, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("the refused port stayed held on 127.0.0.1: %v", err)
	}
	again.Close()
}

func TestListenLoopbackWithoutIPv6Loopback(t *testing.T) {
	previous := listenLoopbackV6
	listenLoopbackV6 = func(int) (net.Listener, error) { return nil, errors.New("no IPv6 loopback") }
	t.Cleanup(func() { listenLoopbackV6 = previous })
	listener, err := listenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
}

// The proof answers in every serve mode and while the account is locked:
// it comes before the session check, workload routing and the lock.
func TestControllerProofInEveryMode(t *testing.T) {
	for _, mode := range []string{"workload", "client", "desktop"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			s.proofKey = randomToken()
			s.clientOnly = mode == "client"
			s.desktopEntry = mode == "desktop"
			s.account.lock()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			startTestController(t, s, listener, s.controlRecord)
			port := listener.Addr().(*net.TCPAddr).Port
			conn, err := controllerDial(s.controlRecord(port))(t.Context(), "tcp", "localhost:"+strconv.Itoa(port))
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			s.proofKey = ""
			if _, err = controllerDial(controlFile{Port: port, Auth: controlAuth, ProofKey: randomToken()})(t.Context(), "tcp", "localhost:"+strconv.Itoa(port)); !errors.Is(err, errControllerUnproven) {
				t.Fatalf("a controller without a proof key proved itself: %v", err)
			}
		})
	}
}

// BenchmarkControllerDial measures the cost of a proven controller connection.
func BenchmarkControllerDial(b *testing.B) {
	s := &Server{proofKey: randomToken(), token: randomToken(), session: randomToken()}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	s.origin = "http://localhost:" + strconv.Itoa(port)
	server := httptest.NewUnstartedServer(s.handler())
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	dial := controllerDial(s.controlRecord(port))
	b.ResetTimer()
	for range b.N {
		conn, err := dial(context.Background(), "tcp", "localhost:"+strconv.Itoa(port))
		if err != nil {
			b.Fatal(err)
		}
		conn.Close()
	}
}
