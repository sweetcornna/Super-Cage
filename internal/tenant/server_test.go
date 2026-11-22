package tenant

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
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
)

const (
	portalOrigin = "https://cage.example.test"
	password     = "  测试 password  "
	guestOrigin  = "http://localhost:45001"
	guestCookie  = "cage_session_45001"
)

var (
	fixtureLauncher = strings.Repeat("a", 64)
	fixtureTicket   = strings.Repeat("b", 64)
	fixtureSession  = strings.Repeat("c", 64)
)

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
	dials       atomic.Int32
}

func serveFixture(t *testing.T, handler http.Handler) *pipeListener {
	t.Helper()
	listener := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "fixture", Net: "unix"} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.connections <- server:
		l.dials.Add(1)
		return client, nil
	case <-l.closed:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	}
}

type fakeRuntime struct {
	t            *testing.T
	mu           sync.Mutex
	guests       map[string]*pipeListener
	calls        map[string]int
	provisions   map[string]int
	specs        map[string]LaunchSpec
	gate         chan struct{}
	entered      chan string
	nextError    atomic.Bool
	imageMissing atomic.Bool
	guestHandler func(string) http.Handler
	// wrapConn, when set, wraps every guest connection of new workspaces.
	wrapConn func(net.Conn) net.Conn
}

func (runtime *fakeRuntime) Prepare(ctx context.Context, spec LaunchSpec) (*Workspace, error) {
	id := spec.VMID
	runtime.mu.Lock()
	runtime.calls[id]++
	runtime.specs[id] = spec
	runtime.mu.Unlock()
	if runtime.entered != nil {
		runtime.entered <- id
	}
	if runtime.gate != nil {
		select {
		case <-runtime.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if runtime.nextError.Swap(false) {
		return nil, errors.New("private runtime details")
	}
	runtime.mu.Lock()
	guest := runtime.guests[id]
	if guest == nil {
		if runtime.imageMissing.Load() {
			runtime.mu.Unlock()
			return nil, errors.New("private missing image detail")
		}
		handler := fixtureGuest(id)
		if runtime.guestHandler != nil {
			handler = runtime.guestHandler(id)
		}
		guest = serveFixture(runtime.t, handler)
		runtime.guests[id] = guest
		runtime.provisions[id]++
	}
	runtime.mu.Unlock()
	dial := guest.dial
	if runtime.wrapConn != nil {
		dial = func(ctx context.Context) (net.Conn, error) {
			conn, err := guest.dial(ctx)
			if err == nil {
				conn = runtime.wrapConn(conn)
			}
			return conn, err
		}
	}
	workspace := newWorkspace(45001, dial)
	if err := workspace.bootstrap(ctx, fixtureLauncher); err != nil {
		workspace.close()
		return nil, err
	}
	return workspace, nil
}

func fixtureGuest(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost:45001" || r.Header.Get("Origin") != guestOrigin {
			http.Error(w, "wrong fixture origin", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/api/launch" || r.URL.Path == "/api/session" {
			var input struct {
				Token string `json:"token"`
			}
			if r.Method != http.MethodPost || r.Header.Get("Cookie") != "" || json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid fixture handshake", http.StatusForbidden)
				return
			}
			if r.URL.Path == "/api/launch" && input.Token == fixtureLauncher {
				jsonReply(w, 200, map[string]string{"url": guestOrigin + "/?launch=fixture#" + fixtureTicket})
				return
			}
			if r.URL.Path == "/api/session" && input.Token == fixtureTicket {
				http.SetCookie(w, &http.Cookie{Name: guestCookie, Value: fixtureSession, Path: "/"})
				jsonReply(w, 200, map[string]bool{"ok": true})
				return
			}
		}
		if r.Header.Get("Cookie") != guestCookie+"="+fixtureSession {
			http.Error(w, "wrong fixture cookie", http.StatusUnauthorized)
			return
		}
		if r.URL.RawQuery == "echo-entry" {
			_, _ = io.WriteString(w, strings.Join(r.Header.Values("X-Cage-Entry"), ",")+"|"+r.Header.Get("User-Agent")+"|"+r.Header.Get("Cookie"))
			return
		}
		switch r.URL.Path {
		case "/api/client/about":
			jsonReply(w, 200, map[string]string{"version": "fixture"})
		case "/api/account":
			if r.Method != http.MethodGet {
				http.Error(w, "fixture account is read-only", http.StatusMethodNotAllowed)
				return
			}
			jsonReply(w, 200, map[string]any{"exists": true, "unlocked": true, "username": "cage"})
		case "/api/echo/file name":
			for _, header := range []string{"Authorization", "Forwarded", "X-Forwarded-For", "X-Real-IP", "Referer", "X-Cage-Connection", "X-Cage-Agent-Window"} {
				if r.Header.Get(header) != "" {
					http.Error(w, "unexpected inbound fixture header", http.StatusForbidden)
					return
				}
			}
			w.Header().Set("Link", "</other>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Set("Set-Cookie", guestCookie+"="+fixtureSession)
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
			w.Header().Set("Cache-Control", "public")
			w.Header().Set("Trailer", "X-Fixture-Trailer")
			defer w.Header().Set("X-Fixture-Trailer", fixtureSession)
			body, _ := io.ReadAll(r.Body)
			jsonReply(w, 200, map[string]string{"id": id, "method": r.Method, "path": r.URL.EscapedPath(), "query": r.URL.RawQuery, "body": string(body)})
		case "/local-redirect":
			http.Redirect(w, r, guestOrigin+"/welcome", 302)
		case "/outside-redirect":
			w.Header().Set("Location", "https://example.invalid/")
			w.WriteHeader(302)
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: "+id+"\n\n")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		case "/socket":
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if conn.WriteMessage(websocket.TextMessage, []byte(id)) != nil {
				return
			}
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil || conn.WriteMessage(kind, data) != nil {
					return
				}
			}
		default:
			_, _ = io.WriteString(w, id)
		}
	})
}

// fixtureAccountGuest models the parts of the workbench account API that the
// tenant gateway uses, while delegating all other paths to fixtureGuest.
type fixtureAccountGuest struct {
	mu       sync.Mutex
	username string
	password string
	exists   bool
	unlocked bool
	reads    int
	creates  int
	unlocks  int
}

func (a *fixtureAccountGuest) handler(id string) http.Handler {
	base := fixtureGuest(id)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/account" && !strings.HasPrefix(r.URL.Path, "/api/account/") {
			base.ServeHTTP(w, r)
			return
		}
		if r.Host != "localhost:45001" || r.Header.Get("Origin") != guestOrigin || r.Header.Get("Cookie") != guestCookie+"="+fixtureSession {
			http.Error(w, "wrong fixture account request", http.StatusUnauthorized)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		switch r.URL.Path {
		case "/api/account":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			a.reads++
			jsonReply(w, 200, map[string]any{"exists": a.exists, "unlocked": a.unlocked, "username": a.username})
		case "/api/account/create":
			var input struct {
				Username        string `json:"username"`
				Password        string `json:"password"`
				ConfirmPassword string `json:"confirm_password"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil || input.Username == "" || input.Password != input.ConfirmPassword || len(input.Password) < 8 {
				http.Error(w, "invalid account", http.StatusBadRequest)
				return
			}
			if a.exists {
				http.Error(w, "已有账户", http.StatusBadRequest)
				return
			}
			a.exists, a.unlocked, a.username, a.password = true, true, input.Username, input.Password
			a.creates++
			jsonReply(w, 200, map[string]any{"exists": true, "unlocked": true, "username": a.username, "recovery_key": strings.Repeat("f", 64)})
		case "/api/account/unlock":
			var input struct {
				Password string `json:"password"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid account", http.StatusBadRequest)
				return
			}
			a.unlocks++
			if !a.exists || input.Password != a.password {
				http.Error(w, "密码不正确", http.StatusBadRequest)
				return
			}
			a.unlocked = true
			jsonReply(w, 200, map[string]any{"exists": true, "unlocked": true, "username": a.username})
		default:
			http.NotFound(w, r)
		}
	})
}

