package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/agentcatalog"
)

type agentGuestSnapshot struct {
	LoggedIn       bool         `json:"logged_in"`
	AuthMode       string       `json:"auth_mode,omitempty"`
	Email          string       `json:"email,omitempty"`
	Username       string       `json:"username,omitempty"`
	Organization   string       `json:"organization,omitempty"`
	Plan           string       `json:"plan,omitempty"`
	Provider       string       `json:"provider,omitempty"`
	BaseURL        string       `json:"base_url,omitempty"`
	Model          string       `json:"model,omitempty"`
	KeyHint        string       `json:"key_hint,omitempty"`
	KeyFingerprint string       `json:"key_fingerprint,omitempty"`
	EnvVar         string       `json:"env_var,omitempty"`
	ExpiresAt      int64        `json:"expires_at,omitempty"`
	Quota          *claudeQuota `json:"quota,omitempty"`
}

func (g agentGuestSnapshot) identityKey(agentID string) string {
	if !g.LoggedIn {
		return ""
	}
	if g.KeyFingerprint != "" {
		return "key:" + g.KeyFingerprint
	}
	parts := []string{
		agentID,
		strings.ToLower(strings.TrimSpace(g.Email)),
		strings.ToLower(strings.TrimSpace(g.Username)),
		strings.TrimSpace(g.Organization),
		strings.TrimSpace(g.Provider),
	}
	if parts[1] == "" && parts[2] == "" && parts[4] == "" {
		return "logged:" + agentID
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:16])
}

func (g agentGuestSnapshot) suggestedLabel(fallback string) string {
	for _, s := range []string{g.Email, g.Username, g.Provider, g.KeyHint, fallback} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return "新账号"
}

type agentDetected struct {
	Identity     string       `json:"identity"`
	AuthMode     string       `json:"auth_mode,omitempty"`
	Email        string       `json:"email,omitempty"`
	Username     string       `json:"username,omitempty"`
	Organization string       `json:"organization,omitempty"`
	Plan         string       `json:"plan,omitempty"`
	Provider     string       `json:"provider,omitempty"`
	Quota        *claudeQuota `json:"quota,omitempty"`
	CheckedAt    int64        `json:"checked_at"`
}

type agentSelection struct {
	AgentID  string
	ID       string
	GuestID  string
	Label    string
	AuthMode string
	Reason   string
}

type agentAccountMutation struct {
	Action          string           `json:"action"`
	ID              string           `json:"id"`
	Label           string           `json:"label"`
	Enabled         *bool            `json:"enabled"`
	Weight          *int             `json:"weight"`
	MaxConcurrency  *int             `json:"max_concurrency"`
	HandoffGroup    *string          `json:"handoff_group"`
	APIKey          string           `json:"api_key"`
	EnvVar          string           `json:"env_var"`
	BaseURL         string           `json:"base_url"`
	Model           string           `json:"model"`
	Provider        string           `json:"provider"`
	Scheduling      *agentScheduling `json:"scheduling"`
	Order           []string         `json:"order"`
	CooldownSeconds int64            `json:"cooldown_seconds"`
	ErrorKind       string           `json:"error_kind"`
	Workspace       string           `json:"workspace"`
	PinTarget       string           `json:"pin_target"`
}

func (s *Server) loadAgentPoolsLocked() (map[string]*agentAccountPool, error) {
	m := &s.agentCLIs
	if m.poolsLoaded {
		return m.pools, nil
	}
	pools := map[string]*agentAccountPool{}
	data, err := os.ReadFile(filepath.Join(s.profile, "agent-accounts.json"))
	if os.IsNotExist(err) {
		m.pools, m.poolsLoaded = pools, true
		return pools, nil
	}
	if err != nil || json.Unmarshal(data, &pools) != nil || pools == nil {
		return nil, errors.New("Agent 账号数据文件无法读取，请检查配置目录或恢复备份")
	}
	for id, pool := range pools {
		if pool == nil {
			pools[id] = &agentAccountPool{Accounts: []agentAccount{}, Scheduling: defaultAgentScheduling()}
			continue
		}
		if pool.Accounts == nil {
			pool.Accounts = []agentAccount{}
		}
		pool.Scheduling = pool.Scheduling.normalized()
	}
	m.pools, m.poolsLoaded = pools, true
	return pools, nil
}

func cloneAgentPools(pools map[string]*agentAccountPool) map[string]*agentAccountPool {
	out := make(map[string]*agentAccountPool, len(pools))
	for k, v := range pools {
		if v == nil {
			continue
		}
		cp := cloneAgentPool(v)
		out[k] = &cp
	}
	return out
}

func (s *Server) saveAgentPoolsLocked(pools map[string]*agentAccountPool) error {
	if s.agentCLIs.flushTimer != nil {
		s.agentCLIs.flushTimer.Stop()
		s.agentCLIs.flushTimer = nil
	}
	s.agentCLIs.poolsGen++
	gen := s.agentCLIs.poolsGen
	s.agentCLIs.flushMu.Lock()
	err := writePrivate(filepath.Join(s.profile, "agent-accounts.json"), pools)
	if err == nil && gen > s.agentCLIs.lastWrittenGen {
		s.agentCLIs.lastWrittenGen = gen
	}
	s.agentCLIs.flushMu.Unlock()
	if err != nil {
		return errors.New("Agent 账号配置未能保存，请检查磁盘空间和权限")
	}
	s.agentCLIs.pools = pools
	s.agentCLIs.poolsLoaded = true
	s.agentCLIs.poolsDirty = false
	return nil
}

// scheduleAgentPoolsFlushLocked coalesces metadata-only updates (like terminal
// allocation timestamps/counters) into a single async write, eliminating
// synchronous disk fsync latency from the terminal launch critical path.
func (s *Server) scheduleAgentPoolsFlushLocked() {
	if s.agentCLIs.closed {
		return
	}
	s.agentCLIs.poolsGen++
	s.agentCLIs.poolsDirty = true
	if s.agentCLIs.flushTimer != nil {
		return
	}
	s.agentCLIs.flushTimer = time.AfterFunc(agentFlushDelay, func() {
		s.flushAgentPools()
	})
}

func (s *Server) flushAgentPools() {
	s.agentCLIs.mu.Lock()
	if s.agentCLIs.flushTimer != nil {
		s.agentCLIs.flushTimer.Stop()
		s.agentCLIs.flushTimer = nil
	}
	if !s.agentCLIs.poolsDirty || !s.agentCLIs.poolsLoaded || s.agentCLIs.pools == nil {
		s.agentCLIs.mu.Unlock()
		return
	}
	gen := s.agentCLIs.poolsGen
	snapshot := cloneAgentPools(s.agentCLIs.pools)
	s.agentCLIs.mu.Unlock()

	// Write outside s.agentCLIs.mu so disk fsync never stalls terminal scheduling
	// or concurrent HTTP readers; flushMu + monotonic generation prevent stale overwrites.
	s.agentCLIs.flushMu.Lock()
	var writeErr error
	if gen > s.agentCLIs.lastWrittenGen {
		writeErr = writePrivate(filepath.Join(s.profile, "agent-accounts.json"), snapshot)
		if writeErr == nil {
			s.agentCLIs.lastWrittenGen = gen
		}
	}
	s.agentCLIs.flushMu.Unlock()

	if writeErr == nil {
		s.agentCLIs.mu.Lock()
		if s.agentCLIs.poolsGen == gen {
			s.agentCLIs.poolsDirty = false
		}
		s.agentCLIs.mu.Unlock()
	}
}

