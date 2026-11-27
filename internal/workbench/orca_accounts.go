package workbench

import (
	"context"
	"time"
)

// activeOrcaAccount reads only the running process's binding. A reconfigured
// roster or a dynamic child never creates a service reservation by itself.
func (s *Server) activeOrcaAccount(agentID string) (string, bool) {
	if s.windowsGuest != nil {
		return "", false
	}
	m := &s.orca
	m.mu.Lock()
	if !m.configLoaded {
		if _, err := s.loadOrcaConfigLocked(); err != nil {
			m.mu.Unlock()
			return "", false
		}
	}
	if !m.config.Enabled {
		m.mu.Unlock()
		return "", false
	}
	port := m.config.GuestPort
	status := m.statusCache
	stale := status.Port != port || m.statusAt.IsZero() || time.Since(m.statusAt) >= orcaStatusTTL
	m.mu.Unlock()
	if stale {
		if !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
			return "", false
		}
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), s.statusProbeWaitTimeout())
		fresh, err := s.probeOrcaStatus(ctx, port, false)
		cancel()
		if err == nil {
			status = fresh
		} else {
			m.mu.Lock()
			status = m.statusCache
			m.mu.Unlock()
		}
	}
	id, bound := status.Accounts[agentID]
	return id, bound && status.PID > 0 && status.Port == port && (id == "" || validClaudeID(id))
}

func (s *Server) activeServiceAccounts(agentID string) map[string]int {
	return s.serviceAccountUsage(agentID, "", nil)
}

// A replacing service can credit only its own previously confirmed seat.
func (s *Server) serviceAccountUsage(agentID, replacingT3 string, replacingOrca *string) map[string]int {
	active := s.activeT3CodeAccounts(agentID)
	if replacingT3 != "" && active[replacingT3] > 0 {
		delete(active, replacingT3)
	}
	if id, bound := s.activeOrcaAccount(agentID); bound && (replacingOrca == nil || id != *replacingOrca) {
		if active == nil {
			active = make(map[string]int, 1)
		}
		active[id]++
	}
	return active
}

func orcaFixedAccounts(bindings map[string]any) map[string]string {
	fixed := make(map[string]string)
	for agentID, raw := range bindings {
		binding, ok := raw.(map[string]any)
		if !ok || binding["mode"] == "auto" || binding["mode"] == "magpie" {
			continue
		}
		if id, ok := binding["account_id"].(string); ok {
			fixed[agentID] = id
		}
	}
	return fixed
}
