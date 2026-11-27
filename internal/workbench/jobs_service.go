package workbench

import (
	"context"
	"fmt"
)

func (s *Server) busy() bool { return s.jobs.snapshot().Running }

// submitJob keeps network intent and account policy in the workbench. The
// manager owns admission, execution, cancellation and published job state.
func (s *Server) submitJob(kind string, run func() error) (Job, error) {
	s.mu.Lock()
	job, intentFailed, err := s.submitJobLocked(kind, run)
	s.mu.Unlock()
	if intentFailed {
		_ = s.closeProxyGate("网络恢复意图无法保存，外网保持关闭")
	}
	return job, err
}

// submitJobLocked is submitJob for a caller that holds s.mu. intentFailed
// asks the caller to close the proxy gate once it has released s.mu.
func (s *Server) submitJobLocked(kind string, run func() error) (job Job, intentFailed bool, err error) {
	if s.closing {
		return Job{}, false, errJobsClosed
	}
	if s.appUpdateSealed {
		return Job{}, false, errAppUpdateSealed
	}
	if s.storageRecoveryError != "" && kind != "storage-recovery" {
		return Job{}, false, fmt.Errorf("环境存储待恢复，请在设置中恢复后继续。%s", s.storageRecoveryError)
	}
	var afterSuccess func() error
	if kind == "logout" {
		kind = "stop"
		afterSuccess = func() error { return s.account.lockAccess(true) }
	}
	job, err = s.jobs.submit(&s.op, kind, s.safety.pauseVersion, func() error {
		if kind == "stop" {
			s.cancelWorkbenchRuntimeRestoreLocked()
		}
		if kind == "safety" || (s.macosGuest == nil && (kind == "start" || kind == "restart" || kind == "restore")) {
			if err := s.saveNetworkIntentLocked(false); err != nil {
				intentFailed = true
				return err
			}
		}
		if s.monitorCancel != nil {
			s.monitorCancel()
		}
		return nil
	}, afterSuccess, func() error {
		defer s.wakeMonitors()
		return run()
	})
	if err == nil {
		s.wakeMonitorsLocked()
	}
	return job, intentFailed, err
}

func (s *Server) setMessage(message string) { s.jobs.execution().message(message) }

func (s *Server) operationContext() context.Context {
	execution := s.jobs.execution()
	if execution.id != "" {
		return execution.ctx
	}
	return s.lifetimeContext()
}
