package workbench

import (
	"context"
	"errors"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

type runtimeRestorePart uint8

const (
	restoreRuntimeProxy runtimeRestorePart = 1 << iota
	restoreRuntimeOrca
	restoreRuntimeTools
	restoreRuntimeSessions
	restoreRuntimeExternal
	restoreRuntimeGuestOrca
	restoreRuntimeAll = restoreRuntimeProxy | restoreRuntimeOrca | restoreRuntimeTools | restoreRuntimeSessions | restoreRuntimeExternal | restoreRuntimeGuestOrca
)

// Protected by Server.mu. A paused network or a stop invalidates the pending
// work before the lifecycle operation can wait for s.op.
type workbenchRuntimeRestore struct {
	pending      runtimeRestorePart
	active       runtimeRestorePart
	waitNetwork  runtimeRestorePart
	cancel       context.CancelFunc
	nextProxyTry time.Time
}

func (s *Server) queueWorkbenchRuntimeRestoreLocked(parts runtimeRestorePart) {
	if s.closing {
		return
	}
	s.runtimeRestore.pending |= parts
	s.wakeMonitorsLocked()
}

func (s *Server) requestWorkbenchRuntimeRestore() {
	s.mu.Lock()
	s.queueWorkbenchRuntimeRestoreLocked(restoreRuntimeAll)
	s.mu.Unlock()
}

func (s *Server) cancelWorkbenchRuntimeRestoreLocked() {
	s.runtimeRestore.pending = 0
	s.runtimeRestore.waitNetwork = 0
	if s.runtimeRestore.cancel != nil {
		s.runtimeRestore.cancel()
	}
}

var errRuntimeRestoreWaitNetwork = errors.New("等待手动恢复外网后继续连接")

func (s *Server) pauseWorkbenchRuntimeNetworkLocked() {
	r := &s.runtimeRestore
	if r.cancel != nil {
		// The attempt may already be opening a network connection. Cancel it
		// and keep its work so local steps can be retried even while paused.
		r.pending |= r.active
		r.cancel()
	}
	s.wakeMonitorsLocked()
}

func (s *Server) requestWorkbenchProxyRecovery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.runtimeRestore.pending != 0 || s.runtimeRestore.cancel != nil || now.Before(s.runtimeRestore.nextProxyTry) {
		return
	}
	if !s.networkPaused && s.runtimeRestoreAllowedLocked() {
		s.runtimeRestore.nextProxyTry = now.Add(time.Minute)
		s.queueWorkbenchRuntimeRestoreLocked(restoreRuntimeProxy | restoreRuntimeOrca)
	}
}

func (s *Server) runtimeRestoreAllowedLocked() bool {
	return s.ready && !s.closing && !s.guestUpdateRequired && !s.appUpdateSealed && s.storageRecoveryError == ""
}

func (s *Server) runnableRuntimeRestoreLocked() runtimeRestorePart {
	if !s.runtimeRestoreAllowedLocked() {
		return 0
	}
	parts := s.runtimeRestore.pending
	if s.networkPaused {
		parts &^= restoreRuntimeProxy | s.runtimeRestore.waitNetwork
	}
	return parts
}

// The ordinary monitor wake signal coalesces attach, unlock and resume. An
// inactive or locked profile does no polling for optional services.
func (s *Server) monitorWorkbenchRuntime(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		wake := s.monitorWakeCh()
		s.mu.Lock()
		pending := s.runnableRuntimeRestoreLocked() != 0
		s.mu.Unlock()
		var epoch context.Context
		var unlocked bool
		if pending && s.account != nil {
			epoch, unlocked = s.account.access()
		}
		if !pending || !unlocked || s.macSetupPending() {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				continue
			}
		}
		if !s.runtimeRestoreOperationAvailable() {
			if !waitMonitor(ctx, wake, timer, 500*time.Millisecond) {
				return
			}
			continue
		}
		s.mu.Lock()
		parts := s.runnableRuntimeRestoreLocked()
		if parts == 0 || epoch.Err() != nil {
			s.mu.Unlock()
			continue
		}
		s.runtimeRestore.pending &^= parts
		s.runtimeRestore.active = parts
		restoreCtx, cancel := context.WithCancel(context.WithValue(ctx, accountEpochKey{}, epoch))
		s.runtimeRestore.cancel = cancel
		s.mu.Unlock()
		stopEpoch := context.AfterFunc(epoch, cancel)
		s.retryWorkbenchRuntime(restoreCtx, timer, parts)
		stopEpoch()
		cancel()
		s.mu.Lock()
		s.runtimeRestore.cancel = nil
		s.runtimeRestore.active = 0
		s.mu.Unlock()
	}
}

