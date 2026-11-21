package controlwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// clientFor points a client at a test server on 127.0.0.1.
func clientFor(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	u, _ := url.Parse(server.URL)
	return &Client{origin: "http://localhost:" + u.Port(), host: "localhost:" + u.Port(), addr: u.Host, record: controlclient.ControlFile{Token: strings.Repeat("a", 64)}}
}

func TestRequestsAsTheControllerExpects(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" host="+r.Host+" origin="+r.Header.Get("Origin")+" conn="+r.Header.Get(controlclient.ConnectionHeader)+" cookie="+r.Header.Get("Cookie")+" type="+r.Header.Get("Content-Type"))
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/session":
			http.SetCookie(w, &http.Cookie{Name: "cage_session_1", Value: "s3cret", Path: "/", HttpOnly: true, MaxAge: 3600})
			w.Write([]byte("{}"))
		case "/api/logout":
			http.SetCookie(w, &http.Cookie{Name: "cage_session_1", Value: "", Path: "/", MaxAge: -1})
		case "/api/echo":
			w.Write(body)
		case "/api/chunked":
			// Larger than the server's buffer, so it is sent chunked.
			w.Write([]byte("{\"data\":\"" + strings.Repeat("x", 100<<10) + "\"}"))
		case "/api/huge":
			w.Header().Set("Content-Length", fmt.Sprint(300<<10))
			w.Write(bytes.Repeat([]byte("y"), 300<<10))
		case "/api/redirect":
			http.Redirect(w, r, "http://elsewhere.example/", http.StatusFound)
		case "/api/refused":
			http.Error(w, "已锁定", http.StatusLocked)
		}
	}))
	defer server.Close()
	c := clientFor(t, server)
	c.ConnectionID = "gen-1"
	ctx := context.Background()
	if status, err := c.CallContext(ctx, "POST", "/api/session", map[string]string{"token": "t"}, nil); err != nil || status != 200 {
		t.Fatal(status, err)
	}
	var echoed map[string]string
	if status, err := c.CallContext(ctx, "POST", "/api/echo", map[string]string{"a": "b"}, &echoed); err != nil || status != 200 || echoed["a"] != "b" {
		t.Fatal(status, err, echoed)
	}
	var chunked struct{ Data string }
	if status, err := c.CallContext(ctx, "GET", "/api/chunked", nil, &chunked); err != nil || status != 200 || len(chunked.Data) != 100<<10 {
		t.Fatal(status, err, len(chunked.Data))
	}
	if status, data, err := c.Do(ctx, "GET", "/api/huge", nil, false, 0); err != nil || status != 200 || len(data) != replyLimit {
		t.Fatal("a reply is cut at 256 KiB", status, err, len(data))
	}
	var redirected *url.Error
	if _, err := c.CallContext(ctx, "GET", "/api/redirect", nil, nil); !errors.As(err, &redirected) {
		t.Fatal("a redirect was followed or not refused", err)
	}
	if status, data, err := c.Do(ctx, "GET", "/api/refused", nil, false, 0); err != nil || status != 423 || strings.TrimSpace(string(data)) != "已锁定" {
		t.Fatal(status, string(data), err)
	}
	if _, err := c.CallContext(ctx, "POST", "/api/logout", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.CallContext(ctx, "GET", "/api/echo", nil, nil)
	for _, bad := range []string{"/api/x HTTP/1.1\r\nX: y", "/api/a b", "//elsewhere/", "relative"} {
		if _, _, err := c.Do(ctx, "GET", bad, nil, false, 0); err == nil {
			t.Fatal("sent a request target that could add a header:", bad)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	port := strings.TrimPrefix(c.origin, "http://localhost:")
	want := " host=localhost:" + port + " origin=http://localhost:" + port + " conn=gen-1"
	for _, line := range seen {
		if !strings.Contains(line, want) {
			t.Fatal("request without the controller's host, origin or generation:", line)
		}
	}
	if !strings.Contains(seen[1], "cookie=cage_session_1=s3cret type=application/json") {
		t.Fatal("the session cookie was not kept", seen[1])
	}
	if last := seen[len(seen)-1]; strings.Contains(last, "s3cret") {
		t.Fatal("a cleared cookie was still sent", last)
	}
}

func TestUnreachableControllerIsANetworkError(t *testing.T) {
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c := &Client{origin: "http://localhost:1", host: "localhost:1", addr: addr}
	var networkError *url.Error
	if _, err := c.CallContext(context.Background(), "POST", "/api/launch", nil, nil); !errors.As(err, &networkError) {
		t.Fatal(err)
	}
	if _, err := c.Dial(context.Background(), controlclient.AgentTarget{Scope: "local"}, "/api/x"); err == nil || err.(*controlclient.AgentError).Code != "connection_failed" {
		t.Fatal(err)
	}
}

// A gorilla server with a tiny write buffer splits every message into many
// frames, and pings between them.
func TestSocketReadsFragmentedMessagesAndAnswersPings(t *testing.T) {
	pongs := make(chan string, 16)
	received := make(chan string, 4)
	readerDone := make(chan struct{})
	upgrader := websocket.Upgrader{WriteBufferSize: 64, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "http://"+r.Host }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values(controlclient.ConnectionHeader)) != 1 || r.Header.Get(controlclient.ConnectionHeader) != "local" || r.Header.Get("Cookie") != "a=b" {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.SetPongHandler(func(data string) error { pongs <- data; return nil })
		ws.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"viewer\",\"protocol\":1}"))
		writer, _ := ws.NextWriter(websocket.BinaryMessage)
		for i := 0; i < 50; i++ {
			writer.Write(bytes.Repeat([]byte{byte('a' + i%26)}, 100))
			if i == 10 {
				ws.WriteControl(websocket.PingMessage, []byte("mid"), time.Now().Add(time.Second))
			}
		}
		writer.Close()
		go func() {
			for {
				_, data, err := ws.ReadMessage()
				if err != nil {
					close(readerDone)
					return
				}
				received <- string(data)
			}
		}()
		ws.WriteControl(websocket.PingMessage, []byte("idle"), time.Now().Add(time.Second))
		time.Sleep(200 * time.Millisecond)
		ws.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"exit\"}"))
		ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		<-readerDone
	}))
	defer server.Close()
	c := clientFor(t, server)
	c.ConnectionID = "gen-default" // the target's generation wins, sent once
	if _, err := c.Dial(context.Background(), controlclient.AgentTarget{Scope: "local"}, "/api/stream"); err == nil || err.(*controlclient.AgentError).Code != "authentication_failed" {
		t.Fatal("a refused upgrade is not the agent's 401:", err)
	}
	c.setCookie("a", "b")
	ws, err := c.Dial(context.Background(), controlclient.AgentTarget{Scope: "local"}, "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	kind, hello, err := ws.ReadMessage(1 << 10)
	if err != nil || kind != TextMessage || !strings.Contains(string(hello), "viewer") {
		t.Fatal(kind, string(hello), err)
	}
	kind, r, err := ws.NextReader()
	if err != nil || kind != BinaryMessage {
		t.Fatal(kind, err)
	}
	var got bytes.Buffer
	buf := make([]byte, 7) // smaller than a frame
	if _, err := io.CopyBuffer(&got, struct{ io.Reader }{r}, buf); err != nil || got.Len() != 5000 || got.Bytes()[4999] != 'a'+49%26 {
		t.Fatal(got.Len(), err)
	}
	if ws.WriteMessage(TextMessage, []byte("{\"type\":\"input\",\"data\":\"x\"}")) != nil {
		t.Fatal("write failed")
	}
	_, exit, err := ws.ReadMessage(1 << 10)
	if err != nil || string(exit) != "{\"type\":\"exit\"}" {
		t.Fatal(string(exit), err)
	}
	var closed *CloseError
	if _, _, err := ws.NextReader(); !errors.As(err, &closed) || closed.Code != 1000 {
		t.Fatal("the close frame did not end the stream:", err)
	}
	if first, second := <-pongs, <-pongs; first != "mid" || second != "idle" {
		t.Fatal("pings were not answered with their payload:", first, second)
	}
	if input := <-received; input != "{\"type\":\"input\",\"data\":\"x\"}" {
		t.Fatal("the server read", input)
	}
}

