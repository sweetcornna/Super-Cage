package workbench

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/localterm"
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
// its rest, a lone Esc is passed on when the wait for its rest is over, and
// neither is the owner at the window. The test ends each wait itself.
func TestEntryReadInputHoldsCutSequences(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	p := &entryPresence{}
	armed := make(chan chan time.Time, 16)
	hold := func() (<-chan time.Time, func() bool) {
		fired := make(chan time.Time, 1)
		armed <- fired
		return fired, func() bool { return true }
	}
	sends := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		entryReadInputHeld(r, p, func() bool { return false }, func(data []byte) bool {
			if len(data) > 0 {
				sends <- string(data)
			}
			return string(data) != "q"
		}, hold)
	}()
	next := func() string {
		t.Helper()
		select {
		case s := <-sends:
			return s
		case <-time.After(10 * time.Second):
			t.Fatal("nothing sent")
			return ""
		}
	}
	wait := func() chan time.Time {
		t.Helper()
		select {
		case fired := <-armed:
			return fired
		case <-time.After(10 * time.Second):
			t.Fatal("no wait armed")
			return nil
		}
	}
	w.Write([]byte("\x1b[12;1"))
	wait() // never ends: the rest comes first
	w.Write([]byte("45R"))
	if got := next(); got != "\x1b[12;145R" || p.lastInput.Load() != 0 {
		t.Fatalf("%q, owner %v", got, p.lastInput.Load() != 0)
	}
	w.Write([]byte("\x1b"))
	wait() <- time.Now()
	if got := next(); got != "\x1b" || p.lastInput.Load() != 0 {
		t.Fatalf("%q, owner %v", got, p.lastInput.Load() != 0)
	}
	w.Write([]byte("a"))
	if got := next(); got != "a" || p.lastInput.Load() == 0 {
		t.Fatalf("%q, owner %v", got, p.lastInput.Load() != 0)
	}
	w.Write([]byte("q"))
	next()
	<-done
	if len(armed) != 0 {
		t.Fatalf("%d waits armed with nothing held", len(armed))
	}
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
	never := func() (<-chan time.Time, func() bool) { return nil, func() bool { return true } }
	entryReadInputHeld(&entryScript{parts: []string{"\x1b[12;1"}}, p, func() bool { return false }, func(data []byte) bool {
		if len(data) > 0 {
			sent = append(sent, string(data))
		}
		return true
	}, never)
	if strings.Join(sent, "") != "\x1b[12;1" || p.lastInput.Load() != 0 {
		t.Fatalf("sent %q, owner %v", sent, p.lastInput.Load() != 0)
	}
}

// The ssh window reads ESC [200~ as a paste start only while its filter has
// passed bracketed paste on to the window (review round 4, finding 2).
func TestEntryWindowPasteFollowsBracketedPaste(t *testing.T) {
	presence, output := entryWindow(localterm.DefaultSettings())
	presence.note([]byte("\x1b[200~abc"), output.filter.GuestFocus)
	if presence.input.InPaste() {
		t.Fatal("a paste with bracketed paste off")
	}
	output.filter.Append(nil, []byte("\x1b[?2004h"))
	presence.note([]byte("\x1b[200~abc"), output.filter.GuestFocus)
	if !presence.input.InPaste() {
		t.Fatal("no paste with bracketed paste on")
	}
	output.filter.Append(nil, []byte("\x1b[?2004l"))
	presence.note([]byte("def"), output.filter.GuestFocus)
	if presence.input.InPaste() {
		t.Fatal("still a paste after bracketed paste went off")
	}
}
