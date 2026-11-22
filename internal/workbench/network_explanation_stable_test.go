package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// What the status says about the network must not change between two polls
// that see the same situation: a number recomputed on every read would change
// the status every time and the workbench would draw again every poll. The
// time is the explanation's deadline_ms, which clients count down to.
func TestNetworkTextIsStableBetweenReads(t *testing.T) {
	type snapshot struct{ explanation, networkError string }
	read := func(s *Server) snapshot {
		s.mu.Lock()
		defer s.mu.Unlock()
		// The status handler and the phone's environment read both refresh.
		s.refreshNetworkExplanationLocked(time.Now())
		data, err := json.Marshal(s.safety.Explanation)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot{string(data), s.currentNetworkErrorLocked()}
	}
	same := func(t *testing.T, s *Server, state string) {
		t.Helper()
		first := read(s)
		var parsed struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(first.explanation), &parsed); err != nil || parsed.State != state {
			t.Fatalf("want state %q: %s (%v)", state, first.explanation, err)
		}
		time.Sleep(1100 * time.Millisecond)
		if second := read(s); second != first {
			t.Fatalf("the status changed between two reads of the same situation:\n%+v\n%+v", first, second)
		}
	}
	writeMemory := func(s *Server, cause, reason string) {
		s.mu.Lock()
		s.safety.memory = memorySnapshot{At: time.Now(), Short: true, Cause: cause, Reason: reason, Advice: []string{"关闭标签页"}}
		s.mu.Unlock()
	}

	t.Run("recheck scheduled", func(t *testing.T) {
		s, _ := failedMonitoringServer(t)
		s.recovery.Next = time.Now().Add(40 * time.Second)
		same(t, s, networkRecheckScheduled)
	})
	t.Run("recheck overdue", func(t *testing.T) {
		s, _ := failedMonitoringServer(t)
		s.recovery.Next = time.Now().Add(-10 * time.Second)
		same(t, s, networkRecheckScheduled)
	})
	t.Run("waiting for memory", func(t *testing.T) {
		s, _ := failedMonitoringServer(t)
		s.recovery.Next = time.Now().Add(-time.Second)
		s.recovery.pressureSince = time.Now()
		writeMemory(s, memoryCauseBrowser, "隔离浏览器内存已到上限（约 1.2 GB）")
		same(t, s, networkWaitingMemory)
	})
	t.Run("waiting for memory past its bound", func(t *testing.T) {
		s, _ := failedMonitoringServer(t)
		s.recovery.Next = time.Now().Add(-time.Second)
		s.recovery.pressureSince = time.Now().Add(-2 * recoveryPressureWait)
		writeMemory(s, memoryCauseEnvironment, "环境内存不足（共 2.8 GB）")
		same(t, s, networkWaitingMemory)
	})
	t.Run("check required", func(t *testing.T) {
		s, _ := failedMonitoringServer(t)
		s.mu.Lock()
		s.recovery = networkRecovery{}
		s.mu.Unlock()
		same(t, s, networkCheckRequired)
	})
	t.Run("paused", func(t *testing.T) {
		s, _ := failedMonitoringServer(t)
		if err := s.pauseNetworkNow("已手动暂停外网"); err != nil {
			t.Fatal(err)
		}
		same(t, s, networkPausedState)
	})
	t.Run("lease unconfirmed", func(t *testing.T) {
		s, gate := unconfirmedMonitorServer(t)
		cfg, _ := loadConfig(s.configPath)
		fakeMonitorReads(t, func(context.Context) (guestObservation, error) { return matchingGuestSettings(cfg), nil },
			func(context.Context) (browserObservation, error) {
				return browserObservation{}, noAnswer(errors.New("浏览器检测未返回完整结果"))
			})
		s.monitorSafetyWithProbe(exitProbeOK)
		if gate.Status().Mode != cage.GateOpen {
			t.Fatalf("gate: %+v", gate.Status())
		}
		writeMemory(s, memoryCauseBrowser, "隔离浏览器内存已到上限（约 1.2 GB）")
		same(t, s, networkUnconfirmed)
	})
}

// The explanation's deadline is the same moment as its RFC 3339 fields, and
// the sentences say what they promise in the target shape.
func TestNetworkExplanationDeadlineAndSentences(t *testing.T) {
	s, _ := failedMonitoringServer(t)
	now := time.Now()
	s.recovery.Next = now.Add(-time.Second)
	s.recovery.pressureSince = now
	s.mu.Lock()
	s.safety.monitorError = "联网许可到期前仍无法读取浏览器时区和语言，外网已暂停"
	s.safety.memory = memorySnapshot{At: now, Short: true, Cause: memoryCauseBrowser, Reason: "隔离浏览器内存已到上限（约 1.2 GB）"}
	s.refreshNetworkExplanationLocked(now)
	e := s.safety.Explanation
	s.mu.Unlock()
	if e == nil || e.State != networkWaitingMemory || e.RecheckAt == nil || e.DeadlineMs != e.RecheckAt.UnixMilli() ||
		e.DeadlineMs != now.Add(recoveryPressureWait).UnixMilli() {
		t.Fatalf("deadline: %+v", e)
	}
	if want := "隔离浏览器内存已到上限（约 1.2 GB），安全检查读不到它的时区和语言，所以暂时关闭了外网。"; e.Reason != want {
		t.Fatalf("reason %q, want %q", e.Reason, want)
	}
	if want := "浏览器内存回落后会自动重检并恢复联网，最迟 1 分钟内也会重检。"; e.Next != want {
		t.Fatalf("next %q, want %q", e.Next, want)
	}
}
