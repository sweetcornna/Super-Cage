package herdrlink

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeEnd collects what the link writes to one side.
type pipeEnd struct {
	mu   sync.Mutex
	data []byte
}

func (p *pipeEnd) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = append(p.data, b...)
	return len(b), nil
}

func (p *pipeEnd) bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.data...)
}

// rig runs one link between a fake owner core and a fake guest core.
type rig struct {
	t       *testing.T
	ownerW  *io.PipeWriter
	guestW  *io.PipeWriter
	toOwner *pipeEnd
	toGuest *pipeEnd
	done    chan error
	result  Result
}

func newRig(t *testing.T, opts Options) *rig {
	t.Helper()
	ownerR, ownerW := io.Pipe()
	guestR, guestW := io.Pipe()
	r := &rig{t: t, ownerW: ownerW, guestW: guestW, toOwner: &pipeEnd{}, toGuest: &pipeEnd{}, done: make(chan error, 1)}
	go func() {
		result, err := Run(opts, ownerR, r.toOwner, guestR, r.toGuest)
		r.result = result
		r.done <- err
	}()
	t.Cleanup(func() { ownerW.Close(); guestW.Close() })
	return r
}

func (r *rig) owner(b []byte) {
	r.t.Helper()
	if _, err := r.ownerW.Write(b); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) guest(b []byte) {
	r.t.Helper()
	if _, err := r.guestW.Write(b); err != nil {
		r.t.Fatal(err)
	}
}

// await waits until cond holds for what reached one side.
func await(t *testing.T, what string, side *pipeEnd, cond func([]byte) bool) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := side.bytes()
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; have %q", what, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type wireFrame struct {
	channel uint32
	kind    byte
	payload []byte
}

// frames parses frames after skip bytes of plain lines.
func frames(t *testing.T, data []byte) []wireFrame {
	t.Helper()
	var out []wireFrame
	for len(data) > 0 {
		if len(data) < HeaderSize {
			t.Fatalf("partial header %q", data)
		}
		size := int(binary.LittleEndian.Uint32(data[5:9]))
		if len(data) < HeaderSize+size {
			t.Fatalf("partial frame")
		}
		out = append(out, wireFrame{binary.LittleEndian.Uint32(data[0:4]), data[4], data[HeaderSize : HeaderSize+size]})
		data = data[HeaderSize+size:]
	}
	return out
}

// afterLines returns the bytes after n newline-ended lines.
func afterLines(data []byte, n int) ([]byte, bool) {
	for ; n > 0; n-- {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil, false
		}
		data = data[i+1:]
	}
	return data, true
}

const ownerHello = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"hello\",\"params\":{\"generation\":1,\"framed\":true,\"core\":{\"id\":\"c0ffee\",\"name\":\"owners-macbook-pro\"},\"calls_you\":\"cage-default\",\"features\":[\"automatic_input\"]}}\n"
const guestHello = "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"core\":{\"id\":\"beef\",\"name\":\"lima-default\"},\"version\":\"0.1.21\",\"framed\":true}}\n"

func call(id int, method string, params string) []byte {
	return Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":"+itoa(id)+",\"method\":\""+method+"\",\"params\":"+params+"}"))
}

func itoa(n int) string { return strings.TrimSpace(string(must(json.Marshal(n)))) }

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// framedRig is a rig past the framed hello, with a terminal stream on
// channel 3 and the events stream on channel 1.
func framedRig(t *testing.T, opts Options) *rig {
	t.Helper()
	r := newRig(t, opts)
	r.guest([]byte("Welcome to the environment\n"))
	r.owner([]byte(ownerHello))
	await(t, "hello at the guest", r.toGuest, func(b []byte) bool { return bytes.HasSuffix(b, []byte("\n")) })
	r.guest([]byte(guestHello))
	await(t, "hello reply at the owner", r.toOwner, func(b []byte) bool { return bytes.Equal(b, []byte(guestHello)) })
	r.owner(call(2, "events.subscribe", "{}"))
	r.owner(call(7, "terminal.attach", "{\"terminal\":\"t1\"}"))
	await(t, "calls at the guest", r.toGuest, func(b []byte) bool {
		rest, ok := afterLines(b, 1)
		return ok && len(rest) > 0 && strings.Contains(string(rest), "terminal.attach")
	})
	r.guest(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"channel\":1,\"terminals\":[]}}")))
	r.guest(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"channel\":3,\"resize_notifications\":false}}")))
	await(t, "both replies at the owner", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":7") })
	return r
}

