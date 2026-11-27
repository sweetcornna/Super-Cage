package workbench

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

const schedulingAPI = 1

func canonicalSchedulingMode(mode string) string {
	switch mode {
	case "manual":
		return "manual"
	case "exhaust", "sequential":
		return "exhaust"
	default:
		return "smart"
	}
}

func schedulingModeAfterSelect(mode string) string {
	if canonicalSchedulingMode(mode) == "exhaust" {
		return mode
	}
	return "manual"
}

func newSchedulingRevision() string { return randomToken()[:24] }

func scheduleManagesLaunches(mode, selected string, count int) bool {
	return count > 0 || mode == "manual" || (canonicalSchedulingMode(mode) == "exhaust" && selected != "")
}

// Use the enclosing timestamp for quota snapshots recorded before windows
// started carrying timestamps of their own.
func schedulingObservationTime(window, snapshot, now int64) int64 {
	if window == 0 {
		window = snapshot
	}
	if window <= 0 || window > now+60 {
		return 0
	}
	return window
}

func manualSchedulingError(label string) error {
	return fmt.Errorf("%s 手动调度锁定的账号不可用、未选择或额度已耗尽，请在账号面板确认选择、登录与额度", label)
}

// orderedAccountIndex starts at the selected account. If it is unavailable,
// exhaust walks the remaining roster once, including wraparound.
func orderedAccountIndex[T any](accounts []T, mode, selected string, id func(T) string, eligible func(T) bool) int {
	start := slices.IndexFunc(accounts, func(a T) bool { return id(a) == selected })
	if mode == "manual" {
		if start >= 0 && eligible(accounts[start]) {
			return start
		}
		return -1
	}
	start = max(0, start)
	for offset := range accounts {
		idx := (start + offset) % len(accounts)
		if eligible(accounts[idx]) {
			return idx
		}
	}
	return -1
}

type guestSchedulingCursor struct {
	AccountID               string `json:"account_id"`
	SelectedAccountID       string `json:"selected_account_id"`
	SelectedAccountRevision string `json:"selected_account_revision"`
}

func (c guestSchedulingCursor) matches(selected, revision string) bool {
	return c.AccountID != "" && c.SelectedAccountID == selected && c.SelectedAccountRevision == revision
}

func (s *Server) readGuestSchedulingCursor(agentID string, nativeCodex bool) guestSchedulingCursor {
	if s.windowsGuest != nil {
		return guestSchedulingCursor{}
	}
	epoch, unlocked := s.account.access()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()
	if !unlocked || !ready && s.agentCLIs.runtimeHook == nil {
		return guestSchedulingCursor{}
	}
	ctx, cancel := context.WithTimeout(epoch, 2*time.Second)
	defer cancel()
	var cursor guestSchedulingCursor
	if err := s.runAgentRuntime(ctx, "schedule-cursor", map[string]any{"agent_id": agentID, "codex_native": nativeCodex}, &cursor); err != nil {
		return guestSchedulingCursor{}
	}
	return cursor
}

var errSchedulingAccountMissing = errors.New("未找到选中的账号，请刷新账号列表")

// selectSchedulingAccount is the in-process equivalent of action=select.
// It accepts roster IDs and uses the account registry for the named agent.
func (s *Server) selectSchedulingAccount(agentID, accountID string) error {
	switch agentID {
	case "claude":
		return s.setClaudeSchedulingAccount(accountID)
	case "codex":
		if s.codexGuestSupported() {
			err := s.setCodexSchedulingAccount(accountID)
			if !errors.Is(err, errSchedulingAccountMissing) {
				return err
			}
		}
	}
	return s.setAgentSchedulingAccount(agentID, accountID)
}

func (s *Server) setClaudeSchedulingAccount(id string) error {
	s.claudeAccounts.mu.Lock()
	defer s.claudeAccounts.mu.Unlock()
	list, err := s.loadClaudeAccounts()
	if err != nil {
		return err
	}
	if list.find(id) == nil {
		return errSchedulingAccountMissing
	}
	list.Scheduling.selectAccount(id)
	return s.saveClaudeAccounts(list)
}

func (s *Server) setCodexSchedulingAccount(id string) error {
	s.codexAccounts.mu.Lock()
	defer s.codexAccounts.mu.Unlock()
	list, err := s.loadCodexAccounts()
	if err != nil {
		return err
	}
	if list.find(id) == nil {
		return errSchedulingAccountMissing
	}
	list.Scheduling.selectAccount(id)
	return s.saveCodexAccounts(list)
}

func (s *Server) setAgentSchedulingAccount(agentID, id string) error {
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	if pool := pools[agentID]; pool == nil || pool.find(id) == nil {
		return errSchedulingAccountMissing
	}
	next, pool := editAgentPool(pools, agentID)
	pool.Scheduling.Mode = schedulingModeAfterSelect(pool.Scheduling.Mode)
	pool.Scheduling.SelectedAccountID = id
	pool.Scheduling.SelectedAccountRevision = newSchedulingRevision()
	return s.saveAgentPoolsLocked(next)
}