func (a *fixtureAccountGuest) snapshot() (exists, unlocked bool, username, password string, reads, creates, unlocks int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exists, a.unlocked, a.username, a.password, a.reads, a.creates, a.unlocks
}

func (a *fixtureAccountGuest) setLocked() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unlocked = false
}

func (a *fixtureAccountGuest) setExistingAccount(username, password string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.username, a.password, a.exists, a.unlocked = username, password, true, false
}

type portalFixture struct {
	t       *testing.T
	server  *Server
	runtime *fakeRuntime
	config  Config
}

func newFixture(t *testing.T, gated bool) *portalFixture {
	t.Helper()
	runtime := &fakeRuntime{t: t, guests: make(map[string]*pipeListener), calls: make(map[string]int), provisions: make(map[string]int), specs: make(map[string]LaunchSpec)}
	if gated {
		runtime.gate = make(chan struct{})
	}
	config := Config{Origin: portalOrigin, EnablePreviewRegistration: true, StateDir: filepath.Join(t.TempDir(), "state"), Runtime: runtime, AccountVMLimit: 2, TotalVMLimit: 4, ParallelStarts: 2,
		Systems: []System{DefaultSystem(strings.Repeat("d", 64)), {systemOption: systemOption{ID: "ubuntu-2404", Label: "Ubuntu 24.04"}, Image: strings.Repeat("e", 64)}},
		UI:      fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Portal")}, "private.txt": &fstest.MapFile{Data: []byte("private")}}}
	fixture := &portalFixture{t: t, runtime: runtime, config: config}
	fixture.openServer()
	return fixture
}

func (f *portalFixture) openServer() {
	f.t.Helper()
	server, err := NewServer(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	f.server = server
	f.t.Cleanup(server.Close)
}

func (f *portalFixture) request(method, origin, path, body string, cookie *http.Cookie, expectedUsers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	parsed, _ := url.Parse(origin)
	request.Host = parsed.Host
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for _, expected := range expectedUsers {
		request.Header.Add("X-Cage-Expected-User", expected)
	}
	return f.serve(request)
}

func (f *portalFixture) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, request)
	return response
}

func wantStatus(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d, want %d: %s", response.Code, status, response.Body.String())
	}
	if code != "" {
		var body struct {
			Error struct {
				Code    string
				Message string
			}
		}
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != code || body.Error.Message == "" {
			t.Fatalf("invalid error response: %s", response.Body.String())
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("response can be cached")
	}
}

func onlyCookie(t *testing.T, response *httptest.ResponseRecorder, name string, secure bool) *http.Cookie {
	t.Helper()
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != name || !cookies[0].HttpOnly || cookies[0].Secure != secure || cookies[0].Domain != "" || cookies[0].Path != "/" || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("wrong scope or attributes for response cookie")
	}
	return cookies[0]
}

func (f *portalFixture) login(action, username string) (*http.Cookie, user) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	response := f.request("POST", portalOrigin, "/api/tenant/"+action, string(body), nil)
	want := http.StatusOK
	if action == "register" {
		want = http.StatusCreated
	}
	wantStatus(f.t, response, want, "")
	var output struct{ User user }
	if json.Unmarshal(response.Body.Bytes(), &output) != nil || output.User.ID == "" {
		f.t.Fatal("no signed-in account")
	}
	return onlyCookie(f.t, response, "__Host-cage-tenant", true), output.User
}

func (f *portalFixture) create(login *http.Cookie, name string) vm {
	f.t.Helper()
	return f.createWithSystem(login, name, defaultSystemID)
}

func (f *portalFixture) createWithSystem(login *http.Cookie, name, id string) vm {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "system_id": id})
	response := f.request("POST", portalOrigin, "/api/tenant/vms", string(body), login)
	wantStatus(f.t, response, 201, "")
	var output struct{ VM vm }
	if json.Unmarshal(response.Body.Bytes(), &output) != nil || !vmIDPattern.MatchString(output.VM.ID) || output.VM.State != stopped || output.VM.Name != strings.TrimSpace(name) || output.VM.SystemID != id {
		f.t.Fatal("invalid new workspace record")
	}
	return output.VM
}

func (f *portalFixture) list(login *http.Cookie) []vm {
	f.t.Helper()
	response := f.request("GET", portalOrigin, "/api/tenant/vms", "", login)
	wantStatus(f.t, response, 200, "")
	var output struct {
		VMs   []vm
		Limit int
	}
	if json.Unmarshal(response.Body.Bytes(), &output) != nil || output.VMs == nil || output.Limit != f.config.AccountVMLimit {
		f.t.Fatal("invalid VM list")
	}
	return output.VMs
}

func waitFixture(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	ticks := time.NewTicker(5 * time.Millisecond)
	defer ticks.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("fixture condition timed out")
		case <-ticks.C:
		}
	}
}

func (f *portalFixture) state(login *http.Cookie, id, state string) {
	f.t.Helper()
	waitFixture(f.t, func() bool {
		for _, record := range f.list(login) {
			if record.ID == id && record.State == state {
				return true
			}
		}
		return false
	})
}

func openPath(id string) string { return "/api/tenant/vms/" + id + "/open" }

func vmOrigin(id string) string { return "https://vm-" + id + ".cage.example.test" }

func (f *portalFixture) ticket(login *http.Cookie, id string) string {
	f.t.Helper()
	response := f.request("POST", portalOrigin, openPath(id), "{}", login)
	if response.Code == 202 {
		f.state(login, id, ready)
		response = f.request("POST", portalOrigin, openPath(id), "{}", login)
	}
	wantStatus(f.t, response, 200, "")
	var output struct {
		URL string `json:"workspace_url"`
	}
	if json.Unmarshal(response.Body.Bytes(), &output) != nil {
		f.t.Fatal("invalid ticket response")
	}
	token, ok := strings.CutPrefix(output.URL, vmOrigin(id)+"/_tenant/enter#")
	if !ok || !secretPattern.MatchString(token) {
		f.t.Fatal("invalid ticket placement or VM origin")
	}
	return token
}

func (f *portalFixture) enter(id, token string) *httptest.ResponseRecorder {
	return f.request("POST", vmOrigin(id), "/_tenant/enter", `{"ticket":"`+token+`"}`, nil)
}

func (f *portalFixture) connect(login *http.Cookie, id string) *http.Cookie {
	f.t.Helper()
	response := f.enter(id, f.ticket(login, id))
	wantStatus(f.t, response, 204, "")
	return onlyCookie(f.t, response, "__Host-cage-workspace", true)
}

