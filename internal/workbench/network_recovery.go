package workbench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

type networkRecovery struct {
	RunID, Configuration string
	PauseVersion         uint64
	Epoch                uint64
	Next                 time.Time
	Attempts             int
	pressureLogged       bool
	// pressureSince is when memory pressure first postponed this attempt
	// after it was due. The attempt runs anyway recoveryPressureWait later.
	pressureSince time.Time
}

// recoveryPressureWait bounds how long memory pressure postpones a due
// automatic recheck. A guest that stays short of memory still gets its full
// check (the same one a user runs by hand), and a failed attempt backs off
// before pressure can postpone the next one again.
var recoveryPressureWait = 60 * time.Second

// networkRecoveryCheck is the full check an automatic recovery runs; tests
// replace it so no guest is needed.
var networkRecoveryCheck = func(s *Server) error { return s.runSafetyCheck() }

// pendingRecoveryLocked reports whether s.recovery still describes the next
// transition of the current gate. Called with s.mu held.
func (s *Server) pendingRecoveryLocked() (networkRecovery, bool) {
	r := s.recovery
	if r.RunID == "" || s.networkPaused || s.closing || s.safety.pauseVersion != r.PauseVersion || s.safety.Gate == nil || s.safety.Gate.RunID != r.RunID || s.safety.Gate.Configuration != r.Configuration || s.safety.Gate.Mode == cage.GateOpen {
		return r, false
	}
	if s.safety.Gate.Mode == cage.GateClosed && s.safety.Gate.Epoch != r.Epoch {
		return r, false // A newer closure must not promise this recovery will run.
	}
	job := s.jobs.snapshot()
	if s.safety.Gate.Mode != cage.GateClosed && (!job.Running || job.Kind != "network-recovery") {
		return r, false // A manual check owns the next transition.
	}
	return r, true
}

// Called with s.mu held. Reuse the ordinary network status text on every
// platform; no independent timer or cached permission is introduced.
func (s *Server) networkRecoveryMessageLocked(now time.Time) string {
	r, pending := s.pendingRecoveryLocked()
	if !pending {
		return ""
	}
	job := s.jobs.snapshot()
	if job.Running {
		if job.Kind == "network-recovery" {
			return "正在执行自动完整重检，通过后恢复联网"
		}
		return "自动重检正在等待当前操作完成"
	}
	if !s.ready {
		return "自动重检正在等待环境就绪"
	}
	if now.Before(r.Next) {
		seconds := (r.Next.Sub(now) + time.Second - 1) / time.Second
		return fmt.Sprintf("自动重检将在 %d 秒后尝试", seconds)
	}
	if !r.pressureSince.IsZero() {
		if left := r.pressureSince.Add(recoveryPressureWait).Sub(now); left > 0 {
			return fmt.Sprintf("环境内存不足，自动重检最多再等待 %d 秒，之后仍会进行", (left+time.Second-1)/time.Second)
		}
		return "环境内存仍不足，自动重检即将进行"
	}
	if r.pressureLogged {
		return "自动重检正在等待内存压力缓解"
	}
	return "自动重检正在等待后台调度"
}

func (s *Server) scheduleNetworkRecovery(state cage.GateStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.networkPaused || s.closing || state.Mode != cage.GateClosed || state.Report == nil {
		return
	}
	s.recovery = networkRecovery{RunID: state.RunID, Configuration: state.Configuration, Epoch: state.Epoch, PauseVersion: s.safety.pauseVersion, Next: time.Now().Add(15 * time.Second)}
	s.refreshNetworkExplanationLocked(time.Now())
	diagnostics.Log("warn", "network", "temporary monitoring failure; full recheck scheduled: "+state.Reason)
}

func recoveryMatches(r networkRecovery, state cage.GateStatus, configuration string, paused bool, generation uint64) bool {
	return r.RunID != "" && !paused && generation == r.PauseVersion && state.RunID == r.RunID && state.Configuration == r.Configuration && configuration == r.Configuration && state.Epoch == r.Epoch && state.Mode == cage.GateClosed
}

