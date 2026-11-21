package workbench

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/webfront"
)

func withScienceHostOS(t *testing.T, goos string) {
	t.Helper()
	old := scienceHostOS
	scienceHostOS = goos
	t.Cleanup(func() { scienceHostOS = old })
}

// scienceWorkload is a Windows workload with a published control file, as
// serve() leaves it.
func scienceWorkload(t *testing.T) *Server {
	t.Helper()
	withScienceHostOS(t, "windows")
	withScienceHostArch(t, "amd64")
	s := testServer(t)
	if err := writePrivate(filepath.Join(s.profile, "control.json"), controlFile{Port: 17777, Token: s.token, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	s.controlPublished = true
	t.Cleanup(s.stopScienceFront)
	return s
}

// scienceEntry is the desktop entry of the same host: it frames the
// workload's front and reads its control file.
func scienceEntry(t *testing.T, workload *Server) *Server {
	t.Helper()
	entry := testServer(t)
	entry.origin = "http://localhost:17778"
	entry.desktopEntry = true
	entry.profile = filepath.Join(workload.profile, "desktop-entry")
	if err := os.MkdirAll(entry.profile, 0700); err != nil {
		t.Fatal(err)
	}
	entry.remote = newDesktopGateway(entry.profile, nil)
	entry.remote.desktop.localVMs = newLocalVMStore(workload.profile, "")
	t.Cleanup(entry.remote.close)
	if err := writePrivate(filepath.Join(entry.profile, "control.json"), controlFile{Port: 17778, Token: entry.token, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	return entry
}

// shaped sends one request in the shape a known caller uses.
type requestShape struct {
	name    string
	method  string
	path    string
	headers map[string]string
	trusted bool
	cookies []string // session cookie values, in order; "" = the real one
}

func (shape requestShape) send(s *Server) *httptest.ResponseRecorder {
	r := httptest.NewRequest(shape.method, s.origin+shape.path, strings.NewReader("{}"))
	if shape.method != "GET" {
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
	}
	for name, value := range shape.headers {
		r.Header.Set(name, value)
	}
	cookies := shape.cookies
	if cookies == nil {
		cookies = []string{""}
	}
	var parts []string
	for _, value := range cookies {
		if value == "" {
			value = s.session
		}
		parts = append(parts, s.cookieName()+"="+value)
	}
	if shape.trusted {
		parts = append(parts, s.cookieName()+"-desktop="+trustedViewProof(s.token))
	}
	r.Header.Set("Cookie", strings.Join(parts, "; "))
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

// The callers the brief names: the three native shells (their page and
// their own navigations), the desktop entry's hop to the workload, the
// remote-host hop, the phone link and the tenant gateway.
var scienceRequestShapes = []requestShape{
	{name: "native shell page", method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}, trusted: true},
	{name: "native shell write", method: "POST", path: "/api/claude-science/nope", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, trusted: true},
	{name: "native shell navigation", method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"}, trusted: true},
	{name: "entry and remote-host hop", method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, trusted: true},
	{name: "phone link", method: "GET", path: "/api/settings"},
	{name: "phone link write", method: "POST", path: "/api/claude-science/nope"},
	{name: "tenant gateway", method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": "same-origin", "X-Forwarded-For": "10.0.0.2"}},
	{name: "static page", method: "GET", path: "/", headers: map[string]string{"Sec-Fetch-Site": "none"}},
	{name: "page session renewal", method: "POST", path: "/api/session", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, cookies: []string{}},
	{name: "Go client bootstrap", method: "POST", path: "/api/launch", cookies: []string{}},
}

func shapeCodes(s *Server) map[string]int {
	codes := map[string]int{}
	for _, shape := range scienceRequestShapes {
		codes[shape.name] = shape.send(s).Code
	}
	return codes
}

func frameSources(t *testing.T, s *Server) string {
	t.Helper()
	w := requestShape{method: "GET", path: "/"}.send(s)
	policy := w.Header().Get("Content-Security-Policy")
	for _, directive := range strings.Split(policy, ";") {
		if fields := strings.Fields(directive); len(fields) > 0 && fields[0] == "frame-src" {
			return strings.Join(fields[1:], " ")
		}
	}
	t.Fatalf("no frame-src in %q", policy)
	return ""
}

func TestScienceHostRenderOffChangesNothing(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			workload := scienceWorkload(t)
			scienceHostOS = goos
			entry := scienceEntry(t, workload)
			workload.syncScienceFront()
			if workload.scienceFrontRunning() != nil {
				t.Fatal("front started while off")
			}
			data, _ := os.ReadFile(filepath.Join(workload.profile, "control.json"))
			if strings.Contains(string(data), "science") {
				t.Fatalf("control file changed: %s", data)
			}
			for _, s := range []*Server{workload, entry} {
				if got := frameSources(t, s); got != "'none'" {
					t.Fatalf("frame-src %q", got)
				}
				if s.scienceHardened() {
					t.Fatal("hardened while off")
				}
				// A same-site or cross-site page is treated as before: only
				// the session cookie decides, and the first cookie of a name
				// is the one read.
				for _, site := range []string{"same-site", "cross-site"} {
					if w := (requestShape{method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": site}}).send(s); w.Code == http.StatusForbidden {
						t.Fatalf("%s refused while off", site)
					}
				}
				if w := (requestShape{method: "GET", path: "/api/settings", cookies: []string{strings.Repeat("f", 64), ""}}).send(s); w.Code != http.StatusUnauthorized {
					t.Fatalf("shadowed cookie while off: %d", w.Code)
				}
			}
			// Nothing listens on the front's port range.
			for port := scienceFrontFirstPort; port < scienceFrontFirstPort+scienceFrontPortCount; port++ {
				if conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond); err == nil {
					conn.Close()
					if workload.scienceFrontRunning() == nil {
						t.Logf("port %d is in use by another program", port)
					}
				}
			}
		})
	}
}

func TestScienceHostRenderAvailability(t *testing.T) {
	trusted := requestShape{method: "GET", path: "/api/claude-science", trusted: true}
	cases := []struct {
		name  string
		setup func(*Server)
		shape requestShape
		code  string
	}{
		{"Windows window", nil, trusted, ""},
		{"macOS host", func(*Server) { scienceHostOS = "darwin" }, trusted, "platform"},
		{"Linux host", func(*Server) { scienceHostOS = "linux" }, trusted, "platform"},
		{"browser or phone without the window's proof", nil, requestShape{method: "GET", path: "/api/claude-science"}, "client"},
		{"desktop entry", func(s *Server) { s.desktopEntry = true }, trusted, "client"},
		{"client only", func(s *Server) { s.clientOnly = true }, trusted, "client"},
		{"Windows guest", func(s *Server) { s.windowsGuest = &guestos.WindowsProfile{} }, trusted, "guest"},
		{"macOS guest", func(s *Server) { s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"} }, trusted, "guest"},
		{"arm64 host", func(*Server) { scienceHostArch = "arm64" }, trusted, "guest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := scienceWorkload(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			r := httptest.NewRequest("GET", s.origin+"/api/claude-science", nil)
			if tc.shape.trusted {
				r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
			}
			state := s.scienceHostRender(r)
			if state.UnavailableCode != tc.code || state.Available != (tc.code == "") || (tc.code != "" && state.UnavailableReason == "") {
				t.Fatalf("%+v", state)
			}
		})
	}
}

func TestScienceHostRenderOnHardensOnlyCrossSiteRequests(t *testing.T) {
	workload := scienceWorkload(t)
	entry := scienceEntry(t, workload)
	before := map[*Server]map[string]int{workload: shapeCodes(workload), entry: shapeCodes(entry)}

	// Turning it on needs the window.
	if w := (requestShape{method: "POST", path: "/api/claude-science/host-render"}).send(workload); w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
		t.Fatalf("empty body: %d", w.Code)
	}
	on := func(s *Server, enabled bool, trusted bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", s.origin+"/api/claude-science/host-render", strings.NewReader(fmt.Sprintf(`{"enabled":%v}`, enabled)))
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		if trusted {
			r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	if w := on(workload, true, false); w.Code != http.StatusConflict || workload.scienceFrontRunning() != nil {
		t.Fatalf("enabled without the window: %d", w.Code)
	}
	w := on(workload, true, true)
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body)
	}
	var state scienceHostRenderState
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || !state.Enabled || !state.Running || !state.Available {
		t.Fatalf("%s", w.Body)
	}
	if strings.Contains(w.Body.String(), "port") {
		t.Fatalf("the front port reached the page: %s", w.Body)
	}
	saved, _ := os.ReadFile(filepath.Join(workload.profile, "workbench.json"))
	var stored Preferences
	if json.Unmarshal(saved, &stored) != nil || !stored.ScienceHostRender || stored.Keyboard != "mac" {
		t.Fatalf("setting not saved with the others: %s", saved)
	}
	front := workload.scienceFrontRunning()
	data, _ := os.ReadFile(filepath.Join(workload.profile, "control.json"))
	var record controlFile
	if json.Unmarshal(data, &record) != nil || record.SciencePort != front.port || record.Port != 17777 || record.Token != workload.token {
		t.Fatalf("control file: %s", data)
	}
	// The front answers only for its own Host.
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/_cage/enter", front.port))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("front with a foreign Host: %d", response.StatusCode)
	}

	// The entry's document may frame exactly that front; the workload's own
	// document still frames nothing.
	entry.scienceHost.cache.at = time.Time{}
	if got := frameSources(t, entry); got != front.origin {
		t.Fatalf("entry frame-src %q", got)
	}
	if got := frameSources(t, workload); got != "'none'" {
		t.Fatalf("workload frame-src %q", got)
	}
	// Every known caller gets exactly what it got before.
	for s, codes := range before {
		if after := shapeCodes(s); fmt.Sprint(after) != fmt.Sprint(codes) {
			t.Fatalf("callers changed:\nbefore %v\nafter  %v", codes, after)
		}
	}
	// Only a page on another site (or another loopback port) is refused, and
	// a shadowing cookie no longer locks the real one out.
	for _, s := range []*Server{workload, entry} {
		for _, site := range []string{"same-site", "cross-site"} {
			if w := (requestShape{method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": site}}).send(s); w.Code != http.StatusForbidden {
				t.Fatalf("%s: %d", site, w.Code)
			}
		}
		if w := (requestShape{method: "GET", path: "/api/settings", cookies: []string{strings.Repeat("f", 64), ""}}).send(s); w.Code == http.StatusUnauthorized {
			t.Fatal("shadowed cookie still locks out")
		}
	}
	// The settings pages do not know the switch: a save keeps it.
	cfg, _ := loadConfig(workload.configPath)
	workload.mu.Lock()
	view := settingsView(cfg, workload.preferences)
	workload.mu.Unlock()
	body, _ := json.Marshal(view)
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	delete(fields, "science_host_render")
	body, _ = json.Marshal(fields)
	recorder := httptest.NewRecorder()
	save := httptest.NewRequest("POST", workload.origin+"/api/settings", strings.NewReader(string(body)))
	save.Header.Set("Content-Type", "application/json")
	proposal, ok := workload.proposed(recorder, save)
	if !ok || !proposal.Preferences.ScienceHostRender {
		t.Fatalf("a settings save dropped the switch: %d %s", recorder.Code, recorder.Body)
	}

	// Off again: everything as before.
	if w := on(workload, false, false); w.Code != http.StatusOK {
		t.Fatalf("disable: %d", w.Code)
	}
	if workload.scienceFrontRunning() != nil {
		t.Fatal("front kept running")
	}
	if conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", front.port), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("front still listening")
	}
	data, _ = os.ReadFile(filepath.Join(workload.profile, "control.json"))
	if strings.Contains(string(data), "science") {
		t.Fatalf("control file kept the front: %s", data)
	}
	entry.scienceHost.cache.at = time.Time{}
	for s, codes := range before {
		if got := frameSources(t, s); got != "'none'" {
			t.Fatalf("frame-src after off: %q", got)
		}
		if after := shapeCodes(s); fmt.Sprint(after) != fmt.Sprint(codes) {
			t.Fatalf("callers changed after off: %v %v", codes, after)
		}
		if w := (requestShape{method: "GET", path: "/api/settings", headers: map[string]string{"Sec-Fetch-Site": "same-site"}}).send(s); w.Code == http.StatusForbidden {
			t.Fatal("still hardened after off")
		}
	}
}

func TestScienceTicketRoute(t *testing.T) {
	s := scienceWorkload(t)
	s.preferences.ScienceHostRender = true
	s.syncScienceFront()
	front := s.scienceFrontRunning()
	if front == nil {
		t.Fatal("front not running")
	}
	cfg, _ := loadConfig(s.configPath)
	cfg.Timezone, cfg.BrowserLanguage = "Asia/Tokyo", "ja-JP"
	if err := writePrivate(s.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	audits := 0
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/audit" {
			return nil, fmt.Errorf("unexpected %s", r.URL.Path)
		}
		audits++
		body := `{"timezone":"Asia/Tokyo","expected_timezone":"Asia/Tokyo","language":"ja-JP","languages":["ja-JP"],"platform":"Linux x86_64","device":{"user_agent":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36","platform":"Linux x86_64","client_hints":{"brands":[{"brand":"Chromium","version":"157"}],"platform":"Linux","mobile":false}}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	ticket := func(trusted bool, host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", s.origin+"/api/claude-science/ticket", strings.NewReader(host))
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		if trusted {
			r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	host := `{"host":{"timezone":"America/Los_Angeles","offset":480,"languages":["fr-FR","fr"]}}`
	if w := ticket(true, host); w.Code != http.StatusConflict {
		t.Fatalf("ticket before the daemon runs: %d", w.Code)
	}
	s.scienceLast.set(&claudeScienceState{Installed: true, Running: true, Ready: true})
	s.ready = true
	if w := ticket(false, host); w.Code != http.StatusConflict {
		t.Fatalf("ticket without the window: %d", w.Code)
	}
	if w := ticket(true, `{"host":{"timezone":"../x"}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid host identity: %d", w.Code)
	}
	w := ticket(true, host)
	if w.Code != http.StatusOK {
		t.Fatalf("ticket: %d %s", w.Code, w.Body)
	}
	var reply struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	if !strings.HasPrefix(reply.URL, front.origin+"/_cage/enter#") || len(strings.TrimPrefix(reply.URL, front.origin+"/_cage/enter#")) != 64 || audits != 1 {
		t.Fatalf("ticket URL %q, audits %d", reply.URL, audits)
	}
	// The shell's copy of the identity is in the control file, not the page.
	data, _ := os.ReadFile(filepath.Join(s.profile, "control.json"))
	var record controlFile
	if json.Unmarshal(data, &record) != nil || record.ScienceIdentity == nil || record.ScienceIdentity.Timezone != "Asia/Tokyo" || record.ScienceIdentity.AcceptLanguage != "ja-JP" {
		t.Fatalf("control file: %s", data)
	}
	if strings.Contains(w.Body.String(), "Asia/Tokyo") {
		t.Fatal("identity in the page's reply")
	}
	// Locked account: no ticket.
	s.account.lock()
	if w := ticket(true, host); w.Code != http.StatusLocked {
		t.Fatalf("ticket while locked: %d", w.Code)
	}
}

// liveScienceFront gives a running front a session and a credential, as
// after a frame opened it.
func liveScienceFront(t *testing.T, s *Server) (*scienceFront, *fakeScienceDaemon) {
	t.Helper()
	front := s.scienceFrontRunning()
	daemon := newFakeScienceDaemon(t)
	upstream := newScienceUpstream(tcpDialer{address: strings.TrimPrefix(daemon.server.URL, "http://")}, 47800, daemon.link)
	if _, err := upstream.token(t.Context()); err != nil {
		t.Fatal(err)
	}
	epoch, unlocked := s.account.accessFor(true, false)
	if !unlocked {
		t.Fatal("locked")
	}
	front.mu.Lock()
	front.upstream, front.session, front.current = upstream, webfront.Secret(32), scienceTicket{epoch: epoch, identity: testScienceIdentity}
	front.state.Session = true
	front.mu.Unlock()
	return front, daemon
}

func frontCleared(front *scienceFront) bool {
	front.mu.Lock()
	defer front.mu.Unlock()
	return front.upstream == nil && front.session == ""
}

func TestScienceFrontDropsTheCredential(t *testing.T) {
	old := scienceWatchInterval
	scienceWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { scienceWatchInterval = old })
	start := func(t *testing.T) *Server {
		s := scienceWorkload(t)
		running := true
		s.scienceHost.vmRunning = func() bool { return running }
		s.preferences.ScienceHostRender = true
		s.scienceLast.set(&claudeScienceState{Installed: true, Running: true, Ready: true, StartedAt: "1"})
		s.syncScienceFront()
		return s
	}
	t.Run("account lock", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		s.account.lock()
		waitFor(t, 5*time.Second, "the reset", func() bool { return frontCleared(front) })
		if !strings.Contains(front.snapshot().Refused, "锁定") {
			t.Fatal(front.snapshot().Refused)
		}
	})
	t.Run("environment stop", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		s.scienceHost.vmRunning = func() bool { return false }
		waitFor(t, 5*time.Second, "the reset", func() bool { return frontCleared(front) })
	})
	t.Run("daemon restart", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		time.Sleep(50 * time.Millisecond) // the watchdog has seen started_at 1
		if frontCleared(front) {
			t.Fatal("reset without a reason")
		}
		s.scienceLast.set(&claudeScienceState{Installed: true, Running: true, Ready: true, StartedAt: "2"})
		waitFor(t, 5*time.Second, "the reset", func() bool { return frontCleared(front) })
	})
	t.Run("daemon stopped", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		s.scienceLast.set(&claudeScienceState{Installed: true})
		waitFor(t, 5*time.Second, "the reset", func() bool { return frontCleared(front) })
	})
	t.Run("stop and update actions", func(t *testing.T) {
		for _, action := range []string{"stop", "update"} {
			s := start(t)
			front, _ := liveScienceFront(t, s)
			// The job itself needs a real environment; the reset comes first.
			(requestShape{method: "POST", path: "/api/claude-science/" + action, trusted: true}).send(s)
			if !frontCleared(front) {
				t.Fatalf("%s kept the credential", action)
			}
		}
	})
	t.Run("pane released", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		if w := (requestShape{method: "POST", path: "/api/claude-science/release", trusted: true}).send(s); w.Code != http.StatusNoContent {
			t.Fatalf("release: %d", w.Code)
		}
		if !frontCleared(front) {
			t.Fatal("release kept the credential")
		}
	})
	t.Run("idle", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		now := time.Now()
		front.mu.Lock()
		front.now = func() time.Time { return now.Add(scienceIdleDrop + time.Second) }
		front.lastActive = now
		front.mu.Unlock()
		waitFor(t, 5*time.Second, "the idle drop", func() bool {
			front.mu.Lock()
			defer front.mu.Unlock()
			return front.upstream == nil
		})
		front.mu.Lock()
		session := front.session
		front.mu.Unlock()
		if session == "" {
			t.Fatal("idle ended the session too")
		}
	})
	t.Run("busy is not idle", func(t *testing.T) {
		s := start(t)
		front, _ := liveScienceFront(t, s)
		front.begin()
		now := time.Now()
		front.mu.Lock()
		front.now = func() time.Time { return now.Add(time.Hour) }
		front.mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		front.mu.Lock()
		kept := front.upstream != nil
		front.mu.Unlock()
		front.end()
		if !kept {
			t.Fatal("an open socket lost its credential")
		}
	})
}

func TestScienceEntryKeepsTheSwitchLocal(t *testing.T) {
	workload := scienceWorkload(t)
	entry := scienceEntry(t, workload)
	for _, path := range []string{"/api/claude-science/host-render", "/api/claude-science/ticket"} {
		r := httptest.NewRequest("POST", entry.origin+path, strings.NewReader("{}"))
		// No local environment is selected in this entry fixture: as for a
		// remote host, the entry answers itself.
		if !entry.scienceEntryRefuses(httptest.NewRecorder(), r) {
			t.Fatalf("%s forwarded without a local selection", path)
		}
	}
	for _, path := range []string{"/api/claude-science", "/api/claude-science/release", "/api/claude-science/open"} {
		r := httptest.NewRequest("POST", entry.origin+path, strings.NewReader("{}"))
		if entry.scienceEntryRefuses(httptest.NewRecorder(), r) {
			t.Fatalf("%s refused", path)
		}
	}
	// A workload never refuses here.
	if workload.scienceEntryRefuses(httptest.NewRecorder(), httptest.NewRequest("POST", workload.origin+"/api/claude-science/ticket", nil)) {
		t.Fatal("workload refused")
	}
}
