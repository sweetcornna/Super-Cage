package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/webfront"
)

func TestScienceTicketReplayExpiryAndEpoch(t *testing.T) {
	env := newTestScienceFront(t)
	client := env.client(t)
	post := func(ticket string) int {
		body, _ := json.Marshal(map[string]string{"ticket": ticket})
		return env.do(t, client, "POST", "/_cage/enter", env.front.origin, string(body), map[string]string{"Content-Type": "application/json"}).StatusCode
	}
	ticket := env.ticket(t)
	if code := post(ticket); code != 204 {
		t.Fatalf("first use: %d", code)
	}
	if code := post(ticket); code != 401 {
		t.Fatalf("replay accepted: %d", code)
	}
	// Expiry: 60 s on the front's clock.
	now := time.Now()
	env.front.now = func() time.Time { return now }
	late := env.ticket(t)
	env.front.now = func() time.Time { return now.Add(61 * time.Second) }
	if code := post(late); code != 401 {
		t.Fatalf("expired ticket accepted: %d", code)
	}
	env.front.now = time.Now
	// A ticket outlives nothing: locking the account voids it.
	locked := env.ticket(t)
	env.lock()
	if code := post(locked); code != 401 {
		t.Fatalf("ticket of a locked account accepted: %d", code)
	}
	for _, bad := range []string{"", "short", strings.Repeat("g", 64)} {
		if code := post(bad); code != 401 {
			t.Fatalf("invalid ticket %q: %d", bad, code)
		}
	}
	// The entry needs the front's own Origin and JSON.
	if r := env.do(t, client, "POST", "/_cage/enter", "http://localhost:17777", "{}", map[string]string{"Content-Type": "application/json"}); r.StatusCode != 403 {
		t.Fatalf("foreign origin entry: %d", r.StatusCode)
	}
}

func TestScienceTicketsAreSingleUseAndBounded(t *testing.T) {
	tickets := webfront.NewTickets[int](time.Minute, 2, nil)
	a, b, c := tickets.Issue(1), tickets.Issue(2), tickets.Issue(3)
	if _, ok := tickets.Consume(a); ok {
		t.Fatal("oldest ticket survived the bound")
	}
	if v, ok := tickets.Consume(b); !ok || v != 2 {
		t.Fatal("ticket lost")
	}
	if _, ok := tickets.Consume(b); ok {
		t.Fatal("ticket reused")
	}
	tickets.Clear()
	if _, ok := tickets.Consume(c); ok {
		t.Fatal("cleared ticket accepted")
	}
}

func TestScienceFrontHostCookieAndOriginGates(t *testing.T) {
	env := newTestScienceFront(t)
	client, code := env.enter(t, goodProbeReports(testScienceIdentity))
	if code != 204 {
		t.Fatalf("probe: %d", code)
	}
	if r := env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode != 200 {
		t.Fatalf("session request: %d %s", r.StatusCode, readBody(t, r))
	}
	// DNS rebinding: any other Host is refused before anything else.
	for _, host := range []string{"127.0.0.1:" + fmt.Sprint(env.front.port), "rebind.example:" + fmt.Sprint(env.front.port), "localhost"} {
		request, _ := http.NewRequest("GET", env.front.origin+"/api/auth/status", nil)
		request.Host = host
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 403 {
			t.Fatalf("Host %q: %d", host, response.StatusCode)
		}
	}
	// No cookie, or a wrong one.
	if r := env.do(t, env.client(t), "GET", "/api/auth/status", "", "", nil); r.StatusCode != 401 {
		t.Fatalf("no cookie: %d", r.StatusCode)
	}
	if r := env.do(t, env.client(t), "GET", "/api/auth/status", "", "", map[string]string{"Cookie": env.front.cookieName() + "=" + strings.Repeat("0", 64)}); r.StatusCode != 401 {
		t.Fatalf("forged cookie: %d", r.StatusCode)
	}
	// Writes and upgrades need the front's Origin.
	for _, origin := range []string{"", "http://localhost:17777", "http://localhost:47800", "null"} {
		if r := env.do(t, client, "POST", "/api/echo", origin, "{}", map[string]string{"Content-Type": "application/json"}); r.StatusCode != 403 {
			t.Fatalf("POST from %q: %d", origin, r.StatusCode)
		}
	}
	if r := env.do(t, client, "POST", "/api/echo", env.front.origin, "{}", map[string]string{"Content-Type": "application/json"}); r.StatusCode != 200 {
		t.Fatalf("POST from the front: %d", r.StatusCode)
	}
	// Workers that per-target overrides cannot reach.
	for _, header := range []map[string]string{{"Sec-Fetch-Dest": "serviceworker"}, {"Sec-Fetch-Dest": "sharedworker"}, {"Service-Worker": "script"}} {
		if r := env.do(t, client, "GET", "/assets/sw.js", "", "", header); r.StatusCode != 403 {
			t.Fatalf("worker script %v: %d", header, r.StatusCode)
		}
	}
	// Paths the tenant gateway's validPath refuses.
	for _, path := range []string{"/api/../api/echo", "/api/%2fecho", "/a\\b"} {
		request, _ := http.NewRequest("GET", env.front.origin+"/", nil)
		request.URL.Opaque = path
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("path %q: %d", path, response.StatusCode)
		}
	}
	// Locking the account ends the session at once.
	env.lock()
	if r := env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode != 401 {
		t.Fatalf("session after lock: %d", r.StatusCode)
	}
}

