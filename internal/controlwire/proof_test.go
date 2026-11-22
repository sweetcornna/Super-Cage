package controlwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

var (
	proofKey = strings.Repeat("5e", 32)
	token    = strings.Repeat("7a", 32)
)

// seenRequest is one request a test server read, with the connection it
// came on.
type seenRequest struct {
	conn, path, cookie, connection, body string
}

// proofController is a controller that answers the challenge with key,
// computed as the workbench does, and logs every request.
type proofController struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
	// answer, when set, replaces the proof the server sends.
	answer func(host, client, server, nonce string) string
}

func newProofController(t *testing.T, key string, listener net.Listener) *proofController {
	p := &proofController{}
	p.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.seen = append(p.seen, seenRequest{r.RemoteAddr, r.URL.Path, r.Header.Get("Cookie"), r.Header.Get(controlclient.ConnectionHeader), string(body)})
		p.mu.Unlock()
		switch r.URL.Path {
		case controlclient.ProofPath:
			var in struct {
				Version int    `json:"version"`
				Nonce   string `json:"nonce"`
			}
			local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
			if json.Unmarshal(body, &in) != nil || in.Version != controlclient.ProofVersion || len(in.Nonce) != 64 || local == nil {
				http.Error(w, "请求格式无效", http.StatusBadRequest)
				return
			}
			proof := controlclient.ProofMAC(key, r.Host, r.RemoteAddr, local.String(), in.Nonce)
			if p.answer != nil {
				proof = p.answer(r.Host, r.RemoteAddr, local.String(), in.Nonce)
			}
			json.NewEncoder(w).Encode(map[string]any{"version": controlclient.ProofVersion, "proof": proof})
		case "/api/session":
			http.SetCookie(w, &http.Cookie{Name: "cage_session_1", Value: "s3cret", Path: "/"})
			w.Write([]byte("{}"))
		default:
			w.Write([]byte("{}"))
		}
	}))
	if listener != nil {
		p.Listener.Close()
		p.Listener = listener
	}
	p.Start()
	t.Cleanup(p.Close)
	return p
}

func (p *proofController) requests() []seenRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]seenRequest(nil), p.seen...)
}

func (p *proofController) client(key string) *Client {
	u, _ := url.Parse(p.URL)
	port, _ := strconv.Atoi(u.Port())
	return &Client{origin: "http://localhost:" + u.Port(), host: "localhost:" + u.Port(), addr: u.Host,
		record: controlclient.ControlFile{Port: port, Token: token, Auth: controlclient.ProofVersion, ProofKey: key}}
}

// Every connection proves the controller first, and the request it was
// opened for follows on it; the proof itself carries no credential.
func TestTheControllerProvesItselfOnEveryConnection(t *testing.T) {
	p := newProofController(t, proofKey, nil)
	c := p.client(proofKey)
	c.ConnectionID = "gen-1"
	ctx := context.Background()
	if status, err := c.CallContext(ctx, "POST", "/api/launch", map[string]string{"token": token}, nil); err != nil || status != 200 {
		t.Fatal(status, err)
	}
	if status, err := c.CallContext(ctx, "POST", "/api/session", map[string]string{"token": "ticket"}, nil); err != nil || status != 200 {
		t.Fatal(status, err)
	}
	if status, err := c.CallContext(ctx, "GET", "/api/terminals", nil, nil); err != nil || status != 200 {
		t.Fatal(status, err)
	}
	seen := p.requests()
	if len(seen) != 6 {
		t.Fatalf("%d requests: %+v", len(seen), seen)
	}
	for i := 0; i < len(seen); i += 2 {
		proof, request := seen[i], seen[i+1]
		if proof.path != controlclient.ProofPath || proof.cookie != "" || proof.connection != "" || strings.Contains(proof.body, token) {
			t.Fatalf("connection %d did not start with a bare proof: %+v", i/2, proof)
		}
		if request.conn != proof.conn {
			t.Fatalf("the request went on another connection than its proof: %+v %+v", proof, request)
		}
	}
	if seen[5].cookie != "cage_session_1=s3cret" || seen[5].connection != "gen-1" {
		t.Fatalf("the proven connection lost the session: %+v", seen[5])
	}
	// A record from before the proof: no challenge, as today's controllers expect.
	old := newProofController(t, proofKey, nil)
	c = old.client("")
	c.record.Auth = 0
	if status, err := c.CallContext(ctx, "GET", "/api/terminals", nil, nil); err != nil || status != 200 {
		t.Fatal(status, err)
	}
	if seen := old.requests(); len(seen) != 1 || seen[0].path != "/api/terminals" {
		t.Fatalf("%+v", seen)
	}
}