// Called with s.op held. Admission uses the ordinary job queue and never changes
// saved network intent. The worker rechecks after acquiring s.op as well.
func (s *Server) recoverNetwork() {
	s.mu.Lock()
	r := s.recovery
	closing, paused, ready, jobRunning := s.closing, s.networkPaused, s.ready, s.jobs.snapshot().Running
	s.mu.Unlock()
	if r.RunID == "" {
		return
	}
	timing := s.startMonitorTiming()
	defer s.recordMonitorTiming("recovery", timing)
	// Read memory before any early return: the status explains it while the
	// recheck waits for any reason.
	resources, err := cage.ReadResourceStatus(s.instanceDir())
	now := time.Now()
	memory := memoryNote(resources, err, now, s.memorySizing(resources))
	s.mu.Lock()
	s.safety.memory = memory
	s.refreshNetworkExplanationLocked(now)
	s.mu.Unlock()
	switch {
	case closing || paused:
		timing.result = "cancelled"
		return
	case !ready:
		timing.result = "environment_not_ready"
		return
	case jobRunning:
		timing.result = "job_running"
		return
	case now.Before(r.Next):
		timing.result = "retry_not_due"
		return
	}
	// The resource manager refreshes UpdatedAt even when a sample fails.
	// Only a recent pressure measurement should postpone the full gate check,
	// and only for recoveryPressureWait: a guest that stays short of memory
	// would otherwise never get its network back.
	// With the guest's governor, pressure is the protected classes'; a
	// browser it holds at its limit (browserHeld) fails the full check's
	// browser stage the same way and waits within the same bound.
	fresh := now.Sub(resources.ObservedAt) <= 10*time.Second
	guestPressure := resources.Pressure && fresh
	browserPressure := browserHeld(resources) && fresh
	if err == nil && (guestPressure || browserPressure || resources.HostPressure) {
		s.mu.Lock()
		current := s.recovery == r
		logWait := current && r.pressureSince.IsZero()
		if logWait {
			s.recovery.pressureLogged = true
			s.recovery.pressureSince = now
			s.refreshNetworkExplanationLocked(now)
		}
		// The job must carry the recovery exactly as stored, or it is dropped.
		r = s.recovery
		s.mu.Unlock()
		if !current {
			timing.result = "superseded"
			return
		}
		if logWait {
			diagnostics.Log("info", "network", fmt.Sprintf("automatic recheck waiting for memory pressure to subside (guest=%t, browser=%t, host=%t); runs anyway after %s", guestPressure, browserPressure, resources.HostPressure, recoveryPressureWait))
		}
		if waited := now.Sub(r.pressureSince); waited < recoveryPressureWait {
			timing.result = "memory_pressure"
			return
		}
		timing.stages = append(timing.stages, "pressure_wait_elapsed=true")
		diagnostics.Log("warn", "network", fmt.Sprintf("memory pressure persisted for %s; running the automatic recheck anyway (guest=%t, browser=%t, host=%t, attempt=%d)", recoveryPressureWait, guestPressure, browserPressure, resources.HostPressure, r.Attempts+1))
	}
	_, _ = s.submitJob("network-recovery", func() error { return s.runNetworkRecovery(r, func() error { return networkRecoveryCheck(s) }) })
}

