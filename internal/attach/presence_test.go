package attach

import (
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// The review's case (review3-ssh R3-1): the program asks for 5000 cursor
// answers; they come back in 32 KiB reads, cut anywhere. None of it is the
// owner at the window, and the bytes pass on unchanged.
func TestNoteInputAnswersCutByReadsAreNotTheOwner(t *testing.T) {
	read := func(a *attacher, stream string) string {
		var out []byte
		for start := 0; start < len(stream); start += 32 << 10 {
			out = append(out, a.noteInput([]byte(stream[start:min(start+32<<10, len(stream))]))...)
		}
		return string(out)
	}
	a := &attacher{filter: termfilter.New(termfilter.Options{})}
	stream := strings.Repeat("\x1b[12;145R", 5000)
	if out := read(a, stream); a.lastInput.Load() != 0 || out != stream {
		t.Fatalf("answers cut by reads counted as the owner: %v, bytes kept: %v", a.lastInput.Load() != 0, out == stream)
	}
	// A key typed among them still counts.
	a = &attacher{filter: termfilter.New(termfilter.Options{})}
	stream = strings.Repeat("\x1b[12;145R", 4000) + "a" + strings.Repeat("\x1b[12;145R", 1000)
	if out := read(a, stream); a.lastInput.Load() == 0 || out != stream {
		t.Fatalf("a key among answers not counted: %v, bytes kept: %v", a.lastInput.Load() == 0, out == stream)
	}
}

// A sequence cut at the end of a read waits termfilter.InputHold for its
// rest; then it is passed on as typed, not as the owner at the window, and
// so is its rest when it comes late. Input that came meanwhile goes first.
func TestHeldInputIsReleasedAsTyped(t *testing.T) {
	a := &attacher{filter: termfilter.New(termfilter.Options{})}
	wait := func() {
		t.Helper()
		a.armHold()
		if a.held() == nil {
			t.Fatal("no wait armed")
		}
		<-a.held()
	}
	if out := a.noteInput([]byte("\x1b")); len(out) != 0 {
		t.Fatalf("a lone Esc passed at once: %q", out)
	}
	wait()
	if out := a.afterHold(make(chan []byte)); string(out) != "\x1b" || a.lastInput.Load() != 0 {
		t.Fatalf("released %q, owner %v", out, a.lastInput.Load() != 0)
	}
	a.noteInput([]byte("\x1b[12;1"))
	wait()
	inputs := make(chan []byte, 1)
	inputs <- []byte("45R")
	if out := a.afterHold(inputs); string(out) != "\x1b[12;145R" || a.lastInput.Load() != 0 {
		t.Fatalf("the rest that came meanwhile: %q, owner %v", out, a.lastInput.Load() != 0)
	}
	a.noteInput([]byte("\x1b[12;1"))
	wait()
	out := string(a.afterHold(make(chan []byte))) + string(a.noteInput([]byte("45R")))
	if out != "\x1b[12;145R" || a.lastInput.Load() != 0 {
		t.Fatalf("a late rest: %q, owner %v", out, a.lastInput.Load() != 0)
	}
	if a.armHold(); a.hold != nil {
		t.Fatal("waiting with nothing held")
	}
	if a.noteInput([]byte("a")); a.lastInput.Load() == 0 {
		t.Fatal("a key after it does not count")
	}
}
