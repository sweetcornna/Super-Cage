package workbench

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Bounds on what the frame's page can make the front do (review 3: F7, F9,
// F10, F12 and the idle watchdog).

// F12: the sign-in, shutdown, update and connector intercepts hold with a
// trailing slash.
func TestScienceInterceptsIgnoreTrailingSlashes(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	before := len(env.daemon.seen())
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/auth/login/", http.StatusNoContent},
		{"POST", "/daemon/shutdown/", http.StatusForbidden},
		{"POST", "/daemon/apply-update/", http.StatusForbidden},
		{"GET", "/oauth/code/callback/", http.StatusNotFound},
		{"GET", "/api/mcp-servers/x/oauth/authorize/", http.StatusConflict},
	} {
		r := env.do(t, client, tc.method, tc.path, env.base(), "", map[string]string{"Sec-Fetch-Mode": "navigate"})
		if r.StatusCode != tc.code {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, r.StatusCode, tc.code)
		}
	}
	for _, request := range env.daemon.seen()[before:] {
		if request.Path != "/api/auth/nonce" { // the front's own credential exchange
			t.Errorf("reached the daemon: %s %s", request.Method, request.Path)
		}
	}
}

// F9: a request that passed the session check just before a reset (lock,
// stop, release) neither keeps nor mints a credential, and is not forwarded.
func TestScienceResetBeatsAnInFlightRequest(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	env.front.mu.Lock()
	original := env.front.hooks.upstream
	env.front.hooks.upstream = func() (*scienceUpstream, error) {
		// The reset lands between the request's session check and its upstream.
		env.front.reset("")
		return original()
	}
	env.front.mu.Unlock()
	before := len(env.daemon.seen())
	if r := env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode == http.StatusOK {
		t.Fatal("forwarded after the reset")
	}
	env.front.mu.Lock()
	upstream := env.front.upstream
	env.front.mu.Unlock()
	if upstream != nil {
		t.Fatal("an upstream outlived the reset")
	}
	for _, request := range env.daemon.seen()[before:] {
		t.Errorf("the daemon saw %s %s after the reset", request.Method, request.Path)
	}
}

// F9: a new entry ends the earlier session's generation as reset does: a
// request of the earlier session that is already past its session check
// gets no upstream.
func TestScienceReEntryBeatsAnInFlightRequest(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	var entered atomic.Int32
	env.front.mu.Lock()
	original := env.front.hooks.upstream
	env.front.hooks.upstream = func() (*scienceUpstream, error) {
		if entered.Add(1) == 1 {
			// Another document enters between the request's session check
			// and its upstream.
			entry, _ := url.Parse(env.front.issue(scienceTicket{epoch: env.epoch, identity: testScienceIdentity, host: testHostIdentity()}))
			label := scienceLabelHost.FindStringSubmatch(entry.Host)[1]
			request := httptest.NewRequest(http.MethodPost, "/_cage/enter", strings.NewReader("{}"))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			env.front.enter(recorder, request, label)
			if recorder.Code != http.StatusNoContent {
				t.Errorf("second entry: %d", recorder.Code)
			}
		}
		return original()
	}
	env.front.mu.Unlock()
	before := len(env.daemon.seen())
	if r := env.do(t, client, "GET", "/api/auth/status", "", "", nil); r.StatusCode == http.StatusOK {
		t.Fatal("forwarded after another entry")
	}
	for _, request := range env.daemon.seen()[before:] {
		t.Errorf("the daemon saw %s %s after another entry", request.Method, request.Path)
	}
	env.front.mu.Lock()
	upstream := env.front.upstream
	env.front.mu.Unlock()
	if upstream != nil {
		t.Fatal("the earlier session's request made an upstream")
	}
}

// F10: a daemon that refuses every credential costs a few exchanges, not one
// per request, and then the session ends with a reason.
func TestScienceRefusedCredentialEndsTheSession(t *testing.T) {
	previous := scienceMintBackoff
	scienceMintBackoff = 20 * time.Millisecond
	t.Cleanup(func() { scienceMintBackoff = previous })
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	env.daemon.mu.Lock()
	env.daemon.refuseAll = true
	env.daemon.mu.Unlock()
	var codes []int
	for range 12 {
		r := env.do(t, client, "GET", "/api/projects", "", "", nil)
		codes = append(codes, r.StatusCode)
		time.Sleep(30 * time.Millisecond)
	}
	env.daemon.mu.Lock()
	exchanges := env.daemon.exchange
	env.daemon.mu.Unlock()
	state := env.front.snapshot()
	t.Logf("codes %v, %d exchanges, state %+v", codes, exchanges, state)
	// The first credential plus scienceMintCap replacements.
	if exchanges > 1+scienceMintCap {
		t.Fatalf("%d exchanges for a daemon that refuses every credential", exchanges)
	}
	if state.Session || state.Refused == "" {
		t.Fatalf("the session outlived the refusals: %+v", state)
	}
}

