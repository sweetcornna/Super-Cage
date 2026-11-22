package herdrlink

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// fuzzPrefix is a guest that answered a framed hello and announced the
// events stream (channel 1) and a terminal stream (channel 3).
func fuzzLink() (*Link, *pipeEnd, []byte) {
	toGuest := &pipeEnd{}
	l := New(Options{}, toGuest)
	l.ownerLine([]byte(ownerHello))
	l.ownerFrame(0, KindData, call(2, "events.subscribe", "{}")[HeaderSize:])
	l.ownerFrame(0, KindData, call(7, "terminal.attach", "{}")[HeaderSize:])
	prefix := []byte(guestHello)
	prefix = append(prefix, Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"channel\":1}}"))...)
	prefix = append(prefix, Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"channel\":3}}"))...)
	return l, toGuest, prefix
}

func FuzzGuestToOwner(f *testing.F) {
	f.Add([]byte{})
	f.Add(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"terminal.send\",\"params\":{\"text\":\"x\"}}")))
	f.Add(Frame(3, KindData, []byte("a\x1b]52;c;?\x07\x1b]52;c;aGk=\x07\x1b]1337;File=:AA\x07\x05b")))
	f.Add(append(Frame(3, KindData, []byte("\x1b]52;c;aG")), Frame(3, KindData, []byte("k=\x07"))...))
	f.Add(Frame(1, KindData, []byte("{\"method\":\"event\"}\n")))
	f.Add(Frame(3, KindClose, []byte("{}")))
	f.Add(Frame(9, KindData, []byte("x")))
	f.Add(Frame(0, KindData, []byte("{\"id\":1,\"method\":\"a\",\"method\":\"$/cancelRequest\"}")))
	f.Fuzz(func(t *testing.T, data []byte) {
		l, toGuest, prefix := fuzzLink()
		toOwner := &pipeEnd{}
		_ = l.Down(bytes.NewReader(append(prefix, data...)), toOwner)
		out := toOwner.bytes()
		if !bytes.HasPrefix(out, []byte(guestHelloCanonical)) {
			t.Fatalf("no hello reply first: %q", out)
		}
		streams := map[uint32][]byte{}
		for _, fr := range parseAll(t, out[len(guestHelloCanonical):]) {
			if len(fr.payload) > MaxPayload {
				t.Fatal("oversized frame")
			}
			switch {
			case fr.channel == 0:
				if fr.kind != KindData {
					t.Fatalf("control kind %d reached the owner", fr.kind)
				}
				fields, ok := topLevel(fr.payload)
				if !ok {
					t.Fatalf("malformed message reached the owner: %q", fr.payload)
				}
				if raw, has := fields["method"]; has {
					var method string
					if json.Unmarshal(raw, &method) != nil || method != "$/cancelRequest" {
						t.Fatalf("a guest call reached the owner: %q", fr.payload)
					}
				}
			case fr.channel == 1 || fr.channel == 3:
				if fr.kind == KindData {
					streams[fr.channel] = append(streams[fr.channel], fr.payload...)
				} else if fr.kind != KindCredit && fr.kind != KindResize && fr.kind != KindClose {
					t.Fatalf("kind %d reached the owner on a stream", fr.kind)
				}
			default:
				t.Fatalf("channel %d reached the owner", fr.channel)
			}
		}
		for channel, text := range streams {
			for _, bad := range []string{"\x1b]52", "\x1b]1337", "\x1b]5113", "\x05", "\x1bP+q", "\x1b[>q"} {
				if bytes.Contains(text, []byte(bad)) {
					t.Fatalf("channel %d passed %q: %q", channel, bad, text)
				}
			}
		}
		for _, fr := range parseAll(t, toGuest.bytes()) {
			switch {
			case fr.channel == 0 && fr.kind == KindData:
				var reply map[string]any
				if json.Unmarshal(fr.payload, &reply) != nil || reply["error"] == nil {
					t.Fatalf("not a denial to the guest: %q", fr.payload)
				}
			case fr.kind == KindCredit && len(fr.payload) == 4 && (fr.channel == 1 || fr.channel == 3):
			default:
				t.Fatalf("unexpected frame to the guest: %d %d %q", fr.channel, fr.kind, fr.payload)
			}
		}
	})
}

func parseAll(t *testing.T, data []byte) []wireFrame {
	t.Helper()
	var out []wireFrame
	for len(data) > 0 {
		if len(data) < HeaderSize {
			t.Fatalf("partial header")
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

func FuzzOwnerLine(f *testing.F) {
	f.Add([]byte(ownerHello))
	f.Add([]byte("{\"method\":\"hello\",\"params\":{\"core\":\"x\"}}\n"))
	f.Add([]byte("{\"method\":\"hello\",\"params\":{\"core\":{\"name\":1}},\"id\":null}\n"))
	f.Fuzz(func(t *testing.T, line []byte) {
		l := New(Options{}, &pipeEnd{})
		_, out := l.ownerLine(line)
		var message map[string]json.RawMessage
		if json.Unmarshal(out, &message) != nil {
			return
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(message["params"], &params) != nil {
			return
		}
		var core map[string]json.RawMessage
		if json.Unmarshal(params["core"], &core) != nil {
			return
		}
		var method string
		_ = json.Unmarshal(message["method"], &method)
		if raw, named := core["name"]; named && method == "hello" && string(raw) != "\""+PeerName+"\"" {
			// A hello the rewrite could not parse passes unchanged only
			// when Go's own parser does not see a name in it either.
			var original map[string]json.RawMessage
			if json.Unmarshal(bytes.TrimSpace(line), &original) == nil {
				t.Fatalf("a hello kept its name: %q -> %q", line, out)
			}
		}
	})
}
