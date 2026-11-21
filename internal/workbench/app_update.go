package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	DecidedAt time.Time `json:"decided_at"`
	// Release names the published files the decision was about; the shell
	// passes it to the download, which refuses any other files.
	Release     string `json:"release"`
	fingerprint string
	changedAt   string
}

// silentOutcomeFile is where the native shell reports how an unattended
// request ended before it reached the decision.
const silentOutcomeFile = "silent-outcome.json"

// appUpdateState is the workbench's part of application updates. Its mutex
// is never held while feeders run: feeders take Server.mu and other locks.
type appUpdateState struct {
	once       sync.Once
	registry   policy.Registry
	lastPhone  atomic.Int64
	lastRemote atomic.Int64

	// ledgerMu serializes every load-modify-save of attempts.json in this
	// process: the decision, the chain and the hourly tick all write it.
	ledgerMu sync.Mutex

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
	network     func() error
	unavailable func() string
	host        func(context.Context) (*policy.HostState, error)
	// stopRun is the update's stop job; nil selects s.stopEnvironment.
	stopRun func() error
	// Durations; zero selects the production value.
	stopTimeout     time.Duration
	decisionTimeout time.Duration
	installerWait   time.Duration
	installerPoll   time.Duration

	unavailableOnce   sync.Once
	unavailableReason string
}

// appUpdateStopTimeout bounds the stop an update makes, like the existing
// update handoff's stop. The macOS shell gives prepare-update 180 seconds.
const appUpdateStopTimeout = 2 * time.Minute

// heldOperation stands in for Server.op when the caller already holds it:
// the update's stop runs as an ordinary stop job inside the decision's
// operation, so nothing can start between the decision and the stop.
type heldOperation struct{}

func (heldOperation) Lock()   {}
func (heldOperation) Unlock() {}

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

// appUpdateNetworkRun is the existing 检查联网条件 action.
func (s *Server) appUpdateNetworkRun() func() error {
	if s.appUpdate.network != nil {
		return s.appUpdate.network
	}
	if s.macosGuest != nil {
		return s.checkMacOSNetwork
	}
	return s.runSafetyCheck
}

// appUpdateStop stops a running environment with the stop button's own
// steps, as a stop job inside the operation the caller holds. A stop that
// outlives its time is reported with ended, which closes when the stop job
// has finished: until then the caller keeps the operation, so that nothing
// runs beside the stop.
func (s *Server) appUpdateStop(running bool) (ended <-chan struct{}, err error) {
	a := &s.appUpdate
	if a.stop != nil {
		return nil, a.stop()
	}
	if !running {
		return nil, nil
	}
	if err := s.requireStorageReady(); err != nil {
		return nil, err
	}
	s.stopEnvironmentOwners()
	s.pauseNetworkForStop()
	run := a.stopRun
	if run == nil {
		run = s.stopEnvironment
	}
	s.mu.Lock()
	job, err := s.jobs.submit(heldOperation{}, "stop", s.safety.pauseVersion, s.cancelMonitorLocked, nil, run)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	timeout := a.stopTimeout
	if timeout == 0 {
		timeout = appUpdateStopTimeout
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), timeout)
	defer cancel()
	done, err := s.waitJob(ctx, job.ID)
	switch {
	case err != nil:
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			_, _ = s.waitJob(s.lifetimeContext(), job.ID)
		}()
		return stopped, errors.New("停止环境超时")
	case done.ID != job.ID || done.Canceled:
		return nil, errors.New("停止环境被中断")
	case done.Error != "":
		return nil, errors.New(done.Error)
	}
	return nil, nil
}

// releaseAfterStop gives back the operation the caller holds once a stop
// that outlived its time (ended) has finished, or at once when there is
// none, and then runs after. Nothing runs beside a stop that goes on.
func (s *Server) releaseAfterStop(ended <-chan struct{}, after func()) {
	go func() {
		if ended != nil {
			<-ended
		}
		s.op.Unlock()
		after()
	}()
}

