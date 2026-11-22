package attach

import (
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// Review round 4, finding 2: after a reconnect cut a paste, only the rest of
// that paste is dropped: to its end marker or to the paste's bound,
// whichever comes first. Typing after it passes, also when the reconnect
// restarted the window (its reset turns bracketed paste off).
func TestReconnectDropsOnlyThePasteItCut(t *testing.T) {
	cut := func() *attacher {
		a := &attacher{filter: termfilter.New(termfilter.Options{})}
		a.input.PasteOn = a.filter.BracketedPaste
		a.filter.Append(nil, []byte("\x1b[?2004h"))
		a.noteInput([]byte("\x1b[200~abc"))
		a.dropPaste = a.inPaste // as reconnect sets it
		return a
	}
	a := cut()
	if out := string(a.noteInput([]byte("def\x1b[201~typed"))); out != "typed" || a.dropPaste {
		t.Fatalf("to the end marker: %q, still dropping %v", out, a.dropPaste)
	}
	a = cut()
	a.filter.Clear(nil)
	if out := string(a.noteInput([]byte("echo x\r\x1b[201~typed"))); out != "typed" || a.dropPaste {
		t.Fatalf("after the window restarted: %q, still dropping %v", out, a.dropPaste)
	}
	a = cut()
	if out := string(a.noteInput([]byte(strings.Repeat("x", 5<<20) + "typed"))); out != strings.Repeat("x", 5<<20-(4<<20-3))+"typed" || a.dropPaste {
		t.Fatalf("past the paste's bound (4 MiB): %d bytes, still dropping %v", len(out), a.dropPaste)
	}
	// Without bracketed paste on, ESC [200~ starts nothing to drop.
	a = &attacher{filter: termfilter.New(termfilter.Options{})}
	a.input.PasteOn = a.filter.BracketedPaste
	if a.noteInput([]byte("\x1b[200~abc")); a.inPaste {
		t.Fatal("a paste with bracketed paste off")
	}
}