func TestTenantBrowserContextUsesCloudWorkbench(t *testing.T) {
	fixture := newFixture(t, false)
	login, _ := fixture.login("register", "alice")
	a, b := fixture.create(login, "first"), fixture.create(login, "second")
	cookie := fixture.connect(login, a.ID)
	const endpoint = "/api/client/context"
	wantStatus(t, fixture.request("GET", vmOrigin(a.ID), endpoint, "", nil), 401, "unauthenticated")
	wantStatus(t, fixture.request("GET", vmOrigin(b.ID), endpoint, "", cookie), 401, "unauthenticated")
	response := fixture.request("GET", vmOrigin(a.ID), endpoint, "", cookie)
	wantStatus(t, response, 200, "")
	var view struct {
		Mode            string `json:"mode"`
		ConnectionID    string `json:"connection_id"`
		ServerPlatform  string `json:"server_platform"`
		TenantWorkspace bool   `json:"tenant_workspace"`
		VirtualMachines bool   `json:"virtual_machines"`
		PortalOrigin    string `json:"portal_origin"`
		DesktopGateway  bool   `json:"desktop_gateway"`
		Name            string `json:"name"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || view.Mode != "local" || view.ConnectionID != "local" || view.ServerPlatform != "linux" || !view.TenantWorkspace || !view.VirtualMachines || view.PortalOrigin != portalOrigin || view.DesktopGateway || view.Name != "first" {
		t.Fatalf("browser workspace context = %+v, %v", view, err)
	}
	wantStatus(t, fixture.request("POST", vmOrigin(a.ID), "/api/client/about", "{}", cookie), 404, "not_found")
}

func TestTenantExpectedUserGuardsSessionAndVMs(t *testing.T) {
	fixture := newFixture(t, false)
	alice, aliceUser := fixture.login("register", "alice")
	bob, bobUser := fixture.login("register", "bob")
	aliceVM := fixture.create(alice, "Alice workspace")
	bobVM := fixture.create(bob, "Bob workspace")
	bobTicket := fixture.ticket(bob, bobVM.ID)
	before, err := os.ReadFile(filepath.Join(fixture.config.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/tenant/systems", ""},
		{"GET", "/api/tenant/vms", ""},
		{"POST", "/api/tenant/vms", `{"name":"New workspace","system_id":"debian-12"}`},
		{"POST", openPath(bobVM.ID), "{}"},
		{"POST", "/api/tenant/logout", "{}"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response := fixture.request(route.method, portalOrigin, route.path, route.body, bob, aliceUser.ID)
			wantStatus(t, response, 409, "session_changed")
			if response.Header().Get("Set-Cookie") != "" || strings.Contains(response.Body.String(), bobVM.ID) {
				t.Fatal("mismatch modified the session cookie or disclosed a VM")
			}
			wantStatus(t, fixture.request(route.method, portalOrigin, route.path, route.body, nil, bobUser.ID), 401, "unauthenticated")
		})
	}
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/vms", "", bob, ""), 409, "session_changed")
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/vms", "", bob, bobUser.ID, bobUser.ID), 409, "session_changed")
	after, err := os.ReadFile(filepath.Join(fixture.config.StateDir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("mismatch changed persisted state: %v", err)
	}
	fixture.server.mu.Lock()
	issuedTickets, openCount := len(fixture.server.tickets), fixture.server.rates[rateKey{"open", bobUser.ID}].used
	fixture.server.mu.Unlock()
	if issuedTickets != 1 || openCount != 2 {
		t.Fatal("mismatch issued a ticket or consumed the other account's open quota")
	}
	wantStatus(t, fixture.enter(bobVM.ID, bobTicket), 204, "")
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/vms", "", bob, bobUser.ID), 200, "")
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(aliceVM.ID), "{}", alice, aliceUser.ID), 202, "")
	logout := fixture.request("POST", portalOrigin, "/api/tenant/logout", "{}", bob, bobUser.ID)
	wantStatus(t, logout, 204, "")
	newAlice, _ := fixture.login("login", "alice")
	if logout.Header().Get("Set-Cookie") != "" {
		t.Fatal("a delayed logout response could overwrite the newer login cookie")
	}
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/session", "", bob), 401, "unauthenticated")
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/session", "", newAlice), 200, "")
}

func TestTenantRegistrationModes(t *testing.T) {
	fixture := newFixture(t, false)
	fixture.login("register", "alice")
	fixture.server.Close()
	fixture.config.EnablePreviewRegistration = false
	fixture.openServer()
	alice, _ := fixture.login("login", "alice")
	body := `{"username":"bob","password":"a long sample password"}`
	response := fixture.request("POST", portalOrigin, "/api/tenant/register", body, alice)
	wantStatus(t, response, 403, "registration_closed")
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("closed registration modified the existing session cookie")
	}
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/session", "", alice), 200, "")
	fixture.server.Close()
	fixture.config.Development, fixture.config.Origin = true, "http://cage.localhost:8080"
	fixture.openServer()
	response = fixture.request("POST", fixture.config.Origin, "/api/tenant/register", body, nil)
	wantStatus(t, response, 201, "")
	onlyCookie(t, response, "cage_tenant", false)
}

func TestTenantOwnershipPersistenceAndConcurrentOpen(t *testing.T) {
	fixture := newFixture(t, true)
	alice, aliceUser := fixture.login("register", " ALIce ")
	bob, _ := fixture.login("register", "bob")
	if aliceUser.Username != "alice" {
		t.Fatal("username not canonical")
	}
	first, second := fixture.create(alice, " 开发环境 "), fixture.create(alice, "开发环境")
	third := fixture.create(bob, "Bob one")
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/vms", `{"name":"overflow","system_id":"debian-12"}`, alice), 409, "vm_limit")
	fourth := fixture.create(bob, "Bob two")
	charlie, _ := fixture.login("register", "charlie")
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/vms", `{"name":"overflow","system_id":"debian-12"}`, charlie), 409, "vm_limit")
	if list := fixture.list(alice); !reflect.DeepEqual(list, []vm{first, second}) {
		t.Fatalf("wrong Alice list: %+v", list)
	}
	if list := fixture.list(bob); !reflect.DeepEqual(list, []vm{third, fourth}) {
		t.Fatalf("wrong Bob list: %+v", list)
	}
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(first.ID), "{}", bob), 404, "vm_not_found")
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(strings.Repeat("f", 32)), "{}", bob), 404, "vm_not_found")

	var requests sync.WaitGroup
	for range 12 {
		requests.Go(func() {
			response := fixture.request("POST", portalOrigin, openPath(first.ID), "{}", alice)
			if response.Code != 202 || !strings.Contains(response.Body.String(), `"state":"starting"`) {
				t.Errorf("unexpected open response: %d %s", response.Code, response.Body.String())
			}
		})
	}
	requests.Wait()
	close(fixture.runtime.gate)
	fixture.state(alice, first.ID, ready)
	for _, selected := range []struct {
		login  *http.Cookie
		record vm
	}{{alice, first}, {alice, second}, {bob, third}, {bob, fourth}, {alice, first}} {
		cookie := fixture.connect(selected.login, selected.record.ID)
		response := fixture.request("GET", vmOrigin(selected.record.ID), "/", "", cookie)
		wantStatus(t, response, 200, "")
		if response.Body.String() != selected.record.ID {
			t.Fatal("response came from wrong guest")
		}
		other := third.ID
		if selected.record.ID == third.ID {
			other = first.ID
		}
		wantStatus(t, fixture.request("GET", vmOrigin(other), "/", "", cookie), 401, "unauthenticated")
	}
	fixture.runtime.mu.Lock()
	calls := fixture.runtime.calls[first.ID]
	aliceLaunch, bobLaunch := fixture.runtime.specs[first.ID], fixture.runtime.specs[third.ID]
	fixture.runtime.mu.Unlock()
	// The storage backend sees the same pinned image across account boundaries,
	// with a separate instance identity for every VM.
	if aliceLaunch.VMID != first.ID || bobLaunch.VMID != third.ID || aliceLaunch.Image != fixture.config.Systems[0].Image || bobLaunch.Image != aliceLaunch.Image {
		t.Fatalf("wrong launch specs across accounts: %+v and %+v", aliceLaunch, bobLaunch)
	}
	if calls != 1 {
		t.Fatalf("same VM started %d times", calls)
	}

	if _, err := NewServer(fixture.config); err == nil {
		t.Fatal("state directory admitted a second writer")
	}
	state, err := os.ReadFile(fixture.config.StateDir + "/state.json")
	if err != nil || strings.Contains(string(state), password) || strings.Contains(string(state), alice.Value) || strings.Contains(string(state), fixtureSession) {
		t.Fatal("unexpected durable credentials")
	}
	fixture.server.Close()
	fixture.openServer()
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/session", "", alice), 401, "unauthenticated")
	alice, reloaded := fixture.login("login", "alice")
	if reloaded != aliceUser {
		t.Fatal("account identity changed across restart")
	}
	if list := fixture.list(alice); !reflect.DeepEqual(list, []vm{first, second}) {
		t.Fatalf("wrong reloaded VMs: %+v", list)
	}
	fixture.connect(alice, first.ID)
	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()
	if fixture.runtime.calls[first.ID] != 2 || fixture.runtime.provisions[first.ID] != 1 {
		t.Fatal("restart failed to reconnect to stored instance")
	}
}

func TestTenantStartupQueueAndConnectionReuse(t *testing.T) {
	fixture := newFixture(t, true)
	fixture.runtime.entered = make(chan string, 4)
	alice, _ := fixture.login("register", "alice")
	bob, _ := fixture.login("register", "bob")
	first := fixture.create(alice, "first")
	second := fixture.create(alice, "second")
	queued := fixture.create(bob, "queued")
	for _, record := range []vm{first, second} {
		wantStatus(t, fixture.request("POST", portalOrigin, openPath(record.ID), "{}", alice), 202, "")
	}
	entered := func() string {
		t.Helper()
		select {
		case id := <-fixture.runtime.entered:
			return id
		case <-time.After(3 * time.Second):
			t.Fatal("startup did not enter the runtime")
			return ""
		}
	}
	started := map[string]bool{entered(): true, entered(): true}
	if !started[first.ID] || !started[second.ID] {
		t.Fatal("wrong VMs obtained the initial startup slots")
	}
	for range 3 {
		wantStatus(t, fixture.request("POST", portalOrigin, openPath(queued.ID), "{}", bob), 202, "")
	}
	if list := fixture.list(bob); len(list) != 1 || list[0].State != starting {
		t.Fatalf("queued VM is not starting: %+v", list)
	}
	select {
	case id := <-fixture.runtime.entered:
		t.Fatalf("queued VM entered a full runtime: %s", id)
	case <-time.After(50 * time.Millisecond):
	}
	close(fixture.runtime.gate)
	if id := entered(); id != queued.ID {
		t.Fatalf("wrong VM left the queue: %s", id)
	}
	for _, selected := range []struct {
		login  *http.Cookie
		record vm
	}{{alice, first}, {alice, second}, {bob, queued}} {
		cookie := fixture.connect(selected.login, selected.record.ID)
		for range 3 {
			response := fixture.request("GET", vmOrigin(selected.record.ID), "/", "", cookie)
			wantStatus(t, response, 200, "")
			if response.Body.String() != selected.record.ID {
				t.Fatal("connection pool returned the wrong guest")
			}
		}
		fixture.runtime.mu.Lock()
		calls, dials := fixture.runtime.calls[selected.record.ID], fixture.runtime.guests[selected.record.ID].dials.Load()
		fixture.runtime.mu.Unlock()
		// The handshake and workbench use separate pools. Three workbench
		// requests still reuse a single connection.
		if calls != 1 || dials != 2 {
			t.Fatalf("VM runtime prepared %d times and dialed %d times", calls, dials)
		}
	}
}

func TestTenantTicketsAndProxyBoundaries(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	first, second := fixture.create(alice, "first"), fixture.create(alice, "second")
	fixture.runtime.nextError.Store(true)
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(first.ID), "{}", alice), 202, "")
	fixture.state(alice, first.ID, failed)
	if list := fixture.list(alice); list[0].Message != startupMessage {
		t.Fatal("unsafe or missing startup error")
	}
	entry := fixture.request("GET", vmOrigin(first.ID), "/_tenant/enter", "", nil)
	wantStatus(t, entry, 200, "")
	// The entry page keeps its exact CSP (no style-src): its look comes from a stylesheet enter.js adopts,
	// never from inline <style> or style attributes. Its only way back is a link to the portal origin.
	page := entry.Body.String()
	if entry.Header().Get("Content-Security-Policy") != "default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors "+portalOrigin ||
		!strings.Contains(page, `src="/_tenant/enter.js"`) || !strings.Contains(page, `lang="zh-CN"`) || !strings.Contains(page, `href="`+portalOrigin+`"`) ||
		strings.Contains(page, "{{portal}}") || strings.Contains(page, "<style") || strings.Contains(page, "style=") {
		t.Fatal("incorrect entry page")
	}
	entryJS := fixture.request("GET", vmOrigin(first.ID), "/_tenant/enter.js", "", nil)
	if !strings.Contains(entryJS.Body.String(), "history.replaceState") {
		t.Fatal("entry must remove the fragment")
	}
	if !strings.Contains(entryJS.Body.String(), "adoptedStyleSheets") || !strings.Contains(entryJS.Body.String(), "工作台入口已失效") || strings.ContainsAny(enterCSS, "\"\\\n`") {
		t.Fatal("entry script must carry the page style and the Chinese expiry text")
	}
	token := fixture.ticket(alice, first.ID)
	wantStatus(t, fixture.enter(second.ID, token), 401, "unauthenticated")
	results := make(chan *httptest.ResponseRecorder, 8)
	for range cap(results) {
		go func() { results <- fixture.enter(first.ID, token) }()
	}
	var cookie *http.Cookie
	for range cap(results) {
		response := <-results
		if response.Code == 204 {
			if cookie != nil {
				t.Fatal("ticket consumed twice")
			}
			cookie = onlyCookie(t, response, "__Host-cage-workspace", true)
		} else {
			wantStatus(t, response, 401, "unauthenticated")
		}
	}
	if cookie == nil {
		t.Fatal("ticket was never consumed")
	}
	old := fixture.ticket(alice, first.ID)
	fixture.server.mu.Lock()
	future := time.Now().Add(time.Minute)
	fixture.server.now = func() time.Time { return future }
	fixture.server.mu.Unlock()
	wantStatus(t, fixture.enter(first.ID, old), 401, "unauthenticated")
	wantStatus(t, fixture.request("GET", vmOrigin(second.ID), "/", "", cookie), 401, "unauthenticated")
	for _, path := range []string{"/api/launch", "/api/%73ession", "/api/handoff", "/api/desktop-local/status", "/api/remote/manifest", "/api/tenant/session"} {
		wantStatus(t, fixture.request("POST", vmOrigin(first.ID), path, "{}", cookie), 404, "not_found")
	}
	request := httptest.NewRequest("PATCH", "/api/echo/file%20name?item=one%20two", strings.NewReader("fixture content"))
	request.Host = strings.TrimPrefix(vmOrigin(first.ID), "https://")
	request.Header.Set("Origin", vmOrigin(first.ID))
	request.AddCookie(cookie)
	request.AddCookie(alice)
	for _, header := range []string{"Authorization", "Forwarded", "X-Forwarded-For", "X-Real-IP", "Referer", "X-Cage-Connection", "X-Cage-Agent-Window"} {
		request.Header.Set(header, "fixture")
	}
	response := fixture.serve(request)
	wantStatus(t, response, 200, "")
	var echoed map[string]string
	if json.Unmarshal(response.Body.Bytes(), &echoed) != nil || !reflect.DeepEqual(echoed, map[string]string{"id": first.ID, "method": "PATCH", "path": "/api/echo/file%20name", "query": "item=one%20two", "body": "fixture content"}) {
		t.Fatalf("native request changed: %+v", echoed)
	}
	for _, name := range []string{"Set-Cookie", "Access-Control-Allow-Origin", "X-Frame-Options", "Link", "Trailer", "X-Fixture-Trailer"} {
		if response.Header().Get(name) != "" || response.Result().Trailer.Get(name) != "" {
			t.Fatalf("guest header leaked: %s", name)
		}
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "wss://"+request.Host) {
		t.Fatal("workspace CSP disallows its WebSocket")
	}
	request.Header.Set("Origin", portalOrigin)
	wantStatus(t, fixture.serve(request), 403, "origin_refused")
	request.Header.Set("Origin", vmOrigin(first.ID))
	request.Host += "."
	wantStatus(t, fixture.serve(request), 403, "origin_refused")
	local := fixture.request("GET", vmOrigin(first.ID), "/local-redirect", "", cookie)
	wantStatus(t, local, 302, "")
	if local.Header().Get("Location") != vmOrigin(first.ID)+"/welcome" {
		t.Fatal("wrong local redirect target")
	}
	wantStatus(t, fixture.request("GET", vmOrigin(first.ID), "/outside-redirect", "", cookie), 503, "runtime_unavailable")

	unused := fixture.ticket(alice, first.ID)
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/logout", "{}", alice), 204, "")
	wantStatus(t, fixture.enter(first.ID, unused), 401, "unauthenticated")
	wantStatus(t, fixture.request("GET", vmOrigin(first.ID), "/", "", cookie), 401, "unauthenticated")
}

