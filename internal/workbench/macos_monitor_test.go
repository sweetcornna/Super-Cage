package workbench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestprofile"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

// readyMacMonitorGuest is a ready virtual Mac whose identity probe is
// answered by probe, with a closed gate and a steady network round.
func readyMacMonitorGuest(t *testing.T, probe func(n int32) uint32) (*Server, *cage.EgressGate, *atomic.Int32) {
	t.Helper()
	probes := &atomic.Int32{}
	g := newMacWorkspaceGuest(t, func(_ ssh.Channel, command string) (uint32, bool) {
		if !strings.Contains(command, "uname -s") {
			return 0, false
		}
		return probe(probes.Add(1)), true
	})
	s := g.s
	s.ready, s.macComponentsChecked = true, true
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := cage.NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := cage.StartGateControl(s.configPath, s.instanceDir(), gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if err := os.MkdirAll(managedproxy.Dir(s.profile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedproxy.Dir(s.profile), "config.yaml"), []byte("interface-name: en0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousInterface, previousCore := physicalProxyInterface, proxyCoreRunning
	physicalProxyInterface = onEn0
	proxyCoreRunning = func(context.Context, string, managedproxy.Store) bool { return true }
	t.Cleanup(func() { physicalProxyInterface, proxyCoreRunning = previousInterface, previousCore })
	return s, gate, probes
}

func TestMacOSMonitorProbeLeavesOperationLockFree(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	s, _, probes := readyMacMonitorGuest(t, func(n int32) uint32 {
		if n == 1 {
			close(arrived)
			<-release
		}
		return 0
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.monitorEnvironmentRound(&environmentMonitor{})
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not probe the guest")
	}
	if !s.op.TryLock() {
		t.Fatal("macOS guest probe held the operation lock")
	}
	s.op.Unlock()
	finish()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor round did not finish")
	}
	if !environmentReady(s) || probes.Load() != 1 {
		t.Fatalf("ready=%v probes=%d; a steady round needs one probe and no attach", environmentReady(s), probes.Load())
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "completed" {
		t.Fatalf("steady macOS round = %v", record)
	}
	if records := monitorTimingRecords(t, s, "network"); len(records) != 1 || records[0]["result"] != "steady" {
		t.Fatalf("network part of the macOS round = %v", records)
	}
}

func TestMacOSMonitorToleratesOneFailedProbeAndAttachesOnTheSecond(t *testing.T) {
	var s *Server
	lockHeld := atomic.Bool{}
	s, _, probes := readyMacMonitorGuest(t, func(n int32) uint32 {
		if n == 3 {
			// The deciding probe is attach's own, under the lock.
			if s.op.TryLock() {
				s.op.Unlock()
			} else {
				lockHeld.Store(true)
			}
		}
		return 1
	})
	m := &environmentMonitor{}
	s.monitorEnvironmentRound(m)
	if !environmentReady(s) || probes.Load() != 1 {
		t.Fatalf("one failed probe: ready=%v probes=%d", environmentReady(s), probes.Load())
	}
	if record := lastEnvironmentRound(t, s); record["result"] != "health_failed_once" {
		t.Fatalf("first failure record = %v", record)
	}
	s.monitorEnvironmentRound(m)
	if environmentReady(s) || probes.Load() != 3 || !lockHeld.Load() {
		t.Fatalf("second failure: ready=%v probes=%d attach under lock=%v", environmentReady(s), probes.Load(), lockHeld.Load())
	}
}

func TestMacOSMonitorRecoveredProbeResetsTheCount(t *testing.T) {
	s, _, probes := readyMacMonitorGuest(t, func(n int32) uint32 {
		if n == 1 || n == 3 {
			return 1
		}
		return 0
	})
	m := &environmentMonitor{}
	for range 3 {
		s.monitorEnvironmentRound(m)
	}
	if !environmentReady(s) || probes.Load() != 3 {
		t.Fatalf("alternating probes: ready=%v probes=%d", environmentReady(s), probes.Load())
	}
}

// Gate decisions are not delayed: a paused network closes an open gate in
// the same round, under the lock.
func TestMacOSMonitorClosesTheGateInTheSameRound(t *testing.T) {
	s, gate, _ := readyMacMonitorGuest(t, func(int32) uint32 { return 0 })
	if err := gate.BeginCheck(); err != nil {
		t.Fatal(err)
	}
	grantTestGate(t, gate)
	if gate.Status().Mode != cage.GateOpen {
		t.Fatal("fixture gate did not open")
	}
	s.mu.Lock()
	s.networkPaused = true
	s.mu.Unlock()
	s.monitorEnvironmentRound(&environmentMonitor{})
	if gate.Status().Mode != cage.GateClosed {
		t.Fatalf("paused network left the gate %s", gate.Status().Mode)
	}
}

// A job that begins while the lock-free round probes the guest owns the
// guest from then on: the round sends it no browser profile.
func TestMacOSMonitorSendsNoProfileAfterAJobBegins(t *testing.T) {
	var s *Server
	s, _, _ = readyMacMonitorGuest(t, func(int32) uint32 {
		s.setJobForTest(Job{ID: "update", Kind: "environment-update", Running: true})
		return 0
	})
	if _, err := guestprofile.Create(s.profile, guestprofile.Options{Username: "alex", AudioName: "speakers"}); err != nil {
		t.Fatal(err)
	}
	if observed := s.observeMacOSGuest(t.Context()); observed != observedChange {
		t.Fatalf("observation = %v, want a change", observed)
	}
	s.mu.Lock()
	client := s.windowsBrowserControl
	s.mu.Unlock()
	if client != nil {
		t.Fatal("the round opened the guest browser controller after a job began")
	}
}
