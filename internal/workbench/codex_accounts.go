package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Codex accounts mirror the Claude roster: the guest helper owns each
// account's private CODEX_HOME and only public status fields reach the host.
type codexAccount struct {
	ID             string      `json:"id"`
	Label          string      `json:"label"`
	Enabled        bool        `json:"enabled"`
	Status         string      `json:"status"`
	Email          string      `json:"email,omitempty"`
	Organization   string      `json:"organization,omitempty"`
	Plan           string      `json:"plan,omitempty"`
	Quota          *codexQuota `json:"quota,omitempty"`
	CheckedAt      int64       `json:"checked_at,omitempty"`
	UsageStatus    string      `json:"usage_status,omitempty"`
	UsageCheckedAt int64       `json:"usage_checked_at,omitempty"`
	UsageChecking  bool        `json:"usage_checking,omitempty"`
}

// Codex reports a short (primary) and a long (secondary) rolling window. Their
// durations come from the account's plan, so the window length is kept.
type codexQuota struct {
	Primary    *codexWindow `json:"primary,omitempty"`
	Secondary  *codexWindow `json:"secondary,omitempty"`
	ObservedAt int64        `json:"observed_at"`
}

type codexWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
	WindowMinutes  int64   `json:"window_minutes,omitempty"`
	ObservedAt     int64   `json:"observed_at,omitempty"`
}

type codexGuestSnapshot struct {
	LoggedIn       bool        `json:"logged_in"`
	AuthMode       string      `json:"auth_mode,omitempty"`
	Email          string      `json:"email"`
	Organization   string      `json:"organization"`
	Plan           string      `json:"plan"`
	Quota          *codexQuota `json:"quota"`
	UsageStatus    string      `json:"usage_status,omitempty"`
	UsageCheckedAt int64       `json:"usage_checked_at,omitempty"`
}

type codexGuestQuota struct {
	codexQuota
	Identity      string `json:"identity"`
	PlainLaunches int    `json:"plain_launches,omitempty"`
}

// Scheduling uses the same modes and switch policies as Claude, stored
// independently. Codex terminals have no in-session handoff, so only the mode
// changes which account a new terminal receives.
type codexAccountList struct {
	Accounts     []codexAccount   `json:"accounts"`
	PendingLogin string           `json:"pending_login,omitempty"`
	Scheduling   claudeScheduling `json:"scheduling"`
}

type codexSelection struct{ ID, GuestID, Subject string }

type codexAccountManager struct {
	mu             sync.Mutex
	probe          func(context.Context, string) (codexGuestSnapshot, error)
	usageProbe     func(context.Context, string, string) (codexGuestSnapshot, error)
	lastPick       string
	recent         map[string]claudeProjectPick
	refreshing     map[string]*claudeRefresh
	attempted      map[string]time.Time
	usageActive    map[string]*claudeUsageCall
	panelRunning   bool
	panelUntil     time.Time
	loginNotice    string
	defaultChecked time.Time
	defaultReading codexGuestSnapshot
	defaultError   error
	defaultGate    chan struct{}
	// unrecorded is a pending login directory last seen holding a sign-in
	// that cannot become an account, such as an API key login.
	unrecorded string
	// labels caches codexAccountLabels for the accounts file labelsFile
	// described: lists poll every 250 ms.
	labels     map[string]string
	labelsFile os.FileInfo
	// launchPublished is the candidate list the guest holds (agent_launch.go).
	launchPublished string
	launchMu        sync.Mutex
	launchRunning   bool
	launchAgain     bool
}

func (s *Server) loadCodexAccounts() (codexAccountList, error) {
	list := codexAccountList{Accounts: []codexAccount{}, Scheduling: (claudeScheduling{}).defaults()}
	data, err := os.ReadFile(filepath.Join(s.profile, "codex-accounts.json"))
	if os.IsNotExist(err) {
		return list, nil
	}
	if err != nil || json.Unmarshal(data, &list) != nil || list.Accounts == nil {
		return list, errors.New("Codex 账号文件无法读取，请检查配置目录或恢复备份")
	}
	list.Scheduling = list.Scheduling.defaults()
	if !list.Scheduling.valid() {
		return list, errors.New("Codex 调度设置无效，请检查配置文件")
	}
	ids := make(map[string]bool)
	for _, a := range list.Accounts {
		if !validClaudeID(a.ID) || ids[a.ID] {
			return list, errors.New("Codex 账号文件含无效或重复的记录，请从备份恢复")
		}
		ids[a.ID] = true
	}
	if list.PendingLogin != "" && (!validClaudeID(list.PendingLogin) || ids[list.PendingLogin]) {
		return list, errors.New("Codex 登录记录损坏，请恢复备份")
	}
	return list, nil
}

