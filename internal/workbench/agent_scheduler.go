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
	Mode                    string            `json:"mode"`
	SelectedAccountID       string            `json:"selected_account_id,omitempty"`
	SelectedAccountRevision string            `json:"selected_account_revision,omitempty"`
	ProjectAffinityMinutes  int               `json:"project_affinity_minutes"`
	AutoCooldown            bool              `json:"auto_cooldown"`
	ProjectPins             map[string]string `json:"project_pins,omitempty"` // workspace -> "account:<id>" or "group:<name>"
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
	return slices.Contains([]string{"manual", "exhaust", "smart", "balanced", "sequential", "sticky_project", "quota_first", "drain"}, p.Mode) &&
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
		if window.ObservedAt < 0 || window.ObservedAt > min(q.ObservedAt+60, now.Unix()+60) {
			return old
		}
		if old != nil {
			oldObserved := old.ObservedAt
			if oldObserved == 0 {
				oldObserved = next.ObservedAt
			}
			if oldObserved > window.ObservedAt {
				return old
			}
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
	// Keep legacy windows tied to their own observation when the other
	// window advances the overall quota timestamp.
	for _, window := range []**claudeWindow{&next.FiveHour, &next.SevenDay} {
		if *window != nil && (*window).ObservedAt == 0 {
			if next.ObservedAt <= 0 {
				*window = nil
				continue
			}
			reading := **window
			reading.ObservedAt = next.ObservedAt
			*window = &reading
		}
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
//   - Hard-blocks exhausted windows.
//   - Rewards soon-expiring short windows ("use-it-or-lose-it").
//   - Protects scarce 7-day weekly budgets by suppressing drain urgency and
//     applying a smooth conservation penalty when 7d headroom < 20%.
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
		obs := schedulingObservationTime(w.ObservedAt, a.Quota.ObservedAt, nowUnix)
		if obs == 0 {
			return -1, false, false, false, 0
		}
		fresh = nowUnix-obs <= int64(claudeQuotaFresh/time.Second)
		if w.UsedPercentage >= 100 && (w.ResetsAt > nowUnix || fresh) {
			return 0, fresh, true, true, max(0, w.ResetsAt-nowUnix)
		}
		rem = 100.0 - w.UsedPercentage
		if fresh && w.ResetsAt > nowUnix {
			timeLeft = w.ResetsAt - nowUnix
		}
		recent := nowUnix-obs <= int64(4*claudeQuotaFresh/time.Second)
		return rem, fresh, recent, false, timeLeft
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

func (p agentAccountPool) defaultAccount() *agentAccount {
	for i := range p.Accounts {
		if p.Accounts[i].Source == "default" {
			return &p.Accounts[i]
		}
	}
	return nil
}

// Guest usage can only disqualify accounts that are otherwise launchable.
func (p *agentAccountPool) hasLaunchCandidate(requestedID, workspace string, now time.Time) bool {
	if p == nil {
		return false
	}
	eligible := func(a *agentAccount) bool {
		return a != nil && agentAccountBlock(a, 0, false, a.analyzeQuota(now).eligible, now.Unix()) == ""
	}
	switch requestedID {
	case "default":
		return eligible(p.defaultAccount())
	case "", "auto":
	default:
		return eligible(p.find(requestedID))
	}
	pin := strings.TrimSpace(p.Scheduling.ProjectPins[workspace])
	if pin == "" && canonicalSchedulingMode(p.Scheduling.Mode) == "manual" {
		return eligible(p.find(p.Scheduling.SelectedAccountID))
	}
	for i := range p.Accounts {
		a := &p.Accounts[i]
		if (pin == "" || matchesAgentPin(a, pin)) && eligible(a) {
			return true
		}
	}
	return false
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

// pickAgentAccount returns the full explainable decision for previews and manifests.
func pickAgentAccount(
	pool agentAccountPool,
	active map[string]int,
	logins map[string]bool,
	workspace, lastPick, recentAccountID string,
	recentAt time.Time,
	now time.Time,
) scheduleDecision {
	decision, _ := chooseAgentAccount(pool, active, logins, workspace, lastPick, recentAccountID, recentAt, now, true)
	return decision
}

func chooseAgentAccount(
	pool agentAccountPool,
	active map[string]int,
	logins map[string]bool,
	workspace, lastPick, recentAccountID string,
	recentAt time.Time,
	now time.Time,
	explain bool,
) (scheduleDecision, int) {
	sched := pool.Scheduling.normalized()
	decision := scheduleDecision{Strategy: sched.Mode}
	pin := strings.TrimSpace(sched.ProjectPins[workspace])
	mode := canonicalSchedulingMode(sched.Mode)
	manual := mode == "manual" && pin == ""
	ordered := mode == "exhaust" || mode == "manual"
	if len(pool.Accounts) == 0 {
		switch {
		case pin != "":
			decision.Reason = "当前项目绑定的账号或分组暂无可用成员，请检查冷却、并发或登录状态"
		case manual:
			decision.Reason = manualSchedulingError("Agent").Error()
		case mode == "exhaust" && sched.SelectedAccountID != "":
			decision.Reason = "已选账号不在账号池内，请检查账号面板"
		default:
			decision.Reason = "尚未加入调度账号，将沿用默认环境登录"
		}
		return decision, -1
	}

	preferred := ""
	affinityTTL := time.Duration(sched.ProjectAffinityMinutes) * time.Minute
	if age := now.Sub(recentAt); affinityTTL > 0 && age >= 0 && age < affinityTTL && !recentAt.IsZero() {
		preferred = recentAccountID
	}
	start := 0
	switch {
	case ordered && pin == "":
		start = max(0, slices.IndexFunc(pool.Accounts, func(a agentAccount) bool { return a.ID == sched.SelectedAccountID }))
	case !ordered && lastPick != "":
		start = (slices.IndexFunc(pool.Accounts, func(a agentAccount) bool { return a.ID == lastPick }) + 1) % len(pool.Accounts)
	}
	if explain {
		decision.Candidates = make([]scheduleCandidate, len(pool.Accounts))
	}

	nowUnix := now.Unix()
	bestIdx, eligible := -1, 0
	var best rankedAgentAccount
	var sticky bool
	for offset := range pool.Accounts {
		idx := offset
		if ordered {
			idx = (start + offset) % len(pool.Accounts)
		}
		a := &pool.Accounts[idx]
		count := active[a.ID]
		quota := a.analyzeQuota(now)
		block := agentAccountBlock(a, count, logins[a.ID], quota.eligible, nowUnix)
		if block == "" && manual && a.ID != sched.SelectedAccountID {
			block = "手动调度已锁定其他账号"
		}
		if block == "" && pin != "" && !matchesAgentPin(a, pin) {
			block = "不属于当前项目绑定的账号或分组"
		}
		ranked := rankedAgentAccount{
			quota: quota, health: a.effectiveHealth(now), active: count, weight: a.effectiveWeight(),
		}
		if explain {
			decision.Candidates[idx] = scheduleCandidate{
				AccountID: a.ID, Label: a.displayName(), Eligible: block == "",
				ActiveSessions: count, EffectiveWeight: ranked.weight,
				Headroom: quota.headroom, HealthScore: math.Round(ranked.health * 100),
				IneligibleReason: agentAccountBlockReason(block, a, count, nowUnix),
			}
		}
		if block != "" {
			continue
		}
		eligible++
		distance := idx - start
		if distance < 0 {
			distance += len(pool.Accounts)
		}
		roundRobin := 0.5 * (1 - float64(distance)/float64(len(pool.Accounts)))
		isSticky := sched.Mode == "sticky_project" && a.ID == preferred
		switch {
		case isSticky:
			ranked.score = 120
		case ordered:
			ranked.score = 100
		default:
			ranked.scoreAccount(a, sched.Mode, a.ID == preferred, now, roundRobin)
		}
		if explain && (!ordered || bestIdx < 0) {
			candidate := &decision.Candidates[idx]
			candidate.Score = roundAgentScore(ranked.score)
			candidate.Reason = ranked.reason(sched.Mode, idx, isSticky)
		}
		if bestIdx < 0 || isSticky || (!sticky && !ordered && ranked.betterThan(best)) {
			bestIdx, best, sticky = idx, ranked, isSticky
			if !explain && (sticky || ordered) {
				break
			}
		}
	}

	if bestIdx < 0 {
		decision.Reason = "暂无可分配的健康账号，请检查账号状态、并发上限或冷却时间"
		if pin != "" {
			decision.Reason = "当前项目绑定的账号或分组暂无可用成员，请检查冷却、并发或登录状态"
		} else if manual {
			decision.Reason = manualSchedulingError("Agent").Error()
			if explain && sched.SelectedAccountID != "" {
				for _, candidate := range decision.Candidates {
					if candidate.AccountID == sched.SelectedAccountID && candidate.IneligibleReason != "" {
						decision.Reason += " · " + candidate.IneligibleReason
						break
					}
				}
			}
		}
		return decision, -1
	}
	if eligible == 1 && best.burstPenalty != 0 {
		best.score -= best.burstPenalty
		if explain {
			decision.Candidates[bestIdx].Score = roundAgentScore(best.score)
		}
	}
	if explain && sticky {
		for i := range decision.Candidates {
			if i != bestIdx {
				decision.Candidates[i].Score = 0
				decision.Candidates[i].Reason = ""
			}
		}
	}
	winner := &pool.Accounts[bestIdx]
	decision.AccountID, decision.Label = winner.ID, winner.displayName()
	decision.Score = roundAgentScore(best.score)
	if explain {
		decision.Reason = decision.Candidates[bestIdx].Reason
	} else {
		decision.Reason = best.reason(sched.Mode, bestIdx, sticky)
	}
	if sched.Mode == "sticky_project" && preferred != "" && !sticky {
		decision.Reason = "原项目账号暂不可用 · " + decision.Reason
	}
	return decision, bestIdx
}

// matchesAgentPin tests the configured ID/group and treats malformed legacy pins as unavailable.
func matchesAgentPin(a *agentAccount, pin string) bool {
	kind, value, ok := strings.Cut(pin, ":")
	if !ok || value == "" {
		return false
	}
	switch kind {
	case "account":
		return a.ID == value
	case "group":
		return a.HandoffGroup == value
	}
	return false
}

func agentAccountBlock(a *agentAccount, active int, login, quotaAvailable bool, now int64) string {
	switch {
	case a.ID == "":
		return "账号 ID 无效"
	case !a.Enabled:
		return "已暂停参与调度"
	case a.Status != "ready":
		return "not_ready"
	case login:
		return "正在执行交互登录"
	case a.CooldownUntil > now:
		return "cooldown"
	case !quotaAvailable:
		return "配额窗口已耗尽"
	case a.MaxConcurrency > 0 && active >= a.MaxConcurrency:
		return "capacity"
	}
	return ""
}

func agentAccountBlockReason(block string, a *agentAccount, active int, now int64) string {
	switch block {
	case "not_ready":
		return "账号未就绪（" + a.Status + "）"
	case "cooldown":
		return fmt.Sprintf("限流冷却中（剩余 %ds）", a.CooldownUntil-now)
	case "capacity":
		return fmt.Sprintf("已达并发上限（%d/%d）", active, a.MaxConcurrency)
	default:
		return block
	}
}

type rankedAgentAccount struct {
	quota        quotaTelemetry
	health       float64
	active       int
	weight       int
	score        float64
	reset        int64
	affinity     float64
	probation    float64
	burstPenalty float64
}

func (r *rankedAgentAccount) scoreAccount(a *agentAccount, mode string, preferred bool, now time.Time, roundRobin float64) {
	load := float64(r.active) / float64(r.weight)
	room := r.quota.headroom
	switch mode {
	case "balanced":
		if room < 0 {
			room = 65
		}
		if preferred {
			r.affinity = 4
		}
		r.score = 100/(1+load*10) + room*0.25 + r.health*8 - r.quota.longPenalty*0.5 + roundRobin + r.affinity
	case "quota_first":
		if room < 0 {
			room = 50
		}
		r.score = room + r.quota.resetUrgency*1.2 - r.quota.longPenalty + 25/(1+load*6) + r.health*6 + roundRobin
	case "drain":
		if room < 0 {
			room = 55
		}
		var urgency float64
		r.reset = math.MaxInt64
		if r.quota.soonestResetSec > 0 && r.quota.longPenalty == 0 {
			r.reset = r.quota.soonestResetSec
			urgency = 70 / (1 + float64(r.reset)/3600)
		}
		r.score = urgency + room*0.3 + 20/(1+load*4) - r.quota.longPenalty + r.health*8 + roundRobin
	default:
		if room < 0 {
			room = 65
		}
		if preferred && r.health >= 0.7 {
			r.affinity = 12
		}
		r.probation = probationPenalty(*a, now)
		if age := now.Unix() - a.LastAllocatedAt; a.LastAllocatedAt > 0 && age >= 0 && age < 15 {
			r.burstPenalty = -6
		}
		r.score = 100/(1+load*4) + room*0.65 + r.health*15 + r.quota.resetUrgency - r.quota.longPenalty + r.affinity + r.probation + r.burstPenalty + roundRobin
	}
}

func (r rankedAgentAccount) betterThan(other rankedAgentAccount) bool {
	return r.reset < other.reset || r.reset == other.reset && r.score > other.score
}

func roundAgentScore(score float64) float64 {
	return math.Round(score*100) / 100
}

func (r rankedAgentAccount) reason(mode string, index int, sticky bool) string {
	switch mode {
	case "manual":
		return fmt.Sprintf("手动调度 · 活跃 %d", r.active)
	case "exhaust":
		return fmt.Sprintf("用尽再切换 · 池内第 %d 个账号 · 活跃 %d", index+1, r.active)
	case "sequential":
		if index == 0 {
			return fmt.Sprintf("顺序优先 · 首选账号（活跃 %d）", r.active)
		}
		return fmt.Sprintf("顺序优先 · 前序不可用已自动故障转移至 #%d", index+1)
	case "sticky_project":
		if sticky {
			return fmt.Sprintf("项目亲和 · 沿用同项目近期账号（活跃 %d）", r.active)
		}
	case "balanced":
		return fmt.Sprintf("均衡负载 · 活跃 %d / 权重 %d", r.active, r.weight)
	case "quota_first":
		if r.quota.resetUrgency >= 2 {
			return fmt.Sprintf("余量与重置优先 · 剩余 %.0f%% · 临近重置", r.quota.headroom)
		}
		if r.quota.headroom >= 0 {
			return fmt.Sprintf("余量优先 · 剩余 %.0f%% · 活跃 %d", r.quota.headroom, r.active)
		}
		return fmt.Sprintf("余量优先 · 活跃 %d / 权重 %d", r.active, r.weight)
	case "drain":
		if r.reset < math.MaxInt64 {
			room := r.quota.headroom
			if room < 0 {
				room = 55
			}
			return fmt.Sprintf("即将重置优先 · 约 %d 分钟后重置 · 剩余 %.0f%%", (r.reset-1)/60+1, room)
		}
		return fmt.Sprintf("即将重置优先 · 活跃 %d / 权重 %d", r.active, r.weight)
	}
	reason := make([]byte, 0, 192)
	reason = append(reason, "智能调度"...)
	if r.affinity > 0 {
		reason = append(reason, " · 项目亲和"...)
	}
	if r.quota.resetUrgency >= 2 {
		reason = append(reason, " · 临近重置优先消耗"...)
	}
	if r.quota.headroom >= 0 {
		reason = fmt.Appendf(reason, " · 余量 %.0f%%", r.quota.headroom)
	}
	reason = fmt.Appendf(reason, " · 活跃 %d · 权重 %d", r.active, r.weight)
	if r.health < 0.85 {
		reason = fmt.Appendf(reason, " · 健康 %.0f%%", r.health*100)
	}
	if r.probation < -1 {
		reason = append(reason, " · 冷却恢复观察中"...)
	}
	return string(reason)
}
