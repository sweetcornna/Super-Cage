package termfilter

import (
	"fmt"
	"strings"
	"testing"
)

// Review round 4, finding 1: a query whose answer echoes a number from the
// query passes only with a number short enough for the answer to be read
// as one (NextInput): DECRQM's mode, OSC 4's colour index.
func TestEchoedNumbersAreShort(t *testing.T) {
	for in, pass := range map[string]bool{
		"\x1b[?2004$p": true, "\x1b[4$p": true, "\x1b[?12345$p": true, "\x1b[?00001$p": true,
		"\x1b[?123456$p": false, "\x1b[?99999999999$p": false, "\x1b[?000001$p": false, "\x1b[?0000000000002004$p": false, "\x1b[123456$p": false, "\x1b[?1:2$p": false,
		"\x1b]4;1;?\x07": true, "\x1b]4;255;?;0;?\x07": true,
		"\x1b]4;1000;?\x07": false, "\x1b]4;99999;?\x07": false, "\x1b]4;1;?;2000;?\x07": false, "\x1b]4;;?\x07": false,
	} {
		got := filterAll(t, Options{}, false, in)
		if pass && got != in || !pass && got != "" {
			t.Errorf("%q gave %q", in, got)
		}
	}
}

// A complete sequence shaped like an answer that the answer grammar refuses
// (a field with too many digits, a string longer than any answer) is typed
// but never a key: whole or cut by reads, it is not the owner at the window.
func TestRefusedAnswersAreFragments(t *testing.T) {
	long := "\x1bP1$r" + strings.Repeat("1;", 70) + "m\x1b\\"
	for in, kind := range map[string]InputKind{
		"\x1b[?99999999999;0$y": InputFragment, "\x1b[99999999999;0$y": InputFragment, "\x1b[?2147483647;0$y": InputReply,
		"\x1b[123456;1R": InputFragment, "\x1b[?1;123456R": InputFragment, "\x1b[?123456;1c": InputFragment,
		"\x1b[>123456;1c": InputFragment, "\x1b[000000n": InputFragment, "\x1b[12;145R": InputReply,
		long: InputFragment, "\x1bP0$r" + strings.Repeat("x", 200) + "\x1b\\": InputFragment,
		"\x1b]4;1234;rgb:0/0/0\x07": InputFragment, "\x1b]11;rgb:fffff/0/0\x1b\\": InputFragment,
		"\x1b]10;rgb:" + strings.Repeat("0", 200) + "\x07": InputFragment,
		// Not shaped like an answer: as before.
		"\x1b[1;1:2R": InputKey, "\x1b[1234567;5u": InputKey, "\x1b[;R": InputKey,
	} {
		if n, got, _ := NextInput([]byte(in + "a")); n != len(in) || got != kind {
			t.Errorf("%q: %d bytes, %v", in, n, got)
		}
	}
	if n, kind, _ := NextInput([]byte("\x1b]0;title\x07")); n != 2 || kind != InputKey {
		t.Errorf("a title: %d bytes, %v", n, kind)
	}
	for _, in := range []string{long, "\x1b[?99999999999;0$y", "\x1b]4;1234;rgb:0/0/0\x07"} {
		for size := 1; size <= len(in); size++ {
			units := readAll(reads(in+"a", size), false)
			if keys := presence(units); fmt.Sprint(keys) != fmt.Sprint([]string{"a"}) || joined(units) != in+"a" {
				t.Fatalf("%q read %d at a time: keys %q, units %q", in, size, keys, units)
			}
		}
	}
}

// Review round 4, finding 2: ESC [200~ starts a paste only while the window
// has bracketed paste on; a paste ends at its end marker, when bracketed
// paste goes off, or after pasteLimit bytes.
func TestPasteNeedsBracketedPasteOn(t *testing.T) {
	on := false
	r := InputReader{PasteOn: func() bool { return on }}
	var units []inputUnit
	each := func(unit []byte, kind InputKind, _ Reply) { units = append(units, inputUnit{string(unit), kind}) }
	r.Read([]byte("\x1b[200~abc\x1b[12;145R"), each)
	if r.InPaste() || fmt.Sprint(units) != fmt.Sprint([]inputUnit{{"\x1b[200~", InputKey}, {"abc", InputKey}, {"\x1b[12;145R", InputReply}}) {
		t.Fatalf("with bracketed paste off: in paste %v, %q", r.InPaste(), units)
	}
	on, units = true, nil
	r.Read([]byte("\x1b[200~abc"), each)
	if !r.InPaste() {
		t.Fatal("with bracketed paste on: no paste")
	}
	on = false
	r.Read([]byte("\x1b[12;145R"), each)
	if r.InPaste() || fmt.Sprint(units) != fmt.Sprint([]inputUnit{{"\x1b[200~", InputKey}, {"abc", InputPaste}, {"\x1b[12;145R", InputReply}}) {
		t.Fatalf("bracketed paste turned off: in paste %v, %q", r.InPaste(), units)
	}
}

func TestPasteEndsAtItsBound(t *testing.T) {
	stream := "\x1b[200~" + strings.Repeat("a", pasteLimit+5) + "\x1b[12;145R"
	units := readAll(reads(stream, 32<<10), false)
	pasted, after := 0, ""
	for _, u := range units {
		switch {
		case u.kind == InputPaste:
			pasted += len(u.data)
		case u.data != "\x1b[200~":
			after += fmt.Sprintf("%q/%v ", u.data, u.kind)
		}
	}
	if pasted != pasteLimit || after != fmt.Sprintf("%q/%v %q/%v ", "aaaaa", InputKey, "\x1b[12;145R", InputReply) || joined(units) != stream {
		t.Fatalf("pasted %d, then %s", pasted, after)
	}
}

// The filter knows whether the window has bracketed paste on: the modes it
// passed, the reset it writes for ESC c, and a soft reset (xterm.js turns
// it off there).
func TestFilterTracksBracketedPaste(t *testing.T) {
	f := New(Options{})
	for _, step := range []struct {
		out string
		on  bool
	}{
		{"\x1b[?2004h", true}, {"\x1b[?1;2004l", false}, {"\x1b[?25;2004h", true}, {"\x1b[?2004;6l", true},
		{"\x1bc", false}, {"\x1b[?2004h", true}, {"\x1b[!p", false}, {"\x1b[?2004h", true}, {"\x1b[?2004l", false},
	} {
		f.Append(nil, []byte(step.out))
		if f.BracketedPaste() != step.on {
			t.Fatalf("after %q: %v", step.out, f.BracketedPaste())
		}
	}
}
