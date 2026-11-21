package workbench

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// writeGuestPressure publishes a fresh resource reading like the one in the
// field report: 3 GiB guest, 46 MB available, swap full.
func writeGuestPressure(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now()
	status := cage.ResourceStatus{Version: 1, UpdatedAt: now, ObservedAt: now, Maximum: 3 << 30, Target: 3 << 30, CPUs: 3, Pressure: true,
		Guest: &cage.ResourceSample{Total: 3049803776, Available: 48779264, SwapTotal: 762449920, Stall: 2.88}}
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-resources.json"), status); err != nil {
		t.Fatal(err)
	}
}

// recoveryRound runs one network-tick recovery step with the operation lock
// held, as the monitor does, and reports whether it admitted a recheck. An
// admitted job runs to its end once the lock is released.
func recoveryRound(t *testing.T, s *Server) bool {
	t.Helper()
	writeGuestPressure(t, s)
	s.op.Lock()
	s.recoverNetwork()
	job := s.jobs.snapshot()
	s.op.Unlock()
	admitted := job.Running && job.Kind == "network-recovery"
	if admitted {
		deadline := time.Now().Add(10 * time.Second)
		for s.jobs.snapshot().Running && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if s.jobs.snapshot().Running {
			t.Fatal("recovery job did not finish")
		}
	}
	return admitted
}

func replaceRecoveryCheck(t *testing.T, check func(*Server) error) {
	t.Helper()
	previous := networkRecoveryCheck
	t.Cleanup(func() { networkRecoveryCheck = previous })
	networkRecoveryCheck = check
}

// grantFreshCheck is a full check that passes: a new check session and grant.
func grantFreshCheck(s *Server, gate *cage.EgressGate) error {
	if err := gate.BeginCheck(); err != nil {
		return err
	}
	state := gate.Status()
	report := *state.Report
	report.CheckID, report.ID, report.CreatedAt = state.CheckID, "pressure-recovery", time.Now()
	for i := range report.Checks {
		report.Checks[i].CheckedAt = report.CreatedAt
	}
	if err := gate.Grant(report); err != nil {
		return err
	}
	s.publishGate(gate.Status())
	return nil
}

func TestRecoveryUnderContinuousPressureRunsAfterBoundAndReopens(t *testing.T) {
	s, gate := failedMonitoringServer(t)
	t.Cleanup(s.jobs.close)
	checks := 0
	replaceRecoveryCheck(t, func(s *Server) error {
		checks++
		if gate.Status().Mode != cage.GateClosed {
			t.Error("opened before the full check")
		}
		return grantFreshCheck(s, gate)
	})
	s.recovery.Next = time.Now().Add(-time.Second)
	if recoveryRound(t, s) || s.recovery.pressureSince.IsZero() {
		t.Fatalf("fresh pressure did not postpone the due recheck: %+v", s.recovery)
	}
	e := s.safety.Explanation
	if e == nil || e.State != networkWaitingMemory || !e.MemoryShort || e.RecheckAt == nil || len(e.Advice) == 0 ||
		!strings.Contains(e.Reason, "环境内存不足") || !strings.Contains(e.Next, "最迟") {
		t.Fatalf("explanation while waiting: %+v", e)
	}
	if want := s.recovery.pressureSince.Add(recoveryPressureWait); e.RecheckAt.Sub(want).Abs() > time.Second {
		t.Fatalf("recheck_at %s, want %s", e.RecheckAt, want)
	}
	if message := s.currentNetworkErrorLocked(); !strings.Contains(message, "最多再等待") {
		t.Fatalf("network_error does not bound the wait: %s", message)
	}
	// Pressure continues inside the bound: still nothing runs.
	s.recovery.pressureSince = time.Now().Add(-recoveryPressureWait + 5*time.Second)
	if recoveryRound(t, s) || checks != 0 {
		t.Fatal("recheck ran before the bound")
	}
	// Pressure continues past the bound: the full check runs anyway.
	s.recovery.pressureSince = time.Now().Add(-recoveryPressureWait - time.Second)
	if !recoveryRound(t, s) || checks != 1 {
		t.Fatalf("recheck did not run after the bound: checks=%d", checks)
	}
	if state := gate.Status(); state.Mode != cage.GateOpen || s.recovery.RunID != "" || s.safety.Explanation != nil {
		t.Fatalf("a passing check did not reopen: gate=%+v recovery=%+v explanation=%+v", state, s.recovery, s.safety.Explanation)
	}
}

