package workbench

import (
	"context"
	_ "embed"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

//go:embed linux-desktop-visibility.py
var linuxDesktopVisibility string

// A controller owns all computer viewers and the paired phone's display lease.
// Serialize guest changes so a late hide completes before a new viewer resumes.
type displayViewDemand struct {
	mu           sync.Mutex
	op           sync.Mutex
	viewers      int
	held         int    // open streams whose pane is hidden; guarded by mu
	joined       uint64 // every stream that became shown, ever; guarded by mu
	idle         *time.Timer
	known, shown bool                    // guarded by op
	sentHeld     int                     // held streams the guest was told about with its last hide; guarded by op
	settling     bool                    // a background worker is applying demand; guarded by mu
	again        bool                    // demand changed while that worker ran; guarded by mu
	retryDelay   func(int) time.Duration // tests only; guarded by mu
	admit        func() bool             // retries wait while it says no; guarded by mu
}

// errDisplayRetryWaits is a retry that waited for the guest to admit it.
var errDisplayRetryWaits = errors.New("the guest is not ready for display changes")

// retryWhen makes failed changes wait, without asking the guest, while admit
// says no: a retry is a lock-free guest call, unlike the viewer's own change.
func (d *displayViewDemand) retryWhen(admit func() bool) {
	d.mu.Lock()
	d.admit = admit
	d.mu.Unlock()
}

func (d *displayViewDemand) apply(update func(bool) error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.mu.Lock()
	show, held := d.viewers > 0, d.held
	d.mu.Unlock()
	if show {
		held = 0
	}
	if d.known && d.shown == show && d.sentHeld == held {
		return nil
	}
	if err := update(show); err != nil {
		d.known = false
		return err
	}
	d.known, d.shown, d.sentHeld = true, show, held
	return nil
}

func (d *displayViewDemand) background(update func(bool) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.viewers != 0 {
		return
	}
	if d.idle != nil {
		d.idle.Stop()
	}
	d.idle = time.AfterFunc(500*time.Millisecond, func() { _ = d.apply(update) })
}

func (d *displayViewDemand) acquire(update func(bool) error) (func(), error) {
	d.mu.Lock()
	d.viewers++
	d.joined++
	if d.idle != nil {
		d.idle.Stop()
		d.idle = nil
	}
	d.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { d.mu.Lock(); d.viewers--; d.mu.Unlock(); d.background(update) }) }
	if err := d.apply(update); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// displayRetryDelay is the wait before the n-th retry (n >= 1) of a guest
// display change that failed.
func displayRetryDelay(n int) time.Duration {
	return min(time.Duration(n)*time.Second, 5*time.Second)
}

// settle brings the guest in line with demand in the background, so a viewer
// never waits for the guest and a failed change never refuses it. Calls
// coalesce into one worker that applies the latest demand. A failed change is
// reported once per streak and retried, at most every five seconds, for as
// long as some viewer still wants the display shown: a client that connects
// while the guest is unavailable must not be left looking at a hidden window
// when the guest returns. While admit says no (a job owns the guest) retries
// do not reach the guest. A failed hide is left to the idle timer, as before.
func (d *displayViewDemand) settle(ctx context.Context, update func(bool) error, failed func(error)) {
	d.mu.Lock()
	d.again = true
	if d.settling {
		d.mu.Unlock()
		return
	}
	d.settling = true
	d.mu.Unlock()
	go func() {
		failures := 0
		for {
			d.mu.Lock()
			d.again = false
			admit := d.admit
			d.mu.Unlock()
			var err error
			if failures > 0 && admit != nil && !admit() {
				err = errDisplayRetryWaits
			} else {
				err = d.apply(update)
			}
			if err == nil {
				failures = 0
			} else {
				if failures == 0 && failed != nil {
					failed(err)
				}
				failures++
			}
			d.mu.Lock()
			retry := err != nil && d.viewers > 0 && ctx.Err() == nil
			if !retry && (!d.again || ctx.Err() != nil) {
				d.settling = false
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
			if retry {
				d.mu.Lock()
				delay := displayRetryDelay
				if d.retryDelay != nil {
					delay = d.retryDelay
				}
				d.mu.Unlock()
				timer := time.NewTimer(delay(failures))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
				// A viewer that left meanwhile is left to the idle timer.
				d.mu.Lock()
				if d.viewers == 0 && !d.again {
					d.settling = false
					d.mu.Unlock()
					return
				}
				d.mu.Unlock()
			}
		}
	}()
}

func (d *displayViewDemand) heldCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.held
}

// A displayViewer is one open display stream's claim on a guest display. A
// stream whose pane is on screen demands the display. A stream whose pane is
// hidden stays open but demands nothing, so the guest can pause the display
// while the client keeps its connection for the next visit. The zero count of
// shown viewers hides the display after the usual short delay.
type displayViewer struct {
	demand  *displayViewDemand
	ctx     context.Context
	update  func(bool) error
	failed  func(error)
	mu      sync.Mutex
	visible bool
	closed  bool
}

