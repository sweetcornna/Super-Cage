package workbench

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func TestWorkbenchRestoreScopeKeepsUnstartedAndIndependentOrca(t *testing.T) {
	for _, test := range []struct {
		name      string
		intent    *orcaRunIntent
		occupied  bool
		wantProbe bool
	}{
		{name: "enabled before first use"},
		{name: "manual stop retained", intent: &orcaRunIntent{}},
		{name: "independent listener owns remembered endpoint", occupied: true, wantProbe: true, intent: &orcaRunIntent{Service: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := testServer(t)
			cfg := defaultOrcaConfig(s.profile)
			cfg.Enabled, cfg.RunIntent = true, test.intent
			s.orca.config, s.orca.configLoaded = cfg, true
			var statusCalls, starts int
			s.agentCLIs.runtimeHook = func(_ context.Context, command string, _ any, output any) error {
				switch command {
				case "orca-status":
					statusCalls++
					*output.(*orcaGuestStatus) = orcaGuestStatus{Installed: true, Port: cfg.GuestPort}
				case "orca-start", "orca-restart":
					starts++
				}
				return nil
			}
			var checkedPort bool
			s.orca.restorePortProbe = func(_ context.Context, addr string, port int) (bool, error) {
				checkedPort = true
				if addr != "127.0.0.1" || port != cfg.GuestPort {
					t.Fatalf("unexpected endpoint %s:%d", addr, port)
				}
				return !test.occupied, nil
			}
			if err := s.restoreOrcaConnection(t.Context()); err != nil {
				t.Fatal(err)
			}
			if starts != 0 || checkedPort != test.wantProbe || test.intent != nil && !test.intent.Service && statusCalls != 0 {
				t.Fatalf("cancelled or independent service touched: starts=%d status=%d port probe=%t", starts, statusCalls, checkedPort)
			}
		})
	}
}

func TestWorkbenchRestoreRetriesOrcaTunnelWithoutRestartAndKeepsPause(t *testing.T) {
	s := testServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := defaultOrcaConfig(s.profile)
	cfg.Enabled, cfg.HostTunnelPort = true, hostPort
	cfg.RunIntent = &orcaRunIntent{Service: true, Tunnel: true}
	if err := s.saveOrcaConfigLocked(cfg); err != nil {
		t.Fatal(err)
	}
	s.ready, s.networkPaused = true, true
	var starts, dials atomic.Int32
	tunnelReconnected := make(chan struct{}, 1)
	guest := orcaGuestStatus{Port: cfg.GuestPort, Running: true, PID: 443}
	s.agentCLIs.runtimeHook = func(_ context.Context, command string, _ any, output any) error {
		switch command {
		case "orca-status":
			*output.(*orcaGuestStatus) = guest
		case "orca-start", "orca-restart":
			starts.Add(1)
		case "orca-stop":
			guest = orcaGuestStatus{Port: cfg.GuestPort}
		}
		return nil
	}
	s.orca.tunnelDialHook = func(_ context.Context, port int) (string, error) {
		if port != cfg.GuestPort {
			return "", errors.New("wrong guest port")
		}
		if dials.Add(1) == 1 {
			return "", errors.New("SSH forward temporarily disconnected")
		}
		select {
		case tunnelReconnected <- struct{}{}:
		default:
		}
		return "127.0.0.1:1", nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	defer s.closeOrcaTunnel()
	s.mu.Lock()
	s.queueWorkbenchRuntimeRestoreLocked(restoreRuntimeProxy | restoreRuntimeOrca)
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.monitorWorkbenchRuntime(ctx)
	}()
	select {
	case <-tunnelReconnected:
	case <-ctx.Done():
		t.Fatal("restore monitor never retried the SSH forward")
	}
	s.orca.opMu.Lock()
	s.orca.opMu.Unlock()
	cancel()
	<-done
	if active, port := s.orcaTunnelSnapshot(); !active || port != hostPort || dials.Load() != 2 || starts.Load() != 0 {
		t.Fatalf("reused service did not regain its tunnel: active=%t port=%d attempts=%d starts=%d", active, port, dials.Load(), starts.Load())
	}
	s.mu.Lock()
	s.queueWorkbenchRuntimeRestoreLocked(restoreRuntimeAll)
	if !s.networkPaused || s.runnableRuntimeRestoreLocked()&restoreRuntimeProxy != 0 ||
		s.runtimeRestore.pending&restoreRuntimeProxy == 0 || s.runnableRuntimeRestoreLocked()&restoreRuntimeSessions == 0 {
		s.mu.Unlock()
		t.Fatal("pause discarded either the pending proxy or runnable local sessions")
	}
	s.mu.Unlock()
	if err := s.stopManagedOrca(t.Context(), cfg.GuestPort, "orca-stop"); err != nil {
		t.Fatal(err)
	}
	s.closeOrcaTunnel()
	s.orca.mu.Lock()
	s.orca.configLoaded = false // Read the persisted manual-stop intent again.
	s.orca.mu.Unlock()
	if err := s.restoreOrcaConnection(t.Context()); err != nil {
		t.Fatal(err)
	}
	if active, _ := s.orcaTunnelSnapshot(); active || starts.Load() != 0 {
		t.Fatal("manual stop was undone by reattachment")
	}
}