// closeAgents seals agent pool scheduling against late async timers and flushes
// any pending metadata snapshot cleanly during server shutdown.
func (s *Server) closeAgents() {
	s.agentCLIs.mu.Lock()
	s.agentCLIs.closed = true
	if s.agentCLIs.flushTimer != nil {
		s.agentCLIs.flushTimer.Stop()
		s.agentCLIs.flushTimer = nil
	}
	dirty := s.agentCLIs.poolsDirty && s.agentCLIs.poolsLoaded && s.agentCLIs.pools != nil
	var gen uint64
	var snapshot map[string]*agentAccountPool
	if dirty {
		gen = s.agentCLIs.poolsGen
		snapshot = cloneAgentPools(s.agentCLIs.pools)
	}
	s.agentCLIs.mu.Unlock()

	if !dirty {
		return
	}
	s.agentCLIs.flushMu.Lock()
	var writeErr error
	if gen > s.agentCLIs.lastWrittenGen {
		writeErr = writePrivate(filepath.Join(s.profile, "agent-accounts.json"), snapshot)
		if writeErr == nil {
			s.agentCLIs.lastWrittenGen = gen
		}
	}
	s.agentCLIs.flushMu.Unlock()

	if writeErr == nil {
		s.agentCLIs.mu.Lock()
		if s.agentCLIs.poolsGen == gen {
			s.agentCLIs.poolsDirty = false
		}
		s.agentCLIs.mu.Unlock()
	}
}

func (s *Server) ensureAgentPoolLocked(pools map[string]*agentAccountPool, agentID string) *agentAccountPool {
	pool := pools[agentID]
	if pool == nil {
		pool = &agentAccountPool{Accounts: []agentAccount{}, Scheduling: defaultAgentScheduling()}
		pools[agentID] = pool
	}
	return pool
}

func (s *Server) activeAgentTerminals(agentID string) (map[string]int, map[string]bool) {
	return s.terminals.agentCounts(agentID)
}

func (s *Server) probeAgentProfile(ctx context.Context, spec agentcatalog.AgentSpec, accountID string) (agentGuestSnapshot, error) {
	var snap agentGuestSnapshot
	err := s.runAgentRuntime(ctx, "snapshot", map[string]any{"spec": spec, "account_id": accountID}, &snap)
	return snap, err
}

// inspectAgentDefault caches & coalesces default profile probes for 20s per
// agent so opening Account Management or switching tabs is sub-millisecond.
func (s *Server) inspectAgentDefault(parent context.Context, spec agentcatalog.AgentSpec, force bool) (agentGuestSnapshot, time.Time, error) {
	m := &s.agentCLIs
	for {
		m.mu.Lock()
		if m.defaultCache == nil {
			m.defaultCache = map[string]*agentDefaultProbeEntry{}
		}
		entry := m.defaultCache[spec.ID]
		if entry == nil {
			entry = &agentDefaultProbeEntry{}
			m.defaultCache[spec.ID] = entry
		}
		if !force && !entry.checkedAt.IsZero() && time.Since(entry.checkedAt) < agentDefaultProbeTTL {
			snap, checked, err := entry.snap, entry.checkedAt, entry.err
			m.mu.Unlock()
			return snap, checked, err
		}
		if call := entry.inflight; call != nil {
			m.mu.Unlock()
			select {
			case <-parent.Done():
				return agentGuestSnapshot{}, time.Time{}, parent.Err()
			case <-call.done:
				if force {
					continue
				}
				return call.snap, time.Now(), call.err
			}
		}
		call := &agentDefaultProbeCall{done: make(chan struct{})}
		entry.inflight = call
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(parent, 6*time.Second)
		snap, err := s.probeAgentProfile(ctx, spec, "default")
		cancel()
		now := time.Now()

		m.mu.Lock()
		call.snap, call.err = snap, err
		entry.snap, entry.checkedAt, entry.err = snap, now, err
		if entry.inflight == call {
			entry.inflight = nil
		}
		close(call.done)
		m.mu.Unlock()
		return snap, now, err
	}
}

func (s *Server) invalidateAgentDefaultCache(agentID string) {
	s.agentCLIs.mu.Lock()
	if s.agentCLIs.defaultCache != nil {
		if entry := s.agentCLIs.defaultCache[agentID]; entry != nil {
			entry.checkedAt = time.Time{}
		}
	}
	s.agentCLIs.mu.Unlock()
}

func applySnapshotToAccount(a *agentAccount, snap agentGuestSnapshot, now time.Time) {
	a.CheckedAt = now.Unix()
	if !snap.LoggedIn {
		if a.Source != "api_key" {
			a.Status = "signed_out"
		}
		return
	}
	a.Status = "ready"
	if snap.AuthMode != "" {
		a.AuthMode = snap.AuthMode
	}
	if snap.Email != "" {
		a.Email = snap.Email
	}
	if snap.Username != "" {
		a.Username = snap.Username
	}
	if snap.Organization != "" {
		a.Organization = snap.Organization
	}
	if snap.Plan != "" {
		a.Plan = snap.Plan
	}
	if snap.Provider != "" {
		a.Provider = snap.Provider
	}
	if snap.BaseURL != "" {
		a.BaseURL = snap.BaseURL
	}
	if snap.Model != "" {
		a.Model = snap.Model
	}
	if snap.KeyHint != "" {
		a.KeyHint = snap.KeyHint
	}
	if snap.KeyFingerprint != "" {
		a.KeyFingerprint = snap.KeyFingerprint
	}
	if snap.EnvVar != "" {
		a.EnvVar = snap.EnvVar
	}
	if snap.ExpiresAt > 0 {
		a.ExpiresAt = snap.ExpiresAt
	}
	if snap.Quota != nil {
		a.mergeQuota(snap.Quota, now)
	}
}

