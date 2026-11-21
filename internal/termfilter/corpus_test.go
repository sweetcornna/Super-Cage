package termfilter

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The corpus is what real programs send (testdata/corpus/README.md). Every
// byte of it passes unchanged, except the sequences named here, each because
// Cage's web terminal (xterm.js 6) does not act on it either or because the
// viewer owns it.
var corpusDropped = map[string][]string{
	"vim": {
		"\x1b[>4;2m", "\x1b[>4;m", // XTMODKEYS: not in xterm.js
		"\x1b[?4m",                                             // XTQMODKEYS query: not in xterm.js
		"\x1b[0%m",                                             // not in xterm.js
		"\x1b[>c",                                              // DA2: names the emulator and its version
		"\x1bPzz\x1b\\",                                        // DCS vim uses as a no-op marker
		"\x1b[22;1t", "\x1b[22;2t", "\x1b[23;1t", "\x1b[23;2t", // title stack: ignored by Cage's xterm.js
		"\x1b[?1004h", "\x1b[?1004l", // focus reports: the viewer's own
	},
	"tmux": {
		"\x1b[>c", "\x1b[>q", // DA2, XTVERSION: name the emulator
		"\x1b[14t", "\x1b[18t", // size reports: not answered by Cage's xterm.js
		"\x1b[22;0;0t", "\x1b[23;0;0t", // title stack
		"\x1b[?2031h", "\x1b[?2031l", "\x1b[?996n", // colour scheme reports: not in xterm.js
		"\x1b[?1004l", // focus reports
	},
	"top":  {"\x1b[?1034h"}, // meta key mode: not in xterm.js
	"less": {},
	"ink": {
		"\x1b[>1u", "\x1b[<u", // kitty keyboard: not in xterm.js
		"\x1b[?1004h", "\x1b[?1004l", // focus reports
	},
	"crossterm": {
		"\x1b[?u", "\x1b[>7u", "\x1b[<1u", // kitty keyboard
		"\x1b[?1004h", "\x1b[?1004l", // focus reports
	},
}

func TestFilterCorpusPassesUnchanged(t *testing.T) {
	files, err := filepath.Glob("testdata/corpus/*.bin")
	if err != nil || len(files) != len(corpusDropped) {
		t.Fatal(files, err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		name = name[:len(name)-len(".bin")]
		dropped, ok := corpusDropped[name]
		if !ok {
			t.Fatalf("%s: no expectation", name)
		}
		in, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want, counted := in, 0
		for _, seq := range dropped {
			n := bytes.Count(want, []byte(seq))
			if n == 0 {
				t.Errorf("%s: %q is not in the corpus", name, seq)
			}
			if seq != "\x1b[?1004h" && seq != "\x1b[?1004l" {
				counted += n
			}
			want = bytes.ReplaceAll(want, []byte(seq), nil)
		}
		f := New(Options{ClipboardWrite: true})
		got := f.Append(nil, in)
		if !bytes.Equal(got, want) {
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}
			t.Errorf("%s: differs at %d: got %q, want %q", name, i, got[i:min(len(got), i+40)], want[i:min(len(want), i+40)])
		}
		total := 0
		for _, n := range f.Dropped() {
			total += n
		}
		if total != counted {
			t.Errorf("%s: %d drops counted, want %d:\n%s", name, total, counted, f.Report())
		}
		// Byte at a time, as a slow stream delivers it, gives the same.
		g := New(Options{ClipboardWrite: true})
		var split []byte
		for i := range in {
			split = g.Append(split, in[i:i+1])
		}
		if !bytes.Equal(split, got) {
			t.Errorf("%s: split output differs", name)
		}
	}
}
