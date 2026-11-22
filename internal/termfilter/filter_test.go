package termfilter

import (
	"bytes"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const esc = "\x1b"

// filterAll runs input through a new filter in one chunk, in every split into
// two chunks, and byte by byte, and fails unless all agree.
func filterAll(t testing.TB, opts Options, replay bool, input string) string {
	t.Helper()
	run := func(chunks ...string) string {
		f := New(opts)
		f.SetReplay(replay)
		var out []byte
		for _, chunk := range chunks {
			out = f.Append(out, []byte(chunk))
			if f.Pending() > HoldLimit+4 {
				t.Fatalf("held %d bytes", f.Pending())
			}
		}
		return string(out)
	}
	whole := run(input)
	for i := 1; i < len(input); i++ {
		if got := run(input[:i], input[i:]); got != whole {
			t.Fatalf("split at %d of %q: %q, whole: %q", i, input, got, whole)
		}
	}
	bytewise := make([]string, len(input))
	for i := 0; i < len(input); i++ {
		bytewise[i] = input[i : i+1]
	}
	if got := run(bytewise...); got != whole {
		t.Fatalf("byte by byte %q: %q, whole: %q", input, got, whole)
	}
	return whole
}

func TestFilterTable(t *testing.T) {
	on := Options{ClipboardWrite: true}
	long := strings.Repeat("x", NoticeLimit+1)
	for _, row := range []struct {
		name, in, out string
		opts          Options
		replay        bool
	}{
		// Design 5.7, row by row.
		{name: "OSC 52 query", in: "a" + esc + "]52;c;?\x07b", out: "ab", opts: on},
		{name: "OSC 52 query ST", in: "a" + esc + "]52;;?" + esc + "\\b", out: "ab", opts: on},
		{name: "OSC 52 set passes", in: "a" + esc + "]52;c;aGVsbG8=\x07b", out: "a" + esc + "]52;c;aGVsbG8=\x07b", opts: on},
		{name: "OSC 52 set empty selection", in: esc + "]52;;aGk=" + esc + "\\", out: esc + "]52;;aGk=" + esc + "\\", opts: on},
		{name: "OSC 52 set off by switch", in: "a" + esc + "]52;c;aGk=\x07b", out: "ab"},
		{name: "OSC 52 primary selection", in: esc + "]52;p;aGk=\x07", out: "", opts: on},
		{name: "OSC 52 not base64 is spoiled", in: esc + "]52;c;aG*k=\x07z", out: esc + "]52;c;aG!\x07z", opts: on},
		{name: "OSC 52 replayed", in: esc + "]52;c;aGk=\x07", out: "", opts: on, replay: true},
		{name: "OSC 8 https", in: esc + "]8;;https://example.com\x07x" + esc + "]8;;\x07", out: esc + "]8;;https://example.com\x07x" + esc + "]8;;\x07"},
		{name: "OSC 8 HTTP upper case", in: esc + "]8;id=1;HTTP://a\x07", out: esc + "]8;id=1;HTTP://a\x07"},
		{name: "OSC 8 file", in: esc + "]8;;file:///etc/passwd\x07x" + esc + "]8;;\x07", out: "x" + esc + "]8;;\x07"},
		{name: "OSC 8 ssh", in: esc + "]8;;ssh://host" + esc + "\\x", out: "x"},
		{name: "OSC 8 x-man-page", in: esc + "]8;;x-man-page://ls\x07", out: ""},
		{name: "OSC 1337 file", in: "a" + esc + "]1337;File=name=eA==;inline=1:aGk=\x07b", out: "ab"},
		{name: "OSC 1337 request upload", in: esc + "]1337;RequestUpload=format=tgz\x07", out: ""},
		{name: "OSC 1337 set user var", in: esc + "]1337;SetUserVar=a=Yg==\x07", out: ""},
		{name: "OSC 5113 kitty file transfer", in: esc + "]5113;ac=send;id=x\x07", out: ""},
		// The inner ESC ends the passthrough in the application too; the lone ST
		// left over is a no-op.
		{name: "DCS tmux passthrough", in: "a" + esc + "Ptmux;" + esc + esc + "]52;c;?\x07" + esc + "\\b", out: "ab"},
		{name: "kitty graphics file", in: "a" + esc + "_Ga=T,t=f;L2V0Yy9wYXNzd2Q=" + esc + "\\b", out: "ab"},
		{name: "kitty graphics temp file", in: esc + "_Gt=t,a=T;L3RtcC94" + esc + "\\", out: ""},
		{name: "kitty graphics shared memory", in: esc + "_Ga=T,t=s;bmFtZQ==" + esc + "\\", out: ""},
		// Images do not work in Cage's web terminal; none passes here.
		{name: "kitty graphics direct", in: "a" + esc + "_Ga=T,f=100,t=d;iVBORw0KGgo=" + esc + "\\b", out: "ab"},
		{name: "kitty graphics default medium", in: esc + "_Ga=T,f=24,s=1,v=1;AAAA" + esc + "\\", out: ""},
		{name: "kitty graphics delete", in: esc + "_Ga=d,d=a" + esc + "\\", out: ""},
		{name: "kitty graphics later key wins", in: esc + "_Gt=d,t=f;AAAA" + esc + "\\", out: ""},
		{name: "kitty graphics BEL is no terminator", in: esc + "_Gt=d\x07,t=f;AAAA" + esc + "\\x", out: "x"},
		{name: "OSC 9 notification", in: esc + "]9;Claude is done\x07", out: esc + "]9;Claude is done\x07"},
		{name: "OSC 9 too long", in: esc + "]9;" + long + "\x07", out: ""},
		{name: "OSC 9 control character", in: esc + "]9;a\nb\x07", out: ""},
		{name: "OSC 777 notify", in: esc + "]777;notify;Claude;done" + esc + "\\", out: esc + "]777;notify;Claude;done" + esc + "\\"},
		{name: "OSC 777 other", in: esc + "]777;preexec\x07", out: ""},
		{name: "OSC 99 actions dropped", in: esc + "]99;i=1:a=report:d=0;Done\x07", out: esc + "]99;i=1:d=0;Done\x07"},
		{name: "OSC 99 plain", in: esc + "]99;;Done\x07", out: esc + "]99;;Done\x07"},
		{name: "OSC 99 query", in: esc + "]99;i=1:p=?;\x07", out: ""},
		{name: "OSC 99 close report", in: esc + "]99;i=1:c=1;x\x07", out: ""},
		{name: "notification replayed", in: esc + "]9;done\x07" + esc + "]777;notify;a;b\x07x", out: "x", replay: true},
		{name: "ENQ", in: "a\x05b", out: "ab"},
		{name: "XTVERSION", in: "a" + esc + "[>q" + esc + "[>0qb", out: "ab"},
		{name: "DA2", in: "a" + esc + "[>c" + esc + "[>0cb", out: "ab"},
		{name: "DA3", in: "a" + esc + "[=c" + esc + "[=0cb", out: "ab"},
		{name: "title report", in: "a" + esc + "[21tb", out: "ab"},
		{name: "icon label report", in: "a" + esc + "[20tb", out: "ab"},
		// Everything else passes.
		{name: "SGR", in: esc + "[1;38;2;255;0;0mred" + esc + "[0m", out: esc + "[1;38;2;255;0;0mred" + esc + "[0m"},
		{name: "undercurl", in: esc + "[4:3m" + esc + "[58:2::255:0:0m~", out: esc + "[4:3m" + esc + "[58:2::255:0:0m~"},
		{name: "OSC 0 title", in: esc + "]0;~/project\x07", out: esc + "]0;~/project\x07"},
		{name: "OSC 2 title UTF-8", in: esc + "]2;项目 ✳\x07", out: esc + "]2;项目 ✳\x07"},
		{name: "OSC 2 title too long", in: esc + "]2;" + long + "\x07", out: ""},
		{name: "OSC 10 colour query", in: esc + "]10;?\x07", out: esc + "]10;?\x07"},
		{name: "OSC 11 colour set", in: esc + "]11;rgb:00/00/00" + esc + "\\", out: esc + "]11;rgb:00/00/00" + esc + "\\"},
		{name: "OSC 104 reset all", in: esc + "]104\x07", out: esc + "]104\x07"},
		{name: "DA1", in: esc + "[c" + esc + "[0c", out: esc + "[c" + esc + "[0c"},
		{name: "cursor position query", in: esc + "[6n", out: esc + "[6n"},
		// The kitty keyboard protocol is not in xterm.js; programs fall back.
		{name: "kitty keyboard", in: esc + "[>1u" + esc + "[?u" + esc + "[<u" + esc + "[=1;1u", out: ""},
		{name: "bracketed paste mode", in: esc + "[?2004h", out: esc + "[?2004h"},
		{name: "sixel", in: "a" + esc + "Pq#0;2;0;0;0#0~~@@vv-\r\n!14@" + esc + "\\b", out: "ab"},
		{name: "sixel with parameters", in: esc + "P0;1;0q\"1;1#0~" + esc + "\\", out: ""},
		{name: "synchronized update by DCS", in: esc + "P=1s" + esc + "\\", out: ""},
		{name: "synchronized update mode", in: esc + "[?2026h" + esc + "[?2026l", out: esc + "[?2026h" + esc + "[?2026l"},
		// Modes no program here sets are dropped; origin mode moves the
		// cursor whenever it changes.
		{name: "origin mode", in: esc + "[?6h" + esc + "[?6l", out: ""},
		{name: "unused mouse and keypad modes", in: esc + "[?9h" + esc + "[?1016h" + esc + "[?66h" + esc + "[?45h", out: ""},
		{name: "a dropped mode drops the sequence", in: esc + "[?1000;6h", out: ""},
		// Focus reports belong to the viewer: taken out, the rest passes.
		{name: "focus alone", in: "a" + esc + "[?1004h" + "b", out: "ab"},
		{name: "focus with others", in: esc + "[?1004;1006h" + esc + "[?1000;1004l", out: esc + "[?1006h" + esc + "[?1000l"},
		{name: "DECRQSS", in: esc + "P$q\"p" + esc + "\\", out: esc + "P$q\"p" + esc + "\\"},
		{name: "charset and keypad", in: esc + "(B" + esc + "=" + esc + "7" + esc + "8", out: esc + "(B" + esc + "=" + esc + "7" + esc + "8"},
		{name: "CJK text", in: "中文图表 ✓ 😀", out: "中文图表 ✓ 😀"},
		// Additions: what the design table did not name.
		{name: "XTGETTCAP names the application", in: "a" + esc + "P+q544e" + esc + "\\b", out: "ab"},
		{name: "C1 CSI as UTF-8", in: "a\xc2\x9b>qb", out: "a>qb"},
		{name: "C1 OSC as UTF-8", in: "a\xc2\x9d52;c;?\x07b", out: "a52;c;?\x07b"},
		{name: "raw C1 CSI", in: "a\x9b>qb", out: "a>qb"},
		{name: "overlong ESC", in: "a\xc0\x9b[>qb", out: "a[>qb"},
		{name: "C1 inside a title", in: esc + "]2;a\xc2\x9cb\x07", out: ""},
		{name: "surrogate", in: "a\xed\xa0\x80b", out: "ab"},
		{name: "OSC 7 working directory", in: esc + "]7;file://host/home/cage\x07", out: ""},
		{name: "OSC 50 font", in: esc + "]50;?\x07", out: ""},
		{name: "unknown OSC", in: esc + "]7501;status\x07x", out: "x"},
		{name: "PM and SOS", in: "a" + esc + "^private" + esc + "\\" + esc + "Xstring" + esc + "\\b", out: "ab"},
		{name: "window resize", in: "a" + esc + "[8;100;200t" + esc + "[3;0;0t" + esc + "[2t" + esc + "[30tb", out: "ab"},
		// Cage's web terminal sets no window options: xterm.js answers no size
		// report and ignores the title stack. The viewer pushes and pops the
		// title itself.
		{name: "window operations", in: esc + "[18t" + esc + "[22;0t" + esc + "[23;0t" + esc + "[22;2t" + esc + "[23;0;0t", out: ""},
		{name: "CSI aborted by ESC", in: esc + "[12" + esc + "[1mx", out: esc + "[1mx"},
		{name: "CSI aborted by CAN", in: esc + "[12\x18x", out: "x"},
		{name: "C0 inside CSI runs first", in: esc + "[1\n;2H", out: "\n" + esc + "[1;2H"},
		{name: "unfinished CSI held", in: esc + "[1;2", out: ""},
		{name: "OSC ended by another escape", in: esc + "]0;t" + esc + "[1m", out: esc + "]0;t" + esc + "\\" + esc + "[1m"},
		{name: "replay drops queries", in: esc + "[c" + esc + "[6n" + esc + "[?u" + esc + "[?1$p" + esc + "]11;?\x07" + esc + "P$qm" + esc + "\\" + esc + "[18t" + esc + "Z" + "\x07x", out: "x", replay: true},
		{name: "replay keeps drawing", in: esc + "[1mx" + esc + "]0;t\x07" + esc + "]11;rgb:0/0/0\x07", out: esc + "[1mx" + esc + "]0;t\x07" + esc + "]11;rgb:0/0/0\x07", replay: true},
		{name: "replay kitty query", in: esc + "_Ga=q,i=1;AAAA" + esc + "\\", out: "", replay: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := filterAll(t, row.opts, row.replay, row.in); got != row.out {
				t.Fatalf("in %q\n got %q\nwant %q", row.in, got, row.out)
			}
		})
	}
}

