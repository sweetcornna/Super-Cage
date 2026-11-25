package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
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

// F1: Once ranOnce is true and a safety check is due, safetyMonitorWait must
// return delay 0 so monitorSafety runs repeatedly (both at the 5 s retry
// cadence when unverified and at the 30 s lease-renewal cadence while the gate
// stays open).
func TestSafetyMonitorRepeatsWithOpenGate(t *testing.T) {
	t.Run("due_returns_zero_delay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := readyLinuxServer(t)
			s.safety.Gate = &cage.GateStatus{Mode: cage.GateOpen}
			s.safety.VerifiedAt = time.Now().Add(-31 * time.Second)
			if delay, wakeOnly := s.safetyMonitorWait(true); delay != 0 || wakeOnly {
				t.Fatalf("due safety check: safetyMonitorWait(true) = (%v, %v), want (0, false)", delay, wakeOnly)
			}
			// Under 5 s remaining before 30 s must return exact remaining, not overshoot to 5 s.
			s.safety.VerifiedAt = time.Now().Add(-28 * time.Second)
			if delay, wakeOnly := s.safetyMonitorWait(true); delay <= 0 || delay > 2*time.Second || wakeOnly {
				t.Fatalf("2s remaining: safetyMonitorWait(true) = (%v, %v), want (<=2s, false)", delay, wakeOnly)
			}
		})
	})

	t.Run("retry_cadence_over_60s", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := steadyNetworkServer(t, func(context.Context) (string, error) { return "en0", nil })
			s.ready = true
			s.credentials = guestCredentials{Token: "guest-token"}
			installHealthGuest(t, s, &healthGuest{})
			s.safety.VerifiedAt = time.Now().Add(-time.Minute)
			s.op.Lock()
			s.startMonitors()
			runs := 0
			for range 12 {
				time.Sleep(5 * time.Second)
				s.mu.Lock()
				if s.safety.Error != "" {
					runs++
					s.safety.Error = ""
				}
				s.mu.Unlock()
			}
			s.op.Unlock()
			s.closeBackground()
			if runs < 10 {
				t.Fatalf("monitorSafety ran only %d times in 60s of fake time, want >= 10", runs)
			}
		})
	})

	t.Run("open_gate_renewal_cadence_over_60s", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := steadyNetworkServer(t, func(context.Context) (string, error) { return "en0", nil })
			s.ready = true
			s.credentials = guestCredentials{Token: "guest-token"}
			installHealthGuest(t, s, &healthGuest{})
			s.safety.Gate = &cage.GateStatus{Mode: cage.GateOpen}
			s.safety.VerifiedAt = time.Now().Add(-time.Minute)
			s.op.Lock()
			s.startMonitors()
			synctest.Wait()

			checkRan := func(step string) {
				t.Helper()
				s.mu.Lock()
				ran := s.safety.Error != ""
				s.safety.Error = ""
				s.safety.Gate = &cage.GateStatus{Mode: cage.GateOpen}
				s.safety.VerifiedAt = time.Now()
				s.mu.Unlock()
				if !ran {
					t.Fatalf("%s: expected monitorSafety to run", step)
				}
			}
			checkRan("t=0s")
			time.Sleep(29 * time.Second)
			synctest.Wait()
			s.mu.Lock()
			early := s.safety.Error != ""
			s.mu.Unlock()
			if early {
				t.Fatal("t=29s: monitorSafety ran before 30s open-gate cadence elapsed")
			}
			time.Sleep(1 * time.Second)
			synctest.Wait()
			checkRan("t=30s")
			time.Sleep(30 * time.Second)
			synctest.Wait()
			checkRan("t=60s")

			s.op.Unlock()
			s.closeBackground()
		})
	})
}

