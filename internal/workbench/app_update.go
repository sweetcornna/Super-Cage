package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/semver"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"github.com/sylvinus/agent-vm/internal/version"
)

// appUpdateGuestTimeout bounds the one guest read of a decision. A guest that
// does not answer in time is unknown, which is busy.
const appUpdateGuestTimeout = 4 * time.Second

var errAppUpdateSealed = errors.New("正在准备升级，请稍候再试")

// silentRequest is what the hourly tick hands to the native shell. Only the
// id and version are written to disk; the fingerprint stays in this process,
// so a request from an earlier process can never be installed.
// silentRequestFile is watched by the native shell while silent mode is on.
const silentRequestFile = "silent-request.json"

// silentRequestLifetime bounds how long the shell may take to act on a
// request; an older one is replaced at the next tick.
const silentRequestLifetime = time.Hour

type silentRequest struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	DecidedAt   time.Time `json:"decided_at"`
	fingerprint string
	publishedAt string
}

// appUpdateState is the workbench's part of application updates. Its mutex
// is never held while feeders run: feeders take Server.mu and other locks.
type appUpdateState struct {
	once      sync.Once
	registry  policy.Registry
	lastPhone atomic.Int64

	mu      sync.Mutex
	mode    policy.Mode // cached preference; empty until first read
	hold    policy.HoldTracker
	pending *policy.Pending
	checked time.Time
	request *silentRequest
	waiting *policy.Decision
	note    string // shown with the chain while it waits for the person

	chain sync.Mutex // serializes the post-update chain in this process

	// Test seams; nil selects the production implementation.
	clock       func() time.Time
	inputIdle   func(context.Context) (time.Duration, error)
	guest       func(context.Context) (policy.Reading, error)
	check       func(context.Context) (*appupdate.Available, error)
	stop        func() error
	start       func() error
	sync        func() error
	unavailable func() string
}

// startRun and syncRun are the chain's environment steps.
func (s *Server) appUpdateStartRun() func() error {
	if s.appUpdate.start != nil {
		return s.appUpdate.start
	}
	return s.startRun(nil, "")
}

func (s *Server) appUpdateSyncRun() func() error {
	if s.appUpdate.sync != nil {
		return s.appUpdate.sync
	}
	return s.synchronizeEnvironment
}

func (a *appUpdateState) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

func (a *appUpdateState) guestRead(s *Server) func(context.Context) (policy.Reading, error) {
	if a.guest != nil {
		return a.guest
	}
	return s.readAppUpdateGuest
}

// appUpdateRoute names the window routes of application updates.
func appUpdateRoute(path string) bool {
	return path == "/api/app-update" || strings.HasPrefix(path, "/api/app-update/")
}

func (s *Server) updateRegistry() *policy.Registry {
	s.appUpdate.once.Do(func() { s.registerAppUpdateFeeders(&s.appUpdate.registry) })
	return &s.appUpdate.registry
}

// updateProfile is the profile whose updates/ directory the native shell
// shares: the full profile, also for the desktop gateway.
func (s *Server) updateProfile() string {
	if s.appUpdateConfigPath != "" {
		return filepath.Dir(s.appUpdateConfigPath)
	}
	return s.profile
}

// noteAppUpdateActivity is called by admission points (a phone turn, an
// upload, a download) so that work which starts and ends between two looks
// still changes the fingerprint.
func (s *Server) noteAppUpdateActivity() { s.appUpdate.registry.Touch() }

func (s *Server) notePhoneRequest() {
	s.appUpdate.lastPhone.Store(time.Now().UnixNano())
	s.appUpdate.registry.Touch()
}

// dropSilentRequest forgets an unattended request that was refused or went
// stale, so the next idle tick can decide afresh.
func (s *Server) dropSilentRequest() {
	a := &s.appUpdate
	a.mu.Lock()
	a.request = nil
	a.mu.Unlock()
	_ = os.Remove(filepath.Join(policy.Dir(s.updateProfile()), silentRequestFile))
}

func (s *Server) cachedUpdateMode() policy.Mode {
	a := &s.appUpdate
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode == "" {
		settings, _ := policy.LoadSettings(s.updateProfile())
		a.mode = settings.Mode
	}
	return a.mode
}

