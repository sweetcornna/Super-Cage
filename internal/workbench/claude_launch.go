package workbench

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// A plain `claude` typed in a guest shell (a Herdr terminal, say) is not
// launched by Super Cage, so it never asks the scheduler for an account. The
// controller publishes the scheduler's candidates instead; the guest helper
// (cage-claude-account auto) takes the first one whose own quota reading is
// not used up. Nothing is polled: the list is written when it changes and
// once each time the environment becomes ready.
type claudeLaunchCandidate struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	Preferred bool   `json:"preferred,omitempty"`
}

type claudeLaunchSchedule struct {
	Mode                    string                  `json:"mode"`
	SelectedAccountID       string                  `json:"selected_account_id,omitempty"`
	SelectedAccountRevision string                  `json:"selected_account_revision,omitempty"`
	AccountOrder            []string                `json:"account_order,omitempty"`
	Managed                 bool                    `json:"managed"`
	Accounts                []claudeLaunchCandidate `json:"accounts"`
	Active                  map[string]int          `json:"active,omitempty"`
	// History stays addressable when a known profile stops taking new work.
	HistoryAccounts []cliHistoryAccount `json:"history_accounts,omitempty"`
}

type cliHistoryAccount struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Label   string `json:"label,omitempty"`
}

func (schedule *claudeLaunchSchedule) updateLoad(active map[string]int, logins map[string]bool, guestID func(string) string) {
	for id, count := range active {
		if guest := guestID(id); guest != "" && count > 0 {
			if schedule.Active == nil {
				schedule.Active = make(map[string]int)
			}
			schedule.Active[guest] = count
		}
	}
	busy := make(map[string]bool)
	for id := range logins {
		busy[guestID(id)] = true
	}
	schedule.Accounts = slices.DeleteFunc(schedule.Accounts, func(candidate claudeLaunchCandidate) bool {
		return busy[candidate.ID]
	})
}

// claudeLaunchScheduleOf orders the candidates as pickClaudeAccount does: the
// preferred account first, then the accounts in automatic scheduling.
func claudeLaunchScheduleOf(list claudeAccountList) claudeLaunchSchedule {
	mode := canonicalSchedulingMode(list.Scheduling.Mode)
	selected := list.Scheduling.claudeSelectedAccountID()
	schedule := claudeLaunchSchedule{
		Mode: mode, Managed: scheduleManagesLaunches(mode, selected, len(list.Accounts)),
		SelectedAccountID: selected, SelectedAccountRevision: list.Scheduling.SelectedAccountRevision,
		Accounts: []claudeLaunchCandidate{},
	}
	if a := list.find(selected); a != nil {
		schedule.SelectedAccountID = a.guestID()
	}
	usable := func(a claudeAccount) bool { return a.Status == "ready" && a.Email != "" }
	legacyPreferred := list.Scheduling.Preferred
	if mode != "smart" && list.Scheduling.SelectedAccountID != "" {
		legacyPreferred = ""
	}
	if a := list.find(legacyPreferred); a != nil && usable(*a) && (mode != "manual" || a.ID == selected) {
		schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.guestID(), claudeIdentityHash(a.Email, a.Organization), true})
	}
	var defaultHistory []cliHistoryAccount
	for _, a := range list.Accounts {
		schedule.AccountOrder = append(schedule.AccountOrder, a.guestID())
		if a.Email != "" {
			history := cliHistoryAccount{a.guestID(), claudeIdentityHash(a.Email, a.Organization), a.Label}
			if history.ID != "default" {
				schedule.HistoryAccounts = append(schedule.HistoryAccounts, history)
			}
			// A distinct ProfileID is assigned when an enrolled default login
			// gains an independent profile. Old sessions still live in ~/.claude.
			if history.ID == "default" || a.ProfileID != "" && a.ProfileID != a.ID {
				history.ID = "default"
				if history.Label != "" {
					history.Label = "默认配置 · " + history.Label
				}
				defaultHistory = append(defaultHistory, history)
			}
		}
		if a.Enabled && usable(a) && a.ID != legacyPreferred && (mode != "manual" || a.ID == selected) {
			schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.guestID(), claudeIdentityHash(a.Email, a.Organization), false})
		}
	}
	if len(defaultHistory) == 1 {
		schedule.HistoryAccounts = append(schedule.HistoryAccounts, defaultHistory[0])
	}
	return schedule
}

// kickClaudeLaunchSchedule publishes in the background, one write at a time;
// a change during a write is published right after it.
func (s *Server) kickClaudeLaunchSchedule() {
	_, release, err := s.beginActivity(context.Background())
	if err != nil {
		return
	}
	m := &s.claudeAccounts
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
			s.publishClaudeLaunchSchedule()
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

func (s *Server) publishClaudeLaunchSchedule() {
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
	m := &s.claudeAccounts
	m.mu.Lock()
	list, err := s.loadClaudeAccounts()
	published := m.launchPublished
	m.mu.Unlock()
	if err != nil {
		return
	}
	schedule := claudeLaunchScheduleOf(list)
	if len(schedule.Accounts) > 0 {
		active, logins := s.claudeAccountLoad()
		list.applyDefaultServiceUsage(active)
		schedule.updateLoad(active, logins, func(id string) string {
			if a := list.find(id); a != nil {
				return a.guestID()
			}
			return ""
		})
	}
	data, err := json.Marshal(schedule)
	if err != nil {
		return
	}
	// Even an empty roster supersedes policy persisted by an earlier controller.
	if string(data) == published {
		return
	}
	ctx, cancel := context.WithTimeout(epoch, 10*time.Second)
	defer cancel()
	script := `umask 077; d="$HOME/.local/share/cage/claude-accounts"; mkdir -p "$d" && printf '%s' ` + shellescape.Quote(string(data)) +
		` > "$d/.launch.json.tmp" && mv -f "$d/.launch.json.tmp" "$d/launch.json"`
	if _, err := s.guestQuick(ctx, script); err != nil {
		return
	}
	// Keep ~/.local/share/cage/auto-shims/claude synced so non-shell direct exec
	// spawns (Orca Structured Chat, T3 Code, scripts) also honor Claude scheduling.
	if err := s.runAgentRuntime(ctx, "sync-auto-shims", map[string]any{}, nil); err != nil {
		return
	}
	m.mu.Lock()
	m.launchPublished = string(data)
	m.mu.Unlock()
}
