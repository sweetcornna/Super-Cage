package workbench

import (
	"context"
	"testing"
	"time"
)

func TestClosingAHiddenStreamTellsTheGuestAtOnce(t *testing.T) {
	var demand displayViewDemand
	type change struct {
		show bool
		held int
	}
	changes := make(chan change, 8)
	update := func(show bool) error {
		changes <- change{show, demand.heldCount()}
		return nil
	}
	next := func(within time.Duration) (change, bool) {
		select {
		case c := <-changes:
			return c, true
		case <-time.After(within):
			return change{}, false
		}
	}
	viewer := demand.join(context.Background(), update, nil)
	if c, ok := next(time.Second); !ok || !c.show {
		t.Fatalf("joining showed nothing: %+v %v", c, ok)
	}
	// The pane is hidden: after the usual delay the guest hides the window and
	// learns that one stream stays open.
	viewer.setVisible(false)
	if c, ok := next(2 * time.Second); !ok || c.show || c.held != 1 {
		t.Fatalf("hiding the pane: %+v %v", c, ok)
	}
	// The hidden stream closes. The guest must not keep counting it, or it
	// takes a legacy viewer for it and hides the window under that viewer.
	viewer.close()
	if c, ok := next(300 * time.Millisecond); !ok || c.show || c.held != 0 {
		t.Fatalf("closing the hidden stream told the guest %+v (sent %v); want a hide with no held streams at once", c, ok)
	}
}