// ownerFrames are the frames that reached the owner after the hello reply.
func (r *rig) ownerFrames() []wireFrame {
	rest, ok := afterLines(r.toOwner.bytes(), 1)
	if !ok {
		r.t.Fatal("no hello reply at the owner")
	}
	return frames(r.t, rest)
}

func (r *rig) guestFrames() []wireFrame {
	rest, ok := afterLines(r.toGuest.bytes(), 1)
	if !ok {
		r.t.Fatal("no hello at the guest")
	}
	return frames(r.t, rest)
}

func streamText(fs []wireFrame, channel uint32) string {
	var b strings.Builder
	for _, f := range fs {
		if f.channel == channel && f.kind == KindData {
			b.Write(f.payload)
		}
	}
	return b.String()
}

func credited(fs []wireFrame, channel uint32) int {
	total := 0
	for _, f := range fs {
		if f.channel == channel && f.kind == KindCredit {
			total += int(binary.LittleEndian.Uint32(f.payload))
		}
	}
	return total
}

func TestHelloLosesTheOwnersHostName(t *testing.T) {
	r := newRig(t, Options{})
	r.owner([]byte(ownerHello))
	got := await(t, "hello", r.toGuest, func(b []byte) bool { return bytes.HasSuffix(b, []byte("\n")) })
	if strings.Contains(string(got), "owners-macbook-pro") {
		t.Fatalf("the host name reached the guest: %s", got)
	}
	var hello map[string]any
	if err := json.Unmarshal(got, &hello); err != nil {
		t.Fatal(err)
	}
	params, _ := hello["params"].(map[string]any)
	core, _ := params["core"].(map[string]any)
	features, _ := params["features"].([]any)
	if hello["method"] != "hello" || hello["id"] != float64(1) || params["framed"] != true || params["generation"] != float64(1) || core["name"] != PeerName || core["id"] != "c0ffee" || params["calls_you"] != "cage-default" || len(features) != 1 || len(core) != 2 || len(params) != 5 {
		t.Fatalf("hello changed beyond the name: %s", got)
	}
}

func TestBannerDroppedReplyPassesFramesFollow(t *testing.T) {
	r := framedRig(t, Options{})
	if !strings.HasPrefix(string(r.toOwner.bytes()), guestHello) {
		t.Fatalf("the banner reached the owner: %q", r.toOwner.bytes())
	}
	if fs := r.ownerFrames(); len(fs) != 2 {
		t.Fatalf("replies at the owner: %d", len(fs))
	}
}