func TestFilterBounds(t *testing.T) {
	on := Options{ClipboardWrite: true}
	// A held sequence never grows past HoldLimit, and one that would is dropped.
	for _, start := range []string{esc + "[", esc + "]0;", esc + "]8;;https://", esc + "P$q", esc + "_Ga=T", esc + "Pq"} {
		f := New(on)
		out := f.Append(nil, []byte(start+strings.Repeat("1", 3*HoldLimit)))
		if f.Pending() > HoldLimit {
			t.Fatalf("%q held %d", start, f.Pending())
		}
		out = f.Append(out, []byte("\x07"+esc+"\\m"+esc+"[0m"))
		if !strings.HasSuffix(string(out), "m"+esc+"[0m") || strings.Contains(string(out), "111") {
			t.Fatalf("%q: %q", start, out)
		}
	}
	// A clipboard write over the web terminal's limit is spoiled and ended.
	f := New(on)
	payload := strings.Repeat("QUFB", ClipboardLimit/4+1)
	out := string(f.Append(nil, []byte(esc+"]52;c;"+payload+"\x07after")))
	if f.Pending() != 0 || !strings.HasSuffix(out, "!\x07after") || len(out) != len(esc+"]52;c;")+ClipboardLimit+len("!\x07after") {
		t.Fatal(len(out), out[len(out)-20:])
	}
	// A clipboard write streams through without being held.
	f = New(on)
	text := strings.Repeat("AAAA", 1<<16)
	out = string(f.Append(nil, []byte(esc+"]52;c;"+text)))
	if f.Pending() != 0 || len(out) != len(esc+"]52;c;")+len(text) {
		t.Fatal(f.Pending(), len(out))
	}
	if out = string(f.Append(nil, []byte("A\x07z"))); out != "A\x07z" {
		t.Fatalf("clipboard write ended by BEL: %q", out)
	}
}

