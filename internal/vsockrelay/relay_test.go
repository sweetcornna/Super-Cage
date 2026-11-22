package vsockrelay

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	config, err := NewConfig(45001)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// echoTarget stands in for the workbench and counts the connections made to it.
func echoTarget(t *testing.T, dials *atomic.Int32) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		dials.Add(1)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = io.Copy(server, server)
		}()
		return client, nil
	}
}

// recorder keeps everything the gateway wrote to a peer.
type recorder struct {
	net.Conn
	written bytes.Buffer
}

func (r *recorder) Write(data []byte) (int, error) {
	r.written.Write(data)
	return r.Conn.Write(data)
}

func TestConfigLineRoundTripsAndRejectsDamage(t *testing.T) {
	config := testConfig(t)
	if config.Listen < 1<<16 || config.Listen >= 1<<30 || len(config.Secret) != SecretSize {
		t.Fatalf("config %+v", config)
	}
	parsed, err := ParseConfig(config.Line())
	if err != nil || parsed.Listen != config.Listen || parsed.Target != 45001 || !bytes.Equal(parsed.Secret, config.Secret) {
		t.Fatalf("parsed %+v, %v", parsed, err)
	}
	line := strings.TrimSuffix(config.Line(), "\n")
	for _, bad := range []string{"", line + " extra", strings.Replace(line, "RELAY 1", "RELAY 2", 1), line[:len(line)-2], strings.ToUpper(line[:len(line)-64]) + strings.ToUpper(line[len(line)-64:]),
		configPrefix + "80 45001 " + hex.EncodeToString(config.Secret), configPrefix + "70000 0 " + hex.EncodeToString(config.Secret)} {
		if _, err := ParseConfig(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	other := testConfig(t)
	if bytes.Equal(other.Secret, config.Secret) {
		t.Fatal("two bridges got the same secret")
	}
}

func TestAuthenticatedConnectionReachesTheWorkbench(t *testing.T) {
	config := testConfig(t)
	var dials atomic.Int32
	relay := &Relay{Secret: config.Secret, Target: echoTarget(t, &dials)}
	gateway, guest := net.Pipe()
	go relay.ServeConn(guest, HostCID)
	if err := Authenticate(gateway, config.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(gateway, "GET / HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 16)
	if _, err := io.ReadFull(gateway, echo); err != nil || string(echo) != "GET / HTTP/1.1\r\n" {
		t.Fatalf("relayed %q, %v", echo, err)
	}
	gateway.Close()
	if dials.Load() != 1 {
		t.Fatalf("%d workbench connections", dials.Load())
	}
}

func TestRelayNeverReachesTheWorkbenchWithoutTheSecret(t *testing.T) {
	config := testConfig(t)
	var dials atomic.Int32
	relay := &Relay{Secret: config.Secret, Target: echoTarget(t, &dials)}
	// A host process that guessed wrong, and one inside the VM (any CID but
	// the host's) that holds the right secret.
	for _, attempt := range []struct {
		peer   uint32
		secret []byte
	}{{HostCID, testConfig(t).Secret}, {1, config.Secret}, {3, config.Secret}, {42, config.Secret}} {
		gateway, guest := net.Pipe()
		done := make(chan struct{})
		go func() { relay.ServeConn(guest, attempt.peer); close(done) }()
		// The relay hangs up without a word: the caller learns nothing
		// either way and never gets a connection.
		if err := Authenticate(gateway, attempt.secret); !errors.Is(err, ErrNoAnswer) {
			t.Fatalf("peer %d: %v", attempt.peer, err)
		}
		gateway.Close()
		<-done
	}
	if dials.Load() != 0 {
		t.Fatalf("the relay connected to the workbench %d times for unauthenticated peers", dials.Load())
	}
}

func TestRelayAnswersAWorkbenchThatIsNotUp(t *testing.T) {
	config := testConfig(t)
	relay := &Relay{Secret: config.Secret, Target: func() (net.Conn, error) { return nil, errors.New("connection refused") }}
	gateway, guest := net.Pipe()
	go relay.ServeConn(guest, HostCID)
	if err := Authenticate(gateway, config.Secret); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
}

// impostor plays a listener in the VM that is not the gateway's relay.
func impostor(t *testing.T, behave func(net.Conn)) *recorder {
	t.Helper()
	gateway, other := net.Pipe()
	go func() {
		defer other.Close()
		behave(other)
	}()
	return &recorder{Conn: gateway}
}

func TestGatewaySendsNothingButTheHandshakeToAnImpostor(t *testing.T) {
	previous := HandshakeTimeout
	HandshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { HandshakeTimeout = previous })
	config := testConfig(t)
	greeting := helloPrefix + strings.Repeat("ab", nonceSize) + "\n"
	cases := map[string]func(net.Conn){
		// An HTTP server, or anything else that waits for the client.
		"silent": func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) },
		"garbage": func(conn net.Conn) {
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n...")
			_, _ = io.Copy(io.Discard, conn)
		},
		// It knows the protocol but not the secret.
		"mimic": func(conn net.Conn) {
			_, _ = io.WriteString(conn, greeting)
			_, _ = io.ReadFull(conn, make([]byte, gatewaySize))
			_, _ = io.WriteString(conn, strings.Repeat("0", 2*32)+"\n")
			_, _ = io.Copy(io.Discard, conn)
		},
		// It sends the gateway's own proof back as the relay's.
		"reflect": func(conn net.Conn) {
			_, _ = io.WriteString(conn, greeting)
			message := make([]byte, gatewaySize)
			if _, err := io.ReadFull(conn, message); err == nil {
				_, _ = conn.Write(message[2*nonceSize+1:])
			}
			_, _ = io.Copy(io.Discard, conn)
		},
	}
	for name, behave := range cases {
		conn := impostor(t, behave)
		err := Authenticate(conn, config.Secret)
		conn.Close()
		// Silence proves nothing either way (a busy relay is silent too);
		// anything that speaks without the secret is not the bridge.
		want := ErrUnauthenticated
		if name == "silent" {
			want = ErrNoAnswer
		}
		if !errors.Is(err, want) || (want == ErrNoAnswer && errors.Is(err, ErrUnauthenticated)) {
			t.Fatalf("%s: %v, want %v", name, err, want)
		}
		switch name {
		case "silent", "garbage":
			if conn.written.Len() != 0 {
				t.Fatalf("%s: the gateway sent %q to a listener that never greeted it", name, conn.written.String())
			}
		default:
			if conn.written.Len() != gatewaySize {
				t.Fatalf("%s: the gateway sent %d bytes, want only its %d-byte proof", name, conn.written.Len(), gatewaySize)
			}
		}
	}
}

// A relay that sheds a connection during a burst has not been impersonated:
// the gateway must count it as an ordinary failed dial, not drop the path.
func TestAConnectionTheRelayShedsIsNoAnswer(t *testing.T) {
	config := testConfig(t)
	gateway, relay := net.Pipe()
	go func() { _ = relay.Close() }()
	err := Authenticate(gateway, config.Secret)
	gateway.Close()
	if !errors.Is(err, ErrNoAnswer) || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a shed connection: %v", err)
	}
}