// rawServer answers one upgrade with reply, written in one piece.
func rawServer(t *testing.T, reply func(key string) string) *httptest.Server {
	server := httptest.NewUnstartedServer(nil)
	server.Listener.Close()
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	server.Listener = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				key := ""
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Sec-WebSocket-Key") {
						key = strings.TrimSpace(value)
					}
					if line == "\r\n" {
						break
					}
				}
				conn.Write([]byte(reply(key)))
				io.Copy(io.Discard, r)
			}()
		}
	}()
	server.URL = "http://" + ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	return server
}

func accept(key string) string {
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(digest[:])
}

func TestSocketHandshakeIsChecked(t *testing.T) {
	frame := "\x81\x05hello" // a text frame sent with the handshake
	for name, reply := range map[string]func(string) string{
		"good": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n\r\n" + frame
		},
		"wrong key": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept("x") + "\r\n\r\n"
		},
		"extension": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n"
		},
		"not upgrade": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n\r\n"
		},
		"refused": func(string) string { return "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n" },
		"reserved": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n\r\n\xc1\x01x"
		},
		"masked": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n\r\n\x81\x81abcdx"
		},
	} {
		c := clientFor(t, rawServer(t, reply))
		ws, err := c.Dial(context.Background(), controlclient.AgentTarget{Scope: "local"}, "/api/stream")
		switch name {
		case "good":
			if err != nil {
				t.Fatal(name, err)
			}
			if _, data, err := ws.ReadMessage(10); err != nil || string(data) != "hello" {
				t.Fatal("a frame sent with the handshake was lost:", string(data), err)
			}
			ws.Close()
		case "refused":
			if err == nil || err.(*controlclient.AgentError).Code != "unsupported_operation" {
				t.Fatal(name, err)
			}
		case "reserved", "masked":
			if err != nil {
				t.Fatal(name, err)
			}
			if _, _, err := ws.NextReader(); err == nil {
				t.Fatal(name, "frame was accepted")
			}
			ws.Close()
		default:
			if err == nil {
				ws.Close()
				t.Fatal(name, "was accepted")
			}
		}
	}
}
