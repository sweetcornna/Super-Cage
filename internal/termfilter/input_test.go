package termfilter

import "testing"

func TestNextInput(t *testing.T) {
	for _, row := range []struct {
		in    string
		n     int
		kind  InputKind
		reply Reply
	}{
		{"ls -l\r", 6, InputKey, ReplyNone},
		{"\x1b", 1, InputKey, ReplyNone},
		{"\x1b[A", 3, InputKey, ReplyNone},
		{"\x1bOA", 3, InputKey, ReplyNone},
		{"\x1b[200~x", 6, InputKey, ReplyNone},
		{"\x1b[I", 3, InputFocusIn, ReplyNone},
		{"\x1b[O", 3, InputFocusOut, ReplyNone},
		{"\x1b[<0;12;5M", 10, InputMouse, ReplyNone},
		{"\x1b[<35;1;1m", 10, InputMouse, ReplyNone},
		{"\x1b[M #!", 6, InputMouse, ReplyNone},
		{"\x1b[32;10;5M", 10, InputMouse, ReplyNone},
		{"\x1b[?62;22c", 9, InputReply, ReplyDA1},
		{"\x1b[?1;2c", 7, InputReply, ReplyDA1},
		{"\x1b[>0;276;0c", 11, InputReply, ReplyDA2},
		{"\x1b[0n", 4, InputReply, ReplyStatus},
		{"\x1b[24;80R", 8, InputReply, ReplyCursor},
		{"\x1b[?24;80;1R", 11, InputReply, ReplyCursor},
		{"\x1b[?2026;2$y", 11, InputReply, ReplyMode},
		{"\x1b[4;2$y", 7, InputReply, ReplyMode},
		{"\x1bP1$r0m\x1b\\", 9, InputReply, ReplySetting},
		{"\x1bP1$r1;30r\x1b\\", 12, InputReply, ReplySetting},
		{"\x1b]11;rgb:1e1e/1e1e/1e1e\x07", 24, InputReply, ReplyColor},
		{"\x1b]4;1;rgb:cd/00/00\x1b\\", 20, InputReply, ReplyColor},
		// Typed keys that start like an answer stay keys.
		{"\x1bP", 2, InputKey, ReplyNone},         // Alt+P
		{"\x1bPhello", 2, InputKey, ReplyNone},    // Alt+P, then typing
		{"\x1b]", 2, InputKey, ReplyNone},         // Alt+]
		{"\x1b]ls\r", 2, InputKey, ReplyNone},     // Esc then ] typed fast
		{"\x1b_", 2, InputKey, ReplyNone},         // Alt+_
		{"\x1b]11;?\x07", 2, InputKey, ReplyNone}, // a query is not an answer
		{"\x1b[1;5A", 6, InputKey, ReplyNone},     // Ctrl+Up
		{"\x1b[15~", 5, InputKey, ReplyNone},      // F5
		{"\x1b[c", 3, InputKey, ReplyNone},
		{"\x1b[?c", 4, InputKey, ReplyNone},
		{"\x1b[5n", 4, InputKey, ReplyNone},
		{"\x1b[?999999c", 10, InputKey, ReplyNone},
		{"\x1b[24", 4, InputKey, ReplyNone}, // unfinished
	} {
		n, kind, reply := NextInput([]byte(row.in))
		if n != row.n || kind != row.kind || reply != row.reply {
			t.Errorf("%q: %d %d %d, want %d %d %d", row.in, n, kind, reply, row.n, row.kind, row.reply)
		}
	}
}

func FuzzNextInput(f *testing.F) {
	f.Add([]byte("\x1b[?62;22c\x1b]11;rgb:0/0/0\x07abc"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for len(data) > 0 {
			n, _, _ := NextInput(data)
			if n < 1 || n > len(data) {
				t.Fatalf("%q: n=%d", data, n)
			}
			data = data[n:]
		}
	})
}
