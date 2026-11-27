package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"unicode"
	"unicode/utf8"
)

// A profile ID selects a guest CLI configuration directory. Only public status
// fields enter this registry; the official CLI owns authentication in the VM.
type claudeAccount struct {
	ID             string       `json:"id"`
	Label          string       `json:"label"`
	Source         string       `json:"source"`
	ProfileID      string       `json:"profile_id,omitempty"`
	Enabled        bool         `json:"enabled"`
	Status         string       `json:"status"`
	Email          string       `json:"email,omitempty"`
	Organization   string       `json:"organization,omitempty"`
	Plan           string       `json:"plan,omitempty"`
	HandoffGroup   string       `json:"handoff_group,omitempty"`
	Quota          *claudeQuota `json:"quota,omitempty"`
	CheckedAt      int64        `json:"checked_at,omitempty"`
	UsageStatus    string       `json:"usage_status,omitempty"`
	UsageCheckedAt int64        `json:"usage_checked_at,omitempty"`
	UsageChecking  bool         `json:"usage_checking,omitempty"`
}

type claudeQuota struct {
	FiveHour   *claudeWindow `json:"five_hour,omitempty"`
	SevenDay   *claudeWindow `json:"seven_day,omitempty"`
	ObservedAt int64         `json:"observed_at"`
}

type claudeWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
	ObservedAt     int64   `json:"observed_at,omitempty"`
}

type claudeGuestSnapshot struct {
	UsageStatus    string       `json:"usage_status,omitempty"`
	UsageCheckedAt int64        `json:"usage_checked_at,omitempty"`
	LoggedIn       bool         `json:"logged_in"`
	Email          string       `json:"email"`
	Organization   string       `json:"organization"`
	Plan           string       `json:"plan"`
	Quota          *claudeQuota `json:"quota"`
	PlainLaunches  int          `json:"plain_launches,omitempty"`
}

type claudeAccountList struct {
	Accounts     []claudeAccount  `json:"accounts"`
	PendingLogin string           `json:"pending_login,omitempty"`
	Scheduling   claudeScheduling `json:"scheduling"`
}

type claudeScheduling struct {
	Mode         string `json:"mode"`
	SwitchPolicy string `json:"switch_policy"`
	// SwitchAnswered: the person gave a standing answer to "what happens when
	// an account's quota runs out", so the first-time question never returns.
	SwitchAnswered bool `json:"switch_answered,omitempty"`
	// UngroupedHandoff: accounts without a 接续分组 form one implicit group
	// for automatic continuation. Set only by the person's yes to it.
	UngroupedHandoff bool `json:"ungrouped_handoff,omitempty"`
	// Preferred is the account new sessions use while it is available.
	Preferred               string `json:"preferred,omitempty"`
	SelectedAccountID       string `json:"selected_account_id,omitempty"`
	SelectedAccountRevision string `json:"selected_account_revision,omitempty"`
}

func (p claudeScheduling) defaults() claudeScheduling {
	if p.Mode == "" {
		p.Mode = "balanced"
	}
	if p.SwitchPolicy == "" {
		p.SwitchPolicy = "ask"
	}
	return p
}

// answered: the first-time question was answered for good. A stored allow
// or wait was a choice (ask was the default) and counts.
func (p claudeScheduling) answered() bool {
	return p.SwitchAnswered || p.SwitchPolicy == "allow" || p.SwitchPolicy == "wait"
}

func (p claudeScheduling) valid() bool {
	return slices.Contains([]string{"manual", "exhaust", "smart", "balanced", "sequential"}, p.Mode) &&
		validSwitchPolicy(p.SwitchPolicy) && (p.Preferred == "" || validClaudeID(p.Preferred)) &&
		(p.SelectedAccountID == "" || validClaudeID(p.SelectedAccountID))
}

func (p claudeScheduling) claudeSelectedAccountID() string {
	if p.SelectedAccountID != "" {
		return p.SelectedAccountID
	}
	return p.Preferred
}