// forbidden finds what the filter must never emit.
func forbidden(out []byte) string {
	s := string(out)
	if !utf8.ValidString(s) {
		return "invalid UTF-8"
	}
	for _, r := range s {
		if r >= 0x80 && r <= 0x9f {
			return "C1 control"
		}
		if r == 0x05 {
			return "ENQ"
		}
	}
	for _, needle := range []string{esc + "]52;c;?", esc + "]52;;?", esc + "]1337", esc + "]5113", esc + "]7;", esc + "[>q", esc + "[>0q", esc + "[>c", esc + "[=c", esc + "[20t", esc + "[21t", esc + "P+q", esc + "Ptmux", esc + "^", esc + "X", esc + "]8;;file"} {
		if strings.Contains(s, needle) {
			return needle
		}
	}
	for _, pattern := range forbiddenPatterns {
		if found := pattern.FindString(s); found != "" {
			return found
		}
	}
	return ""
}

var forbiddenPatterns = []*regexp.Regexp{
	// kitty graphics that read the owner's files or memory
	regexp.MustCompile("\x1b_G[^;\x1b]*t=[fts]"),
	// window operations other than 14, 16, 18, 22 and 23
	regexp.MustCompile("\x1b\\[(?:[0-9]|1[0-357-9]|2[0-14-9]|[3-9][0-9]|[0-9]{3,})(?:;[0-9;]*)?t"),
}