// F2: Any path that flips ready or running to true outside a job must wake
// parked background monitors so the safety loop does not stay asleep until the
// hourly tick.
func TestAttachOutsideJobWakesParkedMonitors(t *testing.T) {
	t.Run("reattach_browser_wakes_safety_monitor", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := steadyNetworkServer(t, func(context.Context) (string, error) { return "en0", nil })
			s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ready":true}`)),
					Request:    r,
				}, nil
			}))
			s.safety.VerifiedAt = time.Now().Add(-time.Minute)
			s.startMonitors()
			time.Sleep(10 * time.Second) // safety monitor is now parked because !ready && !busy

			wake := s.monitorWakeCh()
			if err := s.reattachBrowser(context.Background(), "guest-token"); err != nil {
				s.closeBackground()
				t.Fatalf("reattachBrowser: %v", err)
			}
			select {
			case <-wake:
			default:
				s.closeBackground()
				t.Fatal("reattachBrowser flipped ready=true without waking monitors")
			}
			time.Sleep(6 * time.Second)
			s.mu.Lock()
			ran := s.safety.Error != ""
			s.mu.Unlock()
			s.closeBackground()
			if !ran {
				t.Fatal("parked safety monitor did not run after reattachBrowser set ready=true")
			}
		})
	})

	t.Run("windows_attach_wakes_parked_monitors", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := readyWindowsMonitorServer(t, func(_ int32, _ *Server) (windowsGuestState, error) {
				return freshWindowsState("console-1"), nil
			})
			s.ready = false
			s.windowsState = windowsGuestState{}
			s.networkPaused = true // prevent submitJob from masking a missing wakeMonitorsLocked
			wake := s.monitorWakeCh()
			if err := s.attachWindowsGuest(); err != nil {
				t.Fatalf("attachWindowsGuest: %v", err)
			}
			select {
			case <-wake:
			default:
				t.Fatal("attachWindowsGuest flipped ready/running=true without waking monitors")
			}
		})
	})

	t.Run("windows_remote_attach_wakes_parked_monitors", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := testServer(t)
			s.windowsGuest = &guestos.WindowsProfile{
				Version: 1, Mode: "remote", VMID: uuid.New(), GUIUserSID: "S-1-5-21-1-2-3-1001",
			}
			previous := windowsGuestStatus
			windowsGuestStatus = func(_ *Server, _ context.Context) (windowsGuestState, error) {
				return freshWindowsState("headless-1"), nil
			}
			t.Cleanup(func() { windowsGuestStatus = previous })
			wake := s.monitorWakeCh()
			if err := s.attachWindowsGuest(); err != nil {
				t.Fatalf("remote attachWindowsGuest: %v", err)
			}
			select {
			case <-wake:
			default:
				t.Fatal("remote attachWindowsGuest flipped ready/running=true without waking monitors")
			}
		})
	})
}

// F3: At controller startup a Windows guest's cached windowsState.Running is
// false, so the environment monitor must perform one startup check to reattach
// an already-running Hyper-V guest instead of parking before its first round.
func TestWindowsMonitorStartupReattachesRunningGuest(t *testing.T) {
	t.Run("already_running_guest_reattaches", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, calls := readyWindowsMonitorServer(t, func(_ int32, _ *Server) (windowsGuestState, error) {
				return freshWindowsState("console-1"), nil
			})
			// Fresh controller start: cached state is zero even though Hyper-V VM is running.
			s.ready = false
			s.windowsState = windowsGuestState{}
			s.networkPaused = true
			s.startMonitors()
			time.Sleep(6 * time.Second)
			s.mu.Lock()
			ready, running := s.ready, s.windowsState.Running
			s.mu.Unlock()
			s.closeBackground()
			if calls.Load() == 0 || !ready || !running {
				t.Fatalf("Windows monitor did not reattach running guest at startup: calls=%d ready=%v running=%v", calls.Load(), ready, running)
			}
		})
	})

	t.Run("stopped_guest_checks_once_then_parks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, calls := readyWindowsMonitorServer(t, func(_ int32, _ *Server) (windowsGuestState, error) {
				return windowsGuestState{Running: false}, errors.New("Windows 客机尚未启动")
			})
			s.ready = false
			s.windowsState = windowsGuestState{}
			s.startMonitors()
			time.Sleep(60 * time.Second)
			s.closeBackground()
			if got := calls.Load(); got != 1 {
				t.Fatalf("stopped Windows guest polled %d times in 60s, want 1 startup check then idle", got)
			}
		})
	})
}

