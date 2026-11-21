package workbench

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Review 3 reproductions as browser tests (F1, F2, F3). The window and the
// pane are played by scienceShell (science_shell_test.go), which does what
// desktop/windows/ScienceFrameHost.cs and claude-science.jsx do.

// scienceIdentityPage reports the identity it sees whenever it changes, in
// base64 so that no audit stands between the page and the test, and keeps
// starting workers that die 20-89 ms later (the reviewer's exp1).
const scienceIdentityPage = `<!doctype html><title>Claude Science</title><script nonce="%s">
const id = () => [Intl.DateTimeFormat().resolvedOptions().timeZone, new Date().getTimezoneOffset(), navigator.language, navigator.userAgent].join("|");
let last = "";
const report = (why) => { const v = id(); if (v === last) return; last = v; fetch("/api/test/results", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ why, v: btoa(v) }) }).catch(() => {}); };
report("start");
setInterval(() => report("poll"), 2);
let rounds = 0;
const churn = () => { for (let d = 20; d < 90; d++) { const w = new Worker("/assets/w.js"); setTimeout(() => w.terminate(), d); } if (++rounds < 8) setTimeout(churn, 150); };
setTimeout(churn, 300);
</script>`

// scienceFrameHarness stands in for the desktop entry's document: the
// workbench CSP, the workbench session cookie as server.go sets it, an
// /api/echo that records the Cookie header it gets, and a slow page to
// navigate to.
type scienceFrameHarness struct {
	origin     string
	cookieName string
	mu         sync.Mutex
	requests   []string
}

func newScienceFrameHarness(t *testing.T, frameSources string) *scienceFrameHarness {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	h := &scienceFrameHarness{origin: fmt.Sprintf("http://localhost:%d", port), cookieName: fmt.Sprintf("cage_session_%d", port)}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// As server.go: no response of the workbench is kept.
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: h.cookieName, Value: "real", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400 * 30})
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws://"+r.Host+"; img-src 'self' data: blob:; object-src 'none'; frame-src "+frameSources+"; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><title>Super Cage</title><div id=\"pane\"></div>")
		case "/slow":
			time.Sleep(1500 * time.Millisecond)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><title>next</title>")
		case "/favicon.ico":
			http.NotFound(w, r)
		default:
			h.mu.Lock()
			h.requests = append(h.requests, r.Method+" "+r.URL.Path+" cookie="+r.Header.Get("Cookie"))
			h.mu.Unlock()
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "ok")
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return h
}

func (h *scienceFrameHarness) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

type scienceIdentityReport struct{ Why, Value string }

func scienceIdentityReports(t *testing.T, daemon *fakeScienceDaemon) []scienceIdentityReport {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var reports []scienceIdentityReport
	for _, raw := range daemon.results {
		var report struct{ Why, V string }
		if json.Unmarshal(raw, &report) != nil || report.V == "" {
			continue
		}
		value, _ := base64.StdEncoding.DecodeString(report.V)
		reports = append(reports, scienceIdentityReport{report.Why, string(value)})
	}
	return reports
}

func scienceGuestValues() string {
	offset, _ := jsOffset(testScienceIdentity.Timezone, time.Now())
	return strings.Join([]string{testScienceIdentity.Timezone, fmt.Sprint(offset), testScienceIdentity.Language, testScienceIdentity.UserAgent}, "|")
}

// F1: while the guest page lives it never sees the host's values: not when
// a worker dies while the shell prepares it, not while the window's top
// document navigates away, not across a re-ticket of the same frame.
func TestScienceBrowserFrameNeverSeesTheHost(t *testing.T) {
	for _, variant := range []string{"worker dies while attached", "top-level navigation", "re-ticket"} {
		t.Run(variant, func(t *testing.T) { runScienceFrameNeverSeesTheHost(t, variant) })
	}
}

