package termfilter

import (
	"fmt"
	"strings"
	"testing"
)

// Round 2 of the review: the answers emulators really send, the colour
// queries whose answers are not recognised, unbounded counts, scrollback,
// and what the exit reset undoes.

// The answers emulators send (rewritten from the reviewer's cases, each
// with the emulator that sends it).
func TestNextInputAnswersEmulatorsSend(t *testing.T) {
	for _, row := range []struct {
		name, in string
		kinds    []InputKind
		replies  []Reply
	}{
		{"kitty DA1", esc + "[?62;c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"kitty DA1 with OSC 52", esc + "[?62;52;c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"xterm DA1", esc + "[?64;1;2;6;9;15;16;17;18;21;22;28c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"Ghostty DA1", esc + "[?62;22;52c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"WezTerm DA1", esc + "[?65;4;6;18;22c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"Alacritty DA1", esc + "[?6c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"Terminal.app and xterm.js DA1", esc + "[?1;2c", []InputKind{InputReply}, []Reply{ReplyDA1}},
		{"two empty fields are not an answer", esc + "[?62;;c", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"an empty field alone is not an answer", esc + "[?;c", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"xterm DA2", esc + "[>41;390;0c", []InputKind{InputReply}, []Reply{ReplyDA2}},
		{"kitty DA2", esc + "[>1;4000;41c", []InputKind{InputReply}, []Reply{ReplyDA2}},
		{"DA2 with four fields", esc + "[>1;2;3;4c", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"cursor position", esc + "[12;40R", []InputKind{InputReply}, []Reply{ReplyCursor}},
		{"DECXCPR with page", esc + "[?12;40;1R", []InputKind{InputReply}, []Reply{ReplyCursor}},
		{"DECXCPR without page", esc + "[?12;40R", []InputKind{InputReply}, []Reply{ReplyCursor}},
		{"status", esc + "[0n", []InputKind{InputReply}, []Reply{ReplyStatus}},
		{"DECRPM private", esc + "[?2026;2$y", []InputKind{InputReply}, []Reply{ReplyMode}},
		{"DECRPM ANSI", esc + "[4;2$y", []InputKind{InputReply}, []Reply{ReplyMode}},
		{"DECRQSS SGR", esc + "P1$r0m" + esc + "\\", []InputKind{InputReply}, []Reply{ReplySetting}},
		{"DECRQSS SGR with colons", esc + "P1$r0;4:3;58:2::255:0:0m" + esc + "\\", []InputKind{InputReply}, []Reply{ReplySetting}},
		{"DECRQSS invalid", esc + "P0$r" + esc + "\\", []InputKind{InputReply}, []Reply{ReplySetting}},
		{"OSC 11, four digits, ST", esc + "]11;rgb:1e1e/1e1e/1e1e" + esc + "\\", []InputKind{InputReply}, []Reply{ReplyColor}},
		{"OSC 11, two digits, BEL", esc + "]11;rgb:1e/1e/1e\x07", []InputKind{InputReply}, []Reply{ReplyColor}},
		{"OSC 4", esc + "]4;1;rgb:cdcd/0000/0000" + esc + "\\", []InputKind{InputReply}, []Reply{ReplyColor}},
		{"OSC 4, index 256", esc + "]4;256;rgb:cdcd/0000/0000" + esc + "\\", []InputKind{InputReply}, []Reply{ReplyColor}},
		// Answers to queries the filter does not pass (OSC 110;?, and
		// colour queries with more fields) stay unrecognised: typing.
		{"kitty OSC 110 answer", esc + "]110;rgb:1e1e/1e1e/1e1e" + esc + "\\", []InputKind{InputKey, InputKey, InputKey}, []Reply{ReplyNone, ReplyNone, ReplyNone}},
		{"OSC 17 answer", esc + "]17;rgb:1e1e/1e1e/1e1e" + esc + "\\", []InputKind{InputKey, InputKey, InputKey}, []Reply{ReplyNone, ReplyNone, ReplyNone}},
		{"focus in", esc + "[I", []InputKind{InputFocusIn}, []Reply{ReplyNone}},
		{"focus out", esc + "[O", []InputKind{InputFocusOut}, []Reply{ReplyNone}},
		{"SGR click", esc + "[<0;10;5M", []InputKind{InputMouse}, []Reply{ReplyNone}},
		{"X10 click", esc + "[M !!", []InputKind{InputMouse}, []Reply{ReplyNone}},
		{"urxvt click", esc + "[32;10;5M", []InputKind{InputMouse}, []Reply{ReplyNone}},
		{"SGR drag (button held)", esc + "[<32;10;5M", []InputKind{InputMouse}, []Reply{ReplyNone}},
		{"SGR wheel", esc + "[<64;10;5M", []InputKind{InputMouse}, []Reply{ReplyNone}},
		{"SGR motion, no button", esc + "[<35;10;5M", []InputKind{InputMotion}, []Reply{ReplyNone}},
		{"SGR motion with Shift", esc + "[<39;10;5M", []InputKind{InputMotion}, []Reply{ReplyNone}},
		{"X10 motion, no button", esc + "[MC!!", []InputKind{InputMotion}, []Reply{ReplyNone}},
		{"urxvt motion, no button", esc + "[67;10;5M", []InputKind{InputMotion}, []Reply{ReplyNone}},
		{"kitty key", esc + "[97;5u", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"Alt+P", esc + "P", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"Alt+]", esc + "]", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"Esc then [", esc + "[", []InputKind{InputKey}, []Reply{ReplyNone}},
		{"two answers together", esc + "[?62;c" + esc + "[12;40R", []InputKind{InputReply, InputReply}, []Reply{ReplyDA1, ReplyCursor}},
		{"an answer, then a key", esc + "[12;40Rx", []InputKind{InputReply, InputKey}, []Reply{ReplyCursor, ReplyNone}},
		{"paste start", esc + "[200~", []InputKind{InputKey}, []Reply{ReplyNone}},
	} {
		var kinds []InputKind
		var replies []Reply
		for data := []byte(row.in); len(data) > 0; {
			n, kind, reply := NextInput(data)
			kinds, replies = append(kinds, kind), append(replies, reply)
			data = data[n:]
		}
		if fmt.Sprint(kinds) != fmt.Sprint(row.kinds) || fmt.Sprint(replies) != fmt.Sprint(row.replies) {
			t.Errorf("%s %q: %v %v, want %v %v", row.name, row.in, kinds, replies, row.kinds, row.replies)
		}
	}
}

// Colour queries pass only in the forms whose answers NextInput recognises:
// kitty answers OSC 110;? as 110, and kitty and WezTerm answer the further
// fields of OSC 10/11/12 as 11, 12, 17, 19...
func TestColorQueriesPassOnlyWhereTheAnswerIsKnown(t *testing.T) {
	for _, row := range []struct {
		in   string
		pass bool
	}{
		{esc + "]10;?\x07", true},
		{esc + "]11;?" + esc + "\\", true},
		{esc + "]12;?\x07", true},
		{esc + "]4;1;?\x07", true},
		{esc + "]4;1;?;2;?\x07", true}, // answered one OSC 4 per colour
		{esc + "]11;#101010\x07", true},
		{esc + "]104\x07", true},
		{esc + "]104;1;2\x07", true},
		{esc + "]110\x07", true},
		{esc + "]111\x07", true},
		{esc + "]112\x07", true},
		{esc + "]110;?\x07", false},
		{esc + "]111;?\x07", false},
		{esc + "]112;?" + esc + "\\", false},
		{esc + "]104;?\x07", false},
		{esc + "]10;?;?\x07", false},
		{esc + "]12;?;?;?;?;?\x07", false},
		{esc + "]11;?;#000000\x07", false},
		{esc + "]10;#ffffff;#000000\x07", false}, // a set that runs on into 11
	} {
		got := filterAll(t, Options{}, false, "a"+row.in+"b")
		if want := map[bool]string{true: "a" + row.in + "b", false: "ab"}[row.pass]; got != want {
			t.Errorf("%q: %q, want %q", row.in, got, want)
		}
	}
	// The server still counts every query Cage's web terminal answers, and
	// none it does not.
	if q := Queries([]byte(esc + "]10;?;?\x07" + esc + "]110;?\x07" + esc + "]104;?\x07")); q[ReplyColor] != 2 {
		t.Fatal("colour queries counted:", q[ReplyColor])
	}
}

// A repeat or a count no emulator honours is dropped: WezTerm loops once per
// count (REP, ICH, CHT, CBT) with no bound.
func TestCountsAreBounded(t *testing.T) {
	for _, seq := range []string{
		"b", "@", "A", "B", "C", "D", "E", "F", "G", "I", "L", "M", "P", "S", "T", "X", "Z", "`", "a", "d", "e",
		" @", " A", "'}", "'~",
	} {
		ok := esc + "[65535" + seq
		if got := filterAll(t, Options{}, false, ok); got != ok {
			t.Errorf("%q dropped", ok)
		}
		for _, n := range []string{"65536", "4294967295", "99999999999", "000065536"} {
			if got := filterAll(t, Options{}, false, "x"+esc+"["+n+seq+"y"); got != "xy" {
				t.Errorf("%q passed: %q", n+seq, got)
			}
		}
	}
	for _, in := range []string{esc + "[1;65536H", esc + "[1;70000r", esc + "[38;2;1;65536;3m", esc + "[4:65536m"} {
		if got := filterAll(t, Options{}, false, in); got != "" {
			t.Errorf("%q passed: %q", in, got)
		}
	}
	if in := esc + "[38;2;255;0;0;48:2::1:2:3m"; filterAll(t, Options{}, false, in) != in {
		t.Error("SGR colours dropped")
	}
}

// A full reset becomes a soft reset that clears the screen (the
// scrollback: TestScrollbackClearPassesUnlessKept).
func TestFullResetBecomesASoftReset(t *testing.T) {
	f := New(Options{KeepScrollback: true})
	if got := string(f.Append(nil, []byte("a"+esc+"cb"))); got != "a"+ClearReset+"b" || f.Resets() != 1 {
		t.Fatalf("%q, %d resets", got, f.Resets())
	}
	if strings.Contains(ClearReset, esc+"c") || strings.Contains(ClearReset, "3J") || !strings.HasSuffix(ClearReset, esc+"[H"+esc+"[2J") {
		t.Fatalf("%q", ClearReset)
	}
}

// The exit reset undoes what the guest left and nothing it did not touch:
// the owner's palette and cursor style stay unless the guest changed them.
func TestEndUndoesWhatTheGuestLeft(t *testing.T) {
	const palette = "\x1b]104\x07\x1b]110\x07\x1b]111\x07\x1b]112\x07"
	end := func(output string) string {
		f := New(Options{})
		f.Append(nil, []byte(output))
		return string(f.End(nil, 80))
	}
	clean := end("hello")
	for _, part := range []string{palette, "\x1b[0 q", "\x1b]8;;", "\x1b]9;4;0", "\x1b[3g", "\x1b[?12l", "\x1b]133;D"} {
		if strings.Contains(clean, part) {
			t.Errorf("reset %q the guest never touched", part)
		}
	}
	if !strings.HasPrefix(clean, "\x18\x1b\\") || !strings.HasSuffix(clean, "\x1b[23;0t") || !strings.Contains(clean, "\x1b[0\"q") || !strings.Contains(clean, "\x1b[?1049l") {
		t.Fatalf("base reset: %q", clean)
	}
	tabs := "\x1b7\x1b[3g"
	for col := 9; col <= 80; col += 8 {
		tabs += fmt.Sprintf("\x1b[%dG\x1bH", col)
	}
	tabs += "\x1b8"
	for _, row := range []struct {
		name, output, want string
	}{
		{"palette entry", esc + "]4;1;#ff0000\x07", palette},
		{"background", esc + "]11;rgb:00/00/00" + esc + "\\", palette},
		{"palette reset", esc + "]104\x07", palette},
		{"cursor style", esc + "[5 q", "\x1b[0 q"},
		{"open link", esc + "]8;;https://example.com/\x07text", "\x1b]8;;\x07"},
		{"progress", esc + "]9;4;1;50\x07", "\x1b]9;4;0\x07"},
		{"tab stops cleared", esc + "[3g", tabs},
		{"tab stop set", "    " + esc + "H", tabs},
		{"cursor blink", esc + "[?12h", "\x1b[?12l"},
		{"command running", esc + "]133;C\x07", "\x1b]133;D\x07"},
	} {
		if got := end(row.output); !strings.Contains(got, row.want) || !strings.HasSuffix(got, "\x1b[23;0t") {
			t.Errorf("%s: %q lacks %q", row.name, got, row.want)
		}
	}
	for _, row := range []struct{ name, output, gone string }{
		{"link closed", esc + "]8;;https://example.com/\x07text" + esc + "]8;;\x07", "\x1b]8;;"},
		{"progress cleared", esc + "]9;4;1;50\x07" + esc + "]9;4;0\x07", "\x1b]9;4;0"},
		{"command ended", esc + "]133;C\x07" + esc + "]133;D;0\x07", "\x1b]133;D"},
		{"next prompt", esc + "]133;C\x07" + esc + "]133;A\x07", "\x1b]133;D"},
		{"queries only", esc + "]11;?\x07" + esc + "]4;1;?\x07", palette},
	} {
		if got := end(row.output); strings.Contains(got, row.gone) {
			t.Errorf("%s: %q still has %q", row.name, got, row.gone)
		}
	}
	// Unknown width: stops up to 256 columns.
	f := New(Options{})
	f.Append(nil, []byte(esc+"[3g"))
	if got := string(f.End(nil, 0)); !strings.Contains(got, "\x1b[249G\x1bH") || strings.Contains(got, "\x1b[257G") {
		t.Fatalf("%q", got)
	}
	// A by-hand reset knows nothing of the session: it resets everything
	// that can be reset without knowing it.
	for _, part := range []string{palette, "\x1b[0 q", "\x1b]8;;\x07", "\x1b[0\"q"} {
		if !strings.Contains(ResetSequence, part) {
			t.Errorf("ResetSequence lacks %q", part)
		}
	}
}
