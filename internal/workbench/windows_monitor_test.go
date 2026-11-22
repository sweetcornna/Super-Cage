package workbench

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// readyWindowsMonitorServer is a ready local Windows guest whose status is
// answered by status, with its environment share running.
func readyWindowsMonitorServer(t *testing.T, status func(n int32, s *Server) (windowsGuestState, error)) (*Server, *atomic.Int32) {
	t.Helper()
	s := testServer(t)
	environment, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(s.profile, "environment.json"), cage.EnvironmentRecord{Directory: environment}); err != nil {
		t.Fatal(err)
	}
	share, err := validateWorkspace(s.environmentDir(), s.profile)
	if err != nil {
		t.Fatal(err)
	}
	s.windowsGuest = &guestos.WindowsProfile{Version: 1, VMID: uuid.New(), GUIUserSID: "S-1-5-21-1-2-3-1001"}
	s.ready = true
	s.windowsState = windowsGuestState{Version: 1, OS: "windows", Session: "console-1", Available: true, Running: true, ComponentsChecked: time.Now().Add(-20 * time.Second)}
	s.projectShares = map[string]*projectShare{share: {done: make(chan struct{})}}
	calls := &atomic.Int32{}
	previous := windowsGuestStatus
	windowsGuestStatus = func(s *Server, _ context.Context) (windowsGuestState, error) { return status(calls.Add(1), s) }
	t.Cleanup(func() { windowsGuestStatus = previous })
	return s, calls
}

func freshWindowsState(session string) windowsGuestState {
	return windowsGuestState{Version: 1, OS: "windows", Session: session, Available: true, Running: true, ComponentsChecked: time.Now()}
}

func TestWindowsMonitorStatusLeavesOperationLockFree(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	s, calls := readyWindowsMonitorServer(t, func(n int32, _ *Server) (windowsGuestState, error) {
		if n == 1 {
			close(arrived)
			<-release
		}
		return freshWindowsState("console-1"), nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.monitorEnvironmentRound(&environmentMonitor{})
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not read the Windows status")
	}
	if !s.op.TryLock() {
		t.Fatal("Windows status read held the operation lock")
	}
	s.op.Unlock()
	finish()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor round did not finish")
	}
	s.mu.Lock()
	ready, checked := s.ready, s.windowsState.ComponentsChecked
	s.mu.Unlock()
	if !ready || calls.Load() != 1 || time.Since(checked) > 5*time.Second {
		t.Fatalf("steady round: ready=%v status reads=%d components checked %s ago", ready, calls.Load(), time.Since(checked))
	}
}

func TestWindowsMonitorAttachesUnderLockWhenTheGuestChanged(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    func(n int32, s *Server) (windowsGuestState, error)
		wantReady bool
	}{
		{"new session", func(n int32, s *Server) (windowsGuestState, error) { return freshWindowsState("console-2"), nil }, true},
		{"status error", func(n int32, s *Server) (windowsGuestState, error) {
			return windowsGuestState{}, errWindowsAgentPending
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var underLock atomic.Bool
			s, calls := readyWindowsMonitorServer(t, func(n int32, s *Server) (windowsGuestState, error) {
				if n == 2 {
					if s.op.TryLock() {
						s.op.Unlock()
					} else {
						underLock.Store(true)
					}
				}
				return c.status(n, s)
			})
			s.monitorEnvironmentRound(&environmentMonitor{})
			if calls.Load() != 2 || !underLock.Load() {
				t.Fatalf("status reads=%d, attach under lock=%v; want an attach in the same round", calls.Load(), underLock.Load())
			}
			if environmentReady(s) != c.wantReady {
				t.Fatalf("ready = %v, want %v", environmentReady(s), c.wantReady)
			}
		})
	}
}

// A status read that a reconnect overtook never replaces the newer status.
func TestWindowsMonitorKeepsAStatusPublishedMeanwhile(t *testing.T) {
	s, _ := readyWindowsMonitorServer(t, func(n int32, s *Server) (windowsGuestState, error) {
		if n == 1 {
			s.mu.Lock()
			s.windowsState.Session = "console-2"
			s.mu.Unlock()
			return freshWindowsState("console-1"), nil
		}
		return freshWindowsState("console-2"), nil
	})
	s.monitorEnvironmentRound(&environmentMonitor{})
	s.mu.Lock()
	session := s.windowsState.Session
	s.mu.Unlock()
	if session != "console-2" {
		t.Fatalf("an older status replaced the reconnected session: %q", session)
	}
}

func TestWindowsMonitorLeavesAStoppedShareToAttach(t *testing.T) {
	var underLock atomic.Bool
	s, _ := readyWindowsMonitorServer(t, func(n int32, s *Server) (windowsGuestState, error) {
		if n == 2 && !s.op.TryLock() {
			underLock.Store(true)
		} else if n == 2 {
			s.op.Unlock()
		}
		if n == 2 {
			return windowsGuestState{}, errors.New("synthetic: stop after the decision")
		}
		return freshWindowsState("console-1"), nil
	})
	for _, share := range s.projectShares {
		close(share.done)
	}
	s.monitorEnvironmentRound(&environmentMonitor{})
	if !underLock.Load() {
		t.Fatal("a stopped environment share did not lead to an attach under the lock")
	}
}
