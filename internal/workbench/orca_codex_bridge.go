package workbench

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	orcaCodexWatchInterval = 3 * time.Second
	orcaCodexWatchBackoff  = 10 * time.Second
	orcaCodexFullProbeTTL  = time.Minute
)

type orcaCodexBridge struct {
	epoch  context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	view   orcaCodexBridgeView // guarded by orca.mu
}

type orcaCodexBridgeView struct {
	State                 string `json:"state"`
	AccountID             string `json:"account_id,omitempty"`
	RegistrationSupported bool   `json:"-"`
}

type orcaCodexSnapshot struct {
	Available             bool   `json:"available"`
	Unchanged             bool   `json:"unchanged"`
	Fingerprint           string `json:"fingerprint"`
	RuntimeID             string `json:"runtime_id"`
	RegistrationSupported bool   `json:"registration_supported"`
	OrcaAccountID         string `json:"orca_account_id"`
	CageAccountID         string `json:"cage_account_id"`
	Subject               string `json:"subject"`
}

type orcaCodexRegistration struct {
	Registered    bool   `json:"registered"`
	Created       bool   `json:"created"`
	Selected      bool   `json:"selected"`
	CageAccountID string `json:"cage_account_id"`
	Subject       string `json:"subject,omitempty"`
}

type orcaCodexSelection struct {
	ID       string
	Revision string
}

type orcaCodexObservation struct {
	cage     orcaCodexSelection
	pending  string
	observed bool
	native   orcaCodexSnapshot
	probedAt time.Time
}

func (s *Server) orcaCodexBridgeView() orcaCodexBridgeView {
	s.orca.mu.Lock()
	defer s.orca.mu.Unlock()
	if bridge := s.orca.codexBridge; bridge != nil && bridge.epoch.Err() == nil {
		return bridge.view
	}
	return orcaCodexBridgeView{State: "inactive"}
}

func (s *Server) watchOrcaCodex() {
	if !s.codexGuestSupported() {
		return
	}
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	if !ready {
		return
	}
	epoch, unlocked := s.account.access()
	if !unlocked || epoch.Err() != nil {
		return
	}
	m := &s.orca
	m.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	if err != nil || !orcaCodexBridgeEnabled(cfg) {
		m.mu.Unlock()
		return
	}
	if current := m.codexBridge; current != nil {
		if current.epoch == epoch {
			select {
			case current.wake <- struct{}{}:
			default:
			}
			m.mu.Unlock()
			return
		}
		current.cancel()
	}
	ctx, cancel := context.WithCancel(epoch)
	bridge := &orcaCodexBridge{epoch: epoch, cancel: cancel, wake: make(chan struct{}, 1), view: orcaCodexBridgeView{State: "observing"}}
	m.codexBridge = bridge
	m.mu.Unlock()
	go s.runOrcaCodexBridge(ctx, bridge)
}

func orcaCodexBridgeEnabled(cfg orcaConfig) bool {
	choice := cfg.AccountBindings["codex"]
	return choice == "" || choice == "auto"
}

func orcaCodexManagedPort(cfg orcaConfig) int {
	if cfg.Enabled {
		return cfg.GuestPort
	}
	return 0
}

