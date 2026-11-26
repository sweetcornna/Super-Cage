package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
)

var (
	errCodexUnsupported = errors.New("Codex 账号目前只支持 Linux 环境")
	errCodexUsageBusy   = errors.New("正在检查其他 Codex 账号的额度，请稍后重试")
)

type codexAccountInput struct {
	Action     string            `json:"action"`
	ID         string            `json:"id"`
	Label      string            `json:"label"`
	Enabled    *bool             `json:"enabled"`
	Scheduling *claudeScheduling `json:"scheduling"`
	Order      []string          `json:"order"`
}

// The environment's own ~/.codex login is shown for orientation. New Codex
// terminals use it only while the roster is empty.
type codexDetected struct {
	Identity  string      `json:"identity"`
	Email     string      `json:"email"`
	Plan      string      `json:"plan,omitempty"`
	Quota     *codexQuota `json:"quota,omitempty"`
	CheckedAt int64       `json:"checked_at"`
}

type codexPanel struct {
	Scheduling             claudeScheduling `json:"scheduling"`
	UsagePanelRunning      bool             `json:"usage_panel_running"`
	UsageUnavailableReason string           `json:"usage_unavailable_reason,omitempty"`
	Accounts               []codexAccount   `json:"accounts"`
	LoginPending           bool             `json:"login_pending,omitempty"`
	Detected               *codexDetected   `json:"detected,omitempty"`
	DetectionStatus        string           `json:"detection_status"`
	DetectionError         string           `json:"detection_error,omitempty"`
	LoginNotice            string           `json:"login_notice,omitempty"`
	GuestSupported         bool             `json:"guest_supported"`
	MagpieTakeover         bool             `json:"magpie_takeover,omitempty"`
}

// Codex runs in the Linux guest only; native macOS and Windows guests have
// no helper for private CODEX_HOME profiles yet.
func (s *Server) codexGuestSupported() bool { return s.macosGuest == nil && s.windowsGuest == nil }

func (s *Server) listCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("panel") == "visible" {
		m := &s.codexAccounts
		m.mu.Lock()
		if m.panelRunning {
			m.panelUntil = time.Now().Add(claudePanelLease)
		}
		m.mu.Unlock()
	}
	view, _, err := s.codexPanelView(r.Context())
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, view)
}

