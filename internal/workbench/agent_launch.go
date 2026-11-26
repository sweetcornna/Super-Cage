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
	schedule := claudeLaunchSchedule{Mode: list.Scheduling.defaults().Mode, Accounts: []claudeLaunchCandidate{}}
	usable := func(a codexAccount) bool { return a.Status == "ready" && a.Email != "" }
	if a := list.find(list.Scheduling.Preferred); a != nil && usable(*a) {
		schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.ID, a.subject(), true})
	}
	for _, a := range list.Accounts {
		if a.Enabled && usable(a) && a.ID != list.Scheduling.Preferred {
			schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.ID, a.subject(), false})
		}
	}
	return schedule
}

func (s *Server) kickCodexLaunchSchedule() {
	m := &s.codexAccounts
	m.launchMu.Lock()
	if m.launchRunning {
		m.launchAgain = true
		m.launchMu.Unlock()
		return
	}
	m.launchRunning = true
	m.launchMu.Unlock()
	go func() {
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
	ready := s.ready
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
	data, err := json.Marshal(schedule)
	if err != nil {
		return
	}
	if published == launchPublishedMagpie && len(schedule.Accounts) == 0 {
		m.mu.Lock()
		if m.launchPublished == launchPublishedMagpie {
			m.launchPublished = ""
		}
		m.mu.Unlock()
		return
	}
	if string(data) == published || published == "" && len(schedule.Accounts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(epoch, 10*time.Second)
	defer cancel()
	script := `umask 077; d="$HOME/.local/share/cage/codex-accounts"; mkdir -p "$d" && printf '%s' ` + shellescape.Quote(string(data)) +
		` > "$d/.launch.json.tmp" && mv -f "$d/.launch.json.tmp" "$d/launch.json"`
	if _, err := s.guestQuick(ctx, script); err != nil {
		return
	}
	_ = s.runAgentRuntime(ctx, "sync-auto-shims", map[string]any{"magpie_takeover": false}, nil)
	m.mu.Lock()
	m.launchPublished = string(data)
	m.mu.Unlock()
}

type agentLaunchCandidate struct {
	ID            string  `json:"id"`
	GuestID       string  `json:"guest_id"`
	Label         string  `json:"label,omitempty"`
	Source        string  `json:"source,omitempty"`
	AuthMode      string  `json:"auth_mode,omitempty"`
	HandoffGroup  string  `json:"handoff_group,omitempty"`
	CooldownUntil int64   `json:"cooldown_until,omitempty"`
	Score         float64 `json:"score"`
}

type agentLaunchEntry struct {
	Mode        string                  `json:"mode"`
	ProjectPins map[string]string       `json:"project_pins,omitempty"`
	Candidates  []agentLaunchCandidate  `json:"candidates"`
	Spec        agentcatalog.AgentSpec  `json:"spec"`
}

type agentLaunchSchedule struct {
	Agents map[string]agentLaunchEntry `json:"agents"`
}

// agentLaunchScheduleOf builds the guest scheduling manifest for every non-Claude
// Agent CLI that has accounts in agent-accounts.json, preserving the exact
// multi-factor ranking order computed by pickAgentAccount.
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
		if !ok || len(pool.Accounts) == 0 {
			continue
		}
		cp := cloneAgentPool(pool)
		sched := cp.Scheduling.normalized()
		// Evaluate scores with zero active load and no project pin so we capture
		// each ready account's baseline multi-factor priority (weight, headroom,
		// health EWMA, reset urgency, weekly conservation).
		scorePool := cloneAgentPool(pool)
		scorePool.Scheduling.ProjectPins = nil
		decision := pickAgentAccount(scorePool, map[string]int{}, map[string]bool{}, "", "", "", time.Time{}, now)
		scoreByID := make(map[string]float64, len(decision.Candidates))
		for _, c := range decision.Candidates {
			scoreByID[c.AccountID] = c.Score
		}

		type ranked struct {
			idx  int
			cand agentLaunchCandidate
		}
		var list []ranked
		for idx, a := range cp.Accounts {
			if !a.Enabled || a.Status != "ready" {
				continue
			}
			_, eligible := a.headroom(now)
			if !eligible {
				continue
			}
			list = append(list, ranked{
				idx: idx,
				cand: agentLaunchCandidate{
					ID:            a.ID,
					GuestID:       a.guestID(),
					Label:         a.displayName(),
					Source:        a.Source,
					AuthMode:      a.AuthMode,
					HandoffGroup:  a.HandoffGroup,
					CooldownUntil: a.CooldownUntil,
					Score:         scoreByID[a.ID],
				},
			})
		}
		if len(list) == 0 {
			continue
		}
		if sched.Mode != "sequential" {
			sort.SliceStable(list, func(i, j int) bool {
				if list[i].cand.Score != list[j].cand.Score {
					return list[i].cand.Score > list[j].cand.Score
				}
				return list[i].idx < list[j].idx
			})
		}
		cands := make([]agentLaunchCandidate, len(list))
		for i, r := range list {
			cands[i] = r.cand
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
			ID:              spec.ID,
			Name:            spec.Name,
			ShortName:       spec.ShortName,
			Binary:          spec.Binary,
			FallbackBinaries: slices.Clone(spec.FallbackBinaries),
			Isolation:       spec.Isolation,
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
		out.Agents[id] = agentLaunchEntry{
			Mode:        sched.Mode,
			ProjectPins: pins,
			Candidates:  cands,
			Spec:        compactSpec,
		}
	}
	return out
}

func (s *Server) kickAgentLaunchSchedule() {
	m := &s.agentCLIs
	m.launchMu.Lock()
	if m.launchRunning {
		m.launchAgain = true
		m.launchMu.Unlock()
		return
	}
	m.launchRunning = true
	m.launchMu.Unlock()
	go func() {
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
	data, err := json.Marshal(schedule)
	if err != nil {
		return
	}
	if published == launchPublishedMagpie && len(schedule.Agents) == 0 {
		m.mu.Lock()
		if m.launchPublished == launchPublishedMagpie {
			m.launchPublished = ""
		}
		m.mu.Unlock()
		return
	}
	if string(data) == published || published == "" && len(schedule.Agents) == 0 {
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
// launch schedules to the guest (no-ops with zero SSH calls for empty pools).
func (s *Server) kickAllLaunchSchedules() {
	s.kickClaudeLaunchSchedule()
	s.kickCodexLaunchSchedule()
	s.kickAgentLaunchSchedule()
}

// resolveToolAccountBindings translates tool-level bindings ("auto", "default",
// or a specific account ID) into guest routing specs and human-readable labels
// for both T3 Code and Orca Server.
//
// - "auto" uses dynamic live scheduling through ~/.local/share/cage/auto-shims
//   (reading fresh launch.json per invocation, automatically skipping exhausted
//   or cooled-down accounts without requiring a server restart).
// - "default" explicitly bypasses auto-shims so the CLI uses its default login.
// - "<accountID>" validates and pins a specific managed account (failing loudly
//   if missing, disabled, or not ready).
func (s *Server) resolveToolAccountBindings(bindings map[string]string, workspace, toolName string) (map[string]any, map[string]string, error) {
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
			guestBindings[agentID] = map[string]any{"mode": "default"}
			labels[agentID] = "默认环境"
			continue
		}

		if agentID == "claude" {
			s.claudeAccounts.mu.Lock()
			list, err := s.loadClaudeAccounts()
			s.claudeAccounts.mu.Unlock()
			if err != nil {
				if choice != "auto" {
					return nil, nil, fmt.Errorf("无法读取 %s 绑定的 Claude 账号: %w", toolName, err)
				}
				continue
			}
			if choice == "auto" {
				s.kickClaudeLaunchSchedule()
				sched := claudeLaunchScheduleOf(list)
				if len(sched.Accounts) > 0 {
					guestBindings["claude"] = map[string]any{"mode": "auto"}
					preview := ""
					for _, a := range list.Accounts {
						if a.guestID() == sched.Accounts[0].ID {
							preview = a.Label
							if preview == "" {
								preview = a.Email
							}
							break
						}
					}
					if preview != "" {
						labels["claude"] = "自动调度 · " + preview
					} else {
						labels["claude"] = "自动调度"
					}
				}
				continue
			}
			chosen := list.find(choice)
			if chosen == nil || !chosen.Enabled || chosen.Status != "ready" {
				return nil, nil, fmt.Errorf("%s 绑定的 Claude 账号不存在、已停用或未就绪，请先更新账号绑定", toolName)
			}
			guestBindings["claude"] = map[string]any{
				"guest_id": chosen.guestID(),
				"subject":  claudeIdentityHash(chosen.Email, chosen.Organization),
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
			if codexErr == nil && len(codexList.Accounts) > 0 {
				if choice == "auto" {
					s.kickCodexLaunchSchedule()
					sched := codexLaunchScheduleOf(codexList)
					if len(sched.Accounts) > 0 {
						guestBindings["codex"] = map[string]any{"mode": "auto"}
						preview := ""
						if a := codexList.find(sched.Accounts[0].ID); a != nil {
							preview = a.Label
							if preview == "" {
								preview = a.Email
							}
						}
						if preview != "" {
							labels["codex"] = "自动调度 · " + preview
						} else {
							labels["codex"] = "自动调度"
						}
					}
					continue
				}
				if chosen := codexList.find(choice); chosen != nil {
					if !chosen.Enabled || chosen.Status != "ready" {
						return nil, nil, fmt.Errorf("%s 绑定的 Codex 账号已停用或未就绪，请先更新账号绑定", toolName)
					}
					guestBindings["codex"] = map[string]any{
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
			sel, _, err := s.selectAgentAccount(spec, "auto", false, workspace)
			if err != nil || sel.GuestID == "" || sel.GuestID == "default" {
				continue
			}
			guestBindings[agentID] = map[string]any{"mode": "auto"}
			if sel.Label != "" {
				labels[agentID] = "自动调度 · " + sel.Label
			} else {
				labels[agentID] = "自动调度"
			}
			continue
		}

		s.agentCLIs.mu.Lock()
		pools, loadErr := s.loadAgentPoolsLocked()
		var target *agentAccount
		if loadErr == nil {
			if a := s.ensureAgentPoolLocked(pools, spec.ID).find(choice); a != nil {
				cp := *a
				target = &cp
			}
		}
		s.agentCLIs.mu.Unlock()
		if loadErr != nil || target == nil || !target.Enabled || target.Status != "ready" {
			return nil, nil, fmt.Errorf("%s 绑定的 %s 账号不存在、已停用或未就绪，请先更新账号绑定", toolName, spec.ShortName)
		}
		if target.guestID() != "default" {
			guestBindings[agentID] = map[string]any{
				"guest_id": target.guestID(),
				"spec":     spec,
			}
			labels[agentID] = target.displayName()
		}
	}
	return guestBindings, labels, nil
}