// cancelMonitorLocked is the stop button's preparation, for jobs the update
// submits itself: the network monitor ends before the job runs.
func (s *Server) cancelMonitorLocked() error {
	if s.monitorCancel != nil {
		s.monitorCancel()
	}
	return nil
}

func (a *appUpdateState) duration(value, production time.Duration) time.Duration {
	if value != 0 {
		return value
	}
	return production
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

// appUpdateLocalRequired is the gateway's answer for a cloud connection;
// ui/src/app-update-state.mjs recognizes it.
const appUpdateLocalRequired = "应用升级由本机管理，请切换到本机后查看"

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

// plainLabel makes text from outside this process (the guest, a result
// file) fit for a dialog: printable characters only, no markup brackets,
// at most limit characters.
func plainLabel(text string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(text) {
		if n == limit {
			b.WriteString("…")
			break
		}
		switch {
		case r == '<' || r == '>':
			continue
		case unicode.IsSpace(r):
			r = ' '
		case !unicode.IsPrint(r):
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// noteRemoteRequest is called for every API request of a session that is
// neither the desktop's own view nor a paired phone: another computer.
func (s *Server) noteRemoteRequest() {
	s.appUpdate.lastRemote.Store(time.Now().UnixNano())
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
	// Computed once per process: the answer only changes with a new
	// installation, and on Windows the check creates a file.
	a := &s.appUpdate
	a.unavailableOnce.Do(func() {
		executable, err := os.Executable()
		if err != nil {
			a.unavailableReason = "无法确认安装位置，不支持静默更新"
			return
		}
		a.unavailableReason = silentUnavailableReason(runtime.GOOS, version.Version, executable, dirWritable)
	})
	return a.unavailableReason
}

// silentVerified is the real-machine gate (policy.SilentVerified); tests
// open it to check the rules behind it.
var silentVerified = policy.SilentVerified

func silentUnavailableReason(goos, current, executable string, writable func(string) bool) string {
	switch goos {
	case "linux":
		return "Linux 版本由系统安装包管理，不支持静默更新"
	case "windows":
		// The Windows shell does not follow silent requests yet; offering the
		// mode there would promise an update that never comes.
		return "Windows 版本暂不支持静默更新，可以点击升级"
	case "darwin":
	default:
		return "此平台不支持静默更新"
	}
	if !silentVerified(goos) {
		return policy.NotVerifiedReason
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
	if st.Chain != nil && st.Chain.Step == policy.StepFailed {
		running := s.vmRunning()
		s.mu.Lock()
		gate := s.safety.Gate
		networkOK := s.networkPaused || gate != nil && gate.Mode == cage.GateOpen
		current := !s.guestUpdateRequired
		s.mu.Unlock()
		st.Environment = &policy.EnvironmentNow{Running: running, NetworkOK: running && networkOK, ComponentsCurrent: running && current}
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
	// Admission closes (closing) only after a successful stop, so a refusal
	// clears the seal alone and never reopens a shutdown begun meanwhile.
	// The deferred release also covers a panic on the way.
	sealed := true
	unseal := func() {
		if !sealed {
			return
		}
		sealed = false
		s.mu.Lock()
		s.appUpdateSealed = false
		s.mu.Unlock()
	}
	defer unseal()
	a := &s.appUpdate
	profile := s.updateProfile()
	decisionTime := a.duration(a.decisionTimeout, 2*appUpdateGuestTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), decisionTime)
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
			if pending == nil {
				// A request file no process stands behind any more.
				_ = os.Remove(filepath.Join(policy.Dir(profile), silentRequestFile))
			}
			appUpdateRefuse(w, policy.Changed, "自动升级请求已过期，等待下一次空闲", nil)
			return
		}
		// The shell reached the decision with this request: its download and
		// verification worked, whatever the answer is now.
		_ = s.changeLedger(func(l *policy.Ledger) bool {
			return l.Report(pending.ID, policy.StageDecided, "", now)
		})
		request.Fingerprint, facts.PublishedAt, facts.Hold = pending.fingerprint, pending.changedAt, hold
		facts.InputIdle, facts.InputIdleKnown = s.readInputIdle(ctx)
		facts.Host = s.readHostConditions(ctx)
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
	s.mu.Lock()
	paused := s.networkPaused
	s.mu.Unlock()
	environment := policy.Environment{Running: s.vmRunning(), NetworkPaused: paused, Workspace: s.workspace(), Terminals: tabs}
	// Look once more at the work the seal does not cover (a viewer, a phone,
	// a transfer already admitted). Anything new since the decision wins.
	// The look has its own time: a slow decision read cannot starve it.
	finalCtx, finalCancel := context.WithTimeout(context.Background(), decisionTime)
	final := s.updateRegistry().Snapshot(finalCtx, a.now(), false)
	finalCancel()
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
	chain := &policy.Chain{Schema: policy.ChainSchema, ID: randomToken()[:16], From: version.Version, To: input.Version, Trigger: trigger,
		Step: policy.StepStopping, Environment: environment, StartedAt: now, UpdatedAt: now}
	// The attempt and the record are durable before anything stops: a crash
	// from here on reads as a failed attempt that silent mode never retries.
	// An unreadable history is kept aside, never replaced.
	a.ledgerMu.Lock()
	ledger, err := policy.LoadLedger(profile)
	if err != nil {
		ledger, err = policy.SetLedgerAside(profile, now)
	}
	if err == nil {
		ledger.Begin(policy.Attempt{ID: chain.ID, Version: input.Version, From: version.Version, Trigger: trigger, StartedAt: now})
		err = errors.Join(policy.SaveLedger(profile, ledger), policy.SaveChain(profile, chain))
	}
	a.ledgerMu.Unlock()
	if err != nil {
		unseal()
		appUpdateRefuse(w, policy.Unreadable, "无法保存升级记录，未停止环境："+err.Error(), nil)
		return
	}
	ended, stopErr := s.appUpdateStop(environment.Running)
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
	if stopErr == nil && r.Context().Err() != nil {
		// The shell gave up waiting: nobody would install, so do not exit.
		stopErr = errors.New("升级请求已取消")
	}
	if stopErr == nil {
		// Installing is recorded before the shell may install: a record left
		// at stopping therefore proves that nothing was installed.
		_ = chain.Advance(policy.StepInstalling, a.now())
		if err = policy.SaveChain(profile, chain); err != nil {
			stopErr = errors.New("无法保存升级记录：" + err.Error())
		}
	}
	if stopErr != nil {
		reason := "环境未能安全停止，未安装更新：" + stopErr.Error()
		chain.Fail(policy.StepStopping, reason, a.now())
		_ = policy.SaveChain(profile, chain)
		s.finishAttempt(chain.ID, input.Version, false, policy.StepStopping, reason)
		unseal()
		locked = false
		s.releaseAfterStop(ended, func() {
			if environment.Running {
				s.restoreEnvironmentAfterUpdate(chain.ID, environment)
			}
		})
		appUpdateRefuse(w, policy.StopFailed, reason, nil)
		return
	}
	// Admission closes only now, so that the window could follow the stop.
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	sealed = false // closing now refuses new work; the seal stays as it is.
	diagnostics.Log("info", "app-update", "installing "+input.Version+" ("+string(trigger)+")")
	// The attempt id travels with the installation so that the installer's
	// result can be matched to this record. A fixed length keeps the reply
	// complete even if shutdown cancels the request right after it.
	body, _ := json.Marshal(AppUpdatePrepared{Status: "prepared", Attempt: chain.ID})
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
	_ = http.NewResponseController(w).Flush()
	s.op.Unlock()
	locked = false
	go s.shutdown()
}

// readInputIdle is one read of the host's time without keyboard or mouse
// input; known is false when it cannot be read.
func (s *Server) readInputIdle(ctx context.Context) (idle time.Duration, known bool) {
	read := s.appUpdate.inputIdle
	if read == nil {
		read = hostInputIdle
	}
	if d, err := read(ctx); err == nil {
		return d, true
	}
	return 0, false
}

// readHostConditions is one read of the host's power and display state; nil
// when any part of it cannot be read.
func (s *Server) readHostConditions(ctx context.Context) *policy.HostState {
	read := s.appUpdate.host
	if read == nil {
		read = hostConditions
	}
	state, err := read(ctx)
	if err != nil {
		return nil
	}
	return state
}

// changeLedger is the one way this process changes attempts.json. change
// reports whether it changed the history. An unreadable history is left
// alone: only a decision sets it aside.
func (s *Server) changeLedger(change func(*policy.Ledger) bool) error {
	a := &s.appUpdate
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	profile := s.updateProfile()
	ledger, err := policy.LoadLedger(profile)
	if err != nil {
		return err
	}
	if !change(&ledger) {
		return nil
	}
	return policy.SaveLedger(profile, ledger)
}

// restoreEnvironmentAfterUpdate starts the environment again after an update
// that stopped it and then did not happen.
func (s *Server) restoreEnvironmentAfterUpdate(id string, environment policy.Environment) {
	ctx := s.lifetimeContext()
	if err := s.waitAccountUnlocked(ctx); err != nil {
		return
	}
	if _, err := s.appUpdateRunJob(ctx, "start", s.appUpdateStartRun(), environment.NetworkPaused); err != nil {
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

// appUpdateSubmit submits a chain step. A start keeps a network pause the
// person set, before the update or at any time since; otherwise it resumes
// the network exactly like the start button does. The network check is
// skipped (skipped is true) while the network is paused. The pause is read
// under the lock that submits, so a pause set a moment earlier is kept.
func (s *Server) appUpdateSubmit(kind string, run func() error, keepPause bool) (job Job, skipped bool, err error) {
	s.mu.Lock()
	keep := keepPause || s.networkPaused
	if kind == "safety" && keep {
		s.mu.Unlock()
		return Job{}, true, nil
	}
	if !keep {
		job, intentFailed, err := s.submitJobLocked(kind, run)
		s.mu.Unlock()
		if intentFailed {
			_ = s.closeProxyGate("网络恢复意图无法保存，外网保持关闭")
		}
		return job, false, err
	}
	defer s.mu.Unlock()
	switch {
	case s.closing:
		return Job{}, false, errJobsClosed
	case s.appUpdateSealed:
		return Job{}, false, errAppUpdateSealed
	case s.storageRecoveryError != "":
		return Job{}, false, fmt.Errorf("环境存储待恢复，请在设置中恢复后继续。%s", s.storageRecoveryError)
	}
	job, err = s.jobs.submit(&s.op, kind, s.safety.pauseVersion, s.cancelMonitorLocked, nil, run)
	return job, false, err
}

// appUpdateRunJob submits a job as the chain's own step and waits for it. A
// job the person started first is waited for, then the step is decided again.
// A skipped step returns an empty job.
func (s *Server) appUpdateRunJob(ctx context.Context, kind string, run func() error, keepPause bool) (Job, error) {
	for attempt := 0; attempt < 3; attempt++ {
		job, skipped, err := s.appUpdateSubmit(kind, run, keepPause)
		if skipped {
			return Job{}, nil
		}
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
// chain the previous version began once the installer has confirmed it,
// records an installation the installer rolled back and brings the
// environment back, or retires a record it cannot prove. A silent request
// file of an earlier process goes first: its fingerprint lived there.
func (s *Server) resumeAppUpdateChain() {
	a := &s.appUpdate
	a.chain.Lock()
	defer a.chain.Unlock()
	profile := s.updateProfile()
	_ = os.Remove(filepath.Join(policy.Dir(profile), silentRequestFile))
	c, err := policy.LoadChain(profile)
	if err != nil {
		diagnostics.Log("warn", "app-update", "chain record unreadable: "+err.Error())
		return
	}
	if c == nil {
		return
	}
	ctx := s.lifetimeContext()
	// The new version waits for the installer's result naming this attempt,
	// which it writes once this version's window has started. The wait is a
	// file read per second, for at most a few minutes after an update.
	waitUntil := time.Now().Add(a.duration(a.installerWait, policy.InstallerWait))
	var resumption policy.Resumption
	for {
		resumption = policy.Resume(c, version.Version, a.now(), readInstallerResult(profile))
		if !resumption.Wait {
			break
		}
		if !time.Now().Before(waitUntil) {
			resumption = policy.Resumption{Retire: "新版本没有收到安装程序的完成确认，没有据此恢复环境"}
			break
		}
		a.mu.Lock()
		a.note = "正在等待安装程序确认新版本已启动"
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.duration(a.installerPoll, time.Second)):
		}
	}
	a.mu.Lock()
	a.note = resumption.Hold
	a.mu.Unlock()
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
	case resumption.Retire != "":
		c.Retire(resumption.Retire, now)
		_ = policy.SaveChain(profile, c)
		s.finishAttempt(c.ID, c.To, false, c.FailedStep, resumption.Retire)
		diagnostics.Log("warn", "app-update", "update record "+c.ID+" retired: "+resumption.Retire)
		return
	case resumption.Hold != "":
		diagnostics.Log("warn", "app-update", "update "+c.ID+" left as it is: "+resumption.Hold)
		return
	case resumption.Undone:
		reason := "升级到 " + c.To + " 后又回到了 " + c.From + "，不会再自动安装 " + c.To + "；可以点击升级"
		c.Fail(c.Step, reason, now)
		_ = policy.SaveChain(profile, c)
		_ = s.changeLedger(func(l *policy.Ledger) bool { return l.Undo(c.ID, c.To, reason, now) })
		diagnostics.Log("warn", "app-update", "update "+c.ID+" to "+c.To+" was rolled back")
		return
	case resumption.RolledBack:
		reason := "安装没有完成，已恢复原应用"
		if result := readInstallerResult(profile); result != nil && strings.TrimSpace(result.Message) != "" {
			reason = plainLabel(result.Message, 200)
		}
		c.Fail(policy.StepInstalling, reason, now)
		_ = policy.SaveChain(profile, c)
		s.finishAttempt(c.ID, c.To, false, policy.StepInstalling, reason)
		if c.Environment.Running {
			s.restoreEnvironmentAfterUpdate(c.ID, c.Environment)
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

// readInstallerResult reads the installer's last-result.json; nil when
// there is none or it cannot be read.
func readInstallerResult(profile string) *policy.InstallerResult {
	path := filepath.Join(policy.Dir(profile), "last-result.json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result policy.InstallerResult
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	result.WrittenAt = info.ModTime()
	return &result
}

func (s *Server) finishAttempt(id, version string, ok bool, step policy.Step, reason string) {
	_ = s.changeLedger(func(l *policy.Ledger) bool {
		return l.Finish(id, version, ok, step, reason, s.appUpdate.now())
	})
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
		s.finishAttempt(c.ID, c.To, false, step, reason)
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
				job, err := s.appUpdateRunJob(ctx, "start", s.appUpdateStartRun(), c.Environment.NetworkPaused)
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
				job, err := s.appUpdateRunJob(ctx, "environment-update", s.appUpdateSyncRun(), c.Environment.NetworkPaused)
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
			// A network the person paused, before the update or since, stays
			// paused. Otherwise run 检查联网条件 once, like the button would.
			job, err := s.appUpdateRunJob(ctx, "safety", s.appUpdateNetworkRun(), c.Environment.NetworkPaused)
			if err != nil || job.Error != "" {
				reason := job.Error
				if err != nil {
					reason = err.Error()
				}
				fail(policy.StepNetwork, "已升级到 "+c.To+"，环境已恢复，但联网检查未通过："+reason)
				return
			}
			if !save(policy.StepDone) {
				return
			}
		case policy.StepDone:
			s.finishAttempt(c.ID, c.To, true, policy.StepDone, "")
			diagnostics.Log("info", "app-update", "updated to "+c.To)
			return
		default:
			return
		}
	}
}

// consumeSilentOutcome records what the native shell reported about an
// unattended request that ended before the decision, then removes the
// report. Only a request this workbench handed out can be reported on.
func (s *Server) consumeSilentOutcome(profile string, now time.Time) {
	path := filepath.Join(policy.Dir(profile), silentOutcomeFile)
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	defer os.Remove(path)
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var outcome struct {
		Request string `json:"request"`
		Stage   string `json:"stage"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &outcome) != nil || outcome.Request == "" {
		return
	}
	stage := policy.StageDownload
	switch {
	case outcome.Code == "changed" || outcome.Code == "too_new":
		stage = policy.StageChanged
	case outcome.Stage == "verify" || outcome.Code == "verification":
		stage = policy.StageVerify
	case outcome.Stage == "prepare":
		stage = policy.StagePrepare
	}
	_ = s.changeLedger(func(l *policy.Ledger) bool {
		return l.Report(outcome.Request, stage, plainLabel(outcome.Message, 200), now)
	})
	a := &s.appUpdate
	a.mu.Lock()
	if a.request != nil && a.request.ID == outcome.Request {
		a.request = nil
	}
	if stage == policy.StageChanged {
		// The release moved under the request: look at it again.
		a.checked = time.Time{}
	}
	a.mu.Unlock()
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
		s.dropSilentRequest()
		return
	}
	now := a.now()
	profile := s.updateProfile()
	s.consumeSilentOutcome(profile, now)
	a.mu.Lock()
	pending, checked, request := a.pending, a.checked, a.request
	a.mu.Unlock()
	if checked.IsZero() || now.Sub(checked) >= policy.CheckEvery {
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
			pending = &policy.Pending{Version: available.Version, PublishedAt: available.PublishedAt, ChangedAt: available.ChangedAt,
				Release: available.Release, Installable: available.Installable, Reason: available.Reason}
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
		if request.Version == pending.Version && request.Release == pending.Release && now.Sub(request.DecidedAt) < silentRequestLifetime {
			return
		}
		s.dropSilentRequest()
	}
	// A request that ended before the decision is retried on a schedule,
	// and files that failed verification are never fetched again unattended.
	if ledger, err := policy.LoadLedger(profile); err == nil {
		next, ok := ledger.SilentRetry(pending.Version, pending.Release, now)
		if !ok {
			setWaiting(policy.FailedBefore, pending.Version+" 的自动升级没有完成，不会再自动下载；可以点击升级", nil)
			return
		}
		if now.Before(next) {
			setWaiting(policy.RetryLater, "上次自动升级没有完成，稍后再试", nil)
			return
		}
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
	if !hold.Held(now, snapshot.Fingerprint) {
		setWaiting(policy.NotHeld, "等待电脑持续空闲", nil)
		return
	}
	full := s.updateRegistry().Snapshot(ctx, now, true)
	facts := policy.Facts{Current: version.Version, Snapshot: full, Hold: hold, PublishedAt: pending.ChangedAt, Now: now, SilentUnavailable: ""}
	facts.Settings, facts.SettingsErr = policy.LoadSettings(profile)
	facts.Ledger, facts.LedgerErr = policy.LoadLedger(profile)
	facts.InputIdle, facts.InputIdleKnown = s.readInputIdle(ctx)
	facts.Host = s.readHostConditions(ctx)
	decision := policy.Decide(policy.Request{Trigger: policy.SilentUpdate, Version: pending.Version}, facts)
	a.mu.Lock()
	a.waiting = &decision
	a.mu.Unlock()
	if !decision.Allowed() {
		return
	}
	next := &silentRequest{ID: randomToken()[:16], Version: pending.Version, DecidedAt: now, Release: pending.Release,
		fingerprint: full.Fingerprint, changedAt: pending.ChangedAt}
	// The request counts against the retry schedule from now on, whether or
	// not the shell ever reports on it.
	err := s.changeLedger(func(l *policy.Ledger) bool {
		l.Request(next.ID, next.Version, version.Version, next.Release, now)
		return true
	})
	if err == nil {
		err = policy.WriteRecord(profile, silentRequestFile, next)
	}
	if err != nil {
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