// F4: Scheduled proxy-subscription refreshes must still run on the hourly tick
// while the VM is stopped (for both Linux/Windows networkMonitorIdle and macOS
// environmentMonitorIdle) without reintroducing 5 s idle polling.
func TestStoppedVMHourlyTickRefreshesDueProxySubscriptions(t *testing.T) {
	installDueProxySource := func(t *testing.T, s *Server, updatedAgo time.Duration) {
		t.Helper()
		p, err := managedproxy.Load(s.profile)
		if err != nil {
			t.Fatal(err)
		}
		p.Sources = append(p.Sources, managedproxy.Source{
			ID:       "sub-hourly",
			Name:     "Auto Sub",
			URL:      "https://example.invalid/sub.yaml",
			Interval: 1,
			Updated:  time.Now().Add(-updatedAgo),
		})
		if err := p.Save(s.profile); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("linux_stopped_vm", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var physCalls, fetchCalls atomic.Int64
			oldFetch := fetchProxySource
			fetchProxySource = func(_ context.Context, _ *http.Client, _ string) ([]byte, string, error) {
				fetchCalls.Add(1)
				return nil, "", errors.New("synthetic fetch failure")
			}
			t.Cleanup(func() { fetchProxySource = oldFetch })

			s := steadyNetworkServer(t, func(context.Context) (string, error) {
				physCalls.Add(1)
				return "en0", nil
			})
			// Not yet due at startup (updated 30m ago, interval 1h), becomes due before the 1h tick.
			installDueProxySource(t, s, 30*time.Minute)

			s.startMonitors()
			time.Sleep(25 * time.Minute)
			if fetchCalls.Load() != 0 || physCalls.Load() != 0 {
				s.closeBackground()
				t.Fatalf("stopped VM was not idle before subscription became due: phys=%d fetch=%d", physCalls.Load(), fetchCalls.Load())
			}

			// Advance past the 1 h ticker so wakeMonitors fires while subscription is due.
			time.Sleep(40 * time.Minute)
			// Verify no 5 s polling resumed once Attempted was recorded.
			physAfterRefresh := physCalls.Load()
			time.Sleep(5 * time.Minute)
			physLater := physCalls.Load()
			s.closeBackground()

			if fetchCalls.Load() != 1 {
				t.Fatalf("hourly tick on stopped Linux VM refreshed due subscription %d times, want 1", fetchCalls.Load())
			}
			if physLater != physAfterRefresh {
				t.Fatalf("stopped Linux VM kept polling after subscription refresh: phys %d -> %d", physAfterRefresh, physLater)
			}
		})
	})

	t.Run("macos_stopped_vm", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var physCalls, fetchCalls atomic.Int64
			oldFetch := fetchProxySource
			fetchProxySource = func(_ context.Context, _ *http.Client, _ string) ([]byte, string, error) {
				fetchCalls.Add(1)
				return nil, "", errors.New("synthetic fetch failure")
			}
			t.Cleanup(func() { fetchProxySource = oldFetch })

			s := steadyNetworkServer(t, func(context.Context) (string, error) {
				physCalls.Add(1)
				return "en0", nil
			})
			s.macosGuest = &guestos.MacOSProfile{User: "cage"}
			installDueProxySource(t, s, 30*time.Minute)

			s.startMonitors()
			time.Sleep(25 * time.Minute)
			if fetchCalls.Load() != 0 {
				s.closeBackground()
				t.Fatalf("stopped macOS VM refreshed before subscription was due: fetch=%d", fetchCalls.Load())
			}

			time.Sleep(40 * time.Minute)
			physAfterRefresh := physCalls.Load()
			time.Sleep(5 * time.Minute)
			physLater := physCalls.Load()
			s.closeBackground()

			if fetchCalls.Load() != 1 {
				t.Fatalf("hourly tick on stopped macOS VM refreshed due subscription %d times, want 1", fetchCalls.Load())
			}
			if physLater != physAfterRefresh {
				t.Fatalf("stopped macOS VM kept polling after subscription refresh: phys %d -> %d", physAfterRefresh, physLater)
			}
		})
	})
}