func (s *Server) mutateCodexAccounts(w http.ResponseWriter, r *http.Request) {
	var input codexAccountInput
	if !readJSON(w, r, &input) {
		return
	}
	var err error
	switch input.Action {
	case "remove", "cancel":
		err = s.removeCodexAccount(input.ID)
	case "usage_panel":
		view, list, viewErr := s.codexPanelView(r.Context())
		if viewErr != nil {
			requestError(w, viewErr)
			return
		}
		s.startCodexPanelUsage(list)
		m := &s.codexAccounts
		m.mu.Lock()
		view.UsagePanelRunning = m.panelRunning
		m.mu.Unlock()
		jsonReply(w, view)
		return
	case "refresh":
		err = s.refreshCodexUsage(r.Context(), input.ID)
	case "update", "scheduling":
		s.op.Lock()
		defer s.op.Unlock()
		m := &s.codexAccounts
		m.mu.Lock()
		var list codexAccountList
		list, err = s.loadCodexAccounts()
		if err == nil && input.Action == "scheduling" {
			err = list.reschedule(input)
		} else if err == nil {
			err = list.change(input)
		}
		if err == nil {
			err = s.saveCodexAccounts(list)
		}
		m.mu.Unlock()
	default:
		err = errors.New("未知 Codex 账号操作")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.listCodexAccounts(w, r)
}

func (list *codexAccountList) reschedule(input codexAccountInput) error {
	if input.Scheduling == nil || !input.Scheduling.valid() {
		return errors.New("请选择有效的调度模式和切换授权")
	}
	if input.Order != nil {
		if len(input.Order) != len(list.Accounts) {
			return errors.New("账号列表已变化，请刷新后排序")
		}
		ordered := make([]codexAccount, 0, len(input.Order))
		seen := map[string]bool{}
		for _, id := range input.Order {
			a := list.find(id)
			if a == nil || seen[id] {
				return errors.New("账号顺序无效")
			}
			seen[id] = true
			ordered = append(ordered, *a)
		}
		list.Accounts = ordered
	}
	list.Scheduling = *input.Scheduling
	return nil
}

func (s *Server) codexPanelView(ctx context.Context) (codexPanel, codexAccountList, error) {
	s.mu.Lock()
	ready, update := s.ready, s.guestUpdateRequired
	s.mu.Unlock()
	supported := s.codexGuestSupported()
	view := codexPanel{DetectionStatus: "offline", GuestSupported: supported, MagpieTakeover: s.magpieTakesOverScheduling()}
	if unavailable := s.usageAvailability(); unavailable != nil {
		view.UsageUnavailableReason = unavailable.Error()
	}
	var reading codexGuestSnapshot
	var checked time.Time
	switch {
	case !supported:
		view.DetectionStatus = "unsupported"
	case update:
		view.DetectionStatus = "update_required"
		view.DetectionError = "请同步环境组件后再检测 Codex 登录"
	case ready:
		var err error
		reading, checked, err = s.inspectCodexDefault(ctx)
		if err != nil {
			view.DetectionStatus, view.DetectionError = "unavailable", err.Error()
		} else if reading.subject() == "" {
			view.DetectionStatus = "signed_out"
		} else {
			view.DetectionStatus = "ready"
		}
	}
	m := &s.codexAccounts
	m.mu.Lock()
	list, err := s.loadCodexAccounts()
	if err != nil {
		m.mu.Unlock()
		return view, list, err
	}
	if supported && ready && !update && list.PendingLogin != "" && !s.codexLoginActive(list.PendingLogin) {
		pending := list.PendingLogin
		m.mu.Unlock()
		// Restore a login that completed before a controller restart.
		_ = s.recoverCodexPending(ctx, pending, false)
		m.mu.Lock()
		list, err = s.loadCodexAccounts()
		if err != nil {
			m.mu.Unlock()
			return view, list, err
		}
	}
	defer m.mu.Unlock()
	view.Accounts, view.LoginNotice, view.Scheduling = list.Accounts, m.loginNotice, list.Scheduling
	view.UsagePanelRunning = m.panelRunning
	view.LoginPending = list.PendingLogin != "" && s.codexLoginActive(list.PendingLogin)
	if subject := reading.subject(); view.DetectionStatus == "ready" {
		view.Detected = &codexDetected{Identity: subject, Email: reading.email(), Plan: reading.Plan, Quota: reading.Quota, CheckedAt: checked.Unix()}
	}
	for i := range view.Accounts {
		a := &view.Accounts[i]
		a.UsageChecking = a.Email != "" && m.usageActive[a.subject()] != nil
	}
	return view, list, nil
}

// Panel reads coalesce for 20 seconds, serializing concurrent cold probes so
// simultaneous requests do not spawn duplicate guest reads.
func (s *Server) inspectCodexDefault(parent context.Context) (codexGuestSnapshot, time.Time, error) {
	epoch, unlocked := s.account.access()
	if !unlocked {
		return codexGuestSnapshot{}, time.Time{}, errClaudeAccessLost
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	m := &s.codexAccounts
	m.mu.Lock()
	if time.Since(m.defaultChecked) < 20*time.Second {
		reading, checked, err := m.defaultReading, m.defaultChecked, m.defaultError
		m.mu.Unlock()
		return reading, checked, err
	}
	if m.defaultGate == nil {
		m.defaultGate = make(chan struct{}, 1)
	}
	gate := m.defaultGate
	m.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return codexGuestSnapshot{}, time.Time{}, errors.New("暂时无法读取环境的 Codex 登录，请确认 Codex CLI 和环境组件可用后重试")
	}
	m.mu.Lock()
	if time.Since(m.defaultChecked) < 20*time.Second {
		reading, checked, err := m.defaultReading, m.defaultChecked, m.defaultError
		m.mu.Unlock()
		return reading, checked, err
	}
	m.mu.Unlock()
	reading, err := s.probeCodex(ctx, "default")
	now := time.Now()
	if err != nil || ctx.Err() != nil {
		reading, err = codexGuestSnapshot{}, errors.New("暂时无法读取环境的 Codex 登录，请确认 Codex CLI 和环境组件可用后重试")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultReading, m.defaultChecked, m.defaultError = reading, now, err
	if epoch.Err() != nil || parent.Err() != nil {
		m.defaultChecked = time.Time{}
	}
	return reading, now, err
}

func (s *Server) probeCodex(ctx context.Context, guestID string) (codexGuestSnapshot, error) {
	if probe := s.codexAccounts.probe; probe != nil {
		return probe(ctx, guestID)
	}
	// A snapshot only reads, and the accounts pages poll it, so it shares the
	// guest's control connection.
	return s.readCodexGuestReportVia(ctx, guestID, codexGuestReportCommand(guestID, ""), s.guestQuick)
}

func (s *Server) readCodexGuestUsage(ctx context.Context, id, subject string) (codexGuestSnapshot, error) {
	if len(subject) != 64 || strings.Trim(subject, "0123456789abcdef") != "" {
		return codexGuestSnapshot{}, errors.New("Codex 账号身份无效")
	}
	return s.readCodexGuestReport(ctx, id, codexGuestReportCommand(id, subject))
}

func (s *Server) readCodexGuestReport(ctx context.Context, id, command string) (codexGuestSnapshot, error) {
	return s.readCodexGuestReportVia(ctx, id, command, s.guest)
}

func (s *Server) readCodexGuestReportVia(ctx context.Context, id, command string, run func(context.Context, string) ([]byte, error)) (codexGuestSnapshot, error) {
	var snapshot codexGuestSnapshot
	if !validClaudeGuestID(id) {
		return snapshot, errors.New("Codex 账号 ID 无效")
	}
	if !s.codexGuestSupported() {
		return snapshot, errCodexUnsupported
	}
	data, err := run(ctx, command)
	if err != nil || json.Unmarshal(data, &snapshot) != nil {
		return snapshot, errors.New("无法读取 Codex 账号状态，请确认环境已启动并同步环境组件")
	}
	return snapshot, nil
}

func codexGuestReportCommand(id, subject string) string {
	command := "exec /usr/local/bin/cage-codex-account snapshot " + id
	if subject != "" {
		command = "exec /usr/local/bin/cage-codex-account usage " + id + " " + shellescape.Quote(subject)
	}
	if id == "default" {
		// Read the same CODEX_HOME a regular terminal's login shell uses and
		// keep startup banners out of the JSON stream.
		command = `set -o pipefail; cd "$HOME" && env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 /bin/bash -lic ` +
			shellescape.Quote(`printf '\nCAGE_CODEX_SNAPSHOT '; `+command) +
			` | sed -n 's/^CAGE_CODEX_SNAPSHOT //p'`
	}
	return command
}

func (s *Server) codexLoginActive(id string) bool {
	for _, session := range s.terminals.snapshot() {
		if session["kind"] == "codex-login" && session["codex_account_id"] == id && session["closed"] == false {
			return true
		}
	}
	return false
}

func (m *codexAccountManager) resetGuestCaches() {
	m.mu.Lock()
	m.defaultChecked = time.Time{}
	m.defaultReading = codexGuestSnapshot{}
	m.defaultError = nil
	m.attempted = nil
	m.loginNotice = ""
	m.launchPublished = ""
	m.mu.Unlock()
}

func (m *codexAccountManager) claim(id string) *claudeRefresh {
	if m.refreshing == nil {
		m.refreshing = make(map[string]*claudeRefresh)
	}
	if m.attempted == nil {
		m.attempted = make(map[string]time.Time)
	}
	now := time.Now()
	if _, exists := m.attempted[id]; !exists {
		pruneAttemptedMap(m.attempted, now)
	}
	m.attempted[id] = now
	call := &claudeRefresh{done: make(chan struct{})}
	m.refreshing[id] = call
	return call
}

func (m *codexAccountManager) finish(id string, call *claudeRefresh, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if errors.Is(err, errClaudeAccessLost) {
		delete(m.attempted, id)
	}
	delete(m.refreshing, id)
	call.err = err
	close(call.done)
}

func (s *Server) recoverCodexPending(ctx context.Context, id string, force bool) error {
	m := &s.codexAccounts
	m.mu.Lock()
	recent := time.Since(m.attempted[id]) < time.Minute
	m.mu.Unlock()
	if !force && recent {
		return nil
	}
	return s.refreshCodex(ctx, id, false)
}

// recheckCodexAccount observes an account in the background after a terminal
// using it ended at once, as the guest helper does when the account is signed
// out or changed. One probe per account runs at a time.
func (s *Server) recheckCodexAccount(id string) {
	epoch, unlocked := s.account.access()
	if !unlocked {
		return
	}
	m := &s.codexAccounts
	m.mu.Lock()
	list, err := s.loadCodexAccounts()
	if err != nil || list.find(id) == nil || m.refreshing[id] != nil {
		m.mu.Unlock()
		return
	}
	call := m.claim(id)
	m.mu.Unlock()
	ctx, finish, err := s.beginActivity(epoch)
	if err != nil {
		m.finish(id, call, err)
		return
	}
	go func() {
		defer finish()
		m.finish(id, call, s.runCodexRefresh(ctx, id, false))
	}()
}

// A poll begun before the login completed may have observed the previous
// state. Wait for it before the post-login observation.
func (s *Server) confirmCodexLogin(ctx context.Context, id string) error {
	m := &s.codexAccounts
	m.mu.Lock()
	prior := m.refreshing[id]
	m.mu.Unlock()
	if prior != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-prior.done:
		}
	}
	return s.refreshCodex(ctx, id, true)
}