func (s *Server) listAgentAccountsHTTP(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.PathValue("id"))
	spec, cfg, ok := s.lookupAgentSpec(agentID)
	if !ok {
		http.Error(w, "未找到指定的 Agent CLI", http.StatusNotFound)
		return
	}
	if spec.ID == "claude" {
		s.listClaudeAccounts(w, r)
		return
	}
	workspace := strings.TrimSpace(r.URL.Query().Get("workspace"))
	if workspace == "" {
		workspace = s.workspace()
	}
	skipDetect := r.URL.Query().Get("skip_detect") == "1"
	forceRefresh := r.URL.Query().Get("refresh") == "1"

	// CRITICAL: call s.vmRunning() BEFORE s.mu.Lock() to prevent self-deadlock on Windows.
	running := s.vmRunning()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()

	detectionStatus := "offline"
	detectionError := ""
	var detected *agentDetected

	if !skipDetect && ready && running && s.windowsGuest == nil && spec.Isolation.Mode != "api_key_only" {
		snap, checkedAt, err := s.inspectAgentDefault(r.Context(), spec, forceRefresh)
		if err != nil {
			detectionStatus = "unavailable"
			detectionError = err.Error()
		} else if !snap.LoggedIn {
			detectionStatus = "signed_out"
		} else {
			detectionStatus = "ready"
			ts := checkedAt.Unix()
			if ts <= 0 {
				ts = time.Now().Unix()
			}
			detected = &agentDetected{
				Identity:     snap.identityKey(spec.ID),
				AuthMode:     snap.AuthMode,
				Email:        snap.Email,
				Username:     snap.Username,
				Organization: snap.Organization,
				Plan:         snap.Plan,
				Provider:     snap.Provider,
				Quota:        snap.Quota,
				CheckedAt:    ts,
			}
		}
	}

	if !skipDetect && ready && running && s.windowsGuest == nil {
		// Join any in-flight login confirmation (closing the terminal close -> GET race),
		// or recover an unconfirmed PendingLogin (e.g. after runtime recovery or restart)
		// without any background polling timers.
		s.agentCLIs.mu.Lock()
		var pendingID string
		var inflight *agentConfirmCall
		var attempted time.Time
		if pools, loadErr := s.loadAgentPoolsLocked(); loadErr == nil {
			if pool := pools[spec.ID]; pool != nil && pool.PendingLogin != "" {
				pendingID = pool.PendingLogin
				inflight = s.agentCLIs.confirmInflight[spec.ID+":"+pendingID]
				attempted = s.agentCLIs.pendingAttempted[spec.ID]
			}
		}
		s.agentCLIs.mu.Unlock()

		if inflight != nil {
			// Never block a GET waiting on a confirm blocked behind a job or
			// long operation holding s.op, and always bound joiner waits.
			if !s.busy() && s.op.TryLock() {
				s.op.Unlock()
				joinTimer := time.NewTimer(agentConfirmJoinTimeout)
				select {
				case <-r.Context().Done():
				case <-inflight.done:
				case <-joinTimer.C:
				}
				if !joinTimer.Stop() {
					select {
					case <-joinTimer.C:
					default:
					}
				}
			}
		} else if pendingID != "" && !s.busy() {
			if s.op.TryLock() {
				s.op.Unlock()
				_, logins := s.activeAgentTerminals(spec.ID)
				if !logins[pendingID] && (forceRefresh || attempted.IsZero() || time.Since(attempted) >= agentDefaultProbeTTL) {
					ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
					_ = s.confirmAgentLoginWithLockMode(ctx, spec.ID, pendingID, true)
					cancel()
				}
			}
		}
	}

	s.agentCLIs.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		s.agentCLIs.mu.Unlock()
		requestError(w, err)
		return
	}
	pool := s.ensureAgentPoolLocked(pools, spec.ID)
	// Sync fresh default quota/status onto joined default account if present.
	if detected != nil {
		for i := range pool.Accounts {
			if pool.Accounts[i].Source == "default" {
				if detected.Quota != nil && pool.Accounts[i].mergeQuota(detected.Quota, time.Now()) {
					s.scheduleAgentPoolsFlushLocked()
				}
				detected = nil
				break
			}
		}
	}
	poolCopy := cloneAgentPool(pool)
	last := s.agentCLIs.lastPick[spec.ID]
	var rec claudeProjectPick
	if wsMap := s.agentCLIs.recent[spec.ID]; wsMap != nil {
		rec = wsMap[workspace]
	}
	notice := s.agentCLIs.loginNotice[spec.ID]
	s.agentCLIs.mu.Unlock()

	active, logins := s.activeAgentTerminals(spec.ID)
	preview := pickAgentAccount(poolCopy, active, logins, workspace, last, rec.id, rec.at, time.Now())

	jsonReply(w, map[string]any{
		"agent":            spec,
		"config":           cfg,
		"launch_command":   s.effectiveAgentLaunchCommand(spec, cfg),
		"accounts":         poolCopy.Accounts,
		"scheduling":       poolCopy.Scheduling.normalized(),
		"schedule_preview": preview,
		"active_counts":    active,
		"detected":         detected,
		"detection_status": detectionStatus,
		"detection_error":  detectionError,
		"login_notice":     notice,
	})
}

func (s *Server) mutateAgentAccountsHTTP(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.PathValue("id"))
	spec, _, ok := s.lookupAgentSpec(agentID)
	if !ok {
		http.Error(w, "未找到指定的 Agent CLI", http.StatusNotFound)
		return
	}
	if spec.ID == "claude" {
		s.mutateClaudeAccounts(w, r)
		return
	}
	var in agentAccountMutation
	if !readJSON(w, r, &in) {
		return
	}

	var err error
	switch in.Action {
	case "join":
		err = s.joinAgentDefault(r.Context(), spec)
	case "add_api_key":
		err = s.addAgentAPIKeyAccount(r.Context(), spec, in)
	case "update":
		err = s.updateAgentAccount(r.Context(), spec, in)
	case "scheduling":
		err = s.updateAgentScheduling(spec.ID, in)
	case "pin_project":
		err = s.pinAgentProject(spec.ID, in.Workspace, in.PinTarget)
	case "cooldown":
		err = s.setAgentAccountCooldown(spec.ID, in)
	case "refresh", "refresh_default":
		err = s.refreshAgentAccount(r.Context(), spec, in)
	case "remove":
		err = s.removeAgentAccount(r.Context(), spec, in.ID)
	default:
		err = errors.New("未知账号操作: " + in.Action)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.listAgentAccountsHTTP(w, r)
}

func (s *Server) joinAgentDefault(ctx context.Context, spec agentcatalog.AgentSpec) error {
	if !s.vmRunning() {
		return errors.New("请先启动 Cage 环境后再加入默认登录")
	}
	snap, _, err := s.inspectAgentDefault(ctx, spec, true)
	if err != nil {
		return err
	}
	if !snap.LoggedIn {
		return errors.New("普通终端中尚未检测到已登录的账号，请先在终端完成登录后重试")
	}
	s.op.Lock()
	defer s.op.Unlock()
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	pool := s.ensureAgentPoolLocked(pools, spec.ID)
	for i := range pool.Accounts {
		if pool.Accounts[i].Source == "default" {
			applySnapshotToAccount(&pool.Accounts[i], snap, time.Now())
			return s.saveAgentPoolsLocked(pools)
		}
	}
	acc := agentAccount{
		ID:         randomToken()[:24],
		Label:      snap.suggestedLabel(spec.ShortName + " 默认登录"),
		Source:     "default",
		AuthMode:   snap.AuthMode,
		Enabled:    true,
		Status:     "ready",
		Weight:     10,
		HealthEWMA: 1.0,
	}
	if acc.AuthMode == "" {
		acc.AuthMode = "oauth"
	}
	applySnapshotToAccount(&acc, snap, time.Now())
	pool.Accounts = append(pool.Accounts, acc)
	s.setAgentNoticeLocked(spec.ID, "已将普通终端默认登录加入智能调度池")
	return s.saveAgentPoolsLocked(pools)
}