func (s *Server) runtimeRestoreOperationAvailable() bool {
	if s.busy() || !s.op.TryLock() {
		return false
	}
	s.op.Unlock()
	return true
}

func (s *Server) retryWorkbenchRuntime(ctx context.Context, timer *time.Timer, parts runtimeRestorePart) {
	for attempt := 0; ctx.Err() == nil && parts != 0; {
		s.mu.Lock()
		allowed := s.runtimeRestoreAllowedLocked()
		s.mu.Unlock()
		if !allowed {
			return
		}
		if !s.runtimeRestoreOperationAvailable() {
			if !waitMonitor(ctx, s.monitorWakeCh(), timer, 500*time.Millisecond) {
				return
			}
			continue
		}
		var err error
		parts, err = s.restoreWorkbenchRuntime(ctx, parts)
		attempt++
		if parts == 0 || ctx.Err() != nil {
			return
		}
		if attempt == 5 {
			diagnostics.Log("warn", "runtime-restore", diagnostics.Redact(err.Error()))
			return
		}
		// A health notification does not bypass the backoff for an operation
		// that ran and failed. The parent/epoch still cancel it immediately.
		if !waitMonitor(ctx, nil, timer, time.Duration(1<<(attempt-1))*time.Second) {
			return
		}
	}
}

func (s *Server) restoreWorkbenchRuntime(ctx context.Context, parts runtimeRestorePart) (runtimeRestorePart, error) {
	var retry runtimeRestorePart
	var failure error
	note := func(part runtimeRestorePart, err error) {
		if err == nil {
			return
		}
		if errors.Is(err, errRuntimeRestoreWaitNetwork) {
			s.mu.Lock()
			if s.networkPaused && ctx.Err() == nil {
				s.runtimeRestore.pending |= part
				s.runtimeRestore.waitNetwork |= part
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
		}
		failure = errors.Join(failure, err)
		retry |= part
	}

	// External reconciliation uses the desktop gateway. A slow discovery or
	// offline pairing must not hold native terminal recovery behind it.
	var external <-chan error
	if parts&restoreRuntimeExternal != 0 && ctx.Err() == nil {
		done := make(chan error, 1)
		external = done
		go func() {
			reconcile, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			done <- s.restoreExternalSessions(reconcile)
		}()
	}
	for _, step := range []struct {
		part runtimeRestorePart
		run  func(context.Context) error
	}{
		{restoreRuntimeTools, s.restoreWorkbenchToolServices},
		{restoreRuntimeSessions, s.restoreWorkbenchSessions},
		{restoreRuntimeProxy, s.restoreAppliedProxy},
		{restoreRuntimeOrca, s.restoreOrcaConnection},
		{restoreRuntimeGuestOrca, s.restoreGuestOrcaSessions},
	} {
		if ctx.Err() != nil {
			break
		}
		if parts&step.part != 0 {
			note(step.part, step.run(ctx))
		}
	}
	if external != nil {
		note(restoreRuntimeExternal, <-external)
	}
	return retry, failure
}

func (s *Server) restoreAppliedProxy(ctx context.Context) error {
	s.mu.Lock()
	paused := s.networkPaused
	s.mu.Unlock()
	if paused {
		return errRuntimeRestoreWaitNetwork
	}
	if s.windowsGuest != nil && s.windowsGuest.NetworkPairingFile == "" {
		return nil
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	p, err := managedproxy.Applied(s.profile, cfg.ProxyURL, cfg.ProxyRevision)
	if err != nil {
		return nil // An absent, removed or unapplied node is not restore intent.
	}
	probe, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
	running := proxyCoreRunning(probe, s.profile, p)
	cancelProbe()
	if running {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	job, err := s.submitJob("proxy-network", func() error {
		s.mu.Lock()
		allowed := s.runtimeRestoreAllowedLocked() && !s.networkPaused
		s.mu.Unlock()
		if !allowed || ctx.Err() != nil {
			return context.Canceled
		}
		if s.macosGuest != nil {
			// The native endpoint is tied to the VM's saved configuration.
			// Its normal recovery validates that fingerprint and keeps the revision.
			return s.checkNetworkIfWanted()
		}
		// The ordinary job validates the current applied node and interface,
		// revokes any old gate report, and completes the normal safety check.
		return s.rebindProxyInterface()
	})
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { s.jobs.cancelJob(job.ID, func(kind string) bool { return kind == "proxy-network" }) })
	defer stop()
	result, err := s.waitJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if result.ID != job.ID || result.Canceled {
		return context.Canceled
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