func TestWorkbenchRestoreOrcaKeepsTailnetPendingUntilResume(t *testing.T) {
	s := testServer(t)
	cfg := defaultOrcaConfig(s.profile)
	cfg.Enabled, cfg.BindMode = true, "tailscale"
	cfg.RunIntent = &orcaRunIntent{Service: true}
	if err := s.saveOrcaConfigLocked(cfg); err != nil {
		t.Fatal(err)
	}
	s.ready, s.networkPaused = true, true
	const previousIP, currentIP = "100.64.0.1", "100.64.0.2"
	status := orcaGuestStatus{Port: cfg.GuestPort, Running: true, PID: 101, BindMode: "tailscale", BindIP: previousIP}
	var probes, restarts int
	portBusy := true
	s.orca.restorePortProbe = func(_ context.Context, address string, port int) (bool, error) {
		if address != currentIP || port != cfg.GuestPort {
			return false, errors.New("checked the wrong tailnet endpoint")
		}
		return !portBusy, nil
	}
	s.agentCLIs.runtimeHook = func(_ context.Context, command string, input, output any) error {
		switch command {
		case "orca-status":
			probes++
			*output.(*orcaGuestStatus) = status
		case "orca-restart":
			if input.(map[string]any)["bind_mode"] != "tailscale" {
				return errors.New("changed the configured bind mode")
			}
			restarts++
			status.PID, status.BindIP = 102, currentIP
			*output.(*orcaGuestStatus) = status
		default:
			return errors.New("unexpected guest command " + command)
		}
		return nil
	}
	if retry, err := s.restoreWorkbenchRuntime(t.Context(), restoreRuntimeOrca); retry != 0 || err != nil || probes != 0 {
		t.Fatalf("paused tailnet request used guest status: retry=%b probes=%d err=%v", retry, probes, err)
	}
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.runtimeRestore.pending&restoreRuntimeOrca == 0 || s.runnableRuntimeRestoreLocked()&restoreRuntimeOrca != 0 {
			t.Fatal("paused tailnet request was discarded or left runnable")
		}
		if err := s.saveNetworkIntentLocked(false); err != nil {
			t.Fatal(err)
		}
		if s.runnableRuntimeRestoreLocked()&restoreRuntimeOrca == 0 {
			t.Fatal("resume did not wake the tailnet request")
		}
	}()
	// Once resumed, address acquisition can fail transiently without mutating
	// the managed service. A subsequent attempt rebinds its same service once.
	if err := s.restoreOrcaConnection(t.Context()); err == nil || restarts != 0 {
		t.Fatalf("missing tailnet address: restarts=%d err=%v", restarts, err)
	}
	status.TailscaleIP = currentIP
	if err := s.restoreOrcaConnection(t.Context()); err != nil || restarts != 0 || status.BindIP != previousIP {
		t.Fatalf("busy tailnet endpoint changed managed service: restarts=%d bound=%q err=%v", restarts, status.BindIP, err)
	}
	portBusy = false
	if err := s.restoreOrcaConnection(t.Context()); err != nil || restarts != 1 || status.BindIP != currentIP {
		t.Fatalf("tailnet rebind: restarts=%d bound=%q err=%v", restarts, status.BindIP, err)
	}
	if err := s.restoreOrcaConnection(t.Context()); err != nil || restarts != 1 {
		t.Fatalf("tailnet service started twice: restarts=%d err=%v", restarts, err)
	}
}