func (s *Server) saveCodexAccounts(list codexAccountList) error {
	if err := writePrivate(filepath.Join(s.profile, "codex-accounts.json"), list); err != nil {
		return errors.New("Codex 账号设置未保存，请检查配置目录权限和磁盘空间")
	}
	s.kickCodexLaunchSchedule()
	return nil
}

func (list codexAccountList) find(id string) *codexAccount {
	for i := range list.Accounts {
		if list.Accounts[i].ID == id {
			return &list.Accounts[i]
		}
	}
	return nil
}

func (list codexAccountList) bySubject(subject string) *codexAccount {
	if subject == "" {
		return nil
	}
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.Email != "" && a.subject() == subject {
			return a
		}
	}
	return nil
}

func (a codexAccount) subject() string { return claudeIdentityHash(a.Email, a.Organization) }

func (list *codexAccountList) change(input codexAccountInput) error {
	a := list.find(input.ID)
	if a == nil {
		return errors.New("未找到 Codex 账号，请刷新面板")
	}
	if input.Label != "" {
		label := strings.TrimSpace(input.Label)
		if !validClaudeLabel(label) {
			return errors.New("备注最多 64 个字符，不能含控制字符")
		}
		a.Label = label
	}
	if input.Enabled != nil {
		a.Enabled = *input.Enabled
	}
	return nil
}

func (r codexGuestSnapshot) email() string {
	email := strings.ToLower(strings.TrimSpace(r.Email))
	mailbox, err := mail.ParseAddress(email)
	if !r.LoggedIn || err != nil || mailbox.Address != email {
		return ""
	}
	return email
}

func (r codexGuestSnapshot) subject() string {
	if email := r.email(); email != "" {
		return claudeIdentityHash(email, r.Organization)
	}
	return ""
}

// Never attach a previous identity's quota to a new identity, and never let a
// second account record count the same subscription twice.
func (list codexAccountList) observe(id string, observed codexGuestSnapshot, now time.Time) error {
	a := list.find(id)
	if a == nil {
		return errors.New("未找到 Codex 账号")
	}
	a.CheckedAt = now.Unix()
	email := observed.email()
	if email == "" {
		a.Status = "signed_out"
		return nil
	}
	organization := strings.TrimSpace(observed.Organization)
	if a.Email != "" && (!strings.EqualFold(a.Email, email) || a.Organization != organization) {
		a.Status = "changed"
		return nil
	}
	for _, other := range list.Accounts {
		if other.ID != id && strings.EqualFold(other.Email, email) && other.Organization == organization {
			a.Status = "duplicate"
			return nil
		}
	}
	a.Status, a.Email, a.Organization = "ready", email, organization
	if observed.Plan != "" {
		a.Plan = observed.Plan
	}
	a.mergeQuota(observed.Quota, now)
	return nil
}

func validCodexWindow(w *codexWindow) bool {
	return w != nil && !math.IsNaN(w.UsedPercentage) && w.UsedPercentage >= 0 && w.UsedPercentage <= 100 && w.ResetsAt >= 0 && w.WindowMinutes >= 0
}

