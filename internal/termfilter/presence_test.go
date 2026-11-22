package termfilter

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

type inputUnit struct {
	data string
	kind InputKind
}

// readAll feeds chunks to one reader and lists the units it gives, with a
// Release after every chunk when release is set.
func readAll(chunks []string, release bool) []inputUnit {
	var r InputReader
	var units []inputUnit
	each := func(unit []byte, kind InputKind, _ Reply) {
		units = append(units, inputUnit{string(unit), kind})
	}
	for _, chunk := range chunks {
		r.Read([]byte(chunk), each)
		if release {
			r.Release(each)
		}
	}
	r.Release(each)
	return units
}

func presence(units []inputUnit) (keys []string) {
	for _, u := range units {
		if u.kind == InputKey || u.kind == InputMouse || u.kind == InputPaste {
			keys = append(keys, u.data)
		}
	}
	return keys
}

func joined(units []inputUnit) string {
	var b strings.Builder
	for _, u := range units {
		b.WriteString(u.data)
	}
	return b.String()
}

// merged joins runs of text and of paste, which reads may cut anywhere
// without changing what they are.
func merged(units []inputUnit) []inputUnit {
	var out []inputUnit
	for _, u := range units {
		text := u.kind == InputPaste || u.kind == InputKey && u.data[0] != 0x1b
		if n := len(out); n > 0 && text && out[n-1].kind == u.kind && (u.kind == InputPaste || out[n-1].data[0] != 0x1b) {
			out[n-1].data += u.data
			continue
		}
		out = append(out, u)
	}
	return out
}

func reads(stream string, size int) (chunks []string) {
	for len(stream) > size {
		chunks, stream = append(chunks, stream[:size]), stream[size:]
	}
	return append(chunks, stream)
}

// The review's case (review3-ssh R3-1): 5000 cursor answers read 32 KiB at
// a time are 5000 answers, no key.
func TestInputReaderKeepsAnswersCutByReads(t *testing.T) {
	stream := strings.Repeat("\x1b[12;145R", 5000)
	units := readAll(reads(stream, 32<<10), false)
	if keys := presence(units); len(keys) != 0 {
		t.Fatalf("taken as keys: %q", keys)
	}
	if len(units) != 5000 || joined(units) != stream {
		t.Fatalf("%d units, bytes kept: %v", len(units), joined(units) == stream)
	}
}

// A key typed between answers still counts, wherever the reads cut.
func TestInputReaderKeyBetweenAnswersCounts(t *testing.T) {
	stream := strings.Repeat("\x1b[12;145R", 3) + "a" + strings.Repeat("\x1b[?1;2c", 3)
	for size := 1; size <= len(stream); size++ {
		units := readAll(reads(stream, size), false)
		if keys := presence(units); strings.Join(keys, "") != "a" || joined(units) != stream {
			t.Fatalf("reads of %d: keys %q", size, keys)
		}
	}
}

// Every form an answer or report takes, cut at every byte: the same units
// as read whole; and when the reader gives up between the two reads, the
// pieces are fragments, never a key.
var readerForms = []struct {
	in   string
	kind InputKind
}{
	{"\x1b[?1;2c", InputReply}, {"\x1b[?62;c", InputReply}, {"\x1b[?62;52;c", InputReply},
	{"\x1b[>0;276;0c", InputReply}, {"\x1b[0n", InputReply}, {"\x1b[12;145R", InputReply},
	{"\x1b[?12;145R", InputReply}, {"\x1b[?12;145;1R", InputReply}, {"\x1b[?2004;1$y", InputReply},
	{"\x1b[4;2$y", InputReply}, {"\x1b[?2147483647;0$y", InputReply},
	{"\x1bP1$r0m\x1b\\", InputReply}, {"\x1bP0$r\x1b\\", InputReply}, {"\x1bP1$r61;1\"p\x1b\\", InputReply},
	{"\x1b]11;rgb:0000/0000/0000\x1b\\", InputReply}, {"\x1b]10;rgb:ffff/ffff/ffff\x07", InputReply},
	{"\x1b]4;1;rgb:cdcd/0000/0000\x07", InputReply},
	{"\x1b]12;rgb:d9d9/7777/5757\x1b\\", InputReply}, {"\x1bP1$r" + strings.Repeat("1", 119) + "m\x1b\\", InputReply},
	{"\x1b[<35;10;5M", InputMotion}, {"\x1b[M" + "C*%", InputMotion}, {"\x1b[67;10;5M", InputMotion},
	{"\x1b[<0;10;5M", InputMouse}, {"\x1b[<0;10;5m", InputMouse}, {"\x1b[M" + " *%", InputMouse},
	{"\x1b[I", InputFocusIn}, {"\x1b[O", InputFocusOut},
}