func TestWorkbenchRestoreStoppedProxyRetriesOnlyAppliedNode(t *testing.T) {
	s, gate := interfaceMonitorServer(t)
	s.ready = true
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := managedproxy.Applied(s.profile, cfg.ProxyURL, cfg.ProxyRevision)
	if err != nil {
		t.Fatal(err)
	}
	oldRunning, oldValidate, oldStart := proxyCoreRunning, validateProxyCore, startProxyCore
	t.Cleanup(func() {
		s.jobs.close()
		proxyCoreRunning, validateProxyCore, startProxyCore = oldRunning, oldValidate, oldStart
	})
	var healthy atomic.Bool
	var calls atomic.Int32
	proxyCoreRunning = func(context.Context, string, managedproxy.Store) bool { return healthy.Load() }
	validateProxyCore = func(_ context.Context, profile string, candidate managedproxy.Store) error {
		_, err := candidate.Runtime(profile)
		return err
	}
	startProxyCore = func(_ context.Context, profile string, candidate managedproxy.Store) error {
		if profile != s.profile || candidate.Selected != saved.Selected || candidate.Endpoint() != saved.Endpoint() {
			return errors.New("restore changed the selected proxy")
		}
		if calls.Add(1) == 1 {
			return errors.New("proxy core temporarily unavailable")
		}
		healthy.Store(true)
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
	defer cancel()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	s.retryWorkbenchRuntime(ctx, timer, restoreRuntimeProxy)
	if calls.Load() != 2 || gate.Status().Mode != cage.GateClosed {
		t.Fatalf("core retries=%d, unauthorised gate state=%v", calls.Load(), gate.Status().Mode)
	}
	if err := s.restoreAppliedProxy(ctx); err != nil || calls.Load() != 2 {
		t.Fatalf("healthy existing proxy was restarted: calls=%d err=%v", calls.Load(), err)
	}
}

func TestWorkbenchRuntimeUnlockQueuesLocalRestoreWithoutChangingPause(t *testing.T) {
	s := testServer(t)
	s.account.lock()
	s.ready, s.networkPaused = true, true
	// Use the real account store and HTTP unlock handler with a local test key.
	a := &localAccount{path: s.account.path}
	if _, err := a.createAccount("CageTest", "runtime-restore-test-password", "", false); err != nil {
		t.Fatal(err)
	}
	a.lock()
	t.Cleanup(a.lock)
	s.account = a
	w := request(s, "POST", "/api/account/unlock", s.origin, `{"password":"runtime-restore-test-password"}`, true)
	if w.Code != 200 {
		t.Fatalf("unlock: %d %s", w.Code, w.Body.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runnable := s.runnableRuntimeRestoreLocked()
	if !s.networkPaused || runnable&restoreRuntimeSessions == 0 || runnable&restoreRuntimeExternal == 0 ||
		runnable&restoreRuntimeProxy != 0 || s.runtimeRestore.pending&restoreRuntimeProxy == 0 {
		t.Fatalf("unlock changed paused restoration: paused=%t pending=%b runnable=%b", s.networkPaused, s.runtimeRestore.pending, runnable)
	}
	if err := s.saveNetworkIntentLocked(false); err != nil {
		t.Fatal(err)
	}
	if s.runnableRuntimeRestoreLocked()&restoreRuntimeProxy == 0 {
		t.Fatal("explicit resume did not make the saved proxy request runnable")
	}
	if got := s.jobs.snapshot().Kind; strings.TrimSpace(got) != "" {
		t.Fatalf("account unlock ran an environment job: %s", got)
	}
}