// join registers a shown stream. The guest is brought in line in the
// background: see settle.
func (d *displayViewDemand) join(ctx context.Context, update func(bool) error, failed func(error)) *displayViewer {
	d.mu.Lock()
	d.viewers++
	d.joined++
	if d.idle != nil {
		d.idle.Stop()
		d.idle = nil
	}
	d.mu.Unlock()
	d.settle(ctx, update, failed)
	return &displayViewer{demand: d, ctx: ctx, update: update, failed: failed, visible: true}
}

// setVisible moves the stream between shown and hidden. It is safe to call
// concurrently and repeatedly; a closed viewer ignores it.
func (v *displayViewer) setVisible(visible bool) {
	if v == nil {
		return
	}
	v.mu.Lock()
	if v.closed || v.visible == visible {
		v.mu.Unlock()
		return
	}
	v.visible = visible
	v.mu.Unlock()
	d := v.demand
	d.mu.Lock()
	if visible {
		d.viewers++
		d.joined++
		d.held--
		if d.idle != nil {
			d.idle.Stop()
			d.idle = nil
		}
	} else {
		d.viewers--
		d.held++
	}
	d.mu.Unlock()
	if visible {
		d.settle(v.ctx, v.update, v.failed)
	} else {
		d.background(v.update)
	}
}

func (v *displayViewer) isVisible() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.visible
}

func (v *displayViewer) close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	visible := v.visible
	v.mu.Unlock()
	d := v.demand
	d.mu.Lock()
	if visible {
		d.viewers--
	} else {
		d.held--
	}
	d.mu.Unlock()
	if visible {
		d.background(v.update)
	} else {
		d.heldClosed(v.update)
	}
}

// heldClosed tells the guest that one fewer hidden stream is held. While the
// window is hidden the guest agent counts held streams against the viewers it
// sees connected, so a stale count makes it take a legacy viewer for the
// closed stream and hide the window under it. The agent checks every second,
// so a hidden window learns the lower count at once; otherwise the usual
// short delay applies.
func (d *displayViewDemand) heldClosed(update func(bool) error) {
	go func() {
		d.op.Lock()
		hidden := d.known && !d.shown
		d.op.Unlock()
		if hidden {
			_ = d.apply(update)
			return
		}
		d.background(update)
	}()
}

func (d *displayViewDemand) reset() {
	d.op.Lock()
	d.known = false
	d.op.Unlock()
}

func (s *Server) updateDesktopVisibility(show bool) error {
	// Linux uses a dedicated virtual X display. Native Windows/macOS capture
	// already follows viewer lifetime and retains its ordinary guest windows.
	if s.windowsGuest != nil || s.macosGuest != nil || !s.vmRunning() {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 5*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	script := "/usr/bin/python3 - " + strconv.FormatBool(show) + " <<'CAGE_DESKTOP_VISIBILITY'\n" + linuxDesktopVisibility + "\nCAGE_DESKTOP_VISIBILITY\n"
	_, err := s.guestQuick(ctx, script)
	if err != nil && ctx.Err() == nil {
		_ = diagnostics.New(s.profile).Record("warn", "desktop-visibility", "应用窗口显示状态未能同步："+err.Error())
	}
	return err
}

func (s *Server) acquireDesktopView() (func(), error) {
	if s.windowsGuest != nil || s.macosGuest != nil {
		return func() {}, nil
	}
	return s.desktopViews.acquire(s.changeDesktopVisibility)
}

// joinBrowserView and joinDesktopView are acquire for a display stream that
// must not wait for, or fail with, the guest's window state: the picture
// connects at once and the guest follows in the background. The phone path
// keeps acquire, whose caller needs the display restored before it goes on.
func (s *Server) joinBrowserView() *displayViewer {
	return s.browserViews.join(s.lifetimeContext(), s.changeBrowserVisibility, func(err error) {
		diagnostics.Log("warn", "display-visibility", "浏览器窗口显示状态未能同步，画面已照常连接并会重试："+err.Error())
	})
}

func (s *Server) joinDesktopView() *displayViewer {
	if s.windowsGuest != nil || s.macosGuest != nil {
		return nil
	}
	s.desktopViews.retryWhen(s.guestAdmitsLockFreeCalls)
	return s.desktopViews.join(s.lifetimeContext(), s.changeDesktopVisibility, nil)
}

func (s *Server) changeDesktopVisibility(show bool) error {
	if s.desktopDisplayUpdate != nil {
		return s.desktopDisplayUpdate(show)
	}
	return s.updateDesktopVisibility(show)
}

func (s *Server) changeBrowserVisibility(show bool) error {
	if s.macosGuest != nil || s.windowsGuest != nil {
		return nil
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 5*time.Second)
	defer cancel()
	body := `{"visible":` + strconv.FormatBool(show)
	// Streams a client keeps open while its pane is hidden look like another
	// viewer to the guest. An agent that knows "held" does not count them when
	// it decides whether the window may be hidden; an older one ignores it.
	if held := s.browserViews.heldCount(); !show && held > 0 {
		body += `,"held":` + strconv.Itoa(held)
	}
	_, err := s.guestAPI(ctx, token, "POST", "/visibility", []byte(body+"}"))
	return err
}