// A missing reading cannot invalidate an existing reading, or make an old
// reading fresh.
func (a *codexAccount) mergeQuota(q *codexQuota, now time.Time) bool {
	if q == nil || q.ObservedAt <= 0 || q.ObservedAt > now.Unix() {
		return false
	}
	next := codexQuota{}
	if a.Quota != nil {
		next = *a.Quota
	}
	merge := func(old, reading *codexWindow) *codexWindow {
		if !validCodexWindow(reading) {
			return old
		}
		window := *reading
		if window.ObservedAt == 0 {
			window.ObservedAt = q.ObservedAt
		}
		if window.ObservedAt < 0 || window.ObservedAt > q.ObservedAt {
			return old
		}
		if old != nil && schedulingObservationTime(old.ObservedAt, next.ObservedAt, now.Unix()) > window.ObservedAt {
			return old
		}
		if old != nil && window == *old {
			return old
		}
		return &window
	}
	next.Primary = merge(next.Primary, q.Primary)
	next.Secondary = merge(next.Secondary, q.Secondary)
	if next.Primary == nil && next.Secondary == nil || a.Quota != nil && next == *a.Quota {
		return false
	}
	// Keep older windows tied to their own observation while the snapshot
	// timestamp advances for a newer reading from the other window.
	for _, window := range []**codexWindow{&next.Primary, &next.Secondary} {
		if *window == nil || (*window).ObservedAt != 0 {
			continue
		}
		if next.ObservedAt <= 0 {
			*window = nil
			continue
		}
		reading := **window
		reading.ObservedAt = next.ObservedAt
		*window = &reading
	}
	next.ObservedAt = max(next.ObservedAt, q.ObservedAt)
	a.Quota = &next
	return true
}

// A fully used window blocks new terminals until its actual reset, even when
// the snapshot is stale. An expired or unreported window stays unknown.
func (a codexAccount) headroom(now time.Time) (float64, bool) {
	if a.Quota == nil {
		return -1, true
	}
	least := -1.0
	for _, w := range []*codexWindow{a.Quota.Primary, a.Quota.Secondary} {
		if !validCodexWindow(w) || w.ResetsAt > 0 && w.ResetsAt <= now.Unix() {
			continue
		}
		obs := schedulingObservationTime(w.ObservedAt, a.Quota.ObservedAt, now.Unix())
		if obs == 0 {
			continue
		}
		fresh := now.Unix()-obs <= int64(claudeQuotaFresh/time.Second)
		if w.UsedPercentage >= 100 && (w.ResetsAt > now.Unix() || fresh) {
			return 0, false
		}
		if fresh {
			remaining := 100 - w.UsedPercentage
			if least < 0 || remaining < least {
				least = remaining
			}
		}
	}
	return least, true
}

// selectCodexAccount returns an empty selection when the roster is empty: the
// terminal then uses the environment's own ~/.codex login, as before.
func (s *Server) selectCodexAccount(requestedID string, login bool, workspace string) (codexSelection, error) {
	var empty codexSelection
	if !login && requestedID != "" && requestedID != "auto" {
		return empty, errors.New("Codex 终端会自动分配账号")
	}
	m := &s.codexAccounts
	m.mu.Lock()
	list, err := s.loadCodexAccounts()
	if err == nil && list.PendingLogin != "" && requestedID == "" && !s.codexLoginActive(list.PendingLogin) {
		m.mu.Unlock()
		// Record a login that completed before a controller restart or lock
		// before a new login could write into the same guest directory. This
		// waits for a background check of that directory that is under way.
		if epoch, ok := s.account.access(); ok {
			if probeErr := s.recoverCodexPending(epoch, list.PendingLogin, login); login && probeErr != nil {
				return empty, probeErr
			}
		}
		m.mu.Lock()
		list, err = s.loadCodexAccounts()
	}
	if err == nil && login {
		if requestedID == "" {
			// Never sign in over a login that could not be recorded; leave it
			// in its directory and start the new one in a fresh directory.
			if pending := list.PendingLogin; pending != "" && pending == m.unrecorded && !s.codexLoginActive(pending) {
				list.PendingLogin, m.unrecorded = "", ""
			}
			if list.PendingLogin == "" {
				list.PendingLogin = randomToken()[:24]
				m.loginNotice = ""
				err = s.saveCodexAccounts(list)
			}
			requestedID = list.PendingLogin
		} else if list.find(requestedID) == nil {
			err = errors.New("未找到 Codex 账号，请刷新面板")
		}
	}
	m.mu.Unlock()
	if err != nil {
		return empty, err
	}
	if login {
		return codexSelection{ID: requestedID, GuestID: requestedID}, nil
	}
	if !scheduleManagesLaunches(list.Scheduling.Mode, list.Scheduling.SelectedAccountID, len(list.Accounts)) {
		return empty, nil
	}
	return s.allocateCodexAccount(list, workspace, s.readCodexGuestQuotas)
}

