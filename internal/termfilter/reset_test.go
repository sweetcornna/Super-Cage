package termfilter

import (
	"strings"
	"testing"
)

// The reset covers what the review listed, after aborting whatever the
// terminal was in the middle of.
func TestResetSequenceCoversEveryMode(t *testing.T) {
	if !strings.HasPrefix(ResetSequence, "\x18\x1b\\") {
		t.Fatal("the reset does not abort a sequence in progress first")
	}
	for name, seq := range map[string]string{
		"synchronized update": "\x1b[?2026l", "alternate screen": "\x1b[?1049l",
		"X10 mouse": "\x1b[?9l", "normal mouse": "\x1b[?1000l", "button mouse": "\x1b[?1002l", "any mouse": "\x1b[?1003l",
		"UTF-8 mouse": "\x1b[?1005l", "SGR mouse": "\x1b[?1006l", "urxvt mouse": "\x1b[?1015l", "SGR pixel mouse": "\x1b[?1016l",
		"focus": "\x1b[?1004l", "bracketed paste": "\x1b[?2004l", "in-band resize": "\x1b[?2048l", "colour scheme": "\x1b[?2031l",
		"kitty keyboard": "\x1b[<99u", "cursor keys": "\x1b[?1l", "keypad mode": "\x1b[?66l", "keypad": "\x1b>",
		"main screen and scroll region, cursor kept": "\x1b7\x1b[?1049l\x1b[r\x1b8", "SGR": "\x1b[m", "cursor style": "\x1b[0 q", "cursor visible": "\x1b[?25h",
		"palette": "\x1b]104\x07", "foreground": "\x1b]110\x07", "background": "\x1b]111\x07", "cursor colour": "\x1b]112\x07",
		"title": "\x1b[23;0t", "character set": "\x1b(B", "wrap": "\x1b[?7h",
	} {
		if !strings.Contains(ResetSequence, seq) {
			t.Errorf("%s: %q missing", name, seq)
		}
	}
	if strings.Contains(ResetSequence, "[?6l") || strings.Contains(ResetSequence, "[?6h") {
		t.Fatal("changing origin mode would move the cursor")
	}
	if !strings.HasSuffix(ResetSequence, "\x1b[23;0t") || TitlePush != "\x1b[22;0t" {
		t.Fatal("the title pop is not last, or does not match the push")
	}
}

// End spoils a clipboard write the application is inside before the reset,
// whose string terminator would otherwise complete it.
func TestEndAbortsThenResets(t *testing.T) {
	f := New(Options{ClipboardWrite: true})
	out := f.Append(nil, []byte("\x1b]52;c;QUFB"))
	if !f.Streaming() {
		t.Fatalf("the write did not stream through: %q", out)
	}
	if got := string(f.End(nil)); got != "!\x07"+ResetSequence || f.Streaming() || f.Pending() != 0 {
		t.Fatalf("%q", got)
	}
	if got := string(New(Options{}).End([]byte("x"))); got != "x"+ResetSequence {
		t.Fatalf("%q", got)
	}
}