// A server that does not hold the key (another program on the port) is
// sent no token, no cookie and no connection generation, on a request or a
// WebSocket, even after a session was opened with the real controller.
func TestAnImpostorIsSentNothing(t *testing.T) {
	impostor := newProofController(t, strings.Repeat("0f", 32), nil)
	c := impostor.client(proofKey)
	c.ConnectionID = "gen-1"
	c.setCookie("cage_session_1", "s3cret")
	c.SetTrustedViewCookie(&url.URL{Host: "localhost:1"})
	ctx := context.Background()
	_, err := c.CallContext(ctx, "POST", "/api/launch", map[string]string{"token": token}, nil)
	if !errors.Is(err, controlclient.ErrControllerUnproven) || controlclient.Identity(err) != controlclient.ErrControllerUnproven {
		t.Fatalf("not refused as unproven: %v", err)
	}
	_, err = c.Dial(ctx, controlclient.AgentTarget{Scope: "local"}, "/api/terminals/0123456789abcdef/stream")
	var refused *controlclient.AgentError
	if !errors.As(err, &refused) || refused.Code != "controller_mismatch" || refused.Message != controlclient.ErrControllerUnproven.Error() || !errors.Is(err, controlclient.ErrControllerUnproven) {
		t.Fatalf("the WebSocket was not refused as unproven: %#v", err)
	}
	seen := impostor.requests()
	if len(seen) != 2 {
		t.Fatalf("%+v", seen)
	}
	for _, request := range seen {
		if request.path != controlclient.ProofPath || request.cookie != "" || request.connection != "" || strings.Contains(request.body, token) {
			t.Fatalf("the impostor read more than a challenge: %+v", request)
		}
	}
}

// An answer computed for another connection (a relay that passes the
// challenge to the real controller on its own connection) is refused.
func TestARelayedAnswerIsRefused(t *testing.T) {
	p := newProofController(t, proofKey, nil)
	p.answer = func(host, client, server, nonce string) string {
		return controlclient.ProofMAC(proofKey, host, "127.0.0.1:1", server, nonce)
	}
	c := p.client(proofKey)
	if _, err := c.CallContext(context.Background(), "POST", "/api/launch", map[string]string{"token": token}, nil); !errors.Is(err, controlclient.ErrControllerUnproven) {
		t.Fatal(err)
	}
	if seen := p.requests(); len(seen) != 1 {
		t.Fatalf("%+v", seen)
	}
}

