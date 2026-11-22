// Package vsockrelay is the guest half of the tenant gateway's optional vsock
// path (cage-tenant -incus-vsock) and the handshake both halves speak.
//
// The gateway starts the relay in a workspace VM through incus exec as
// "cage vsock-relay" and writes one configuration line on its stdin: the vsock
// port to listen on, the workbench's loopback port and a secret made for this
// bridge alone. Nothing secret is in argv or the environment. The relay ends
// when its stdin does, so it never outlives the gateway that started it.
//
// Every connection is authenticated both ways before a single HTTP byte:
//
//	relay   -> gateway  "CAGE-VSOCK 1 " + hex(nonceR) + "\n"
//	gateway -> relay    hex(nonceG) + " " + hex(HMAC(secret, "cage-vsock gateway\x00" | nonceR | nonceG)) + "\n"
//	relay   -> gateway  hex(HMAC(secret, "cage-vsock relay\x00" | nonceR | nonceG)) + "\n"
//
// The relay checks the gateway's proof before it connects to the workbench,
// and proves itself only once that connection is made (a failed one is
// answered with the "relay-unready" label instead). The relay speaks first,
// so a listener that is not a relay gets nothing from the gateway; the labels
// differ, so neither proof can be reflected; both nonces are fresh, so none
// can be replayed. On Linux the relay also drops every peer whose vsock
// address is not the host's (CID 2), which keeps processes in the VM, its
// containers and nested guests away from the handshake; host processes share
// CID 2, and the secret is what keeps them out.
package vsockrelay

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	configPrefix = "CAGE-VSOCK-RELAY 1 "
	helloPrefix  = "CAGE-VSOCK 1 "
	nonceSize    = 16
	// SecretSize is the length of a bridge's secret.
	SecretSize = 32
	// HostCID is the vsock address every connection from the host carries.
	HostCID = 2
	// handshakes bounds the connections that have not proved the secret yet.
	handshakes = 32
)

var (
	// ErrUnauthenticated: whatever answered on the port spoke, but not as the
	// bridge: it did not greet as a relay or did not prove the bridge's
	// secret. Nothing but the handshake was sent to it.
	ErrUnauthenticated = errors.New("the vsock listener did not prove the bridge secret")
	// ErrNoAnswer: the listener closed the connection or stayed silent during
	// the handshake, as a relay that is busy or slow can. It proves nothing
	// either way. Nothing but the handshake was sent to it.
	ErrNoAnswer = errors.New("the vsock listener did not finish the handshake")
	// ErrNotReady: the relay proved itself but could not reach the workbench.
	ErrNotReady = errors.New("the vsock relay could not reach the workbench")
)

// HandshakeTimeout bounds one connection's handshake on either side.
var HandshakeTimeout = 3 * time.Second

const (
	labelGateway = "cage-vsock gateway\x00"
	labelRelay   = "cage-vsock relay\x00"
	labelUnready = "cage-vsock relay-unready\x00"
)

var (
	helloSize   = len(helloPrefix) + 2*nonceSize + 1
	gatewaySize = 2*nonceSize + 1 + 2*sha256.Size + 1
	proofSize   = 2*sha256.Size + 1
)

// Config is what the gateway hands one relay on its stdin.
type Config struct {
	Listen, Target uint32
	Secret         []byte
}

// NewConfig makes the configuration of a new bridge to the workbench's
// loopback port: a random vsock port and a fresh secret.
func NewConfig(target int) (Config, error) {
	if target < 1 || target > 65535 {
		return Config{}, errors.New("invalid workbench port")
	}
	var random [4 + SecretSize]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Config{}, err
	}
	// Above the ports anything else in the guest uses, below VMADDR_PORT_ANY.
	listen := 1<<16 + binary.BigEndian.Uint32(random[:4])%(1<<30-1<<16)
	return Config{Listen: listen, Target: uint32(target), Secret: random[4:]}, nil
}

// Line is the configuration as the relay reads it, newline included.
func (c Config) Line() string {
	return fmt.Sprintf("%s%d %d %s\n", configPrefix, c.Listen, c.Target, hex.EncodeToString(c.Secret))
}

// ParseConfig reads one configuration line, with or without its newline.
func ParseConfig(line string) (Config, error) {
	invalid := errors.New("invalid vsock relay configuration")
	rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), configPrefix)
	fields := strings.Split(rest, " ")
	if !ok || len(fields) != 3 {
		return Config{}, invalid
	}
	listen, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil || listen < 1024 || listen == 1<<32-1 {
		return Config{}, invalid
	}
	target, err := strconv.ParseUint(fields[1], 10, 16)
	if err != nil || target == 0 {
		return Config{}, invalid
	}
	secret, err := hex.DecodeString(fields[2])
	if err != nil || len(secret) != SecretSize || fields[2] != hex.EncodeToString(secret) {
		return Config{}, invalid
	}
	return Config{Listen: uint32(listen), Target: uint32(target), Secret: secret}, nil
}

func proof(secret []byte, label string, relayNonce, gatewayNonce []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(label))
	mac.Write(relayNonce)
	mac.Write(gatewayNonce)
	return []byte(hex.EncodeToString(mac.Sum(nil)) + "\n")
}