// silentUnavailable names why this installation cannot be updated with
// nobody watching; empty when it can. The publisher checks must be able to
// run, and the installer must not need elevation.
func (s *Server) silentUnavailable() string {
	if s.appUpdate.unavailable != nil {
		return s.appUpdate.unavailable()
	}
	executable, err := os.Executable()
	if err != nil {
		return "无法确认安装位置，不支持静默更新"
	}
	return silentUnavailableReason(runtime.GOOS, version.Version, executable, dirWritable)
}

func silentUnavailableReason(goos, current, executable string, writable func(string) bool) string {
	switch goos {
	case "linux":
		return "Linux 版本由系统安装包管理，不支持静默更新"
	case "darwin", "windows":
	default:
		return "此平台不支持静默更新"
	}
	if tag := "v" + current; !semver.IsValid(tag) || semver.Canonical(tag) != tag {
		return "开发版本没有可核验的发布者，不支持静默更新"
	}
	dir := filepath.Dir(executable)
	if goos == "darwin" {
		// …/Super Cage.app/Contents/Resources/cage: the bundle's folder must be
		// writable, or replacing the app needs an administrator's approval.
		bundle := filepath.Dir(filepath.Dir(dir))
		if filepath.Ext(bundle) != ".app" || filepath.Base(dir) != "Resources" {
			return "开发构建不支持静默更新"
		}
		dir = filepath.Dir(bundle)
	}
	if !writable(dir) {
		return "安装位置需要管理员权限，不能静默更新；可以点击升级"
	}
	return ""
}

// appUpdateStatusView is what the window shows in the button and in 设置 → 关于.
func (s *Server) appUpdateStatusView() policy.Status {
	profile := s.updateProfile()
	settings, err := policy.LoadSettings(profile)
	st := policy.Status{Mode: settings.Mode}
	if err != nil {
		st.ModeError = err.Error()
	}
	reason := s.silentUnavailable()
	st.SilentAvailable, st.SilentUnavailableReason = reason == "", reason
	a := &s.appUpdate
	a.mu.Lock()
	if a.pending != nil {
		pending := *a.pending
		pending.Requested = a.request != nil && a.request.Version == pending.Version
		st.Pending = &pending
	}
	if a.waiting != nil && settings.Mode == policy.Silent {
		waiting := *a.waiting
		st.Waiting = &waiting
	}
	note := a.note
	a.mu.Unlock()
	if ledger, err := policy.LoadLedger(profile); err == nil {
		st.LastAttempt = ledger.Last()
	}
	c, err := policy.LoadChain(profile)
	if err != nil {
		st.Chain = &policy.ChainView{Step: policy.StepAttention, Error: "升级进度记录无法读取，可以重新点击升级"}
	} else if st.Chain = policy.View(c); st.Chain != nil && c.Active() {
		st.Chain.Note = note
	}
	return st
}

func (s *Server) appUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) {
		http.Error(w, "请在本机 Super Cage 窗口中查看升级设置", http.StatusForbidden)
		return
	}
	jsonReply(w, s.appUpdateStatusView())
}

