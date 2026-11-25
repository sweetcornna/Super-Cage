package workbench

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Idle server with VM stopped and no clients connected must spawn 0 external
// interface checks (powershell.exe on Windows, networksetup/route/scutil on
// macOS), 0 SSH processes, and 0 diagnostics disk writes across a full 60 s.
func TestIdleServerMonitorsSleepWhenVMStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var physCalls atomic.Int64
		s := steadyNetworkServer(t, func(context.Context) (string, error) {
			physCalls.Add(1)
			return "en0", nil
		})
		s.startMonitors()
		time.Sleep(60 * time.Second)
		s.closeBackground()

		if got := physCalls.Load(); got != 0 {
			t.Fatalf("idle stopped-VM server called physicalProxyInterface %d times in 60s, want 0", got)
		}
		eventsPath := filepath.Join(s.profile, "logs", "events.jsonl")
		if data, err := os.ReadFile(eventsPath); err == nil && len(data) > 0 {
			t.Fatalf("idle stopped-VM server wrote %d bytes to events.jsonl in 60s, want 0", len(data))
		}
	})
}

// Steady healthy rounds while the VM is running must not rewrite identical
// quiet monitor records to events.jsonl every 5 s.
func TestMonitorTimingSuppressesRepeatedQuietRecords(t *testing.T) {
	s := readyLinuxServer(t)
	installHealthGuest(t, s, &healthGuest{})
	m := &environmentMonitor{}
	for range 5 {
		s.monitorEnvironmentRound(m)
	}
	records := monitorTimingRecords(t, s, "environment")
	if len(records) != 1 {
		t.Fatalf("steady repeated environment rounds wrote %d events.jsonl records, want 1", len(records))
	}
}

// Waking monitors after the VM starts resumes monitoring promptly.
func TestIdleServerMonitorsWakeWhenVMBeginsRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var physCalls atomic.Int64
		s := steadyNetworkServer(t, func(context.Context) (string, error) {
			physCalls.Add(1)
			return "en0", nil
		})
		guest := &healthGuest{}
		installHealthGuest(t, s, guest)
		s.startMonitors()
		time.Sleep(30 * time.Second)
		if physCalls.Load() != 0 || guest.requests.Load() != 0 {
			s.closeBackground()
			t.Fatalf("idle server ran before VM became ready: phys=%d health=%d", physCalls.Load(), guest.requests.Load())
		}
		s.mu.Lock()
		s.ready = true
		s.credentials = guestCredentials{Token: "guest-token"}
		s.mu.Unlock()
		s.wakeMonitors()
		time.Sleep(6 * time.Second)
		s.closeBackground()
		if guest.requests.Load() == 0 {
			t.Fatal("environment monitor did not wake after VM became ready")
		}
		if physCalls.Load() == 0 {
			t.Fatal("network monitor did not wake after VM became ready")
		}
	})
}