func (p *claudeScheduling) selectAccount(id string) {
	p.Mode = schedulingModeAfterSelect(p.Mode)
	p.SelectedAccountID, p.Preferred = id, ""
	p.SelectedAccountRevision = newSchedulingRevision()
}

type claudeAccountManager struct {
	usageProbe     func(context.Context, string, string) (claudeGuestSnapshot, error)
	usageActive    map[string]*claudeUsageCall
	panelRunning   bool
	panelUntil     time.Time
	detectedUsage  map[string]*claudeAccount
	defaultGate    chan struct{}
	loginNotice    string
	defaultChecked time.Time
	defaultReading claudeGuestSnapshot
	defaultError   error
	probe          func(context.Context, string) (claudeGuestSnapshot, error)
	recoverLogin   func(context.Context, string, string, string) (claudeGuestSnapshot, error)
	mu             sync.Mutex
	lastPick       string
	recent         map[string]claudeProjectPick
	refreshing     map[string]*claudeRefresh
	attempted      map[string]time.Time
	// launchPublished is the candidate list the guest holds (claude_launch.go).
	launchPublished string
	launchMu        sync.Mutex
	launchRunning   bool
	launchAgain     bool
	// now is the handoff clock; tests set it.
	now func() time.Time
}

type claudeRefresh struct {
	done chan struct{}
	err  error
}

const claudeQuotaFresh = 15 * time.Minute

func validClaudeID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 24 && err == nil && strings.ToLower(id) == id
}

func (s *Server) loadClaudeAccounts() (claudeAccountList, error) {
	list := claudeAccountList{Accounts: []claudeAccount{}, Scheduling: (claudeScheduling{}).defaults()}
	data, err := os.ReadFile(filepath.Join(s.profile, "claude-accounts.json"))
	if os.IsNotExist(err) {
		return list, nil
	}
	if err != nil || json.Unmarshal(data, &list) != nil || list.Accounts == nil {
		return list, errors.New("Claude 账号文件无法读取，请检查配置目录或恢复备份")
	}
	list.Scheduling = list.Scheduling.defaults()
	if !list.Scheduling.valid() {
		return list, errors.New("Claude 调度设置无效，请检查配置文件")
	}
	ids := make(map[string]bool)
	profiles := make(map[string]bool)
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.Source == "" {
			a.Source = "managed"
		}
		if !validClaudeID(a.ID) || ids[a.ID] || a.Source != "managed" && a.Source != "default" || a.Source == "default" && (a.Email == "" || a.ProfileID != "") || a.ProfileID != "" && !validClaudeID(a.ProfileID) || len(a.HandoffGroup) > 64 {
			return list, errors.New("Claude 账号文件含无效或重复的记录，请从备份恢复")
		}
		ids[a.ID] = true
		if a.Source == "managed" {
			if profiles[a.guestID()] {
				return list, errors.New("多个账号引用了同一个独立登录目录，请从备份恢复")
			}
			profiles[a.guestID()] = true
		}
	}
	if list.PendingLogin != "" && (!validClaudeID(list.PendingLogin) || ids[list.PendingLogin] || profiles[list.PendingLogin]) {
		return list, errors.New("Claude 登录记录损坏，请恢复备份")
	}
	return list, nil
}

func (s *Server) saveClaudeAccounts(list claudeAccountList) error {
	if err := writePrivate(filepath.Join(s.profile, "claude-accounts.json"), list); err != nil {
		return errors.New("Claude 账号设置未保存，请检查配置目录权限和磁盘空间")
	}
	s.kickClaudeLaunchSchedule()
	return nil
}

func (a claudeAccount) guestID() string {
	if a.Source == "default" {
		return "default"
	}
	if a.ProfileID != "" {
		return a.ProfileID
	}
	return a.ID
}

func (list claudeAccountList) find(id string) *claudeAccount {
	for i := range list.Accounts {
		if list.Accounts[i].ID == id {
			return &list.Accounts[i]
		}
	}
	return nil
}

