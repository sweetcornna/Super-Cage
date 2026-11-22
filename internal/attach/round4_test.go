package attach

import (
	"bytes"
	"testing"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// --keep-scrollback, by hand only, keeps the program from erasing the
// window's scrollback; by default it may, as over ssh. A restart of the
// history clears like a full reset does.
func TestKeepScrollbackIsAnOptIn(t *testing.T) {
	if cfg, err := ParseArgs([]string{"--keep-scrollback", "--terminal", "0123456789abcdef"}); err != nil || !cfg.KeepScrollback {
		t.Fatal(cfg, err)
	}
	if cfg, err := ParseArgs([]string{"--terminal", "0123456789abcdef"}); err != nil || cfg.KeepScrollback {
		t.Fatal("kept by default", cfg, err)
	}
	for _, keep := range []bool{false, true} {
		var out bytes.Buffer
		a := &attacher{filter: termfilter.New(termfilter.Options{KeepScrollback: keep}), tty: TTY{Out: &out}}
		if err := a.restart(); err != nil {
			t.Fatal(err)
		}
		want := termfilter.ClearReset + "\x1b[3J" + focusOn
		if keep {
			want = termfilter.ClearReset + focusOn
		}
		if out.String() != want {
			t.Errorf("restart (keep %v): %q", keep, out.String())
		}
	}
}