func (s *Server) addAgentAPIKeyAccount(ctx context.Context, spec agentcatalog.AgentSpec, in agentAccountMutation) error {
	if !spec.SupportsAuth("api_key") {
		return fmt.Errorf("%s 不支持 API Key 认证模式", spec.Name)
	}
	if strings.TrimSpace(in.APIKey) == "" {
		return errors.New("请输入有效的 API Key")
	}
	// Validate all user-supplied fields BEFORE writing any credential to the guest.
	label := strings.TrimSpace(in.Label)
	if label != "" && !validClaudeLabel(label) {
		return errors.New("账号名称须为 1-64 个有效字符")
	}
	weight := 10
	if in.Weight != nil {
		if *in.Weight < 1 || *in.Weight > 100 {
			return errors.New("权重须在 1 到 100 之间")
		}
		weight = *in.Weight
	}
	maxConc := 0
	if in.MaxConcurrency != nil {
		if *in.MaxConcurrency < 0 || *in.MaxConcurrency > 64 {
			return errors.New("最大并发数须在 0（不限）到 64 之间")
		}
		maxConc = *in.MaxConcurrency
	}
	group := ""
	if in.HandoffGroup != nil {
		group = strings.TrimSpace(*in.HandoffGroup)
		if len(group) > 64 || strings.IndexFunc(group, unicode.IsControl) >= 0 {
			return errors.New("分组名称最多 64 字节，不能含控制字符")
		}
	}
	if !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
		return errors.New("请先启动 Cage 环境后再添加 API Key 账号")
	}

	id := randomToken()[:24]
	var meta agentGuestSnapshot
	err := s.runAgentRuntime(ctx, "set-api-key", map[string]any{
		"spec":       spec,
		"account_id": id,
		"api_key":    in.APIKey,
		"env_var":    in.EnvVar,
		"base_url":   in.BaseURL,
		"model":      in.Model,
		"provider":   in.Provider,
	}, &meta)
	if err != nil {
		return err
	}
	if label == "" {
		label = meta.suggestedLabel(spec.ShortName + " API")
		if !validClaudeLabel(label) {
			label = spec.ShortName + " API"
		}
	}

	var saveErr error
	func() {
		s.op.Lock()
		defer s.op.Unlock()
		s.agentCLIs.mu.Lock()
		defer s.agentCLIs.mu.Unlock()
		pools, loadErr := s.loadAgentPoolsLocked()
		if loadErr != nil {
			saveErr = loadErr
			return
		}
		nextPools := cloneAgentPools(pools)
		pool := s.ensureAgentPoolLocked(nextPools, spec.ID)
		acc := agentAccount{
			ID:             id,
			Label:          label,
			Source:         "api_key",
			AuthMode:       "api_key",
			Enabled:        true,
			Status:         "ready",
			Weight:         weight,
			MaxConcurrency: maxConc,
			HandoffGroup:   group,
			HealthEWMA:     1.0,
		}
		applySnapshotToAccount(&acc, meta, time.Now())
		pool.Accounts = append(pool.Accounts, acc)
		saveErr = s.saveAgentPoolsLocked(nextPools)
		if saveErr == nil {
			s.setAgentNoticeLocked(spec.ID, "已安全写入隔离 API Key（0600 权限）并加入调度池")
		}
	}()
	if saveErr != nil {
		// Roll back the guest credential so a failed roster save never orphans a secret.
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rbErr := s.runAgentRuntime(rbCtx, "clear-credentials", map[string]any{"spec": spec, "account_id": id}, nil)
		cancel()
		if rbErr != nil {
			return fmt.Errorf("%w（且客机凭据回滚失败，请稍后重试清理: %v）", saveErr, rbErr)
		}
		return saveErr
	}
	return nil
}

func (s *Server) updateAgentAccount(ctx context.Context, spec agentcatalog.AgentSpec, in agentAccountMutation) error {
	if !validClaudeID(in.ID) {
		return errors.New("账号 ID 无效")
	}
	// Validate all user-supplied fields BEFORE acquiring locks or mutating guest credentials.
	if in.Label != "" && !validClaudeLabel(strings.TrimSpace(in.Label)) {
		return errors.New("账号备注须为 1-64 个有效字符")
	}
	if in.Weight != nil && (*in.Weight < 1 || *in.Weight > 100) {
		return errors.New("权重须在 1 到 100 之间")
	}
	if in.MaxConcurrency != nil && (*in.MaxConcurrency < 0 || *in.MaxConcurrency > 64) {
		return errors.New("最大并发数须在 0（不限）到 64 之间")
	}
	if in.HandoffGroup != nil {
		g := strings.TrimSpace(*in.HandoffGroup)
		if len(g) > 64 || strings.IndexFunc(g, unicode.IsControl) >= 0 {
			return errors.New("分组名称最多 64 字节，不能含控制字符")
		}
	}

	// Serialize with concurrent updates, removals, and terminal creation.
	s.op.Lock()
	defer s.op.Unlock()

	var existingSource, existingGuestID string
	var existingBaseURL, existingModel, existingProvider, existingEnvVar string
	var lookupErr error
	s.agentCLIs.mu.Lock()
	if pools, err := s.loadAgentPoolsLocked(); err != nil {
		lookupErr = err
	} else if a := s.ensureAgentPoolLocked(pools, spec.ID).find(in.ID); a == nil {
		lookupErr = errors.New("未找到要更新的账号")
	} else {
		existingSource = a.Source
		existingGuestID = a.guestID()
		existingBaseURL = a.BaseURL
		existingModel = a.Model
		existingProvider = a.Provider
		existingEnvVar = a.EnvVar
	}
	s.agentCLIs.mu.Unlock()
	if lookupErr != nil {
		return lookupErr
	}

	var updatedMeta *agentGuestSnapshot
	mutatedGuestKey := false
	mutatedGuestMeta := false
	if strings.TrimSpace(in.APIKey) != "" {
		// Prevent writing an API key onto an OAuth/default account whose Source != "api_key",
		// which would leave mismatched Source metadata and orphaned secrets.
		if existingSource != "api_key" || existingGuestID == "default" {
			return errors.New("仅 API Key 类型账号支持修改密钥；OAuth 或默认登录请新增独立的 API Key 账号")
		}
		if !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
			return errors.New("请先启动 Cage 环境后再更新 API Key")
		}
		effectiveEnvVar := strings.TrimSpace(in.EnvVar)
		if effectiveEnvVar == "" {
			effectiveEnvVar = existingEnvVar
		}
		var meta agentGuestSnapshot
		err := s.runAgentRuntime(ctx, "set-api-key", map[string]any{
			"spec":       spec,
			"account_id": existingGuestID,
			"api_key":    in.APIKey,
			"env_var":    effectiveEnvVar,
			"base_url":   in.BaseURL,
			"model":      in.Model,
			"provider":   in.Provider,
		}, &meta)
		if err != nil {
			return err
		}
		updatedMeta = &meta
		mutatedGuestKey = true
	} else if existingSource == "api_key" && (in.BaseURL != "" || in.Model != "" || in.Provider != "" || in.EnvVar != "") {
		// Allow editing base_url / model / provider / env_var without re-entering raw API key.
		if in.BaseURL != existingBaseURL || in.Model != existingModel || in.Provider != existingProvider || (in.EnvVar != "" && in.EnvVar != existingEnvVar) {
			if !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
				return errors.New("请先启动 Cage 环境后再修改 API 网关或模型配置")
			}
			var meta agentGuestSnapshot
			err := s.runAgentRuntime(ctx, "update-api-key-meta", map[string]any{
				"spec":       spec,
				"account_id": existingGuestID,
				"env_var":    in.EnvVar,
				"base_url":   in.BaseURL,
				"model":      in.Model,
				"provider":   in.Provider,
			}, &meta)
			if err != nil {
				return err
			}
			updatedMeta = &meta
			mutatedGuestMeta = true
		}
	}

	var saveErr error
	func() {
		s.agentCLIs.mu.Lock()
		defer s.agentCLIs.mu.Unlock()
		pools, err := s.loadAgentPoolsLocked()
		if err != nil {
			saveErr = err
			return
		}
		nextPools := cloneAgentPools(pools)
		pool := s.ensureAgentPoolLocked(nextPools, spec.ID)
		a := pool.find(in.ID)
		if a == nil {
			saveErr = errors.New("未找到要更新的账号")
			return
		}
		if label := strings.TrimSpace(in.Label); in.Label != "" {
			a.Label = label
		}
		if in.Enabled != nil {
			a.Enabled = *in.Enabled
		}
		if in.Weight != nil {
			a.Weight = *in.Weight
		}
		if in.MaxConcurrency != nil {
			a.MaxConcurrency = *in.MaxConcurrency
		}
		if in.HandoffGroup != nil {
			a.HandoffGroup = strings.TrimSpace(*in.HandoffGroup)
		}
		if updatedMeta != nil {
			applySnapshotToAccount(a, *updatedMeta, time.Now())
			a.BaseURL = updatedMeta.BaseURL
			a.Model = updatedMeta.Model
			a.Provider = updatedMeta.Provider
			if mutatedGuestKey {
				clearAccountCooldown(a)
			}
		}
		saveErr = s.saveAgentPoolsLocked(nextPools)
	}()
	if saveErr != nil {
		var rbErr error
		metaChanged := (mutatedGuestMeta || mutatedGuestKey) && (updatedMeta == nil ||
			updatedMeta.BaseURL != existingBaseURL ||
			updatedMeta.Model != existingModel ||
			updatedMeta.Provider != existingProvider ||
			updatedMeta.EnvVar != existingEnvVar)
		if metaChanged {
			rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rbErr = s.runAgentRuntime(rbCtx, "update-api-key-meta", map[string]any{
				"spec":       spec,
				"account_id": existingGuestID,
				"env_var":    existingEnvVar,
				"base_url":   existingBaseURL,
				"model":      existingModel,
				"provider":   existingProvider,
			}, nil)
			cancel()
		}
		if mutatedGuestKey && rbErr != nil {
			return fmt.Errorf("%w（注意：客机隔离 API Key 已更新，且元数据回滚失败: %v）", saveErr, rbErr)
		}
		if mutatedGuestKey && metaChanged {
			return fmt.Errorf("%w（注意：客机隔离 API Key 已更新但本地元数据保存失败，已回滚客机网关/模型元数据，请检查磁盘后重试）", saveErr)
		}
		if mutatedGuestKey {
			return fmt.Errorf("%w（注意：客机隔离 API Key 已更新但本地元数据保存失败，请检查磁盘后重试）", saveErr)
		}
		if rbErr != nil {
			return fmt.Errorf("%w（客机配置回滚亦失败: %v）", saveErr, rbErr)
		}
		return saveErr
	}
	return nil
}