func TestCallBacksAreDroppedAndDenied(t *testing.T) {
	r := framedRig(t, Options{})
	messages := []struct {
		json string
		pass bool
		deny string
	}{
		{"{\"jsonrpc\":\"2.0\",\"id\":40,\"method\":\"terminal.send\",\"params\":{\"terminal\":\"t3\",\"text\":\"rm -rf ~\"}}", false, "40"},
		{"{\"jsonrpc\":\"2.0\",\"id\":\"x\",\"method\":\"terminal.read\",\"params\":{\"terminal\":\"t3\"}}", false, "\"x\""},
		{"{\"jsonrpc\":\"2.0\",\"id\":41,\"method\":\"terminal.get\",\"params\":{}}", false, "41"},
		{"{\"jsonrpc\":\"2.0\",\"id\":42,\"method\":\"terminal.wait\",\"params\":{}}", false, "42"},
		{"{\"jsonrpc\":\"2.0\",\"method\":\"notes\",\"params\":{\"parent\":\"t3\",\"text\":\"typed\"}}", false, ""},
		{"{\"jsonrpc\":\"2.0\",\"method\":\"$/orphaned\",\"params\":{\"parent\":\"x/t1\"}}", false, ""},
		{"{\"jsonrpc\":\"2.0\",\"id\":43,\"method\":\"made.up\",\"params\":{}}", false, "43"},
		{"{\"jsonrpc\":\"2.0\",\"id\":44,\"method\":\"terminal.send\",\"method\":\"$/cancelRequest\"}", false, ""},
		{"[{\"jsonrpc\":\"2.0\",\"id\":45,\"method\":\"terminal.send\"}]", false, ""},
		{"not json", false, ""},
		{"{\"jsonrpc\":\"2.0\",\"method\":\"$/cancelRequest\",\"params\":{\"id\":9}}", true, ""},
		{"{\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{\"ok\":true}}", true, ""},
		{"{\"jsonrpc\":\"2.0\",\"id\":10,\"error\":{\"code\":-32000,\"message\":\"no\"}}", true, ""},
		// Herdr's parser is case-sensitive, like this filter: no method.
		{"{\"jsonrpc\":\"2.0\",\"id\":11,\"Method\":\"terminal.send\",\"result\":{}}", true, ""},
	}
	before := len(r.ownerFrames())
	beforeGuest := len(r.guestFrames())
	for _, m := range messages {
		r.guest(Frame(0, KindData, []byte(m.json)))
	}
	r.guest(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}")))
	await(t, "the last reply", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":99") })
	var passed []string
	for _, f := range r.ownerFrames()[before:] {
		passed = append(passed, string(f.payload))
	}
	want := []string{}
	for _, m := range messages {
		if m.pass {
			want = append(want, m.json)
		}
	}
	want = append(want, "{\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}")
	if strings.Join(passed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("at the owner:\n%s\nwant:\n%s", strings.Join(passed, "\n"), strings.Join(want, "\n"))
	}
	var denied []string
	for _, f := range r.guestFrames()[beforeGuest:] {
		var reply map[string]any
		if f.channel != 0 || json.Unmarshal(f.payload, &reply) != nil {
			t.Fatalf("unexpected frame to the guest: %d %d %s", f.channel, f.kind, f.payload)
		}
		failure, _ := reply["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		if data["kind"] != "denied" || failure["code"] != float64(-32000) || failure["message"] != DeniedMessage {
			t.Fatalf("not a denial: %s", f.payload)
		}
		id, _ := json.Marshal(reply["id"])
		denied = append(denied, string(id))
	}
	wantDenied := []string{}
	for _, m := range messages {
		if m.deny != "" {
			wantDenied = append(wantDenied, m.deny)
		}
	}
	if strings.Join(denied, ",") != strings.Join(wantDenied, ",") {
		t.Fatalf("denied %v, want %v", denied, wantDenied)
	}
}

func TestStreamsAreFilteredAndCreditedBack(t *testing.T) {
	r := framedRig(t, Options{ClipboardWrite: true, Window: time.Hour})
	r.owner(Frame(3, KindFocus, []byte{1}))
	r.owner(call(8, "server.status", "{}"))
	await(t, "owner frames at the guest", r.toGuest, func(b []byte) bool { return strings.Contains(string(b), "server.status") })
	input := "hello \x1b]52;c;?\x07\x1b]1337;File=name=eA==:AAAA\x07\x1b]52;c;aGk=\x07\x1b[>q\x1b]8;;file:///etc/passwd\x07x\x1b]8;;\x07 world"
	r.guest(Frame(3, KindData, []byte(input)))
	r.guest(Frame(4, KindData, []byte("unannounced \x1b]52;c;aGk=\x07")))
	r.guest(Frame(1, KindData, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"heartbeat\",\"params\":{\"seq\":1}}\n")))
	await(t, "the events line", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "heartbeat") })
	fs := r.ownerFrames()
	got := streamText(fs, 3)
	want := "hello \x1b]52;c;aGk=\x07x\x1b]8;;\x07 world"
	if got != want {
		t.Fatalf("stream 3 at the owner: %q, want %q", got, want)
	}
	if streamText(fs, 4) != "" {
		t.Fatal("an unannounced channel reached the owner")
	}
	if streamText(fs, 1) != "{\"jsonrpc\":\"2.0\",\"method\":\"heartbeat\",\"params\":{\"seq\":1}}\n" {
		t.Fatalf("events changed: %q", streamText(fs, 1))
	}
	removed := len(input) - len(got)
	await(t, "credit back", r.toGuest, func([]byte) bool { return credited(r.guestFrames(), 3) == removed })
	if credited(r.guestFrames(), 1) != 0 {
		t.Fatal("events were credited though nothing was removed")
	}
}