func TestInputReaderEveryFormCutAtEveryByte(t *testing.T) {
	for _, form := range readerForms {
		if units := readAll([]string{form.in}, false); len(units) != 1 || units[0].kind != form.kind {
			t.Fatalf("%q whole: %v", form.in, units)
		}
		for i := 1; i < len(form.in); i++ {
			chunks := []string{"x" + form.in[:i], form.in[i:] + "y"}
			units := readAll(chunks, false)
			want := []inputUnit{{"x", InputKey}, {form.in, form.kind}, {"y", InputKey}}
			if fmt.Sprint(units) != fmt.Sprint(want) {
				t.Fatalf("%q cut at %d: %q", form.in, i, units)
			}
			released := readAll(chunks, true)
			if keys := presence(released); fmt.Sprint(keys) != fmt.Sprint([]string{"x", "y"}) || joined(released) != "x"+form.in+"y" {
				t.Fatalf("%q cut at %d, given up between: keys %q, units %q", form.in, i, keys, released)
			}
		}
	}
}

// What is complete at the end of a read is not held; only what could still
// grow into a longer unit is, and never past the longest answer.
func TestInputReaderHoldsOnlyWhatCouldContinue(t *testing.T) {
	for in, held := range map[string]bool{
		"a": false, "\x1b[A": false, "\x1bOP": false, "\x1bx": false, "\x1b[12;145R": false, "\x1b]11;rgb:0/0/0\x07": false,
		"\x1b": true, "\x1b[": true, "\x1b[12;14": true, "\x1bO": true, "\x1b[M": true, "\x1b[M " + "*": true,
		"\x1b]11;rgb:0": true, "\x1bP1$r0m": true, "\x1bP1$r0m\x1b": true, "\x1b[<35;10;5": true,
	} {
		var r InputReader
		r.Read([]byte(in), func([]byte, InputKind, Reply) {})
		if r.Held() != held {
			t.Errorf("%q held: %v", in, r.Held())
		}
	}
	var r InputReader
	var units []inputUnit
	long := "\x1b[" + strings.Repeat("1", 200)
	for _, chunk := range reads(long, 16) {
		r.Read([]byte(chunk), func(unit []byte, kind InputKind, _ Reply) { units = append(units, inputUnit{string(unit), kind}) })
	}
	r.Release(func(unit []byte, kind InputKind, _ Reply) { units = append(units, inputUnit{string(unit), kind}) })
	if r.Held() || joined(units) != long || units[0] != (inputUnit{long[:replyLimit], InputFragment}) || len(presence(units)) != 0 {
		t.Fatalf("a sequence longer than any answer: held %v, %q", r.Held(), units)
	}
}