// N5 (round 2) and Q11: a 401 to a good bearer is the app's own answer (a
// signed-out account): the page gets it, the session stays with a visible
// "需要登录", and nothing is minted, however often it alternates with
// successes.
func TestScienceSignedOutKeepsTheSession(t *testing.T) {
	previous := scienceMintBackoff
	scienceMintBackoff = time.Millisecond
	t.Cleanup(func() { scienceMintBackoff = previous })
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	env.daemon.mu.Lock()
	env.daemon.signedOut = true
	env.daemon.mu.Unlock()
	var codes []int
	for i := range 12 {
		path := "/api/projects"
		if i%2 == 1 {
			path = "/api/echo"
		}
		codes = append(codes, env.do(t, client, "GET", path, "", "", nil).StatusCode)
		time.Sleep(5 * time.Millisecond)
	}
	env.daemon.mu.Lock()
	exchanges := env.daemon.exchange
	env.daemon.mu.Unlock()
	state, _ := json.Marshal(env.front.snapshot())
	t.Logf("codes %v, %d exchanges, state %s", codes, exchanges, state)
	if exchanges != 1 {
		t.Errorf("%d credential exchanges for a signed-out account", exchanges)
	}
	if codes[0] != http.StatusUnauthorized || codes[1] != http.StatusOK {
		t.Errorf("the page saw %v", codes)
	}
	if !strings.Contains(string(state), `"session":true`) || !strings.Contains(string(state), `"signin_needed":true`) {
		t.Errorf("state %s", state)
	}
	// Signed in again: the next answer clears it.
	env.daemon.mu.Lock()
	env.daemon.signedOut = false
	env.daemon.mu.Unlock()
	if r := env.do(t, client, "GET", "/api/projects", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %d", r.StatusCode)
	}
	if state, _ := json.Marshal(env.front.snapshot()); strings.Contains(string(state), "signin_needed") {
		t.Errorf("still asking for a sign-in: %s", state)
	}
}

// When the bearer check learns nothing, the 401 goes to the page as it is:
// no credential is minted and no sign-in is asked for.
func TestScienceUncheckedUnauthorizedPassesThrough(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	env.daemon.mu.Lock()
	env.daemon.signedOut, env.daemon.statusFails = true, true
	env.daemon.mu.Unlock()
	for range 4 {
		if r := env.do(t, client, "GET", "/api/projects", "", "", nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("page saw %d", r.StatusCode)
		}
	}
	env.daemon.mu.Lock()
	exchanges := env.daemon.exchange
	env.daemon.mu.Unlock()
	state := env.front.snapshot()
	if exchanges != 1 || state.SignInNeeded || !state.Session {
		t.Fatalf("%d exchanges, %+v", exchanges, state)
	}
}

// The watchdog sleeps until a session exists, and again once it ends.
func TestScienceWatchdogSleepsWithoutASession(t *testing.T) {
	previous := scienceWatchInterval
	scienceWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { scienceWatchInterval = previous })
	s := scienceWorkload(t)
	s.preferences.ScienceHostRender = true
	s.syncScienceFront()
	front := s.scienceFrontRunning()
	if front == nil {
		t.Fatal("front not running")
	}
	start := scienceWatchWakes.Load()
	time.Sleep(200 * time.Millisecond)
	if woke := scienceWatchWakes.Load() - start; woke != 0 {
		t.Fatalf("the watchdog woke %d times without a session", woke)
	}
	liveScienceFront(t, s)
	waitFor(t, 2*time.Second, "the watchdog", func() bool { return scienceWatchWakes.Load() > start })
	front.reset("")
	time.Sleep(50 * time.Millisecond)
	settled := scienceWatchWakes.Load()
	time.Sleep(200 * time.Millisecond)
	if woke := scienceWatchWakes.Load() - settled; woke != 0 {
		t.Fatalf("the watchdog woke %d times after the session ended", woke)
	}
}