// pieces are fragments a hostile guest mixes; the property test glues them
// at random and cuts the result into random chunks.
var pieces = []string{
	"text ", "中文", "\xc2", "\x9b", "\x9d", "\xc2\x9b", "\x05", "\x07", "\x18", "\n", esc, esc + "\\",
	esc + "[", "?", ">", "=", "q", "c", "t", "20", "21", ";", "0", "1", "8", "3J",
	esc + "]", "52;c;", "52;c;?", "aGk=", "1337;File=", "8;;", "file:///", "https://x", "7;", "9;", "99;", "a=report", "777;notify;",
	esc + "P", "tmux;", "+q", "$q", "q#0~", esc + "_G", "t=d", "t=f", ",", "a=T", "AAAA", esc + "^", esc + "X",
}

func TestFilterPropertyNeverEmitsDroppedSequences(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 20000; round++ {
		var input strings.Builder
		for n := rng.Intn(24); n >= 0; n-- {
			input.WriteString(pieces[rng.Intn(len(pieces))])
		}
		in := []byte(input.String())
		for _, opts := range []Options{{}, {ClipboardWrite: true}, {KeepScrollback: true}} {
			for _, replay := range []bool{false, true} {
				whole := New(opts)
				whole.SetReplay(replay)
				want := whole.Append(nil, in)
				f := New(opts)
				f.SetReplay(replay)
				var got []byte
				for rest := in; len(rest) > 0; {
					n := 1 + rng.Intn(len(rest))
					got = f.Append(got, rest[:n])
					rest = rest[n:]
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("chunking changed %q: %q vs %q", in, got, want)
				}
				if bad := forbidden(want); bad != "" {
					t.Fatalf("%q emitted %q in %q", in, bad, want)
				}
				again := New(opts)
				again.SetReplay(replay)
				if twice := again.Append(nil, want); !bytes.Equal(twice, want) {
					t.Fatalf("output %q of %q is not stable: %q", want, in, twice)
				}
			}
		}
	}
}

