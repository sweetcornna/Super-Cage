package workbench

import (
	"context"
	"encoding/json"
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
	Mode     string                  `json:"mode"`
	Accounts []claudeLaunchCandidate `json:"accounts"`
}

// claudeLaunchScheduleOf orders the candidates as pickClaudeAccount does: the
// preferred account first, then the accounts in automatic scheduling.
func claudeLaunchScheduleOf(list claudeAccountList) claudeLaunchSchedule {
	schedule := claudeLaunchSchedule{Mode: list.Scheduling.defaults().Mode, Accounts: []claudeLaunchCandidate{}}
	usable := func(a claudeAccount) bool { return a.Source == "managed" && a.Status == "ready" && a.Email != "" }
	if a := list.find(list.Scheduling.Preferred); a != nil && usable(*a) {
		schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.guestID(), claudeIdentityHash(a.Email, a.Organization), true})
	}
	for _, a := range list.Accounts {
		if a.Enabled && usable(a) && a.ID != list.Scheduling.Preferred {
			schedule.Accounts = append(schedule.Accounts, claudeLaunchCandidate{a.guestID(), claudeIdentityHash(a.Email, a.Organization), false})
		}
	}
	return schedule
}

// kickClaudeLaunchSchedule publishes in the background, one write at a time;
// a change during a write is published right after it.
func (s *Server) kickClaudeLaunchSchedule() {
	m := &s.claudeAccounts
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
	ready := s.ready
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
	data, err := json.Marshal(schedule)
	// Without accounts there is nothing to tell a guest that was never told
	// anything; an emptied list still replaces one published before.
	if err != nil || string(data) == published || published == "" && len(schedule.Accounts) == 0 {
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
	_ = s.runAgentRuntime(ctx, "sync-auto-shims", map[string]any{}, nil)
	m.mu.Lock()
	m.launchPublished = string(data)
	m.mu.Unlock()
}