// F7: the socket audit streams. A 16 MiB frame passes through with a small
// fixed amount of memory (provisional figures: this Mac is loaded), the
// bytes unchanged, and a host value in it is reported, not refused.
func TestScienceSocketAuditStreams(t *testing.T) {
	audit := newScienceAudit(testHostIdentity(), testScienceIdentity, time.Now())
	size := 16 << 20
	if scienceRaceDetector {
		size = 2 << 20
	}
	payload := bytes.Repeat([]byte("a"), size)
	copy(payload[size/2-5:], "America/Los_Angeles")
	frame := maskedFrame(0x2, payload)
	var hits atomic.Int32
	var out countingWriter
	writer := newScienceSocketAudit(&out, audit, func(string) { hits.Add(1) })
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for chunk := frame; len(chunk) > 0; {
		n := min(len(chunk), 32<<10)
		if written, err := writer.Write(chunk[:n]); err != nil || written != n {
			t.Fatalf("write: %d %v", written, err)
		}
		chunk = chunk[n:]
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d MiB frame: %d KiB allocated by the audit (provisional)", size>>20, allocated>>10)
	if out.n != int64(len(frame)) || !bytes.Equal(out.head, frame[:len(out.head)]) {
		t.Fatalf("forwarded %d of %d bytes", out.n, len(frame))
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
	if allocated > 1<<20 && !scienceRaceDetector {
		t.Fatalf("the audit allocated %d KiB for one frame", allocated>>10)
	}
}

// F7: sockets per session are capped; closing one frees a place.
func TestScienceSocketsPerSessionAreCapped(t *testing.T) {
	env := newTestScienceFront(t)
	client, _ := env.enter(t, goodProbeReports(testScienceIdentity))
	dial := func() (*websocket.Conn, int) {
		dialer := websocket.Dialer{NetDialContext: env.dial}
		conn, response, err := dialer.Dial("ws://"+env.host()+"/api/ws", http.Header{"Origin": {env.base()}})
		if err != nil {
			if response != nil {
				return nil, response.StatusCode
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn, http.StatusSwitchingProtocols
	}
	_ = client
	var open []*websocket.Conn
	for range scienceSocketCap {
		conn, code := dial()
		if conn == nil {
			t.Fatalf("socket %d refused: %d", len(open)+1, code)
		}
		open = append(open, conn)
	}
	if conn, code := dial(); conn != nil || code != http.StatusTooManyRequests {
		t.Fatalf("socket over the cap: %d", code)
	}
	open[0].Close()
	waitFor(t, 5*time.Second, "a place", func() bool {
		conn, _ := dial()
		if conn != nil {
			conn.Close()
		}
		return conn != nil
	})
}

// F7: request bodies the front holds at once are capped per session.
func TestScienceBodiesPerSessionAreCapped(t *testing.T) {
	env := newTestScienceFront(t)
	env.enter(t, goodProbeReports(testScienceIdentity))
	stall := func() net.Conn {
		conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", env.front.port))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		fmt.Fprintf(conn, "POST /api/echo HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", env.host(), env.base(), scienceBodyLimit)
		chunk := make([]byte, 64<<10)
		_, _ = rand.Read(chunk)
		_, _ = conn.Write(chunk)
		return conn
	}
	for range scienceBodyBudget / scienceBodyLimit {
		stall()
	}
	waitFor(t, 5*time.Second, "the held bodies", func() bool {
		env.front.mu.Lock()
		defer env.front.mu.Unlock()
		return env.front.bodies >= scienceBodyBudget
	})
	r := env.do(t, env.client(t), "POST", "/api/echo", env.base(), strings.Repeat("x", 1024), map[string]string{"Content-Type": "application/json"})
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a body over the session's budget: %d", r.StatusCode)
	}
}

// maskedFrame is a client frame (RFC 6455, masked) with one payload.
func maskedFrame(opcode byte, payload []byte) []byte {
	header := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n < 1<<16:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	key := []byte{1, 2, 3, 4}
	frame := append(append(header, key...), payload...)
	for i := range payload {
		frame[len(header)+4+i] ^= key[i%4]
	}
	return frame
}

type countingWriter struct {
	n    int64
	head []byte
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if len(w.head) < 64 {
		w.head = append(w.head, p[:min(len(p), 64-len(w.head))]...)
	}
	w.n += int64(len(p))
	return len(p), nil
}

var _ io.Writer = (*countingWriter)(nil)
