package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHServer is an in-process SSH server that accepts one user key and
// forwards direct-tcpip channels, standing in for the Lima guest's sshd.
type testSSHServer struct {
	listener net.Listener
	hostKey  ssh.Signer
	mu       sync.Mutex
	targets  []string
	accepts  int
}

func newTestSSHServer(t *testing.T, user ssh.PublicKey, forward func(string) (net.Conn, error)) *testSSHServer {
	t.Helper()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, _ := ssh.NewSignerFromKey(private)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testSSHServer{listener: listener, hostKey: hostKey}
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() == "guest" && string(key.Marshal()) == string(user.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	config.AddHostKey(hostKey)
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			server.mu.Lock()
			server.accepts++
			server.mu.Unlock()
			go func() {
				conn, channels, requests, err := ssh.NewServerConn(raw, config)
				if err != nil {
					raw.Close()
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "direct-tcpip" {
						_ = incoming.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var payload struct {
						Host       string
						Port       uint32
						OriginHost string
						OriginPort uint32
					}
					if ssh.Unmarshal(incoming.ExtraData(), &payload) != nil {
						_ = incoming.Reject(ssh.ConnectionFailed, "bad")
						continue
					}
					target := net.JoinHostPort(payload.Host, fmt.Sprint(payload.Port))
					server.mu.Lock()
					server.targets = append(server.targets, target)
					server.mu.Unlock()
					upstream, err := forward(target)
					if err != nil {
						_ = incoming.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					channel, reqs, err := incoming.Accept()
					if err != nil {
						upstream.Close()
						continue
					}
					go ssh.DiscardRequests(reqs)
					go func() {
						_, _ = io.Copy(channel, upstream)
						_ = channel.CloseWrite()
					}()
					go func() {
						_, _ = io.Copy(upstream, channel)
						_ = upstream.Close()
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return server
}

func writeTestIdentity(t *testing.T, dir string) (string, ssh.PublicKey) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "user")
	if err = os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	key, _ := ssh.NewPublicKey(public)
	return path, key
}

func TestScienceSSHConfigParse(t *testing.T) {
	// The shape limactl writes (ssh.config of the instance).
	config := []byte(`# This SSH config file can be passed to 'ssh -F'.
Host lima-cage
  IdentityFile "/home/me/.lima/_config/user"
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  NoHostAuthenticationForLocalhost yes
  PreferredAuthentications publickey
  BatchMode yes
  User me
  ControlMaster auto
  ControlPath "/home/me/.lima/cage/ssh.sock"
  ControlPersist yes
  Hostname 127.0.0.1
  Port 60022
Host other
  User nobody
`)
	endpoint, err := parseSSHConfig(config, "lima-cage")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Address != "127.0.0.1:60022" || endpoint.User != "me" || len(endpoint.Identities) != 1 || endpoint.Identities[0] != "/home/me/.lima/_config/user" || endpoint.Strict || endpoint.KnownHosts != "/dev/null" {
		t.Fatalf("%+v", endpoint)
	}
	// limactl on Windows: quoted drive paths and NUL.
	windows := []byte("Host lima-cage\r\n  IdentityFile \"C:\\Users\\me\\.lima\\_config\\user\"\r\n  StrictHostKeyChecking no\r\n  UserKnownHostsFile NUL\r\n  User me\r\n  Hostname 127.0.0.1\r\n  Port 50022\r\n")
	endpoint, err = parseSSHConfig(windows, "lima-cage")
	if err != nil || endpoint.Address != "127.0.0.1:50022" || endpoint.Identities[0] != `C:\Users\me\.lima\_config\user` || endpoint.KnownHosts != "NUL" || endpoint.Strict {
		t.Fatalf("Windows config: %+v %v", endpoint, err)
	}
	if _, err = parseSSHConfig(config, "lima-missing"); err == nil {
		t.Fatal("missing host accepted")
	}
	if _, err = parseSSHConfig(append([]byte("Include other\n"), config...), "lima-cage"); err == nil {
		t.Fatal("Include accepted")
	}
	if _, err = parseSSHConfig([]byte("Host lima-cage\n User me\n IdentityFile /k\n Hostname 127.0.0.1\n Port 0\n"), "lima-cage"); err == nil {
		t.Fatal("port 0 accepted")
	}
}

func TestScienceSSHDialerCarriesTheDaemonOnly(t *testing.T) {
	dir := t.TempDir()
	identity, public := writeTestIdentity(t, dir)
	daemon := newFakeScienceDaemon(t)
	daemonAddress := strings.TrimPrefix(daemon.server.URL, "http://")
	server := newTestSSHServer(t, public, func(target string) (net.Conn, error) {
		if target != "127.0.0.1:47800" {
			return nil, errors.New("refused")
		}
		return net.Dial("tcp", daemonAddress)
	})
	endpoint := sshEndpoint{Address: server.listener.Addr().String(), User: "guest", Identities: []string{identity}, KnownHosts: "/dev/null"}
	dialer := newScienceSSHDialer(endpoint, "127.0.0.1:47800")
	t.Cleanup(func() { dialer.Close() })
	upstream := newScienceUpstream(dialer, 47800, daemon.link)
	t.Cleanup(upstream.close)
	client := &http.Client{Transport: upstream}
	for i := 0; i < 5; i++ {
		response, err := client.Get("http://localhost:47800/api/auth/status")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("status %d", response.StatusCode)
		}
	}
	server.mu.Lock()
	accepts, targets := server.accepts, append([]string(nil), server.targets...)
	server.mu.Unlock()
	if accepts != 1 {
		t.Fatalf("one SSH connection expected, got %d", accepts)
	}
	for _, target := range targets {
		if target != "127.0.0.1:47800" {
			t.Fatalf("channel opened to %s", target)
		}
	}
	// The upstream transport cannot be pointed anywhere else.
	if _, err := client.Get("http://localhost:47801/"); err == nil {
		t.Fatal("transport dialed another port")
	}
	// A changed host key (another machine answering) is refused.
	dialer.mu.Lock()
	if dialer.client != nil {
		dialer.client.Close()
	}
	dialer.client = nil
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	dialer.pinned, _ = ssh.NewPublicKey(other.Public())
	dialer.mu.Unlock()
	if _, err := dialer.DialContext(context.Background()); err == nil {
		t.Fatal("changed host key accepted")
	}
}

func TestScienceSSHDialerStrictKnownHosts(t *testing.T) {
	dir := t.TempDir()
	identity, public := writeTestIdentity(t, dir)
	server := newTestSSHServer(t, public, func(string) (net.Conn, error) { return nil, errors.New("unused") })
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("[cage-guest]:22 "+string(ssh.MarshalAuthorizedKey(server.hostKey.PublicKey()))), 0600); err != nil {
		t.Fatal(err)
	}
	base := sshEndpoint{Address: server.listener.Addr().String(), User: "guest", Identities: []string{identity}, KnownHosts: known, Strict: true}
	wrongAlias := base
	wrongAlias.Alias = "other-guest"
	if _, err := newScienceSSHDialer(wrongAlias, "127.0.0.1:47800").connect(context.Background()); err == nil {
		t.Fatal("host key not in known_hosts accepted")
	}
	rightAlias := base
	rightAlias.Alias = "[cage-guest]:22"
	dialer := newScienceSSHDialer(rightAlias, "127.0.0.1:47800")
	t.Cleanup(func() { dialer.Close() })
	if _, err := dialer.connect(context.Background()); err != nil {
		t.Fatalf("known host refused: %v", err)
	}
}

type failingDialer struct {
	err    error
	closed bool
}

func (d *failingDialer) DialContext(context.Context) (net.Conn, error) { return nil, d.err }
func (d *failingDialer) Mode() string                                  { return "failing" }
func (d *failingDialer) Close() error                                  { d.closed = true; return nil }

func TestScienceAutoDialerFallsBackForGood(t *testing.T) {
	daemon := newFakeScienceDaemon(t)
	primary := &failingDialer{err: errors.New("handshake failed")}
	auto := &scienceAutoDialer{primary: primary, fallback: tcpDialer{address: strings.TrimPrefix(daemon.server.URL, "http://")}}
	if auto.Mode() != "failing" {
		t.Fatal(auto.Mode())
	}
	conn, err := auto.DialContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if auto.Mode() != "test" || !primary.closed {
		t.Fatalf("mode %s closed %v", auto.Mode(), primary.closed)
	}
	// A refused channel (daemon down) is not a transport failure.
	refused := &failingDialer{err: &ssh.OpenChannelError{Reason: ssh.ConnectionFailed}}
	auto = &scienceAutoDialer{primary: refused, fallback: tcpDialer{}}
	if _, err = auto.DialContext(context.Background()); err == nil || auto.Mode() != "failing" {
		t.Fatal("refused channel switched transports")
	}
}