func runScienceFrameNeverSeesTheHost(t *testing.T, variant string) {
	env := newTestScienceFront(t)
	env.daemon.page = func(nonce string) string { return fmt.Sprintf(scienceIdentityPage, nonce) }
	env.daemon.assets["/assets/w.js"] = "postMessage(1)"
	harness := newScienceFrameHarness(t, scienceTestFrameSources(env))
	env.front.hooks.ancestor = func() string { return harness.origin }
	proxyPort, _ := newScienceCanaryProxy(t)
	browser := launchTestBrowser(t, scienceHostEnv, append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")...)
	page := browser.page(t)
	shell := newScienceShell(t, browser, page)
	shell.load(t, harness.origin+"/")
	ticket := func() string {
		return env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()})
	}
	shell.open(t, ticket())
	waitFor(t, 30*time.Second, "the session", func() bool { return env.front.snapshot().Session })
	waitFor(t, 15*time.Second, "the app's first report", func() bool { return len(scienceIdentityReports(t, env.daemon)) > 0 })
	switch variant {
	case "worker dies while attached":
		time.Sleep(3 * time.Second)
	case "top-level navigation":
		time.Sleep(600 * time.Millisecond)
		if scienceShellTrace {
			t.Logf("%s navigate", time.Now().Format("05.000"))
		}
		shell.navigateTop(t, harness.origin+"/slow")
		time.Sleep(3 * time.Second)
	case "re-ticket":
		time.Sleep(600 * time.Millisecond)
		before := len(scienceIdentityReports(t, env.daemon))
		shell.reticket(t, ticket())
		waitFor(t, 30*time.Second, "the new session", func() bool { return env.front.snapshot().Session })
		waitFor(t, 15*time.Second, "the new document's report", func() bool { return len(scienceIdentityReports(t, env.daemon)) > before })
		time.Sleep(2 * time.Second)
	}
	guest := scienceGuestValues()
	reports := scienceIdentityReports(t, env.daemon)
	t.Logf("%d identity reports, %d failed calls, %d frames ended by the shell", len(reports), shell.failed.Load(), shell.ended())
	for _, report := range reports {
		if scienceShellTrace {
			t.Logf("report %s %.40q", report.Why, report.Value)
		}
		if report.Value != guest {
			t.Errorf("the guest page saw %q (%s)", report.Value, report.Why)
		}
	}
}

// scienceStoragePage leaves what one environment's page could leave for
// another's: a document the browser may keep, local storage and a cookie
// named like the daemon's CSRF cookie.
const scienceStoragePage = `<!doctype html><title>Claude Science</title><script nonce="%[1]s">
const prior = { storage: localStorage.getItem("env") || "", cookie: document.cookie };
localStorage.setItem("env", "%[2]s");
document.cookie = "operon_csrf=planted-%[2]s; path=/";
fetch("/api/test/results", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ doc: "%[2]s", prior }) });
</script>`

