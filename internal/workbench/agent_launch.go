package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

// Universal guest launch schedule publishing for Codex (native Linux pool) and
// all Agent CLIs (agent-accounts.json), plus unified dynamic/explicit account
// binding resolution shared by T3 Code and Orca Server.

func codexLaunchScheduleOf(list codexAccountList) claudeLaunchSchedule {
	mode := canonicalSchedulingMode(list.Scheduling.Mode)
	selected := list.Scheduling.SelectedAccountID
	schedule := claudeLaunchSchedule{
		Mode: mode, Managed: scheduleManagesLaunches(mode, selected, len(list.Accounts)),
		SelectedAccountID: selected, SelectedAccountRevision: list.Scheduling.SelectedAccountRevision,
		Accounts: []claudeLaunchCandidate{},
	}
	usable := func(a codexAccount) bool { return a.Status == "ready" && a.Email != "" }
	for _, a := range list.Accounts {
		schedule.AccountOrder = append(schedule.AccountOrder, a.ID)
		if a.Email != "" {
			schedule.HistoryAccounts = append(schedule.HistoryAccounts, cliHistoryAccount{a.ID, a.subject(), a.Label})
		}
		if a.Enabled && usable(a) && (mode != "manual" || a.ID == selected) {
			schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.ID, a.subject(), false})
		}
	}
	return schedule
}

func (s *Server) kickCodexLaunchSchedule() {
	s.watchOrcaCodex()
	_, release, err := s.beginActivity(context.Background())
	if err != nil {
		return
	}
	m := &s.codexAccounts
	m.launchMu.Lock()
	if m.launchRunning {
		m.launchAgain = true
		m.launchMu.Unlock()
		release()
		return
	}
	m.launchRunning = true
	m.launchMu.Unlock()
	go func() {
		defer release()
		for {
			s.publishCodexLaunchSchedule()
			m.launchMu.Lock()
			if !m.launchAgain {
				m.launchRunning = false
				m.launchMu.Unlock()
				return
			}
			m.launchAgain = false
			m.launchMu.Unlock()
		}
	}()
}

func (s *Server) publishCodexLaunchSchedule() {
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
	if !unlocked {
		return
	}
	m := &s.codexAccounts
	s.magpie.syncMu.Lock()
	defer s.magpie.syncMu.Unlock()
	if s.magpieTakesOverScheduling() {
		m.mu.Lock()
		already := m.launchPublished == launchPublishedMagpie
		m.mu.Unlock()
		if !already {
			s.disableGuestAccountSchedulingLocked(epoch, false)
		}
		return
	}
	m.mu.Lock()
	list, err := s.loadCodexAccounts()
	published := m.launchPublished
	m.mu.Unlock()
	if err != nil {
		return
	}
	schedule := codexLaunchScheduleOf(list)
	if len(schedule.Accounts) > 0 {
		active, logins := s.codexAccountLoad()
		schedule.updateLoad(active, logins, func(id string) string { return id })
	}
	data, err := json.Marshal(schedule)
	if err != nil {
		return
	}
	if string(data) == published {
		return
	}
	ctx, cancel := context.WithTimeout(epoch, 10*time.Second)
	defer cancel()
	script := `umask 077; d="$HOME/.local/share/cage/codex-accounts"; mkdir -p "$d" && printf '%s' ` + shellescape.Quote(string(data)) +
		` > "$d/.launch.json.tmp" && mv -f "$d/.launch.json.tmp" "$d/launch.json"`
	if _, err := s.guestQuick(ctx, script); err != nil {
		return
	}
	if err := s.runAgentRuntime(ctx, "sync-auto-shims", map[string]any{"magpie_takeover": false}, nil); err != nil {
		return
	}
	m.mu.Lock()
	m.launchPublished = string(data)
	m.mu.Unlock()
}