func (s *Server) readCodexGuestQuotas(ctx context.Context, ids []string) (map[string]codexGuestQuota, error) {
	for _, id := range ids {
		if !validClaudeID(id) {
			return nil, errors.New("Codex 账号 ID 无效")
		}
	}
	readings := make(map[string]codexGuestQuota)
	for chunk := range slices.Chunk(ids, 96) {
		data, err := s.guestQuick(ctx, "exec /usr/local/bin/cage-codex-account quotas "+strings.Join(chunk, " "))
		if err != nil {
			return readings, err
		}
		if err := json.Unmarshal(data, &readings); err != nil {
			return readings, err
		}
	}
	return readings, nil
}

// The guest reads only small local files: the last usage report and the
// newest rate-limit event recorded by the account's own sessions.
func (s *Server) allocateCodexAccount(previous codexAccountList, workspace string, read func(context.Context, []string) (map[string]codexGuestQuota, error)) (codexSelection, error) {
	const unavailable = "暂无可分配的 Codex 账号。请在 Codex 账号面板检查登录、参与状态和额度重置时间"
	var empty codexSelection
	epoch, unlocked := s.account.access()
	if !unlocked {
		return empty, errors.New("Cage 已锁定，请先解锁")
	}
	var ids []string
	mode := canonicalSchedulingMode(previous.Scheduling.Mode)
	manualCandidate := false
	for _, a := range previous.Accounts {
		if a.Enabled && a.Status == "ready" && a.Email != "" {
			ids = append(ids, a.ID)
			manualCandidate = manualCandidate || a.ID == previous.Scheduling.SelectedAccountID
		}
	}
	if mode == "manual" && !manualCandidate {
		return empty, manualSchedulingError("Codex")
	}
	if len(ids) == 0 {
		return empty, errors.New(unavailable)
	}
	ctx, cancel := context.WithTimeout(epoch, 4*time.Second)
	readings, _ := read(ctx, ids) // Retain the last snapshot when SSH is unavailable.
	cancel()
	var guestCursor guestSchedulingCursor
	if mode == "exhaust" {
		guestCursor = s.readGuestSchedulingCursor("codex", true)
	}
	active, logins := s.codexAccountLoad()
	for id, reading := range readings {
		active[id] += max(0, reading.PlainLaunches)
	}
	m := &s.codexAccounts
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch.Err() != nil {
		return empty, errors.New("Cage 已锁定，请先解锁")
	}
	list, err := s.loadCodexAccounts()
	if err != nil {
		return empty, err
	}
	now := time.Now()
	updated := false
	for i := range list.Accounts {
		a := &list.Accounts[i]
		q, ok := readings[a.ID]
		if a.Enabled && a.Status == "ready" && ok && q.Identity == a.subject() && a.mergeQuota(&q.codexQuota, now) {
			updated = true
		}
	}
	preferred := ""
	if recent := m.recent[workspace]; now.Sub(recent.at) < claudeProjectAffinity {
		preferred = recent.id
	}
	view := list
	mode = canonicalSchedulingMode(list.Scheduling.Mode)
	configured := list.Scheduling.SelectedAccountID
	if mode == "exhaust" && guestCursor.matches(configured, list.Scheduling.SelectedAccountRevision) && list.find(guestCursor.AccountID) != nil {
		view.Scheduling.SelectedAccountID = guestCursor.AccountID
	}
	selected := pickCodexAccount(view, active, logins, m.lastPick, preferred, now)
	if mode == "exhaust" && selected != "" && selected != configured {
		list.Scheduling.SelectedAccountID = selected
		list.Scheduling.SelectedAccountRevision = newSchedulingRevision()
		updated = true
	}
	if updated {
		if err = s.saveCodexAccounts(list); err != nil {
			return empty, err
		}
	}
	if selected == "" {
		if mode == "manual" {
			return empty, manualSchedulingError("Codex")
		}
		return empty, errors.New(unavailable)
	}
	a := list.find(selected)
	return codexSelection{ID: selected, GuestID: selected, Subject: a.subject()}, nil
}

