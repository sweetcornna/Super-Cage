package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

// A loopback relay that counts what only a joining computer asks for: room
// lookups and client-role joins. A host's own registrations are not counted.
func deviceTestRelay(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	edge, err := relay.New(relay.Config{PublicRooms: true, AdminToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	var joiner atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/invitation" || strings.HasSuffix(r.URL.Path, "/client") {
			joiner.Add(1)
		}
		edge.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { edge.Close(); server.Close() })
	return server.URL, &joiner
}

// A host of today's build: it pairs the joining computer as its one phone
// (mobile_pair.go), keep-awake helper included, stopped at cleanup.
func deviceTestHost(t *testing.T, relayOrigin string) *Server {
	t.Helper()
	s, _ := grantedToolUpdateServer(t)
	s.preferences.MobileRelay = relayOrigin
	server := httptest.NewUnstartedServer(s.handler())
	s.origin = "http://" + server.Listener.Addr().String()
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(s.stopPhone)
	return s
}

func deviceHostCode(t *testing.T, host *Server) string {
	t.Helper()
	w := deviceRequest(host, "POST", "/api/mobile-pair", "{}", deviceCall{})
	var code struct{ Manual string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &code) != nil || !strings.HasPrefix(code.Manual, "CAGE-") {
		t.Fatalf("host pairing code: HTTP %d %s", w.Code, w.Body.String())
	}
	return code.Manual
}

type deviceCall struct {
	agent, untrusted      bool
	connection, phoneRoom string
}

func deviceRequest(s *Server, method, path, body string, call deviceCall) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	if !call.untrusted {
		r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	}
	if call.agent {
		r.Header.Set(agentRequestHeader, "1")
	}
	if call.connection != "" {
		r.Header.Set(connectionHeader, call.connection)
	}
	if call.phoneRoom != "" {
		r.Header.Set(mobilelink.PhoneRoomHeader, call.phoneRoom)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func joinComputerForTest(t *testing.T, s *Server, code string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code, "name": "家里的 Mac mini"})
	w := deviceRequest(s, "POST", "/api/client/devices/join", string(body), deviceCall{})
	var joined struct{ ID, Name string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &joined) != nil || !joinedComputerID.MatchString(joined.ID) || joined.Name != "家里的 Mac mini" {
		t.Fatalf("join: HTTP %d %s", w.Code, w.Body.String())
	}
	return joined.ID
}

func openComputerForTest(t *testing.T, s *Server, id string) string {
	t.Helper()
	w := deviceRequest(s, "POST", "/api/client/devices/"+id+"/open", "{}", deviceCall{})
	var opened struct{ Connection, URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &opened) != nil || !strings.HasPrefix(opened.Connection, "device:"+id+":") || opened.URL != "/mobile.html" {
		t.Fatalf("open: HTTP %d %s", w.Code, w.Body.String())
	}
	return opened.Connection
}