// Once part of a unit was given up on for want of its rest, the rest of it
// is given at once as it comes, never held again, and only up to the
// longest answer: past that no answer goes on, and input is read afresh. A
// sequence given up on for its length is given at once to its end.
func TestInputReaderGivenUpUnitIsNotHeldAgain(t *testing.T) {
	for start, filler := range map[string]string{"\x1b]": "j", "\x1bP": "j", "\x1b[12;1": "1"} {
		var r InputReader
		var units []inputUnit
		each := func(unit []byte, kind InputKind, _ Reply) { units = append(units, inputUnit{string(unit), kind}) }
		r.Read([]byte(start[:1]), each)
		r.Release(each)
		typed := start[1:] + strings.Repeat(filler, 2*replyLimit)
		for i := range len(typed) {
			before := len(units)
			r.Read([]byte{typed[i]}, each)
			if r.Held() || len(units) != before+1 || units[before].data != typed[i:i+1] {
				t.Fatalf("%q given up on, then %q: held %v, units %q", start[:1], typed[:i+1], r.Held(), units[before:])
			}
			if fragment := units[before].kind == InputFragment; fragment != (i+1 < replyLimit) {
				t.Fatalf("%q given up on, then %d bytes: %q is %v", start[:1], i+1, typed[i:i+1], units[before].kind)
			}
		}
	}
	var r InputReader
	var units []inputUnit
	each := func(unit []byte, kind InputKind, _ Reply) { units = append(units, inputUnit{string(unit), kind}) }
	long := "\x1b]" + strings.Repeat("a", replyLimit)
	r.Read([]byte(long), each)
	r.Read([]byte("b"), each)
	r.Read([]byte("\x07c"), each)
	if r.Held() || fmt.Sprint(units) != fmt.Sprint([]inputUnit{{long, InputFragment}, {"b", InputFragment}, {"\x07", InputFragment}, {"c", InputKey}}) {
		t.Fatalf("after a sequence longer than any answer: held %v, %q", r.Held(), units)
	}
}

// A bracketed paste is typed whatever it holds, its markers cut anywhere.
func TestInputReaderPasteCutAnywhere(t *testing.T) {
	stream := "\x1b[12;1R\x1b[200~\x1b[12;1Rtext\x1b[201~\x1b[12;1R"
	for size := 1; size <= len(stream); size++ {
		units := readAll(reads(stream, size), false)
		var kinds []InputKind
		for _, u := range units {
			if len(kinds) == 0 || kinds[len(kinds)-1] != u.kind {
				kinds = append(kinds, u.kind)
			}
		}
		if fmt.Sprint(kinds) != fmt.Sprint([]InputKind{InputReply, InputKey, InputPaste, InputReply}) || joined(units) != stream {
			t.Fatalf("reads of %d: %q", size, units)
		}
	}
}

// Any input cut anywhere gives the units it gives whole, byte for byte.
func TestInputReaderChunkingIsInvisible(t *testing.T) {
	pieces := []string{"a", "中", "\x1b", "[", "\x1b[12;145R", "\x1b[?1;2c", "\x1b[<35;1;1M", "\x1b[<0;1;1M", "\x1b[I", "\x1b[O",
		"\x1bP1$r0m\x1b\\", "\x1b]11;rgb:0000/0000/0000\x07", "\x1b[200~", "\x1b[201~", "\x1bOP", "\x1b[A", "\x1b]", "\x1bP", "1;2", "R", "~",
		"\x1bP1$r" + strings.Repeat("1", 119) + "m\x1b\\"} // the longest answer the grammar takes
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 3000; round++ {
		var in strings.Builder
		for range 1 + rng.Intn(12) {
			in.WriteString(pieces[rng.Intn(len(pieces))])
		}
		stream := in.String()
		whole := readAll([]string{stream}, false)
		var chunks []string
		for rest := stream; len(rest) > 0; {
			n := 1 + rng.Intn(len(rest))
			chunks, rest = append(chunks, rest[:n]), rest[n:]
		}
		if got := readAll(chunks, false); fmt.Sprint(merged(got)) != fmt.Sprint(merged(whole)) || joined(got) != stream {
			t.Fatalf("%q in %q: %q, whole %q", stream, chunks, got, whole)
		}
		if got := readAll(reads(stream, 1), false); fmt.Sprint(merged(got)) != fmt.Sprint(merged(whole)) {
			t.Fatalf("%q byte by byte: %q, whole %q", stream, got, whole)
		}
	}
}