// Login completion, panel polls and manual refreshes share one probe per account.
func (s *Server) refreshCodex(ctx context.Context, id string, finished bool) error {
	m := &s.codexAccounts
	m.mu.Lock()
	if call := m.refreshing[id]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			return call.err
		}
	}
	list, err := s.loadCodexAccounts()
	if err == nil && list.find(id) == nil && (id == "" || list.PendingLogin != id) {
		err = errors.New("未找到 Codex 账号")
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	call := m.claim(id)
	m.mu.Unlock()
	err = s.runCodexRefresh(ctx, id, finished)
	m.finish(id, call, err)
	return err
}

func (s *Server) runCodexRefresh(parent context.Context, id string, finished bool) error {
	epoch, unlocked := s.account.access()
	if !unlocked {
		return errClaudeAccessLost
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	reading, probeErr := s.probeCodex(ctx, id)
	if epoch.Err() != nil {
		return errClaudeAccessLost
	}
	if probeErr != nil {
		return errors.New("无法读取 Codex 账号状态，请确认环境已启动并检查 Codex CLI")
	}
	m := &s.codexAccounts
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch.Err() != nil {
		return errClaudeAccessLost
	}
	if ctx.Err() != nil {
		return errors.New("Codex 账号状态检查已取消")
	}
	list, err := s.loadCodexAccounts()
	if err != nil {
		return err
	}
	now, notice := time.Now(), ""
	if id == list.PendingLogin {
		// A sign-in without a ChatGPT identity cannot be recorded, but a new
		// login must not overwrite it.
		unrecorded := reading.subject() == "" && (reading.LoggedIn || reading.AuthMode != "")
		switch {
		case !finished && s.codexLoginActive(id):
			// A login started in this directory after the probe began. Its own
			// completion check records the result.
			return nil
		case reading.subject() != "":
			if list.bySubject(reading.subject()) != nil {
				notice = "此身份已在 Codex 账号列表中，本次登录未新增账号。如需更新原账号的登录，请在原账号卡片上重新登录。"
			} else {
				list.Accounts = append(list.Accounts, codexAccount{ID: id, Label: reading.email(), Enabled: true})
				err = list.observe(id, reading, now)
				notice = "已将登录成功的 Codex 账号加入自动分配"
			}
			list.PendingLogin = ""
		case finished:
			notice = "本次登录未完成，没有添加 Codex 账号"
			if unrecorded {
				notice = "本次登录不是 ChatGPT 订阅账号（例如 API Key 登录），没有添加 Codex 账号"
			}
			// A new attempt may already be entering this guest directory.
			if s.op.TryLock() {
				if !s.codexLoginActive(id) {
					list.PendingLogin = ""
				}
				s.op.Unlock()
			}
		}
		if unrecorded {
			m.unrecorded = id
		} else if m.unrecorded == id {
			m.unrecorded = ""
		}
	} else {
		err = list.observe(id, reading, now)
	}
	if err == nil {
		err = s.saveCodexAccounts(list)
	}
	if err == nil && notice != "" {
		m.loginNotice = notice
	}
	return err
}

// Terminal admission holds op through publication, so a removed account can
// never receive a terminal that was already being prepared for it.
func (s *Server) removeCodexAccount(id string) error {
	if !validClaudeID(id) {
		return errors.New("未找到要移除的 Codex 账号")
	}
	if !s.op.TryLock() {
		return errors.New("环境正在处理其他操作，请稍后重试")
	}
	defer s.op.Unlock()
	m := &s.codexAccounts
	m.mu.Lock()
	defer m.mu.Unlock()
	list, err := s.loadCodexAccounts()
	if err != nil {
		return err
	}
	a := list.find(id)
	if a == nil {
		return nil
	}
	if m.refreshing[id] != nil || a.Email != "" && m.usageActive[a.subject()] != nil {
		return errors.New("账号状态检查尚未结束，请稍后重试")
	}
	for _, session := range s.terminals.snapshot() {
		if session["codex_account_id"] == id && session["closed"] == false {
			return errors.New("请先关闭使用此账号的 Codex 终端或登录终端，再移除账号")
		}
	}
	list.Accounts = slices.DeleteFunc(list.Accounts, func(a codexAccount) bool { return a.ID == id })
	if err = s.saveCodexAccounts(list); err != nil {
		return err
	}
	delete(m.attempted, id)
	if m.lastPick == id {
		m.lastPick = ""
	}
	return nil
}

func (s *Server) refreshCodexUsage(ctx context.Context, id string) error {
	if !s.codexGuestSupported() {
		return errCodexUnsupported
	}
	if err := s.usageAvailability(); err != nil {
		return err
	}
	m := &s.codexAccounts
	m.mu.Lock()
	list, err := s.loadCodexAccounts()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	a := list.find(id)
	if a == nil {
		return errors.New("未找到要检查的 Codex 账号")
	}
	if a.Email == "" || a.Status != "ready" {
		if err = s.refreshCodex(ctx, id, false); err != nil {
			return err
		}
		m.mu.Lock()
		list, err = s.loadCodexAccounts()
		m.mu.Unlock()
		if err != nil {
			return err
		}
		if a = list.find(id); a == nil || a.Status != "ready" {
			return nil // The card shows why this account cannot be used.
		}
	}
	return s.checkCodexUsage(ctx, id, a.subject(), false)
}

func (s *Server) checkCodexUsage(parent context.Context, id, subject string, automatic bool) (err error) {
	if err = s.usageAvailability(); err != nil {
		return err
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return errClaudeAccessLost
	}
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	m := &s.codexAccounts
	m.mu.Lock()
	if call := m.usageActive[subject]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			return call.err
		}
	}
	list, err := s.loadCodexAccounts()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if a := list.find(id); a == nil || a.Email == "" || a.subject() != subject {
		m.mu.Unlock()
		return errors.New("账号身份已变化，请刷新面板")
	} else if automatic && !claudeUsageDue(a.UsageCheckedAt) {
		m.mu.Unlock()
		return nil
	}
	if len(m.usageActive) >= 2 {
		m.mu.Unlock()
		return errCodexUsageBusy
	}
	if m.usageActive == nil {
		m.usageActive = make(map[string]*claudeUsageCall)
	}
	call := &claudeUsageCall{done: make(chan struct{})}
	m.usageActive[subject] = call
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.usageActive, subject)
		call.err = err
		close(call.done)
		m.mu.Unlock()
	}()
	probe := m.usageProbe
	if probe == nil {
		probe = s.readCodexGuestUsage
	}
	reading, probeErr := probe(ctx, id, subject)
	if epoch.Err() != nil {
		return errClaudeAccessLost
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if list, err = s.loadCodexAccounts(); err != nil {
		return err
	}
	a := list.find(id)
	if a == nil || a.subject() != subject {
		return errors.New("账号身份已变化，未记录此次额度")
	}
	now := time.Now()
	status := reading.UsageStatus
	switch {
	case probeErr != nil || status == "error":
		status = "error"
	case status == "identity_changed" || reading.subject() != subject:
		status = "identity_changed"
		a.Status = "changed"
	case status == "reported" || status == "partial" || status == "cached":
		a.mergeQuota(reading.Quota, now)
	case status == "unavailable" || status == "not_applicable":
	default:
		status = "error"
	}
	if status != "error" && status != "identity_changed" {
		a.Status = "ready"
		if reading.Plan != "" {
			a.Plan = reading.Plan
		}
	}
	a.UsageStatus, a.UsageCheckedAt = status, now.Unix()
	return s.saveCodexAccounts(list)
}