func validClaudeLabel(label string) bool {
	return label != "" && utf8.RuneCountInString(label) <= 64 && strings.IndexFunc(label, unicode.IsControl) < 0
}

func (list *claudeAccountList) change(input claudeAccountInput) error {
	label := strings.TrimSpace(input.Label)
	if input.Action != "update" {
		return errors.New("未知 Claude 账号操作")
	}
	a := list.find(input.ID)
	if a == nil {
		return errors.New("未找到 Claude 账号，请刷新面板")
	}
	if input.Label != "" {
		if !validClaudeLabel(label) && (label == "" || label != a.Email) {
			return errors.New("备注最多 64 个字符，不能含控制字符")
		}
		a.Label = label
	}
	if input.Enabled != nil {
		a.Enabled = *input.Enabled
	}
	if input.HandoffGroup != nil {
		group := strings.TrimSpace(*input.HandoffGroup)
		if len(group) > 64 || strings.IndexFunc(group, unicode.IsControl) >= 0 {
			return errors.New("接续分组最多 64 字节，不能含控制字符")
		}
		a.HandoffGroup = group
	}
	return nil
}

func (r claudeGuestSnapshot) email() string {
	email := strings.ToLower(strings.TrimSpace(r.Email))
	mailbox, err := mail.ParseAddress(email)
	if !r.LoggedIn || err != nil || mailbox.Address != email {
		return ""
	}
	return email
}

func (r claudeGuestSnapshot) subject() string {
	if email := r.email(); email != "" {
		return claudeIdentityHash(email, r.Organization)
	}
	return ""
}

func validClaudeWindow(w *claudeWindow) bool {
	return w != nil && !math.IsNaN(w.UsedPercentage) && w.UsedPercentage >= 0 && w.UsedPercentage <= 100 && w.ResetsAt >= 0
}