func (s *Server) updateAgentScheduling(agentID string, in agentAccountMutation) error {
	if in.Scheduling == nil {
		return errors.New("缺少调度策略配置")
	}
	sched := in.Scheduling.normalized()
	if !sched.valid() {
		return errors.New("请选择有效的调度模式与项目亲和时长")
	}
	s.op.Lock()
	defer s.op.Unlock()
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	pool := s.ensureAgentPoolLocked(pools, agentID)
	if sched.ProjectPins == nil {
		sched.ProjectPins = pool.Scheduling.ProjectPins
	}
	pool.Scheduling = sched
	if in.Order != nil {
		if len(in.Order) != len(pool.Accounts) {
			return errors.New("账号列表已变化，请刷新后再排序")
		}
		ordered := make([]agentAccount, 0, len(in.Order))
		seen := map[string]bool{}
		for _, id := range in.Order {
			a := pool.find(id)
			if a == nil || seen[id] {
				return errors.New("账号顺序无效")
			}
			seen[id] = true
			ordered = append(ordered, *a)
		}
		pool.Accounts = ordered
	}
	return s.saveAgentPoolsLocked(pools)
}

func (s *Server) pinAgentProject(agentID, workspace, target string) error {
	workspace = strings.TrimSpace(workspace)
	target = strings.TrimSpace(target)
	if workspace == "" || len(workspace) > 1024 {
		return errors.New("项目路径无效")
	}
	if target != "" && !strings.HasPrefix(target, "account:") && !strings.HasPrefix(target, "group:") {
		return errors.New("绑定目标须为 account:<ID> 或 group:<分组名>")
	}
	s.op.Lock()
	defer s.op.Unlock()
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	pool := s.ensureAgentPoolLocked(pools, agentID)
	if pool.Scheduling.ProjectPins == nil {
		pool.Scheduling.ProjectPins = map[string]string{}
	}
	if target == "" {
		delete(pool.Scheduling.ProjectPins, workspace)
	} else {
		if utf8.RuneCountInString(target) > 96 {
			return errors.New("绑定目标过长")
		}
		pool.Scheduling.ProjectPins[workspace] = target
	}
	return s.saveAgentPoolsLocked(pools)
}

func (s *Server) setAgentAccountCooldown(agentID string, in agentAccountMutation) error {
	if !validClaudeID(in.ID) {
		return errors.New("账号 ID 无效")
	}
	s.op.Lock()
	defer s.op.Unlock()
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	pool := s.ensureAgentPoolLocked(pools, agentID)
	a := pool.find(in.ID)
	if a == nil {
		return errors.New("未找到指定的账号")
	}
	if in.CooldownSeconds < 0 {
		clearAccountCooldown(a)
		s.setAgentNoticeLocked(agentID, fmt.Sprintf("已恢复「%s」的调度健康状态", a.displayName()))
	} else {
		applyAccountCooldown(a, in.ErrorKind, in.CooldownSeconds, time.Now())
		s.setAgentNoticeLocked(agentID, fmt.Sprintf("已将「%s」置入熔断冷却，新建终端将自动路由至其他健康账号", a.displayName()))
	}
	return s.saveAgentPoolsLocked(pools)
}

// onAgentRateLimit is invoked passively by terminal PTY output scanning when an
// agent CLI emits a 429 / rate-limit / quota-exceeded error. It honors the
// pool's AutoCooldown setting and applies exponential backoff without blocking PTY reads.
func (s *Server) onAgentRateLimit(agentID, accountID, kind string) {
	if agentID == "" || accountID == "" {
		return
	}
	if _, unlocked := s.account.access(); !unlocked {
		return
	}
	now := time.Now()
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	if s.agentCLIs.closed {
		return
	}
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return
	}
	pool := pools[agentID]
	if pool == nil || !pool.Scheduling.AutoCooldown {
		return
	}
	a := pool.find(accountID)
	if a == nil {
		return
	}
	// Debounce duplicate triggers from the same burst within 20 seconds.
	if a.LastErrorAt > 0 && now.Unix()-a.LastErrorAt < 20 {
		return
	}
	applyAccountCooldown(a, kind, 0, now)
	s.setAgentNoticeLocked(agentID, fmt.Sprintf("终端检测到「%s」触发限流，已自动进入熔断冷却并避让", a.displayName()))
	s.scheduleAgentPoolsFlushLocked()
}