type agentLaunchCandidate struct {
	ID            string       `json:"id"`
	GuestID       string       `json:"guest_id"`
	Subject       string       `json:"subject,omitempty"`
	Quota         *claudeQuota `json:"quota,omitempty"`
	Label         string       `json:"label,omitempty"`
	Source        string       `json:"source,omitempty"`
	AuthMode      string       `json:"auth_mode,omitempty"`
	HandoffGroup  string       `json:"handoff_group,omitempty"`
	CooldownUntil int64        `json:"cooldown_until,omitempty"`
	MaxConcurrent int          `json:"max_concurrency,omitempty"`
	Active        int          `json:"active,omitempty"`
	Score         float64      `json:"score"`
}

type agentLaunchEntry struct {
	Mode                    string                 `json:"mode"`
	RankingMode             string                 `json:"ranking_mode,omitempty"`
	SelectedAccountID       string                 `json:"selected_account_id,omitempty"`
	SelectedAccountRevision string                 `json:"selected_account_revision,omitempty"`
	AccountOrder            []string               `json:"account_order,omitempty"`
	ProjectPins             map[string]string      `json:"project_pins,omitempty"`
	Candidates              []agentLaunchCandidate `json:"candidates"`
	HistoryAccounts         []cliHistoryAccount    `json:"history_accounts,omitempty"`
	Spec                    agentcatalog.AgentSpec `json:"spec"`
}

type agentLaunchSchedule struct {
	Agents map[string]agentLaunchEntry `json:"agents"`
}