func (s *Server) runNetworkRecovery(expected networkRecovery, recheck func() error) error {
	s.mu.Lock()
	stopped := s.closing || s.networkPaused || s.safety.pauseVersion != expected.PauseVersion
	if stopped {
		s.recovery = networkRecovery{}
	}
	s.mu.Unlock()
	if stopped {
		return context.Canceled
	}
	client, err := s.gateClient()
	if err != nil {
		return s.finishNetworkRecovery(expected, err)
	}
	ctx, cancel := context.WithTimeout(s.operationContext(), 5*time.Second)
	state, err := client.Status(ctx)
	cancel()
	if err != nil {
		return s.finishNetworkRecovery(expected, err)
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return s.finishNetworkRecovery(expected, err)
	}
	s.mu.Lock()
	valid := !s.closing && s.recovery == expected && recoveryMatches(expected, state, cfg.Fingerprint(), s.networkPaused, s.safety.pauseVersion)
	if !valid {
		s.recovery = networkRecovery{}
	}
	s.mu.Unlock()
	if !valid {
		return context.Canceled
	}
	if err := s.operationContext().Err(); err != nil {
		return err
	}
	s.setMessage("正在重新检查环境与出口，通过后自动恢复联网…")
	err = recheck()
	return s.finishNetworkRecovery(expected, err)
}

