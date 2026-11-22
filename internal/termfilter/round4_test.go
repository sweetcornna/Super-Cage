package termfilter

import (
	"strings"
	"testing"
)

// A program may clear the window's scrollback, as over ssh: Ink-based
// programs (Claude Code) send 2J 3J H on every full redraw, and dropping 3J
// piles stale copies of the screen into the scrollback. KeepScrollback
// drops it, for an owner attaching by hand to a window whose scrollback
// they want kept. Whole, split and byte by byte (filterAll checks all).
func TestScrollbackClearPassesUnlessKept(t *testing.T) {
	for _, in := range []string{esc + "[3J", esc + "[03J", esc + "[?3J", esc + "[3;0J", esc + "[2J" + esc + "[3J" + esc + "[H"} {
		if got := filterAll(t, Options{}, false, "a"+in+"b"); got != "a"+in+"b" {
			t.Errorf("%q dropped by default: %q", in, got)
		}
		want := "a" + strings.ReplaceAll(in, esc+"[3J", "") + "b"
		if !strings.Contains(in, "[2J") {
			want = "ab"
		}
		if got := filterAll(t, Options{KeepScrollback: true}, false, "a"+in+"b"); got != want {
			t.Errorf("%q with KeepScrollback: %q, want %q", in, got, want)
		}
	}
	// Replayed history draws the same way.
	if got := filterAll(t, Options{}, true, "a"+esc+"[3Jb"); got != "a"+esc+"[3Jb" {
		t.Errorf("3J dropped in replay: %q", got)
	}
	if got := filterAll(t, Options{KeepScrollback: true}, true, "a"+esc+"[3Jb"); got != "ab" {
		t.Errorf("3J kept in replay with KeepScrollback: %q", got)
	}
	for _, in := range []string{esc + "[J", esc + "[2J", esc + "[1J", esc + "[?2J"} {
		for _, keep := range []bool{false, true} {
			if got := filterAll(t, Options{KeepScrollback: keep}, false, in); got != in {
				t.Errorf("%q dropped (keep %v): %q", in, keep, got)
			}
		}
	}
	// A full reset clears the scrollback too, unless it is kept.
	for _, keep := range []bool{false, true} {
		want := "a" + ClearReset + esc + "[3J" + "b"
		if keep {
			want = "a" + ClearReset + "b"
		}
		f := New(Options{KeepScrollback: keep})
		if got := filterAll(t, Options{KeepScrollback: keep}, false, "a"+esc+"cb"); got != want {
			t.Errorf("ESC c (keep %v): %q, want %q", keep, got, want)
		}
		if f.Append(nil, []byte(esc+"c")); f.Resets() != 1 {
			t.Errorf("ESC c not counted as a reset (keep %v)", keep)
		}
		if got := string(f.Clear(nil)); got != strings.TrimSuffix(strings.TrimPrefix(want, "a"), "b") {
			t.Errorf("Clear (keep %v): %q", keep, got)
		}
	}
}

// Tab stops are per screen in kitty: they are restored on the main screen,
// after the reset leaves the alternate one.
func TestEndRestoresTabStopsOnTheMainScreen(t *testing.T) {
	f := New(Options{})
	f.Append(nil, []byte(esc+"[3g"+esc+"[?1049h"))
	got := string(f.End(nil, 80))
	if tabs, main := strings.Index(got, esc+"[3g"), strings.Index(got, esc+"[?1049l"); tabs < 0 || main < 0 || tabs < main {
		t.Fatalf("tab stops at %d, main screen at %d: %q", tabs, main, got)
	}
}

// The reset by hand (cage attach --reset) knows nothing of what the guest
// did: it restores everything End restores when changed.
func TestResetSequenceRestoresEverythingEndMay(t *testing.T) {
	for name, part := range map[string]string{
		"tab stops": defaultTabStops, "blink": resetBlink, "progress": endProgress, "command mark": endCommand,
		"palette": resetPalette, "cursor style": resetCursorStyle, "link": endLink, "protection": resetProtection,
	} {
		if !strings.Contains(ResetSequence, part) {
			t.Errorf("%s missing", name)
		}
	}
	if string(appendTabStops(nil, 0)) != defaultTabStops {
		t.Fatal("the unknown-width tab stops differ from End's")
	}
	if strings.Index(ResetSequence, defaultTabStops) < strings.Index(ResetSequence, esc+"[?1049l") {
		t.Fatal("tab stops restored before leaving the alternate screen")
	}
}

// The spelled-out soft reset also sets new line mode off and US ASCII in
// G1 with G0 in use.
func TestClearResetSetsCharacterSetsAndNewLine(t *testing.T) {
	for _, part := range []string{esc + "[20l", esc + ")B", "\x0f"} {
		if !strings.Contains(ClearReset, part) {
			t.Errorf("%q missing", part)
		}
	}
	if got := string(New(Options{}).Append(nil, []byte(ClearReset))); got != ClearReset {
		t.Fatalf("the filter changes its own rewrite: %q", got)
	}
}
