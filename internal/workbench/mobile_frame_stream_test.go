package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

// frameGuest plays the guest side of both phone surfaces. Its picture changes
// only when a test bumps the version, so a held read has something to wait for.
type frameGuest struct {
	mu            sync.Mutex
	version       int
	url, title    string
	browserFrames atomic.Int32
	desktopFrames atomic.Int32
	taps          atomic.Int32
}

func (g *frameGuest) bump() {
	g.mu.Lock()
	g.version++
	g.mu.Unlock()
}

func (g *frameGuest) current() (int, string, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.version, g.url, g.title
}

func frameJPEG(version int) []byte {
	return append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte(fmt.Sprintf("picture %d %s", version, strings.Repeat("~", 3000)))...)
}

func frameGuestTag(version int) string {
	sum := sha256.Sum256([]byte(fmt.Sprint("guest-tag-", version)))
	return hex.EncodeToString(sum[:16])
}

func jsonString(value string) string {
	out, _ := json.Marshal(value)
	return string(out)
}

func (g *frameGuest) browserBody(id string) string {
	version, url, title := g.current()
	return fmt.Sprintf(`{"id":%q,"url":%s,"title":%s,"width":390,"height":640,"document":"doc","focus":"","password":false,"frame_tag":%q,"image":%q}`,
		id, jsonString(url), jsonString(title), frameGuestTag(version), base64.StdEncoding.EncodeToString(frameJPEG(version)))
}

func (g *frameGuest) desktopBody(id string) string {
	version, _, _ := g.current()
	return fmt.Sprintf(`{"id":%q,"document":"test","window":"desktop","image":%q,"width":390,"height":640,"focus":"","password":false,"title":"Claude Desktop","shared":true}`,
		id, base64.StdEncoding.EncodeToString(frameJPEG(version)))
}