func TestClipboardWritesFollowTheOwnersPane(t *testing.T) {
	set := "\x1b]52;c;aGk=\x07"
	for _, c := range []struct {
		name  string
		setup func(r *rig)
		pass  bool
	}{
		{"focused", func(r *rig) { r.owner(Frame(3, KindFocus, []byte{1})) }, true},
		{"focus left", func(r *rig) { r.owner(Frame(3, KindFocus, []byte{1})); r.owner(Frame(3, KindFocus, []byte{0})) }, false},
		{"typed, no focus reports", func(r *rig) { r.owner(Frame(3, KindData, []byte("ls\r"))) }, true},
		{"only a terminal reply", func(r *rig) { r.owner(Frame(3, KindData, []byte("\x1b[12;40R"))) }, false},
		{"only a focus report", func(r *rig) { r.owner(Frame(3, KindData, []byte("\x1b[I"))) }, false},
		{"automatic input", func(r *rig) { r.owner(Frame(3, KindAutomaticInput, []byte("a"))) }, false},
		{"nothing", func(*rig) {}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := framedRig(t, Options{ClipboardWrite: true, Window: time.Hour})
			c.setup(r)
			r.owner(call(8, "server.status", "{}"))
			await(t, "owner frames at the guest", r.toGuest, func(b []byte) bool { return strings.Contains(string(b), "server.status") })
			r.guest(Frame(3, KindData, []byte("a"+set+"b")))
			r.guest(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":8,\"result\":{}}")))
			await(t, "the status reply", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":8") })
			got := streamText(r.ownerFrames(), 3)
			if passed := got == "a"+set+"b"; passed != c.pass {
				t.Fatalf("got %q, pass %v", got, c.pass)
			}
		})
	}
	t.Run("window passed", func(t *testing.T) {
		r := framedRig(t, Options{ClipboardWrite: true, Window: time.Millisecond})
		r.owner(Frame(3, KindData, []byte("ls\r")))
		r.owner(call(8, "server.status", "{}"))
		await(t, "owner frames at the guest", r.toGuest, func(b []byte) bool { return strings.Contains(string(b), "server.status") })
		time.Sleep(20 * time.Millisecond)
		r.guest(Frame(3, KindData, []byte("a"+set+"b")))
		r.guest(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":8,\"result\":{}}")))
		await(t, "reply", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":8") })
		if got := streamText(r.ownerFrames(), 3); got != "ab" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("setting off", func(t *testing.T) {
		r := framedRig(t, Options{ClipboardWrite: false, Window: time.Hour})
		r.owner(Frame(3, KindFocus, []byte{1}))
		r.owner(call(8, "server.status", "{}"))
		await(t, "owner frames at the guest", r.toGuest, func(b []byte) bool { return strings.Contains(string(b), "server.status") })
		r.guest(Frame(3, KindData, []byte("a"+set+"b")))
		r.guest(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}")))
		await(t, "reply", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":99") })
		if got := streamText(r.ownerFrames(), 3); got != "ab" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestSplitFramesGiveTheSameOutput(t *testing.T) {
	stream := []byte("x\x1b]52;c;?\x07\x1b[31mred\x1b[0m \xe4\xbd\xa0\xe5\xa5\xbd \x1b]0;title\x07\x1b]1337;SetUserVar=a=b\x07end")
	var outputs []string
	for _, step := range []int{1 << 20, 1, 7} {
		r := framedRig(t, Options{})
		var wire []byte
		for start := 0; start < len(stream); start += 5 {
			wire = append(wire, Frame(3, KindData, stream[start:min(start+5, len(stream))])...)
		}
		wire = append(wire, Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}"))...)
		for start := 0; start < len(wire); start += step {
			r.guest(wire[start:min(start+step, len(wire))])
		}
		await(t, "reply", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":99") })
		outputs = append(outputs, streamText(r.ownerFrames(), 3))
	}
	want := "x\x1b[31mred\x1b[0m \xe4\xbd\xa0\xe5\xa5\xbd \x1b]0;title\x07end"
	for _, got := range outputs {
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestBoundsEndTheLink(t *testing.T) {
	big := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(big[5:9], MaxPayload+1)
	t.Run("guest frame", func(t *testing.T) {
		r := framedRig(t, Options{})
		r.guest(big)
		if err := <-r.done; !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	})
	t.Run("owner frame", func(t *testing.T) {
		r := framedRig(t, Options{})
		go r.ownerW.Write(big)
		if err := <-r.done; !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	})
	t.Run("banner", func(t *testing.T) {
		r := newRig(t, Options{})
		go r.guestW.Write(bytes.Repeat([]byte("banner line\n"), MaxBanner/10))
		if err := <-r.done; !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	})
	t.Run("line", func(t *testing.T) {
		r := newRig(t, Options{})
		go r.guestW.Write(bytes.Repeat([]byte("x"), MaxLine+10))
		if err := <-r.done; !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	})
	t.Run("close reason", func(t *testing.T) {
		r := framedRig(t, Options{})
		go r.guestW.Write(Frame(3, KindClose, make([]byte, MaxClose+1)))
		if err := <-r.done; !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	})
}

func TestAProbeStaysPlain(t *testing.T) {
	r := newRig(t, Options{})
	r.owner([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"hello\",\"params\":{\"generation\":1}}\n"))
	await(t, "hello", r.toGuest, func(b []byte) bool { return bytes.HasSuffix(b, []byte("\n")) })
	r.guest([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"core\":{\"id\":\"beef\",\"name\":\"g\"}}}\n"))
	r.guest([]byte("{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"terminal.send\",\"params\":{}}\n"))
	r.guest([]byte("{\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{}}\n"))
	got := await(t, "replies", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":4") })
	if strings.Contains(string(got), "terminal.send") {
		t.Fatalf("a plain call-back passed: %s", got)
	}
	r.guestW.Close()
	if err := <-r.done; err != nil || r.result.Framed {
		t.Fatal(err, r.result)
	}
}

type closeRecorder struct {
	pipeEnd
	closed atomic.Bool
}

func (c *closeRecorder) Close() error { c.closed.Store(true); return nil }

func TestOwnerHangUpEndsTheGuestSide(t *testing.T) {
	ownerR, ownerW := io.Pipe()
	guestR, guestW := io.Pipe()
	toGuest := &closeRecorder{}
	done := make(chan Result, 1)
	go func() {
		result, _ := Run(Options{}, ownerR, &pipeEnd{}, guestR, toGuest)
		done <- result
	}()
	ownerW.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !toGuest.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the guest side was not closed")
		}
		time.Sleep(time.Millisecond)
	}
	guestW.Close()
	if result := <-done; !result.OwnerEnded {
		t.Fatal("owner end not reported")
	}
}

func TestRemovedBytesNeverStallTheStream(t *testing.T) {
	r := framedRig(t, Options{})
	dropped := bytes.Repeat([]byte("\x1b]1337;SetUserVar=x=eA==\x07"), 4096)
	total := 0
	for total < 4*(256<<10) {
		r.guest(Frame(3, KindData, dropped))
		total += len(dropped)
	}
	await(t, "every byte credited", r.toGuest, func([]byte) bool { return credited(r.guestFrames(), 3) == total })
}