// appUpdatePolicy is the only writer of the update mode. A phone, a remote
// view and the agent transport cannot reach it.
func (s *Server) appUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) {
		http.Error(w, "只能在本机 Super Cage 窗口中更改更新方式", http.StatusForbidden)
		return
	}
	var input struct {
		Mode policy.Mode `json:"mode"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !input.Mode.Valid() {
		http.Error(w, "更新方式无效", http.StatusBadRequest)
		return
	}
	if input.Mode == policy.Silent {
		if reason := s.silentUnavailable(); reason != "" {
			http.Error(w, reason, http.StatusConflict)
			return
		}
	}
	a := &s.appUpdate
	if err := policy.SaveSettings(s.updateProfile(), policy.Settings{Mode: input.Mode, UpdatedAt: a.now()}); err != nil {
		http.Error(w, "无法保存更新方式："+err.Error(), http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.mode = input.Mode
	if input.Mode == policy.Manual {
		a.hold, a.request, a.waiting = policy.HoldTracker{}, nil, nil
	}
	a.mu.Unlock()
	if input.Mode == policy.Manual {
		_ = os.Remove(filepath.Join(policy.Dir(s.updateProfile()), silentRequestFile))
	}
	diagnostics.Log("info", "app-update", "update mode set to "+string(input.Mode))
	jsonReply(w, s.appUpdateStatusView())
}

type appUpdateInterruptions struct {
	Blockers           []policy.Blocker  `json:"blockers"`
	MustWait           []policy.Blocker  `json:"must_wait"`
	Confirm            []string          `json:"confirm"`
	Reopen             []policy.Terminal `json:"reopen"`
	EnvironmentRunning bool              `json:"environment_running"`
}

// appUpdateInterruptions lists what an update would interrupt right now, for
// the confirmation shown before the click proceeds.
func (s *Server) appUpdateInterruptions(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) {
		http.Error(w, "请在本机 Super Cage 窗口中升级", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*appUpdateGuestTimeout)
	defer cancel()
	snapshot := s.updateRegistry().Snapshot(ctx, s.appUpdate.now(), true)
	_, tabs := s.terminals.updateActivity()
	out := appUpdateInterruptions{Blockers: []policy.Blocker{}, MustWait: []policy.Blocker{}, Confirm: policy.ConfirmationIDs(snapshot.Blockers), Reopen: tabs, EnvironmentRunning: s.vmRunning()}
	for _, b := range snapshot.Blockers {
		if b.Interruptible {
			out.Blockers = append(out.Blockers, b)
		} else {
			out.MustWait = append(out.MustWait, b)
		}
	}
	if out.Confirm == nil {
		out.Confirm = []string{}
	}
	jsonReply(w, out)
}

type appUpdateHandoffRequest struct {
	Token     string   `json:"token"`
	Version   string   `json:"version"`
	Trigger   string   `json:"trigger"`
	Confirmed []string `json:"confirmed"`
	Request   string   `json:"request"`
}

type appUpdateHandoffReply struct {
	Status   string           `json:"status"`
	Code     policy.Code      `json:"code,omitempty"`
	Message  string           `json:"message,omitempty"`
	Blockers []policy.Blocker `json:"blockers,omitempty"`
}

func appUpdateRefuse(w http.ResponseWriter, code policy.Code, message string, blockers []policy.Blocker) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(appUpdateHandoffReply{Status: "blocked", Code: code, Message: message, Blockers: blockers})
}

// appUpdateBlockers answers the launcher with the current snapshot.
func (s *Server) appUpdateBlockers(w http.ResponseWriter, r *http.Request) {
	if !s.launcherAuthenticated(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*appUpdateGuestTimeout)
	defer cancel()
	jsonReply(w, s.updateRegistry().Snapshot(ctx, s.appUpdate.now(), true))
}

// appUpdateHandoff answers "may I install now?" for the launcher. The whole
// decision and the stop are one operation under the operation lock, with new
// work refused while it runs, so nothing can start between the decision and
// the stop.
func (s *Server) appUpdateHandoff(w http.ResponseWriter, r *http.Request) {
	var input appUpdateHandoffRequest
	if !readJSON(w, r, &input) {
		return
	}
	if !equalSecret(input.Token, s.token) {
		http.Error(w, "Launcher refused", http.StatusUnauthorized)
		return
	}
	trigger := policy.Trigger(input.Trigger)
	if trigger != policy.Click && trigger != policy.SilentUpdate {
		appUpdateRefuse(w, policy.NotEligible, "升级请求无效", nil)
		return
	}
	if !s.op.TryLock() {
		appUpdateRefuse(w, policy.Busy, "另一个操作仍在进行，请稍后再升级", nil)
		return
	}
	locked := true
	defer func() {
		if locked {
			s.op.Unlock()
		}
	}()
	s.mu.Lock()
	if s.closing || s.shutdown == nil {
		s.mu.Unlock()
		http.Error(w, "后台正在更新，请重新打开 Super Cage。", http.StatusServiceUnavailable)
		return
	}
	s.appUpdateSealed = true
	s.mu.Unlock()
	unseal := func() {
		s.mu.Lock()
		s.appUpdateSealed, s.closing = false, false
		s.mu.Unlock()
	}
	a := &s.appUpdate
	profile := s.updateProfile()
	ctx, cancel := context.WithTimeout(context.Background(), 2*appUpdateGuestTimeout)
	defer cancel()
	now := a.now()
	snapshot := s.updateRegistry().Snapshot(ctx, now, true)
	request := policy.Request{Trigger: trigger, Version: input.Version, Confirmed: input.Confirmed}
	facts := policy.Facts{Current: version.Version, Snapshot: snapshot, Now: now}
	if trigger == policy.SilentUpdate {
		facts.Settings, facts.SettingsErr = policy.LoadSettings(profile)
		facts.Ledger, facts.LedgerErr = policy.LoadLedger(profile)
		facts.SilentUnavailable = s.silentUnavailable()
		a.mu.Lock()
		pending, hold := a.request, a.hold
		a.mu.Unlock()
		if pending == nil || pending.ID == "" || pending.ID != input.Request || pending.Version != input.Version {
			unseal()
			appUpdateRefuse(w, policy.Changed, "自动升级请求已过期，等待下一次空闲", nil)
			return
		}
		request.Fingerprint, facts.PublishedAt, facts.Hold = pending.fingerprint, pending.publishedAt, hold
		idle := a.inputIdle
		if idle == nil {
			idle = hostInputIdle
		}
		if d, err := idle(ctx); err == nil {
			facts.InputIdle, facts.InputIdleKnown = d, true
		}
	}
	decision := policy.Decide(request, facts)
	if !decision.Allowed() {
		unseal()
		if trigger == policy.SilentUpdate {
			s.dropSilentRequest()
			a.mu.Lock()
			a.waiting = &decision
			a.mu.Unlock()
		}
		appUpdateRefuse(w, decision.Code, decision.Message, decision.Blockers)
		return
	}
	_, tabs := s.terminals.updateActivity()
	environment := policy.Environment{Running: s.vmRunning(), Workspace: s.workspace(), Terminals: tabs}
	// Close admission completely, then look once more at the work the
	// seal does not cover. Anything new since the decision wins.
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	final := s.updateRegistry().Snapshot(ctx, a.now(), false)
	if trigger == policy.SilentUpdate && (final.Fingerprint != snapshot.Fingerprint || !final.Idle()) {
		unseal()
		s.dropSilentRequest()
		appUpdateRefuse(w, policy.Changed, "空闲期间有新的活动，等待下一次空闲", final.Blockers)
		return
	}
	if trigger == policy.Click {
		// A click accepted what it saw; it still never interrupts work that
		// started after the confirmation.
		facts.Snapshot = final
		if again := policy.Decide(request, facts); !again.Allowed() {
			unseal()
			appUpdateRefuse(w, again.Code, again.Message, again.Blockers)
			return
		}
	}
	ledger, err := policy.LoadLedger(profile)
	if err != nil {
		ledger = policy.Ledger{}
	}
	ledger.Begin(policy.Attempt{Version: input.Version, From: version.Version, Trigger: trigger, StartedAt: now})
	chain := &policy.Chain{Schema: policy.ChainSchema, ID: randomToken()[:16], From: version.Version, To: input.Version, Trigger: trigger,
		Step: policy.StepStopping, Environment: environment, StartedAt: now, UpdatedAt: now}
	// The attempt and the record are durable before anything stops: a crash
	// from here on reads as a failed attempt that silent mode never retries.
	if err = errors.Join(policy.SaveLedger(profile, ledger), policy.SaveChain(profile, chain)); err != nil {
		unseal()
		appUpdateRefuse(w, policy.Unreadable, "无法保存升级记录，未停止环境："+err.Error(), nil)
		return
	}
	stop := a.stop
	if stop == nil {
		stop = s.stopEnvironment
	}
	stopErr := stop()
	if stopErr == nil {
		if owners, probeErr := controllerVMOwners(s.profile); probeErr != nil || len(owners) != 0 {
			stopErr = errors.New("环境仍在运行")
		}
	}
	if stopErr == nil {
		if err = managedproxy.Stop(s.profile); err != nil {
			stopErr = errors.New("代理尚未停止")
		}
	}
	if stopErr != nil {
		reason := "环境未能安全停止，未安装更新：" + stopErr.Error()
		chain.Fail(policy.StepStopping, reason, a.now())
		ledger.Finish(input.Version, false, policy.StepStopping, reason, a.now())
		_ = errors.Join(policy.SaveChain(profile, chain), policy.SaveLedger(profile, ledger))
		unseal()
		s.op.Unlock()
		locked = false
		if environment.Running {
			go s.restoreEnvironmentAfterUpdate(chain.ID)
		}
		appUpdateRefuse(w, policy.Code("stop_failed"), reason, nil)
		return
	}
	_ = chain.Advance(policy.StepInstalling, a.now())
	if err = policy.SaveChain(profile, chain); err != nil {
		diagnostics.Log("warn", "app-update", "chain record not advanced: "+err.Error())
	}
	diagnostics.Log("info", "app-update", "installing "+input.Version+" ("+string(trigger)+")")
	w.Header().Set("Content-Type", "application/json")
	const prepared = "{\"status\":\"prepared\"}\n"
	w.Header().Set("Content-Length", "21")
	_, _ = w.Write([]byte(prepared))
	_ = http.NewResponseController(w).Flush()
	s.op.Unlock()
	locked = false
	go s.shutdown()
}

// restoreEnvironmentAfterUpdate starts the environment again after an update
// that stopped it and then did not happen.
func (s *Server) restoreEnvironmentAfterUpdate(id string) {
	ctx := s.lifetimeContext()
	if err := s.waitAccountUnlocked(ctx); err != nil {
		return
	}
	if _, err := s.appUpdateRunJob(ctx, "start", s.appUpdateStartRun()); err != nil {
		diagnostics.Log("warn", "app-update", "environment not restored after update "+id+": "+err.Error())
	}
}

func (s *Server) waitAccountUnlocked(ctx context.Context) error {
	a := &s.appUpdate
	for {
		if _, ok := s.account.access(); ok {
			a.mu.Lock()
			a.note = ""
			a.mu.Unlock()
			return nil
		}
		a.mu.Lock()
		a.note = "解锁 Super Cage 后继续恢复环境"
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// appUpdateRunJob submits a job as the chain's own step and waits for it. A
// job the person started first is waited for, then the step is decided again.
func (s *Server) appUpdateRunJob(ctx context.Context, kind string, run func() error) (Job, error) {
	for attempt := 0; attempt < 3; attempt++ {
		job, err := s.submitJob(kind, run)
		if errors.Is(err, errJobBusy) {
			if _, err = s.waitJob(ctx, ""); err != nil {
				return Job{}, err
			}
			continue
		}
		if err != nil {
			return Job{}, err
		}
		return s.waitJob(ctx, job.ID)
	}
	return Job{}, errJobBusy
}

// waitJob waits until job id (or, for "", whatever is running) has ended.
func (s *Server) waitJob(ctx context.Context, id string) (Job, error) {
	for {
		job := s.jobs.snapshot()
		if !job.Running || (id != "" && job.ID != id) {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// resumeAppUpdateChain runs once when the workload starts. It continues a
// chain the previous version began, or records that the installation rolled
// back and brings the environment back.
func (s *Server) resumeAppUpdateChain() {
	a := &s.appUpdate
	a.chain.Lock()
	defer a.chain.Unlock()
	profile := s.updateProfile()
	c, err := policy.LoadChain(profile)
	if err != nil {
		diagnostics.Log("warn", "app-update", "chain record unreadable: "+err.Error())
		return
	}
	resumption := policy.Resume(c, version.Version)
	now := a.now()
	switch {
	case resumption.Attention:
		// Never rewrite a record of another schema; a known record that this
		// version cannot continue is marked so that the button returns.
		if c.Known() {
			_ = c.Advance(policy.StepAttention, now)
			_ = policy.SaveChain(profile, c)
		}
		return
	case resumption.RolledBack:
		reason := lastInstallerMessage(profile)
		if reason == "" {
			reason = "安装没有完成，已恢复原应用"
		}
		c.Fail(policy.StepInstalling, reason, now)
		_ = policy.SaveChain(profile, c)
		s.finishAttempt(c.To, false, policy.StepInstalling, reason)
		if c.Environment.Running {
			s.restoreEnvironmentAfterUpdate(c.ID)
		}
		return
	case resumption.Continue == "":
		return
	}
	if c.Step != resumption.Continue {
		if err = c.Advance(resumption.Continue, now); err != nil {
			return
		}
		_ = policy.SaveChain(profile, c)
	}
	s.runAppUpdateChain(c)
}

func lastInstallerMessage(profile string) string {
	data, err := os.ReadFile(filepath.Join(policy.Dir(profile), "last-result.json"))
	if err != nil || len(data) > 16384 {
		return ""
	}
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &result) != nil || result.Success {
		return ""
	}
	return strings.TrimSpace(result.Message)
}

func (s *Server) finishAttempt(version string, ok bool, step policy.Step, reason string) {
	profile := s.updateProfile()
	ledger, err := policy.LoadLedger(profile)
	if err != nil {
		return
	}
	if ledger.Finish(version, ok, step, reason, s.appUpdate.now()) {
		_ = policy.SaveLedger(profile, ledger)
	}
}

// runAppUpdateChain brings the environment back as it was on the new
// version: start it if it was running, synchronize components when the
// workbench reports them out of date, then confirm the network gate. Each
// step is idempotent, so a restart in the middle continues where it stood.
func (s *Server) runAppUpdateChain(c *policy.Chain) {
	profile := s.updateProfile()
	ctx := s.lifetimeContext()
	save := func(next policy.Step) bool {
		if err := c.Advance(next, s.appUpdate.now()); err != nil {
			return false
		}
		return policy.SaveChain(profile, c) == nil
	}
	fail := func(step policy.Step, reason string) {
		c.Fail(step, reason, s.appUpdate.now())
		_ = policy.SaveChain(profile, c)
		s.finishAttempt(c.To, false, step, reason)
		diagnostics.Log("warn", "app-update", "update chain stopped at "+string(step)+": "+reason)
	}
	for ctx.Err() == nil {
		switch c.Step {
		case policy.StepRelaunched:
			next := policy.StepDone
			if c.Environment.Running {
				next = policy.StepRestoring
			}
			if !save(next) {
				return
			}
		case policy.StepRestoring:
			if err := s.waitAccountUnlocked(ctx); err != nil {
				return
			}
			if !s.vmRunning() {
				job, err := s.appUpdateRunJob(ctx, "start", s.appUpdateStartRun())
				if err != nil || job.Error != "" {
					reason := job.Error
					if err != nil {
						reason = err.Error()
					}
					if job.Message == "环境已启动，联网检查未通过" {
						fail(policy.StepNetwork, "已升级到 "+c.To+"，环境已启动，但联网检查未通过："+reason)
					} else {
						fail(policy.StepRestoring, "已升级到 "+c.To+"，但环境未能启动："+reason)
					}
					return
				}
			}
			s.mu.Lock()
			required := s.guestUpdateRequired
			s.mu.Unlock()
			next := policy.StepTools
			if required {
				next = policy.StepComponents
			}
			if !save(next) {
				return
			}
		case policy.StepComponents:
			s.mu.Lock()
			required := s.guestUpdateRequired
			s.mu.Unlock()
			if required {
				job, err := s.appUpdateRunJob(ctx, "environment-update", s.appUpdateSyncRun())
				if err != nil || job.Error != "" {
					reason := job.Error
					if err != nil {
						reason = err.Error()
					}
					fail(policy.StepComponents, "已升级到 "+c.To+"，但环境组件未能更新："+reason)
					return
				}
			}
			if !save(policy.StepTools) {
				return
			}
		case policy.StepTools:
			// No current version requires a tool update to work: pinned tools
			// stay at their versions until 更新环境工具, as before. A version
			// that requires one adds the check here.
			if !save(policy.StepNetwork) {
				return
			}
		case policy.StepNetwork:
			s.mu.Lock()
			paused, gate, problem := s.networkPaused, s.safety.Gate, s.safety.Error
			s.mu.Unlock()
			if !paused && (gate == nil || gate.Mode != cage.GateOpen) {
				if problem == "" {
					problem = "外网尚未放行"
				}
				fail(policy.StepNetwork, "已升级到 "+c.To+"，环境已恢复，但联网检查未通过："+problem)
				return
			}
			if !save(policy.StepDone) {
				return
			}
		case policy.StepDone:
			s.finishAttempt(c.To, true, policy.StepDone, "")
			diagnostics.Log("info", "app-update", "updated to "+c.To)
			return
		default:
			return
		}
	}
}

// appUpdateTick runs on the workload's existing hourly ticker. In manual mode
// it reads one in-memory value and returns: no request, no file, no guest.
func (s *Server) appUpdateTick(ctx context.Context) {
	if s.cachedUpdateMode() != policy.Silent {
		return
	}
	a := &s.appUpdate
	setWaiting := func(code policy.Code, message string, blockers []policy.Blocker) {
		a.mu.Lock()
		a.waiting = &policy.Decision{Code: code, Message: message, Blockers: blockers}
		a.mu.Unlock()
	}
	if reason := s.silentUnavailable(); reason != "" {
		setWaiting(policy.SilentUnavailable, reason, nil)
		return
	}
	now := a.now()
	a.mu.Lock()
	pending, checked, request := a.pending, a.checked, a.request
	a.mu.Unlock()
	if pending == nil || now.Sub(checked) >= policy.CheckEvery {
		check := a.check
		if check == nil {
			check = func(ctx context.Context) (*appupdate.Available, error) {
				return appupdate.CheckLatest(ctx, s.appUpdateConfigPathOrDefault())
			}
		}
		available, err := check(ctx)
		a.mu.Lock()
		a.checked = now
		a.mu.Unlock()
		if err != nil {
			setWaiting(policy.Unreadable, "暂时无法检查新版本，稍后再试", nil)
			return
		}
		pending = nil
		if available != nil && policy.Eligible(available.Version, version.Version) == nil {
			pending = &policy.Pending{Version: available.Version, PublishedAt: available.PublishedAt, Installable: available.Installable, Reason: available.Reason}
		}
		a.mu.Lock()
		a.pending = pending
		if pending == nil {
			a.waiting, a.request, a.hold = nil, nil, policy.HoldTracker{}
		}
		a.mu.Unlock()
	}
	if pending == nil {
		return
	}
	if !pending.Installable {
		setWaiting(policy.SilentUnavailable, pending.Reason, nil)
		return
	}
	if request != nil {
		if request.Version == pending.Version && now.Sub(request.DecidedAt) < silentRequestLifetime {
			return
		}
		s.dropSilentRequest()
	}
	snapshot := s.updateRegistry().Snapshot(ctx, now, false)
	a.mu.Lock()
	a.hold.Observe(snapshot)
	hold := a.hold
	a.mu.Unlock()
	if !snapshot.Idle() {
		setWaiting(policy.Busy, "等待空闲："+snapshot.Blockers[0].Label, snapshot.Blockers)
		return
	}
	if hold.HeldFor(now, snapshot.Fingerprint) < policy.HoldPeriod {
		setWaiting(policy.NotHeld, "等待电脑持续空闲", nil)
		return
	}
	full := s.updateRegistry().Snapshot(ctx, now, true)
	profile := s.updateProfile()
	facts := policy.Facts{Current: version.Version, Snapshot: full, Hold: hold, PublishedAt: pending.PublishedAt, Now: now, SilentUnavailable: ""}
	facts.Settings, facts.SettingsErr = policy.LoadSettings(profile)
	facts.Ledger, facts.LedgerErr = policy.LoadLedger(profile)
	idle := a.inputIdle
	if idle == nil {
		idle = hostInputIdle
	}
	if d, err := idle(ctx); err == nil {
		facts.InputIdle, facts.InputIdleKnown = d, true
	}
	decision := policy.Decide(policy.Request{Trigger: policy.SilentUpdate, Version: pending.Version}, facts)
	a.mu.Lock()
	a.waiting = &decision
	a.mu.Unlock()
	if !decision.Allowed() {
		return
	}
	next := &silentRequest{ID: randomToken()[:16], Version: pending.Version, DecidedAt: now, fingerprint: full.Fingerprint, publishedAt: pending.PublishedAt}
	if err := policy.WriteRecord(profile, silentRequestFile, next); err != nil {
		setWaiting(policy.Unreadable, "无法写入自动升级请求："+err.Error(), nil)
		return
	}
	a.mu.Lock()
	a.request = next
	a.waiting = &policy.Decision{Code: policy.Allowed, Message: "电脑空闲，正在自动升级到 " + pending.Version}
	a.mu.Unlock()
	diagnostics.Log("info", "app-update", "silent update requested for "+pending.Version)
}

func (s *Server) appUpdateConfigPathOrDefault() string {
	if s.appUpdateConfigPath != "" {
		return s.appUpdateConfigPath
	}
	return s.configPath
}
