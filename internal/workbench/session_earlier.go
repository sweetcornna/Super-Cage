package workbench

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Earlier parts of a terminal conversation: what a /clear in its terminal left
// behind (mobileRecord.Cleared, oldest first). They are read only when asked
// for: one guest view per part, at most earlierLimit of the newest.
const earlierLimit = 20

type earlierPart struct {
	CLIID     string    `json:"cli_id"`
	ClearedAt time.Time `json:"cleared_at"`
	Prompt    string    `json:"prompt"`
	Turns     int       `json:"turns"`
}

type earlierView struct {
	earlierPart
	ReadOnly bool         `json:"read_only"`
	Items    []mobileTurn `json:"items"`
}

var errNoEarlierPart = errors.New("这段清除前的对话已不在此会话中，请重新读取")

// earlierRecord is the authorized terminal record of the request.
func (s *Server) earlierRecord(w http.ResponseWriter, r *http.Request) (*mobileRecord, bool) {
	view, terminal, ok := s.authorizedMobileSessionRecord(w, r)
	if !ok {
		return nil, false
	}
	if !terminal || view.Provider == codexProvider {
		http.Error(w, "此会话没有清除前的对话", http.StatusNotFound)
		return nil, false
	}
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, view.ID)
	m.mu.Unlock()
	if err != nil {
		http.Error(w, "无法读取主机上的 CLI 会话记录", 500)
		return nil, false
	}
	return record, true
}

// readEarlier reads one cleared part from the guest with the account and
// configuration it was recorded under.
func (s *Server) readEarlier(ctx context.Context, record *mobileRecord, part clearedPart) (earlierView, error) {
	index := slices.IndexFunc(record.Executions, func(e claudeExecution) bool { return e.CLIID == part.CLIID })
	if index < 0 {
		return earlierView{}, errNoEarlierPart
	}
	execution := record.Executions[index]
	input := map[string]any{"cli_id": part.CLIID, "config": execution.Config, "account": execution.Account.GuestID}
	if err := s.windowsConversationInput(record, input); err != nil {
		return earlierView{}, err
	}
	var current terminalConversation
	if err := s.runCLIChannel(ctx, "view", input, &current); err != nil {
		return earlierView{}, err
	}
	turns := slices.DeleteFunc(slices.Clone(current.Turns), func(turn mobileTurn) bool { return earlierCommand(turn.Prompt) })
	view := earlierView{earlierPart: earlierPart{CLIID: part.CLIID, ClearedAt: part.ClearedAt, Turns: len(turns)}, ReadOnly: true, Items: turns}
	if len(turns) > 0 {
		view.Prompt = clipRunes(strings.TrimSpace(turns[0].Prompt), 80)
	}
	return view, nil
}

// earlierCommand is a turn the CLI wrote for a local command (/clear itself).
func earlierCommand(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	return prompt == "" || strings.HasPrefix(prompt, "<command-") || strings.HasPrefix(prompt, "<local-command")
}

func clipRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// mobileEarlierParts lists the cleared parts that hold a conversation, newest
// first. Parts without any prompt (a /clear right after another) are left out.
func (s *Server) mobileEarlierParts(w http.ResponseWriter, r *http.Request) {
	record, ok := s.earlierRecord(w, r)
	if !ok {
		return
	}
	parts := []earlierPart{}
	for i := len(record.Cleared) - 1; i >= 0 && len(parts) < earlierLimit; i-- {
		part := record.Cleared[i]
		if part.Empty {
			continue
		}
		view, err := s.readEarlier(r.Context(), record, part)
		if err != nil {
			requestError(w, err)
			return
		}
		if view.Turns > 0 {
			parts = append(parts, view.earlierPart)
		}
	}
	jsonReply(w, map[string]any{"parts": parts})
}

func (s *Server) earlierPartOf(w http.ResponseWriter, r *http.Request, record *mobileRecord) (clearedPart, bool) {
	part := record.clearedPart(r.PathValue("cli"))
	if part == nil || part.Empty {
		http.Error(w, errNoEarlierPart.Error(), http.StatusNotFound)
		return clearedPart{}, false
	}
	return *part, true
}

// mobileEarlierPart is one cleared part, read only.
func (s *Server) mobileEarlierPart(w http.ResponseWriter, r *http.Request) {
	record, ok := s.earlierRecord(w, r)
	if !ok {
		return
	}
	part, ok := s.earlierPartOf(w, r, record)
	if !ok {
		return
	}
	view, err := s.readEarlier(r.Context(), record, part)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, view)
}