func TestUnauthenticatedConnectionsAreDroppedAfterTheHandshakeTimeout(t *testing.T) {
	previous := HandshakeTimeout
	HandshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { HandshakeTimeout = previous })
	config := testConfig(t)
	var dials atomic.Int32
	relay := &Relay{Secret: config.Secret, Target: echoTarget(t, &dials)}
	idle, guest := net.Pipe()
	done := make(chan struct{})
	go func() { relay.ServeConn(guest, HostCID); close(done) }()
	_, _ = io.ReadFull(idle, make([]byte, helloSize))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a connection that never answered the greeting was kept")
	}
	idle.Close()
}

type fakeListener struct {
	conns  chan net.Conn
	closed chan struct{}
}

func (l *fakeListener) Accept() (net.Conn, uint32, error) {
	select {
	case conn := <-l.conns:
		return conn, HostCID, nil
	case <-l.closed:
		return nil, 0, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func TestRelayEndsWithItsStdin(t *testing.T) {
	config := testConfig(t)
	listener := &fakeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	var port atomic.Uint32
	stdin, feed := io.Pipe()
	var stderr bytes.Buffer
	exited := make(chan int, 1)
	go func() {
		exited <- run(stdin, &stderr, func(listen uint32) (Listener, error) { port.Store(listen); return listener, nil }, func(uint32) (net.Conn, error) {
			client, server := net.Pipe()
			go func() { _, _ = io.Copy(server, server) }()
			return client, nil
		})
	}()
	if _, err := io.WriteString(feed, config.Line()); err != nil {
		t.Fatal(err)
	}
	gateway, guest := net.Pipe()
	listener.conns <- guest
	if err := Authenticate(gateway, config.Secret); err != nil {
		t.Fatal(err)
	}
	if port.Load() != config.Listen {
		t.Fatalf("listened on %d, want %d", port.Load(), config.Listen)
	}
	// The gateway, or its bridge, goes away: the relay and its listener end.
	feed.Close()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the relay outlived its stdin")
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("the listener stayed open")
	}
	if strings.Contains(stderr.String(), hex.EncodeToString(config.Secret)) {
		t.Fatal("the secret reached stderr")
	}
}

func TestRelayRefusesABadConfiguration(t *testing.T) {
	var stderr bytes.Buffer
	listened := false
	code := run(strings.NewReader("CAGE-VSOCK-RELAY 1 70000 45001 short\n"), &stderr, func(uint32) (Listener, error) { listened = true; return nil, errors.New("unused") }, nil)
	if code == 0 || listened {
		t.Fatalf("exit %d, listened %v", code, listened)
	}
}
