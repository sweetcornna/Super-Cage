package workbench

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// healthGuest answers the browser-control /health probe through a fake
// transport. fail decides each reply; the default is healthy.
type healthGuest struct {
	requests atomic.Int32
	fail     func(n int32, r *http.Request) error
}

func installHealthGuest(t *testing.T, s *Server, guest *healthGuest) {
	t.Helper()
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected guest request %s %s", r.Method, r.URL.Path)
		}
		n := guest.requests.Add(1)
		if guest.fail != nil {
			if err := guest.fail(n, r); err != nil {
				return nil, err
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ready":true}`)), Request: r}, nil
	}))
}

func readyLinuxServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	s.ready = true
	s.credentials = guestCredentials{Token: "guest-token"}
	return s
}

func lastEnvironmentRound(t *testing.T, s *Server) map[string]string {
	t.Helper()
	records := monitorTimingRecords(t, s, "environment")
	if len(records) == 0 {
		t.Fatal("missing environment monitor record")
	}
	return records[len(records)-1]
}

func environmentReady(s *Server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// A user action that arrives while the monitor waits for the guest must not
// be refused as "环境正在执行其他操作".
func TestEnvironmentHealthProbeLeavesOperationLockFree(t *testing.T) {
	s := readyLinuxServer(t)
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	installHealthGuest(t, s, &healthGuest{fail: func(_ int32, r *http.Request) error {
		close(arrived)
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.monitorEnvironmentRound(&environmentMonitor{})
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not probe the guest")
	}
	if _, err := s.createTerminal(terminalRequest{Kind: "shell"}); errors.Is(err, errTerminalBusy) {
		t.Fatal("terminal creation was refused while the monitor probed the guest")
	}
	if !s.op.TryLock() {
		t.Fatal("health probe held the operation lock")
	}
	s.op.Unlock()
	finish()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor round did not finish")
	}
	if !environmentReady(s) {
		t.Fatal("healthy probe changed readiness")
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "completed" || record["environment_health_ms"] == "" {
		t.Fatalf("healthy round record = %v", record)
	}
}

// A monitor tick that overlaps a running user action still checks the guest.
func TestEnvironmentHealthProbeRunsWhileUserActionHoldsLock(t *testing.T) {
	s := readyLinuxServer(t)
	guest := &healthGuest{}
	installHealthGuest(t, s, guest)
	s.op.Lock()
	m := &environmentMonitor{}
	s.monitorEnvironmentRound(m)
	s.op.Unlock()
	if guest.requests.Load() != 1 {
		t.Fatalf("health probes = %d, want 1", guest.requests.Load())
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "completed" || record["consecutive_lock_skips"] != "0" {
		t.Fatalf("round under a user action = %v", record)
	}
}

func TestEnvironmentMonitorToleratesOneFailedProbe(t *testing.T) {
	s := readyLinuxServer(t)
	installHealthGuest(t, s, &healthGuest{fail: func(n int32, _ *http.Request) error {
		if n == 1 || n == 3 {
			return errors.New("synthetic slow guest")
		}
		return nil
	}})
	m := &environmentMonitor{}
	for round := 1; round <= 3; round++ {
		s.monitorEnvironmentRound(m)
		if !environmentReady(s) {
			t.Fatalf("round %d: one failed probe marked the environment not ready", round)
		}
	}
	records := monitorTimingRecords(t, s, "environment")
	if len(records) != 3 || records[0]["result"] != "health_failed_once" || records[1]["result"] != "completed" || records[2]["result"] != "health_failed_once" {
		t.Fatalf("round results = %v", records)
	}
}

func TestEnvironmentMonitorTwoConsecutiveFailuresMarkNotReady(t *testing.T) {
	s := readyLinuxServer(t)
	installHealthGuest(t, s, &healthGuest{fail: func(int32, *http.Request) error { return errors.New("synthetic guest down") }})
	m := &environmentMonitor{}
	s.monitorEnvironmentRound(m)
	if !environmentReady(s) {
		t.Fatal("first failure marked the environment not ready")
	}
	s.monitorEnvironmentRound(m)
	if environmentReady(s) {
		t.Fatal("second consecutive failure kept the environment ready")
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "health_failed" {
		t.Fatalf("second failure record = %v", record)
	}
	if !s.op.TryLock() {
		t.Fatal("monitor kept the operation lock")
	}
	s.op.Unlock()
}

// The not-ready decision is serialized with user actions: while one holds the
// lock the environment stays ready, and the next failure applies it.
func TestEnvironmentMonitorMarksNotReadyOnlyUnderOperationLock(t *testing.T) {
	s := readyLinuxServer(t)
	installHealthGuest(t, s, &healthGuest{fail: func(int32, *http.Request) error { return errors.New("synthetic guest down") }})
	m := &environmentMonitor{}
	s.op.Lock()
	s.monitorEnvironmentRound(m)
	s.monitorEnvironmentRound(m)
	ready := environmentReady(s)
	record := lastEnvironmentRound(t, s)
	s.op.Unlock()
	if !ready || record["result"] != "operation_lock_busy" || record["consecutive_lock_skips"] != "1" {
		t.Fatalf("ready=%v record=%v while a user action held the lock", ready, record)
	}
	s.monitorEnvironmentRound(m)
	if environmentReady(s) {
		t.Fatal("failure after the lock was released kept the environment ready")
	}
}

// A job that ran between two failed probes starts a new count, and a job
// submitted while the deciding probe was in flight wins over its failure.
func TestEnvironmentMonitorFailureDoesNotOverrideLifecycleJob(t *testing.T) {
	s := readyLinuxServer(t)
	var duringProbe func()
	installHealthGuest(t, s, &healthGuest{fail: func(int32, *http.Request) error {
		if duringProbe != nil {
			duringProbe()
		}
		return errors.New("synthetic restart in progress")
	}})
	m := &environmentMonitor{}
	s.monitorEnvironmentRound(m)
	s.setJobForTest(Job{ID: "restart-1", Kind: "restart"})
	s.monitorEnvironmentRound(m)
	if !environmentReady(s) {
		t.Fatal("failures across a completed job were counted together")
	}
	duringProbe = func() { s.setJobForTest(Job{ID: "restart-2", Kind: "restart"}) }
	s.monitorEnvironmentRound(m)
	if !environmentReady(s) {
		t.Fatal("a failure observed before a job replaced the guest marked it not ready")
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "health_failed_superseded" {
		t.Fatalf("superseded failure record = %v", record)
	}
}

func TestEnvironmentMonitorSkipsProbeWhileJobRuns(t *testing.T) {
	s := readyLinuxServer(t)
	guest := &healthGuest{fail: func(int32, *http.Request) error { return errors.New("synthetic guest down") }}
	installHealthGuest(t, s, guest)
	m := &environmentMonitor{}
	s.monitorEnvironmentRound(m)
	s.setJobForTest(Job{ID: "stop-1", Kind: "stop", Running: true})
	s.monitorEnvironmentRound(m)
	if guest.requests.Load() != 1 {
		t.Fatalf("monitor probed the guest during a job: %d probes", guest.requests.Load())
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "job_running" {
		t.Fatalf("job round record = %v", record)
	}
	s.setJobForTest(Job{ID: "stop-1", Kind: "stop"})
	s.monitorEnvironmentRound(m)
	if !environmentReady(s) {
		t.Fatal("failure before a job was counted with one after it")
	}
}

// Concurrent rounds and user actions under the race detector: the probe
// never holds the operation lock, and readiness only changes under it.
func TestEnvironmentMonitorOverlapsUserActions(t *testing.T) {
	s := readyLinuxServer(t)
	installHealthGuest(t, s, &healthGuest{fail: func(n int32, _ *http.Request) error {
		time.Sleep(time.Millisecond)
		if n%2 == 0 {
			return errors.New("synthetic intermittent failure")
		}
		return nil
	}})
	stop := make(chan struct{})
	refused := atomic.Int32{}
	actions := make(chan struct{})
	go func() {
		defer close(actions)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.createTerminal(terminalRequest{Kind: "shell"}); errors.Is(err, errTerminalBusy) {
				refused.Add(1)
			}
		}
	}()
	m := &environmentMonitor{}
	for range 20 {
		s.monitorEnvironmentRound(m)
	}
	close(stop)
	<-actions
	if !environmentReady(s) {
		t.Fatal("alternating probe results marked the environment not ready")
	}
	if refused.Load() != 0 {
		t.Fatalf("%d user actions were refused while only the monitor ran", refused.Load())
	}
}

// A lock-free round after a busy one reports no lock skips.
func TestEnvironmentMonitorLockSkipsResetOnLockFreeRounds(t *testing.T) {
	s := readyLinuxServer(t)
	installHealthGuest(t, s, &healthGuest{fail: func(n int32, _ *http.Request) error {
		if n <= 2 {
			return errors.New("synthetic guest down")
		}
		return nil
	}})
	m := &environmentMonitor{}
	s.op.Lock()
	s.monitorEnvironmentRound(m)
	s.monitorEnvironmentRound(m)
	s.op.Unlock()
	if record := lastEnvironmentRound(t, s); record["consecutive_lock_skips"] != "1" {
		t.Fatalf("busy round = %v", record)
	}
	s.monitorEnvironmentRound(m)
	if record := lastEnvironmentRound(t, s); record["result"] != "completed" || record["consecutive_lock_skips"] != "0" {
		t.Fatalf("healthy round after a busy one = %v", record)
	}
}