// agentLaunchScheduleOf builds the guest scheduling manifest for every non-Claude
// Agent CLI with a managed launch policy. Eligibility that can change with
// time is evaluated in the guest against the published metadata.
func agentLaunchScheduleOf(
	pools map[string]*agentAccountPool,
	specs []agentcatalog.AgentSpec,
	workspaceMapper func(string) string,
	now time.Time,
) agentLaunchSchedule {
	out := agentLaunchSchedule{Agents: map[string]agentLaunchEntry{}}
	specByID := make(map[string]agentcatalog.AgentSpec, len(specs))
	for _, sp := range specs {
		specByID[sp.ID] = sp
	}
	ids := make([]string, 0, len(pools))
	for id := range pools {
		if id == "claude" || pools[id] == nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		pool := pools[id]
		spec, ok := specByID[id]
		if !ok || !scheduleManagesLaunches(pool.Scheduling.Mode, pool.Scheduling.SelectedAccountID, len(pool.Accounts)) && len(pool.Scheduling.ProjectPins) == 0 {
			continue
		}
		cp := cloneAgentPool(pool)
		sched := cp.Scheduling
		mode := canonicalSchedulingMode(sched.Mode)
		type ranked struct {
			candidate agentLaunchCandidate
			ranking   rankedAgentAccount
		}
		list := make([]ranked, 0, len(cp.Accounts))
		accountOrder := make([]string, 0, len(cp.Accounts))
		for _, a := range cp.Accounts {
			accountOrder = append(accountOrder, a.ID)
			if !a.Enabled || a.Status != "ready" {
				continue
			}
			subject := ""
			if id == "codex" && a.AuthMode == "oauth" && a.Email != "" {
				subject = claudeIdentityHash(a.Email, a.Organization)
			}
			// Publish the baseline ranking. The guest applies live load and
			// can admit a quota-blocked candidate after its window resets.
			var ranking rankedAgentAccount
			if mode == "smart" {
				quota := a.analyzeQuota(now)
				if !quota.eligible {
					quota = quotaTelemetry{headroom: -1, eligible: true}
				}
				ranking = rankedAgentAccount{quota: quota, weight: a.effectiveWeight(), health: a.effectiveHealth(now)}
				ranking.scoreAccount(&a, sched.Mode, false, now, 0)
			}
			list = append(list, ranked{
				ranking: ranking,
				candidate: agentLaunchCandidate{
					ID:            a.ID,
					GuestID:       a.guestID(),
					Subject:       subject,
					Quota:         a.Quota,
					Label:         a.displayName(),
					Source:        a.Source,
					AuthMode:      a.AuthMode,
					HandoffGroup:  a.HandoffGroup,
					CooldownUntil: a.CooldownUntil,
					MaxConcurrent: a.MaxConcurrency,
					Score:         roundAgentScore(ranking.score),
				},
			})
		}
		if mode == "smart" {
			sort.SliceStable(list, func(i, j int) bool {
				return list[i].ranking.betterThan(list[j].ranking)
			})
		}
		cands := make([]agentLaunchCandidate, len(list))
		for i, r := range list {
			cands[i] = r.candidate
		}
		var historyAccounts []cliHistoryAccount
		if id == "codex" {
			for _, a := range cp.Accounts {
				if a.AuthMode == "oauth" && a.Email != "" {
					historyAccounts = append(historyAccounts, cliHistoryAccount{a.guestID(), claudeIdentityHash(a.Email, a.Organization), a.Label})
				}
			}
		}

		var pins map[string]string
		if len(sched.ProjectPins) > 0 {
			pins = make(map[string]string, len(sched.ProjectPins))
			for ws, target := range sched.ProjectPins {
				guestWS := ws
				if workspaceMapper != nil && ws != "" {
					if mapped := workspaceMapper(ws); mapped != "" {
						guestWS = mapped
					}
				}
				pins[guestWS] = target
				if guestWS != ws {
					pins[ws] = target
				}
			}
		}

		compactSpec := agentcatalog.AgentSpec{
			ID:               spec.ID,
			Name:             spec.Name,
			ShortName:        spec.ShortName,
			Binary:           spec.Binary,
			FallbackBinaries: slices.Clone(spec.FallbackBinaries),
			Isolation:        spec.Isolation,
			Auth: agentcatalog.AuthSpec{
				APIKeyEnvVars:   slices.Clone(spec.Auth.APIKeyEnvVars),
				BaseURLEnvVar:   spec.Auth.BaseURLEnvVar,
				ModelEnvVar:     spec.Auth.ModelEnvVar,
				StripEnvOnOAuth: slices.Clone(spec.Auth.StripEnvOnOAuth),
			},
			Terminal: agentcatalog.TerminalSpec{
				EnvDefaults: maps.Clone(spec.Terminal.EnvDefaults),
			},
		}
		rankingMode := ""
		if mode == "smart" && mode != sched.Mode {
			rankingMode = sched.Mode
		}
		out.Agents[id] = agentLaunchEntry{
			Mode:                    mode,
			RankingMode:             rankingMode,
			SelectedAccountID:       sched.SelectedAccountID,
			SelectedAccountRevision: sched.SelectedAccountRevision,
			AccountOrder:            accountOrder,
			ProjectPins:             pins,
			Candidates:              cands,
			HistoryAccounts:         historyAccounts,
			Spec:                    compactSpec,
		}
	}
	return out
}

func (s *Server) kickAgentLaunchSchedule() {
	_, release, err := s.beginActivity(context.Background())
	if err != nil {
		return
	}
	m := &s.agentCLIs
	m.launchMu.Lock()
	if m.launchRunning {
		m.launchAgain = true
		m.launchMu.Unlock()
		release()
		return
	}
	m.launchRunning = true
	m.launchMu.Unlock()
	go func() {
		defer release()
		for {
			s.publishAgentLaunchSchedule()
			m.launchMu.Lock()
			if !m.launchAgain {
				m.launchRunning = false
				m.launchMu.Unlock()
				return
			}
			m.launchAgain = false
			m.launchMu.Unlock()
		}
	}()
}