// Authenticate runs the gateway's side of the handshake on a new connection.
// It sends nothing until the peer has spoken as a relay, and returns nil only
// once the peer proved the secret and reached the workbench.
func Authenticate(conn net.Conn, secret []byte) error {
	_ = conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})
	hello := make([]byte, helloSize)
	if n, err := io.ReadFull(conn, hello); err != nil {
		// A start that is not a relay's greeting is an answer, and a wrong one.
		if got := string(hello[:n]); !strings.HasPrefix(helloPrefix, got) && !strings.HasPrefix(got, helloPrefix) {
			return fmt.Errorf("%w: not a relay greeting", ErrUnauthenticated)
		}
		return fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	encoded, ok := strings.CutPrefix(string(hello), helloPrefix)
	relayNonce, err := hex.DecodeString(strings.TrimSuffix(encoded, "\n"))
	if !ok || !strings.HasSuffix(encoded, "\n") || err != nil || len(relayNonce) != nonceSize {
		return fmt.Errorf("%w: not a relay greeting", ErrUnauthenticated)
	}
	gatewayNonce := make([]byte, nonceSize)
	if _, err := rand.Read(gatewayNonce); err != nil {
		return err
	}
	message := hex.EncodeToString(gatewayNonce) + " " + string(proof(secret, labelGateway, relayNonce, gatewayNonce))
	if _, err := io.WriteString(conn, message); err != nil {
		return fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	answer := make([]byte, proofSize)
	if _, err := io.ReadFull(conn, answer); err != nil {
		return fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	switch {
	case hmac.Equal(answer, proof(secret, labelRelay, relayNonce, gatewayNonce)):
		return nil
	case hmac.Equal(answer, proof(secret, labelUnready, relayNonce, gatewayNonce)):
		return ErrNotReady
	}
	return fmt.Errorf("%w: wrong proof", ErrUnauthenticated)
}

// Listener yields connections with the vsock address (CID) of their peer.
type Listener interface {
	Accept() (net.Conn, uint32, error)
	Close() error
}

// Relay forwards authenticated connections from the host to the workbench.
type Relay struct {
	Secret []byte
	// Target connects to the workbench.
	Target  func() (net.Conn, error)
	once    sync.Once
	waiting chan struct{}
}

// Serve accepts connections until the listener fails or is closed.
func (r *Relay) Serve(listener Listener) error {
	for {
		conn, peer, err := listener.Accept()
		if err != nil {
			return err
		}
		go r.ServeConn(conn, peer)
	}
}

// ServeConn relays one connection whose peer has the given vsock address.
func (r *Relay) ServeConn(conn net.Conn, peer uint32) {
	if peer != HostCID {
		_ = conn.Close()
		return
	}
	target, ok := r.admit(conn)
	if !ok {
		_ = conn.Close()
		return
	}
	splice(conn, target)
}

func (r *Relay) admit(conn net.Conn) (net.Conn, bool) {
	r.once.Do(func() { r.waiting = make(chan struct{}, handshakes) })
	select {
	case r.waiting <- struct{}{}:
		defer func() { <-r.waiting }()
	default:
		return nil, false // too many connections are still proving themselves
	}
	_ = conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	relayNonce := make([]byte, nonceSize)
	if _, err := rand.Read(relayNonce); err != nil {
		return nil, false
	}
	if _, err := io.WriteString(conn, helloPrefix+hex.EncodeToString(relayNonce)+"\n"); err != nil {
		return nil, false
	}
	message := make([]byte, gatewaySize)
	if _, err := io.ReadFull(conn, message); err != nil {
		return nil, false
	}
	gatewayNonce, err := hex.DecodeString(string(message[:2*nonceSize]))
	if err != nil || message[2*nonceSize] != ' ' || subtle.ConstantTimeCompare(message[2*nonceSize+1:], proof(r.Secret, labelGateway, relayNonce, gatewayNonce)) != 1 {
		return nil, false
	}
	target, err := r.Target()
	if err != nil {
		_, _ = conn.Write(proof(r.Secret, labelUnready, relayNonce, gatewayNonce))
		return nil, false
	}
	if _, err := conn.Write(proof(r.Secret, labelRelay, relayNonce, gatewayNonce)); err != nil {
		_ = target.Close()
		return nil, false
	}
	_ = conn.SetDeadline(time.Time{})
	return target, true
}

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	forward := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go forward(a, b)
	go forward(b, a)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

// Main runs "cage vsock-relay": it reads its configuration from stdin, relays
// until stdin ends, and reports problems on stderr without the secret.
func Main(stdin io.Reader, stderr io.Writer) int {
	return run(stdin, stderr, ListenVsock, func(port uint32) (net.Conn, error) {
		return net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 5*time.Second)
	})
}

func run(stdin io.Reader, stderr io.Writer, listen func(uint32) (Listener, error), dial func(uint32) (net.Conn, error)) int {
	input := bufio.NewReaderSize(stdin, 256)
	line, err := input.ReadString('\n')
	config, parseErr := ParseConfig(line)
	if err != nil || parseErr != nil {
		fmt.Fprintln(stderr, "cage vsock-relay: invalid configuration on stdin")
		return 2
	}
	listener, err := listen(config.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "cage vsock-relay: cannot listen on vsock port %d: %v\n", config.Listen, err)
		return 1
	}
	defer listener.Close()
	relay := &Relay{Secret: config.Secret, Target: func() (net.Conn, error) { return dial(config.Target) }}
	served := make(chan error, 1)
	go func() { served <- relay.Serve(listener) }()
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, input)
		close(ended)
	}()
	select {
	case <-ended:
		return 0 // the gateway or its bridge went away
	case err := <-served:
		fmt.Fprintf(stderr, "cage vsock-relay: %v\n", err)
		return 1
	}
}
