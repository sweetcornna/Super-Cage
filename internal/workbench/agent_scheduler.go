package workbench

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// agentScheduling configures how new terminals for an agent select accounts.
type agentScheduling struct {
	Mode                   string            `json:"mode"` // smart, balanced, sequential, sticky_project, quota_first, drain
	ProjectAffinityMinutes int               `json:"project_affinity_minutes"`
	AutoCooldown           bool              `json:"auto_cooldown"`
	ProjectPins            map[string]string `json:"project_pins,omitempty"` // workspace -> "account:<id>" or "group:<name>"
}

func defaultAgentScheduling() agentScheduling {
	return agentScheduling{
		Mode:                   "smart",
		ProjectAffinityMinutes: 120,
		AutoCooldown:           true,
	}
}

func (p agentScheduling) normalized() agentScheduling {
	if p.Mode == "" {
		p.Mode = "smart"
	}
	if p.ProjectAffinityMinutes < 0 {
		p.ProjectAffinityMinutes = 0
	}
	if p.ProjectAffinityMinutes > 1440 {
		p.ProjectAffinityMinutes = 1440
	}
	return p
}

func (p agentScheduling) valid() bool {
	return slices.Contains([]string{"smart", "balanced", "sequential", "sticky_project", "quota_first", "drain"}, p.Mode) &&
		p.ProjectAffinityMinutes >= 0 && p.ProjectAffinityMinutes <= 1440
}

// agentAccount stores public scheduling, identity and health metadata for one
// isolated agent profile. Raw OAuth tokens and API keys never enter this struct.
type agentAccount struct {
	ID                  string       `json:"id"`
	Label               string       `json:"label"`
	Source              string       `json:"source"`    // managed, default, api_key
	AuthMode            string       `json:"auth_mode"` // oauth, api_key
	ProfileID           string       `json:"profile_id,omitempty"`
	Enabled             bool         `json:"enabled"`
	Status              string       `json:"status"` // ready, pending, signed_out, changed, duplicate
	Email               string       `json:"email,omitempty"`
	Username            string       `json:"username,omitempty"`
	Organization        string       `json:"organization,omitempty"`
	Plan                string       `json:"plan,omitempty"`
	Provider            string       `json:"provider,omitempty"`
	BaseURL             string       `json:"base_url,omitempty"`
	Model               string       `json:"model,omitempty"`
	KeyHint             string       `json:"key_hint,omitempty"`
	KeyFingerprint      string       `json:"key_fingerprint,omitempty"`
	EnvVar              string       `json:"env_var,omitempty"`
	ExpiresAt           int64        `json:"expires_at,omitempty"`
	Weight              int          `json:"weight"`
	MaxConcurrency      int          `json:"max_concurrency"`
	HandoffGroup        string       `json:"handoff_group,omitempty"`
	CooldownUntil       int64        `json:"cooldown_until,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures,omitempty"`
	HealthEWMA          float64      `json:"health_ewma,omitempty"`
	LastErrorKind       string       `json:"last_error_kind,omitempty"`
	LastErrorAt         int64        `json:"last_error_at,omitempty"`
	TotalAllocations    int64        `json:"total_allocations,omitempty"`
	LastAllocatedAt     int64        `json:"last_allocated_at,omitempty"`
	Quota               *claudeQuota `json:"quota,omitempty"`
	CheckedAt           int64        `json:"checked_at,omitempty"`
}

func (a agentAccount) guestID() string {
	if a.Source == "default" {
		return "default"
	}
	if a.ProfileID != "" {
		return a.ProfileID
	}
	return a.ID
}

func (a agentAccount) effectiveWeight() int {
	if a.Weight <= 0 {
		return 10
	}
	if a.Weight > 100 {
		return 100
	}
	return a.Weight
}

func (a agentAccount) effectiveHealth(now time.Time) float64 {
	h := a.HealthEWMA
	if h <= 0 || math.IsNaN(h) {
		h = 1.0
	}
	if h > 1.0 {
		h = 1.0
	}
	// Naturally recover health toward 1.0 after cooldown has elapsed so a
	// transient rate limit hours ago never permanently starves an account.
	if a.LastErrorAt > 0 && a.CooldownUntil <= now.Unix() {
		elapsed := now.Unix() - max(a.LastErrorAt, a.CooldownUntil)
		if elapsed > 0 {
			// Half-life recovery ~15 minutes.
			decay := 1.0 - math.Exp(-float64(elapsed)/900.0)
			h = h + (1.0-h)*decay
		}
	}
	if h < 0.05 {
		return 0.05
	}
	if h > 1.0 {
		return 1.0
	}
	return h
}

