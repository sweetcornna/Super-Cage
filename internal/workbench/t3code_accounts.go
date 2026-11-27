package workbench

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
)

// A managed T3 server gives its provider children a single binding per
// provider. Resolve these at admission, while the environment lock also
// excludes terminal launch and changes to the account roster.
func (s *Server) resolveT3CodeBindings(cfg t3CodeConfig, workspace string) (map[string]any, map[string]string, error) {
	return s.resolveT3CodeLaunchBindings(cfg, workspace, nil)
}

func (s *Server) resolveT3CodeLaunchBindings(cfg t3CodeConfig, workspace string, replacing map[string]string) (map[string]any, map[string]string, error) {
	choices := defaultT3CodeConfig().AccountBindings
	maps.Copy(choices, cfg.AccountBindings)
	if s.magpieTakesOverScheduling() {
		return s.resolveToolAccountBindings(choices, workspace, "T3 Code")
	}
	bindings := make(map[string]any, len(choices))
	labels := make(map[string]string, len(choices))
	for agentID, choice := range choices {
		if choice = strings.TrimSpace(choice); choice == "" {
			choice = "default"
		}
		binding, label, err := s.resolveT3CodeAccount(agentID, choice, workspace, replacing[agentID])
		if err != nil {
			return nil, nil, fmt.Errorf("T3 Code 无法绑定 %s 账号：%w", agentID, err)
		}
		bindings[agentID], labels[agentID] = binding, label
	}
	return bindings, labels, nil
}

func (s *Server) resolveT3CodeAccount(agentID, choice, workspace, replacing string) (map[string]any, string, error) {
	defaultBinding := map[string]any{"mode": "default"}
	const defaultLabel = "默认环境"
	if agentID == "claude" {
		if choice == "default" {
			return defaultBinding, defaultLabel, nil
		}
		var selection claudeSelection
		var err error
		if choice == "auto" {
			selection, err = s.selectClaudeAccount(choice, false, workspace)
		} else {
			s.claudeAccounts.mu.Lock()
			list, loadErr := s.loadClaudeAccounts()
			s.claudeAccounts.mu.Unlock()
			if loadErr != nil {
				return nil, "", loadErr
			}
			if account := list.find(choice); account == nil || !account.Enabled || account.Status != "ready" {
				return nil, "", fmt.Errorf("指定的 Claude 账号不存在、已停用或未就绪")
			}
			selection, err = s.allocateClaudeBinding(list, workspace, choice, s.readClaudeGuestQuotas)
		}
		if err != nil {
			return nil, "", err
		}
		if selection.ID == "" {
			return defaultBinding, defaultLabel, nil
		}
		s.claudeAccounts.mu.Lock()
		list, err := s.loadClaudeAccounts()
		s.claudeAccounts.mu.Unlock()
		if err != nil {
			return nil, "", err
		}
		account := list.find(selection.ID)
		if account == nil {
			return nil, "", fmt.Errorf("Claude 账号已变化，请重试")
		}
		return map[string]any{"account_id": selection.ID, "guest_id": selection.GuestID, "subject": selection.Subject},
			t3CodeAccountLabel(account.Label, account.Email), nil
	}

	if agentID == "codex" && choice != "default" && s.codexGuestSupported() {
		s.codexAccounts.mu.Lock()
		list, err := s.loadCodexAccounts()
		s.codexAccounts.mu.Unlock()
		if err != nil {
			return nil, "", err
		}
		account := list.find(choice)
		if account != nil || choice == "auto" && scheduleManagesLaunches(list.Scheduling.Mode, list.Scheduling.SelectedAccountID, len(list.Accounts)) {
			var selection codexSelection
			if account == nil {
				selection, err = s.selectCodexAccount("auto", false, workspace)
			} else {
				if epoch, unlocked := s.account.access(); unlocked && account.Enabled && account.Status == "ready" {
					ctx, cancel := context.WithTimeout(epoch, 4*time.Second)
					readings, _ := s.readCodexGuestQuotas(ctx, []string{account.ID})
					cancel()
					if reading, ok := readings[account.ID]; ok && reading.Identity == account.subject() {
						account.mergeQuota(&reading.codexQuota, time.Now())
					}
				}
				_, available := account.headroom(time.Now())
				if !account.Enabled || account.Status != "ready" || !available || s.codexLoginActive(account.ID) {
					return nil, "", fmt.Errorf("指定的 Codex 账号已停用、未就绪、正在登录或额度已耗尽")
				}
				selection = codexSelection{ID: account.ID, GuestID: account.ID, Subject: account.subject()}
			}
			if err != nil {
				return nil, "", err
			}
			account = list.find(selection.ID)
			if account == nil {
				return nil, "", fmt.Errorf("Codex 账号已变化，请重试")
			}
			return map[string]any{"account_id": selection.ID, "guest_id": selection.GuestID, "subject": selection.Subject, "codex_native": true},
				t3CodeAccountLabel(account.Label, account.Email), nil
		}
	}

	spec, _, ok := s.lookupAgentSpec(agentID)
	if !ok {
		if choice == "auto" || choice == "default" {
			return defaultBinding, defaultLabel, nil
		}
		return nil, "", fmt.Errorf("未找到指定的 Agent")
	}
	selection, _, err := s.selectAgentAccountReplacing(spec, choice, false, workspace, replacing)
	if err != nil {
		return nil, "", err
	}
	if selection.ID == "" {
		return defaultBinding, defaultLabel, nil
	}
	if selection.GuestID == "default" {
		defaultBinding["account_id"] = selection.ID
		return defaultBinding, selection.Label, nil
	}
	return map[string]any{"account_id": selection.ID, "guest_id": selection.GuestID, "spec": spec}, selection.Label, nil
}

func t3CodeAccountLabel(label, email string) string {
	if label != "" {
		return label
	}
	return email
}

// Read the account binding of the live guest process, including after a
// controller restart. Probe errors preserve the last confirmed reservation.
// "default" identifies a guest default that had no roster ID at launch.
// Terminal/account callers may already hold the environment admission lock.
func (s *Server) activeT3CodeAccounts(agentID string) map[string]int {
	if s.windowsGuest != nil {
		return nil
	}
	m := &s.t3code
	m.mu.Lock()
	if !m.configLoaded {
		if _, err := s.loadT3CodeConfigLocked(); err != nil {
			m.mu.Unlock()
			return nil
		}
	}
	if !m.config.Enabled || m.config.Mode != "managed" {
		m.mu.Unlock()
		return nil
	}
	port := m.config.Port
	status := m.statusCache
	stale := m.statusAt.IsZero() || time.Since(m.statusAt) >= t3CodeStatusTTL || status.Port != port
	m.mu.Unlock()
	if stale {
		if !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), s.statusProbeWaitTimeout())
		fresh, probeErr := s.probeT3CodeStatus(ctx, port, false)
		cancel()
		if probeErr == nil {
			status = fresh
		}
	}
	if id := status.Accounts[agentID]; status.PID > 0 && status.Port == port && (id == "default" || validClaudeID(id)) {
		return map[string]int{id: 1}
	}
	return nil
}
