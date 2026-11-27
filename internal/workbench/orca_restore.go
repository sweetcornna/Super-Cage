package workbench

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// A missing record predates recovery support. Enabled alone only opens the
// settings UI; it is not permission to start an unused or manually stopped service.
type orcaRunIntent struct {
	Service bool `json:"service"`
	Tunnel  bool `json:"tunnel"`
}

func (s *Server) rememberOrcaService(view *orcaConfig, tunnel bool) error {
	s.orca.mu.Lock()
	defer s.orca.mu.Unlock()
	cfg, err := s.loadOrcaConfigLocked()
	if err != nil || !cfg.Enabled {
		return err
	}
	next := orcaRunIntent{Service: true, Tunnel: tunnel}
	if cfg.RunIntent != nil {
		next.Tunnel = next.Tunnel || cfg.RunIntent.Tunnel
		if *cfg.RunIntent == next {
			view.RunIntent = cfg.RunIntent
			return nil
		}
	}
	cfg.RunIntent = &next
	if err := s.saveOrcaConfigLocked(cfg); err != nil {
		return err
	}
	view.RunIntent = cfg.RunIntent
	return nil
}

func (s *Server) forgetOrcaService() error {
	s.orca.mu.Lock()
	defer s.orca.mu.Unlock()
	cfg, err := s.loadOrcaConfigLocked()
	if err != nil || cfg.RunIntent != nil && !cfg.RunIntent.Service && !cfg.RunIntent.Tunnel {
		return err
	}
	cfg.RunIntent = &orcaRunIntent{}
	return s.saveOrcaConfigLocked(cfg)
}

func (s *Server) ensureRememberedOrcaTunnel(ctx context.Context, cfg *orcaConfig) (*orcaTunnel, error) {
	tunnel, err := s.ensureOrcaTunnel(ctx, cfg.effectiveTunnelPort(), cfg.GuestPort)
	if err != nil {
		return nil, err
	}
	return tunnel, s.rememberOrcaService(cfg, true)
}

func (s *Server) rememberLegacyOrcaService(cfg *orcaConfig, status orcaGuestStatus) error {
	if status.PID <= 0 || !status.Running {
		return nil
	}
	return s.rememberOrcaService(cfg, cfg.PairedEnvID != "" && cfg.effectiveTunnelPort() != 0)
}

// Component maintenance can be the first operation on a legacy controller.
// Remember a positively identified service before its guest is taken down.
func (s *Server) captureOrcaBeforeMaintenance(ctx context.Context) {
	if s.windowsGuest != nil || s.account == nil || ctx.Err() != nil || !s.orca.opMu.TryLock() {
		return
	}
	defer s.orca.opMu.Unlock()
	if _, unlocked := s.account.access(); !unlocked {
		return
	}
	s.orca.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	s.orca.mu.Unlock()
	if err != nil || !cfg.Enabled || cfg.RunIntent != nil || !s.vmRunning() {
		return
	}
	probe, cancel := context.WithTimeout(ctx, s.statusProbeWaitTimeout())
	defer cancel()
	if status, err := s.probeOrcaStatus(probe, cfg.GuestPort, false); err == nil {
		_ = s.rememberLegacyOrcaService(&cfg, status)
	}
}

func (s *Server) restoreOrcaConnection(ctx context.Context) error {
	if s.windowsGuest != nil || s.clientOnly {
		return nil
	}
	if !s.orca.opMu.TryLock() {
		return errTerminalBusy
	}
	defer s.orca.opMu.Unlock()
	s.orca.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	s.orca.mu.Unlock()
	if err != nil || !cfg.Enabled || cfg.RunIntent != nil && !cfg.RunIntent.Service {
		return err
	}
	s.mu.Lock()
	paused := s.networkPaused
	s.mu.Unlock()
	if paused && cfg.BindMode == "tailscale" {
		return errRuntimeRestoreWaitNetwork
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, s.statusProbeWaitTimeout())
	status, err := s.probeOrcaStatus(probeCtx, cfg.GuestPort, true)
	cancelProbe()
	if err != nil {
		return err
	}
	if cfg.RunIntent == nil {
		// The guest reports managed process identity, not just a listening port.
		// Legacy profiles may adopt it while alive, but cannot guess whether a
		// missing process used to run or was deliberately stopped.
		if err := s.rememberLegacyOrcaService(&cfg, status); err != nil {
			return err
		}
		if cfg.RunIntent == nil {
			return nil
		}
	}

	if cfg.BindMode == "tailscale" && status.TailscaleIP == "" {
		return errors.New("Orca Server 等待 Tailscale 地址恢复")
	}
	rebind := cfg.BindMode == "tailscale" && status.BindMode == "tailscale" &&
		status.TailscaleIP != "" && status.BindIP != status.TailscaleIP
	if status.PID <= 0 || !status.Running || rebind {
		if status.PID <= 0 || rebind {
			free, e := s.orcaRestorePortFree(ctx, cfg, status)
			if e != nil {
				return e
			}
			if !free {
				return nil // An independent service owns the endpoint.
			}
		}
		started, _, e := s.ensureManagedOrcaRunning(ctx, rebind, "")
		if e != nil {
			return e
		}
		cfg = started
	}
	if !cfg.RunIntent.Tunnel || cfg.effectiveTunnelPort() == 0 {
		return nil
	}
	_, err = s.ensureOrcaTunnel(ctx, cfg.effectiveTunnelPort(), cfg.GuestPort)
	return err
}

func (s *Server) orcaRestorePortFree(ctx context.Context, cfg orcaConfig, status orcaGuestStatus) (bool, error) {
	address := "127.0.0.1"
	if cfg.BindMode == "tailscale" {
		ip, err := netip.ParseAddr(status.TailscaleIP)
		if err != nil || !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			return false, errors.New("Orca Server 等待 Tailscale 地址恢复")
		}
		address = ip.String()
	}
	if hook := s.orca.restorePortProbe; hook != nil {
		return hook(ctx, address, cfg.GuestPort)
	}
	// Use the same bind address as the managed service, inside the guest.
	// A live external listener, including a wildcard bind, cannot be replaced.
	const portProbe = `import errno,socket,sys
with socket.socket() as listener:
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        listener.bind((sys.argv[1], int(sys.argv[2])))
    except OSError as error:
        if error.errno != errno.EADDRINUSE:
            raise
        print("busy")
    else:
        print("free")`
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := s.guestQuick(probe, "exec "+shellescape.QuoteCommand([]string{s.guestPythonBinary(), "-I", "-c", portProbe, address, strconv.Itoa(cfg.GuestPort)}))
	if err != nil {
		return false, fmt.Errorf("无法确认 Orca Server 客机端口: %w", err)
	}
	return strings.TrimSpace(string(output)) == "free", nil
}
