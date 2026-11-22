//go:build !windows

package workbench

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
)

// cloudEntryFixture is a cloud workspace's workbench in a build with the
// cloud entry, one device enrolled as the gateway enrols it.
type cloudEntryFixture struct {
	*entryFixture
	id  string
	key ssh.Signer
}

func newCloudEntryFixture(t *testing.T) *cloudEntryFixture {
	t.Helper()
	cloudentry.Lift(t)
	entryRealHomeGuard(t)
	// The workbench is a cloud workspace's from the start: its routes are
	// registered when the listener's handler is built.
	s := testServer(t)
	s.tenantWorkspace = true
	f := &cloudEntryFixture{entryFixture: newEntryFixtureFor(t, entryTestControllerOn(t, agentTestControllerOn(t, s)), true), id: strings.Repeat("d1", 16)}
	f.key = entryWriteKey(t, filepath.Join(f.dir, "cloud_ed25519"))
	f.hostKey = f.enrol(f.id, f.key.PublicKey())
	return f
}

// enrol is the gateway's call on its control connection.
func (f *cloudEntryFixture) enrol(id string, key ssh.PublicKey) ssh.PublicKey {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id, "name": "工作电脑", "public_key": string(ssh.MarshalAuthorizedKey(key))})
	w := request(f.s, "POST", cloudentry.DevicesPath, f.s.origin, string(body), true)
	var answer struct {
		HostKey string `json:"host_key"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil {
		f.t.Fatalf("enrolment: %d %s", w.Code, w.Body)
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(answer.HostKey))
	if err != nil {
		f.t.Fatal(err)
	}
	return hostKey
}

// header is what the gateway sets for an admitted device.
func (f *cloudEntryFixture) header(id string, key ssh.PublicKey) http.Header {
	return http.Header{cloudentry.HeaderDevice: {id}, cloudentry.HeaderKey: {cloudentry.Fingerprint(key.Marshal())}}
}

// dial opens the endpoint as the gateway's dialEntry does: the workspace's
// Origin and cookie, and header.
func (f *cloudEntryFixture) dial(header http.Header) (*websocket.Conn, *http.Response, error) {
	h := header.Clone()
	h.Set("Origin", f.s.origin)
	h.Set("Cookie", f.s.cookieName()+"="+f.s.session)
	return websocket.DefaultDialer.Dial("ws://"+strings.TrimPrefix(f.s.origin, "http://")+cloudentry.WorkbenchPath, h)
}

// client dials and starts an SSH client with key over the WebSocket, as
// the relay and cage-cloud-entry carry it.
func (f *cloudEntryFixture) client(header http.Header, key ssh.Signer) (*ssh.Client, *cloudSocket, error) {
	ws, response, err := f.dial(header)
	if err != nil {
		return nil, nil, err
	}
	if line := response.Header.Get(cloudentry.HostKeyHeader); line != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(f.hostKey))) {
		ws.Close()
		return nil, nil, errors.New("host key header " + line)
	}
	socket := &cloudSocket{ws: ws}
	config := &ssh.ClientConfig{User: "whoever", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(f.hostKey), Timeout: 10 * time.Second}
	conn, channels, requests, err := ssh.NewClientConn(socket, "cage-cloud-test", config)
	if err != nil {
		socket.Close()
		return nil, nil, err
	}
	client := ssh.NewClient(conn, channels, requests)
	f.t.Cleanup(func() { client.Close() })
	return client, socket, nil
}

// cloudSocket is the device's end of the splice: binary messages carry the
// SSH bytes; the text messages are the activity marks the gateway reads.
type cloudSocket struct {
	ws      *websocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
	marks   atomic.Int32
	other   atomic.Int32
}

func (c *cloudSocket) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			kind, reader, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				if data, _ := io.ReadAll(reader); string(data) == cloudentry.ActiveMark {
					c.marks.Add(1)
				} else {
					c.other.Add(1)
				}
				continue
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

func (c *cloudSocket) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *cloudSocket) Close() error                     { return c.ws.Close() }
func (c *cloudSocket) LocalAddr() net.Addr              { return c.ws.LocalAddr() }
func (c *cloudSocket) RemoteAddr() net.Addr             { return c.ws.RemoteAddr() }
func (c *cloudSocket) SetDeadline(time.Time) error      { return nil }
func (c *cloudSocket) SetReadDeadline(time.Time) error  { return nil }
func (c *cloudSocket) SetWriteDeadline(time.Time) error { return nil }

// The endpoint takes the device and key the gateway admitted, each exactly
// once, and only a device of its own list with that very key; the SSH key
// must be that key too.
func TestEntryCloudBindsTheGatewayHeaders(t *testing.T) {
	f := newCloudEntryFixture(t)
	st, err := entryLoadState(f.s.profile)
	if err != nil || !st.Enabled || st.device(f.id) == nil {
		t.Fatal("the enrolment did not turn the entry on with the device", err)
	}
	other := entryWriteKey(t, filepath.Join(f.dir, "other_ed25519"))
	good := f.header(f.id, f.key.PublicKey())
	refused := map[string]http.Header{
		"no headers":         {},
		"another key":        f.header(f.id, other.PublicKey()),
		"unknown device":     f.header(strings.Repeat("e2", 16), f.key.PublicKey()),
		"invalid device":     f.header("../"+f.id, f.key.PublicKey()),
		"two devices":        {cloudentry.HeaderDevice: {f.id, f.id}, cloudentry.HeaderKey: good[cloudentry.HeaderKey]},
		"two keys":           {cloudentry.HeaderDevice: good[cloudentry.HeaderDevice], cloudentry.HeaderKey: {good.Get(cloudentry.HeaderKey), good.Get(cloudentry.HeaderKey)}},
		"key without scheme": {cloudentry.HeaderDevice: {f.id}, cloudentry.HeaderKey: {strings.TrimPrefix(good.Get(cloudentry.HeaderKey), "SHA256:")}},
	}
	for name, header := range refused {
		_, response, err := f.dial(header)
		if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %v %v", name, err, response)
		}
		var refusal entryRefusal
		if json.NewDecoder(response.Body).Decode(&refusal) != nil || refusal.Code != "device_unknown" {
			t.Fatalf("%s: %+v", name, refusal)
		}
	}
	agent := good.Clone()
	agent.Set(agentRequestHeader, "1")
	if _, response, _ := f.dial(agent); response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("an agent request reached the endpoint")
	}
	// Admitted, the SSH key must still be the enrolled one.
	if _, _, err := f.client(good, other); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("another SSH key under the admitted headers: %v", err)
	}
	client, _, err := f.client(good, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if stdout, _, status := entryRun(t, client, "echo cloud", nil, nil); stdout != "cloud\n" || status != 0 {
		t.Fatalf("%q %d", stdout, status)
	}
	// A known id keeps its key: the gateway cannot swap it under the id.
	body, _ := json.Marshal(map[string]string{"id": f.id, "name": "x", "public_key": string(ssh.MarshalAuthorizedKey(other.PublicKey()))})
	if w := request(f.s, "POST", cloudentry.DevicesPath, f.s.origin, string(body), true); w.Code != http.StatusConflict {
		t.Fatalf("a second key under a known id: %d", w.Code)
	}
	for name, input := range map[string]map[string]string{
		"rsa key":     {"id": strings.Repeat("f3", 16), "name": "x", "public_key": "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC"},
		"bad id":      {"id": "x", "name": "x", "public_key": string(ssh.MarshalAuthorizedKey(other.PublicKey()))},
		"no name":     {"id": strings.Repeat("f3", 16), "name": "", "public_key": string(ssh.MarshalAuthorizedKey(other.PublicKey()))},
		"agent asked": nil,
	} {
		w := httptest.NewRecorder()
		if input == nil {
			r := httptest.NewRequest("POST", f.s.origin+cloudentry.DevicesPath, strings.NewReader("{}"))
			r.Header.Set("Origin", f.s.origin)
			r.Header.Set(agentRequestHeader, "1")
			r.AddCookie(&http.Cookie{Name: f.s.cookieName(), Value: f.s.session})
			f.s.handler().ServeHTTP(w, r)
		} else {
			body, _ := json.Marshal(input)
			w = request(f.s, "POST", cloudentry.DevicesPath, f.s.origin, string(body), true)
		}
		if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
}

// Keepalives never mark; a command the owner starts does, once per
// cloudentry.MarkInterval at most; nothing else comes as text.
func TestEntryCloudMarksOwnerActivityOnly(t *testing.T) {
	f := newCloudEntryFixture(t)
	client, socket, err := f.client(f.header(f.id, f.key.PublicKey()), f.key)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := socket.marks.Load(); n != 0 {
		t.Fatalf("keepalives sent %d marks", n)
	}
	if stdout, _, _ := entryRun(t, client, "echo one", nil, nil); stdout != "one\n" {
		t.Fatalf("%q", stdout)
	}
	entryAwait(t, 5*time.Second, "the activity mark", func() bool { return socket.marks.Load() == 1 })
	if stdout, _, _ := entryRun(t, client, "cat", []byte("typed"), nil); stdout != "typed" {
		t.Fatalf("%q", stdout)
	}
	time.Sleep(200 * time.Millisecond)
	if socket.marks.Load() != 1 || socket.other.Load() != 0 {
		t.Fatalf("marks %d, other text %d: more than one mark per interval", socket.marks.Load(), socket.other.Load())
	}
}

// What reads the owner's input marks only a cloud connection, and only
// reads that carried bytes; the local entry's readers are the readers
// themselves.
func TestEntryCloudActiveReader(t *testing.T) {
	local := &entryConn{}
	source := strings.NewReader("ls\r")
	if local.activeReader(source) != io.Reader(source) {
		t.Fatal("the local entry's input is wrapped")
	}
	local.noteActive()
	var marks atomic.Int32
	cloud := &entryConn{cloudKey: "SHA256:x", active: func() { marks.Add(1) }}
	reader := cloud.activeReader(strings.NewReader("ls\r"))
	buffer := make([]byte, 8)
	if n, _ := reader.Read(buffer); n != 3 || marks.Load() != 1 {
		t.Fatalf("read %d, marks %d", n, marks.Load())
	}
	if n, _ := reader.Read(buffer); n != 0 || marks.Load() != 1 {
		t.Fatal("an empty read marked")
	}
}

// markActive writes one text message per cloudentry.MarkInterval, between
// the binary messages and never inside one.
func TestEntryCloudMarkRate(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			accepted <- ws
		}
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ws := <-accepted
	defer ws.Close()
	conn := newEntryWSConn(ws)
	for range 3 {
		conn.markActive()
		_, _ = conn.Write([]byte("bytes"))
	}
	// An interval later another mark goes.
	conn.marked.Store(time.Now().Add(-cloudentry.MarkInterval - time.Second).UnixNano())
	conn.markActive()
	texts, binaries := 0, 0
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	for texts < 2 || binaries < 3 {
		kind, data, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("%d marks, %d writes: %v", texts, binaries, err)
		}
		switch {
		case kind == websocket.TextMessage && string(data) == cloudentry.ActiveMark:
			texts++
		case kind == websocket.BinaryMessage && string(data) == "bytes":
			binaries++
		default:
			t.Fatalf("unexpected message %d %q", kind, data)
		}
	}
	_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, data, err := client.ReadMessage(); err == nil {
		t.Fatalf("a further message %q", data)
	}
}