func TestScienceShadowCookieDoesNotLockOut(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	// A same-site page sets an impostor with a longer path: browsers send it first.
	cookies := client.Jar.Cookies(mustURL(t, env.front.origin+"/api/"))
	var real string
	for _, c := range cookies {
		if c.Name == env.front.cookieName() {
			real = c.Value
		}
	}
	header := env.front.cookieName() + "=" + strings.Repeat("f", 64) + "; " + env.front.cookieName() + "=" + real
	if r := env.do(t, env.client(t), "GET", "/api/auth/status", "", "", map[string]string{"Cookie": header}); r.StatusCode != 200 {
		t.Fatalf("shadowed session: %d", r.StatusCode)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestScienceProbeDecidesTheCookie(t *testing.T) {
	env := newTestScienceFront(t)
	host := testHostIdentity()
	cases := map[string]func([]scienceProbeReport){
		"host timezone in a worker": func(r []scienceProbeReport) { r[2].Timezone = host.Timezone },
		"host offset":               func(r []scienceProbeReport) { r[0].Offset = *host.Offset },
		"host language in a blob":   func(r []scienceProbeReport) { r[3].Language, r[3].Languages = "fr-FR", []string{"fr-FR", "fr"} },
		"host user agent":           func(r []scienceProbeReport) { r[1].UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)" },
		"host platform in a worker": func(r []scienceProbeReport) { r[2].Platform = "Win32" },
		"shared worker available":   func(r []scienceProbeReport) { r[0].SharedWorker = true },
		"missing context":           func(r []scienceProbeReport) { r[3].Context = "main" },
		"WebRTC candidates":         func(r []scienceProbeReport) { two := 2; r[0].RTC = &two },
		"WebRTC unmeasured":         func(r []scienceProbeReport) { r[0].RTC = nil },
	}
	for name, change := range cases {
		reports := goodProbeReports(testScienceIdentity)
		change(reports)
		client, code := env.enter(t, reports)
		if code != 403 {
			t.Fatalf("%s: probe accepted (%d)", name, code)
		}
		for _, c := range client.Jar.Cookies(mustURL(t, env.front.origin+"/")) {
			if c.Name == env.front.cookieName() {
				t.Fatalf("%s: session cookie issued", name)
			}
		}
		if r := env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode != 401 {
			t.Fatalf("%s: daemon reachable after a failed probe: %d", name, r.StatusCode)
		}
		if state := env.front.snapshot(); !strings.HasPrefix(state.Refused, "身份核对未通过") || state.Session {
			t.Fatalf("%s: refusal not reported: %+v", name, state)
		}
	}
	// The probe pages need the probe cookie; the probe cannot be replayed.
	if r := env.do(t, env.client(t), "GET", "/_cage/probe", "", "", nil); r.StatusCode != 401 {
		t.Fatalf("probe page without entry: %d", r.StatusCode)
	}
	client, code := env.enter(t, goodProbeReports(testScienceIdentity))
	if code != 204 {
		t.Fatalf("good probe: %d", code)
	}
	payload, _ := json.Marshal(map[string]any{"reports": goodProbeReports(testScienceIdentity)})
	if r := env.do(t, client, "POST", "/_cage/probe", env.front.origin, string(payload), map[string]string{"Content-Type": "application/json"}); r.StatusCode != 401 {
		t.Fatalf("probe replay: %d", r.StatusCode)
	}
	// A new entry revokes the previous document's session.
	first := client
	env.enter(t, goodProbeReports(testScienceIdentity))
	if r := env.do(t, first, "GET", "/api/auth/status", "", "", nil); r.StatusCode != 401 {
		t.Fatalf("old session survived a new entry: %d", r.StatusCode)
	}
}

func TestScienceProbeReportCheck(t *testing.T) {
	good := goodProbeReports(testScienceIdentity)
	if reason := checkScienceProbe(testScienceIdentity, good, time.Now()); reason != "" {
		t.Fatal(reason)
	}
	// Without userAgentData (null) the other values still decide.
	noHints := goodProbeReports(testScienceIdentity)
	for i := range noHints {
		noHints[i].UAPlatform = nil
	}
	if reason := checkScienceProbe(testScienceIdentity, noHints, time.Now()); reason != "" {
		t.Fatal(reason)
	}
	if reason := checkScienceProbe(testScienceIdentity, good[:3], time.Now()); !strings.Contains(reason, "没有完成") {
		t.Fatalf("missing blob worker: %q", reason)
	}
	unknown := append(goodProbeReports(testScienceIdentity), scienceProbeReport{Context: "service"})
	if reason := checkScienceProbe(testScienceIdentity, unknown, time.Now()); reason == "" {
		t.Fatal("unknown context accepted")
	}
}

func TestScienceProxyRewritesBothWays(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	request, _ := http.NewRequest("GET", env.front.origin+"/", nil)
	request.Header.Set("Cookie", "cage_session_17777="+strings.Repeat("b", 64)+"; operon_csrf=keep; operon_auth=stolen; other=1")
	request.Header.Set("Accept-Language", "fr-FR,fr;q=0.9")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Edg/157")
	request.Header.Set("X-Forwarded-For", "10.0.0.1")
	request.Header.Set("Forwarded", "for=10.0.0.1")
	request.Header.Set("Authorization", "Bearer page-supplied")
	request.Header.Set("X-Operon-Client", "spa")
	request.Header.Set("Sec-Ch-Ua-Platform", "\"Windows\"")
	for _, c := range client.Jar.Cookies(mustURL(t, env.front.origin+"/")) {
		request.AddCookie(c)
	}
	response, err := env.client(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, response)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(body, "inlineRan") {
		t.Fatalf("document: %d %s", response.StatusCode, body)
	}
	seen := env.daemon.seen()
	last := seen[len(seen)-1]
	bearers := env.daemon.bearers()
	if len(bearers) != 1 || last.Authorization != "Bearer "+bearers[0] {
		t.Fatalf("bearer not injected: %q", last.Authorization)
	}
	if last.Host != "localhost:47800" || last.Forwarded || last.Origin != "" {
		t.Fatalf("upstream identity headers: %+v", last)
	}
	if last.Cookie != "operon_csrf=keep" {
		t.Fatalf("upstream cookies: %q", last.Cookie)
	}
	if last.UserAgent != testScienceIdentity.UserAgent || last.AcceptLanguage != "ja-JP,ja;q=0.9" {
		t.Fatalf("UA/Accept-Language not the guest's: %q %q", last.UserAgent, last.AcceptLanguage)
	}
	if last.Header.Get("X-Operon-Client") != "spa" || last.Header.Get("Sec-Ch-Ua-Platform") != "" {
		t.Fatalf("header allow-list: %v", last.Header)
	}
	// Response side.
	if response.Header.Get("X-Frame-Options") != "" || response.Header.Get("Content-Security-Policy-Report-Only") != "" || response.Header.Get("Report-To") != "" {
		t.Fatalf("frame or report headers kept: %v", response.Header)
	}
	policies := response.Header.Values("Content-Security-Policy")
	if len(policies) != 2 {
		t.Fatalf("policies: %q", policies)
	}
	daemonPolicy := webfront.ParsePolicy(policies[0])
	if ancestors, _ := daemonPolicy.Get("frame-ancestors"); strings.Join(ancestors, " ") != "'self' http://localhost:17777" {
		t.Fatalf("daemon frame-ancestors: %q", policies[0])
	}
	if script, _ := daemonPolicy.Get("script-src"); !strings.Contains(strings.Join(script, " "), "'nonce-") {
		t.Fatalf("daemon nonce lost: %q", policies[0])
	}
	if strings.Contains(policies[0], "report-uri") {
		t.Fatalf("remote report endpoint kept: %q", policies[0])
	}
	lockdown := webfront.ParsePolicy(policies[1])
	for directive, want := range map[string]string{
		"default-src": "'self'", "connect-src": "'self' ws://" + env.front.host + " blob: data:", "img-src": "'self' data: blob:",
		"frame-src": "'self'", "form-action": "'self'", "object-src": "'none'", "frame-ancestors": "'self' http://localhost:17777", "report-uri": "/_cage/csp-report",
	} {
		if got, _ := lockdown.Get(directive); strings.Join(got, " ") != want {
			t.Fatalf("lock-down %s = %q", directive, got)
		}
	}
	for _, name := range []string{"X-DNS-Prefetch-Control", "Referrer-Policy", "Cross-Origin-Resource-Policy", "Permissions-Policy"} {
		if response.Header.Get(name) == "" {
			t.Fatalf("%s missing", name)
		}
	}
	// Set-Cookie: only operon_csrf survives.
	r := env.do(t, client, "GET", "/api/auth/status", "", "", nil)
	cookies := r.Header.Values("Set-Cookie")
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0], "operon_csrf=csrf-status") || strings.Contains(strings.Join(cookies, ";"), "operon_session") {
		t.Fatalf("Set-Cookie: %q", cookies)
	}
	// Redirects: the daemon's own origin becomes the front's; others fail.
	r = env.do(t, client, "GET", "/redirect-internal", "", "", nil)
	if r.StatusCode != 302 || r.Header.Get("Location") != env.front.origin+"/next?x=1" {
		t.Fatalf("internal redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r = env.do(t, client, "GET", "/redirect-external", "", "", nil); r.StatusCode != 502 || r.Header.Get("Location") != "" {
		t.Fatalf("external redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// Cache headers of hashed assets are kept.
	env.daemon.assets["/assets/app-1a2b.js"] = "console.log(1)"
	if r = env.do(t, client, "GET", "/assets/app-1a2b.js", "", "", nil); !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset cache: %q", r.Header.Get("Cache-Control"))
	}
	// A request body with a foreign encoding cannot be audited.
	if r = env.do(t, client, "POST", "/api/echo", env.front.origin, "x", map[string]string{"Content-Encoding": "gzip"}); r.StatusCode != 415 {
		t.Fatalf("encoded body: %d", r.StatusCode)
	}
}

func TestScienceIntercepts(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	before := len(env.daemon.seen())
	if r := env.do(t, client, "GET", "/api/auth/login?provider=claude_ai", "", "", map[string]string{"Sec-Fetch-Mode": "navigate", "Accept": "text/html"}); r.StatusCode != 204 {
		t.Fatalf("login navigation: %d", r.StatusCode)
	}
	if r := env.do(t, client, "GET", "/api/auth/login?provider=claude_ai&mode=paste", "", "", map[string]string{"Accept": "application/json"}); r.StatusCode != 409 || !strings.Contains(readBody(t, r), "隔离浏览器") {
		t.Fatalf("login XHR: %d", r.StatusCode)
	}
	for path, want := range map[string]int{
		"/oauth/code/callback?code=x&state=y": 404, "/api/auth/callback": 404, "/api/auth/exchange": 404, "/api/auth/broker-exchange": 404,
		"/daemon/shutdown": 403, "/daemon/apply-update": 403, "/daemon/check-update": 403, "/api/mcp-servers/pubmed/oauth/authorize": 409,
	} {
		method := "GET"
		if !strings.Contains(path, "callback") && !strings.HasPrefix(path, "/api/mcp") {
			method = "POST"
		}
		if r := env.do(t, client, method, path, env.front.origin, "", nil); r.StatusCode != want {
			t.Fatalf("%s %s: %d", method, path, r.StatusCode)
		}
	}
	if after := len(env.daemon.seen()); after != before {
		t.Fatalf("intercepted routes reached the daemon: %d requests", after-before)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		env.mu.Lock()
		signins := env.signins
		env.mu.Unlock()
		state := env.front.snapshot()
		if signins == 1 && state.SignIn != nil && state.SignIn.Tab != nil && state.SignIn.Tab.Kind == "science" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-in not handed to the guest browser once: %d %+v", signins, state.SignIn)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScienceBearerLifecycle(t *testing.T) {
	// Everything the process logs while the bearer is in use.
	var logged strings.Builder
	var logMu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logged.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	var bodies []string
	for i := 0; i < 3; i++ {
		r := env.do(t, client, "GET", "/api/auth/status", "", "", nil)
		if r.StatusCode != 200 {
			t.Fatalf("status: %d", r.StatusCode)
		}
		bodies = append(bodies, fmt.Sprint(r.Header)+readBody(t, r))
	}
	if env.daemon.exchange != 1 {
		t.Fatalf("bearer minted %d times for one session", env.daemon.exchange)
	}
	first := env.daemon.bearers()[0]
	// The daemon restarts: the next request gets a 401, the front mints once
	// more and replays the request (the body too).
	env.daemon.rotate()
	r := env.do(t, client, "POST", "/api/echo", env.front.origin, `{"x":1}`, map[string]string{"Content-Type": "application/json"})
	if r.StatusCode != 200 || readBody(t, r) != `{"x":1}` || env.daemon.exchange != 2 {
		t.Fatalf("re-mint on 401: %d exchanges %d", r.StatusCode, env.daemon.exchange)
	}
	second := env.daemon.bearers()[0]
	if second == first {
		t.Fatal("bearer not replaced")
	}
	// The bearer never reaches the page: not in headers, bodies or cookies.
	r = env.do(t, client, "GET", "/", "", "", nil)
	bodies = append(bodies, fmt.Sprint(r.Header)+readBody(t, r))
	for _, text := range bodies {
		if strings.Contains(text, first) || strings.Contains(text, second) {
			t.Fatal("bearer leaked to the page")
		}
	}
	for _, c := range client.Jar.Cookies(mustURL(t, env.front.origin+"/")) {
		if c.Value == first || c.Value == second || c.Name == "operon_auth" {
			t.Fatal("bearer cookie reached the page")
		}
	}
	// reset (stop, lock, environment stop, VM change) forgets it.
	env.front.reset("")
	env.front.mu.Lock()
	upstream := env.front.upstream
	env.front.mu.Unlock()
	if upstream != nil {
		t.Fatal("upstream kept after reset")
	}
	if r = env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode != 401 {
		t.Fatalf("session after reset: %d", r.StatusCode)
	}
	// Nor in the process log or the status the workbench serves.
	status, _ := json.Marshal(env.front.snapshot())
	logMu.Lock()
	text := logged.String()
	logMu.Unlock()
	for _, secret := range []string{first, second} {
		if strings.Contains(text, secret) || strings.Contains(string(status), secret) {
			t.Fatal("bearer in the log or the status")
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestScienceAuditRefusesHostValues(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	host := testHostIdentity()
	east := -*host.Offset
	sign := "+"
	if east < 0 {
		sign, east = "-", -east
	}
	offset := fmt.Sprintf("%s%02d%02d", sign, east/60, east%60)
	isoOffset := fmt.Sprintf("%s%02d:%02d", sign, east/60, east%60)
	refused := []struct {
		name, path, body string
		headers          map[string]string
	}{
		{"body zone", "/api/echo", `{"timezone":"America/Los_Angeles"}`, nil},
		{"escaped zone", "/api/echo", `{"tz":"America\/Los_Angeles"}`, nil},
		{"header zone", "/api/echo", "{}", map[string]string{"X-Operon-Timezone": "America/Los_Angeles"}},
		{"query zone", "/api/echo?tz=America%2FLos_Angeles", "{}", nil},
		{"date string", "/api/echo", `{"d":"Sat Nov 21 2026 08:00:00 GMT` + offset + ` (Pacific Standard Time)"}`, nil},
		{"iso offset", "/api/echo", `{"d":"2026-11-21T08:00:00` + isoOffset + `"}`, nil},
		{"offset field", "/api/echo", fmt.Sprintf(`{"timezoneOffset": %d}`, *host.Offset), nil},
		{"locale", "/api/echo", `{"locale":"fr-FR"}`, nil},
	}
	before := len(env.daemon.seen())
	for _, tc := range refused {
		headers := map[string]string{"Content-Type": "application/json"}
		for k, v := range tc.headers {
			headers[k] = v
		}
		r := env.do(t, client, "POST", tc.path, env.front.origin, tc.body, headers)
		if r.StatusCode != 403 || !strings.Contains(readBody(t, r), "已拦截") {
			t.Fatalf("%s passed the audit: %d", tc.name, r.StatusCode)
		}
	}
	if after := len(env.daemon.seen()); after != before {
		t.Fatalf("refused requests reached the daemon: %d", after-before)
	}
	state := env.front.snapshot()
	if state.BlockedCount != len(refused) || !state.Audit.Known || !state.Audit.Timezone || !state.Audit.Offset || !state.Audit.Locale {
		t.Fatalf("audit state: %+v", state)
	}
	// The guest's own values pass.
	if r := env.do(t, client, "POST", "/api/echo", env.front.origin, `{"timezone":"Asia/Tokyo","locale":"ja-JP","d":"2026-11-21T08:00:00+09:00"}`, map[string]string{"Content-Type": "application/json"}); r.StatusCode != 200 {
		t.Fatalf("guest values refused: %d", r.StatusCode)
	}
	// A WebSocket text frame with the host zone closes the socket before
	// the daemon sees it; ordinary frames echo.
	conn := env.dialSocket(t, client)
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "hello" {
		t.Fatalf("echo: %q %v", data, err)
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"tz":"America/Los_Angeles"}`))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("socket stayed open after a host value")
	}
	env.daemon.mu.Lock()
	frames := append([]string(nil), env.daemon.frames...)
	env.daemon.mu.Unlock()
	for _, frame := range frames {
		if strings.Contains(frame, "Los_Angeles") {
			t.Fatal("host zone reached the daemon over the socket")
		}
	}
	if state := env.front.snapshot(); state.BlockedCount != len(refused)+1 || state.Blocked != "timezone" {
		t.Fatalf("socket refusal not recorded: %+v", state)
	}
}

func TestScienceAuditWithEqualValues(t *testing.T) {
	offset, _ := jsOffset(testScienceIdentity.Timezone, time.Now())
	audit := newScienceAudit(scienceHostIdentity{Timezone: "Asia/Tokyo", Offset: &offset, Languages: []string{"ja-JP"}}, testScienceIdentity, time.Now())
	if !audit.Distinct.Known || audit.Distinct.Timezone || audit.Distinct.Offset || audit.Distinct.Locale {
		t.Fatalf("%+v", audit.Distinct)
	}
	if audit.scan([]byte(`{"tz":"Asia/Tokyo","d":"GMT+0900","l":"ja-JP"}`)) != "" {
		t.Fatal("guest values flagged")
	}
	if (newScienceAudit(scienceHostIdentity{}, testScienceIdentity, time.Now())).Distinct.Known {
		t.Fatal("unmeasured host reported as known")
	}
	// Narrow patterns: numbers and words that merely look alike pass.
	host := testHostIdentity()
	wide := newScienceAudit(host, testScienceIdentity, time.Now())
	for _, text := range []string{`{"count": 480, "width": 420}`, "french fries", "fr", "the fr-FRANCE notation", `"America"`} {
		if category := wide.scan([]byte(text)); category != "" {
			t.Fatalf("false positive %q: %s", text, category)
		}
	}
}

func TestScienceWebSocketAndEventStream(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	conn := env.dialSocket(t, client)
	for i := 0; i < 3; i++ {
		message := fmt.Sprintf("frame-%d-%s", i, strings.Repeat("x", 70000*i))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			t.Fatal(err)
		}
		if _, data, err := conn.ReadMessage(); err != nil || string(data) != message {
			t.Fatalf("echo %d: %v", i, err)
		}
	}
	seen := env.daemon.seen()
	for _, request := range seen {
		if request.Path == "/api/ws" && request.Header.Get("Sec-Websocket-Extensions") != "" {
			t.Fatal("compression negotiated: frames could not be audited")
		}
	}
	// An upgrade from a foreign origin is refused.
	dialer := websocket.Dialer{NetDialContext: env.dial}
	header := http.Header{"Origin": {"http://localhost:17777"}}
	if _, response, err := dialer.Dial("ws://"+env.front.host+"/api/ws", header); err == nil || response == nil || response.StatusCode != 403 {
		t.Fatal("cross-origin upgrade accepted")
	}
	r := env.do(t, client, "GET", "/api/events", "", "", nil)
	body := readBody(t, r)
	if r.Header.Get("Content-Type") != "text/event-stream" || strings.Count(body, "data: tick-") != 3 {
		t.Fatalf("event stream: %q %q", r.Header.Get("Content-Type"), body)
	}
}

func (env *testScienceFront) dial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp4", fmt.Sprintf("127.0.0.1:%d", env.front.port))
}

func (env *testScienceFront) dialSocket(t *testing.T, client *http.Client) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{NetDialContext: env.dial, Jar: client.Jar, EnableCompression: true}
	conn, response, err := dialer.Dial("ws://"+env.front.host+"/api/ws", http.Header{"Origin": {env.front.origin}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("socket: %v %d", err, status)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestScienceCSPMerge(t *testing.T) {
	policy := webfront.ParsePolicy(fmt.Sprintf(fakeScienceSPAPolicy, "abc") + "; report-uri https://x.example/r; frame-ancestors 'self'")
	if got, _ := policy.Get("frame-ancestors"); strings.Join(got, " ") != "'none'" {
		t.Fatalf("first directive must win: %q", got)
	}
	merged := policy.Without("report-uri", "report-to").Set("frame-ancestors", "http://localhost:17777")
	text := merged.String()
	if strings.Contains(text, "report-uri") || !strings.Contains(text, "frame-ancestors http://localhost:17777") || !strings.Contains(text, "'nonce-abc'") {
		t.Fatalf("%s", text)
	}
	if n := len(webfront.ParsePolicies([]string{"default-src 'self', img-src 'none'", "script-src 'none'"})); n != 3 {
		t.Fatalf("policies: %d", n)
	}
}

func TestScienceAcceptLanguageMatchesChromium(t *testing.T) {
	// Observed: Chromium --accept-lang=ja-JP sends "ja-JP,ja;q=0.9" and keeps
	// navigator.languages ["ja-JP"].
	for in, want := range map[string]string{"ja-JP": "ja-JP,ja;q=0.9", "en-US": "en-US,en;q=0.9", "zh-CN": "zh-CN,zh;q=0.9", "de": "de"} {
		if got := scienceAcceptLanguage([]string{in}); got != want {
			t.Fatalf("%s: %q", in, got)
		}
	}
}