func (g *frameGuest) transport(t *testing.T, id string) browserTransport {
	return func(r *http.Request) (*http.Response, error) {
		var input phoneSurfaceInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		body := fmt.Sprintf(`{"id":%q}`, id)
		switch r.URL.Path {
		case "/phone-browser/frame":
			g.browserFrames.Add(1)
			version, _, _ := g.current()
			if input.After == frameGuestTag(version) {
				body = fmt.Sprintf(`{"id":%q,"frame_tag":%q,"unchanged":true}`, id, input.After)
			} else {
				body = g.browserBody(id)
			}
		case "/phone-desktop/frame":
			g.desktopFrames.Add(1)
			if input.After != "" {
				t.Error("the host must not hand its own frame tag to a desktop guest")
			}
			body = g.desktopBody(id)
		case "/phone-browser/action":
			g.taps.Add(1)
		case "/phone-browser/open", "/phone-browser/close", "/phone-desktop/open", "/phone-desktop/close", "/phone-desktop/action":
		default:
			t.Errorf("unexpected guest route %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func eventually(t *testing.T, what string, limit time.Duration, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(limit); !check(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

type frameMeta map[string]any

func decodeFrameMeta(t *testing.T, header string) frameMeta {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		t.Fatal("frame metadata is not base64url:", err)
	}
	var meta frameMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// Drives the negotiated frame reads through the real relay, TLS peer and
// HTTP/2 link of the pairing test, against a guest whose picture the test controls.
func testPairedFrameStream(t *testing.T, ctx context.Context, s *Server, phone *mobilelink.Client) {
	t.Helper()
	guest := &frameGuest{url: "https://example.com/文件?q=é", title: "标题：远程画面"}
	id := uuid.NewString()
	var shown atomic.Bool
	s.desktopDisplayUpdate = func(show bool) error { shown.Store(show); return nil }
	s.browserControl = browserClientForTest(t, s, guest.transport(t, id))
	browser := &s.phones.browser
	proof := liveProof(t, s)

	call := func(callCtx context.Context, method, surface, path, body string) (*mobilelink.Reply, error) {
		var payload []byte
		if body != "" {
			payload = []byte(body)
		}
		return phone.Do(callCtx, method, "/api/mobile/"+surface+"/"+path, payload)
	}
	must := func(method, surface, path, body string, want int) *mobilelink.Reply {
		t.Helper()
		reply, err := call(ctx, method, surface, path, body)
		if err != nil {
			t.Fatal(err)
		}
		if reply.Status != want {
			t.Fatalf("%s %s: HTTP %d, want %d (%s)", surface, path, reply.Status, want, reply.Body)
		}
		return reply
	}
	open := fmt.Sprintf(`{"id":%q,"width":390,"height":640,"url":"https://example.com/"}`, id)
	must("POST", "browser", "open", open, 200)
	must("POST", "desktop", "open", strings.Replace(open, `,"url":"https://example.com/"`, "", 1), 200)
	frame := func(surface, query string) string { return id + "/frame" + query }

	// A phone that sends nothing new gets the replies it always got, byte for
	// byte: the guest object plus the one annotation the host always added.
	legacy := must("GET", "browser", frame("browser", ""), "", 200)
	body := guest.browserBody(id)
	if string(legacy.Body) != body[:len(body)-1]+`,"action_frame":true}` || legacy.ContentType != "application/json" || legacy.Header.Get(mobilelink.FrameMetaHeader) != "" {
		t.Fatalf("legacy browser frame changed: %s", legacy.Body)
	}
	if legacy.Header.Get(mobilelink.FrameStreamHeader) != "1" {
		t.Fatal("host did not announce negotiated reads")
	}
	tag := frameGuestTag(0)
	unchanged := must("GET", "browser", frame("browser", "?after="+tag), "", 200)
	if want := fmt.Sprintf(`{"id":%q,"frame_tag":%q,"unchanged":true,"action_frame":true}`, id, tag); string(unchanged.Body) != want {
		t.Fatalf("legacy conditional read changed: %s", unchanged.Body)
	}
	if reply := must("GET", "desktop", frame("desktop", ""), "", 200); string(reply.Body) != guest.desktopBody(id) {
		t.Fatalf("legacy desktop frame changed: %s", reply.Body)
	}
	// Queries that were never valid stay refused by the host itself, whatever the link lets through.
	refused := func(path string) int {
		r := httptest.NewRequest("GET", s.origin+path, nil)
		r.Header.Set("Origin", s.origin)
		r.Header.Set(mobilelink.PhoneRoomHeader, proof)
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w.Code
	}
	for _, path := range []string{"/api/mobile/desktop/" + id + "/frame?after=" + tag, "/api/mobile/browser/" + id + "/frame?wait=500",
		"/api/mobile/browser/" + id + "/frame?after=" + tag + "&wait=0", "/api/mobile/browser/" + id + "/frame?fmt=png"} {
		if code := refused(path); code != 400 {
			t.Fatalf("%s: HTTP %d, want 400", path, code)
		}
	}

	// The same frame as a JPEG body with its metadata in one ASCII header.
	binary := must("GET", "browser", frame("browser", "?fmt=jpeg"), "", 200)
	if binary.ContentType != "image/jpeg" || string(binary.Body) != string(frameJPEG(0)) {
		t.Fatalf("binary frame: %s, %d bytes", binary.ContentType, len(binary.Body))
	}
	meta := decodeFrameMeta(t, binary.Header.Get(mobilelink.FrameMetaHeader))
	if meta["url"] != guest.url || meta["title"] != guest.title || meta["frame_tag"] != tag || meta["document"] != "doc" ||
		meta["width"] != float64(390) || meta["image"] != nil || meta["frame_seq"] == nil {
		t.Fatalf("frame metadata lost on the way: %v", meta)
	}
	if len(binary.Body) >= len(legacy.Body)*3/4 {
		t.Fatal("the binary frame is not smaller than the base64 one")
	}
	// A picture the header cannot describe is still delivered, as the original JSON.
	guest.mu.Lock()
	guest.title = strings.Repeat("标", 3000)
	guest.mu.Unlock()
	if big := must("GET", "browser", frame("browser", "?fmt=jpeg"), "", 200); big.ContentType != "application/json" || !strings.Contains(string(big.Body), `"image"`) {
		t.Fatalf("oversized metadata was not answered as JSON: %s", big.ContentType)
	}
	guest.mu.Lock()
	guest.title = "标题：远程画面"
	guest.mu.Unlock()

	// A read for the tag the phone already shows is held until the picture changes.
	held := func(callCtx context.Context, surface, query string) <-chan *mobilelink.Reply {
		out := make(chan *mobilelink.Reply, 1)
		go func() {
			reply, err := call(callCtx, "GET", surface, frame(surface, query), "")
			if err != nil {
				reply = nil
			}
			out <- reply
		}()
		return out
	}
	frames := guest.browserFrames.Load()
	started := time.Now()
	wait := held(ctx, "browser", "?after="+tag+"&wait=8000&fmt=jpeg")
	eventually(t, "a held read to keep looking at the guest", 3*time.Second, func() bool { return guest.browserFrames.Load() >= frames+3 })
	select {
	case reply := <-wait:
		t.Fatalf("an unchanged picture ended the read: %+v", reply)
	case <-time.After(350 * time.Millisecond):
	}
	// Input is not queued behind the held read.
	tapped := time.Now()
	must("POST", "browser", id+"/action", `{"type":"tap","x":1,"y":1,"document":"doc"}`, 200)
	if took := time.Since(tapped); took > 800*time.Millisecond {
		t.Fatalf("a tap waited %v behind a held read", took)
	}
	guest.bump()
	select {
	case reply := <-wait:
		if reply == nil || reply.Status != 200 || reply.ContentType != "image/jpeg" || string(reply.Body) != string(frameJPEG(1)) {
			t.Fatalf("held read answered with %+v", reply)
		}
		next := decodeFrameMeta(t, reply.Header.Get(mobilelink.FrameMetaHeader))
		if next["frame_tag"] != frameGuestTag(1) || next["frame_seq"].(float64) <= meta["frame_seq"].(float64) {
			t.Fatalf("held read did not move forward: %v", next)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held read missed the new picture")
	}
	if took := time.Since(started); took < 350*time.Millisecond || took > 4*time.Second {
		t.Fatalf("held read took %v", took)
	}

	// With nothing new the read ends at its bound with the original unchanged shape.
	tag = frameGuestTag(1)
	started = time.Now()
	quiet := must("GET", "browser", frame("browser", "?after="+tag+"&wait=400&fmt=jpeg"), "", 200)
	if want := fmt.Sprintf(`{"frame_tag":%q,"id":%q,"unchanged":true}`, tag, id); string(quiet.Body) != want || quiet.ContentType != "application/json" {
		t.Fatalf("quiet read: %s", quiet.Body)
	}
	if took := time.Since(started); took < 350*time.Millisecond || took > 2*time.Second {
		t.Fatalf("quiet read took %v", took)
	}
	// Without wait the answer is immediate, as the conditional read always was.
	started = time.Now()
	must("GET", "browser", frame("browser", "?after="+tag+"&fmt=jpeg"), "", 200)
	if time.Since(started) > 400*time.Millisecond {
		t.Fatal("an unheld read waited")
	}

	// A newer held read replaces the older one, so a phone never has two.
	first := held(ctx, "browser", "?after="+tag+"&wait=8000&fmt=jpeg")
	eventually(t, "the first held read", 2*time.Second, func() bool { browser.hold.mu.Lock(); defer browser.hold.mu.Unlock(); return browser.hold.cur != nil })
	second := held(ctx, "browser", "?after="+tag+"&wait=8000&fmt=jpeg")
	select {
	case reply := <-first:
		if reply == nil || reply.Status != 200 || !strings.Contains(string(reply.Body), `"unchanged":true`) {
			t.Fatalf("superseded read answered with %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the older held read was not released")
	}
	select {
	case reply := <-second:
		t.Fatalf("the newer read ended too: %+v", reply)
	case <-time.After(200 * time.Millisecond):
	}
	guest.bump()
	if reply := <-second; reply == nil || reply.Status != 200 || string(reply.Body) != string(frameJPEG(2)) {
		t.Fatalf("newer read answered with %+v", reply)
	}
	browser.hold.mu.Lock()
	leaked := browser.hold.cur != nil
	browser.hold.mu.Unlock()
	if leaked {
		t.Fatal("a finished read still counts as held")
	}

	// A phone that goes away releases the wait and stops the looking.
	tag = frameGuestTag(2)
	gone, leave := context.WithCancel(ctx)
	abandoned := held(gone, "browser", "?after="+tag+"&wait=8000&fmt=jpeg")
	eventually(t, "the abandoned read", 2*time.Second, func() bool { browser.hold.mu.Lock(); defer browser.hold.mu.Unlock(); return browser.hold.cur != nil })
	leave()
	if reply := <-abandoned; reply != nil {
		t.Fatalf("a cancelled read produced %+v", reply)
	}
	eventually(t, "the host to release the abandoned read", 2*time.Second, func() bool {
		browser.hold.mu.Lock()
		defer browser.hold.mu.Unlock()
		return browser.hold.cur == nil
	})
	settled := guest.browserFrames.Load()
	time.Sleep(450 * time.Millisecond)
	if guest.browserFrames.Load() != settled {
		t.Fatal("the host kept polling the guest for a phone that left")
	}

	// The desktop guest has no tags: the host names its frames and compares them.
	first2 := must("GET", "desktop", frame("desktop", "?fmt=jpeg"), "", 200)
	desktopMeta := decodeFrameMeta(t, first2.Header.Get(mobilelink.FrameMetaHeader))
	desktopTag, _ := desktopMeta["frame_tag"].(string)
	if first2.ContentType != "image/jpeg" || string(first2.Body) != string(frameJPEG(2)) || len(desktopTag) != 32 || desktopMeta["window"] != "desktop" {
		t.Fatalf("desktop frame: %s %v", first2.ContentType, desktopMeta)
	}
	started = time.Now()
	still := must("GET", "desktop", frame("desktop", "?after="+desktopTag+"&wait=500&fmt=jpeg"), "", 200)
	if !strings.Contains(string(still.Body), `"unchanged":true`) || time.Since(started) < 450*time.Millisecond {
		t.Fatalf("unchanged desktop read: %s after %v", still.Body, time.Since(started))
	}
	moving := held(ctx, "desktop", "?after="+desktopTag+"&wait=8000&fmt=jpeg")
	time.Sleep(250 * time.Millisecond)
	guest.bump()
	reply := <-moving
	if reply == nil || reply.Status != 200 || string(reply.Body) != string(frameJPEG(3)) ||
		decodeFrameMeta(t, reply.Header.Get(mobilelink.FrameMetaHeader))["frame_tag"] == desktopTag {
		t.Fatalf("held desktop read answered with %+v", reply)
	}

	// Looking again and again does not ask the gate again and again. Revoking it
	// stops the next picture; a read that carries no picture may still be
	// answered from the grant seen less than a TTL ago.
	var lookups atomic.Int32
	var revoked atomic.Bool
	setNetwork := func(ttl time.Duration, fetch func(*Server, context.Context) (string, cage.GateStatus, error)) {
		browser.network.mu.Lock()
		browser.network.ttl, browser.network.fetch, browser.network.at = ttl, fetch, time.Time{}
		browser.network.mu.Unlock()
	}
	setNetwork(time.Hour, func(s *Server, ctx context.Context) (string, cage.GateStatus, error) {
		lookups.Add(1)
		fingerprint, gate, err := s.phoneGateLookup(ctx)
		gate.Fault = gate.Fault || revoked.Load()
		return fingerprint, gate, err
	})
	defer setNetwork(0, nil)
	tag = frameGuestTag(3)
	before := guest.browserFrames.Load()
	must("GET", "browser", frame("browser", "?after="+tag+"&wait=1500&fmt=jpeg"), "", 200)
	if looked := guest.browserFrames.Load() - before; looked < 2 || lookups.Load() > 1 {
		t.Fatalf("%d looks at the guest needed %d gate lookups", looked, lookups.Load())
	}
	revoked.Store(true)
	if reply := must("GET", "browser", frame("browser", "?after="+tag+"&fmt=jpeg"), "", 200); !strings.Contains(string(reply.Body), `"unchanged":true`) {
		t.Fatalf("a read without a picture was refused: %s", reply.Body)
	}
	guest.bump()
	must("GET", "browser", frame("browser", "?after="+tag+"&fmt=jpeg"), "", 409)
	must("GET", "browser", frame("browser", "?after="+tag+"&fmt=jpeg"), "", 410)

	// Locking the account, or ending the link, cancels the session: a held read ends at once.
	latest := must("GET", "desktop", frame("desktop", "?fmt=jpeg"), "", 200)
	latestTag, _ := decodeFrameMeta(t, latest.Header.Get(mobilelink.FrameMetaHeader))["frame_tag"].(string)
	locked := held(ctx, "desktop", "?after="+latestTag+"&wait=8000&fmt=jpeg")
	desktop := &s.phones.desktop
	eventually(t, "the desktop read to be held", 2*time.Second, func() bool { desktop.hold.mu.Lock(); defer desktop.hold.mu.Unlock(); return desktop.hold.cur != nil })
	desktop.op.Lock()
	session := desktop.session
	desktop.op.Unlock()
	session.cancel()
	select {
	case reply := <-locked:
		if reply == nil || reply.Status == 200 {
			t.Fatalf("a held read survived the end of its session: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a held read outlived its session")
	}
	eventually(t, "the desktop view to be released", 3*time.Second, func() bool { return !shown.Load() })
}

func TestPhoneFrameTagIgnoresKeyOrderAndSpacing(t *testing.T) {
	a := `{"id":"x","document":"d","image":"AAAA","width":390,"content_width":390.5,"focus":"","window":"desktop"}`
	b := `{ "window" : "desktop", "content_width":390.5, "image":"AAAA", "focus":"", "width":390,"document":"d", "id":"x" }`
	first, ok := parsePhoneFrame([]byte(a), "")
	second, ok2 := parsePhoneFrame([]byte(b), "")
	if !ok || !ok2 || first.tag != second.tag || !phoneFrameTag.MatchString(first.tag) {
		t.Fatalf("same frame, different serialization: %q vs %q", first.tag, second.tag)
	}
	for _, changed := range []string{
		strings.Replace(a, `"AAAA"`, `"AAAB"`, 1), strings.Replace(a, `"focus":""`, `"focus":"f"`, 1), strings.Replace(a, `"width":390`, `"width":391`, 1),
	} {
		other, _ := parsePhoneFrame([]byte(changed), "")
		if other.tag == first.tag {
			t.Fatalf("a different frame kept its name: %s", changed)
		}
	}
	// A guest tag wins over the digest, and an unchanged answer keeps the tag it answers.
	const guestTag = "b542cd94286a2489bc2df1241b043c76"
	if named, _ := parsePhoneFrame([]byte(`{"id":"x","frame_tag":"`+guestTag+`","image":"AAAA"}`), ""); named.tag != guestTag {
		t.Fatal("the guest's own tag was replaced")
	}
	if kept, _ := parsePhoneFrame([]byte(`{"id":"x","unchanged":true}`), guestTag); !kept.unchanged || kept.tag != guestTag {
		t.Fatal("an unchanged answer lost its tag")
	}
}