func updateHealthEWMA(current, outcome float64) float64 {
	if current <= 0 || math.IsNaN(current) {
		current = 1.0
	}
	const alpha = 0.28
	next := (1.0-alpha)*current + alpha*outcome
	if next < 0.05 {
		return 0.05
	}
	if next > 1.0 {
		return 1.0
	}
	return math.Round(next*1000) / 1000
}

func (a agentAccount) displayName() string {
	for _, s := range []string{a.Label, a.Email, a.Username, a.KeyHint, a.ID} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return "账号"
}

func (a *agentAccount) mergeQuota(q *claudeQuota, now time.Time) bool {
	if q == nil || q.ObservedAt <= 0 || q.ObservedAt > now.Unix()+60 {
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
		if window.ObservedAt < 0 || window.ObservedAt > q.ObservedAt+60 || (old != nil && old.ObservedAt > window.ObservedAt) {
			return old
		}
		if old != nil && window == *old {
			return old
		}
		return &window
	}
	next.FiveHour = merge(next.FiveHour, q.FiveHour)
	next.SevenDay = merge(next.SevenDay, q.SevenDay)
	if (next.FiveHour == nil && next.SevenDay == nil) || (a.Quota != nil && next == *a.Quota) {
		return false
	}
	next.ObservedAt = max(next.ObservedAt, q.ObservedAt)
	a.Quota = &next
	return true
}

type quotaTelemetry struct {
	headroom        float64 // -1 if unknown
	eligible        bool
	resetUrgency    float64 // [0, 14] short-window use-it-or-lose-it bonus
	longPenalty     float64 // [0, 22] weekly/7d conservation penalty
	soonestResetSec int64   // 0 if unknown
}

// analyzeQuota evaluates 5h and 7d windows together:
// - Hard-blocks exhausted windows.
// - Rewards soon-expiring short windows ("use-it-or-lose-it").
// - Protects scarce 7-day weekly budgets by suppressing drain urgency and
//   applying a smooth conservation penalty when 7d headroom < 20%.
func (a agentAccount) analyzeQuota(now time.Time) quotaTelemetry {
	out := quotaTelemetry{headroom: -1, eligible: true}
	if a.Quota == nil {
		return out
	}
	nowUnix := now.Unix()
	evalWindow := func(w *claudeWindow) (rem float64, fresh, active, exhausted bool, timeLeft int64) {
		if !validClaudeWindow(w) {
			return -1, false, false, false, 0
		}
		if w.ResetsAt > 0 && w.ResetsAt <= nowUnix {
			return -1, false, false, false, 0
		}
		obs := w.ObservedAt
		if obs <= 0 {
			obs = a.Quota.ObservedAt
		}
		fresh = obs > 0 && now.Sub(time.Unix(obs, 0)) <= claudeQuotaFresh
		if w.UsedPercentage >= 100 && (w.ResetsAt > nowUnix || fresh) {
			return 0, fresh, true, true, max(0, w.ResetsAt-nowUnix)
		}
		rem = 100.0 - w.UsedPercentage
		if w.ResetsAt > nowUnix {
			timeLeft = w.ResetsAt - nowUnix
		}
		return rem, fresh, true, false, timeLeft
	}

	rem5, fresh5, active5, ex5, left5 := evalWindow(a.Quota.FiveHour)
	rem7, fresh7, active7, ex7, left7 := evalWindow(a.Quota.SevenDay)
	if ex5 || ex7 {
		return quotaTelemetry{headroom: 0, eligible: false}
	}

	if active5 && fresh5 {
		out.headroom = rem5
	}
	if active7 && fresh7 && (out.headroom < 0 || rem7 < out.headroom) {
		out.headroom = rem7
	}
	// Fall back gently to slightly older telemetry blended toward 55% if no fresh reading.
	if out.headroom < 0 {
		if active5 {
			out.headroom = math.Round((rem5*0.5+55.0*0.5)*10) / 10
		}
		if active7 {
			blended := math.Round((rem7*0.5+55.0*0.5)*10) / 10
			if out.headroom < 0 || blended < out.headroom {
				out.headroom = blended
			}
		}
	}

	if left5 > 0 {
		out.soonestResetSec = left5
	}
	if left7 > 0 && (out.soonestResetSec == 0 || left7 < out.soonestResetSec) {
		out.soonestResetSec = left7
	}

	// Short-window reset urgency bonus (within 75 minutes of reset with >= 12% left).
	if active5 && fresh5 && rem5 >= 12 && left5 > 0 && left5 <= 4500 {
		out.resetUrgency = (1.0 - float64(left5)/4500.0) * (rem5 / 100.0) * 14.0
	}

	// Long-window (7d) conservation: protect scarce weekly quota from being drained.
	if active7 && fresh7 && rem7 < 20 {
		out.longPenalty = (20.0 - rem7) * 1.1
		if rem7 < 15 {
			out.resetUrgency = 0
		}
	}
	return out
}