// mobileSeparateEarlier makes a cleared part an ordinary session again: it
// leaves the entry and gets its own, resumable under the account that
// recorded it.
func (s *Server) mobileSeparateEarlier(w http.ResponseWriter, r *http.Request) {
	record, ok := s.earlierRecord(w, r)
	if !ok {
		return
	}
	if _, ok := s.earlierPartOf(w, r, record); !ok {
		return
	}
	var input struct{}
	if !readJSON(w, r, &input) {
		return
	}
	cliID := r.PathValue("cli")
	view, err := s.readEarlier(r.Context(), record, *record.clearedPart(cliID))
	if err != nil {
		requestError(w, err)
		return
	}
	summary, err := s.separateEarlier(record.ID, cliID, view.Prompt)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, summary)
}

// terminalAccountProblem says why the Claude account a terminal conversation
// was recorded under cannot resume it and what fixes that, or "" when nothing
// is known to be wrong (the guest still checks the login at launch).
func (s *Server) terminalAccountProblem(record *mobileRecord) string {
	selection := record.Account
	if !record.Terminal || selection.ID == "" {
		return ""
	}
	s.claudeAccounts.mu.Lock()
	list, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return ""
	}
	account := list.find(selection.ID)
	label := record.AccountLabel
	if account != nil && account.Label != "" {
		label = account.Label
	}
	if label == "" {
		label = "原账号"
	}
	// Only what the guest's account wrapper refuses at launch (claude-account.py
	// "run": the profile's login must be the recorded subject). A paused or
	// unlisted account whose profile still holds that login resumes as before.
	var reason, fix string
	switch {
	case account == nil || account.guestID() != selection.GuestID:
		return ""
	case account.Status == "signed_out":
		reason, fix = "已退出登录", "重新登录这个账号"
	case account.Status == "changed" || account.Email != "" && claudeIdentityHash(account.Email, account.Organization) != selection.Subject:
		reason, fix = "登录的身份已变化", "用原来的身份重新登录这个账号"
	default:
		return ""
	}
	return "这段会话记录在 Claude 账号「" + label + "」下，该账号" + reason + "，无法接续。请在「Claude 账号管理」中" + fix + "后再打开。"
}

func (s *Server) separateEarlier(id, cliID, title string) (mobileSummary, error) {
	s.claudeAccounts.mu.Lock()
	accounts, err := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()
	if err != nil {
		return mobileSummary{}, err
	}
	m := &s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.record(s.profile, id)
	if err != nil {
		return mobileSummary{}, err
	}
	index := slices.IndexFunc(record.Executions, func(e claudeExecution) bool { return e.CLIID == cliID })
	if record.Deleted || record.clearedPart(cliID) == nil || index < 0 {
		return mobileSummary{}, errNoEarlierPart
	}
	execution := record.Executions[index]
	separate := &mobileRecord{mobileSession: mobileSession{mobileSummary: mobileSummary{ID: uuid.NewString(), Workspace: record.Workspace, Title: title, UpdatedAt: time.Now().UTC()}, Turns: []mobileTurn{}},
		CLIID: cliID, Terminal: true, Resume: true, Account: execution.Account, GuestConfig: execution.Config, Launcher: record.Launcher, GuestWorkspace: record.GuestWorkspace}
	// The part's own account: a handoff since may have moved the entry.
	separate.AccountID = execution.Account.ID
	if account := accounts.find(execution.Account.ID); account != nil {
		separate.AccountLabel = account.Label
	} else if execution.Account.ID == record.Account.ID {
		separate.AccountLabel = record.AccountLabel
	}
	separate.Generation = m.epochLocked()
	separate.changed()
	if err := writePrivate(mobileRecordPath(s.profile, separate.ID), separate); err != nil {
		return mobileSummary{}, err
	}
	record.Executions = slices.Delete(record.Executions, index, index+1)
	record.Cleared = slices.DeleteFunc(record.Cleared, func(part clearedPart) bool { return part.CLIID == cliID })
	record.changed()
	if err := writePrivate(mobileRecordPath(s.profile, record.ID), record); err != nil {
		_ = os.Remove(mobileRecordPath(s.profile, separate.ID))
		return mobileSummary{}, err
	}
	if m.catalog != nil {
		m.catalog[record.ID] = record.summary()
		m.catalog[separate.ID] = separate.summary()
	}
	return separate.summary(), nil
}