func TestFailingRecheckUnderPressureBacksOff(t *testing.T) {
	s, gate := failedMonitoringServer(t)
	t.Cleanup(s.jobs.close)
	checks := 0
	replaceRecoveryCheck(t, func(*Server) error {
		checks++
		return errors.New("浏览器检测未返回完整结果，请更新或重启环境")
	})
	s.recovery.Next = time.Now().Add(-time.Second)
	s.recovery.pressureSince = time.Now().Add(-recoveryPressureWait - time.Second)
	if !recoveryRound(t, s) || checks != 1 {
		t.Fatal("first recheck after the bound did not run")
	}
	if s.recovery.Attempts != 1 || !s.recovery.Next.After(time.Now().Add(25*time.Second)) || !s.recovery.pressureSince.IsZero() || gate.Status().Mode != cage.GateClosed {
		t.Fatalf("failure did not back off: %+v", s.recovery)
	}
	// Within the backoff nothing runs, however long the pressure lasts.
	for i := 0; i < 3; i++ {
		if recoveryRound(t, s) {
			t.Fatal("recheck ran inside its backoff")
		}
	}
	// Backoff over, pressure still on: the pressure wait starts again.
	s.recovery.Next = time.Now().Add(-time.Second)
	if recoveryRound(t, s) || s.recovery.pressureSince.IsZero() || checks != 1 {
		t.Fatalf("pressure did not postpone the next attempt again: %+v", s.recovery)
	}
	if e := s.safety.Explanation; e == nil || e.State != networkWaitingMemory || !strings.Contains(e.Reason, "浏览器检测未返回完整结果") {
		t.Fatalf("explanation after a failed attempt: %+v", e)
	}
	s.recovery.pressureSince = time.Now().Add(-recoveryPressureWait - time.Second)
	if !recoveryRound(t, s) || checks != 2 || s.recovery.Attempts != 2 || !s.recovery.Next.After(time.Now().Add(55*time.Second)) {
		t.Fatalf("second attempt did not back off further: checks=%d %+v", checks, s.recovery)
	}
}

func TestNetworkExplanationReachesStatusPhoneAndProxyLabel(t *testing.T) {
	s, _ := failedMonitoringServer(t)
	t.Cleanup(s.jobs.close)
	s.recovery.Next = time.Now().Add(-time.Second)
	if recoveryRound(t, s) {
		t.Fatal("recheck ran under fresh pressure")
	}
	response := httptest.NewRecorder()
	s.status(response, httptest.NewRequest("GET", "/api/status", nil))
	var status struct {
		NetworkError string `json:"network_error"`
		Safety       struct {
			Gate        *cage.GateStatus   `json:"gate"`
			Explanation networkExplanation `json:"explanation"`
		} `json:"safety"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	e := status.Safety.Explanation
	if status.Safety.Gate == nil || status.Safety.Gate.Mode != cage.GateClosed || e.State != networkWaitingMemory || !e.MemoryShort ||
		e.RecheckAt == nil || len(e.Advice) == 0 || strings.Count(e.Reason, "46 MB") != 1 || !strings.Contains(status.NetworkError, "自动重检最多再等待") {
		t.Fatalf("/api/status: %s", response.Body.String())
	}
	w := request(s, "GET", "/api/mobile/environment", "", "", true)
	var phone struct {
		Network struct {
			Mode        string              `json:"mode"`
			Explanation *networkExplanation `json:"explanation"`
		} `json:"network"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &phone); err != nil {
		t.Fatal(err)
	}
	if phone.Network.Mode != cage.GateClosed || phone.Network.Explanation == nil || phone.Network.Explanation.State != networkWaitingMemory || phone.Network.Explanation.Reason != e.Reason {
		t.Fatalf("/api/mobile/environment: %s", w.Body.String())
	}
	if label := s.networkStateLabel("paused", "外网未放行"); label != "外网未放行：环境内存不足，等待自动重检" {
		t.Fatalf("proxy label: %s", label)
	}
	if label := s.networkStateLabel("checking", "正在检查"); label != "正在检查" {
		t.Fatalf("proxy label changed a live state: %s", label)
	}
	// A paused network says so instead.
	if err := s.pauseNetworkNow("已手动暂停外网"); err != nil {
		t.Fatal(err)
	}
	if e := s.safety.Explanation; e == nil || e.State != networkPausedState {
		t.Fatalf("explanation after pause: %+v", e)
	}
}

func TestMemoryNoteKeepsPressureWhileProbeTimesOut(t *testing.T) {
	now := time.Now()
	sample := &cage.ResourceSample{Total: 3 << 30, Available: 48 << 20}
	for _, tc := range []struct {
		name   string
		status cage.ResourceStatus
		err    error
		short  bool
		reason string
	}{
		{"fresh pressure", cage.ResourceStatus{ObservedAt: now, Pressure: true, Guest: sample}, nil, true, "环境内存不足（可用约 48 MB，共 3.0 GB）"},
		{"probe timing out after a pressured sample", cage.ResourceStatus{ObservedAt: now.Add(-time.Minute), Pressure: true, Guest: sample, Error: "读取资源用量失败：等待响应超时。自动回收内存已暂停"}, nil, true, "资源读数也暂时超时"},
		{"host short", cage.ResourceStatus{ObservedAt: now, HostPressure: true, Guest: sample}, nil, true, "这台机器内存不足"},
		{"no pressure", cage.ResourceStatus{ObservedAt: now, Guest: sample}, nil, false, ""},
		{"no reading", cage.ResourceStatus{}, errors.New("resource observation expired"), false, ""},
	} {
		note := memoryNote(tc.status, tc.err, now, memorySizing{})
		if note.Short != tc.short || !strings.Contains(note.Reason, tc.reason) || (note.Short && len(note.Advice) == 0) {
			t.Errorf("%s: %+v", tc.name, note)
		}
	}
}