func (s *Server) refreshAgentAccount(ctx context.Context, spec agentcatalog.AgentSpec, in agentAccountMutation) error {
	if !s.vmRunning() {
		return errors.New("请先启动 Cage 环境后再检查账号状态")
	}
	if in.Action == "refresh_default" {
		_, _, err := s.inspectAgentDefault(ctx, spec, true)
		return err
	}
	if !validClaudeID(in.ID) {
		return errors.New("账号 ID 无效")
	}
	var guestID string
	s.agentCLIs.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	if err == nil {
		if a := s.ensureAgentPoolLocked(pools, spec.ID).find(in.ID); a != nil {
			guestID = a.guestID()
		} else {
			err = errors.New("未找到要检查的账号")
		}
	}
	s.agentCLIs.mu.Unlock()
	if err != nil {
		return err
	}
	snap, err := s.probeAgentProfile(ctx, spec, guestID)
	if err != nil {
		return err
	}
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err = s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	pool := s.ensureAgentPoolLocked(pools, spec.ID)
	if a := pool.find(in.ID); a != nil {
		now := time.Now()
		applySnapshotToAccount(a, snap, now)
		if snap.LoggedIn && a.CooldownUntil <= now.Unix() {
			a.ConsecutiveFailures = 0
			a.HealthEWMA = updateHealthEWMA(a.HealthEWMA, 1.0)
		}
	}
	return s.saveAgentPoolsLocked(pools)
}

func (s *Server) removeAgentAccount(ctx context.Context, spec agentcatalog.AgentSpec, id string) error {
	if !validClaudeID(id) {
		return errors.New("账号 ID 无效")
	}
	// Acquire s.op BEFORE checking active terminals so no new terminal can race admission.
	s.op.Lock()
	defer s.op.Unlock()
	active, logins := s.activeAgentTerminals(spec.ID)
	if active[id] > 0 || logins[id] {
		return errors.New("请先关闭仍在使用此账号的终端，再移除账号")
	}

	var source, authMode, guestID string
	var notFound bool
	var prevPools map[string]*agentAccountPool
	s.agentCLIs.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	if err == nil {
		pool := s.ensureAgentPoolLocked(pools, spec.ID)
		target := pool.find(id)
		if target == nil {
			notFound = true
		} else {
			source = target.Source
			authMode = target.AuthMode
			guestID = target.guestID()
			prevPools = cloneAgentPools(pools)
		}
	}
	s.agentCLIs.mu.Unlock()
	if err != nil {
		return err
	}
	if notFound {
		return nil
	}

	needClearGuest := (source == "api_key" || authMode == "api_key") && guestID != "default" && s.windowsGuest == nil
	if needClearGuest && !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
		return errors.New("请先启动 Cage 环境后再移除 API Key 账号，以确保客机内的隔离密钥被彻底清除")
	}

	// Persist roster deletion FIRST while holding s.op so a disk save failure leaves
	// guest credentials untouched; if guest credential wipe then fails, restore prevPools.
	s.agentCLIs.mu.Lock()
	nextPools := cloneAgentPools(prevPools)
	pool := s.ensureAgentPoolLocked(nextPools, spec.ID)
	pool.Accounts = slices.DeleteFunc(pool.Accounts, func(a agentAccount) bool { return a.ID == id })
	for ws, pin := range pool.Scheduling.ProjectPins {
		if pin == "account:"+id {
			delete(pool.Scheduling.ProjectPins, ws)
		}
	}
	err = s.saveAgentPoolsLocked(nextPools)
	s.agentCLIs.mu.Unlock()
	if err != nil {
		return err
	}

	if needClearGuest {
		if clearErr := s.runAgentRuntime(ctx, "clear-credentials", map[string]any{"spec": spec, "account_id": guestID}, nil); clearErr != nil {
			s.agentCLIs.mu.Lock()
			rbErr := s.saveAgentPoolsLocked(prevPools)
			s.agentCLIs.mu.Unlock()
			if rbErr != nil {
				return fmt.Errorf("清除客机中的隔离凭据失败且恢复账号列表失败: %v / %v", clearErr, rbErr)
			}
			return fmt.Errorf("清除客机中的隔离凭据失败，已取消移除以防密钥残留: %w", clearErr)
		}
	}
	if source == "default" {
		s.invalidateAgentDefaultCache(spec.ID)
	}
	return nil
}

func (s *Server) setAgentNoticeLocked(agentID, notice string) {
	if s.agentCLIs.loginNotice == nil {
		s.agentCLIs.loginNotice = map[string]string{}
	}
	s.agentCLIs.loginNotice[agentID] = notice
}

// selectAgentAccount resolves the account for a new Agent terminal in < 1ms
// from the in-memory pool cache + zero-alloc active terminal counts (zero SSH!).
func (s *Server) selectAgentAccount(spec agentcatalog.AgentSpec, requestedID string, login bool, workspace string) (agentSelection, scheduleDecision, error) {
	var empty agentSelection
	var decision scheduleDecision
	s.agentCLIs.mu.Lock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		s.agentCLIs.mu.Unlock()
		return empty, decision, err
	}
	pool := s.ensureAgentPoolLocked(pools, spec.ID)
	if login {
		if requestedID == "" {
			if pool.PendingLogin == "" {
				pool.PendingLogin = randomToken()[:24]
				err = s.saveAgentPoolsLocked(pools)
			}
			if err == nil {
				s.setAgentNoticeLocked(spec.ID, "")
				delete(s.agentCLIs.pendingAttempted, spec.ID)
			}
			requestedID = pool.PendingLogin
		} else if a := pool.find(requestedID); a == nil || a.Source == "default" {
			err = errors.New("请在普通终端管理默认环境的登录")
		}
		if err != nil {
			s.agentCLIs.mu.Unlock()
			return empty, decision, err
		}
		guestID := requestedID
		label := spec.ShortName + " 新账号登录"
		if a := pool.find(requestedID); a != nil {
			guestID = a.guestID()
			label = a.displayName()
		}
		s.agentCLIs.mu.Unlock()
		return agentSelection{AgentID: spec.ID, ID: requestedID, GuestID: guestID, Label: label, AuthMode: "oauth"}, decision, nil
	}

	if requestedID != "" && requestedID != "auto" {
		a := pool.find(requestedID)
		if a == nil {
			s.agentCLIs.mu.Unlock()
			return empty, decision, errors.New("未找到指定的账号")
		}
		sel := agentSelection{
			AgentID:  spec.ID,
			ID:       a.ID,
			GuestID:  a.guestID(),
			Label:    a.displayName(),
			AuthMode: a.AuthMode,
			Reason:   "手动指定账号",
		}
		s.agentCLIs.mu.Unlock()
		return sel, scheduleDecision{AccountID: a.ID, Label: a.displayName(), Strategy: "manual", Reason: "手动指定账号"}, nil
	}

	if len(pool.Accounts) == 0 {
		s.agentCLIs.mu.Unlock()
		return agentSelection{AgentID: spec.ID, GuestID: "default", Label: "默认环境", Reason: "尚未登记多账号，沿用默认环境"},
			scheduleDecision{Strategy: pool.Scheduling.normalized().Mode, Reason: "默认环境"}, nil
	}

	poolCopy := cloneAgentPool(pool)
	last := s.agentCLIs.lastPick[spec.ID]
	var rec claudeProjectPick
	if wsMap := s.agentCLIs.recent[spec.ID]; wsMap != nil {
		rec = wsMap[workspace]
	}
	s.agentCLIs.mu.Unlock()

	active, logins := s.activeAgentTerminals(spec.ID)
	decision = pickAgentAccount(poolCopy, active, logins, workspace, last, rec.id, rec.at, time.Now())
	if decision.AccountID == "" {
		return empty, decision, errors.New(decision.Reason)
	}
	a := poolCopy.find(decision.AccountID)
	return agentSelection{
		AgentID:  spec.ID,
		ID:       a.ID,
		GuestID:  a.guestID(),
		Label:    a.displayName(),
		AuthMode: a.AuthMode,
		Reason:   decision.Reason,
	}, decision, nil
}