func (s *Server) publishAgentLaunchSchedule() {
	if s.windowsGuest != nil {
		return
	}
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	if !ready {
		return
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return
	}
	m := &s.agentCLIs
	s.magpie.syncMu.Lock()
	defer s.magpie.syncMu.Unlock()
	if s.magpieTakesOverScheduling() {
		m.mu.Lock()
		already := m.closed || m.launchPublished == launchPublishedMagpie
		m.mu.Unlock()
		if !already {
			s.disableGuestAccountSchedulingLocked(epoch, false)
		}
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	pools, err := s.loadAgentPoolsLocked()
	var poolsSnap map[string]*agentAccountPool
	if err == nil {
		poolsSnap = cloneAgentPools(pools)
	}
	store, storeErr := s.loadAgentStoreLocked()
	published := m.launchPublished
	m.mu.Unlock()
	if err != nil || storeErr != nil {
		return
	}
	specs := agentcatalog.All(store.CustomSpecs)
	schedule := agentLaunchScheduleOf(poolsSnap, specs, s.cliWorkspace, time.Now())
	for id, entry := range schedule.Agents {
		if len(entry.Candidates) == 0 {
			continue
		}
		external := s.activeServiceAccounts(id)
		m.mu.Lock()
		active, logins := s.agentAccountUsageLocked(id, poolsSnap[id], external)
		m.mu.Unlock()
		entry.Candidates = slices.DeleteFunc(entry.Candidates, func(candidate agentLaunchCandidate) bool {
			return logins[candidate.ID]
		})
		for i := range entry.Candidates {
			entry.Candidates[i].Active = active[entry.Candidates[i].ID]
		}
		schedule.Agents[id] = entry
	}
	data, err := json.Marshal(schedule)
	if err != nil {
		return
	}
	if string(data) == published {
		return
	}
	ctx, cancel := context.WithTimeout(epoch, 12*time.Second)
	defer cancel()
	if err := s.runAgentRuntime(ctx, "sync-auto-shims", map[string]any{"schedule": schedule, "magpie_takeover": false}, nil); err != nil {
		return
	}
	m.mu.Lock()
	m.launchPublished = string(data)
	m.mu.Unlock()
}

// kickAllLaunchSchedules publishes Claude, native Codex, and universal Agent CLI
// launch schedules to the guest.
func (s *Server) kickAllLaunchSchedules() {
	s.kickClaudeLaunchSchedule()
	s.kickCodexLaunchSchedule()
	s.kickAgentLaunchSchedule()
}

// A finite-capacity account shares its seats with programs started in the
// guest's own shell. Read their lease counts in one guest call before admission.
func (s *Server) agentGuestLaunchLoad(agentID, requestedID string, includeUncapped bool) (map[string]int, error) {
	if s.windowsGuest != nil {
		return nil, nil
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return nil, nil
	}
	m := &s.agentCLIs
	m.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	pool := pools[agentID]
	if err != nil || pool == nil {
		m.mu.Unlock()
		return nil, err
	}
	ids := make(map[string]string)
	hasCapacityLimit := false
	for _, account := range pool.Accounts {
		if requestedID != "" && requestedID != "auto" && requestedID != account.ID && !(requestedID == "default" && account.Source == "default") {
			continue
		}
		if includeUncapped || account.MaxConcurrency > 0 {
			ids[account.guestID()] = account.ID
			hasCapacityLimit = hasCapacityLimit || account.MaxConcurrency > 0
		}
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if closing || !ready && s.agentCLIs.runtimeHook == nil && !s.vmRunning() {
		return nil, nil
	}
	guestIDs := slices.Sorted(maps.Keys(ids))
	ctx, cancel := context.WithTimeout(epoch, 2*time.Second)
	defer cancel()
	var readings map[string]int
	if err := s.runAgentRuntime(ctx, "plain-launches", map[string]any{"agent_id": agentID, "guest_ids": guestIDs}, &readings); err != nil {
		if hasCapacityLimit || includeUncapped && requestedID != "" && requestedID != "auto" {
			return nil, fmt.Errorf("暂时无法核对客机中正在使用的账号，请重试：%w", err)
		}
		return nil, nil
	}
	active := make(map[string]int)
	for guestID, count := range readings {
		if id := ids[guestID]; id != "" && count > 0 {
			active[id] = count
		}
	}
	return active, nil
}

// resolveToolAccountBindings translates tool-level bindings ("auto", "default",
// or a specific account ID) into guest routing specs and human-readable labels
// for both T3 Code and Orca Server.
//
//   - "auto" uses dynamic live scheduling through ~/.local/share/cage/auto-shims
//     (reading fresh launch.json per invocation, automatically skipping exhausted
//     or cooled-down accounts without requiring a server restart).
//   - "default" explicitly bypasses auto-shims so the CLI uses its default login.
//   - "<accountID>" validates and pins a specific managed account (failing loudly
//     if missing, disabled, or not ready).
func (s *Server) resolveToolAccountBindings(bindings map[string]string, workspace, toolName string) (map[string]any, map[string]string, error) {
	return s.resolveToolAccountBindingsReplacing(bindings, workspace, toolName, nil)
}

func (s *Server) resolveToolAccountBindingsReplacing(bindings map[string]string, workspace, toolName string, replacing map[string]string) (map[string]any, map[string]string, error) {
	guestBindings := map[string]any{}
	labels := map[string]string{}

	// When Magpie is enabled, Super Cage automatically hands off account
	// scheduling to Magpie across all harnesses so every CLI reads its native
	// config written by Magpie without Cage isolation overrides.
	if s.magpieTakesOverScheduling() {
		for agentID, choice := range bindings {
			agentID = strings.TrimSpace(agentID)
			choice = strings.TrimSpace(choice)
			if agentID == "" || choice == "" {
				continue
			}
			guestBindings[agentID] = map[string]any{"mode": "magpie"}
			labels[agentID] = magpieTakeoverLabel
		}
		return guestBindings, labels, nil
	}

	for agentID, choice := range bindings {
		agentID = strings.TrimSpace(agentID)
		choice = strings.TrimSpace(choice)
		if agentID == "" || choice == "" {
			continue
		}
		if choice == "default" {
			guestBindings[agentID] = map[string]any{"mode": "default", "account_id": ""}
			labels[agentID] = "默认环境"
			continue
		}

		if agentID == "claude" {
			s.claudeAccounts.mu.Lock()
			list, err := s.loadClaudeAccounts()
			s.claudeAccounts.mu.Unlock()
			if err != nil {
				return nil, nil, fmt.Errorf("无法读取 %s 绑定的 Claude 账号: %w", toolName, err)
			}
			if choice == "auto" {
				s.kickClaudeLaunchSchedule()
				sched := claudeLaunchScheduleOf(list)
				if sched.Managed {
					guestBindings["claude"] = map[string]any{"mode": "auto"}
					labels["claude"] = "自动调度"
					if len(sched.Accounts) > 0 {
						for _, a := range list.Accounts {
							if a.guestID() == sched.Accounts[0].ID {
								preview := a.Label
								if preview == "" {
									preview = a.Email
								}
								labels["claude"] += " · " + preview
								break
							}
						}
					}
				}
				continue
			}
			chosen := list.find(choice)
			if chosen == nil || !chosen.Enabled || chosen.Status != "ready" {
				return nil, nil, fmt.Errorf("%s 绑定的 Claude 账号不存在、已停用或未就绪，请先更新账号绑定", toolName)
			}
			selected, err := s.allocateClaudeBinding(list, workspace, choice, s.readClaudeGuestQuotas)
			if err != nil {
				return nil, nil, fmt.Errorf("%s 无法绑定 Claude 账号：%w", toolName, err)
			}
			guestBindings["claude"] = map[string]any{
				"account_id": selected.ID,
				"guest_id":   selected.GuestID,
				"subject":    selected.Subject,
			}
			labels["claude"] = chosen.Label
			if labels["claude"] == "" {
				labels["claude"] = chosen.Email
			}
			continue
		}

		// Native Linux Codex pool takes precedence on Linux guests when populated.
		if agentID == "codex" && s.codexGuestSupported() {
			s.codexAccounts.mu.Lock()
			codexList, codexErr := s.loadCodexAccounts()
			s.codexAccounts.mu.Unlock()
			if codexErr != nil {
				return nil, nil, fmt.Errorf("无法读取 %s 绑定的 Codex 账号: %w", toolName, codexErr)
			}
			if scheduleManagesLaunches(codexList.Scheduling.Mode, codexList.Scheduling.SelectedAccountID, len(codexList.Accounts)) {
				if choice == "auto" {
					s.kickCodexLaunchSchedule()
					sched := codexLaunchScheduleOf(codexList)
					guestBindings["codex"] = map[string]any{"mode": "auto", "codex_native": true}
					labels["codex"] = "自动调度"
					if len(sched.Accounts) > 0 {
						preview := ""
						if a := codexList.find(sched.Accounts[0].ID); a != nil {
							preview = a.Label
							if preview == "" {
								preview = a.Email
							}
						}
						if preview != "" {
							labels["codex"] = "自动调度 · " + preview
						}
					}
					continue
				}
				if chosen := codexList.find(choice); chosen != nil {
					_, available := chosen.headroom(time.Now())
					if !chosen.Enabled || chosen.Status != "ready" || !available || s.codexLoginActive(chosen.ID) {
						return nil, nil, fmt.Errorf("%s 绑定的 Codex 账号已停用、未就绪、正在登录或配额已耗尽", toolName)
					}
					guestBindings["codex"] = map[string]any{
						"account_id":   chosen.ID,
						"guest_id":     chosen.ID,
						"subject":      chosen.subject(),
						"codex_native": true,
					}
					labels["codex"] = chosen.Label
					if labels["codex"] == "" {
						labels["codex"] = chosen.Email
					}
					continue
				}
				// Explicit ID not in native Codex pool: fall through to check agent-accounts.json.
			}
		}

		spec, _, ok := s.lookupAgentSpec(agentID)
		if !ok {
			if choice != "auto" {
				return nil, nil, fmt.Errorf("%s 绑定的 Agent (%s) 不存在，请先更新绑定", toolName, agentID)
			}
			continue
		}
		if choice == "auto" {
			s.kickAgentLaunchSchedule()
			external := s.activeServiceAccounts(agentID)
			s.agentCLIs.mu.Lock()
			pools, loadErr := s.loadAgentPoolsLocked()
			pool := cloneAgentPool(pools[agentID])
			active, logins := s.agentAccountUsageLocked(agentID, &pool, external)
			last, recent := s.agentCLIs.lastPick[agentID], s.agentCLIs.recent[agentID][workspace]
			s.agentCLIs.mu.Unlock()
			managed := scheduleManagesLaunches(pool.Scheduling.Mode, pool.Scheduling.SelectedAccountID, len(pool.Accounts)) || len(pool.Scheduling.ProjectPins) > 0
			if loadErr != nil {
				return nil, nil, fmt.Errorf("无法读取 %s 绑定的 %s 账号: %w", toolName, spec.ShortName, loadErr)
			}
			if !managed {
				continue
			}
			guestBindings[agentID] = map[string]any{"mode": "auto", "binary": spec.Binary, "fallback_binaries": spec.FallbackBinaries}
			labels[agentID] = "自动调度"
			preview := pickAgentAccount(pool, active, logins, workspace, last, recent.id, recent.at, time.Now())
			if preview.Label != "" {
				labels[agentID] += " · " + preview.Label
			}
			continue
		}

		var replacingID *string
		if id, ok := replacing[spec.ID]; ok {
			replacingID = &id
		}
		selected, _, err := s.selectAgentAccountForServices(spec, choice, false, workspace, "", replacingID)
		if err != nil {
			return nil, nil, fmt.Errorf("%s 无法绑定 %s 账号：%w", toolName, spec.ShortName, err)
		}
		if selected.GuestID == "default" {
			guestBindings[agentID] = map[string]any{"mode": "default", "account_id": selected.ID}
		} else {
			guestBindings[agentID] = map[string]any{"account_id": selected.ID, "guest_id": selected.GuestID, "spec": spec}
		}
		labels[agentID] = selected.Label
	}
	return guestBindings, labels, nil
}