// headroom returns remaining % (-1 if unknown) and eligibility.
func (a agentAccount) headroom(now time.Time) (float64, bool) {
	q := a.analyzeQuota(now)
	return q.headroom, q.eligible
}

// resetUrgencyBonus rewards accounts whose unexpired quota resets soon so remaining
// subscription allowance is utilized before expiring ("use-it-or-lose-it").
func (a agentAccount) resetUrgencyBonus(now time.Time) float64 {
	return a.analyzeQuota(now).resetUrgency
}

type agentAccountPool struct {
	Accounts     []agentAccount  `json:"accounts"`
	PendingLogin string          `json:"pending_login,omitempty"`
	Scheduling   agentScheduling `json:"scheduling"`
}

func cloneAgentPool(p *agentAccountPool) agentAccountPool {
	if p == nil {
		return agentAccountPool{Accounts: []agentAccount{}, Scheduling: defaultAgentScheduling()}
	}
	cp := *p
	cp.Accounts = make([]agentAccount, len(p.Accounts))
	for i, a := range p.Accounts {
		if a.Quota != nil {
			q := *a.Quota
			if q.FiveHour != nil {
				w := *q.FiveHour
				q.FiveHour = &w
			}
			if q.SevenDay != nil {
				w := *q.SevenDay
				q.SevenDay = &w
			}
			a.Quota = &q
		}
		cp.Accounts[i] = a
	}
	cp.Scheduling = p.Scheduling.normalized()
	if p.Scheduling.ProjectPins != nil {
		cp.Scheduling.ProjectPins = maps.Clone(p.Scheduling.ProjectPins)
	}
	return cp
}

func (p agentAccountPool) find(id string) *agentAccount {
	for i := range p.Accounts {
		if p.Accounts[i].ID == id {
			return &p.Accounts[i]
		}
	}
	return nil
}

type scheduleCandidate struct {
	AccountID        string  `json:"account_id"`
	Label            string  `json:"label"`
	Eligible         bool    `json:"eligible"`
	Score            float64 `json:"score"`
	ActiveSessions   int     `json:"active_sessions"`
	EffectiveWeight  int     `json:"effective_weight"`
	Headroom         float64 `json:"headroom"` // -1 means unknown
	HealthScore      float64 `json:"health_score,omitempty"`
	Reason           string  `json:"reason,omitempty"`
	IneligibleReason string  `json:"ineligible_reason,omitempty"`
}

type scheduleDecision struct {
	AccountID  string              `json:"account_id,omitempty"`
	Label      string              `json:"label,omitempty"`
	Strategy   string              `json:"strategy"`
	Score      float64             `json:"score,omitempty"`
	Reason     string              `json:"reason"`
	Candidates []scheduleCandidate `json:"candidates,omitempty"`
}

// applyAccountCooldown puts an account into circuit-breaker cooldown with bounded exponential backoff.
func applyAccountCooldown(a *agentAccount, kind string, durationSeconds int64, now time.Time) {
	a.ConsecutiveFailures++
	if kind == "" {
		kind = "rate_limit"
	}
	a.LastErrorKind = kind
	a.LastErrorAt = now.Unix()
	outcome := 0.35
	if kind == "auth_error" || kind == "quota_exhausted" {
		outcome = 0.0
	}
	a.HealthEWMA = updateHealthEWMA(a.HealthEWMA, outcome)
	if durationSeconds <= 0 {
		// Exponential backoff: 60s -> 180s -> 600s -> 1800s.
		backoffs := []int64{60, 180, 600, 1800}
		idx := min(a.ConsecutiveFailures-1, len(backoffs)-1)
		if idx < 0 {
			idx = 0
		}
		durationSeconds = backoffs[idx]
	}
	if durationSeconds > 86400 {
		durationSeconds = 86400
	}
	until := now.Unix() + durationSeconds
	if until > a.CooldownUntil {
		a.CooldownUntil = until
	}
}