// noteAgentSelection records the chosen account in memory (<500ns, zero sync disk I/O)
// and schedules a coalesced background persistence flush.
func (s *Server) noteAgentSelection(agentID, accountID, workspace string) {
	if agentID == "" || accountID == "" {
		return
	}
	now := time.Now()
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	if s.agentCLIs.lastPick == nil {
		s.agentCLIs.lastPick = map[string]string{}
	}
	s.agentCLIs.lastPick[agentID] = accountID
	if workspace != "" {
		if s.agentCLIs.recent == nil {
			s.agentCLIs.recent = map[string]map[string]claudeProjectPick{}
		}
		wsMap := s.agentCLIs.recent[agentID]
		if wsMap == nil {
			wsMap = map[string]claudeProjectPick{}
			s.agentCLIs.recent[agentID] = wsMap
		}
		wsMap[workspace] = claudeProjectPick{id: accountID, at: now}
		// Keep workspace affinity map strictly bounded.
		if len(wsMap) > maxRecentWorkspaces {
			cutoff := now.Add(-24 * time.Hour)
			for ws, pick := range wsMap {
				if pick.at.Before(cutoff) {
					delete(wsMap, ws)
				}
			}
			for len(wsMap) > maxRecentWorkspaces {
				oldestWS := ""
				var oldestAt time.Time
				for ws, pick := range wsMap {
					if oldestWS == "" || pick.at.Before(oldestAt) {
						oldestWS, oldestAt = ws, pick.at
					}
				}
				if oldestWS == "" {
					break
				}
				delete(wsMap, oldestWS)
			}
		}
	}
	if pools, err := s.loadAgentPoolsLocked(); err == nil {
		if a := s.ensureAgentPoolLocked(pools, agentID).find(accountID); a != nil {
			a.TotalAllocations++
			a.LastAllocatedAt = now.Unix()
			s.scheduleAgentPoolsFlushLocked()
		}
	}
}

var (
	agentConfirmJoinTimeout = 4 * time.Second
	errAgentConfirmOpBusy   = errors.New("operation lock busy")
)

func (s *Server) confirmAgentLogin(ctx context.Context, agentID, accountID string) error {
	return s.confirmAgentLoginWithLockMode(ctx, agentID, accountID, false)
}

func (s *Server) confirmAgentLoginWithLockMode(ctx context.Context, agentID, accountID string, tryOp bool) (retErr error) {
	if _, unlocked := s.account.access(); !unlocked {
		return errors.New("Cage 已锁定")
	}
	spec, _, ok := s.lookupAgentSpec(agentID)
	if !ok {
		return errors.New("unknown agent")
	}

	m := &s.agentCLIs
	key := spec.ID + ":" + accountID
	for {
		m.mu.Lock()
		if call := m.confirmInflight[key]; call != nil {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-call.done:
				if !tryOp && errors.Is(call.err, errAgentConfirmOpBusy) {
					continue
				}
				return call.err
			}
		}
		if m.confirmInflight == nil {
			m.confirmInflight = map[string]*agentConfirmCall{}
		}
		call := &agentConfirmCall{done: make(chan struct{})}
		m.confirmInflight[key] = call
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			if m.confirmInflight[key] == call {
				delete(m.confirmInflight, key)
			}
			call.err = retErr
			close(call.done)
			m.mu.Unlock()
		}()
		break
	}

	snap, err := s.probeAgentProfile(ctx, spec, accountID)
	if err != nil {
		// Surface the runtime/snapshot failure explicitly and keep PendingLogin
		// intact so a subsequent refresh or retry can still pick up the credentials.
		m.mu.Lock()
		if _, unlocked := s.account.access(); unlocked {
			if pools, loadErr := s.loadAgentPoolsLocked(); loadErr == nil {
				pool := s.ensureAgentPoolLocked(pools, spec.ID)
				if accountID == pool.PendingLogin || pool.find(accountID) != nil {
					reason := strings.TrimSpace(err.Error())
					if reason == "" {
						reason = "运行环境通信失败"
					}
					s.setAgentNoticeLocked(spec.ID, "无法读取登录凭据："+reason)
					if accountID == pool.PendingLogin {
						if m.pendingAttempted == nil {
							m.pendingAttempted = map[string]time.Time{}
						}
						m.pendingAttempted[spec.ID] = time.Now()
					}
				}
			}
		}
		m.mu.Unlock()
		return err
	}
	if tryOp {
		if s.busy() || !s.op.TryLock() {
			return errAgentConfirmOpBusy
		}
	} else {
		s.op.Lock()
	}
	defer s.op.Unlock()
	if _, unlocked := s.account.access(); !unlocked {
		return errors.New("Cage 已锁定")
	}
	s.agentCLIs.mu.Lock()
	defer s.agentCLIs.mu.Unlock()
	pools, err := s.loadAgentPoolsLocked()
	if err != nil {
		return err
	}
	pool := s.ensureAgentPoolLocked(pools, spec.ID)
	if accountID == pool.PendingLogin {
		if snap.LoggedIn {
			identity := snap.identityKey(spec.ID)
			var existing *agentAccount
			for i := range pool.Accounts {
				a := &pool.Accounts[i]
				sameEmail := a.Email != "" && snap.Email != "" &&
					strings.EqualFold(a.Email, snap.Email) &&
					strings.EqualFold(strings.TrimSpace(a.Organization), strings.TrimSpace(snap.Organization))
				sameUser := a.Username != "" && snap.Username != "" &&
					strings.EqualFold(a.Username, snap.Username) &&
					strings.EqualFold(strings.TrimSpace(a.Organization), strings.TrimSpace(snap.Organization))
				sameProviderOnly := a.Email == "" && snap.Email == "" && a.Username == "" && snap.Username == "" &&
					a.Provider != "" && snap.Provider != "" && strings.EqualFold(a.Provider, snap.Provider)
				sameKey := identity != "" && a.KeyFingerprint != "" && identity == "key:"+a.KeyFingerprint
				if sameEmail || sameUser || sameProviderOnly || sameKey {
					existing = a
					break
				}
			}
			if existing != nil {
				if existing.Source == "default" {
					existing.Source, existing.ProfileID = "managed", accountID
					applySnapshotToAccount(existing, snap, time.Now())
					clearAccountCooldown(existing)
					s.setAgentNoticeLocked(spec.ID, fmt.Sprintf("已为「%s」升级为独立隔离登录目录", existing.displayName()))
				} else {
					active, _ := s.activeAgentTerminals(spec.ID)
					if active[existing.ID] == 0 {
						existing.ProfileID = accountID
						applySnapshotToAccount(existing, snap, time.Now())
						clearAccountCooldown(existing)
						s.setAgentNoticeLocked(spec.ID, fmt.Sprintf("「%s」已在调度池中，已刷新其登录状态", existing.displayName()))
					} else {
						s.setAgentNoticeLocked(spec.ID, fmt.Sprintf("「%s」当前有活跃终端，未覆盖旧目录；请关闭旧终端后重新登录", existing.displayName()))
					}
				}
			} else {
				acc := agentAccount{
					ID:         accountID,
					Label:      snap.suggestedLabel(spec.ShortName + " 账号"),
					Source:     "managed",
					AuthMode:   "oauth",
					Enabled:    true,
					Status:     "ready",
					Weight:     10,
					HealthEWMA: 1.0,
				}
				applySnapshotToAccount(&acc, snap, time.Now())
				pool.Accounts = append(pool.Accounts, acc)
				s.setAgentNoticeLocked(spec.ID, fmt.Sprintf("已将「%s」加入 %s 智能调度池", acc.displayName(), spec.Name))
			}
			pool.PendingLogin = ""
			delete(m.pendingAttempted, spec.ID)
		} else {
			_, logins := s.activeAgentTerminals(spec.ID)
			if logins[accountID] {
				return nil
			}
			s.setAgentNoticeLocked(spec.ID, "登录终端已关闭，未检测到新登录凭据")
			pool.PendingLogin = ""
			delete(m.pendingAttempted, spec.ID)
		}
	} else if a := pool.find(accountID); a != nil {
		applySnapshotToAccount(a, snap, time.Now())
		if snap.LoggedIn {
			clearAccountCooldown(a)
			s.setAgentNoticeLocked(spec.ID, fmt.Sprintf("已重新确认「%s」的登录状态", a.displayName()))
		}
	}
	return s.saveAgentPoolsLocked(pools)
}