func TestTenantCatalogRetirementPreservesBindings(t *testing.T) {
	fixture := newFixture(t, false)
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/systems", "", nil), 401, "unauthenticated")
	alice, _ := fixture.login("register", "alice")
	options := fixture.request("GET", portalOrigin, "/api/tenant/systems", "", alice)
	wantStatus(t, options, 200, "")
	if options.Body.String() != "{\"systems\":[{\"id\":\"debian-12\",\"label\":\"Debian 12\"},{\"id\":\"ubuntu-2404\",\"label\":\"Ubuntu 24.04\"}]}\n" {
		t.Fatal("system directory does not match the configured public fields")
	}
	for _, invalid := range []string{`{"name":"   "}`, `{"name":"unknown","system_id":"fedora"}`, `{"name":"override","system_id":"debian-12","image":"other"}`} {
		wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/vms", invalid, alice), 400, "invalid_input")
	}
	current := fixture.createWithSystem(alice, "existing", "ubuntu-2404")
	pending := fixture.createWithSystem(alice, "pending", "ubuntu-2404")
	fixture.connect(alice, current.ID)
	fixture.server.Close()
	fixture.config.Systems = fixture.config.Systems[:1]
	fixture.openServer()
	alice, _ = fixture.login("login", "alice")
	options = fixture.request("GET", portalOrigin, "/api/tenant/systems", "", alice)
	wantStatus(t, options, 200, "")
	if strings.Contains(options.Body.String(), "ubuntu") || current.SystemLabel != "Ubuntu 24.04" || !reflect.DeepEqual(fixture.list(alice), []vm{current, pending}) {
		t.Fatal("catalog retirement changed saved VM system fields")
	}
	fixture.runtime.imageMissing.Store(true)
	fixture.connect(alice, current.ID)
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(pending.ID), "{}", alice), 202, "")
	fixture.state(alice, pending.ID, failed)
	fixture.runtime.mu.Lock()
	spec := fixture.runtime.specs[pending.ID]
	fixture.runtime.mu.Unlock()
	if spec.SystemID != "ubuntu-2404" || spec.Image != strings.Repeat("e", 64) {
		t.Fatal("pending VM fell back to an unrelated system image")
	}
	fixture.runtime.imageMissing.Store(false)
	fixture.connect(alice, pending.ID)
}

