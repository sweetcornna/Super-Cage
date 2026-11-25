package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func (s *Server) lifetimeContextLocked() context.Context {
	if s.lifeContext == nil {
		s.lifeContext, s.lifeCancel = context.WithCancel(context.Background())
		if s.closing {
			s.lifeCancel()
		}
	}
	return s.lifeContext
}

func (s *Server) lifetimeContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lifetimeContextLocked()
}

func (s *Server) monitorWakeCh() <-chan struct{} {
	s.mu.Lock()
	if s.monitorWake == nil {
		s.monitorWake = make(chan struct{})
	}
	ch := s.monitorWake
	s.mu.Unlock()
	return ch
}

func (s *Server) wakeMonitorsLocked() {
	if s.monitorWake != nil {
		close(s.monitorWake)
		s.monitorWake = nil
	}
}

func (s *Server) wakeMonitors() {
	s.mu.Lock()
	s.wakeMonitorsLocked()
	s.mu.Unlock()
}

// safetyMonitorWait returns how long the background safety loop should sleep
// before its next check. waitWakeOnly means no timed wakeup is needed until a
// lifecycle or gate event calls wakeMonitors.
func (s *Server) safetyMonitorWait(ranOnce bool) (delay time.Duration, waitWakeOnly bool) {
	s.mu.Lock()
	ready := s.ready
	paused := s.networkPaused
	closing := s.closing
	last := s.safety.VerifiedAt
	retry := s.safety.deferral.RetryAt
	gate := s.safety.Gate
	s.mu.Unlock()
	if closing {
		return 0, true
	}
	busy := s.busy()
	if !ready && !busy {
		return 0, true
	}
	if !ranOnce {
		return 0, false
	}
	if paused || (gate != nil && gate.Mode != cage.GateOpen) {
		return 0, true
	}
	now := time.Now()
	if !last.IsZero() {
		if remaining := last.Add(30 * time.Second).Sub(now); remaining > 5*time.Second {
			return remaining, false
		}
	}
	if retry.After(now) {
		if remaining := retry.Sub(now); remaining > 5*time.Second {
			return remaining, false
		}
	}
	return 5 * time.Second, false
}

func waitMonitor(ctx context.Context, wake <-chan struct{}, timer *time.Timer, delay time.Duration) bool {
	timer.Reset(delay)
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return true
	case <-timer.C:
		return true
	}
}

func (s *Server) startMonitors() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	ctx := s.lifetimeContextLocked()
	s.monitors.Add(4)
	s.mu.Unlock()
	go func() {
		defer s.monitors.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := diagnostics.New(s.profile).Clean(); err != nil {
					diagnostics.Log("error", "diagnostics", "log cleanup failed: "+err.Error())
				}
				// Silent updates share this ticker; manual mode returns at once.
				s.appUpdateTick(ctx)
				// Wake idle monitors hourly so scheduled proxy subscription refreshes still run.
				s.wakeMonitors()
			}
		}
	}()
	go func() {
		defer s.monitors.Done()
		s.monitor(ctx)
	}()
	go func() {
		defer s.monitors.Done()
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		defer timer.Stop()
		ranOnce := false
		for ctx.Err() == nil {
			wake := s.monitorWakeCh()
			delay, waitWakeOnly := s.safetyMonitorWait(ranOnce)
			if waitWakeOnly {
				ranOnce = false
				select {
				case <-ctx.Done():
					return
				case <-wake:
					continue
				}
			}
			if delay > 0 {
				if !waitMonitor(ctx, wake, timer, delay) {
					return
				}
				continue
			}
			// Long-running maintenance jobs own their own monitor and stop it
			// before changing guest components. Other jobs cancel observation.
			if !s.busy() {
				s.monitorSafety()
				ranOnce = true
			}
			wake = s.monitorWakeCh()
			nextDelay, sleepOnly := s.safetyMonitorWait(ranOnce)
			if sleepOnly {
				ranOnce = false
				select {
				case <-ctx.Done():
					return
				case <-wake:
					continue
				}
			}
			if nextDelay < 5*time.Second {
				nextDelay = 5 * time.Second
			}
			if !waitMonitor(ctx, wake, timer, nextDelay) {
				return
			}
		}
	}()
	go func() {
		defer s.monitors.Done()
		// macOS performs both probes from the same tick. Its periodic SSH
		// attestation otherwise races this loop for the lifecycle lock.
		if s.macosGuest == nil && (s.windowsGuest == nil || s.windowsGuest.NetworkPairingFile != "") {
			s.monitorNetwork(ctx)
		}
	}()
}

// beginClose seals workbench admission and cancels service work without waiting.
func (s *Server) beginClose() {
	s.mu.Lock()
	s.closing = true
	s.lifetimeContextLocked()
	s.lifeCancel()
	if s.checkCancel != nil {
		s.checkCancel()
	}
	if s.monitorCancel != nil {
		s.monitorCancel()
	}
	s.mu.Unlock()
}

// Wait outside the operation lock: a cancelled job may still need that lock
// before it can finish and release its resources.
func (s *Server) closeBackground() {
	s.beginClose()
	s.jobs.close()
	s.monitors.Wait()
}

// An activity lasts through the handler's stream and child goroutine cleanup.
// Add and the closing decision share mu, so shutdown can safely wait outside it.
func (s *Server) beginActivity(parent context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, errJobsClosed
	}
	lifetime := s.lifetimeContextLocked()
	s.activities.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(lifetime, cancel)
	return ctx, func() { stop(); cancel(); s.activities.Done() }, nil
}

// closeService releases host connections while retaining the guest VM and its
// disk. It runs outside request handlers and without either workbench lock.
func (s *Server) closeService() error {
	s.closeOnce.Do(func() {
		s.beginClose()
		// Closing PTYs first releases handlers blocked writing terminal input.
		// The manager also rejects a terminal created by an already-admitted call.
		s.terminals.close()
		s.closeBackground()
		s.activities.Wait()
		s.mobileUploads.close()
		s.op.Lock()
		defer s.op.Unlock()
		s.closeMacOSScreenSharing()
		s.closeProjectShares()
		s.closeT3CodeProxy()
		s.closeMagpieProxy()
		s.closeAgents()
		s.closeWindowsBrowser()
		if s.windowsGuest != nil {
			s.closeErr = s.closeProxyGate("控制服务正在退出，外网保持关闭")
		}
		s.closeWindowsNetwork()
		s.mu.Lock()
		browser := s.browserControl
		s.ready = false
		s.credentials = guestCredentials{}
		s.mu.Unlock()
		if browser != nil {
			browser.close()
		}
		s.closeGuestSessions()
		if s.windowsGuest == nil {
			s.closeErr = s.closeProxyGate("控制服务正在退出，外网保持关闭")
		}
		s.closeErr = errors.Join(s.closeErr, managedproxy.Stop(s.profile))
		if s.controlPublished {
			if err := os.Remove(filepath.Join(s.profile, "control.json")); err != nil && !os.IsNotExist(err) {
				s.closeErr = errors.Join(s.closeErr, err)
			}
		}
	})
	return s.closeErr
}

func (s *Server) controlContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.lifetimeContext(), 5*time.Second)
}