func agentPrepHash(spec agentcatalog.AgentSpec, accountID string) string {
	specJSON, _ := json.Marshal(spec)
	h := sha256.New()
	h.Write([]byte(agentRuntimeHash))
	h.Write([]byte{0})
	h.Write([]byte(accountID))
	h.Write([]byte{0})
	h.Write(specJSON)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// managedAgentTerminalScript builds the ultra-low-latency isolated guest shell
// script:
// - Uses a deterministic prep_hash stamp (.prepared-<hash>) so any spec or
//   runtime upgrade invalidates stale stamps/shims automatically while warm
//   opens skip Python completely (<1ms!).
// - Sources 0600 shlex-quoted profile.env inside a scoped subshell for the
//   agent command ONLY: absolute-path / npx invocations get full isolation dirs,
//   OAuth env unsets, and API keys with zero shell interpolation injection, while
//   the fallback interactive shell never holds raw API keys in its environment.
// - Supports both Linux (/bin/bash) and macOS (/bin/zsh + bundled python3).
func managedAgentTerminalScript(workspace string, spec agentcatalog.AgentSpec, launchCommand, accountID, loginMode string, login, macos bool) string {
	pythonBin := "python3"
	if macos {
		pythonBin = "/usr/local/libexec/cage-cli/bin/python3"
	}
	if login {
		payload, _ := json.Marshal(map[string]any{
			"spec":       spec,
			"account_id": accountID,
			"login_mode": loginMode,
		})
		return `cd "$HOME" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 CAGE_CLI_LOGIN_JSON=` +
			shellescape.Quote(string(payload)) + " " + agentRuntimeCommandWithPython(pythonBin, "login")
	}
	if accountID == "" {
		accountID = "default"
	}
	shellName := "bash"
	shellArgs := "-lic"
	fallbackExec := "exec bash -i"
	if macos {
		shellName = "/bin/zsh"
		shellArgs = "-lic"
		fallbackExec = "exec /bin/zsh -i"
	}
	if accountID == "default" {
		body := `export CAGE_CLI_ID=` + shellescape.Quote(spec.ID) + ` CAGE_CLI_ACCOUNT_ID='default'; ` +
			`export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"; ` +
			`eval "$CAGE_TERMINAL_COMMAND"; unset CAGE_TERMINAL_COMMAND; ` + fallbackExec
		return "cd " + shellescape.Quote(workspace) +
			" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 " +
			"CAGE_TERMINAL_COMMAND=" + shellescape.Quote(launchCommand) +
			" " + shellName + " " + shellArgs + " " + shellescape.Quote(body)
	}

	prepHash := agentPrepHash(spec, accountID)
	payload, _ := json.Marshal(map[string]any{
		"spec":       spec,
		"account_id": accountID,
		"prep_hash":  prepHash,
	})
	shimDir := fmt.Sprintf(`$HOME/.local/share/cage/agent-shims/%s`, spec.ID)
	shimBin := fmt.Sprintf(`%s/%s`, shimDir, spec.Binary)
	profileDir := fmt.Sprintf(`$HOME/.local/share/cage/agent-accounts/%s/%s`, spec.ID, accountID)
	stamp := fmt.Sprintf(`%s/.prepared-%s`, profileDir, prepHash)
	envFile := fmt.Sprintf(`%s/profile.env`, profileDir)
	prepCmd := `if [ ! -f "` + stamp + `" ] || [ ! -x "` + shimBin + `" ] || [ ! -r "` + envFile + `" ]; then ` +
		`printf '%s' "$CAGE_CLI_PREP_JSON" | ` + agentRuntimeCommandWithPython(pythonBin, "prepare-session") + ` >/dev/null || { echo "Agent 账号隔离环境准备失败，已中止启动以防凭据混用。" >&2; ` + fallbackExec + `; }; fi; `
	body := prepCmd +
		`export CAGE_CLI_ID=` + shellescape.Quote(spec.ID) + ` CAGE_CLI_ACCOUNT_ID=` + shellescape.Quote(accountID) + `; ` +
		`export PATH="` + shimDir + `:$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"; ` +
		`unset CAGE_CLI_PREP_JSON; ` +
		`( . "` + envFile + `" && eval "$CAGE_TERMINAL_COMMAND" ); unset CAGE_TERMINAL_COMMAND; ` + fallbackExec
	return "cd " + shellescape.Quote(workspace) +
		" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 " +
		"CAGE_CLI_PREP_JSON=" + shellescape.Quote(string(payload)) + " " +
		"CAGE_TERMINAL_COMMAND=" + shellescape.Quote(launchCommand) +
		" " + shellName + " " + shellArgs + " " + shellescape.Quote(body)
}
