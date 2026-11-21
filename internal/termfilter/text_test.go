package termfilter

import "testing"

func TestControlsAndPlain(t *testing.T) {
	for _, row := range []struct {
		in, plain string
		controls  bool
	}{
		{in: "项目 一", plain: "项目 一"},
		{in: "a\x1b[2Jb", plain: "a [2Jb", controls: true},
		{in: "a\u009b31mb", plain: "a 31mb", controls: true},
		{in: "a\u0085b\x7f", plain: "a b ", controls: true},
		{in: "a\xffb", plain: "a�b", controls: true},
		{in: "two\nlines\r", plain: "two\nlines ", controls: true},
	} {
		if got := Controls(row.in); got != row.controls {
			t.Errorf("Controls(%q) = %v", row.in, got)
		}
		if got := Plain(row.in); got != row.plain {
			t.Errorf("Plain(%q) = %q, want %q", row.in, got, row.plain)
		}
	}
}
