package herdrlink

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

// desyncGuest is review 3's H1: a guest that answers the hello with a line
// Go reads and serde_json does not (pad), then hides a hello reply in a
// channel-0 frame's JSON whitespace, so that the owner's core would start
// reading frames where the guest chose, and lines up whole request frames
// inside stream data. pad "0" is the control: a reply both read.
func desyncGuest(pad string) []byte {
	l1 := `{"jsonrpc":"2.0","id":1,"result":{"core":{"id":"guestc0re"},"version":"0.1.21","features":[],"pad":` + pad + "}}\n"
	inner := `{"jsonrpc":"2.0","id":1,"result":{"core":{"id":"guestc0re"},"version":"0.1.21","features":[]}}`
	f1 := Frame(0, KindData, []byte("{\"x\":[\n"+inner+"\n,0,0]}"))
	// The events stream's reply, padded so that the owner's misread length
	// ends where the stream data's frames start.
	p2 := `{"jsonrpc":"2.0","id":2,"result":{"channel":16777216,"terminals":[]}}`
	p2 += strings.Repeat(" ", 125-len(p2))
	f2 := Frame(0, KindData, []byte(p2))
	d := bytes.Repeat([]byte("A"), 26)
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":77,"method":"terminal.send","params":{"terminal":"t1","text":"echo injected"}}`,
		`{"jsonrpc":"2.0","method":"notes","params":{"parent":"t1","text":"injected note"}}`,
	} {
		var h [HeaderSize]byte
		binary.LittleEndian.PutUint32(h[5:], uint32(len(m)))
		d = append(append(d, h[:]...), m...)
	}
	f3 := Frame(16777216, KindData, d)
	return slices.Concat([]byte(l1), f1, f2, f3)
}

var desyncPads = map[string]string{
	"lone surrogate": `"\ud800"`,
	"deep nesting":   strings.Repeat("[", 200) + "0" + strings.Repeat("]", 200),
	"out of range":   "1e400",
	"control":        "0",
}

// ownerSideLink is a link whose owner said a framed hello, subscribed to
// events (id 2) and attached to t1 (id 7), as the owner's core does first.
func ownerSideLink() (*Link, *pipeEnd) {
	toGuest := &pipeEnd{}
	l := New(Options{}, toGuest)
	l.ownerLine([]byte(ownerHello))
	l.ownerFrame(0, KindData, call(2, "events.subscribe", "{}")[HeaderSize:])
	l.ownerFrame(0, KindData, call(7, "terminal.attach", "{\"terminal\":\"t1\"}")[HeaderSize:])
	return l, toGuest
}

// ownerAgrees checks what the filter wrote to the owner against the owner's
// own reading: the same place where frames start, the same frames, and no
// call from the environment that the owner's core would serve.
func ownerAgrees(t *testing.T, l *Link, out []byte) ownerView {
	t.Helper()
	view := readAsOwner(out, 2)
	if len(view.calls) > 0 {
		t.Fatalf("the owner's core would serve calls from the environment: %v (%s)", view.calls, view.end)
	}
	if !l.framed.Load() {
		if view.helloEnd != -1 {
			t.Fatalf("the filter stayed with lines; the owner's core reads frames from %d", view.helloEnd)
		}
		return view
	}
	helloEnd := bytes.IndexByte(out, '\n') + 1
	var starts []int
	for pos := helloEnd; len(out)-pos >= HeaderSize; {
		starts = append(starts, pos)
		pos += HeaderSize + int(binary.LittleEndian.Uint32(out[pos+5:pos+9]))
	}
	if view.helloEnd != helloEnd || !slices.Equal(view.starts, starts) {
		t.Fatalf("the owner's core reads frames elsewhere: hello reply ends at %d for the owner, %d for the filter; frames at %v for the owner, %v for the filter (%s)", view.helloEnd, helloEnd, view.starts, starts, view.end)
	}
	return view
}

func TestTheOwnerReadsWhatTheFilterWrites(t *testing.T) {
	for name, pad := range desyncPads {
		t.Run(name, func(t *testing.T) {
			l, _ := ownerSideLink()
			toOwner := &pipeEnd{}
			err := l.Down(bytes.NewReader(desyncGuest(pad)), toOwner)
			view := ownerAgrees(t, l, toOwner.bytes())
			t.Logf("link: %v; owner: hello reply ends at %d, %d frames, %s", err, view.helloEnd, len(view.starts), view.end)
		})
	}
}

func FuzzTheOwnerReadsWhatTheFilterWrites(f *testing.F) {
	for _, pad := range desyncPads {
		f.Add(desyncGuest(pad))
	}
	f.Add([]byte(guestHello))
	f.Add([]byte("{\"jsonrpc\":\"2.0\",\"id\":1}\n"))
	f.Add([]byte("banner\n{\"id\":1,\"result\":{},\"x\":\"\\udc00\"}\n{\"id\":1,\"result\":{}}\n"))
	f.Add(slices.Concat([]byte(guestHello), Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"channel\":1}}")), Frame(1, KindData, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"event\",\"params\":{}}\n"))))
	f.Add(slices.Concat([]byte(guestHello), Frame(0, KindData, []byte("{\"id\":7,\"result\":{\"channel\":3}}")), Frame(3, KindData, []byte("\x00\x00\x00\x00\x00\x05\x00\x00\x00{\"m\":1}"))))
	f.Fuzz(func(t *testing.T, guest []byte) {
		l, _ := ownerSideLink()
		toOwner := &pipeEnd{}
		_ = l.Down(bytes.NewReader(guest), toOwner)
		ownerAgrees(t, l, toOwner.bytes())
	})
}