func FuzzFilter(f *testing.F) {
	for _, piece := range pieces {
		f.Add([]byte(piece), uint8(3), true)
	}
	f.Add([]byte(esc+"]52;c;aGk=\x07"+esc+"_Ga=T,t=f;AA"+esc+"\\\xc2\x9b>q"), uint8(1), false)
	f.Fuzz(func(t *testing.T, in []byte, cut uint8, clipboard bool) {
		opts := Options{ClipboardWrite: clipboard, KeepScrollback: cut&0x80 != 0}
		want := New(opts).Append(nil, in)
		g := New(opts)
		var got []byte
		step := int(cut%16) + 1
		for rest := in; len(rest) > 0; {
			n := min(step, len(rest))
			got = g.Append(got, rest[:n])
			if g.Pending() > HoldLimit+4 {
				t.Fatalf("held %d", g.Pending())
			}
			rest = rest[n:]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunking changed the output: %q vs %q", got, want)
		}
		if bad := forbidden(want); bad != "" {
			t.Fatalf("emitted %q", bad)
		}
		if twice := New(opts).Append(nil, want); !bytes.Equal(twice, want) {
			t.Fatalf("output not stable: %q -> %q", want, twice)
		}
	})
}

func benchmarkFilter(b *testing.B, chunk []byte) {
	f := New(Options{ClipboardWrite: true})
	dst := make([]byte, 0, 2*len(chunk))
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = f.Append(dst[:0], chunk)
	}
}