func (s *Server) finishNetworkRecovery(expected networkRecovery, err error) error {
	// A failed full check changes the gate epoch. Keep that exact epoch for the
	// next attempt, while preserving user/configuration changes as cancellation.
	var current cage.GateStatus
	if err != nil {
		if client, e := s.gateClient(); e == nil {
			ctx, cancel := context.WithTimeout(s.lifetimeContext(), 3*time.Second)
			current, _ = client.Status(ctx)
			cancel()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.refreshNetworkExplanationLocked(time.Now())
	if s.recovery != expected {
		return err
	}
	if err == nil || s.closing || s.networkPaused || s.safety.pauseVersion != expected.PauseVersion || errors.Is(err, context.Canceled) {
		s.recovery = networkRecovery{}
		return err
	}
	if current.RunID != "" {
		if current.RunID != expected.RunID || current.Configuration != expected.Configuration || current.Mode != cage.GateClosed {
			s.recovery = networkRecovery{}
			return err
		}
		s.recovery.Epoch = current.Epoch
	}
	s.recovery.Attempts++
	delay := time.Duration(min(300, 15<<min(s.recovery.Attempts, 5))) * time.Second
	s.recovery.Next = time.Now().Add(delay)
	// Pressure may postpone the next attempt again, for at most
	// recoveryPressureWait after this backoff.
	s.recovery.pressureLogged = false
	s.recovery.pressureSince = time.Time{}
	// Show the latest full-check failure rather than the initial monitoring
	// error, which may describe a different missing sample by this point.
	s.safety.monitorError = diagnostics.Redact(usererror.Display("自动完整重检", err, "等待下一次重检；仍失败时导出诊断包。").Error())
	diagnostics.Log("warn", "network", fmt.Sprintf("automatic recheck attempt %d failed; next attempt in %s", s.recovery.Attempts, delay))
	return err
}

// memorySnapshot is what the network status says about the guest's memory.
type memorySnapshot struct {
	At    time.Time
	Short bool
	// Cause is what is short of memory: "environment" (the protected
	// classes, or the whole guest without the governor), "browser" (the
	// browser, held at its limit by the governor) or "machine".
	Cause  string
	Reason string
	Advice []string
}

// The memory a monitoring read was short of.
const (
	memoryCauseEnvironment = "environment"
	memoryCauseBrowser     = "browser"
	memoryCauseMachine     = "machine"
)

// memorySizing is what the resource status cannot say on its own: whether
// sizing advice applies (a Linux guest the host agent sizes, see
// resource-allocation.md 7.2), whether someone chose the allocation, and,
// for an older host agent that does not publish possible_bytes, what
// automatic sizing gives on this machine.
type memorySizing struct {
	applies, manual bool
	automatic       uint64
}

// automaticMemoryBytes is what automatic sizing gives on this machine;
// tests replace it.
var automaticMemoryBytes = func() uint64 {
	memory, _ := cage.AutomaticLimits()
	return uint64(memory) << 30
}

func (s *Server) memorySizing(status cage.ResourceStatus) memorySizing {
	if s.windowsGuest != nil || s.macosGuest != nil || containerenv.Configured(s.profile) {
		return memorySizing{}
	}
	sizing := memorySizing{applies: true}
	if allocation, err := cage.LoadResourceAllocation(s.configPath); err == nil {
		sizing.manual = !allocation.Effective().Automatic
	}
	if status.Possible == 0 {
		sizing.automatic = automaticMemoryBytes()
	}
	return sizing
}

// memoryNote builds the user-facing memory reason and advice from the
// resource manager's status (resource-allocation.md 7.4, 7.12). It is the
// one place the network explanation takes its memory wording from.
//
// pressure means the guest's protected classes stall (the whole guest
// without the governor) and short that the guest has all it can get now;
// browser_pressure that the governor holds the browser at its limit. They
// are kept through failed samples, so they count with a fresh reading only;
// pressure also counts with a failed one, which a starving guest causes.
func memoryNote(status cage.ResourceStatus, err error, now time.Time, sizing memorySizing) memorySnapshot {
	note := memorySnapshot{At: now}
	if err != nil {
		return note
	}
	fresh := status.Guest != nil && now.Sub(status.ObservedAt) <= 30*time.Second
	switch {
	case (status.Pressure || status.Short) && fresh:
		note.Short, note.Cause = true, memoryCauseEnvironment
		note.Reason = fmt.Sprintf("环境内存不足（可用约 %s，共 %s）", humanBytes(status.Guest.Available), humanBytes(status.Guest.Total))
	case status.Pressure && status.Error != "":
		// A starving guest also fails to answer the resource probe; keep its
		// last pressured reading rather than report the memory as fine.
		note.Short, note.Cause = true, memoryCauseEnvironment
		note.Reason = "环境内存不足，资源读数也暂时超时"
	case status.BrowserPressure && fresh:
		note.Short, note.Cause = true, memoryCauseBrowser
		note.Reason = "隔离浏览器内存紧张"
		if browserHeld(status) {
			note.Reason = "隔离浏览器占用的内存超过了环境能分给它的上限"
		}
	case status.HostPressure:
		note.Short, note.Cause = true, memoryCauseMachine
		note.Reason = "运行环境的这台机器内存不足"
		note.Advice = []string{"关闭这台机器上其他占用内存较多的程序"}
		return note
	default:
		return note
	}
	if note.Cause == memoryCauseBrowser {
		note.Advice = []string{"在隔离浏览器中关闭或刷新占用内存较多的标签页"}
	} else {
		note.Advice = []string{"在隔离浏览器中关闭或刷新占用内存较多的标签页，或结束环境里不需要的程序"}
	}
	if advice := sizingAdvice(status, sizing); advice != "" {
		note.Advice = append(note.Advice, advice)
	} else if !sizing.applies {
		// Guests the host agent does not size keep their own settings.
		note.Advice = append(note.Advice, "在资源设置中查看环境内存；需要时调高内存上限（重启环境后生效）")
	}
	return note
}

// browserHeldSlack is how far below its soft limit (memory.high) the browser
// class still counts as held there: one step of the governor's 64 MiB
// granularity (resource-governor.py STEP), since the kernel reclaims the
// class to just under memory.high rather than to it.
const browserHeldSlack = 64 << 20

// browserHeld reports whether the guest's governor holds the browser at its
// limit: its class stalls (browser_pressure, a 1 % stall) while its use has
// reached the soft limit, where the kernel throttles it. A browser that
// stalls below its limit is short of memory like the rest of the guest.
func browserHeld(status cage.ResourceStatus) bool {
	if !status.BrowserPressure {
		return false
	}
	for _, class := range status.Classes {
		if class.Name == "browser" {
			return class.High > 0 && class.Current+browserHeldSlack >= class.High
		}
	}
	return false
}

// sizingAdvice is the allocator's own sentence about more memory when it
// has one (short at its maximum), what holds the guest when the machine
// does, and otherwise "可以调到 N GB" when automatic sizing gives more than
// the environment has: possible_bytes from the host agent, or this machine's
// automatic size against an older host agent that does not publish it.
func sizingAdvice(status cage.ResourceStatus, sizing memorySizing) string {
	if status.Advice != nil && status.Advice.Message != "" {
		return status.Advice.Message
	}
	if !sizing.applies {
		return ""
	}
	if status.LimitedBy == "machine" {
		return "这台机器暂时没有更多内存可给环境；关闭这台机器上不用的程序后会自动增加"
	}
	possible := status.Possible
	if possible == 0 {
		possible = sizing.automatic
	}
	if status.Maximum == 0 || possible <= status.Maximum {
		return ""
	}
	if sizing.manual {
		return "在资源设置中改为自动（推荐）后可以调到 " + memoryGB(possible) + "，需要重启环境"
	}
	return "在资源设置中可以把内存调到 " + memoryGB(possible) + "，需要重启环境"
}

// memoryGB words a memory size as the allocator's advice does.
func memoryGB(size uint64) string {
	value := float64(size) / float64(1<<30)
	if value >= 10 || value == math.Trunc(value) {
		return fmt.Sprintf("%.0f GB", value)
	}
	return fmt.Sprintf("%.1f GB", value)
}

func (s *Server) readMemorySnapshot() memorySnapshot {
	status, err := cage.ReadResourceStatus(s.instanceDir())
	return memoryNote(status, err, time.Now(), s.memorySizing(status))
}

// withMemoryReason leads text with the memory shortage that likely caused it.
func withMemoryReason(memory memorySnapshot, text string) string {
	if !memory.Short {
		return text
	}
	return memory.Reason + "，" + text
}

func humanBytes(n uint64) string {
	if n < 1<<30 {
		return fmt.Sprintf("%d MB", n>>20)
	}
	return fmt.Sprintf("%.1f GB", math.Floor(float64(n)/float64(1<<30)*10)/10)
}

// Network explanation states. The UI and the phone show reason, next and
// advice as they are; state lets them pick an icon or an action.
const (
	// The gate is open, but the last monitoring round could not confirm the
	// guest. The lease is not extended; it ends at lease_until unless a later
	// round confirms the guest first.
	networkUnconfirmed = "unconfirmed"
	// Closed; an automatic full recheck is due but waits for memory pressure
	// to subside, at most until recheck_at.
	networkWaitingMemory = "waiting_memory"
	// Closed; an automatic full recheck is scheduled (recheck_at, when known).
	networkRecheckScheduled = "recheck_scheduled"
	// The automatic full recheck is running.
	networkRechecking = "rechecking"
	// Closed with no automatic recheck: the user must act and check again.
	networkCheckRequired = "check_required"
	// The user paused networking.
	networkPausedState = "paused"
)

// networkExplanation says why networking is closed or at risk and what will
// happen next, in the product's words. Times are absolute; clients count down.
type networkExplanation struct {
	State       string     `json:"state"`
	Reason      string     `json:"reason"`
	Next        string     `json:"next"`
	RecheckAt   *time.Time `json:"recheck_at,omitempty"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	MemoryShort bool       `json:"memory_short"`
	// MemoryCause says what is short when memory_short is set: environment,
	// browser (held at its limit) or machine. Additive; older clients ignore it.
	MemoryCause string    `json:"memory_cause,omitempty"`
	Advice      []string  `json:"advice,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// refreshNetworkExplanationLocked recomputes safety.Explanation from the
// current state. Called with s.mu held on every network transition and every
// monitor and recovery round, so the stored value is at most a round old.
func (s *Server) refreshNetworkExplanationLocked(now time.Time) {
	s.safety.Explanation = s.networkExplanationLocked(now)
}

func (s *Server) networkExplanationLocked(now time.Time) *networkExplanation {
	gate := s.safety.Gate
	if s.closing || gate == nil {
		return nil
	}
	memory := s.safety.memory
	if now.Sub(memory.At) > 30*time.Second {
		memory = memorySnapshot{}
	}
	e := &networkExplanation{MemoryShort: memory.Short, MemoryCause: memory.Cause, Advice: memory.Advice, UpdatedAt: now.UTC()}
	if s.networkPaused {
		e.State, e.Reason, e.Next, e.Advice = networkPausedState, "外网已手动暂停", "离线终端和文件仍可使用；重新检测通过后恢复联网。", nil
		e.MemoryShort, e.MemoryCause = false, ""
		return e
	}
	reason := s.networkErrorLocked()
	if reason == "" {
		reason = gate.Reason
	}
	if gate.Mode == cage.GateOpen {
		d := s.activeDeferralLocked(now)
		if d == nil {
			return nil
		}
		// The lease's end as the gate times it, shown on today's wall clock.
		until := now.Add(d.Deadline.Sub(now)).UTC()
		e.State, e.LeaseUntil = networkUnconfirmed, &until
		e.Reason = withMemoryReason(memory, "联网监测暂时"+unconfirmedStage(d.Stage))
		e.Next = "外网暂时保持开放，但联网许可没有延长。系统会继续重试；许可到期前仍无法确认时，外网会自动暂停并安排完整重检。"
		return e
	}
	if s.proxyNetworkLost {
		e.State, e.Reason = networkRecheckScheduled, reason
		e.Next = "外网暂停中。电脑的网络恢复后，会自动重新连接代理并重新检测。"
		return e
	}
	if r, pending := s.pendingRecoveryLocked(); pending {
		// A temporary failure: the memory shortage is its likely cause.
		e.Reason = withMemoryReason(memory, reason)
		job := s.jobs.snapshot()
		switch {
		case job.Running && job.Kind == "network-recovery":
			e.State, e.Next = networkRechecking, "正在执行自动完整重检，通过后自动恢复联网。"
		case job.Running:
			e.State, e.Next = networkRecheckScheduled, "外网暂停中。自动完整重检会在当前操作完成后进行，通过后自动恢复联网。"
		case !s.ready:
			e.State, e.Next = networkRecheckScheduled, "外网暂停中。自动完整重检会在环境就绪后进行，通过后自动恢复联网。"
		case !r.pressureSince.IsZero():
			at := r.pressureSince.Add(recoveryPressureWait)
			if at.Before(now) {
				at = now
			}
			at = at.UTC()
			e.State, e.RecheckAt = networkWaitingMemory, &at
			e.Next = "外网暂停中。自动完整重检正在等待内存压力缓解，最迟" + roughWait(at.Sub(now)) + "也会进行，通过后自动恢复联网。"
		default:
			at := r.Next
			if at.Before(now) {
				at = now
			}
			at = at.UTC()
			e.State, e.RecheckAt = networkRecheckScheduled, &at
			e.Next = "外网暂停中。自动完整重检将在" + roughWait(at.Sub(now)) + "进行，通过后自动恢复联网。"
		}
		return e
	}
	if gate.Mode != cage.GateClosed || s.jobs.snapshot().Running {
		// A check or another operation owns the next transition; its
		// progress is the job's message.
		return nil
	}
	if gate.Report != nil && (gate.Reason == gateLeaseExpired || s.windowsGuest != nil && gate.Reason == windowsPacketDisconnectedReason) {
		// The monitor schedules the recheck on its next round.
		e.State, e.Reason = networkRecheckScheduled, withMemoryReason(memory, reason)
		e.Next = "外网暂停中。马上安排自动完整重检，通过后自动恢复联网。"
		return e
	}
	// No automatic recheck is scheduled. Memory stays in memory_short and
	// advice; the reason is the closure's own cause.
	e.State, e.Reason = networkCheckRequired, reason
	e.Next = "外网保持关闭，目前没有安排自动重检。处理上述问题后请重新检测。"
	return e
}

// roughWait words a wait that clients also show as a countdown; the stored
// text may be a monitor round old.
func roughWait(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "片刻后"
	case d < 90*time.Second:
		return fmt.Sprintf("约 %d 秒后", (int(d.Seconds())+4)/5*5)
	default:
		return fmt.Sprintf("约 %d 分钟后", int(math.Round(d.Minutes())))
	}
}