// E1: While the VM is stopped, an hourly scheduled refresh that succeeds must
// update the stored subscription without starting the managed proxy core; the
// next environment start applies the refreshed store.
func TestStoppedVMScheduledProxyRefreshUpdatesStoreWithoutStartingCore(t *testing.T) {
	for _, guest := range []string{"linux", "macos"} {
		t.Run(guest+"_stopped_vm", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var fetchCalls, startCalls atomic.Int64
				oldFetch, oldValidate, oldStart := fetchProxySource, validateProxyCore, startProxyCore
				fetchProxySource = func(_ context.Context, _ *http.Client, _ string) ([]byte, string, error) {
					fetchCalls.Add(1)
					return []byte(`proxies: [{name: qa, type: socks5, server: 127.0.0.1, port: 2080, password: fresh-after-stop}]`), "", nil
				}
				validateProxyCore = func(_ context.Context, profile string, candidate managedproxy.Store) error {
					_, err := candidate.Runtime(profile)
					return err
				}
				startProxyCore = func(context.Context, string, managedproxy.Store) error {
					startCalls.Add(1)
					return nil
				}
				t.Cleanup(func() {
					fetchProxySource, validateProxyCore, startProxyCore = oldFetch, oldValidate, oldStart
				})

				s := steadyNetworkServer(t, onEn0)
				if guest == "macos" {
					s.macosGuest = &guestos.MacOSProfile{User: "cage"}
				}
				p, err := managedproxy.Load(s.profile)
				if err != nil {
					t.Fatal(err)
				}
				beforeUpdated := time.Now().Add(-30 * time.Minute)
				p.Sources[0].URL = "https://example.invalid/sub.yaml"
				p.Sources[0].Interval = 1
				p.Sources[0].Updated = beforeUpdated
				p.Sources[0].Attempted = beforeUpdated
				if err := p.Save(s.profile); err != nil {
					t.Fatal(err)
				}

				s.startMonitors()
				// Advance past the 1 h hourly tick so the due subscription refreshes,
				// then confirm monitors stay parked afterward.
				time.Sleep(65 * time.Minute)
				time.Sleep(5 * time.Minute)
				s.closeBackground()

				if got := fetchCalls.Load(); got != 1 {
					t.Fatalf("stopped %s VM refreshed due subscription %d times, want 1", guest, got)
				}
				if got := startCalls.Load(); got != 0 {
					t.Fatalf("stopped %s VM scheduled refresh started proxy core %d times, want 0", guest, got)
				}
				after, err := managedproxy.Load(s.profile)
				if err != nil {
					t.Fatal(err)
				}
				if got := after.Sources[0].Nodes[0].Config["password"]; got != "fresh-after-stop" {
					t.Fatalf("store was not updated with fresh subscription nodes: password=%v", got)
				}
				if !after.Sources[0].Updated.After(beforeUpdated) || after.Sources[0].Error != "" {
					t.Fatalf("refreshed source metadata not updated: updated=%v error=%q", after.Sources[0].Updated, after.Sources[0].Error)
				}
				cfg, err := loadConfig(s.configPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := managedproxy.Applied(s.profile, cfg.ProxyURL, cfg.ProxyRevision); err != nil {
					t.Fatalf("refreshed store must remain applied for next environment start: %v", err)
				}
			})
		})
	}
}

