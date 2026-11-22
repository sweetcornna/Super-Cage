package workbench

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// The review's case (review3-ssh R3-1), for the ssh window: answers to 5000
// queries read 32 KiB at a time are not the owner typing; a key among them
// is.
func TestEntryPresenceAnswersCutByReadsAreNotTheOwner(t *testing.T) {
	read := func(p *entryPresence, stream string) string {
		var out []byte
		for start := 0; start < len(stream); start += 32 << 10 {
			out = append(out, p.note([]byte(stream[start:min(start+32<<10, len(stream))]), func() bool { return false })...)
		}
		return string(out)
	}
	p := &entryPresence{}
	stream := strings.Repeat("\x1b[12;145R", 5000)
	if out := read(p, stream); p.lastInput.Load() != 0 || out != stream {
		t.Fatalf("answers cut by reads counted as the owner: %v, bytes kept: %v", p.lastInput.Load() != 0, out == stream)
	}
	p = &entryPresence{}
	stream = strings.Repeat("\x1b[12;145R", 4000) + "a" + strings.Repeat("\x1b[12;145R", 1000)
	if out := read(p, stream); p.lastInput.Load() == 0 || out != stream {
		t.Fatalf("a key among answers not counted: %v, bytes kept: %v", p.lastInput.Load() == 0, out == stream)
	}
}

// The ssh window's input reaches send in whole units: a cut answer waits for
// its rest, a lone Esc is passed on after termfilter.InputHold, and neither
// is the owner at the window.
func TestEntryReadInputHoldsCutSequences(t *testing.T) {
	r, w := io.Pipe()
	p := &entryPresence{}
	sends := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		entryReadInput(r, p, func() bool { return false }, func(data []byte) bool {
			if len(data) > 0 {
				sends <- string(data)
			}
			return string(data) != "q"
		})
	}()
	next := func() string {
		t.Helper()
		select {
		case s := <-sends:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("nothing sent")
			return ""
		}
	}
	w.Write([]byte("\x1b[12;1"))
	w.Write([]byte("45R"))
	if got := next(); got != "\x1b[12;145R" || p.lastInput.Load() != 0 {
		t.Fatalf("%q, owner %v", got, p.lastInput.Load() != 0)
	}
	start := time.Now()
	w.Write([]byte("\x1b"))
	if got := next(); got != "\x1b" || time.Since(start) < termfilter.InputHold || p.lastInput.Load() != 0 {
		t.Fatalf("%q after %v, owner %v", got, time.Since(start), p.lastInput.Load() != 0)
	}
	w.Write([]byte("a"))
	if got := next(); got != "a" || p.lastInput.Load() == 0 {
		t.Fatalf("%q, owner %v", got, p.lastInput.Load() != 0)
	}
	w.Write([]byte("q"))
	next()
	<-done
	w.Close()
}

// entryScript is a window's input: each Read gives the next part, the last
// with io.EOF.
type entryScript struct{ parts []string }

func (s *entryScript) Read(b []byte) (int, error) {
	if len(s.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(b, s.parts[0])
	if s.parts = s.parts[1:]; len(s.parts) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// When the window's input ends inside a sequence, what was held is still
// sent: bytes read are bytes sent, and it is not the owner at the window.
func TestEntryReadInputEndSendsWhatIsHeld(t *testing.T) {
	p := &entryPresence{}
	var sent []string
	entryReadInput(&entryScript{parts: []string{"\x1b[12;1"}}, p, func() bool { return false }, func(data []byte) bool {
		if len(data) > 0 {
			sent = append(sent, string(data))
		}
		return true
	})
	if strings.Join(sent, "") != "\x1b[12;1" || p.lastInput.Load() != 0 {
		t.Fatalf("sent %q, owner %v", sent, p.lastInput.Load() != 0)
	}
}