// A missing reading cannot invalidate an existing reading, or make an old
// reading fresh. Never attach a previous identity's quota to a new identity.
func (list claudeAccountList) observe(id string, observed claudeGuestSnapshot, now time.Time) error {
	a := list.find(id)
	if a == nil {
		return errors.New("未找到 Claude 账号")
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
	a.Status, a.Email, a.Organization, a.Plan = "ready", email, organization, observed.Plan
	a.mergeQuota(observed.Quota, now)
	return nil
}

func (a *claudeAccount) mergeQuota(q *claudeQuota, now time.Time) bool {
	if q == nil || q.ObservedAt <= 0 || q.ObservedAt > now.Unix() {
		return false
	}
	next := claudeQuota{}
	if a.Quota != nil {
		next = *a.Quota
	}
	merge := func(old, reading *claudeWindow) *claudeWindow {
		if !validClaudeWindow(reading) {
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
	next.FiveHour = merge(next.FiveHour, q.FiveHour)
	next.SevenDay = merge(next.SevenDay, q.SevenDay)
	if next.FiveHour == nil && next.SevenDay == nil || a.Quota != nil && next == *a.Quota {
		return false
	}
	// Keep older windows tied to their own observation while the snapshot
	// timestamp advances for a newer reading from the other window.
	for _, window := range []**claudeWindow{&next.FiveHour, &next.SevenDay} {
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

func claudeIdentityHash(email, organization string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "\n" + strings.TrimSpace(organization)))
	return hex.EncodeToString(digest[:])
}

// A fully used window still blocks new sessions until its actual reset, even
// if the snapshot is stale. An expired or unreported window stays unknown.
func (a claudeAccount) headroom(now time.Time) (float64, bool) {
	if a.Quota == nil {
		return -1, true
	}
	least := -1.0
	for _, w := range []*claudeWindow{a.Quota.FiveHour, a.Quota.SevenDay} {
		if !validClaudeWindow(w) || w.ResetsAt > 0 && w.ResetsAt <= now.Unix() {
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

// exhaustedWindow names the window that a confirmed exhaustion comes from:
// a window read at 100% whose reset is still ahead. When both are, the one
// that resets last binds. A model-specific limit has no window here.
func (a claudeAccount) exhaustedWindow(now time.Time) (string, int64) {
	if _, available := a.headroom(now); available || a.Quota == nil {
		return "", 0
	}
	window, reset := "", int64(0)
	for name, w := range map[string]*claudeWindow{"five_hour": a.Quota.FiveHour, "seven_day": a.Quota.SevenDay} {
		if validClaudeWindow(w) && w.UsedPercentage >= 100 && (window == "" || w.ResetsAt > reset) {
			obs := schedulingObservationTime(w.ObservedAt, a.Quota.ObservedAt, now.Unix())
			if obs == 0 || w.ResetsAt > 0 && w.ResetsAt <= now.Unix() || w.ResetsAt == 0 && now.Unix()-obs > int64(claudeQuotaFresh/time.Second) {
				continue
			}
			window, reset = name, w.ResetsAt
		}
	}
	return window, reset
}

// A selection carries the identity verified for this launch. The caller
// must not replace Subject or GuestID by looking the record up again.
type claudeSelection struct{ ID, GuestID, Subject string }

func (s *Server) selectClaudeAccount(requestedID string, login bool, workspace string) (claudeSelection, error) {
	var empty claudeSelection
	if !login && requestedID == "auto" {
		requestedID = ""
	}
	if !login && requestedID != "" {
		return empty, errors.New("Claude 终端会自动分配账号")
	}
	if !login && s.magpieTakesOverScheduling() {
		return empty, nil
	}
	m := &s.claudeAccounts
	m.mu.Lock()
	list, err := s.loadClaudeAccounts()
	if err == nil && list.PendingLogin != "" && requestedID == "" && !s.claudeLoginActive(list.PendingLogin) {
		m.mu.Unlock()
		// Restore a successful login from before a controller restart or lock
		// before a new login could write into the same guest directory.
		if epoch, ok := s.account.access(); ok {
			if probeErr := s.recoverClaudePending(epoch, list.PendingLogin, login); login && probeErr != nil {
				return empty, probeErr
			}
		}
		m.mu.Lock()
		list, err = s.loadClaudeAccounts()
	}
	if err == nil && login {
		if requestedID == "" {
			if list.PendingLogin == "" {
				list.PendingLogin = randomToken()[:24]
				m.loginNotice = ""
				err = s.saveClaudeAccounts(list)
			}
			requestedID = list.PendingLogin
		} else if a := list.find(requestedID); a == nil || a.Source != "managed" {
			err = errors.New("请在普通终端管理环境的默认登录")
		}
	}
	m.mu.Unlock()
	if err != nil {
		return empty, err
	}
	if login {
		guestID := requestedID
		if a := list.find(requestedID); a != nil {
			guestID = a.guestID()
		}
		return claudeSelection{ID: requestedID, GuestID: guestID}, nil
	}
	if !scheduleManagesLaunches(list.Scheduling.Mode, list.Scheduling.claudeSelectedAccountID(), len(list.Accounts)) {
		return empty, nil
	}
	return s.allocateClaudeAccount(list, workspace, s.readClaudeGuestQuotas)
}

type claudeGuestQuota struct {
	claudeQuota
	Identity      string `json:"identity"`
	PlainLaunches int    `json:"plain_launches,omitempty"`
}

func (s *Server) readClaudeGuestQuotas(ctx context.Context, ids []string) (map[string]claudeGuestQuota, error) {
	for _, id := range ids {
		if !validClaudeGuestID(id) {
			return nil, errors.New("Claude 账号 ID 无效")
		}
	}
	// The read is idempotent and runs on every launch, so it shares the
	// guest's control connection. That bounds its output to 64 KiB. Even a
	// large pool uses one server-controlled deadline; keep completed chunks
	// when a later one fails.
	readings := make(map[string]claudeGuestQuota)
	for chunk := range slices.Chunk(ids, 96) {
		if s.windowsGuest != nil {
			if err := s.windowsCLIRequest(ctx, map[string]any{"action": "account", "mode": "quotas", "ids": chunk}, &readings); err != nil {
				return readings, err
			}
			continue
		}
		data, err := s.guestQuick(ctx, "exec /usr/local/bin/cage-claude-account quotas "+strings.Join(chunk, " "))
		if err != nil {
			return readings, err
		}
		if err := json.Unmarshal(data, &readings); err != nil {
			return readings, err
		}
	}
	return readings, nil
}

// The status line may have written a newer reading while the account panel
// was closed. Fetch only its small local files in one SSH command. The guest
// does not start the CLI, read tokens, or contact Anthropic for this read.
func (s *Server) allocateClaudeAccount(previous claudeAccountList, workspace string, read func(context.Context, []string) (map[string]claudeGuestQuota, error)) (claudeSelection, error) {
	return s.allocateClaudeBinding(previous, workspace, "", read)
}

func (s *Server) allocateClaudeBinding(previous claudeAccountList, workspace, requestedID string, read func(context.Context, []string) (map[string]claudeGuestQuota, error)) (claudeSelection, error) {
	const unavailable = "暂无可分配的 Claude 账号。请在账号面板检查登录、参与状态和额度重置时间"
	var empty claudeSelection
	epoch, unlocked := s.account.access()
	if !unlocked {
		return empty, errors.New("Super Cage 已锁定，请先解锁")
	}
	var ids []string
	checkDefault := false
	mode := canonicalSchedulingMode(previous.Scheduling.Mode)
	manual := requestedID == "" && mode == "manual"
	manualCandidate := false
	configured := previous.Scheduling.claudeSelectedAccountID()
	legacyPreferred := previous.Scheduling.SelectedAccountID == "" && previous.Scheduling.Preferred != ""
	for _, a := range previous.Accounts {
		if requestedID != "" && a.ID != requestedID {
			continue
		}
		// The preferred account is used for new sessions even when it does
		// not take part in automatic scheduling.
		if a.Status != "ready" || a.Email == "" || !a.Enabled && a.ID != previous.Scheduling.Preferred {
			continue
		}
		manualCandidate = manualCandidate || a.ID == configured && (a.Enabled || legacyPreferred)
		if a.Source == "default" {
			checkDefault = true
		} else {
			ids = append(ids, a.guestID())
		}
	}
	if manual && !manualCandidate {
		return empty, manualSchedulingError("Claude")
	}
	if !checkDefault && len(ids) == 0 {
		return empty, errors.New(unavailable)
	}
	// The default login is verified in a login shell and the managed quotas are
	// small file reads: neither needs the other, so a launch waits for the
	// slower of them, not for both.
	defaultSubject := ""
	defaultPlainLaunches := 0
	var defaultCheck sync.WaitGroup
	if checkDefault {
		defaultCheck.Add(1)
		go func() {
			defer defaultCheck.Done()
			if reading, _, err := s.inspectClaudeDefault(epoch, true); err == nil {
				defaultSubject = reading.subject()
				defaultPlainLaunches = max(0, reading.PlainLaunches)
			}
		}()
	}
	var readings map[string]claudeGuestQuota
	if len(ids) > 0 {
		ctx, cancel := context.WithTimeout(epoch, 4*time.Second)
		readings, _ = read(ctx, ids) // Retain the last snapshot when SSH is unavailable.
		cancel()
	}
	defaultCheck.Wait()
	var guestCursor guestSchedulingCursor
	if requestedID == "" && mode == "exhaust" {
		guestCursor = s.readGuestSchedulingCursor("claude", false)
	}
	active, logins := s.claudeAccountLoad()
	m := &s.claudeAccounts
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch.Err() != nil {
		return empty, errors.New("Super Cage 已锁定，请先解锁")
	}
	list, err := s.loadClaudeAccounts()
	if err != nil {
		return empty, err
	}
	now := time.Now()
	list.applyDefaultServiceUsage(active)
	updated := false
	for i := range list.Accounts {
		a := &list.Accounts[i]
		q, ok := readings[a.guestID()]
		active[a.ID] += max(0, q.PlainLaunches)
		if a.Source == "default" {
			active[a.ID] += defaultPlainLaunches
		}
		if (a.Enabled || a.ID == list.Scheduling.Preferred) && a.Status == "ready" && ok && q.Identity == claudeIdentityHash(a.Email, a.Organization) {
			if a.mergeQuota(&q.claudeQuota, now) {
				updated = true
			}
		}
	}
	recent := m.recent[workspace]
	preferred := ""
	if now.Sub(recent.at) < claudeProjectAffinity {
		preferred = recent.id
	}
	// Only a positively verified matching default profile can be admitted.
	for i := range list.Accounts {
		a := &list.Accounts[i]
		if a.Email == "" || a.Source == "default" && (defaultSubject == "" || defaultSubject != claudeIdentityHash(a.Email, a.Organization)) {
			logins[a.ID] = true // only the admission view treats this profile as unavailable
		}
	}
	selected := requestedID
	mode = canonicalSchedulingMode(list.Scheduling.Mode)
	if selected == "" {
		view := list
		configured := list.Scheduling.claudeSelectedAccountID()
		guestID := configured
		if a := list.find(configured); a != nil {
			guestID = a.guestID()
		}
		if mode == "exhaust" && guestCursor.matches(guestID, list.Scheduling.SelectedAccountRevision) {
			for _, a := range list.Accounts {
				if a.guestID() == guestCursor.AccountID && (a.Source != "default" || defaultSubject == claudeIdentityHash(a.Email, a.Organization)) {
					if a.ID != configured {
						view.Scheduling.SelectedAccountID = a.ID
					}
					break
				}
			}
		}
		selected = pickClaudeAccount(view, active, logins, m.lastPick, preferred, now)
		if mode == "exhaust" && selected != "" && selected != configured {
			list.Scheduling.SelectedAccountID = selected
			list.Scheduling.SelectedAccountRevision = newSchedulingRevision()
			updated = true
		}
	} else if a := list.find(selected); a == nil || !a.Enabled || a.Status != "ready" || logins[selected] {
		selected = ""
	} else if _, eligible := a.headroom(now); !eligible {
		selected = ""
	}
	if updated {
		if err = s.saveClaudeAccounts(list); err != nil {
			return empty, err
		}
	}
	if selected == "" {
		if requestedID == "" && mode == "manual" {
			return empty, manualSchedulingError("Claude")
		}
		return empty, errors.New(unavailable)
	}
	a := list.find(selected)
	return claudeSelection{ID: selected, GuestID: a.guestID(), Subject: claudeIdentityHash(a.Email, a.Organization)}, nil
}

// Count a shared conversation once even when both a PTY and the guest's
// conversation index report it. An ended shared CLI leaves a plain shell.
func (s *Server) claudeAccountLoad() (map[string]int, map[string]bool) {
	active := s.mobileCLI.activeAccounts()
	delete(active, "")
	for id, count := range s.activeServiceAccounts("claude") {
		active[id] += count
	}
	logins := make(map[string]bool)
	counted := make(map[string]bool)
	s.terminals.mu.Lock()
	for _, session := range s.terminals.sessions {
		id := session.claudeAccountID
		if id == "" {
			continue
		}
		select {
		case <-session.done:
			continue
		default:
		}
		session.mu.Lock()
		if session.kind == "claude-login" {
			logins[id] = true
		} else if !session.conversationEnded {
			active[id]++
		}
		if session.sharedID != "" {
			counted[session.sharedID] = true
		}
		session.mu.Unlock()
	}
	s.terminals.mu.Unlock()
	s.mobileCLI.mu.Lock()
	for id := range s.mobileCLI.claudeShared {
		if account := s.mobileCLI.catalog[id].AccountID; account != "" && !counted[id] {
			active[account]++
		}
	}
	s.mobileCLI.mu.Unlock()
	return active, logins
}

// An explicit default service can predate the default profile joining the roster.
func (list claudeAccountList) applyDefaultServiceUsage(active map[string]int) {
	if count := active[""]; count > 0 {
		for _, account := range list.Accounts {
			if account.Source == "default" {
				active[account.ID] += count
				break
			}
		}
		delete(active, "")
	}
}

// A resumed conversation retains the identity it already had. The enabled
// flag controls automatic assignment of new work.
func (s *Server) validateBoundClaudeAccount(list claudeAccountList, selection claudeSelection) error {
	if selection.ID == "" {
		return nil
	}
	account := list.find(selection.ID)
	// A default login may subsequently be adopted into a managed profile
	// without moving the existing transcript. Its old default binding is
	// still subject to the guest's original identity check.
	if account == nil || account.Status != "ready" || (account.guestID() != selection.GuestID && selection.GuestID != "default") ||
		claudeIdentityHash(account.Email, account.Organization) != selection.Subject {
		return errors.New("原会话的 Claude 账号已移除、退出登录或发生变化")
	}
	if _, err := s.terminals.existingClaudeLogin(selection.ID, false); err != nil {
		return err
	}
	return nil
}

// Start at the account following the last allocation. Among eligible accounts,
// prefer fewer live sessions, then the largest known remaining minimum window.
func pickClaudeAccount(list claudeAccountList, active map[string]int, logins map[string]bool, last, preferred string, now time.Time) string {
	mode := canonicalSchedulingMode(list.Scheduling.Mode)
	if mode != "smart" {
		selected := list.Scheduling.claudeSelectedAccountID()
		legacyPreferred := list.Scheduling.SelectedAccountID == "" && list.Scheduling.Preferred != ""
		idx := orderedAccountIndex(list.Accounts, mode, selected, func(a claudeAccount) string { return a.ID }, func(a claudeAccount) bool {
			_, eligible := a.headroom(now)
			return (a.Enabled || legacyPreferred && a.ID == selected) && a.Status == "ready" && !logins[a.ID] && eligible
		})
		if idx >= 0 {
			return list.Accounts[idx].ID
		}
		return ""
	}

	// The person's chosen account wins while it can take a session, whether
	// or not it takes part in automatic scheduling.
	if a := list.find(list.Scheduling.Preferred); a != nil && a.Status == "ready" && !logins[a.ID] {
		if _, eligible := a.headroom(now); eligible {
			return a.ID
		}
	}
	start := slices.IndexFunc(list.Accounts, func(a claudeAccount) bool { return a.ID == last }) + 1
	best := ""
	var bestActive int
	var bestRoom float64
	for offset := range list.Accounts {
		a := list.Accounts[(start+offset)%len(list.Accounts)]
		if !a.Enabled || a.Status != "ready" || logins[a.ID] {
			continue
		}
		room, eligible := a.headroom(now)
		if eligible && (best == "" || active[a.ID] < bestActive || active[a.ID] == bestActive && room > bestRoom) {
			best, bestActive, bestRoom = a.ID, active[a.ID], room
		}
	}
	// Keep a recent project's profile only when it has the same load and
	// comparable headroom (both unknown, or within five percentage points).
	if a := list.find(preferred); a != nil && a.Enabled && a.Status == "ready" && !logins[a.ID] && active[a.ID] == bestActive {
		room, ok := a.headroom(now)
		if ok && (room == bestRoom || room >= 0 && bestRoom-room <= min(5, bestRoom*0.1)) {
			return a.ID
		}
	}
	return best
}

type claudeProjectPick struct {
	id string
	at time.Time
}

const claudeProjectAffinity = 5 * time.Minute

func (s *Server) noteClaudeSelection(id, workspace string) {
	m := &s.claudeAccounts
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