func TestTenantCatalogPinsAndLegacyState(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "systems.json")
	image := strings.Repeat("d", 64)
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"fingerprint", `[{"id":"debian-12","label":"Debian 12","image":"` + image + `"}]`, true},
		{"alias", `[{"id":"debian-12","label":"Debian 12","image":"cage-blank"}]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(filename, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			systems, err := LoadSystems(filename)
			if (err == nil) != test.want || (test.want && !reflect.DeepEqual(systems, []System{DefaultSystem(image)})) {
				t.Fatalf("unexpected catalog result: %+v, %v", systems, err)
			}
		})
	}
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	original := fixture.create(alice, "legacy workspace")
	fixture.server.Close()
	legacy := fixture.server.store.database
	legacy.Version = 1
	record := legacy.VMs[original.ID]
	record.SystemID, record.SystemLabel, record.Image = "", "", ""
	record.State = ready
	legacy.VMs[original.ID] = record
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	filename = filepath.Join(fixture.config.StateDir, "state.json")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if server, err := NewServer(fixture.config); err == nil {
		server.Close()
		t.Fatal("v1 records were migrated without a verified original fingerprint")
	}
	unchanged, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(data, unchanged) {
		t.Fatalf("refused migration rewrote state: %v", err)
	}
	fixture.config.LegacyDebianImage = image
	fixture.openServer()
	alice, _ = fixture.login("login", "alice")
	if list := fixture.list(alice); !reflect.DeepEqual(list, []vm{original}) {
		t.Fatalf("legacy VM did not retain its identity and default system: %+v", list)
	}
	fixture.connect(alice, original.ID)
	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()
	if spec := fixture.runtime.specs[original.ID]; spec.SystemID != defaultSystemID || spec.Image != image {
		t.Fatalf("legacy VM did not get its configured image: %+v", spec)
	}
}

func TestTenantRechecksFailedWorkspaceWithoutReplayingWrites(t *testing.T) {
	// A request that fails on a reused connection is confirmed with a probe, so a
	// healthy controller keeps the workspace ready; proxy_fault_test.go covers
	// the connection that was dialed for the request itself.
	for _, failure := range []string{"connection lost", "connection lost and controller down", "controller session expired"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newFixture(t, false)
			var expired, down atomic.Bool
			var writes atomic.Int32
			const changePath = "/api/fixture/save"
			fixture.runtime.guestHandler = func(id string) http.Handler {
				guest := fixtureGuest(id)
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/fixture/login" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					if r.URL.Path == "/api/launch" {
						expired.Store(false)
						down.Store(false)
					}
					if r.URL.Path == "/api/client/about" && down.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if r.URL.Path == changePath {
						writes.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						if strings.HasPrefix(failure, "connection lost") {
							down.Store(failure == "connection lost and controller down")
							panic(http.ErrAbortHandler)
						}
						expired.Store(true)
					}
					if expired.Load() && (r.URL.Path == changePath || r.URL.Path == "/api/client/about") {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					guest.ServeHTTP(w, r)
				})
			}
			alice, _ := fixture.login("register", "alice")
			record := fixture.create(alice, "workspace")
			cookie := fixture.connect(alice, record.ID)
			unused := fixture.ticket(alice, record.ID)
			wantStatus(t, fixture.request("POST", vmOrigin(record.ID), "/api/fixture/login", `{"password":"not accepted"}`, cookie), 401, "")
			wantStatus(t, fixture.request("POST", portalOrigin, openPath(record.ID), "{}", alice), 200, "")
			response := fixture.request("POST", vmOrigin(record.ID), changePath, `{"name":"notes"}`, cookie)
			if strings.HasPrefix(failure, "connection lost") {
				wantStatus(t, response, 503, "runtime_unavailable")
			} else {
				wantStatus(t, response, 401, "")
			}
			if failure == "connection lost" {
				// The probe finds a healthy controller: nothing is invalidated.
				fixture.server.mu.Lock()
				workspace := fixture.server.workspaces[record.ID]
				fixture.server.mu.Unlock()
				waitFixture(t, func() bool { return !workspace.checkingSession.Load() })
				fixture.state(alice, record.ID, ready)
				wantStatus(t, fixture.enter(record.ID, unused), 204, "")
			} else {
				// The probe runs in the background; the workspace fails once it ends.
				fixture.state(alice, record.ID, failed)
				wantStatus(t, fixture.enter(record.ID, unused), 401, "unauthenticated")
				wantStatus(t, fixture.request("POST", portalOrigin, openPath(record.ID), "{}", alice), 202, "")
				fixture.state(alice, record.ID, ready)
			}
			fixture.connect(alice, record.ID)
			fixture.runtime.mu.Lock()
			calls := fixture.runtime.calls[record.ID]
			fixture.runtime.mu.Unlock()
			if writes.Load() != 1 || (failure != "connection lost" && calls != 2) || (failure == "connection lost" && calls != 1) {
				t.Fatalf("unexpected write or handshake count: writes=%d prepares=%d", writes.Load(), calls)
			}
		})
	}
}

func TestTenantRecheckKeepsBusyWorkspace(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)

	s := fixture.server
	s.mu.Lock()
	workspace := s.workspaces[record.ID]
	workspace.checkedAt = s.now().Add(-readinessTTL)
	s.mu.Unlock()
	// Occupy the entire workbench pool with ordinary long-lived streams.
	streams := make([]io.ReadCloser, 0, workspace.transport.MaxConnsPerHost)
	defer func() {
		for _, stream := range streams {
			stream.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	for range workspace.transport.MaxConnsPerHost {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, workspace.origin.String()+"/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(workspace.cookie)
		req.Header.Set("Origin", workspace.origin.String())
		response, err := workspace.transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("workbench stream: HTTP %d", response.StatusCode)
		}
	}
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(record.ID), "{}", alice), 202, "")
	fixture.state(alice, record.ID, ready)
	if workspace.ctx.Err() != nil {
		t.Fatal("a busy pool caused the workspace to be closed")
	}
	for _, stream := range streams {
		stream.Close()
	}
	wantStatus(t, fixture.request("GET", vmOrigin(record.ID), "/", "", cookie), 200, "")
}

// resetConn reports a reset instead of an orderly end while reset is set.
type resetConn struct {
	net.Conn
	reset *atomic.Bool
}

func (conn resetConn) Read(buffer []byte) (int, error) {
	count, err := conn.Conn.Read(buffer)
	if errors.Is(err, io.EOF) && conn.reset.Load() {
		err = syscall.ECONNRESET
	}
	return count, err
}

func TestTenantSocketCloseConfirmsControllerHealth(t *testing.T) {
	fixture := newFixture(t, false)
	var controllerDown, resetStreams atomic.Bool
	var healthChecks atomic.Int32
	fixture.runtime.wrapConn = func(conn net.Conn) net.Conn { return resetConn{Conn: conn, reset: &resetStreams} }
	fixture.runtime.guestHandler = func(id string) http.Handler {
		guest := fixtureGuest(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/client/about" {
				healthChecks.Add(1)
				if controllerDown.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			if r.URL.Path == "/closing-socket" {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err == nil {
					conn.Close()
				}
				return
			}
			guest.ServeHTTP(w, r)
		})
	}
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)
	listener := serveFixture(t, fixture.server)
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return listener.dial(ctx)
	}, HandshakeTimeout: 2 * time.Second}
	dropSocket := func() {
		t.Helper()
		conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(vmOrigin(record.ID), "https")+"/closing-socket", http.Header{
			"Origin": {vmOrigin(record.ID)}, "Cookie": {cookie.Name + "=" + cookie.Value},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("expected the guest to close the socket")
		}
	}
	s := fixture.server
	s.mu.Lock()
	workspace := s.workspaces[record.ID]
	s.mu.Unlock()
	// An orderly guest close (a terminal that ended, a released viewer) is
	// not a fault: it starts no health check even while one would fail.
	controllerDown.Store(true)
	before := healthChecks.Load()
	dropSocket()
	s.mu.Lock()
	probed := !workspace.nextFaultProbe.IsZero()
	s.mu.Unlock()
	if probed || healthChecks.Load() != before || workspace.checkingSession.Load() {
		t.Fatal("an orderly socket close started a fault probe")
	}
	fixture.state(alice, record.ID, ready)
	controllerDown.Store(false)

	// A stream that breaks is confirmed through the controller.
	ticket := fixture.ticket(alice, record.ID)
	resetStreams.Store(true)
	dropSocket()
	waitFixture(t, func() bool { return healthChecks.Load() > before && !workspace.checkingSession.Load() })
	fixture.state(alice, record.ID, ready)
	s.mu.Lock()
	workspace.nextFaultProbe = time.Time{}
	s.mu.Unlock()
	controllerDown.Store(true)
	dropSocket()
	fixture.state(alice, record.ID, failed)
	wantStatus(t, fixture.enter(record.ID, ticket), 401, "unauthenticated")
}

func TestTenantFaultProbeTimeoutKeepsWorkspaceReady(t *testing.T) {
	timeout := faultProbeTimeout
	faultProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { faultProbeTimeout = timeout })
	fixture := newFixture(t, false)
	var stall atomic.Bool
	var stalled atomic.Int32
	fixture.runtime.guestHandler = func(id string) http.Handler {
		guest := fixtureGuest(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/client/about" && stall.Load() {
				stalled.Add(1)
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			guest.ServeHTTP(w, r)
		})
	}
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)
	s := fixture.server
	s.mu.Lock()
	workspace := s.workspaces[record.ID]
	s.mu.Unlock()
	stall.Store(true)
	s.confirmWorkspaceFault(record.ID, workspace)
	waitFixture(t, func() bool { return stalled.Load() > 0 && !workspace.checkingSession.Load() })
	s.mu.Lock()
	ready := s.workspaces[record.ID] == workspace && !workspace.checkedAt.IsZero()
	s.mu.Unlock()
	if !ready {
		t.Fatal("a fault probe that only timed out marked the workspace unready")
	}
	stall.Store(false)
	wantStatus(t, fixture.request("GET", vmOrigin(record.ID), "/", "", cookie), 200, "")
}

func TestTenantUnreadyWorkspaceRecoversFromWorkbenchRequest(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)
	s := fixture.server
	s.mu.Lock()
	workspace := s.workspaces[record.ID]
	s.markWorkspaceUnready(record.ID, workspace)
	s.mu.Unlock()
	fixture.runtime.mu.Lock()
	prepares := fixture.runtime.calls[record.ID]
	fixture.runtime.mu.Unlock()
	// The workbench's own request is refused but starts a recheck, so its
	// retry succeeds without reopening the workspace from the portal.
	wantStatus(t, fixture.request("GET", vmOrigin(record.ID), "/", "", cookie), 503, "runtime_unavailable")
	waitFixture(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.workspaces[record.ID] == workspace && !workspace.checkedAt.IsZero() && !s.starting[record.ID]
	})
	wantStatus(t, fixture.request("GET", vmOrigin(record.ID), "/", "", cookie), 200, "")
	fixture.runtime.mu.Lock()
	calls := fixture.runtime.calls[record.ID]
	fixture.runtime.mu.Unlock()
	if calls != prepares {
		t.Fatalf("a healthy workspace was prepared again: %d -> %d", prepares, calls)
	}
}

func TestTenantStartupDoesNotWriteDurableState(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	state := fixture.config.StateDir
	temporary := state + "-offline"
	if err := os.Rename(state, temporary); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(temporary, state); err != nil {
			t.Error(err)
		}
	}()
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/vms", `{"name":"Unsaved","system_id":"debian-12"}`, alice), 503, "runtime_unavailable")
	fixture.runtime.nextError.Store(true)
	wantStatus(t, fixture.request("POST", portalOrigin, openPath(record.ID), "{}", alice), 202, "")
	fixture.state(alice, record.ID, failed)
	fixture.login("login", "alice")
	fixture.connect(alice, record.ID)
	if list := fixture.list(alice); len(list) != 1 || list[0].State != ready {
		t.Fatalf("unexpected state after recoverable storage error: %+v", list)
	}
}

func TestTenantCanBindEmptyLegacyV2Image(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	legacy := fixture.create(alice, "legacy workspace")
	pinned := fixture.create(alice, "pinned workspace")
	fixture.server.Close()
	database := fixture.server.store.database
	record := database.VMs[legacy.ID]
	record.Image = ""
	database.VMs[legacy.ID] = record
	state, err := json.Marshal(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.config.StateDir, "state.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.openServer()
	if fixture.server.store.VMs[legacy.ID].Image != "" {
		t.Fatal("legacy binding inferred from the current catalog")
	}
	fixture.server.Close()
	fixture.config.LegacyDebianImage = strings.Repeat("f", 64)
	fixture.openServer()
	fixture.server.Close()
	fixture.config.LegacyDebianImage = ""
	fixture.openServer()
	if fixture.server.store.VMs[legacy.ID].Image != strings.Repeat("f", 64) || fixture.server.store.VMs[pinned.ID].Image != database.VMs[pinned.ID].Image {
		t.Fatal("legacy binding was not persisted independently of the existing pin")
	}
}

func TestTenantLogoutReleasesSlowUpload(t *testing.T) {
	fixture := newFixture(t, false)
	uploading := make(chan struct{})
	fixture.runtime.guestHandler = func(id string) http.Handler {
		guest := fixtureGuest(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/fixture/upload" {
				guest.ServeHTTP(w, r)
				return
			}
			prefix := make([]byte, len("partial"))
			if _, err := io.ReadFull(r.Body, prefix); err == nil {
				close(uploading)
				_, _ = io.Copy(io.Discard, r.Body)
			}
		})
	}
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)
	listener := serveFixture(t, fixture.server)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.dial(ctx) }}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second}
	reader, writer := io.Pipe()
	defer writer.Close()
	request, err := http.NewRequest("POST", "http://vm-"+record.ID+".cage.example.test/api/fixture/upload", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	request.Header.Set("Origin", vmOrigin(record.ID))
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	go func() { _, _ = io.WriteString(writer, "partial") }()
	select {
	case <-uploading:
	case <-time.After(3 * time.Second):
		t.Fatal("guest did not receive partial upload")
	}
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/logout", "{}", alice), 204, "")
	waitFixture(t, func() bool {
		fixture.server.mu.Lock()
		defer fixture.server.mu.Unlock()
		workspace := fixture.server.workspaces[record.ID]
		return len(workspace.slots) == 0 && !workspace.checkedAt.IsZero()
	})
	writer.Close()
	<-uploadDone
}

func TestTenantLogoutAndExpiryRevokeLiveConnections(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	otherLogin, _ := fixture.login("login", "alice")
	record := fixture.create(alice, "workspace")
	cookie := fixture.connect(alice, record.ID)
	otherCookie := fixture.connect(otherLogin, record.ID)
	listener := serveFixture(t, fixture.server)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.dial(ctx) }}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second}
	url := "http://vm-" + record.ID + ".cage.example.test"
	openStream := func(cookie *http.Cookie) *http.Response {
		request, _ := http.NewRequest("GET", url+"/stream", nil)
		request.AddCookie(cookie)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		expected := "data: " + record.ID + "\n\n"
		data := make([]byte, len(expected))
		if _, err := io.ReadFull(response.Body, data); err != nil || response.StatusCode != 200 || string(data) != expected {
			t.Fatalf("stream setup failed: %d %v", response.StatusCode, err)
		}
		return response
	}
	openSocket := func(cookie *http.Cookie) *websocket.Conn {
		dialer := &websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.dial(ctx) }, HandshakeTimeout: 2 * time.Second}
		conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(url, "http")+"/socket", http.Header{"Origin": {vmOrigin(record.ID)}, "Cookie": {cookie.Name + "=" + cookie.Value}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if _, data, err := conn.ReadMessage(); err != nil || string(data) != record.ID {
			t.Fatalf("socket setup failed: %v", err)
		}
		return conn
	}
	checkRevocation := func(stream *http.Response, socket *websocket.Conn) {
		t.Helper()
		streamClosed := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, stream.Body); streamClosed <- err }()
		select {
		case <-streamClosed:
		case <-time.After(3 * time.Second):
			t.Fatal("stream remained open after revocation")
		}
		_ = socket.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := socket.ReadMessage(); err == nil {
			t.Fatal("socket remained open after revocation")
		} else if deadline, ok := errors.AsType[net.Error](err); ok && deadline.Timeout() {
			t.Fatal("socket closure timed out")
		}
	}
	stream, socket, survivingSocket := openStream(cookie), openSocket(cookie), openSocket(otherCookie)
	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/logout", "{}", alice), 204, "")
	checkRevocation(stream, socket)
	_ = survivingSocket.SetReadDeadline(time.Now().Add(time.Second))
	if err := survivingSocket.WriteMessage(websocket.TextMessage, []byte("still connected")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := survivingSocket.ReadMessage(); err != nil || string(data) != "still connected" {
		t.Fatalf("separate login was revoked: %v", err)
	}

	fixture.server.mu.Lock()
	fixture.server.sessionTTL = 1200 * time.Millisecond
	fixture.server.mu.Unlock()
	expiring, _ := fixture.login("login", "alice")
	expiringCookie := fixture.connect(expiring, record.ID)
	checkRevocation(openStream(expiringCookie), openSocket(expiringCookie))
	wantStatus(t, fixture.request("GET", portalOrigin, "/api/tenant/session", "", expiring), 401, "unauthenticated")
}

func TestTenantNameOnlyPortalCreateUsesDefaultSystem(t *testing.T) {
	fixture := newFixture(t, false)
	alice, _ := fixture.login("register", "alice")
	response := fixture.request("POST", portalOrigin, "/api/tenant/vms", `{"name":"only a name"}`, alice)
	wantStatus(t, response, 201, "")
	var created struct{ VM vm }
	if json.Unmarshal(response.Body.Bytes(), &created) != nil || created.VM.SystemID != defaultSystemID || created.VM.SystemLabel != "Debian 12" || created.VM.State != stopped {
		t.Fatalf("name-only create did not use the configured default: %+v", created.VM)
	}
	response = fixture.request("POST", portalOrigin, "/api/tenant/vms", `{"name":"explicit","system_id":"ubuntu-2404"}`, alice)
	wantStatus(t, response, 201, "")
	if json.Unmarshal(response.Body.Bytes(), &created) != nil || created.VM.SystemID != "ubuntu-2404" || created.VM.SystemLabel != "Ubuntu 24.04" {
		t.Fatalf("explicit system_id compatibility was lost: %+v", created.VM)
	}
}

func TestTenantBrowserVirtualMachineControls(t *testing.T) {
	fixture := newFixture(t, false)
	alice, aliceUser := fixture.login("register", "alice")
	bob, _ := fixture.login("register", "bob")
	first := fixture.create(alice, "first")
	bobVM := fixture.create(bob, "bob workspace")
	cookie := fixture.connect(alice, first.ID)
	const listPath = "/api/client/virtual-machines"

	wantStatus(t, fixture.request("GET", vmOrigin(first.ID), listPath, "", nil), 401, "unauthenticated")
	wantStatus(t, fixture.request("GET", vmOrigin(bobVM.ID), listPath, "", cookie), 401, "unauthenticated")
	response := fixture.request("GET", vmOrigin(first.ID), listPath, "", cookie)
	wantStatus(t, response, 200, "")
	var list struct {
		VMs       []vm   `json:"vms"`
		CurrentID string `json:"current_id"`
	}
	if json.Unmarshal(response.Body.Bytes(), &list) != nil || list.CurrentID != first.ID || len(list.VMs) != 1 || list.VMs[0].ID != first.ID {
		t.Fatalf("wrong owner-scoped VM list: %+v", list)
	}

	response = fixture.request("POST", vmOrigin(first.ID), listPath, `{"name":"second"}`, cookie)
	wantStatus(t, response, 201, "")
	var created struct{ VM vm }
	if json.Unmarshal(response.Body.Bytes(), &created) != nil || !vmIDPattern.MatchString(created.VM.ID) || created.VM.Name != "second" || created.VM.SystemID != defaultSystemID || created.VM.State != stopped {
		t.Fatalf("invalid browser create response: %s", response.Body.String())
	}
	fixture.runtime.mu.Lock()
	started := fixture.runtime.calls[created.VM.ID]
	fixture.runtime.mu.Unlock()
	if started != 0 {
		t.Fatal("VM creation did more than persist metadata")
	}
	wantStatus(t, fixture.request("POST", vmOrigin(first.ID), listPath, `{"name":"third","system_id":"ubuntu-2404"}`, cookie), 400, "invalid_input")
	wantStatus(t, fixture.request("POST", vmOrigin(first.ID), listPath, `{"name":"third"}`, cookie), 409, "vm_limit")

	wantStatus(t, fixture.request("POST", vmOrigin(first.ID), "/api/client/virtual-machines/"+bobVM.ID+"/open", "{}", cookie), 404, "vm_not_found")
	wantStatus(t, fixture.request("POST", vmOrigin(first.ID), "/api/client/virtual-machines/not-an-id/open", "{}", cookie), 404, "not_found")

	fixture.server.mu.Lock()
	tickets := len(fixture.server.tickets)
	fixture.server.mu.Unlock()
	response = fixture.request("POST", vmOrigin(first.ID), "/api/client/virtual-machines/"+created.VM.ID+"/open", "{}", cookie)
	wantStatus(t, response, 200, "")
	var opened struct {
		VMID string `json:"vm_id"`
		VM   vm     `json:"vm"`
	}
	if json.Unmarshal(response.Body.Bytes(), &opened) != nil || opened.VMID != created.VM.ID || (opened.VM.State != starting && opened.VM.State != ready) {
		t.Fatalf("invalid browser open response: %s", response.Body.String())
	}
	fixture.server.mu.Lock()
	if len(fixture.server.tickets) != tickets {
		fixture.server.mu.Unlock()
		t.Fatal("browser open issued a duplicate ticket")
	}
	fixture.server.mu.Unlock()

	fixture.state(alice, created.VM.ID, ready)
	response = fixture.request("POST", vmOrigin(first.ID), "/api/client/virtual-machines/"+created.VM.ID+"/open", "{}", cookie)
	wantStatus(t, response, 200, "")
	if json.Unmarshal(response.Body.Bytes(), &opened) != nil || opened.VM.State != ready {
		t.Fatalf("ready open did not report the ready VM: %s", response.Body.String())
	}
	fixture.server.mu.Lock()
	if len(fixture.server.tickets) != tickets {
		fixture.server.mu.Unlock()
		t.Fatal("ready open issued a duplicate ticket")
	}
	fixture.server.mu.Unlock()

	wantStatus(t, fixture.request("POST", vmOrigin(first.ID), "/api/client/virtual-machines/"+created.VM.ID+"/open", "{}", cookie, strings.Repeat("a", 32)), 409, "session_changed")

	request := httptest.NewRequest("POST", listPath, strings.NewReader(`{"name":"origin"}`))
	request.Host = strings.TrimPrefix(vmOrigin(first.ID), "https://")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", portalOrigin)
	request.AddCookie(cookie)
	wantStatus(t, fixture.serve(request), 403, "origin_refused")
	request.Header.Set("Origin", vmOrigin(first.ID))
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	wantStatus(t, fixture.serve(request), 403, "origin_refused")

	bobCookie := fixture.connect(bob, bobVM.ID)
	response = fixture.request("GET", vmOrigin(bobVM.ID), listPath, "", bobCookie)
	wantStatus(t, response, 200, "")
	if json.Unmarshal(response.Body.Bytes(), &list) != nil || list.CurrentID != bobVM.ID || len(list.VMs) != 1 || list.VMs[0].ID != bobVM.ID {
		t.Fatalf("wrong second-account list: %+v", list)
	}

	wantStatus(t, fixture.request("POST", portalOrigin, "/api/tenant/logout", "{}", alice, aliceUser.ID), 204, "")
	wantStatus(t, fixture.request("GET", vmOrigin(first.ID), listPath, "", cookie), 401, "unauthenticated")
}

func TestTenantManagedGuestAccount(t *testing.T) {
	fixture := newFixture(t, false)
	guest := &fixtureAccountGuest{}
	fixture.runtime.guestHandler = func(id string) http.Handler { return guest.handler(id) }
	alice, _ := fixture.login("register", "alice")
	record := fixture.create(alice, "managed workspace")

	path := filepath.Join(fixture.config.StateDir, guestAccountStateFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("managed account secret was not persisted privately: %v", err)
	}
	loaded, err := LoadGuestAccount(path)
	if err != nil {
		t.Fatal(err)
	}
	derived := string(loaded.password(record.ID))
	if !secretPattern.MatchString(derived) || len(derived) != 64 {
		t.Fatal("derived guest password is not a 32-byte hex secret")
	}

	cookie := fixture.connect(alice, record.ID)
	exists, unlocked, username, password, reads, creates, unlocks := guest.snapshot()
	if !exists || !unlocked || username != defaultGuestAccountUsername || password != derived || creates != 1 || unlocks != 0 {
		t.Fatalf("managed account was not created and entered: exists=%v unlocked=%v username=%q creates=%d unlocks=%d", exists, unlocked, username, creates, unlocks)
	}
	if reads == 0 {
		t.Fatal("gateway did not inspect the guest account")
	}

	// Serving a ready workspace must not repeat the Argon2 account work.
	for range 3 {
		wantStatus(t, fixture.request("GET", vmOrigin(record.ID), "/", "", cookie), 200, "")
	}
	response := fixture.request("POST", vmOrigin(record.ID), "/api/client/virtual-machines/"+record.ID+"/open", "{}", cookie)
	wantStatus(t, response, 200, "")
	if _, _, _, _, after, _, _ := guest.snapshot(); after != reads {
		t.Fatal("proxy or ready open touched the guest account")
	}

	// A gateway restart on the same VM boot must silently unlock the existing
	// account instead of creating another one or asking the user.
	guest.setLocked()
	fixture.server.Close()
	fixture.openServer()
	alice, _ = fixture.login("login", "alice")
	cookie = fixture.connect(alice, record.ID)
	exists, unlocked, username, password, _, creates, unlocks = guest.snapshot()
	if !exists || !unlocked || username != defaultGuestAccountUsername || password != derived || creates != 1 || unlocks != 1 {
		t.Fatalf("restart did not unlock the managed account: exists=%v unlocked=%v creates=%d unlocks=%d", exists, unlocked, creates, unlocks)
	}

	// An account created before this feature (or with another name) is
	// preserved: no re-keying, no account creation, and no forced prompt loss.
	guest.setExistingAccount("someone-else", "the previous account password")
	fixture.server.Close()
	fixture.openServer()
	alice, _ = fixture.login("login", "alice")
	cookie = fixture.connect(alice, record.ID)
	exists, unlocked, username, password, _, creates, unlocks = guest.snapshot()
	if !exists || unlocked || username != "someone-else" || password != "the previous account password" || creates != 1 || unlocks != 1 {
		t.Fatalf("existing account was modified: exists=%v unlocked=%v username=%q creates=%d unlocks=%d", exists, unlocked, username, creates, unlocks)
	}

	// The upstream workbench reports a wrong password as 400. A managed-name
	// account with a changed password must also be left untouched and fall
	// back to its normal password screen.
	guest.setExistingAccount(defaultGuestAccountUsername, "changed by the owner")
	fixture.server.Close()
	fixture.openServer()
	alice, _ = fixture.login("login", "alice")
	cookie = fixture.connect(alice, record.ID)
	exists, unlocked, username, password, _, creates, unlocks = guest.snapshot()
	if !exists || unlocked || username != defaultGuestAccountUsername || password != "changed by the owner" || creates != 1 || unlocks != 2 {
		t.Fatalf("changed-password account was modified: exists=%v unlocked=%v username=%q creates=%d unlocks=%d", exists, unlocked, username, creates, unlocks)
	}
	wantStatus(t, fixture.request("GET", vmOrigin(record.ID), "/api/client/virtual-machines", "", cookie), 200, "")
}