// At most queryRate answered queries pass each second; the rest are
// dropped, and still heard by OnQuery (the server counts what Cage's web
// terminal answers).
func TestQueriesPassAtMostRatePerSecond(t *testing.T) {
	heard := 0
	f := New(Options{OnQuery: func(Reply) { heard++ }})
	now := time.Unix(1000, 0)
	f.clock = func() time.Time { return now }
	out := string(f.Append(nil, []byte(strings.Repeat("\x1b[6n", 100))))
	if got := strings.Count(out, "\x1b[6n"); got != 64 || heard != 100 {
		t.Fatalf("%d passed, %d heard", got, heard)
	}
	out = string(f.Append(nil, []byte("\x1b]11;?\x07\x1bP$qm\x1b\\\x1b[c")))
	if out != "" {
		t.Fatalf("passed over the rate: %q", out)
	}
	now = now.Add(time.Second)
	out = string(f.Append(nil, []byte("\x1b]11;?\x07\x1bP$qm\x1b\\\x1b[c\x1b[?2004$p")))
	if out != "\x1b]11;?\x07\x1bP$qm\x1b\\\x1b[c\x1b[?2004$p" {
		t.Fatalf("not passed in the next second: %q", out)
	}
	// Text and other sequences are not limited.
	if out := string(f.Append(nil, []byte(strings.Repeat("\x1b[1mx", 200)))); strings.Count(out, "x") != 200 {
		t.Fatalf("%q", out)
	}
	if !bytes.Contains([]byte(f.Report()), []byte("over the rate")) {
		t.Fatal(f.Report())
	}
}

// A lone Esc given up on (the Esc key) is the start of an answer only when
// what follows can continue one: a key or another sequence after it is
// what it is.
func TestInputReaderReleasedEscStartsNoAnswer(t *testing.T) {
	for rest, keys := range map[string][]string{
		"a":                    {"a"},
		"\x1b[A":               {"\x1b[A"},
		"\x1b":                 nil, // held, then given up on too
		"[12;145R":             nil,
		"]11;rgb:0/0/0\x07":    nil,
		"P1$r0m\x1b\\":         nil,
		"\x1b[12;145Ra":        {"a"},
		"[12;145R\x1b[12;145R": nil,
	} {
		units := readAll([]string{"\x1b", rest}, true)
		if got := presence(units); fmt.Sprint(got) != fmt.Sprint(keys) || joined(units) != "\x1b"+rest {
			t.Errorf("Esc given up on, then %q: keys %q, units %q", rest, got, units)
		}
	}
}

// Whatever the input and wherever reads cut it, the bytes given are the
// bytes read, in order, no unit is empty, and what is held stays short.
func FuzzInputReader(f *testing.F) {
	f.Add([]byte("\x1b[12;145R\x1b[200~a\x1b[2\x1b[201~\x1b]11;rgb:0/0/0\x1b\\\x1b"), uint8(3), true)
	f.Add([]byte("\x1b\x1b[A\x1bP1$r0m\x1b\\x\x1bOP"), uint8(1), false)
	f.Fuzz(func(t *testing.T, data []byte, size uint8, release bool) {
		var r InputReader
		var out []byte
		each := func(unit []byte, _ InputKind, _ Reply) {
			if len(unit) == 0 {
				t.Fatalf("%q: an empty unit", data)
			}
			out = append(out, unit...)
		}
		n := int(size)%64 + 1
		for start := 0; start < len(data); start += n {
			r.Read(data[start:min(start+n, len(data))], each)
			if len(r.held) >= replyLimit {
				t.Fatalf("%q: held %d bytes", data, len(r.held))
			}
			if release {
				r.Release(each)
			}
		}
		r.Release(each)
		if string(out) != string(data) {
			t.Fatalf("%q read %d at a time gave %q", data, n, out)
		}
	})
}