// E2: The 15-minute subscription refresh back-off must hold even when a failed
// refresh cannot stamp sources.json (applyProxy rollback, loadConfig error),
// and macOS setup-pending must not keep stopped-VM monitors awake every 5 s.
func TestStoppedVMProxyRefreshBackoff(t *testing.T) {
	makeSourceDue := func(t *testing.T, s *Server) {
		t.Helper()
		p, err := managedproxy.Load(s.profile)
		if err != nil {
			t.Fatal(err)
		}
		p.Sources[0].URL = "https://example.invalid/sub.yaml"
		p.Sources[0].Interval = 1
		p.Sources[0].Updated = time.Now().Add(-2 * time.Hour)
		p.Sources[0].Attempted = time.Now().Add(-2 * time.Hour)
		if err := p.Save(s.profile); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("apply_failure_rollback_stopped_vm", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var s *Server
			var fetchCalls, startCalls atomic.Int64
			oldFetch, oldValidate, oldStart := fetchProxySource, validateProxyCore, startProxyCore
			fetchProxySource = func(_ context.Context, _ *http.Client, _ string) ([]byte, string, error) {
				fetchCalls.Add(1)
				return []byte(`proxies: [{name: qa, type: socks5, server: 127.0.0.1, port: 1, password: refreshed}]`), "", nil
			}
			validateProxyCore = func(_ context.Context, profile string, candidate managedproxy.Store) error {
				_, err := candidate.Runtime(profile)
				return err
			}
			startProxyCore = func(context.Context, string, managedproxy.Store) error {
				if startCalls.Add(1) >= 20 && s != nil {
					s.beginClose()
				}
				return errors.New("Super Cage 代理端口已被占用，请更换独立端口")
			}
			t.Cleanup(func() {
				fetchProxySource, validateProxyCore, startProxyCore = oldFetch, oldValidate, oldStart
			})

			s = steadyNetworkServer(t, onEn0)
			makeSourceDue(t, s)

			s.startMonitors()
			time.Sleep(30 * time.Minute)
			s.wakeMonitors()
			time.Sleep(5 * time.Minute)
			s.closeBackground()

			if got := fetchCalls.Load(); got > 2 {
				t.Fatalf("stopped VM with failing applyProxy fetched subscription %d times in 30m, want <= 2", got)
			}
			if got := startCalls.Load(); got > 2 {
				t.Fatalf("stopped VM with failing applyProxy called startProxyCore %d times in 30m, want <= 2", got)
			}
		})
	})

	t.Run("apply_failure_rollback_running_then_stopped_vm", func(t *testing.T) {
		s, _ := interfaceMonitorServer(t)
		oldPhys := physicalProxyInterface
		physicalProxyInterface = onEn0
		t.Cleanup(func() { physicalProxyInterface = oldPhys })

		oldCoreRunning := proxyCoreRunning
		proxyCoreRunning = func(context.Context, string, managedproxy.Store) bool { return true }
		t.Cleanup(func() { proxyCoreRunning = oldCoreRunning })

		var cancel context.CancelFunc
		var fetchCalls, startCalls atomic.Int64
		oldFetch, oldValidate, oldStart := fetchProxySource, validateProxyCore, startProxyCore
		fetchProxySource = func(_ context.Context, _ *http.Client, _ string) ([]byte, string, error) {
			fetchCalls.Add(1)
			return []byte(`proxies: [{name: qa, type: socks5, server: 127.0.0.1, port: 1, password: refreshed}]`), "", nil
		}
		validateProxyCore = func(_ context.Context, profile string, candidate managedproxy.Store) error {
			_, err := candidate.Runtime(profile)
			return err
		}
		startProxyCore = func(context.Context, string, managedproxy.Store) error {
			if startCalls.Add(1) >= 20 && cancel != nil {
				cancel()
			}
			return errors.New("Super Cage 代理端口已被占用，请更换独立端口")
		}
		t.Cleanup(func() {
			fetchProxySource, validateProxyCore, startProxyCore = oldFetch, oldValidate, oldStart
		})

		synctest.Test(t, func(t *testing.T) {
			makeSourceDue(t, s)
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.monitorNetwork(ctx)
			}()
			time.Sleep(30 * time.Minute)
			runningFetches := fetchCalls.Load()
			runningStarts := startCalls.Load()

			// Stop the VM right after an applyProxy rollback: the stopped-VM
			// monitor must park instead of retrying every 5 s.
			marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
			if err := os.Remove(marker); err != nil {
				cancel()
				<-done
				s.jobs.close()
				t.Fatal(err)
			}
			fetchesAtStop := fetchCalls.Load()
			time.Sleep(10 * time.Minute)
			fetchesAfterStop := fetchCalls.Load()

			cancel()
			<-done
			s.jobs.close()

			if runningStarts == 0 || runningFetches == 0 {
				t.Fatalf("expected applyProxy rollback path to run at least once: fetches=%d starts=%d", runningFetches, runningStarts)
			}
			if runningFetches > 2 || runningStarts > 2 {
				t.Fatalf("applyProxy rollback retried every 5s: fetches=%d starts=%d in 30m, want <= 2", runningFetches, runningStarts)
			}
			if fetchesAfterStop != fetchesAtStop {
				t.Fatalf("stopped VM after applyProxy rollback kept retrying: fetches %d -> %d", fetchesAtStop, fetchesAfterStop)
			}
		})
	})

	t.Run("load_config_failure_stopped_vm", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var s *Server
			var loadCalls atomic.Int64
			oldLoad := loadConfig
			loadConfig = func(string) (cage.Config, error) {
				if loadCalls.Add(1) >= 50 && s != nil {
					s.beginClose()
				}
				return cage.Config{}, errors.New("synthetic config read failure")
			}
			t.Cleanup(func() { loadConfig = oldLoad })

			s = steadyNetworkServer(t, onEn0)
			makeSourceDue(t, s)

			s.startMonitors()
			time.Sleep(1 * time.Minute)
			if !s.networkMonitorIdle() {
				s.closeBackground()
				t.Fatal("networkMonitorIdle() = false within 15m backoff after loadConfig failure, want true")
			}
			callsAfterFirst := loadCalls.Load()
			// Even an explicit wake inside the 15-minute backoff must not retry.
			time.Sleep(9 * time.Minute)
			s.wakeMonitors()
			time.Sleep(20 * time.Minute)
			s.closeBackground()

			// One round invokes loadConfig a handful of times (networkRoundWanted,
			// monitorProxyInterface, checkProxyHealth, changeProxy); a spin loop
			// over 30 min invokes it repeatedly.
			if got := loadCalls.Load(); got > 10 || got != callsAfterFirst {
				t.Fatalf("stopped VM with failing loadConfig called loadConfig %d times in 30m (after 1m: %d), want <= 10 and no retry while backed off/parked", got, callsAfterFirst)
			}
		})
	})

	t.Run("macos_setup_pending_stopped_vm", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var fetchCalls atomic.Int64
			oldFetch := fetchProxySource
			fetchProxySource = func(_ context.Context, _ *http.Client, _ string) ([]byte, string, error) {
				fetchCalls.Add(1)
				return nil, "", errors.New("unexpected fetch during macOS setup")
			}
			t.Cleanup(func() { fetchProxySource = oldFetch })

			s := steadyNetworkServer(t, onEn0)
			s.macosGuest = &guestos.MacOSProfile{User: "cage", Instance: "mac", SetupID: "pending-setup"}
			makeSourceDue(t, s)

			s.startMonitors()
			time.Sleep(30 * time.Minute)
			s.wakeMonitors()
			time.Sleep(5 * time.Minute)
			s.closeBackground()

			// Under synctest fake time (year 2000) vs real file mtimes, each
			// diagnostics write rotates events.jsonl, so count across all files.
			networkTicks := countAllMonitorTimingRecords(t, s, "network")
			if networkTicks > 2 {
				t.Fatalf("stopped macOS VM with setup pending ran %d network monitor ticks in 30m, want <= 2", networkTicks)
			}
			if fetchCalls.Load() != 0 {
				t.Fatalf("macOS setup pending unexpectedly fetched subscription %d times", fetchCalls.Load())
			}
			if s.proxySubscriptionRefreshDue() {
				t.Fatal("proxySubscriptionRefreshDue() = true while macSetupPending(), want false")
			}
			if !s.environmentMonitorIdle() || !s.networkMonitorIdle() {
				t.Fatalf("idle checks with macSetupPending() = (env=%v, net=%v), want (true, true)", s.environmentMonitorIdle(), s.networkMonitorIdle())
			}
		})
	})
}

func countAllMonitorTimingRecords(t *testing.T, s *Server, kind string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(s.profile, "logs", "events*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if strings.Contains(line, `"source":"network-monitor"`) && strings.Contains(line, "monitor="+kind+" ") {
				count++
			}
		}
	}
	return count
}

func makeValidGuestConnectionPayload(t *testing.T) string {
	t.Helper()
	creds, err := json.Marshal(guestCredentials{Token: "guest-token", DisplayScale: 1})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(cage.RequiredGuestManifest())
	if err != nil {
		t.Fatal(err)
	}
	return string(creds) + "\nvnc-pass\n" + string(manifest) + "\n"
}
