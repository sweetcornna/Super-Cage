package workbench

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func shortDisplayRetry(demand *displayViewDemand) {
	demand.mu.Lock()
	demand.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	demand.mu.Unlock()
}

func receive[T any](t *testing.T, channel <-chan T, within time.Duration, what string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(within):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// A viewer that joins returns at once, even while the guest is slow, and the
// guest is brought in line afterwards. acquire waits; join must not.
func TestJoinedViewerNeverWaitsForTheGuest(t *testing.T) {
	var demand displayViewDemand
	release, entered := make(chan struct{}), make(chan bool, 4)
	update := func(show bool) error {
		entered <- show
		<-release
		return nil
	}
	joined := make(chan *displayViewer, 1)
	go func() { joined <- demand.join(t.Context(), update, nil) }()
	viewer := receive(t, joined, time.Second, "join to return while the guest is blocked")
	if !receive(t, entered, time.Second, "the show to start") {
		t.Fatal("the guest was told to hide")
	}
	close(release)
	viewer.close()
	if show := receive(t, entered, 3*time.Second, "the hide"); show {
		t.Fatal("the last viewer left the guest shown")
	}
}

// A failed guest change must not refuse the viewer: it is reported once, then
// retried while the viewer is there.
func TestJoinedViewerRetriesAFailedChangeAndReportsOnce(t *testing.T) {
	var demand displayViewDemand
	shortDisplayRetry(&demand)
	var attempts, reports atomic.Int32
	shown := make(chan struct{})
	update := func(show bool) error {
		if !show {
			return nil
		}
		if attempts.Add(1) < 3 {
			return errors.New("guest unavailable")
		}
		close(shown)
		return nil
	}
	viewer := demand.join(t.Context(), update, func(error) { reports.Add(1) })
	defer viewer.close()
	receive(t, shown, 3*time.Second, "the retried show")
	if reports.Load() != 1 {
		t.Fatalf("failure reported %d times, want once per streak", reports.Load())
	}
}

func TestJoinedViewerStopsRetryingWhenItLeaves(t *testing.T) {
	var demand displayViewDemand
	shortDisplayRetry(&demand)
	var attempts atomic.Int32
	failing := make(chan struct{}, 64)
	update := func(show bool) error {
		if show {
			attempts.Add(1)
			failing <- struct{}{}
			return errors.New("guest unavailable")
		}
		return nil
	}
	viewer := demand.join(t.Context(), update, nil)
	receive(t, failing, time.Second, "the first attempt")
	viewer.close()
	time.Sleep(200 * time.Millisecond)
	settled := attempts.Load()
	time.Sleep(200 * time.Millisecond)
	if attempts.Load() != settled {
		t.Fatalf("kept retrying for a viewer that left: %d attempts, %d after the wait", settled, attempts.Load())
	}
}

// The guest can be away for a long time: a viewer that connected meanwhile is
// shown the window as soon as the guest answers, with no action of its own.
func TestJoinedViewerRecoversAfterALongOutage(t *testing.T) {
	var demand displayViewDemand
	shortDisplayRetry(&demand)
	var attempts atomic.Int32
	shown := make(chan struct{})
	update := func(show bool) error {
		if !show {
			return nil
		}
		if attempts.Add(1) <= 40 {
			return errors.New("guest unavailable")
		}
		close(shown)
		return nil
	}
	viewer := demand.join(t.Context(), update, nil)
	defer viewer.close()
	receive(t, shown, 5*time.Second, "the show after 40 failed attempts")
}

// Ordering of guest changes is the one thing acquire promised that join keeps:
// a new viewer's show waits for the hide of the viewer before it.
func TestJoinedViewerShowFollowsPendingHide(t *testing.T) {
	var demand displayViewDemand
	var mu sync.Mutex
	var log []bool
	hiding, finishHide := make(chan struct{}), make(chan struct{})
	update := func(show bool) error {
		mu.Lock()
		log = append(log, show)
		mu.Unlock()
		if !show {
			select {
			case <-hiding:
			default:
				close(hiding)
				<-finishHide
			}
		}
		return nil
	}
	first := demand.join(t.Context(), update, nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(log)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	first.close()
	receive(t, hiding, 3*time.Second, "the hide to start")
	second := demand.join(t.Context(), update, nil)
	defer second.close()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(log) != 2 {
		mu.Unlock()
		t.Fatalf("the show ran before the hide finished: %v", log)
	}
	mu.Unlock()
	close(finishHide)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(log)
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(log) != 3 || !log[2] {
		t.Fatalf("the show did not follow the hide: %v", log)
	}
}

// A hidden stream stays open but demands nothing; showing it demands again.
func TestHiddenViewerReleasesTheDisplayAndShowingRestoresIt(t *testing.T) {
	var demand displayViewDemand
	changes := make(chan bool, 8)
	update := func(show bool) error { changes <- show; return nil }
	viewer := demand.join(t.Context(), update, nil)
	if !receive(t, changes, time.Second, "the first show") {
		t.Fatal("not shown")
	}
	viewer.setVisible(false)
	if demand.heldCount() != 1 {
		t.Fatalf("held streams: %d", demand.heldCount())
	}
	if show := receive(t, changes, 3*time.Second, "the hide"); show {
		t.Fatal("a hidden stream kept the guest shown")
	}
	viewer.setVisible(false) // repeated messages change nothing
	viewer.setVisible(true)
	if demand.heldCount() != 0 {
		t.Fatalf("held streams: %d", demand.heldCount())
	}
	if show := receive(t, changes, 3*time.Second, "the show on return"); !show {
		t.Fatal("returning did not show the guest")
	}
	// Closing a hidden stream leaves the guest hidden; it only repeats the
	// hide, with one fewer held stream (see heldClosed).
	viewer.setVisible(false)
	receive(t, changes, 3*time.Second, "the second hide")
	viewer.close()
	viewer.setVisible(true) // a closed viewer ignores it
	if show := receive(t, changes, time.Second, "the hide with the lower held count"); show {
		t.Fatal("closing a hidden stream showed the guest")
	}
	select {
	case show := <-changes:
		t.Fatalf("closing a hidden stream changed the guest again, to %t", show)
	case <-time.After(700 * time.Millisecond):
	}
	if demand.heldCount() != 0 {
		t.Fatalf("closed stream still held: %d", demand.heldCount())
	}
}

// One stream hidden and another shown: the display stays shown.
func TestHiddenViewerDoesNotHideAnotherViewersDisplay(t *testing.T) {
	var demand displayViewDemand
	changes := make(chan bool, 8)
	update := func(show bool) error { changes <- show; return nil }
	first := demand.join(t.Context(), update, nil)
	defer first.close()
	receive(t, changes, time.Second, "the first show")
	second := demand.join(t.Context(), update, nil)
	defer second.close()
	first.setVisible(false)
	select {
	case show := <-changes:
		t.Fatalf("a hidden stream changed another viewer's display to %t", show)
	case <-time.After(800 * time.Millisecond):
	}
	second.setVisible(false)
	if show := receive(t, changes, 3*time.Second, "the hide"); show {
		t.Fatal("both streams hidden but the guest stayed shown")
	}
	if demand.heldCount() != 2 {
		t.Fatalf("held streams: %d", demand.heldCount())
	}
}