// Opening the panel checks due accounts one at a time while it stays visible.
func (s *Server) startCodexPanelUsage(list codexAccountList) {
	if !s.codexGuestSupported() || s.usageAvailability() != nil {
		return
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return
	}
	m := &s.codexAccounts
	m.mu.Lock()
	m.panelUntil = time.Now().Add(claudePanelLease)
	if m.panelRunning {
		m.mu.Unlock()
		return
	}
	var targets []codexAccount
	for _, a := range list.Accounts {
		if a.Status == "ready" && a.Email != "" && claudeUsageDue(a.UsageCheckedAt) || a.Status != "ready" && time.Since(m.attempted[a.ID]) >= time.Minute {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		m.mu.Unlock()
		return
	}
	m.panelRunning = true
	m.mu.Unlock()
	ctx, finish, err := s.beginActivity(epoch)
	if err != nil {
		m.mu.Lock()
		m.panelRunning = false
		m.mu.Unlock()
		return
	}
	go func() {
		defer finish()
		defer func() { m.mu.Lock(); m.panelRunning = false; m.mu.Unlock() }()
		for _, a := range targets {
			m.mu.Lock()
			visible := time.Now().Before(m.panelUntil)
			m.mu.Unlock()
			if !visible || ctx.Err() != nil || s.usageAvailability() != nil {
				return
			}
			if a.Status == "ready" {
				_ = s.checkCodexUsage(ctx, a.ID, a.subject(), true)
			} else {
				_ = s.refreshCodex(ctx, a.ID, false)
			}
		}
	}()
}
