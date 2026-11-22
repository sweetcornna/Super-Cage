package workbench

import (
	"io"
	"time"

	"github.com/sylvinus/agent-vm/internal/localterm"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// entryWindow is an ssh window's presence and output filter: clipboard
// writes follow the owner's presence, and the window's input reads ESC
// [200~ as a paste start only while the filter has passed bracketed paste
// on to it.
func entryWindow(settings localterm.Settings) (*entryPresence, *entryOutput) {
	presence := &entryPresence{}
	output := &entryOutput{filter: termfilter.New(termfilter.Options{ClipboardWrite: settings.ClipboardWrite, ClipboardNow: presence.here})}
	presence.input.PasteOn = output.filter.BracketedPaste
	return presence, output
}

// entryReadInput reads an ssh window's input and hands it to send in whole
// units: entryPresence.note holds an escape sequence cut at the end of a
// read for the next, so an answer to a query cut by a read is still an
// answer, here and in the viewer rules. A held sequence whose rest does not
// come within termfilter.InputHold of the last send is released (a lone Esc
// key), typed but not the owner's presence. It returns when send fails or
// the window's input ends; the reading goroutine ends with the channel.
func entryReadInput(channel io.Reader, presence *entryPresence, guestFocus func() bool, send func([]byte) bool) {
	entryReadInputHeld(channel, presence, guestFocus, send, entryHoldTimer)
}

// entryHold starts the wait for the rest of a held sequence: fired when it
// is over, stop ends it sooner. Tests end it themselves.
type entryHold func() (fired <-chan time.Time, stop func() bool)

func entryHoldTimer() (<-chan time.Time, func() bool) {
	timer := time.NewTimer(termfilter.InputHold)
	return timer.C, timer.Stop
}

func entryReadInputHeld(channel io.Reader, presence *entryPresence, guestFocus func() bool, send func([]byte) bool, hold entryHold) {
	reads := make(chan []byte)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(reads)
		buffer := make([]byte, 32<<10)
		for {
			n, err := channel.Read(buffer)
			if n > 0 {
				select {
				case reads <- append([]byte(nil), buffer[:n]...):
				case <-stop:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var stopHold func() bool
	defer func() {
		if stopHold != nil {
			stopHold()
		}
	}()
	for {
		var held <-chan time.Time
		if presence.input.Held() {
			// Armed after the last send returned: the wait is for the
			// owner's terminal, not for the program reading its input.
			held, stopHold = hold()
		}
		var data []byte
		select {
		case read, ok := <-reads:
			if !ok {
				// The input ended: what was held goes as it is.
				send(presence.release())
				return
			}
			data = presence.note(read, guestFocus)
		case <-held:
			// Input that came meanwhile goes first.
			select {
			case read, ok := <-reads:
				if !ok {
					send(presence.release())
					return
				}
				data = presence.note(read, guestFocus)
			default:
				data = presence.release()
			}
		}
		if stopHold != nil {
			stopHold()
			stopHold = nil
		}
		if !send(data) {
			return
		}
	}
}

// release passes on what note held when its rest did not come in time:
// typed (pasted, inside a paste), but never the owner's presence, since an
// answer cut by a read looks the same.
func (p *entryPresence) release() []byte {
	var out []byte
	p.input.Release(func(unit []byte, _ termfilter.InputKind, _ termfilter.Reply) {
		out = append(out, unit...)
	})
	return out
}