func clearAccountCooldown(a *agentAccount) {
	a.CooldownUntil = 0
	a.ConsecutiveFailures = 0
	a.LastErrorKind = ""
	a.HealthEWMA = updateHealthEWMA(a.HealthEWMA, 1.0)
}

// probationPenalty applies a smoothly decaying half-open probation penalty
// right after cooldown ends (decays to 0 over 15 minutes so recovered accounts
// are never permanently penalized).
func probationPenalty(a agentAccount, now time.Time) float64 {
	if a.ConsecutiveFailures <= 0 {
		return 0
	}
	base := 15.0 * float64(min(3, a.ConsecutiveFailures))
	ref := max(a.CooldownUntil, a.LastErrorAt)
	if ref <= 0 || now.Unix() <= ref {
		return -base
	}
	elapsed := float64(now.Unix() - ref)
	if elapsed >= 900 {
		return 0
	}
	return -base * (1.0 - elapsed/900.0)
}

// pickAgentAccount runs the O(N) deterministic multi-factor scheduler and returns
// both the winning account and a full explainable decision trace.
func pickAgentAccount(
	pool agentAccountPool,
	active map[string]int,
	logins map[string]bool,
	workspace, lastPick, recentAccountID string,
	recentAt time.Time,
	now time.Time,
) scheduleDecision {
	sched := pool.Scheduling.normalized()
	decision := scheduleDecision{Strategy: sched.Mode}
	if len(pool.Accounts) == 0 {
		decision.Reason = "尚未加入调度账号，将沿用默认环境登录"
		return decision
	}

	pin := ""
	if workspace != "" && len(sched.ProjectPins) > 0 {
		pin = strings.TrimSpace(sched.ProjectPins[workspace])
	}
	affinityTTL := time.Duration(sched.ProjectAffinityMinutes) * time.Minute
	preferred := ""
	if recentAccountID != "" && affinityTTL > 0 && !recentAt.IsZero() && now.Sub(recentAt) < affinityTTL {
		preferred = recentAccountID
	}

	start := slices.IndexFunc(pool.Accounts, func(a agentAccount) bool { return a.ID == lastPick }) + 1
	if start >= len(pool.Accounts) {
		start = 0
	}

	candidates := make([]scheduleCandidate, 0, len(pool.Accounts))
	telemetries := make([]quotaTelemetry, len(pool.Accounts))
	var eligible []int

	for idx, a := range pool.Accounts {
		weight := a.effectiveWeight()
		act := active[a.ID]
		qt := a.analyzeQuota(now)
		telemetries[idx] = qt
		health := a.effectiveHealth(now)
		cand := scheduleCandidate{
			AccountID:       a.ID,
			Label:           a.displayName(),
			ActiveSessions:  act,
			EffectiveWeight: weight,
			Headroom:        qt.headroom,
			HealthScore:     math.Round(health * 100),
		}
		switch {
		case !a.Enabled:
			cand.IneligibleReason = "已暂停参与调度"
		case a.Status != "ready":
			cand.IneligibleReason = "账号未就绪（" + a.Status + "）"
		case logins[a.ID]:
			cand.IneligibleReason = "正在执行交互登录"
		case a.CooldownUntil > now.Unix():
			rem := a.CooldownUntil - now.Unix()
			cand.IneligibleReason = fmt.Sprintf("限流冷却中（剩余 %ds）", rem)
		case !qt.eligible:
			cand.IneligibleReason = "配额窗口已耗尽"
		case a.MaxConcurrency > 0 && act >= a.MaxConcurrency:
			cand.IneligibleReason = fmt.Sprintf("已达并发上限（%d/%d）", act, a.MaxConcurrency)
		case strings.HasPrefix(pin, "account:") && a.ID != strings.TrimPrefix(pin, "account:"):
			cand.IneligibleReason = "当前项目已绑定其他账号"
		case strings.HasPrefix(pin, "group:") && a.HandoffGroup != strings.TrimPrefix(pin, "group:"):
			cand.IneligibleReason = "不属于当前项目绑定的账号分组"
		default:
			cand.Eligible = true
			eligible = append(eligible, idx)
		}
		candidates = append(candidates, cand)
	}

	decision.Candidates = candidates
	if len(eligible) == 0 {
		if pin != "" {
			decision.Reason = "当前项目绑定的账号或分组暂无可用成员，请检查冷却、并发或登录状态"
		} else {
			decision.Reason = "暂无可分配的健康账号，请检查账号状态、并发上限或冷却时间"
		}
		return decision
	}

	rrBonus := func(idx int) float64 {
		dist := (idx - start + len(pool.Accounts)) % len(pool.Accounts)
		return 0.5 * (1.0 - float64(dist)/float64(max(1, len(pool.Accounts))))
	}

	bestIdx := -1
	bestScore := math.Inf(-1)
	bestReason := ""

	switch sched.Mode {
	case "sequential":
		bestIdx = eligible[0]
		a := pool.Accounts[bestIdx]
		bestScore = 100
		if bestIdx == 0 {
			bestReason = fmt.Sprintf("顺序优先 · 首选账号（活跃 %d）", active[a.ID])
		} else {
			bestReason = fmt.Sprintf("顺序优先 · 前序不可用已自动故障转移至 #%d", bestIdx+1)
		}
		candidates[bestIdx].Score = bestScore
		candidates[bestIdx].Reason = bestReason

	case "sticky_project":
		if preferred != "" {
			for _, idx := range eligible {
				if pool.Accounts[idx].ID == preferred {
					bestIdx = idx
					bestScore = 120
					bestReason = fmt.Sprintf("项目亲和 · 沿用同项目近期账号（活跃 %d）", active[pool.Accounts[idx].ID])
					candidates[idx].Score = bestScore
					candidates[idx].Reason = bestReason
					break
				}
			}
		}
		if bestIdx < 0 {
			bestIdx, bestScore, bestReason = scoreSmartCandidates(pool.Accounts, eligible, candidates, telemetries, preferred, now, rrBonus)
			if preferred != "" {
				bestReason = "原项目账号暂不可用 · " + bestReason
			}
		}

	case "balanced":
		for _, idx := range eligible {
			a := pool.Accounts[idx]
			c := &candidates[idx]
			qt := telemetries[idx]
			loadRatio := float64(c.ActiveSessions) / float64(c.EffectiveWeight)
			effRoom := c.Headroom
			if effRoom < 0 {
				effRoom = 65
			}
			score := 100.0/(1.0+loadRatio*10.0) + effRoom*0.25 + a.effectiveHealth(now)*8.0 - qt.longPenalty*0.5 + rrBonus(idx)
			if a.ID == preferred {
				score += 4.0
			}
			c.Score = math.Round(score*100) / 100
			c.Reason = fmt.Sprintf("均衡负载 · 活跃 %d / 权重 %d", c.ActiveSessions, c.EffectiveWeight)
			if score > bestScore {
				bestScore, bestIdx, bestReason = score, idx, c.Reason
			}
		}

	case "quota_first":
		for _, idx := range eligible {
			a := pool.Accounts[idx]
			c := &candidates[idx]
			qt := telemetries[idx]
			effRoom := c.Headroom
			if effRoom < 0 {
				effRoom = 50
			}
			loadRatio := float64(c.ActiveSessions) / float64(c.EffectiveWeight)
			score := effRoom + qt.resetUrgency*1.2 - qt.longPenalty + 25.0/(1.0+loadRatio*6.0) + a.effectiveHealth(now)*6.0 + rrBonus(idx)
			c.Score = math.Round(score*100) / 100
			if qt.resetUrgency >= 2.0 {
				c.Reason = fmt.Sprintf("余量与重置优先 · 剩余 %.0f%% · 临近重置", c.Headroom)
			} else if c.Headroom >= 0 {
				c.Reason = fmt.Sprintf("余量优先 · 剩余 %.0f%% · 活跃 %d", c.Headroom, c.ActiveSessions)
			} else {
				c.Reason = fmt.Sprintf("余量优先 · 活跃 %d / 权重 %d", c.ActiveSessions, c.EffectiveWeight)
			}
			if score > bestScore {
				bestScore, bestIdx, bestReason = score, idx, c.Reason
			}
		}

	case "drain":
		bestSoonest := int64(math.MaxInt64)
		for _, idx := range eligible {
			a := pool.Accounts[idx]
			c := &candidates[idx]
			qt := telemetries[idx]
			effRoom := c.Headroom
			if effRoom < 0 {
				effRoom = 55
			}
			loadRatio := float64(c.ActiveSessions) / float64(c.EffectiveWeight)
			urgency := 0.0
			if qt.soonestResetSec > 0 && qt.longPenalty == 0 {
				urgency = 70.0 / (1.0 + float64(qt.soonestResetSec)/3600.0)
			}
			score := urgency + effRoom*0.3 + 20.0/(1.0+loadRatio*4.0) - qt.longPenalty + a.effectiveHealth(now)*8.0 + rrBonus(idx)
			c.Score = math.Round(score*100) / 100
			if qt.soonestResetSec > 0 && qt.longPenalty == 0 {
				c.Reason = fmt.Sprintf("即将重置优先 · 约 %d 分钟后重置 · 剩余 %.0f%%", max(1, (qt.soonestResetSec+59)/60), effRoom)
			} else {
				c.Reason = fmt.Sprintf("即将重置优先 · 活跃 %d / 权重 %d", c.ActiveSessions, c.EffectiveWeight)
			}
			soonest := qt.soonestResetSec
			if soonest <= 0 || qt.longPenalty > 0 {
				soonest = math.MaxInt64 / 2
			}
			if bestIdx < 0 || (soonest < bestSoonest && qt.longPenalty == 0) || (soonest == bestSoonest && score > bestScore) {
				bestIdx, bestScore, bestReason, bestSoonest = idx, score, c.Reason, soonest
			}
		}

	default: // "smart"
		bestIdx, bestScore, bestReason = scoreSmartCandidates(pool.Accounts, eligible, candidates, telemetries, preferred, now, rrBonus)
	}

	winner := pool.Accounts[bestIdx]
	decision.AccountID = winner.ID
	decision.Label = winner.displayName()
	decision.Score = math.Round(bestScore*100) / 100
	decision.Reason = bestReason
	return decision
}

