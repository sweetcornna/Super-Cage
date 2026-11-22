package attach

import (
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// Review round 4, finding 2: after a reconnect cut a paste, only the rest of
// that paste is dropped: to its end marker, until bracketed paste goes off,
// or to the paste's bound, whichever comes first. Typing after it passes.
func TestReconnectDropsOnlyThePasteItCut(t *testing.T) {
	on := true
	cut := func() *attacher {
		a := &attacher{filter: termfilter.New(termfilter.Options{})}
		a.input.PasteOn = func() bool { return on }
		a.noteInput([]byte("\x1b[200~abc"))
		a.dropPaste = a.inPaste // as reconnect sets it
		return a
	}
	a := cut()
	if out := string(a.noteInput([]byte("def\x1b[201~typed"))); out != "typed" || a.dropPaste {
		t.Fatalf("to the end marker: %q, still dropping %v", out, a.dropPaste)
	}
	a = cut()
	on = false
	if out := string(a.noteInput([]byte("typed"))); out != "typed" || a.dropPaste {
		t.Fatalf("bracketed paste turned off: %q, still dropping %v", out, a.dropPaste)
	}
	on = true
	a = cut()
	if out := string(a.noteInput([]byte(strings.Repeat("x", 5<<20) + "typed"))); out != strings.Repeat("x", 5<<20-(4<<20-3))+"typed" {
		t.Fatalf("past the bound: %d bytes, still dropping %v", len(out), a.dropPaste)
	}
}