// F2: environment B's front on the port A's front used never serves A's
// document, and nothing A's page stored reaches B's page or B's daemon.
func TestScienceBrowserEnvironmentsShareNothing(t *testing.T) {
	a := newTestScienceFront(t)
	a.daemon.page = func(nonce string) string { return fmt.Sprintf(scienceStoragePage, nonce, "A") }
	a.daemon.rootHeaders = map[string]string{"Cache-Control": "public, max-age=31536000, immutable", "Etag": `"a"`, "Last-Modified": "Mon, 01 Jan 2024 00:00:00 GMT"}
	port := a.front.port
	harness := newScienceFrameHarness(t, scienceTestFrameSources(a))
	a.front.hooks.ancestor = func() string { return harness.origin }
	proxyPort, _ := newScienceCanaryProxy(t)
	browser := launchTestBrowser(t, scienceHostEnv, append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")...)
	page := browser.page(t)
	shell := newScienceShell(t, browser, page)
	shell.load(t, harness.origin+"/")
	shell.open(t, a.front.issue(scienceTicket{epoch: a.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
	waitFor(t, 30*time.Second, "A's report", func() bool { return len(scienceDocReports(a.daemon)) > 0 })
	time.Sleep(500 * time.Millisecond)
	// A's environment stops; B's front takes the same port.
	shell.close(t)
	a.front.close()
	b := newTestScienceFrontAt(t, port)
	b.daemon.page = func(nonce string) string { return fmt.Sprintf(scienceStoragePage, nonce, "B") }
	b.front.hooks.ancestor = func() string { return harness.origin }
	shell.open(t, b.front.issue(scienceTicket{epoch: b.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
	waitFor(t, 30*time.Second, "a report at B", func() bool { return len(scienceDocReports(b.daemon)) > 0 })
	time.Sleep(500 * time.Millisecond)
	for _, report := range scienceDocReports(b.daemon) {
		t.Logf("B's daemon got %+v", report)
		if report.Doc != "B" {
			t.Errorf("B's frame ran %s's document", report.Doc)
		}
		if report.Prior.Storage != "" || strings.Contains(report.Prior.Cookie, "planted") {
			t.Errorf("B's page found what an earlier page left: %+v", report.Prior)
		}
	}
	for _, request := range b.daemon.seen() {
		if strings.Contains(request.Cookie, "planted") || strings.Contains(request.Header.Get("X-Csrf-Token"), "planted") {
			t.Errorf("B's daemon got a planted value: %s %s cookie=%q", request.Method, request.Path, request.Cookie)
		}
	}
}

type scienceDocReport struct {
	Doc   string `json:"doc"`
	Prior struct {
		Storage string `json:"storage"`
		Cookie  string `json:"cookie"`
	} `json:"prior"`
}

func scienceDocReports(daemon *fakeScienceDaemon) []scienceDocReport {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var reports []scienceDocReport
	for _, raw := range daemon.results {
		var report scienceDocReport
		if json.Unmarshal(raw, &report) == nil && report.Doc != "" {
			reports = append(reports, report)
		}
	}
	return reports
}

// scienceCookiePage writes every cookie it may into the workbench's
// cookies: a shadow of the session cookie on a longer path, the strong
// forms (Domain=localhost, SameSite=None, Secure, Partitioned) and a flood
// past the workbench's 16 KiB header limit.
const scienceCookiePage = `<!doctype html><title>Claude Science</title><script nonce="%[1]s">
const big = "x".repeat(4000);
const tries = ["%[2]s=impostor; path=/api; max-age=100000000", "%[2]s=impostor2; domain=localhost; path=/api; secure; samesite=none", "%[2]s=impostor3; domain=localhost; path=/api; secure; samesite=none; partitioned"];
for (let i = 0; i < 6; i++) tries.push("bomb" + i + "=" + big + "; path=/; max-age=100000000", "dbomb" + i + "=" + big + "; domain=localhost; path=/; secure; samesite=none");
for (const c of tries) document.cookie = c;
fetch("/api/test/results", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ doc: "cookies", prior: { cookie: document.cookie.slice(0, 200) } }) });
</script>`

// scienceWorkbenchCallsPage is a page on the front's host form without any
// CSP: what reaches the workbench if the frame's own policy were gone.
const scienceWorkbenchCallsPage = `<!doctype html><script>
const w = %q;
Promise.all([
  fetch(w + "/api/echo-get", { mode: "no-cors", credentials: "include" }).catch(() => {}),
  fetch(w + "/api/echo-post", { method: "POST", mode: "no-cors", credentials: "include", body: "x" }).catch(() => {}),
  new Promise((resolve) => { const s = new WebSocket(w.replace("http", "ws") + "/api/echo-ws"); s.onerror = s.onclose = resolve; setTimeout(resolve, 2000); }),
]).then(() => parent.postMessage("sent", "*"));
</script>`

// F3: nothing the frame does produces a Cookie header on the workbench:
// neither its document.cookie writes nor its own requests to the workbench.
func TestScienceBrowserFrameCannotTouchWorkbenchCookies(t *testing.T) {
	env := newTestScienceFront(t)
	calls, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := scienceTestFrameHost(env)
	callsOrigin := fmt.Sprintf("http://%s:%d", host, calls.Addr().(*net.TCPAddr).Port)
	harness := newScienceFrameHarness(t, scienceTestFrameSources(env)+" "+callsOrigin)
	env.front.hooks.ancestor = func() string { return harness.origin }
	env.daemon.page = func(nonce string) string {
		return fmt.Sprintf(scienceCookiePage, nonce, harness.cookieName)
	}
	callServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, scienceWorkbenchCallsPage, harness.origin)
	}))
	callServer.Listener = calls
	callServer.Start()
	t.Cleanup(callServer.Close)
	proxyPort, _ := newScienceCanaryProxy(t)
	browser := launchTestBrowser(t, scienceHostEnv, append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")...)
	page := browser.page(t)
	shell := newScienceShell(t, browser, page)
	shell.load(t, harness.origin+"/")
	shell.open(t, env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
	waitFor(t, 30*time.Second, "the cookie page", func() bool {
		for _, request := range env.daemon.seen() {
			if request.Path == "/" {
				return true
			}
		}
		return false
	})
	// Its report may not get through its own flood.
	for deadline := time.Now().Add(5 * time.Second); len(scienceDocReports(env.daemon)) == 0 && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	if reports := scienceDocReports(env.daemon); len(reports) > 0 {
		t.Logf("the frame's own cookies: %+v", reports[0].Prior)
	}
	// The workbench's own request after the writes.
	status := shell.eval(t, "fetch('/api/echo-top').then(r => r.status, e => 'error ' + e)")
	if fmt.Sprint(status) != "200" {
		t.Errorf("the workbench answered its own request with %v", status)
	}
	// Requests the frame's document makes to the workbench itself.
	shell.eval(t, fmt.Sprintf("(() => { const f = document.createElement('iframe'); f.src = %q; document.getElementById('pane').append(f); return true })()", callsOrigin+"/"))
	waitFor(t, 15*time.Second, "the frame's calls", func() bool {
		count := 0
		for _, line := range harness.seen() {
			if strings.Contains(line, "/api/echo-") && !strings.Contains(line, "echo-top") {
				count++
			}
		}
		return count >= 3
	})
	want := harness.cookieName + "=real"
	for _, line := range harness.seen() {
		path, cookie, _ := strings.Cut(line, " cookie=")
		switch {
		case strings.HasSuffix(path, "/api/echo-top"):
			if cookie != want {
				t.Errorf("the workbench's own request carried %s, want %q", scienceCookieSummary(cookie), want)
			}
		default:
			if cookie != "" {
				t.Errorf("%s from the frame carried %s", path, scienceCookieSummary(cookie))
			}
		}
	}
}

// scienceTestFrameHost is the host the front's frame URLs use.
func scienceTestFrameHost(env *testScienceFront) string {
	u, _ := url.Parse(env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
	env.front.reset("")
	return u.Hostname()
}

// F2 and F3 at the front: nothing it serves may be kept by the browser, no
// cookie crosses it in either direction, and a flood of the page's own
// cookies does not end the session.
func TestScienceFrontLeavesNothingInTheBrowser(t *testing.T) {
	env := newTestScienceFront(t)
	env.daemon.assets["/assets/app.js"] = "1"
	env.daemon.rootHeaders = map[string]string{"Cache-Control": "public, max-age=31536000, immutable", "Etag": `"a"`, "Last-Modified": "Mon, 01 Jan 2024 00:00:00 GMT", "Expires": "Thu, 01 Jan 2037 00:00:00 GMT", "Age": "10", "Vary": "Accept"}
	env.ticket(t)
	enter := env.do(t, env.client(t), "GET", "/_cage/enter", "", "", nil)
	if got := enter.Header.Get("Clear-Site-Data"); got != `"cache", "storage"` {
		t.Errorf("enter page Clear-Site-Data %q", got)
	}
	client, code := env.enter(t, goodProbeReports(testScienceIdentity))
	if code != http.StatusNoContent {
		t.Fatalf("probe: %d", code)
	}
	// The daemon sets its cookies; the front keeps them.
	if r := env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode != 200 || len(r.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("auth status: %d, Set-Cookie %q", r.StatusCode, r.Header.Values("Set-Cookie"))
	}
	planted := map[string]string{"Cookie": "operon_csrf=planted; other=1", "If-None-Match": `"a"`, "If-Modified-Since": "Mon, 01 Jan 2024 00:00:00 GMT", "X-Csrf-Token": "planted"}
	for _, path := range []string{"/", "/assets/app.js"} {
		r := env.do(t, client, "GET", path, "", "", planted)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, r.StatusCode, readBody(t, r))
		}
		if got := r.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control %q", path, got)
		}
		for _, name := range []string{"Etag", "Last-Modified", "Expires", "Age", "Vary", "Set-Cookie"} {
			if r.Header.Get(name) != "" {
				t.Errorf("%s passed %s: %q", path, name, r.Header.Get(name))
			}
		}
	}
	post := env.do(t, client, "POST", "/api/echo", env.sessionOrigin(), "{}", map[string]string{"Content-Type": "application/json", "Cookie": "operon_csrf=planted"})
	if post.StatusCode != 200 {
		t.Fatalf("post: %d", post.StatusCode)
	}
	var sawHeld bool
	for _, request := range env.daemon.seen() {
		for _, name := range []string{"If-None-Match", "If-Modified-Since"} {
			if request.Header.Get(name) != "" {
				t.Errorf("%s %s forwarded %s", request.Method, request.Path, name)
			}
		}
		if strings.Contains(request.Cookie, "planted") || strings.Contains(request.Cookie, "other=1") || request.Header.Get("X-Csrf-Token") == "planted" {
			t.Errorf("%s %s forwarded the browser's value: cookie=%q token=%q", request.Method, request.Path, request.Cookie, request.Header.Get("X-Csrf-Token"))
		}
		if request.Path == "/api/echo" && strings.Contains(request.Cookie, "operon_csrf=csrf-status") && request.Header.Get("X-Csrf-Token") == "csrf-status" {
			sawHeld = true
		}
	}
	if !sawHeld {
		t.Error("the daemon's own operon_csrf was not replayed by the front")
	}
	// A flood of the page's own cookies (Chromium keeps up to 180 of 4 KiB
	// per host) does not end the session.
	flood := strings.Repeat("f="+strings.Repeat("x", 4090)+"; ", 180)
	if r := env.do(t, client, "GET", "/assets/app.js", "", "", map[string]string{"Cookie": flood}); r.StatusCode != 200 {
		t.Errorf("flooded request: %d", r.StatusCode)
	}
}

// scienceCookieSummary names the cookies of a Cookie header with their
// value sizes (a flood would fill the log).
func scienceCookieSummary(header string) string {
	var parts []string
	for _, part := range strings.Split(header, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		if len(value) > 24 {
			value = fmt.Sprintf("<%d bytes>", len(value))
		}
		parts = append(parts, name+"="+value)
	}
	return fmt.Sprintf("%q (%d bytes)", strings.Join(parts, "; "), len(header))
}