func scoreSmartCandidates(
	accounts []agentAccount,
	eligible []int,
	candidates []scheduleCandidate,
	telemetries []quotaTelemetry,
	preferred string,
	now time.Time,
	rrBonus func(int) float64,
) (int, float64, string) {
	bestIdx := eligible[0]
	bestScore := math.Inf(-1)
	bestReason := ""
	for _, idx := range eligible {
		a := accounts[idx]
		c := &candidates[idx]
		qt := telemetries[idx]
		loadRatio := float64(c.ActiveSessions) / float64(c.EffectiveWeight)
		concurrencyScore := 100.0 / (1.0 + loadRatio*4.0)
		effRoom := c.Headroom
		if effRoom < 0 {
			effRoom = 65.0
		}
		headroomScore := effRoom * 0.65
		health := a.effectiveHealth(now)
		healthScore := health * 15.0
		urgency := qt.resetUrgency
		affinity := 0.0
		if a.ID == preferred && health >= 0.7 && (a.MaxConcurrency == 0 || c.ActiveSessions < a.MaxConcurrency) {
			affinity = 12.0
		}
		probation := probationPenalty(a, now)
		burstPenalty := 0.0
		if a.LastAllocatedAt > 0 && now.Unix()-a.LastAllocatedAt < 15 && len(eligible) > 1 {
			burstPenalty = -6.0
		}
		score := concurrencyScore + headroomScore + healthScore + urgency - qt.longPenalty + affinity + probation + burstPenalty + rrBonus(idx)
		c.Score = math.Round(score*100) / 100

		var parts []string
		parts = append(parts, "智能调度")
		if affinity > 0 {
			parts = append(parts, "项目亲和")
		}
		if urgency >= 2.0 {
			parts = append(parts, "临近重置优先消耗")
		}
		if c.Headroom >= 0 {
			parts = append(parts, fmt.Sprintf("余量 %.0f%%", c.Headroom))
		}
		parts = append(parts, fmt.Sprintf("活跃 %d · 权重 %d", c.ActiveSessions, c.EffectiveWeight))
		if health < 0.85 {
			parts = append(parts, fmt.Sprintf("健康 %.0f%%", health*100))
		}
		if probation < -1.0 {
			parts = append(parts, "冷却恢复观察中")
		}
		c.Reason = strings.Join(parts, " · ")

		if score > bestScore {
			bestScore, bestIdx, bestReason = score, idx, c.Reason
		}
	}
	return bestIdx, bestScore, bestReason
}