// The answer must come alone, with its length, on a connection the server
// keeps open; otherwise the connection is dropped with nothing sent on it.
func TestTheProofAnswerIsCheckedBeforeTheConnectionIsUsed(t *testing.T) {
	valid := func(conn net.Conn, host, nonce string) string {
		body, _ := json.Marshal(map[string]any{"version": 1, "proof": controlclient.ProofMAC(proofKey, host, conn.RemoteAddr().String(), conn.LocalAddr().String(), nonce)})
		return string(body)
	}
	for _, c := range []struct {
		name  string
		reply func(body string) string
		ok    bool
	}{
		{"good", func(b string) string {
			return "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
		}, true},
		{"closing", func(b string) string {
			return "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
		}, false},
		{"HTTP/1.0", func(b string) string {
			return "HTTP/1.0 200 OK\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
		}, false},
		{"chunked", func(b string) string {
			return "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + fmt.Sprintf("%x", len(b)) + "\r\n" + b + "\r\n0\r\n\r\n"
		}, false},
		{"no length", func(b string) string { return "HTTP/1.1 200 OK\r\n\r\n" + b }, false},
		{"something after it", func(b string) string {
			return "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b + "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}"
		}, false},
		{"refused", func(string) string { return "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n" }, false},
		{"too long", func(b string) string {
			b = b[:len(b)-1] + ",\"pad\":\"" + strings.Repeat("x", 1100) + "\"}"
			return "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			after := make(chan string, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				r := bufio.NewReader(conn)
				request, err := http.ReadRequest(r)
				if err != nil {
					after <- "unreadable challenge"
					return
				}
				var in struct{ Nonce string }
				_ = json.NewDecoder(request.Body).Decode(&in)
				if request.URL.Path != controlclient.ProofPath || request.Header.Get("Cookie") != "" {
					after <- "not a bare challenge: " + request.URL.Path
					return
				}
				conn.Write([]byte(c.reply(valid(conn, request.Host, in.Nonce))))
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				rest, _ := io.ReadAll(r)
				after <- string(rest)
			}()
			port := ln.Addr().(*net.TCPAddr).Port
			client := &Client{origin: fmt.Sprintf("http://localhost:%d", port), host: fmt.Sprintf("localhost:%d", port), addr: ln.Addr().String(),
				record: controlclient.ControlFile{Port: port, Token: token, Auth: 1, ProofKey: proofKey}}
			client.setCookie("cage_session_1", "s3cret")
			_, _, err = client.Do(context.Background(), "POST", "/api/launch", []byte("{\"token\":\""+token+"\"}"), true, 3*time.Second)
			rest := <-after
			if c.ok {
				if !strings.HasPrefix(rest, "POST /api/launch HTTP/1.1\r\n") {
					t.Fatalf("the proven connection did not carry the request: %q (%v)", rest, err)
				}
				return
			}
			if err == nil || rest != "" {
				t.Fatalf("accepted, or used the connection: err=%v, then sent %q", err, rest)
			}
		})
	}
}

// ipv6Twin returns an ephemeral port free on 127.0.0.1 whose [::1] twin is
// free too; never one of the installed app's ports (17777..17807), whose
// [::1] twin a test must not take.
func ipv6Twin(t *testing.T) (net.Listener, int) {
	for range 20 {
		v4, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := v4.Addr().(*net.TCPAddr).Port
		if port >= 17777 && port <= 17807 {
			v4.Close()
			continue
		}
		probe, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
		if err == nil {
			probe.Close()
			return v4, port
		}
		v4.Close()
	}
	t.Fatal("no ephemeral port with a free [::1] twin")
	return nil, 0
}

// Another local user listening on the [::1] twin of the controller's port,
// where "localhost" resolves first on macOS, gets nothing: the client dials
// 127.0.0.1 itself, for requests and WebSockets alike.
func TestAnIPv6SquatterGetsNothing(t *testing.T) {
	if l, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback on this system: nothing can listen on [::1]")
	} else {
		l.Close()
	}
	v4, port := ipv6Twin(t)
	squatter, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		v4.Close()
		t.Fatal(err)
	}
	defer squatter.Close()
	var mu sync.Mutex
	conns := 0
	go func() {
		for {
			conn, err := squatter.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns++
			mu.Unlock()
			conn.Close()
		}
	}()
	p := newProofController(t, proofKey, v4)
	for _, key := range []string{proofKey, ""} {
		c := p.client(key)
		if c.addr != fmt.Sprintf("127.0.0.1:%d", port) || c.host != fmt.Sprintf("localhost:%d", port) {
			t.Fatal(c.addr, c.host)
		}
		ctx := context.Background()
		if status, err := c.CallContext(ctx, "POST", "/api/launch", map[string]string{"token": token}, nil); err != nil || status != 200 {
			t.Fatal(status, err)
		}
		_, _ = c.Dial(ctx, controlclient.AgentTarget{Scope: "local"}, "/api/terminals/0123456789abcdef/stream")
	}
	// Whatever reached [::1] would have been accepted by now.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if conns != 0 {
		t.Fatalf("the squatter on [::1]:%d got %d connections", port, conns)
	}
}
