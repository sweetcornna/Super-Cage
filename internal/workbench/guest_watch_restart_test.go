package workbench

import (
	"context"
	"testing"
)

// A watch stopped by the last held list can still be on its way to send a
// request when the next held list starts a new watch: what it records must
// not count for the new one, or held lists skip their own look while
// nothing answers.
func TestAStoppedWatchDoesNotSpeakForTheNextOne(t *testing.T) {
	s := testServer(t)
	d := &s.cliDiscovery
	ended, stop := context.WithCancel(context.Background())
	stop()
	d.mu.Lock()
	d.stopWatch = func() {} // the next watch runs
	d.mu.Unlock()
	d.setWatching(ended, true)
	d.mu.Lock()
	watching := d.watching
	d.mu.Unlock()
	if watching {
		t.Fatal("a stopped watch marked the next one as answering")
	}
}
