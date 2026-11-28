package workbench

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

// Keep the admitted identities and their guest routing, without copying any
// Agent spec, credentials, pairing URLs or process IDs into the run intent.
type t3CodeRunIntent struct {
	Workspace string                          `json:"workspace"`
	Bindings  map[string]t3CodeRestoreBinding `json:"bindings"`
}

type t3CodeRestoreBinding struct {
	Mode        string `json:"mode,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	GuestID     string `json:"guest_id,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Label       string `json:"label,omitempty"`
	CodexNative bool   `json:"codex_native,omitempty"`
}

func snapshotT3CodeRun(workspace string, resolved map[string]any, labels map[string]string) *t3CodeRunIntent {
	intent := &t3CodeRunIntent{Workspace: workspace, Bindings: make(map[string]t3CodeRestoreBinding, len(resolved))}
	for agent, raw := range resolved {
		intent.Bindings[agent] = t3CodeBindingForRestore(raw.(map[string]any), labels[agent])
	}
	return intent
}

func t3CodeBindingForRestore(binding map[string]any, label string) t3CodeRestoreBinding {
	saved := t3CodeRestoreBinding{Label: label}
	saved.Mode, _ = binding["mode"].(string)
	saved.AccountID, _ = binding["account_id"].(string)
	saved.GuestID, _ = binding["guest_id"].(string)
	saved.Subject, _ = binding["subject"].(string)
	saved.CodexNative, _ = binding["codex_native"].(bool)
	return saved
}

func (s *Server) resolveT3CodeRun(intent *t3CodeRunIntent, replacing map[string]string) (map[string]any, map[string]string, error) {
	if len(intent.Bindings) == 0 {
		return nil, nil, fmt.Errorf("T3 Code 缺少原会话的账号绑定，请重启以重新分配")
	}
	resolved := make(map[string]any, len(intent.Bindings))
	labels := make(map[string]string, len(intent.Bindings))
	for agent, saved := range intent.Bindings {
		if saved.Mode == "magpie" || saved.Mode == "scoped-auto" {
			resolved[agent], labels[agent] = map[string]any{"mode": saved.Mode}, saved.Label
			continue
		}
		choice := saved.AccountID
		if choice == "" && saved.Mode == "default" {
			choice = "default"
		} else if !validClaudeID(choice) {
			return nil, nil, fmt.Errorf("T3 Code 原有 %s 账号绑定无效", agent)
		}
		binding, label, err := s.resolveT3CodeAccount(agent, choice, intent.Workspace, replacing[agent])
		if err != nil {
			return nil, nil, fmt.Errorf("T3 Code 无法恢复原有 %s 账号：%w", agent, err)
		}
		actual := t3CodeBindingForRestore(binding, saved.Label)
		if saved != actual && !(saved.Mode == "default" && saved.AccountID == "" && actual.Mode == "default") {
			return nil, nil, fmt.Errorf("T3 Code 原有 %s 账号身份已变化，请重启以重新分配", agent)
		}
		resolved[agent], labels[agent] = binding, label
	}
	return resolved, labels, nil
}

// Older controllers saved account IDs in the guest's running process state.
// Adopt a complete report when the user opens that process. A legacy dynamic
// process without a complete report can still be used and restarted in place.
func (s *Server) adoptT3CodeRun(cfg t3CodeConfig, live t3CodeGuestStatus, workspace string) error {
	choices := defaultT3CodeConfig().AccountBindings
	maps.Copy(choices, cfg.AccountBindings)
	if len(live.Accounts) != len(choices) {
		return nil
	}
	if workspace == "" {
		workspace = s.workspace()
	}
	if workspace != "" {
		path, err := s.projectWorkspace(workspace)
		if err != nil {
			return nil
		}
		workspace = path
	}
	resolved := make(map[string]any, len(choices))
	labels := make(map[string]string, len(choices))
	for agent := range choices {
		account := live.Accounts[agent]
		if account != "default" && !validClaudeID(account) {
			return nil
		}
		binding, label, err := s.resolveT3CodeAccount(agent, account, workspace, account)
		if err != nil {
			return nil
		}
		resolved[agent], labels[agent] = binding, label
	}
	cfg.Run = snapshotT3CodeRun(workspace, resolved, labels)
	s.t3code.mu.Lock()
	defer s.t3code.mu.Unlock()
	s.t3code.activeBindings = labels
	return s.saveT3CodeConfigLocked(cfg)
}

// A live process can be reused without consulting a roster that may have been
// edited while the process was running. Only label a binding it still owns.
func (run *t3CodeRunIntent) activeLabels(accounts map[string]string) map[string]string {
	labels := map[string]string{}
	for agent, saved := range run.Bindings {
		want := saved.AccountID
		if want == "" && saved.Mode == "default" {
			want = "default"
		}
		if owner := accounts[agent]; want == owner {
			labels[agent] = saved.Label
		}
	}
	return labels
}

// A managed T3 server gives its provider children a single binding per
// provider. Resolve these at admission, while the environment lock also
// excludes terminal launch and changes to the account roster.
func (s *Server) resolveT3CodeBindings(cfg t3CodeConfig, workspace string) (map[string]any, map[string]string, error) {
	return s.resolveT3CodeLaunchBindings(cfg, workspace, nil)
}

func (s *Server) resolveT3CodeLaunchBindings(cfg t3CodeConfig, workspace string, replacing map[string]string) (map[string]any, map[string]string, error) {
	choices := defaultT3CodeConfig().AccountBindings
	maps.Copy(choices, cfg.AccountBindings)
	bindings := make(map[string]any, len(choices))
	labels := make(map[string]string, len(choices))
	for agentID, choice := range choices {
		if choice = strings.TrimSpace(choice); choice == "" {
			choice = "default"
		}
		// A route can change after a long-lived service starts and different
		// children may use different scopes. Keep automatic bindings dynamic
		// only for the native shims which can inspect each actual launch.
		if choice == "auto" && (agentID == "claude" || agentID == "codex" && s.codexGuestSupported()) && s.magpieNativeAutoEligible(agentID) {
			native, err := s.magpieNewLaunch(agentID, agentID, workspace)
			if native || errors.Is(err, errMagpieRouteUnknown) {
				bindings[agentID] = map[string]any{"mode": "scoped-auto"}
				labels[agentID] = "按启动配置路由"
				continue
			}
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
