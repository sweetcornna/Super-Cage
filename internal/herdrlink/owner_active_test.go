package herdrlink

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// OwnerActive hears of what the owner does, never of flow control,
// heartbeats or what Herdr sends by itself: the cloud entry's idle rule
// rests on it (design 5.2).
func TestOwnerActiveIsTheOwnersOwnActivity(t *testing.T) {
	var active atomic.Int32
	r := framedRig(t, Options{Window: time.Second, OwnerActive: func() { active.Add(1) }})
	if got := active.Load(); got != 1 {
		t.Fatalf("attaching a pane: %d", got)
	}
	step := func(frame []byte, want int32, what string) {
		t.Helper()
		before := len(r.toGuest.bytes())
		r.owner(frame)
		await(t, what, r.toGuest, func(b []byte) bool { return len(b) >= before+len(frame) })
		if got := active.Load(); got != want {
			t.Fatalf("%s: %d, want %d", what, got, want)
		}
	}
	credit := make([]byte, 4)
	binary.LittleEndian.PutUint32(credit, 1024)
	step(Frame(3, KindCredit, credit), 1, "credit")
	step(call(9, "ping", "{}"), 1, "a channel-0 call that attaches nothing (a heartbeat)")
	step(call(10, "events.subscribe", "{}"), 1, "a subscription")
	step(Frame(3, KindFacts, []byte("{}")), 1, "facts")
	step(Frame(3, KindAutomaticInput, []byte("\x1b[1;1R")), 1, "automatic input")
	step(Frame(3, KindData, []byte("\x1b[1;1R")), 1, "a terminal's reply sent as data")
	step(Frame(1, KindData, []byte("x")), 1, "data on the events stream")
	step(Frame(3, KindFocus, []byte{0}), 1, "focus out")
	step(Frame(3, KindData, []byte("a")), 2, "a key")
	step(Frame(3, KindFocus, []byte{1}), 3, "focus in")
	step(Frame(3, KindResize, []byte{80, 0, 24, 0}), 4, "a resize")
	step(Frame(9, KindData, []byte("a")), 4, "data on a channel no reply announced")
}