func BenchmarkPlainASCII(b *testing.B) {
	benchmarkFilter(b, bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog 0123456789\r\n"), 1200))
}

func BenchmarkCJK(b *testing.B) {
	benchmarkFilter(b, bytes.Repeat([]byte("终端输出中文内容，混合 ASCII text 和标点。\r\n"), 1000))
}

func BenchmarkTUI(b *testing.B) {
	// Claude Code style output: colours, cursor moves and text.
	benchmarkFilter(b, bytes.Repeat([]byte(esc+"[2K"+esc+"[1G"+esc+"[38;2;215;119;87m✻"+esc+"[39m "+esc+"[1mThinking…"+esc+"[22m "+esc+"[2m(esc to interrupt)"+esc+"[0m\r\n"), 600))
}

// Review F1: OSC 9 sub-commands (ConEmu) and OSC 133 extras act outside the
// window in some applications: Ghostty turns 9;9 into a working directory
// for the owner's next local tab. The reviewer's bytes, row by row.
func TestFilterOSCNotificationsAndMarks(t *testing.T) {
	for _, row := range []struct{ name, in, out string }{
		{"9;9 file URL BEL", "a\x1b]9;9;file://localhost/Users/owner/.claude-cage-downloads/r\x07b", "ab"},
		{"9;9 file URL ST", "a\x1b]9;9;file://localhost/Users/owner/.claude-cage-downloads/r\x1b\\b", "ab"},
		{"9;9 path", "\x1b]9;9;/tmp\x07", ""},
		{"9;2 message box", "\x1b]9;2;message box\x07", ""},
		{"9;6 GUI macro", "\x1b]9;6;guimacro\x07", ""},
		{"9;7 run process", "\x1b]9;7;cmd\x07", ""},
		{"9;1 sleep", "\x1b]9;1;100\x07", ""},
		{"9;12 prompt mark", "\x1b]9;12\x07", ""},
		{"9 notification", "\x1b]9;hello\x07", "\x1b]9;hello\x07"},
		{"9 notification starting with digits and text", "\x1b]9;2 tests failed\x07", "\x1b]9;2 tests failed\x07"},
		{"9;4 progress", "\x1b]9;4;1;50\x07", "\x1b]9;4;1;50\x07"},
		{"9;4 clear", "\x1b]9;4;0\x07", "\x1b]9;4;0\x07"},
		{"9;4 indeterminate", "\x1b]9;4;3\x07", "\x1b]9;4;3\x07"},
		{"9;4 state out of range", "\x1b]9;4;5;10\x07", ""},
		{"9;4 progress out of range", "\x1b]9;4;1;101\x07", ""},
		{"9;4 extra field", "\x1b]9;4;1;50;x\x07", ""},
		{"9;4 text progress", "\x1b]9;4;1;abc\x07", ""},
		{"133 prompt start", "\x1b]133;A\x07", "\x1b]133;A\x07"},
		{"133 command end with status", "\x1b]133;D;0\x07", "\x1b]133;D;0\x07"},
		{"133 command end", "\x1b]133;D\x07", "\x1b]133;D\x07"},
		{"133 click events", "\x1b]133;A;click_events=1\x07", ""},
		{"133 command line URL", "\x1b]133;C;cmdline_url=file:///x\x07", ""},
		{"133 aid", "\x1b]133;A;aid=1\x07", ""},
		{"133 unknown mark", "\x1b]133;P;k=i\x07", ""},
		{"133 status text", "\x1b]133;D;err\x07", ""},
		{"777 notify", "\x1b]777;notify;t;b\x07", "\x1b]777;notify;t;b\x07"},
		{"777 other", "\x1b]777;preexec\x07", ""},
	} {
		if got := filterAll(t, Options{}, false, row.in); got != row.out {
			t.Errorf("%s: %q -> %q, want %q", row.name, row.in, got, row.out)
		}
	}
}

func TestFilterRemembersGuestFocusAndResets(t *testing.T) {
	f := New(Options{})
	f.Append(nil, []byte(esc+"[?1004h"))
	if !f.GuestFocus() {
		t.Fatal("focus request not remembered")
	}
	f.Append(nil, []byte(esc+"[?1006;1004l"))
	if f.GuestFocus() {
		t.Fatal("focus reset not remembered")
	}
	if f.Append(nil, []byte(esc+"c"+esc+"[!p")); f.Resets() != 2 {
		t.Fatal("resets", f.Resets())
	}
	if len(f.Dropped()) != 0 {
		t.Fatal(f.Report())
	}
}

// The counts are named after what the program sent, so their number is
// bounded: a program cannot grow the viewer's memory with distinct drops.
func TestFilterDropCountsAreBounded(t *testing.T) {
	f := New(Options{})
	for i := 0; i < 100000; i++ {
		f.Append(nil, []byte(fmt.Sprintf(esc+"[%dt"+esc+"]%d;x\x07", i, 20000+i%9999)))
	}
	if len(f.Dropped()) > maxDropKinds {
		t.Fatal(len(f.Dropped()))
	}
	total := 0
	for _, n := range f.Dropped() {
		total += n
	}
	if total != 200000 {
		t.Fatal(total)
	}
}

// Ending mid clipboard write spoils it, so the reset after it cannot apply
// a truncated clipboard.
func TestFilterAbortSpoilsAnOpenClipboardWrite(t *testing.T) {
	f := New(Options{ClipboardWrite: true})
	out := f.Append(nil, []byte(esc+"]52;c;QUFBQUFB"))
	if out = f.Abort(out); string(out) != esc+"]52;c;QUFBQUFB!\x07" || f.Pending() != 0 {
		t.Fatalf("%q", out)
	}
	g := New(Options{ClipboardWrite: true})
	if out := g.Abort(g.Append(nil, []byte(esc+"]0;tit"))); len(out) != 0 {
		t.Fatalf("a held title was sent: %q", out)
	}
}
