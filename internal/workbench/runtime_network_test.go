package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

// steadyNetworkServer has an applied proxy bound to the current physical
// interface, a stopped VM and no due subscription: nothing for a network
// round to do.
func steadyNetworkServer(t *testing.T, physical func(context.Context) (string, error)) *Server {
	t.Helper()
	s := testServer(t)
	if err := os.MkdirAll(managedproxy.Dir(s.profile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedproxy.Dir(s.profile), "config.yaml"), []byte("interface-name: en0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := physicalProxyInterface
	physicalProxyInterface = physical
	t.Cleanup(func() { physicalProxyInterface = previous })
	return s
}

func onEn0(context.Context) (string, error) { return "en0", nil }

func TestSteadyNetworkRoundLeavesOperationLockFree(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	s := steadyNetworkServer(t, func(context.Context) (string, error) {
		close(arrived)
		<-release
		return "en0", nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		skipped := 0
		s.networkMonitorRound(&skipped, s.networkRoundWanted, func() { t.Error("steady round ran the locked tick") })
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("round did not read the physical interface")
	}
	if !s.op.TryLock() {
		t.Fatal("network round held the operation lock while reading the interface")
	}
	s.op.Unlock()
	finish()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("round did not finish")
	}
	records := monitorTimingRecords(t, s, "network")
	if len(records) != 1 || records[0]["result"] != "steady" || records[0]["physical_interface_ms"] == "" {
		t.Fatalf("steady round record = %v", records)
	}
}

// A round must run its locked tick whenever one of the tick's steps could
// act. This table follows the steps of monitorNetworkTick.
func TestNetworkRoundRunsLockedTickWheneverAStepCouldAct(t *testing.T) {
	running := func(t *testing.T, s *Server) {
		marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
		if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
			t.Fatal(err)
		}
	}
	coreRunning := func(t *testing.T, up bool) {
		previous := proxyCoreRunning
		proxyCoreRunning = func(context.Context, string, managedproxy.Store) bool { return up }
		t.Cleanup(func() { proxyCoreRunning = previous })
	}
	for _, c := range []struct {
		name     string
		physical func(context.Context) (string, error)
		prepare  func(*testing.T, *Server)
		want     bool
	}{
		{"steady with the VM stopped", onEn0, func(*testing.T, *Server) {}, false},
		{"steady with the VM and proxy running", onEn0, func(t *testing.T, s *Server) { running(t, s); coreRunning(t, true) }, false},
		{"proxy network marked lost", onEn0, func(_ *testing.T, s *Server) { s.proxyNetworkLost = true }, true},
		{"network recovery pending", onEn0, func(_ *testing.T, s *Server) { s.recovery.RunID = "run" }, true},
		{"job running", onEn0, func(_ *testing.T, s *Server) { s.setJobForTest(Job{ID: "j", Kind: "proxy", Running: true}) }, true},
		{"storage recovery required", onEn0, func(_ *testing.T, s *Server) { s.storageRecoveryError = "moved" }, true},
		{"configuration unreadable", onEn0, func(t *testing.T, s *Server) {
			if err := os.WriteFile(s.configPath, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"proxy not applied", onEn0, func(t *testing.T, s *Server) {
			cfg, _ := loadConfig(s.configPath)
			cfg.ProxyRevision = "other"
			if err := writePrivate(s.configPath, cfg); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"physical interface unreadable", func(context.Context) (string, error) { return "", errors.New("no uplink") }, func(*testing.T, *Server) {}, true},
		{"physical interface changed", func(context.Context) (string, error) { return "en1", nil }, func(*testing.T, *Server) {}, true},
		{"bound interface unreadable", onEn0, func(t *testing.T, s *Server) {
			if err := os.Remove(filepath.Join(managedproxy.Dir(s.profile), "config.yaml")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"proxy core down while the VM runs", onEn0, func(t *testing.T, s *Server) { running(t, s); coreRunning(t, false) }, true},
		{"subscription refresh due", onEn0, func(t *testing.T, s *Server) {
			p, err := managedproxy.Load(s.profile)
			if err != nil {
				t.Fatal(err)
			}
			p.Sources[0].URL, p.Sources[0].Interval = "https://example.invalid/sub", 1
			p.Sources[0].Updated, p.Sources[0].Attempted = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
			if err := p.Save(s.profile); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := steadyNetworkServer(t, c.physical)
			c.prepare(t, s)
			ticked := false
			skipped := 0
			s.networkMonitorRound(&skipped, s.networkRoundWanted, func() {
				if s.op.TryLock() {
					s.op.Unlock()
					t.Error("locked tick ran without the operation lock")
				}
				ticked = true
			})
			if ticked != c.want {
				t.Fatalf("locked tick ran = %v, want %v", ticked, c.want)
			}
		})
	}
}