// sessionsThroughLink reads the session list of the joined computer through
// this computer's gateway and returns its revision.
func sessionsThroughLink(t *testing.T, s *Server, connection string) string {
	t.Helper()
	w := deviceRequest(s, "GET", "/api/mobile/cli/sessions", "", deviceCall{connection: connection})
	var list struct{ Revision string }
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Revision == "" {
		t.Fatalf("sessions through the link: HTTP %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	return list.Revision
}

// heldRead starts a held session-list read on the link and reports when it
// ends and how.
func heldRead(s *Server, connection, revision string) <-chan int {
	done := make(chan int, 1)
	go func() {
		w := deviceRequest(s, "GET", "/api/mobile/cli/sessions?after="+revision+"&wait=25", "", deviceCall{connection: connection})
		done <- w.Code
	}()
	return done
}

func cutWithin(t *testing.T, done <-chan int, what string, limit time.Duration) {
	t.Helper()
	select {
	case code := <-done:
		if code == 200 || code == 204 {
			t.Fatalf("%s: the held read finished normally (%d)", what, code)
		}
	case <-time.After(limit):
		t.Fatalf("%s did not cut the held read within %s", what, limit)
	}
}

func readPrivateJoined(t *testing.T, s *Server) joinedComputersRecord {
	t.Helper()
	info, err := os.Stat(s.joinedComputersPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("joined-computers.json: %v %v", info, err)
	}
	var record joinedComputersRecord
	b, _ := os.ReadFile(s.joinedComputersPath())
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// A workload serving its own window joins a host of today's build with the
// long code, as that host's one phone, and opens it in the window.
func TestJoinedComputerOpensTodaysHostAsItsPhone(t *testing.T) {
	relayOrigin, joinerHits := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)
	s := testServer(t)
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(s.remote.close)
	t.Cleanup(s.devices.shutdown)
	s.devices.restAfter = time.Hour
	code := deviceHostCode(t, host)

	// Only the window joins: not an agent, not a session without the view.
	for _, call := range []deviceCall{{agent: true}, {untrusted: true}} {
		if w := deviceRequest(s, "POST", "/api/client/devices/join", `{"code":"`+code+`"}`, call); w.Code != http.StatusForbidden {
			t.Fatalf("join %+v: HTTP %d", call, w.Code)
		}
	}
	// The code says which relay it was made on.
	if w := deviceRequest(s, "POST", "/api/client/devices/join", `{"code":"`+code+`"}`, deviceCall{}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), relayOrigin) {
		t.Fatalf("relay mismatch: HTTP %d %s", w.Code, w.Body.String())
	}
	s.preferences.MobileRelay = relayOrigin
	if joinerHits.Load() != 0 {
		t.Fatal("refused joins reached the relay")
	}
	id := joinComputerForTest(t, s, code)
	record := readPrivateJoined(t, s)
	if len(record.Computers) != 1 || len(record.Computers[0].Seed) != 32 || record.Computers[0].Relay != relayOrigin {
		t.Fatalf("saved record: %+v", record)
	}
	saved := record.Computers[0]
	// Today's host now has its one phone: this computer.
	if w := deviceRequest(host, "GET", "/api/mobile-pair", "", deviceCall{}); !strings.Contains(w.Body.String(), `"paired":true`) {
		t.Fatalf("host status: %s", w.Body.String())
	}
	// A saved, unopened computer costs nothing: no lookups, no tunnel.
	before := joinerHits.Load()
	w := deviceRequest(s, "GET", "/api/client/devices", "", deviceCall{})
	time.Sleep(300 * time.Millisecond)
	if joinerHits.Load() != before {
		t.Fatal("a saved computer reached the relay before it was opened")
	}
	list := w.Body.String()
	if w.Code != 200 || !strings.Contains(list, id) || !strings.Contains(list, `"state":"closed"`) {
		t.Fatalf("list: HTTP %d %s", w.Code, list)
	}
	for _, secret := range []string{saved.Lookup, saved.HostPin, string(mustJSON(saved.Seed)), "seed", "lookup", "host_pin"} {
		if strings.Contains(list, strings.Trim(secret, `"`)) {
			t.Fatalf("the list shows %q", secret)
		}
	}
	if w := deviceRequest(s, "GET", "/api/client/devices", "", deviceCall{agent: true}); w.Code != http.StatusForbidden {
		t.Fatalf("agent list: HTTP %d", w.Code)
	}

	// Opening is the window's too.
	for _, call := range []deviceCall{{agent: true}, {untrusted: true}} {
		if w := deviceRequest(s, "POST", "/api/client/devices/"+id+"/open", "{}", call); w.Code != http.StatusForbidden {
			t.Fatalf("open %+v: HTTP %d", call, w.Code)
		}
	}
	started := time.Now()
	connection := openComputerForTest(t, s, id)
	opened := time.Since(started)
	started = time.Now()
	revision := sessionsThroughLink(t, s, connection)
	t.Logf("loopback relay: open %s, first session list %s", opened.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
	if w := deviceRequest(s, "GET", "/api/client/devices", "", deviceCall{}); !strings.Contains(w.Body.String(), `"state":"open"`) {
		t.Fatalf("list after open: %s", w.Body.String())
	}
	// Only the phone surface crosses the link.
	for _, path := range []string{"/api/settings", "/api/status", "/api/devices"} {
		if w := deviceRequest(s, "GET", path, "", deviceCall{connection: connection}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "手机版功能") {
			t.Fatalf("%s on the link: HTTP %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := deviceRequest(s, "POST", "/api/account/recovery-generate", `{"password":"x"}`, deviceCall{connection: connection}); w.Code != http.StatusConflict {
		t.Fatalf("recovery key on the link: HTTP %d", w.Code)
	}
	if w := deviceRequest(s, "GET", "/api/mobile/not-a-route", "", deviceCall{connection: connection}); w.Code != http.StatusNotFound {
		t.Fatalf("unlisted mobile route: HTTP %d", w.Code)
	}
	// Only this computer's window drives the link: not an agent (it carries
	// the trusted view too), not a phone room of this computer, not a session
	// without the view (an SSH-entry or tenant session).
	for _, call := range []deviceCall{
		{connection: connection, agent: true},
		{connection: connection, untrusted: true},
		{connection: connection, untrusted: true, phoneRoom: "room-proof"},
	} {
		if w := deviceRequest(s, "GET", "/api/mobile/cli/sessions", "", call); w.Code != http.StatusForbidden {
			t.Fatalf("%+v: HTTP %d", call, w.Code)
		}
	}
	// The link's own allowlist refuses what never crosses it.
	s.devices.mu.Lock()
	link := s.devices.links[id]
	s.devices.mu.Unlock()
	if _, err := link.client.Do(context.Background(), "POST", "/api/account/recovery-generate", []byte("{}")); err == nil {
		t.Fatal("the link carried an account route")
	}

	// A local lock closes the link and cuts what is in flight.
	held := heldRead(s, connection, revision)
	time.Sleep(200 * time.Millisecond)
	s.account.lock()
	cutWithin(t, held, "a local lock", 2*time.Second)
	if w := deviceRequest(s, "GET", "/api/mobile/cli/sessions", "", deviceCall{connection: connection}); w.Code != http.StatusConflict {
		t.Fatalf("closed link after lock: HTTP %d", w.Code)
	}
	if w := deviceRequest(s, "GET", "/api/client/devices", "", deviceCall{}); !strings.Contains(w.Body.String(), `"state":"closed"`) {
		t.Fatalf("list after a lock: %s", w.Body.String())
	}
	if w := deviceRequest(s, "POST", "/api/client/devices/"+id+"/open", "{}", deviceCall{}); w.Code != http.StatusLocked {
		t.Fatalf("open while locked: HTTP %d", w.Code)
	}
	s.account.setKey(make([]byte, 32))

	// A new open retires the old generation.
	next := openComputerForTest(t, s, id)
	if w := deviceRequest(s, "GET", "/api/mobile/cli/sessions", "", deviceCall{connection: connection}); w.Code != http.StatusConflict {
		t.Fatalf("stale generation: HTTP %d", w.Code)
	}
	revision = sessionsThroughLink(t, s, next)

	// 返回本机 closes it, with the device connection still on the request.
	held = heldRead(s, next, revision)
	time.Sleep(200 * time.Millisecond)
	if w := deviceRequest(s, "POST", "/api/client/devices/"+id+"/close", "{}", deviceCall{connection: next}); w.Code != 200 {
		t.Fatalf("close: HTTP %d", w.Code)
	}
	cutWithin(t, held, "closing", 2*time.Second)

	// A quiet link closes its tunnel and wakes for the next request.
	s.devices.restAfter = 100 * time.Millisecond
	connection = openComputerForTest(t, s, id)
	sessionsThroughLink(t, s, connection)
	time.Sleep(400 * time.Millisecond)
	s.devices.mu.Lock()
	link = s.devices.links[id]
	s.devices.mu.Unlock()
	link.wake.Lock()
	resting := link.resting
	link.wake.Unlock()
	if !resting {
		t.Fatal("a quiet link kept its tunnel")
	}
	started = time.Now()
	sessionsThroughLink(t, s, connection)
	t.Logf("loopback relay: first read after resting %s", time.Since(started).Round(time.Millisecond))

	// Forgetting it here cuts it at once and drops the record.
	revision = sessionsThroughLink(t, s, connection)
	held = heldRead(s, connection, revision)
	time.Sleep(200 * time.Millisecond)
	if w := deviceRequest(s, "DELETE", "/api/client/devices/"+id, "", deviceCall{}); w.Code != 200 {
		t.Fatalf("forget: HTTP %d", w.Code)
	}
	cutWithin(t, held, "forgetting", 2*time.Second)
	if record := readPrivateJoined(t, s); len(record.Computers) != 0 {
		t.Fatalf("forgotten computer still saved: %+v", record)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// The host removing this computer cuts the link at once; opening it again
// says the computer cannot be reached.
func TestJoinedComputerRemovedByTheHost(t *testing.T) {
	relayOrigin, _ := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)
	s := testServer(t)
	s.preferences.MobileRelay = relayOrigin
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(s.remote.close)
	t.Cleanup(s.devices.shutdown)
	id := joinComputerForTest(t, s, deviceHostCode(t, host))
	connection := openComputerForTest(t, s, id)
	held := heldRead(s, connection, sessionsThroughLink(t, s, connection))
	time.Sleep(200 * time.Millisecond)
	if w := deviceRequest(host, "DELETE", "/api/mobile-pair", "", deviceCall{}); w.Code != 200 {
		t.Fatalf("host removal: HTTP %d", w.Code)
	}
	// The host ends the tunnel at once. A read is the one request the joining
	// side retries (as the phone does): it looks the room up again, finds
	// none, and fails after the lookup's retries (about 8 s).
	cutWithin(t, held, "removal on the host", 15*time.Second)
	if w := deviceRequest(s, "POST", "/api/client/devices/"+id+"/open", "{}", deviceCall{}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "连不上") {
		t.Fatalf("open after removal: HTTP %d %s", w.Code, w.Body.String())
	}
}

type deviceLocalFake struct {
	endpoint *httptest.Server
	unlocked atomic.Bool
	locks    atomic.Int32
}

func newDeviceLocalFake(t *testing.T) *deviceLocalFake {
	f := &deviceLocalFake{}
	f.unlocked.Store(true)
	f.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/account":
			_ = json.NewEncoder(w).Encode(map[string]bool{"unlocked": f.unlocked.Load()})
		case r.Method == "POST" && r.URL.Path == "/api/account/lock":
			f.unlocked.Store(false)
			f.locks.Add(1)
			_, _ = w.Write([]byte("{}"))
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(f.endpoint.Close)
	return f
}

func (f *deviceLocalFake) Connect(ctx context.Context) (*remotehost.Session, error) {
	return &remotehost.Session{Origin: f.endpoint.URL, Client: f.endpoint.Client(), Transport: f.endpoint.Client().Transport, Context: ctx, Manifest: remotehost.PublicManifest{Platform: "linux", InstanceID: strings.Repeat("a", 64)}}, nil
}
func (f *deviceLocalFake) Status(context.Context) (DesktopLocalStatus, error) {
	return DesktopLocalStatus{Known: true, Available: true}, nil
}
func (f *deviceLocalFake) Release(context.Context, DesktopLocalStatus, bool) error { return nil }
func (f *deviceLocalFake) ReleaseOwnedIdle(context.Context, DesktopLocalStatus) (bool, error) {
	return false, nil
}

// joinerProfile writes this computer's relay choice where a gateway or
// client-only process reads it: the workbench.json beside its configuration.
func joinerProfile(t *testing.T, s *Server, relayOrigin string) {
	t.Helper()
	dir := t.TempDir()
	s.appUpdateConfigPath = filepath.Join(dir, "config.json")
	if err := writePrivate(filepath.Join(dir, "workbench.json"), map[string]string{"mobile_relay": relayOrigin}); err != nil {
		t.Fatal(err)
	}
}

// The desktop gateway, which answers the window of the installed app, and
// the client-only process join and open the same way. The gateway has no
// account of its own: the environment it shows decides, and a lock of it
// closes every link.
func TestJoinedComputerThroughDesktopGatewayAndClientOnly(t *testing.T) {
	relayOrigin, _ := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)

	gateway := testServer(t)
	gateway.account = nil
	gateway.desktopEntry = true
	joinerProfile(t, gateway, relayOrigin)
	local := newDeviceLocalFake(t)
	gateway.remote = newDesktopGateway(gateway.profile, local)
	t.Cleanup(gateway.remote.close)
	t.Cleanup(gateway.devices.shutdown)
	view := chooseLocal(t, gateway.remote.desktop, gateway.remote.desktop.generation)

	id := joinComputerForTest(t, gateway, deviceHostCode(t, host))
	connection := openComputerForTest(t, gateway, id)
	sessionsThroughLink(t, gateway, connection)
	if w := deviceRequest(gateway, "GET", "/api/settings", "", deviceCall{connection: connection}); w.Code != http.StatusConflict {
		t.Fatalf("non-mobile route through the gateway: HTTP %d", w.Code)
	}
	if w := deviceRequest(gateway, "GET", "/api/mobile/cli/sessions", "", deviceCall{connection: connection, untrusted: true}); w.Code != http.StatusForbidden {
		t.Fatalf("untrusted view through the gateway: HTTP %d", w.Code)
	}
	if w := deviceRequest(gateway, "GET", "/api/mobile/cli/sessions", "", deviceCall{connection: connection, agent: true}); w.Code != http.StatusForbidden {
		t.Fatalf("agent through the gateway: HTTP %d", w.Code)
	}
	if w := deviceRequest(gateway, "GET", "/mobile.html", "", deviceCall{}); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/mobile.html on the gateway: HTTP %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	// A lock that passes through the gateway closes the link at once.
	held := heldRead(gateway, connection, sessionsThroughLink(t, gateway, connection))
	time.Sleep(200 * time.Millisecond)
	if w := deviceRequest(gateway, "POST", "/api/account/lock", "{}", deviceCall{connection: view}); w.Code != 200 || local.locks.Load() != 1 {
		t.Fatalf("lock through the gateway: HTTP %d, %d locks", w.Code, local.locks.Load())
	}
	cutWithin(t, held, "a lock through the gateway", 2*time.Second)
	if w := deviceRequest(gateway, "POST", "/api/client/devices/"+id+"/open", "{}", deviceCall{}); w.Code != http.StatusLocked {
		t.Fatalf("open while the shown environment is locked: HTTP %d", w.Code)
	}
	// A lock made elsewhere (an agent at the workload) is seen at the next
	// request and closes the link.
	local.unlocked.Store(true)
	gateway.devices.recheck = time.Millisecond
	connection = openComputerForTest(t, gateway, id)
	sessionsThroughLink(t, gateway, connection)
	local.unlocked.Store(false)
	time.Sleep(5 * time.Millisecond)
	if w := deviceRequest(gateway, "GET", "/api/mobile/cli/sessions", "", deviceCall{connection: connection}); w.Code != http.StatusLocked {
		t.Fatalf("request after a lock elsewhere: HTTP %d", w.Code)
	}
	if gateway.devices.lookup(strings.TrimPrefix(connection, deviceConnectionPrefix)) != nil {
		t.Fatal("the link stayed open after the shown environment locked")
	}
	if w := deviceRequest(gateway, "DELETE", "/api/client/devices/"+id, "", deviceCall{}); w.Code != 200 {
		t.Fatalf("forget: HTTP %d", w.Code)
	}

	// The client-only process: no account, and /api/mobile/ answers only
	// for a joined computer.
	if w := deviceRequest(host, "DELETE", "/api/mobile-pair", "", deviceCall{}); w.Code != 200 {
		t.Fatalf("host removal: HTTP %d", w.Code)
	}
	client := testServer(t)
	client.account = nil
	client.clientOnly = true
	joinerProfile(t, client, relayOrigin)
	client.remote = newRemoteGateway(client.profile)
	client.remote.clientOnly = true
	t.Cleanup(client.remote.close)
	t.Cleanup(client.devices.shutdown)
	if w := deviceRequest(client, "GET", "/api/mobile/cli/sessions", "", deviceCall{}); w.Code != http.StatusConflict {
		t.Fatalf("client-only without a device: HTTP %d", w.Code)
	}
	id = joinComputerForTest(t, client, deviceHostCode(t, host))
	connection = openComputerForTest(t, client, id)
	sessionsThroughLink(t, client, connection)
	if w := deviceRequest(client, "GET", "/mobile.html", "", deviceCall{}); w.Code != 200 {
		t.Fatalf("/mobile.html on the client-only process: HTTP %d", w.Code)
	}
}