func (s *Server) codexAccountLoad() (map[string]int, map[string]bool) {
	active := s.mobileCLI.codexPhoneLoad()
	for id, count := range s.activeServiceAccounts("codex") {
		if id != "" {
			active[id] += count
		}
	}
	logins := make(map[string]bool)
	for _, session := range s.terminals.snapshot() {
		id, _ := session["codex_account_id"].(string)
		if session["closed"] == true || id == "" {
			continue
		}
		if session["kind"] == "codex-login" {
			logins[id] = true
		} else {
			active[id]++
		}
	}
	return active, logins
}

func (a codexAccount) eligible(logins map[string]bool, now time.Time) (float64, bool) {
	if !a.Enabled || a.Status != "ready" || a.Email == "" || logins[a.ID] {
		return 0, false
	}
	return a.headroom(now)
}

// Sequential mode takes the first eligible account in list order. Balanced
// mode starts after the last allocation and prefers fewer live terminals,
// then the largest known remaining minimum window.
func pickCodexAccount(list codexAccountList, active map[string]int, logins map[string]bool, last, preferred string, now time.Time) string {
	if mode := canonicalSchedulingMode(list.Scheduling.Mode); mode != "smart" {
		idx := orderedAccountIndex(list.Accounts, mode, list.Scheduling.SelectedAccountID, func(a codexAccount) string { return a.ID }, func(a codexAccount) bool {
			_, eligible := a.eligible(logins, now)
			return eligible
		})
		if idx >= 0 {
			return list.Accounts[idx].ID
		}
		return ""
	}
	if len(list.Accounts) == 0 {
		return ""
	}
	start := slices.IndexFunc(list.Accounts, func(a codexAccount) bool { return a.ID == last }) + 1
	best := ""
	var bestActive int
	var bestRoom float64
	for offset := range list.Accounts {
		a := list.Accounts[(start+offset)%len(list.Accounts)]
		room, ok := a.eligible(logins, now)
		if ok && (best == "" || active[a.ID] < bestActive || active[a.ID] == bestActive && room > bestRoom) {
			best, bestActive, bestRoom = a.ID, active[a.ID], room
		}
	}
	// Keep a recent project's account only with the same load and comparable
	// headroom (both unknown, or within five percentage points).
	if a := list.find(preferred); a != nil && active[a.ID] == bestActive {
		if room, ok := a.eligible(logins, now); ok && (room == bestRoom || room >= 0 && bestRoom-room <= min(5, bestRoom*0.1)) {
			return a.ID
		}
	}
	return best
}

// pinnedCodexSelection is the account a Codex session started with; it never
// changes. Opening the session on the desktop and sending from the phone
// accept and refuse the same accounts.
func (s *Server) pinnedCodexSelection(record *mobileRecord) (codexSelection, error) {
	if record.CodexAccount == "" {
		return codexSelection{}, nil
	}
	s.mu.Lock()
	update := s.guestUpdateRequired
	s.mu.Unlock()
	if update {
		return codexSelection{}, errGuestUpdateRequired
	}
	s.codexAccounts.mu.Lock()
	list, err := s.loadCodexAccounts()
	s.codexAccounts.mu.Unlock()
	if err != nil {
		return codexSelection{}, err
	}
	a := list.find(record.CodexAccount)
	if a == nil {
		return codexSelection{}, errors.New("这段会话所用的 Codex 账号已移除，无法继续")
	}
	if a.Email == "" {
		return codexSelection{}, errors.New("这段会话所用的 Codex 账号尚未登录，请在 Codex 账号管理中检查")
	}
	return codexSelection{ID: a.ID, GuestID: a.ID, Subject: a.subject()}, nil
}

func (s *Server) noteCodexSelection(id, workspace string) {
	m := &s.codexAccounts
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPick = id
	if workspace == "" {
		return
	}
	if m.recent == nil {
		m.recent = make(map[string]claudeProjectPick)
	}
	now := time.Now()
	var oldest string
	for path, pick := range m.recent {
		if now.Sub(pick.at) >= claudeProjectAffinity {
			delete(m.recent, path)
		} else if oldest == "" || pick.at.Before(m.recent[oldest].at) {
			oldest = path
		}
	}
	m.recent[workspace] = claudeProjectPick{id: id, at: now}
	if len(m.recent) > 64 {
		delete(m.recent, oldest)
	}
}
