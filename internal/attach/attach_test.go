package attach

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestParseArgsReset(t *testing.T) {
	cfg, err := ParseArgs([]string{"--reset"})
	if err != nil || !cfg.Reset {
		t.Fatal(cfg, err)
	}
}

// A project path reaches the window's command line and a copied command:
// no control character, read by rune, and valid UTF-8; any other text is
// fine.
func TestParseArgsRefusesControlsInProject(t *testing.T) {
	for _, path := range []string{"/work/a\x1b[201~b", "/work/a\x7fb", "/work/a\u0085b", "/work/a\u009b31mb", "/work/a\xffb", "/work/a\x0fb", "/work/a\tb"} {
		if _, err := ParseArgs([]string{"--new", "shell", "--project", path}); err == nil {
			t.Errorf("%q accepted", path)
		}
	}
	for _, path := range []string{"/work/项目 一", "/work/it's mine", "/work/émoji-😀"} {
		if _, err := ParseArgs([]string{"--new", "shell", "--project", path}); err != nil {
			t.Errorf("%q refused: %v", path, err)
		}
	}
}

// What a server says is printed only as text.
func TestServerTextIsPrinted(t *testing.T) {
	refused := &attachHTTPError{status: 409, text: "\x1b]0;owned\x07项目不存在\x1b[2J\u009b31m\xff"}
	message := refused.exit().Message
	if strings.ContainsAny(message, "\x1b\x07\u009b") || !utf8.ValidString(message) || !strings.Contains(message, "项目不存在") {
		t.Fatalf("server text printed as %q", message)
	}
}

// The desktop's 复制命令 (ui/src/local-terminal-state.mjs copyCommand) gives
// these; each must run as it is.
func TestParseArgsCopiedCommands(t *testing.T) {
	for _, line := range []string{
		"--terminal 0123456789abcdef current",
		"--terminal 0123456789abcdef --observe current",
		"--resume 3f2a8c1e-1111-4222-8333-944455556666 current",
		"--new shell --project /work/x current",
		"--new claude --project /work/x --claude-account acct-1 current",
	} {
		cfg, err := ParseArgs(strings.Fields(line))
		if err != nil || cfg.Target.Connection != "current" {
			t.Errorf("%q: %+v %v", line, cfg.Target, err)
		}
	}
	// The operand first stops flag parsing: refused, never misread.
	if _, err := ParseArgs([]string{"current", "--terminal", "0123456789abcdef"}); err == nil {
		t.Error("operand before flags accepted")
	}
}
