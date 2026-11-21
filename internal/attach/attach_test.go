package attach

import (
	"strings"
	"testing"
)

func TestParseArgsFilterStatsIsHidden(t *testing.T) {
	cfg, err := ParseArgs([]string{"--filter-stats", "--terminal", "0123456789abcdef", "--config", "/tmp/x/config.json"})
	if err != nil || !cfg.FilterStats || cfg.Terminal != "0123456789abcdef" {
		t.Fatal(cfg, err)
	}
	if cfg, err = ParseArgs([]string{"--config", "/tmp/x/config.json"}); err != nil || cfg.FilterStats {
		t.Fatal(cfg, err)
	}
	if strings.Contains(attachUsage, "filter-stats") {
		t.Fatal("the usage names a hidden flag")
	}
}

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
		"scroll region": "\x1b[r", "SGR": "\x1b[m", "cursor style": "\x1b[0 q", "cursor visible": "\x1b[?25h",
		"palette": "\x1b]104\x07", "foreground": "\x1b]110\x07", "background": "\x1b]111\x07", "cursor colour": "\x1b]112\x07",
		"title": "\x1b[23;0t", "character set": "\x1b(B", "wrap": "\x1b[?7h",
	} {
		if !strings.Contains(ResetSequence, seq) {
			t.Errorf("%s: %q missing", name, seq)
		}
	}
	if !strings.HasSuffix(ResetSequence, "\x1b[23;0t") || TitlePush != "\x1b[22;0t" {
		t.Fatal("the title pop is not last, or does not match the push")
	}
}

func TestParseArgsReset(t *testing.T) {
	cfg, err := ParseArgs([]string{"--reset"})
	if err != nil || !cfg.Reset {
		t.Fatal(cfg, err)
	}
}
