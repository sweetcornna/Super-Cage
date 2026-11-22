package tenant

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
)

// entryGuest is the workbench side of the cloud entry in these tests: its
// /api/entry/cloud/ssh records what the gateway sent and echoes binary
// messages; its device route answers a host key.
type entryGuest struct {
	mu       sync.Mutex
	dials    []http.Header
	enrolled []map[string]string
	refuse   int // answer the next dial with this status and a refusal
	sockets  chan *websocket.Conn
}

var entryHostKey = cloudentry.AuthorizedKey(ed25519.PublicKey(strings.Repeat("k", ed25519.PublicKeySize)))

func newEntryGuest(fixture *portalFixture) *entryGuest {
	guest := &entryGuest{sockets: make(chan *websocket.Conn, 16)}
	fixture.runtime.guestHandler = func(id string) http.Handler {
		base := fixtureGuest(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case cloudentry.WorkbenchPath:
				guest.mu.Lock()
				guest.dials = append(guest.dials, r.Header.Clone())
				refuse := guest.refuse
				guest.refuse = 0
				guest.mu.Unlock()
				if r.Header.Get("Cookie") != guestCookie+"="+fixtureSession || r.Header.Get("Origin") != guestOrigin {
					http.Error(w, "wrong fixture credentials", http.StatusUnauthorized)
					return
				}
				if refuse != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(refuse)
					_, _ = w.Write([]byte(`{"code":"locked","message":"Super Cage 已锁定，请在应用中解锁"}`))
					return
				}
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, http.Header{cloudentry.HostKeyHeader: {entryHostKey}})
				if err != nil {
					return
				}
				guest.sockets <- conn
			case cloudentry.DevicesPath:
				var input map[string]string
				if r.Method != http.MethodPost || r.Header.Get("Cookie") != guestCookie+"="+fixtureSession || json.NewDecoder(r.Body).Decode(&input) != nil {
					http.Error(w, "invalid fixture enrolment", http.StatusBadRequest)
					return
				}
				guest.mu.Lock()
				guest.enrolled = append(guest.enrolled, input)
				guest.mu.Unlock()
				jsonReply(w, 200, map[string]string{"host_key": entryHostKey + " comment"})
			default:
				base.ServeHTTP(w, r)
			}
		})
	}
	return guest
}

func (g *entryGuest) socket(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case conn := <-g.sockets:
		t.Cleanup(func() { conn.Close() })
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("the workbench was not dialled")
		return nil
	}
}

func (g *entryGuest) dialCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.dials)
}

// entryOwner is one computer of an account, enrolled for a workspace.
type entryOwner struct {
	key    ed25519.PrivateKey
	device string
	vm     string
}

func newEntryKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (f *portalFixture) enrolEntry(t *testing.T, grant *http.Cookie, vm string, key ed25519.PrivateKey) (*httptest_response, entryOwner) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": "MacBook", "public_key": cloudentry.AuthorizedKey(key.Public().(ed25519.PublicKey)) + " owner@mac"})
	response := f.request("POST", vmOrigin(vm), "/api/client/cloud-entry", string(body), grant)
	var reply struct {
		DeviceID string `json:"device_id"`
		HostKey  string `json:"host_key"`
		VMID     string `json:"vm_id"`
		Host     string `json:"host"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &reply)
	return &httptest_response{status: response.Code, body: response.Body.String(), hostKey: reply.HostKey, host: reply.Host}, entryOwner{key: key, device: reply.DeviceID, vm: vm}
}

type httptest_response struct {
	status        int
	body, hostKey string
	host          string
}

// entryClient dials /_cage/entry the way cage entry proxy does.
type entryClient struct {
	t        *testing.T
	listener *pipeListener
}

func (c entryClient) dial(host string, header http.Header) (*websocket.Conn, int, string) {
	c.t.Helper()
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.listener.dial(ctx)
	}, HandshakeTimeout: 3 * time.Second}
	conn, response, err := dialer.Dial("ws://"+host+cloudentry.Path, header)
	if err != nil {
		if response != nil {
			var refusal struct {
				Code  string
				Error struct{ Code string }
			}
			_ = json.NewDecoder(response.Body).Decode(&refusal)
			if refusal.Code == "" {
				refusal.Code = refusal.Error.Code
			}
			return nil, response.StatusCode, refusal.Code
		}
		c.t.Fatalf("entry handshake failed: %v", err)
	}
	c.t.Cleanup(func() { conn.Close() })
	if response.Header.Get(cloudentry.HostKeyHeader) != entryHostKey {
		c.t.Fatalf("host key %q", response.Header.Get(cloudentry.HostKeyHeader))
	}
	return conn, http.StatusSwitchingProtocols, ""
}

func entryHost(vm string) string { return "vm-" + vm + ".cage.example.test" }

func signed(owner entryOwner, host string, at time.Time) http.Header {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	return http.Header{"Authorization": {cloudentry.Sign(owner.key, owner.device, host, owner.vm, at, nonce).String()}}
}

type entryFixture struct {
	*portalFixture
	guest  *entryGuest
	client entryClient
	alice  *http.Cookie
	grant  *http.Cookie
	owner  entryOwner
}

func newEntryFixture(t *testing.T, base *portalFixture) *entryFixture {
	t.Helper()
	cloudentry.Lift(t)
	if base == nil {
		base = newFixture(t, false)
	}
	guest := newEntryGuest(base)
	alice, _ := base.login("register", "alice")
	record := base.create(alice, "first")
	grant := base.connect(alice, record.ID)
	enrolled, owner := base.enrolEntry(t, grant, record.ID, newEntryKey(t))
	if enrolled.status != 200 || !cloudentry.ValidDevice(owner.device) || enrolled.hostKey != entryHostKey || enrolled.host != entryHost(record.ID) {
		t.Fatalf("enrolment: %d %s", enrolled.status, enrolled.body)
	}
	return &entryFixture{portalFixture: base, guest: guest, client: entryClient{t: t, listener: serveFixture(t, base.server)}, alice: alice, grant: grant, owner: owner}
}

func (f *entryFixture) lastIO() int64 {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return f.server.workspaces[f.owner.vm].lastIO.Load()
}

// Closed (every build until the cloud entry is verified): the gateway answers
// /_cage/entry as not found before it reads a header, whatever they are, the
// device API refuses enrolment and lists nothing, and nothing reaches the
// workbench.
func TestCageEntryClosedGate(t *testing.T) {
	if cloudentry.Open() {
		t.Fatal("this build has the cloud entry open")
	}
	fixture := newFixture(t, false)
	guest := newEntryGuest(fixture)
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "first")
	grant := fixture.connect(alice, record.ID)
	key := newEntryKey(t)
	for _, header := range []http.Header{
		{},
		{"Origin": {"https://evil.example"}, "Sec-Fetch-Site": {"cross-site"}},
		signed(entryOwner{key: key, device: strings.Repeat("a", 32), vm: record.ID}, entryHost(record.ID), time.Now()),
	} {
		request := httptestRequest("GET", entryHost(record.ID), cloudentry.Path, header)
		response := fixture.serve(request)
		wantStatus(t, response, http.StatusNotFound, "not_found")
	}
	enrolled, _ := fixture.enrolEntry(t, grant, record.ID, key)
	if enrolled.status != http.StatusNotFound {
		t.Fatalf("enrolment with the gate closed: %d %s", enrolled.status, enrolled.body)
	}
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/devices", "", alice), http.StatusNotFound, "not_found")
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/devices/"+strings.Repeat("a", 32)+"/revoke", "{}", alice), http.StatusNotFound, "not_found")
	if guest.dialCount() != 0 || len(guest.enrolled) != 0 {
		t.Fatal("the closed gate let a request reach the workbench")
	}
	if fixture.server.entryState.nonces != nil || fixture.server.entryState.devices != nil || fixture.server.store.Devices != nil {
		t.Fatal("the closed gate allocated entry state")
	}
}

func httptestRequest(method, host, path string, header http.Header) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.Host = host
	for name, values := range header {
		request.Header[name] = values
	}
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return request
}

// Admission: signature by the device's registered key, |now - ts| <= 60 s,
// a nonce unseen in 120 s, the device not revoked, its account owning the
// VM; and never a browser.
func TestCageEntryAdmission(t *testing.T) {
	f := newEntryFixture(t, nil)
	host := entryHost(f.owner.vm)
	now := f.server.now()
	other := f.owner
	other.key = newEntryKey(t)
	bob, _ := f.login("register", "bobby")
	bobVM := f.create(bob, "bob's")
	bobGrant := f.connect(bob, bobVM.ID)
	_, bobOwner := f.enrolEntry(t, bobGrant, bobVM.ID, newEntryKey(t))
	if bobOwner.device == "" || bobOwner.device == f.owner.device {
		t.Fatal("bob's device")
	}
	crossTenant := bobOwner
	crossTenant.vm = f.owner.vm
	replayed := signed(f.owner, host, now)
	browser := signed(f.owner, host, now)
	browser.Set("Origin", vmOrigin(f.owner.vm))
	for _, test := range []struct {
		name   string
		header http.Header
		status int
		code   string
	}{
		{"no authorization", http.Header{}, 401, "unauthenticated"},
		{"malformed", http.Header{"Authorization": {"CageEntry device=" + f.owner.device}}, 401, "unauthenticated"},
		{"two authorizations", http.Header{"Authorization": {signed(f.owner, host, now).Get("Authorization"), signed(f.owner, host, now).Get("Authorization")}}, 401, "unauthenticated"},
		{"another key", signed(other, host, now), 401, "unauthenticated"},
		{"signed for another host", signed(f.owner, "vm-"+f.owner.vm+".cage.example.test:8443", now), 401, "unauthenticated"},
		{"signed for another VM", signed(entryOwner{key: f.owner.key, device: f.owner.device, vm: bobVM.ID}, host, now), 401, "unauthenticated"},
		{"too old", signed(f.owner, host, now.Add(-62*time.Second)), 401, "clock"},
		{"too new", signed(f.owner, host, now.Add(62*time.Second)), 401, "clock"},
		{"a browser", browser, 403, "origin_refused"},
		{"another tenant's device", signed(crossTenant, host, now), 403, "not_owner"},
		{"accepted", replayed, 101, ""},
		{"replayed", replayed, 401, "replay"},
		{"skew within a minute", signed(f.owner, host, now.Add(-58*time.Second)), 101, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, status, code := f.client.dial(host, test.header)
			if status != test.status || code != test.code {
				t.Fatalf("got %d %q, want %d %q", status, code, test.status, test.code)
			}
		})
	}
	// The workbench saw the admitted device and its key's fingerprint, set
	// by the gateway, and none of the owner's headers.
	f.guest.mu.Lock()
	dials := slices.Clone(f.guest.dials)
	f.guest.mu.Unlock()
	if len(dials) != 2 {
		t.Fatalf("workbench dialled %d times, want 2", len(dials))
	}
	want := cloudentry.Fingerprint(mustBlob(t, f.owner.key))
	for _, header := range dials {
		if header.Get(cloudentry.HeaderDevice) != f.owner.device || header.Get(cloudentry.HeaderKey) != want ||
			len(header.Values(cloudentry.HeaderDevice)) != 1 || len(header.Values(cloudentry.HeaderKey)) != 1 ||
			header.Get("Authorization") != "" || header.Get(cloudentry.HeaderWake) != "" {
			t.Fatalf("workbench saw %v", header)
		}
	}
	// Revoked from the portal: refused from then on.
	wantStatus(t, f.request("POST", portalOrigin, "/api/tenant/devices/"+f.owner.device+"/revoke", "{}", bob), 404, "not_found")
	response := f.request("POST", portalOrigin, "/api/tenant/devices/"+f.owner.device+"/revoke", "{}", f.alice)
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", response.Code, response.Body.String())
	}
	if _, status, code := f.client.dial(host, signed(f.owner, host, f.server.now())); status != 403 || code != "revoked" {
		t.Fatalf("revoked device: %d %q", status, code)
	}
	// Its key stays refused at enrolment: the desktop makes a new one.
	if again, _ := f.enrolEntry(t, f.grant, f.owner.vm, f.owner.key); again.status != 403 {
		t.Fatalf("revoked key enrolled again: %d %s", again.status, again.body)
	}
}

func mustBlob(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	_, blob, ok := cloudentry.ParseAuthorizedKey(cloudentry.AuthorizedKey(key.Public().(ed25519.PublicKey)))
	if !ok {
		t.Fatal("key")
	}
	return blob
}

// The workbench's refusal reaches the owner in its own words.
func TestCageEntryPassesTheWorkbenchRefusal(t *testing.T) {
	f := newEntryFixture(t, nil)
	f.guest.mu.Lock()
	f.guest.refuse = 423
	f.guest.mu.Unlock()
	host := entryHost(f.owner.vm)
	if _, status, code := f.client.dial(host, signed(f.owner, host, f.server.now())); status != 423 || code != "locked" {
		t.Fatalf("%d %q", status, code)
	}
}

// Idle cooling still works: binary messages in both directions (heartbeats,
// keepalives) and the admission itself never touch(); the workbench's
// activity mark does, at most once per 30 s; text from either side is never
// passed on.
func TestCageEntryRelayTouchesOnlyOnMarks(t *testing.T) {
	f := newEntryFixture(t, nil)
	host := entryHost(f.owner.vm)
	f.server.mu.Lock()
	f.server.workspaces[f.owner.vm].lastIO.Store(1)
	f.server.mu.Unlock()
	client, status, _ := f.client.dial(host, signed(f.owner, host, f.server.now()))
	if status != 101 {
		t.Fatal(status)
	}
	bench := f.guest.socket(t)
	deadline := time.Now().Add(3 * time.Second)
	_ = client.SetReadDeadline(deadline)
	_ = bench.SetReadDeadline(deadline)
	for i := range 20 {
		heartbeat := []byte{0, 0, 0, 1, byte(i)}
		if client.WriteMessage(websocket.BinaryMessage, heartbeat) != nil {
			t.Fatal("client write")
		}
		if kind, data, err := bench.ReadMessage(); err != nil || kind != websocket.BinaryMessage || string(data) != string(heartbeat) {
			t.Fatalf("workbench read %v %q %v", kind, data, err)
		}
		if bench.WriteMessage(websocket.BinaryMessage, heartbeat) != nil {
			t.Fatal("workbench write")
		}
		if kind, data, err := client.ReadMessage(); err != nil || kind != websocket.BinaryMessage || string(data) != string(heartbeat) {
			t.Fatalf("client read %v %q %v", kind, data, err)
		}
	}
	// The owner's text and a forged mark from the owner's side reach nothing.
	_ = client.WriteMessage(websocket.TextMessage, []byte(cloudentry.ActiveMark))
	_ = bench.WriteMessage(websocket.TextMessage, []byte("{\"t\":\"other\"}"))
	_ = client.WriteMessage(websocket.BinaryMessage, []byte("after"))
	if kind, data, err := bench.ReadMessage(); err != nil || kind != websocket.BinaryMessage || string(data) != "after" {
		t.Fatalf("the owner's text reached the workbench: %v %q %v", kind, data, err)
	}
	if got := f.lastIO(); got != 1 {
		t.Fatalf("lastActive moved without a mark (%d)", got)
	}
	_ = bench.WriteMessage(websocket.TextMessage, []byte(cloudentry.ActiveMark))
	_ = bench.WriteMessage(websocket.BinaryMessage, []byte("marked"))
	if kind, data, err := client.ReadMessage(); err != nil || kind != websocket.BinaryMessage || string(data) != "marked" {
		t.Fatalf("a text message reached the owner: %v %q %v", kind, data, err)
	}
	first := f.lastIO()
	if first == 1 {
		t.Fatal("the mark did not touch")
	}
	f.server.mu.Lock()
	f.server.workspaces[f.owner.vm].lastIO.Store(2)
	f.server.mu.Unlock()
	_ = bench.WriteMessage(websocket.TextMessage, []byte(cloudentry.ActiveMark))
	_ = bench.WriteMessage(websocket.BinaryMessage, []byte("again"))
	if _, data, err := client.ReadMessage(); err != nil || string(data) != "again" {
		t.Fatal("relay")
	}
	if got := f.lastIO(); got != 2 {
		t.Fatal("a second mark within 30 s touched again")
	}
	// Closing touches nothing either.
	_ = client.Close()
	waitFixture(t, func() bool {
		_, _, err := bench.ReadMessage()
		return err != nil
	})
	if got := f.lastIO(); got != 2 {
		t.Fatal("the end of the relay touched")
	}
}

// Frozen or stopped: without Cage-Entry-Wake the answer is 409 and nothing
// wakes; with it a frozen workspace resumes and a stopped one starts.
func TestCageEntryWakeRule(t *testing.T) {
	cooling := newCoolingFixture(t)
	f := newEntryFixture(t, cooling.portalFixture)
	host := entryHost(f.owner.vm)
	id := f.owner.vm
	cooling.server.coolOnce()
	cooling.advance(11 * time.Minute)
	cooling.server.coolOnce()
	if cooling.cooler.state(id) != "Frozen" {
		t.Fatal("workspace not frozen")
	}
	lastActive := func() time.Time {
		cooling.server.mu.Lock()
		defer cooling.server.mu.Unlock()
		return cooling.server.lastActiveLocked(id, cooling.server.now())
	}
	before := lastActive()
	for range 3 {
		if _, status, code := f.client.dial(host, signed(f.owner, host, f.server.now())); status != 409 || code != "asleep" {
			t.Fatalf("frozen without wake: %d %q", status, code)
		}
	}
	if cooling.cooler.count("thaw", id) != 0 || !lastActive().Equal(before) || f.guest.dialCount() != 0 {
		t.Fatal("a dial without wake woke or touched the workspace")
	}
	wake := signed(f.owner, host, f.server.now())
	wake.Set(cloudentry.HeaderWake, "1")
	if _, status, _ := f.client.dial(host, wake); status != 101 || cooling.cooler.count("thaw", id) != 1 {
		t.Fatalf("frozen with wake: %d, thaws %d", status, cooling.cooler.count("thaw", id))
	}
	// Stopped: the sweep stops it after an hour idle.
	cooling.advance(2 * time.Hour)
	cooling.server.coolOnce()
	cooling.server.coolOnce()
	waitFixture(t, func() bool { return cooling.cooler.state(id) == "Stopped" })
	cooling.runtime.mu.Lock()
	calls := cooling.runtime.calls[id]
	cooling.runtime.mu.Unlock()
	if _, status, code := f.client.dial(host, signed(f.owner, host, f.server.now())); status != 409 || code != "stopped" {
		t.Fatalf("stopped without wake: %d %q", status, code)
	}
	cooling.runtime.mu.Lock()
	unchanged := cooling.runtime.calls[id] == calls
	cooling.runtime.mu.Unlock()
	if !unchanged {
		t.Fatal("a dial without wake started the workspace")
	}
	wake = signed(f.owner, host, f.server.now())
	wake.Set(cloudentry.HeaderWake, "1")
	if _, status, code := f.client.dial(host, wake); status != 503 || code != "starting" {
		t.Fatalf("stopped with wake: %d %q", status, code)
	}
	waitFixture(t, func() bool {
		cooling.runtime.mu.Lock()
		defer cooling.runtime.mu.Unlock()
		return cooling.runtime.calls[id] == calls+1
	})
}

// Revoking the device, freezing or stopping the workspace cut a live relay at
// once: both sockets end.
func TestCageEntryCuts(t *testing.T) {
	for _, event := range []string{"revoke", "freeze", "stop"} {
		t.Run(event, func(t *testing.T) {
			f := newEntryFixture(t, nil)
			host := entryHost(f.owner.vm)
			client, status, _ := f.client.dial(host, signed(f.owner, host, f.server.now()))
			if status != 101 {
				t.Fatal(status)
			}
			bench := f.guest.socket(t)
			started := time.Now()
			switch event {
			case "revoke":
				if response := f.request("POST", portalOrigin, "/api/tenant/devices/"+f.owner.device+"/revoke", "{}", f.alice); response.Code != 204 {
					t.Fatal(response.Code)
				}
			case "freeze":
				f.server.mu.Lock()
				f.server.workspaces[f.owner.vm].sleep()
				f.server.mu.Unlock()
			case "stop":
				f.server.mu.Lock()
				f.server.workspaces[f.owner.vm].close()
				f.server.mu.Unlock()
			}
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			_ = bench.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err := client.ReadMessage(); err == nil {
				t.Fatal("the owner's socket outlived the cut")
			}
			if _, _, err := bench.ReadMessage(); err == nil {
				t.Fatal("the workbench's socket outlived the cut")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("cut took %s", elapsed)
			}
		})
	}
}

// The normal proxy path never reaches the entry: /api/entry/ is internal for
// every method and upgrade, and the two headers only the gateway sets are
// stripped from every browser request.
func TestCageEntryProxyHardening(t *testing.T) {
	// Always on, whatever the gate.
	for _, lifted := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "lifted"}[lifted], func(t *testing.T) {
			if lifted {
				cloudentry.Lift(t)
			}
			fixture := newFixture(t, false)
			guest := newEntryGuest(fixture)
			alice, _ := fixture.login("register", "alice")
			record := fixture.create(alice, "first")
			grant := fixture.connect(alice, record.ID)
			for _, path := range []string{"/api/entry", "/api/entry/ssh", cloudentry.WorkbenchPath, cloudentry.DevicesPath, "/api/entry/on"} {
				for _, upgrade := range []bool{false, true} {
					request := httptestRequest("GET", entryHost(record.ID), path, http.Header{"Origin": {vmOrigin(record.ID)}})
					if !upgrade {
						request.Header.Del("Upgrade")
						request.Header.Del("Connection")
					}
					request.AddCookie(grant)
					wantStatus(t, fixture.serve(request), 404, "not_found")
				}
				wantStatus(t, fixture.request("POST", vmOrigin(record.ID), path, "{}", grant), 404, "not_found")
			}
			if guest.dialCount() != 0 {
				t.Fatal("the proxy reached the entry endpoint")
			}
			// A browser's forged headers never reach the workbench.
			request := httptestRequest("GET", entryHost(record.ID), "/?echo-entry", http.Header{
				cloudentry.HeaderDevice: {strings.Repeat("a", 32)}, cloudentry.HeaderKey: {"SHA256:forged"}, "X-Cage-Entry": {"phone"},
			})
			request.Header.Del("Upgrade")
			request.Header.Del("Connection")
			request.AddCookie(grant)
			request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh)")
			if response := fixture.serve(request); response.Code != 200 || response.Body.String() != "desktop||"+guestCookie+"="+fixtureSession {
				t.Fatalf("%d %q", response.Code, response.Body.String())
			}
		})
	}
}

// The request headers the proxy forwards are pinned: the list is unchanged
// by the cloud entry (it is an allow-list, so Cage-Entry-Device and
// Cage-Entry-Key were never on it), and the Rewrite deletes exactly those two
// names after it, so a later addition to the list cannot let a browser
// present them.
func TestCageEntryForwardedHeadersArePinned(t *testing.T) {
	want := []string{
		"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "Content-Encoding", "Range", "If-Match", "If-None-Match", "If-Range",
		"If-Modified-Since", "If-Unmodified-Since", "Last-Event-ID", "Connection", "Upgrade",
		"Sec-WebSocket-Extensions", "Sec-WebSocket-Key", "Sec-WebSocket-Protocol", "Sec-WebSocket-Version",
	}
	if !slices.Equal(requestHeaders, want) {
		t.Fatalf("forwarded request headers changed: %v", requestHeaders)
	}
	// Even with both names on the allow-list, the Rewrite drops them.
	saved := requestHeaders
	requestHeaders = append(slices.Clone(saved), cloudentry.HeaderDevice, cloudentry.HeaderKey)
	t.Cleanup(func() { requestHeaders = saved })
	workspace := newWorkspace(45001, func(context.Context) (net.Conn, error) { return nil, errController })
	workspace.cookie = &http.Cookie{Name: guestCookie, Value: fixtureSession}
	proxy := workspace.proxy(guestOrigin, func() {}, func() {})
	in, _ := http.NewRequest("GET", "http://vm.example/x", nil)
	for _, name := range requestHeaders {
		in.Header.Set(name, "v")
	}
	out := in.Clone(context.Background())
	proxy.Rewrite(&httputil.ProxyRequest{In: in, Out: out})
	var forwarded []string
	for name := range out.Header {
		forwarded = append(forwarded, name)
	}
	slices.Sort(forwarded)
	wantOut := []string{"Origin", "User-Agent", "Cookie"}
	for _, name := range want {
		wantOut = append(wantOut, http.CanonicalHeaderKey(name))
	}
	slices.Sort(wantOut)
	if !slices.Equal(forwarded, wantOut) {
		t.Fatalf("forwarded %v, want %v", forwarded, wantOut)
	}
}

// Nothing runs while no cloud entry is connected: after a relay ends the
// goroutines are back to what they were, and the nonce cache is pruned when
// it is used.
func TestCageEntryLeavesNothingRunning(t *testing.T) {
	f := newEntryFixture(t, nil)
	host := entryHost(f.owner.vm)
	runtime.GC()
	before := runtime.NumGoroutine()
	for range 3 {
		client, status, _ := f.client.dial(host, signed(f.owner, host, f.server.now()))
		if status != 101 {
			t.Fatal(status)
		}
		bench := f.guest.socket(t)
		_ = client.Close()
		_ = bench.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			if _, _, err := bench.ReadMessage(); err != nil {
				break
			}
		}
		_ = bench.Close()
	}
	waitFixture(t, func() bool { return runtime.NumGoroutine() <= before })
	// Pruned on use: an entry older than the window goes with the next add.
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if len(f.server.entryState.nonces) != 3 {
		t.Fatalf("%d nonces", len(f.server.entryState.nonces))
	}
	later := time.Now().Add(cloudentry.NonceWindow + time.Second)
	if !f.server.entryNonceLocked("x/y", later) || len(f.server.entryState.nonces) != 1 {
		t.Fatalf("not pruned on use: %d", len(f.server.entryState.nonces))
	}
}

// The signed host is the one the owner dials, port included.
func TestCageEntrySignedHost(t *testing.T) {
	plain, _ := parseSite("https://cage.example.test", false)
	ported, _ := parseSite("https://cage.example.test:9443", false)
	id := strings.Repeat("a", 32)
	if plain.vmHost(id) != "vm-"+id+".cage.example.test" || ported.vmHost(id) != "vm-"+id+".cage.example.test:9443" {
		t.Fatal(plain.vmHost(id), ported.vmHost(id))
	}
}

// Device API: enrolling the same key again finds the same device; the list
// shows the account's own devices only.
func TestCageEntryDeviceAPI(t *testing.T) {
	f := newEntryFixture(t, nil)
	again, owner := f.enrolEntry(t, f.grant, f.owner.vm, f.owner.key)
	if again.status != 200 || owner.device != f.owner.device {
		t.Fatalf("the same key enrolled as %q", owner.device)
	}
	response := f.request("GET", portalOrigin, "/api/tenant/devices", "", f.alice)
	var list struct{ Devices []deviceView }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &list) != nil || len(list.Devices) != 1 || list.Devices[0].ID != f.owner.device ||
		list.Devices[0].Fingerprint != cloudentry.Fingerprint(mustBlob(t, f.owner.key)) || list.Devices[0].Revoked || list.Devices[0].Name != "MacBook" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	bob, _ := f.login("register", "bobby")
	response = f.request("GET", portalOrigin, "/api/tenant/devices", "", bob)
	if response.Code != 200 || strings.Contains(response.Body.String(), f.owner.device) {
		t.Fatal("another account sees alice's device")
	}
	// The workspace's own list got the device and its key.
	f.guest.mu.Lock()
	enrolled := slices.Clone(f.guest.enrolled)
	f.guest.mu.Unlock()
	if len(enrolled) != 2 || enrolled[0]["id"] != f.owner.device || enrolled[0]["public_key"] != cloudentry.AuthorizedKey(f.owner.key.Public().(ed25519.PublicKey)) {
		t.Fatalf("%v", enrolled)
	}
	// It survives a restart, and the state file names no device before the
	// first enrolment (devices.go).
	f.server.Close()
	f.openServer()
	if _, ok := f.server.store.Devices[f.owner.device]; !ok {
		t.Fatal("device not saved")
	}
}