func (s *Server) registerOrcaCodexHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID string `json:"account_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.AccountID = strings.TrimSpace(in.AccountID)
	if !validClaudeID(in.AccountID) {
		http.Error(w, "无效的 Cage Codex 账号", http.StatusBadRequest)
		return
	}
	result, err := s.registerOrcaCodex(r.Context(), in.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	result.Subject = ""
	jsonReply(w, result)
}

func (s *Server) registerOrcaCodex(ctx context.Context, accountID string) (orcaCodexRegistration, error) {
	var result orcaCodexRegistration
	epoch, unlocked := s.account.access()
	if !unlocked || epoch.Err() != nil {
		return result, errClaudeAccessLost
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stop := context.AfterFunc(epoch, cancel)
	defer stop()
	s.orca.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	s.orca.mu.Unlock()
	if err != nil {
		return result, err
	}
	if !s.codexGuestSupported() || !orcaCodexBridgeEnabled(cfg) || s.magpieTakesOverScheduling() {
		return result, errors.New("当前环境未启用 Orca Codex 账号登记")
	}
	s.codexAccounts.mu.Lock()
	list, err := s.loadCodexAccounts()
	s.codexAccounts.mu.Unlock()
	if err != nil {
		return result, err
	}
	roster := orcaCodexRoster(list)
	subject := roster[accountID]
	if subject == "" || s.codexLoginActive(accountID) {
		return result, errors.New("请先在 Cage 验证此 Codex 账号和 ChatGPT 工作区")
	}
	for id, identity := range roster {
		if id != accountID && identity == subject {
			return result, errors.New("此 Codex 身份对应多个 Cage 账号，无法安全登记")
		}
	}
	err = s.runAgentRuntime(ctx, "orca-codex-accounts", map[string]any{
		"managed_port": orcaCodexManagedPort(cfg), "accounts": map[string]string{accountID: subject}, "register": accountID,
	}, &result)
	if err != nil {
		return result, err
	}
	if !result.Registered || result.CageAccountID != accountID || result.Subject != subject {
		return result, errors.New("未找到此来宾用户已运行且支持登记的 Orca Server，或 Orca 无法验证所选 Codex 账号")
	}
	s.watchOrcaCodex()
	return result, nil
}

func (s *Server) runOrcaCodexBridge(ctx context.Context, bridge *orcaCodexBridge) {
	var seen orcaCodexObservation
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer bridge.cancel()
	defer func() {
		s.orca.mu.Lock()
		if s.orca.codexBridge == bridge {
			s.orca.codexBridge = nil
		}
		s.orca.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-bridge.wake:
		case <-timer.C:
		}
		s.orca.mu.Lock()
		cfg := s.orca.config
		enabled := s.orca.codexBridge == bridge && orcaCodexBridgeEnabled(cfg)
		s.orca.mu.Unlock()
		if !enabled {
			return
		}
		s.mu.Lock()
		ready := s.ready && !s.closing
		s.mu.Unlock()
		if !ready || !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
			return
		}
		view := s.reconcileOrcaCodex(ctx, cfg, &seen)
		s.orca.mu.Lock()
		if s.orca.codexBridge == bridge {
			bridge.view = view
		}
		s.orca.mu.Unlock()
		delay := orcaCodexWatchInterval
		if view.State != "linked" {
			delay = orcaCodexWatchBackoff
		}
		timer.Reset(delay)
	}
}

func orcaCodexRoster(list codexAccountList) map[string]string {
	roster := make(map[string]string)
	for _, account := range list.Accounts {
		if account.Enabled && account.Status == "ready" && account.Email != "" && account.Organization != "" {
			roster[account.ID] = account.subject()
		}
	}
	return roster
}

func (s *Server) reconcileOrcaCodex(ctx context.Context, cfg orcaConfig, seen *orcaCodexObservation) orcaCodexBridgeView {
	view := orcaCodexBridgeView{State: "unmatched"}
	if s.magpieTakesOverScheduling() {
		*seen = orcaCodexObservation{}
		return orcaCodexBridgeView{State: "inactive"}
	}
	s.codexAccounts.mu.Lock()
	list, err := s.loadCodexAccounts()
	s.codexAccounts.mu.Unlock()
	if err != nil {
		return orcaCodexBridgeView{State: "unavailable"}
	}
	roster := orcaCodexRoster(list)
	if len(roster) == 0 {
		*seen = orcaCodexObservation{}
		return view
	}
	cage := orcaCodexSelection{list.Scheduling.SelectedAccountID, list.Scheduling.SelectedAccountRevision}
	query := map[string]any{"managed_port": orcaCodexManagedPort(cfg), "accounts": roster}
	if seen.cage == cage && time.Since(seen.probedAt) < orcaCodexFullProbeTTL {
		query["fingerprint"] = seen.native.Fingerprint
	}
	base, err := s.readOrcaCodex(ctx, query)
	if err != nil {
		return orcaCodexBridgeView{State: "unavailable"}
	}
	if !base.Available {
		*seen = orcaCodexObservation{}
		return orcaCodexBridgeView{State: "inactive"}
	}
	fresh := !base.Unchanged
	if fresh {
		seen.probedAt = time.Now()
	} else {
		base = seen.native
	}
	view.RegistrationSupported = base.RegistrationSupported
	if !seen.observed || seen.cage != cage || seen.native.RuntimeID != base.RuntimeID {
		seen.pending = cage.ID
	} else if seen.native.OrcaAccountID != base.OrcaAccountID {
		// A switch in the native UI takes effect even when the previous Cage
		// selection had no Orca counterpart.
		seen.pending = ""
		if matchedOrcaCodex(roster, base) && base.CageAccountID != cage.ID {
			if next, ok := s.applyOrcaCodexSelection(ctx, cage, base); ok {
				cage = next
			}
		}
	}
	seen.observed, seen.cage, seen.native = true, cage, base

	if fresh && seen.pending != "" && roster[seen.pending] != "" && base.CageAccountID != seen.pending {
		query["target"], query["expected_orca_account_id"] = seen.pending, base.OrcaAccountID
		query["expected_runtime_id"] = base.RuntimeID
		changed, changeErr := s.readOrcaCodex(ctx, query)
		if changeErr != nil {
			return orcaCodexBridgeView{State: "unavailable"}
		}
		if !changed.Available {
			return orcaCodexBridgeView{State: "inactive"}
		}
		if changed.CageAccountID == seen.pending && matchedOrcaCodex(roster, changed) {
			base, seen.native = changed, changed
		}
	}
	if matchedOrcaCodex(roster, base) && (cage.ID == "" || base.CageAccountID == cage.ID) {
		seen.pending = ""
		return orcaCodexBridgeView{State: "linked", AccountID: base.CageAccountID, RegistrationSupported: view.RegistrationSupported}
	}
	return view
}

func matchedOrcaCodex(roster map[string]string, reading orcaCodexSnapshot) bool {
	return reading.CageAccountID != "" && reading.Subject != "" && roster[reading.CageAccountID] == reading.Subject
}

func (s *Server) readOrcaCodex(ctx context.Context, query map[string]any) (orcaCodexSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var result orcaCodexSnapshot
	err := s.runAgentRuntime(ctx, "orca-codex-accounts", query, &result)
	return result, err
}

func (s *Server) applyOrcaCodexSelection(ctx context.Context, previous orcaCodexSelection, reading orcaCodexSnapshot) (orcaCodexSelection, bool) {
	s.codexAccounts.mu.Lock()
	defer s.codexAccounts.mu.Unlock()
	list, err := s.loadCodexAccounts()
	if err != nil || ctx.Err() != nil || previous.ID != list.Scheduling.SelectedAccountID ||
		previous.Revision != list.Scheduling.SelectedAccountRevision || !matchedOrcaCodex(orcaCodexRoster(list), reading) {
		return previous, false
	}
	list.Scheduling.selectAccount(reading.CageAccountID)
	if s.saveCodexAccounts(list) != nil {
		return previous, false
	}
	return orcaCodexSelection{list.Scheduling.SelectedAccountID, list.Scheduling.SelectedAccountRevision}, true
}
