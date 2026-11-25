package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/hostimage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

var loadConfig = cage.Load
var defaultConfig = cage.DefaultConfig

type guestCredentials struct {
	Token        string  `json:"token"`
	VNCPassword  string  `json:"-"`
	DisplayScale float64 `json:"display_scale"`
	DynamicScale bool    `json:"dynamic_scale"`
}

func (s *Server) workspaceLocked() string {
	if s.clientWorkspace != "" {
		return s.clientWorkspace
	}
	return s.preferences.Workspace
}
func (s *Server) workspace() string      { s.mu.Lock(); defer s.mu.Unlock(); return s.workspaceLocked() }
func (s *Server) environmentDir() string { return environmentWorkspace(s.profile) }
func (s *Server) sshTarget() string {
	if s.macosGuest != nil {
		return "cage-macos"
	}
	if containerenv.Configured(s.profile) {
		return "cage-container"
	}
	return "lima-" + cage.InstanceName(s.profile, s.environmentDir())
}

func (s *Server) environmentPrepared() bool {
	if s.windowsGuest != nil {
		return true
	}
	if containerenv.Configured(s.profile) {
		environment, err := containerenv.Load(s.profile)
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), 3*time.Second)
		defer cancel()
		state, err := environment.State(ctx)
		return err == nil && state.Exists
	}
	_, err := os.Stat(filepath.Join(s.profile, "state", ".agent-vm-base-version"))
	return err == nil
}
func (s *Server) sshCommand(ctx context.Context, args ...string) *exec.Cmd {
	return s.sshCommandOptions(ctx, false, false, args...)
}
func (s *Server) sshCommandWithForwarding(ctx context.Context, args ...string) *exec.Cmd {
	return s.sshCommandOptions(ctx, true, false, args...)
}
func (s *Server) sshLocalForwardCommand(ctx context.Context, localAddr string, guestPort int) *exec.Cmd {
	spec := fmt.Sprintf("%s:127.0.0.1:%d", localAddr, guestPort)
	return s.sshCommandOptions(
		ctx,
		true,
		true,
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "LocalCommand=echo 1",
		"-L", spec,
		s.sshTarget(),
	)
}
func (s *Server) sshCommandOptions(ctx context.Context, maintenance bool, allowLocalCommand bool, args ...string) *exec.Cmd {
	if s.windowsGuest != nil {
		return &exec.Cmd{Err: errors.New("Windows 客机不支持 Linux SSH 控制脚本")}
	}
	if s.macosGuest != nil {
		return s.macOSSSHCommand(ctx, args...)
	}
	cfg := filepath.Join(s.instanceDir(), "ssh.config")
	base := []string{"-q", "-e", "none", "-F", cfg, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "PermitLocalCommand=no", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no", "-o", "ProxyCommand=none", "-o", "ProxyJump=none", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "-o", "ForwardAgent=no", "-o", "IdentityAgent=none", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "ControlPath=none"}
	if maintenance || allowLocalCommand {
		for index, value := range base {
			if maintenance && value == "ClearAllForwardings=yes" {
				base[index] = "ClearAllForwardings=no"
			}
			if allowLocalCommand && value == "PermitLocalCommand=no" {
				base[index] = "PermitLocalCommand=yes"
			}
		}
	}
	base = append(base, args...)
	cmd := exec.CommandContext(ctx, sshExecutable(), base...)
	cmd.Env = sshEnvironment()
	host.HideCommandWindow(cmd)
	return cmd
}
func (s *Server) guest(ctx context.Context, script string) ([]byte, error) {
	return s.guestControl(ctx, script, "")
}

func (s *Server) guestControl(ctx context.Context, script, diagnosticSource string) ([]byte, error) {
	if s.windowsGuest != nil {
		return nil, errors.New("Windows 客机不支持 Linux 控制脚本")
	}
	// SSH enters the user's default shell. Run control scripts in Bash so
	// zsh initialization cannot change their redirection or error behavior.
	command := "exec /bin/bash --noprofile --norc -c " + shellescape.Quote(script)
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	var stderr fileControlStderr
	if diagnosticSource != "" {
		cmd.Stderr = &stderr
		defer func() {
			if len(stderr.data) > 0 {
				_ = diagnostics.New(s.profile).Record("warn", diagnosticSource, "guest control stderr: "+string(stderr.data))
			}
		}()
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, (64<<10)+1))
	if readErr != nil || len(data) > 64<<10 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("guest control response exceeded its limit")
	}
	if err = cmd.Wait(); err != nil {
		return nil, err
	}
	return data, nil
}
func (s *Server) attach() error {
	if err := s.requireStorageReady(); err != nil {
		return err
	}
	if s.windowsGuest != nil {
		return s.attachWindowsGuest()
	}
	if s.macosGuest != nil {
		return s.attachMacOSGuest()
	}
	if err := s.requireCurrentLinuxTransferMount(); err != nil {
		return err
	}
	ctx, cancel := s.controlContext()
	defer cancel()
	if containerenv.Configured(s.profile) {
		environment, err := containerenv.Load(s.profile)
		if err != nil {
			return err
		}
		// A reused state could still show the mounts of a start in progress,
		// and this decides whether to close the gate.
		state, err := environment.FreshState(ctx)
		if err != nil {
			return err
		}
		if !state.Running {
			return errors.New("环境尚未启动")
		}
		if !state.Ready {
			s.mu.Lock()
			s.ready = false
			s.mu.Unlock()
			if !state.MountsReady {
				failure := &projectShareError{Recovery: "environment-restart", Message: "容器文件共享需要重新连接，请保存工作后完整停止并重新启动环境；文件和登录状态会保留。"}
				_ = s.closeProxyGate(failure.Message)
				return failure
			}
			if !state.ResourcesVerified {
				return errors.New("容器资源限制尚未通过核验，请检查服务器资源配置")
			}
			return errors.New("容器网络与文件连接尚未就绪，请重试启动")
		}
	} else if !s.vmRunning() {
		return errors.New("环境尚未启动")
	}
	// The shared control connection saves an SSH login per attach; it falls
	// back to a connection of its own when none can be opened.
	b, err := s.guestQuick(ctx, guestConnectionSnapshot)
	if err != nil {
		return errors.New("环境尚未准备好")
	}
	credentials, manifest, presence, err := parseGuestConnection(b)
	s.recordClientPresence(presence)
	s.mu.Lock()
	s.guestManifest = manifest
	s.guestUpdateRequired = errors.Is(err, errGuestUpdateRequired)
	s.guestUpdateError = ""
	if err != nil {
		s.ready = false
		s.guestUpdateError = err.Error()
	}
	s.mu.Unlock()
	if err != nil {
		if errors.Is(err, errGuestUpdateRequired) {
			_ = s.closeProxyGate("环境组件需要更新，外网保持关闭")
		}
		return err
	}
	s.mu.Lock()
	home := browserHomeURL(s.preferences)
	s.mu.Unlock()
	// An accepted homepage proves what a separate /health request would: the
	// browser agent answers with these credentials. One round trip fewer.
	if err = s.setBrowserHome(ctx, credentials.Token, home); err != nil {
		return err
	}
	if containerenv.Configured(s.profile) {
		if err := s.recoverClientMounts(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.credentials = credentials
	s.ready = true
	s.mu.Unlock()
	s.restoreProjectShares()
	s.desktopViews.background(s.changeDesktopVisibility)
	// A browser controller that restarted since (a crash, or a reset the host
	// stopped following) starts with its window shown: tell it again.
	s.browserViews.reset()
	s.browserViews.background(s.changeBrowserVisibility)
	return nil
}

// environmentMonitor carries the environment monitor's state between rounds.
type environmentMonitor struct {
	skipped int
	// failures counts consecutive failed health probes of a ready Linux
	// guest. One failure is tolerated: a single slow or lost reply would
	// otherwise unmount the browser until a full attach rebuilds it. The
	// count restarts when a job ran or the guest credentials changed, so two
	// failures always belong to the same connection.
	failures   int
	job, token string
}

func (s *Server) environmentMonitorIdle() bool {
	// Call vmRunning before s.mu.Lock to avoid nested lock on Windows guests.
	running := s.vmRunning()
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	return !ready && !running && !s.busy()
}

func (s *Server) monitor(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var state environmentMonitor
	for ctx.Err() == nil {
		wake := s.monitorWakeCh()
		if s.environmentMonitorIdle() {
			state.failures, state.skipped = 0, 0
			select {
			case <-ctx.Done():
				return
			case <-wake:
				continue
			}
		}
		s.monitorEnvironmentRound(&state)
		if !waitMonitor(ctx, s.monitorWakeCh(), timer, 5*time.Second) {
			return
		}
	}
}

// monitorEnvironmentRound runs one round of the environment monitor.
//
// The health probe of a ready guest is read-only and runs without the
// operation lock: user actions take that lock with TryLock and are refused
// while it is held, so holding it across guest round trips every five
// seconds refused clicks. The lock is still taken to change the environment:
// to attach, and for the instant it takes to mark the environment not ready.
func (s *Server) monitorEnvironmentRound(m *environmentMonitor) {
	timing := s.startMonitorTiming()
	defer func() {
		timing.stages = append(timing.stages, fmt.Sprintf("consecutive_lock_skips=%d", m.skipped))
		s.recordMonitorTiming("environment", timing)
	}()
	if s.busy() {
		// A job owns the lifecycle and reattaches itself when it is done.
		m.skipped, m.failures = 0, 0
		timing.result = "job_running"
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	switch {
	case s.windowsGuest != nil:
		if ready {
			observed := s.observeWindowsGuest(s.lifetimeContext())
			timing.stage("environment_health")
			if observed == observedSteady {
				m.skipped = 0
				return
			}
		}
		s.lockedEnvironmentRound(m, timing, true, false)
	case s.macosGuest != nil:
		s.monitorMacOSRound(m, timing, ready)
	case ready:
		s.monitorLinuxHealth(m, timing, token)
	default:
		m.failures = 0
		s.lockedEnvironmentRound(m, timing, true, false)
	}
}

// lockedEnvironmentRound attaches and/or runs the network tick under the
// operation lock. It reports whether it ran.
func (s *Server) lockedEnvironmentRound(m *environmentMonitor, timing *monitorTiming, attach, network bool) bool {
	if !s.op.TryLock() {
		m.skipped++
		timing.result = "operation_lock_busy"
		return false
	}
	defer s.op.Unlock()
	m.skipped = 0
	if s.busy() {
		timing.result = "job_running"
		return false
	}
	if attach {
		_ = s.attach()
		timing.stage("environment_health")
	}
	if network {
		s.monitorNetworkTick()
		timing.stage("network_tick")
	}
	return true
}

// monitorMacOSRound looks at a ready virtual Mac and its network inputs
// without the operation lock, and takes the lock only when attach or the
// network tick has something to do. A failed guest probe is tolerated once;
// the second attaches under the lock, which marks the guest not ready if
// its own probe fails too. Gate and VM changes attach at once.
func (s *Server) monitorMacOSRound(m *environmentMonitor, timing *monitorTiming, ready bool) {
	attach := !ready
	if ready {
		if job := s.jobs.snapshot().ID; job != m.job {
			m.failures, m.job = 0, job
		}
		ctx, cancel := s.controlContext()
		observed := s.observeMacOSGuest(ctx)
		cancel()
		timing.stage("environment_health")
		switch observed {
		case observedSteady:
			m.failures = 0
		case observedUnreachable:
			m.failures++
			attach = m.failures >= 2
			if !attach {
				timing.result = "health_failed_once"
			}
		default:
			attach = true
		}
	} else {
		m.failures = 0
	}
	networkTiming := s.startMonitorTiming()
	network := s.networkRoundWanted(networkTiming)
	if !network {
		s.recordMonitorTiming("network", networkTiming)
	}
	if !attach && !network {
		m.skipped = 0
		return
	}
	if s.lockedEnvironmentRound(m, timing, attach, network) && attach {
		m.failures = 0
	}
}

// monitorLinuxHealth probes a ready Linux guest without the operation lock.
// The second consecutive failure marks the environment not ready; the next
// round then reattaches under the lock as before.
func (s *Server) monitorLinuxHealth(m *environmentMonitor, timing *monitorTiming, token string) {
	// Lifecycle changes that make the guest ready again are jobs (start,
	// restart, settings, safety and component updates all attach inside a job)
	// or this monitor's own attach. A job submitted while the probe was in
	// flight has a new ID, so its outcome is never overwritten by a stale
	// failure.
	job := s.jobs.snapshot().ID
	if job != m.job || token != m.token {
		m.failures, m.job, m.token = 0, job, token
	}
	ctx, cancel := s.controlContext()
	_, err := s.guestAPI(ctx, token, "GET", "/health", nil)
	cancel()
	timing.stage("environment_health")
	if err == nil {
		m.failures, m.skipped = 0, 0
		return
	}
	m.failures++
	if m.failures < 2 {
		m.skipped = 0
		timing.result = "health_failed_once"
		return
	}
	if !s.op.TryLock() {
		// Keep the count: the next failed probe tries again.
		m.skipped++
		timing.result = "operation_lock_busy"
		return
	}
	defer s.op.Unlock()
	m.skipped = 0
	current := s.jobs.snapshot()
	s.mu.Lock()
	stale := current.Running || current.ID != job || !s.ready || s.credentials.Token != token
	if !stale {
		s.ready = false
	}
	s.mu.Unlock()
	m.failures = 0
	if stale {
		timing.result = "health_failed_superseded"
		return
	}
	timing.result = "health_failed"
}

func (s *Server) runCage(args ...string) error {
	if len(args) > 0 && args[0] == "up" {
		if err := s.requireCurrentLinuxTransferMount(); err != nil {
			return err
		}
	}
	ctx := s.operationContext()
	// Setup and startup can both install dependencies. Downloads own their
	// inactivity timeouts; the operation context still permits an explicit stop.
	if len(args) != 1 || (args[0] != "setup" && args[0] != "up") {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, s.exe, args...)
	host.HideCommandWindow(cmd)
	cmd.Dir = s.environmentDir()
	if cmd.Dir == "" {
		cmd.Dir = s.profile
	} else if len(args) > 0 && args[0] == "stop" {
		if info, err := os.Stat(cmd.Dir); err != nil || !info.IsDir() {
			cmd.Dir = s.profile
		}
	}
	cmd.Env = append(os.Environ(), cage.ConfigEnv+"="+s.configPath)
	execution := s.jobs.execution()
	if len(args) == 1 && args[0] == "setup" {
		cmd.Env = append(cmd.Env, "CAGE_SETUP_PROGRESS=1")
	}
	cmd.Env = append(cmd.Env, cage.JobEnv+"="+execution.id)
	cmd.Stdout, cmd.Stderr = execution, execution
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return context.Canceled
		}
		action := map[string]string{"setup": "准备 Linux 环境", "up": "启动环境", "down": "停止环境", "stop": "停止环境"}[args[0]]
		if action == "" {
			action = "执行环境操作"
		}
		if p := s.jobs.snapshot().SetupProgress; p != nil && p.Label != "" {
			action = strings.TrimPrefix(p.Label, "正在")
		}
		// A start refused for memory says so in one sentence, before the
		// generic QEMU reason below would show the kernel's words.
		if reason := bootMemoryFailure(s.jobs.snapshot().Log); reason != "" {
			return errors.New(reason)
		}
		// CLI failures can include the original QEMU stderr before a generic
		// "Failed to start base VM" footer. Surface that reason in job.error.
		// The start's own failure puts it on a line of its own, after
		// "--- Lima's output ---", without the "Error: " prefix.
		for _, line := range strings.Split(s.jobs.snapshot().Log, "\n") {
			if _, reason, ok := strings.Cut(line, "QEMU 启动失败："); ok {
				return fmt.Errorf("%s失败：虚拟机未能启动。QEMU 启动失败：%s。请查看操作详情；仍无法启动时，在设置中导出诊断包。", action, reason)
			}
		}
		if packageErr := packageInstallFailure(action, errors.Join(ctx.Err(), err), s.jobs.snapshot().Log); packageErr != nil {
			return packageErr
		}
		diagnostics.Log("error", "environment-operation", fmt.Sprintf("operation=%s error=%v context=%v", args[0], err, ctx.Err()))
		return executionFailure(action, errors.Join(ctx.Err(), err), s.jobs.snapshot().Log)
	}
	return nil
}

// qemuMemorySentence explains a QEMU start the kernel refused memory for.
const qemuMemorySentence = cage.BootMemoryMarker + "：QEMU 没能为环境分到内存（cannot set up guest memory），这台机器现在的内存不够环境的内存上限；请稍后重试，或在设置中调低环境内存；在服务器上请管理员为这个工作区增加内存。"

// bootMemoryFailure is the sentence for a start refused for memory, "" for
// any other failure: the start's own (cage.PrepareBoot, which names the
// sizes), or QEMU's when the kernel refused the guest's memory.
func bootMemoryFailure(log string) string {
	lines := strings.Split(log, "\n")
	for _, line := range lines {
		if i := strings.Index(line, cage.BootMemoryMarker+"："); i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "cannot set up guest memory") || strings.Contains(lower, "no enough memory for the allocation") {
			return qemuMemorySentence
		}
	}
	return ""
}

// closeGuestSessions ends the long-lived connections to the guest, which a
// start, stop or restart leaves pointing at a guest that is gone, and the
// resident helper's back-off with them: the next guest is tried afresh. The
// last Claude Science state read goes too: it described the old guest.
// Without it a connection whose packets are lost stays up until ssh gives up
// (30 s), and requests on it time out and back the helper off for a minute.
func (s *Server) closeGuestSessions() {
	s.controlSession.close()
	s.guestScripts.reset()
	s.nativeControl.close()
	s.cliDiscovery.reset()
	s.cliDiscovery.resident.close()
	s.agentCLIs.resetGuestCaches()
	s.claudeAccounts.resetGuestCaches()
	s.codexAccounts.resetGuestCaches()
	s.scienceLast.set(nil)
}

func (s *Server) startEnvironment(syncTimezone bool) error {
	s.entryGuestStarting()
	s.desktopViews.reset()
	s.browserViews.reset()
	s.closeGuestSessions()
	s.desktopStatus.invalidate()
	defer s.desktopStatus.invalidate()
	if err := s.requireStorageReady(); err != nil {
		return err
	}
	if s.macosGuest != nil {
		return s.startMacOSGuest()
	}
	if s.windowsGuest != nil {
		return s.startWindowsGuest()
	}
	if err := s.requireCurrentLinuxTransferMount(); err != nil {
		return err
	}
	pending, err := guestos.LoadInstallation(s.profile)
	if err != nil || (pending != nil && !hostimage.IsLinux(pending.System)) {
		return errors.New("请从首次安装入口继续原生客户机安装")
	}
	linuxSystem, err := s.installedLinuxSystem()
	if err != nil {
		return err
	}
	if pending != nil {
		prepared, err := hostimage.Load(filepath.Join(s.profile, "state", "guest-media"))
		if err != nil || pending.Media == "" || prepared != pending.Source {
			return errors.New("请从首次安装入口完成本机系统材料准备")
		}
	}
	if _, err := validateWorkspace(s.environmentDir(), s.profile); err != nil {
		return err
	}
	if containerenv.Configured(s.profile) {
		environment, err := containerenv.Load(s.profile)
		if err != nil {
			return err
		}
		state, err := environment.FreshState(s.operationContext())
		if err != nil {
			return err
		}
		if state.Running && !state.MountsReady {
			s.mu.Lock()
			s.ready = false
			s.mu.Unlock()
			failure := &projectShareError{Recovery: "environment-restart", Message: "容器文件共享需要重新连接，请保存工作后完整停止并重新启动环境；文件和登录状态会保留。"}
			_ = s.closeProxyGate(failure.Message)
			return failure
		}
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	if err = cfg.RequireManagedProxy(s.profile); err != nil {
		_ = s.closeProxyGate(err.Error())
		return err
	}
	{
		p, e := managedproxy.Load(s.profile)
		if e != nil {
			return e
		}
		if p.Selected == "" || p.Endpoint() != cfg.ProxyURL || p.Revision != cfg.ProxyRevision {
			return errors.New("内置代理配置未应用，请重新选择节点")
		}
		name, e := physicalProxyInterface(s.operationContext())
		if e != nil {
			_ = s.closeProxyGate(e.Error())
			return e
		}
		bound, _ := managedproxy.BoundInterface(filepath.Join(managedproxy.Dir(s.profile), "config.yaml"))
		if bound != name || !managedproxy.Running(s.operationContext(), s.profile, p) {
			// Every core start changes the report generation, including recovery.
			if e = s.activateProxy(p, cfg); e != nil {
				return e
			}
			cfg, e = loadConfig(s.configPath)
			if e != nil {
				return e
			}
		}
	}

	attachErr := s.attach()
	if errors.Is(attachErr, errGuestUpdateRequired) {
		return s.synchronizeEnvironment()
	}
	if attachErr == nil {
		client, gateErr := s.gateClient()
		if gateErr == nil {
			state, e := client.Status(context.Background())
			if e == nil {
				if state.Mode == cage.GateOpen && state.Report != nil && state.Report.Authorizes(state.RunID, cfg.Fingerprint(), time.Now()) == nil {
					s.publishGate(state)
					return nil
				}
				return s.checkNetworkIfWanted()
			}
		}
		s.setMessage("正在重启环境以加载 VM 外网络门槛…")
		if err := s.stopEnvironment(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	preferences := s.preferences
	auto := preferences.AutoTimezone || preferences.AutoLanguage
	s.mu.Unlock()
	s.setMessage("正在验证代理出口…")
	loc, probeErr := probeEnvironmentLocation(s.operationContext(), cfg, preferences)
	if s.operationContext().Err() != nil {
		return s.operationContext().Err()
	}
	if err := s.finishLocationProbe(cfg, loc, probeErr); err != nil {
		return err
	}
	if probeErr != nil && !s.environmentPrepared() {
		return probeErr
	}
	if cfg.ExitPolicy.NeedsBinding() && loc.IP != "" && !s.vmRunning() {
		bound := cfg.ExitPolicy.BindMissing(loc.exitLocation())
		if bound != cfg.ExitPolicy {
			cfg.ExitPolicy = bound
			if err = s.saveExitPolicyConfig(cfg); err != nil {
				return err
			}
		}
	}
	if auto && syncTimezone && !s.vmRunning() && probeErr == nil {
		observed := loc.exitLocation()
		within, _ := cfg.ExitPolicy.Evaluate(loc.IP, &observed)
		updated := cfg
		if within == cage.CheckPassed {
			updated = autoRegionSettings(cfg, preferences, loc)
		}
		if updated != cfg {
			// Changing a live hostagent's profile is forbidden. Stop before replacing it.
			s.setMessage("正在应用出口所在地的时区与语言建议…")
			if err = s.stopEnvironment(); err != nil {
				return err
			}
			cfg = updated
			if err = writePrivate(s.configPath, cfg); err != nil {
				return err
			}
		}
	} else if probeErr != nil {
		s.setMessage("代理出口无法通过检查，正在启动离线环境…")
	}
	if _, err = os.Stat(filepath.Join(s.profile, "state", ".agent-vm-base-version")); !containerenv.Configured(s.profile) && os.IsNotExist(err) {
		s.setMessage("首次准备 Linux 环境与开发工具，可能需要数分钟…")
		job := s.jobs.execution()
		job.setSetupProgress(&SetupProgress{Phase: "image", System: linuxSystem, Label: "正在准备 " + guestProgressName(linuxSystem) + " 镜像"})
		if err = s.runCage("setup"); err != nil {
			return err
		}
		job.setSetupProgress(&SetupProgress{Phase: "services", Label: "正在启动浏览器和编辑器"})
		defer job.setSetupProgress(nil)
	}
	s.setMessage("正在启动浏览器、编辑器与共享文件…")
	if err = s.runCage("up"); err != nil {
		return err
	}
	if err = retryAttach(s.operationContext(), s.attach); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return errors.New("环境已启动，但浏览器连接尚未就绪。请重试连接或查看操作详情。")
	}
	return s.checkNetworkIfWanted()
}

// attachRetryBudget is the total wait between attach attempts after cage up,
// as with the former eight one-second pauses.
const attachRetryBudget = 8 * time.Second

// retryAttach retries attach after cage up. cage up has already waited for
// the guest services, so a retry usually succeeds at once: start at 100 ms
// and double to 1 s, instead of pausing a full second every time.
func retryAttach(ctx context.Context, attach func() error) error {
	delay, waited := 100*time.Millisecond, time.Duration(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := attach()
		if err == nil || waited >= attachRetryBudget {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		waited += delay
		delay = min(2*delay, time.Second)
	}
}
func (s *Server) stopEnvironment() (returned error) {
	s.desktopStatus.invalidate()
	defer s.desktopStatus.invalidate()
	s.closeGuestSessions()
	explicitStop := s.jobs.snapshot().Kind == "stop"
	defer func() {
		if explicitStop && returned == nil {
			returned = managedproxy.Stop(s.profile)
		}
	}()
	if s.windowsGuest != nil {
		return s.stopWindowsGuest()
	}
	if s.macosGuest != nil {
		return s.stopMacOSGuest()
	}
	if explicitStop {
		s.browserDataAutoClean("stop")
	}
	s.setMessage("正在停止 Super Cage 环境…")
	if (explicitStop || s.jobs.snapshot().Kind == "storage") && cage.InstanceJobID(filepath.Join(s.profile, "state"), "base-building") != "" {
		if err := s.runCage("stop", "--keep-proxy", "base-building"); err != nil {
			return err
		}
	}
	if client, err := s.gateClient(); err == nil {
		if state, e := client.Close(context.Background(), "环境正在停止"); e == nil {
			s.publishGate(state)
		}
	}
	s.mu.Lock()
	browser := s.browserControl
	s.browserControl = nil
	s.ready = false
	s.credentials = guestCredentials{}
	s.guestManifest = nil
	s.guestUpdateRequired = false
	s.macComponentsChecked = false
	s.guestUpdateError = ""
	s.safety.Error = ""
	s.mu.Unlock()
	if browser != nil {
		browser.close()
	}
	s.entryEnvironmentStopped()
	s.terminals.closeAll()
	s.mobileCLI.stopAll()
	s.closeT3CodeProxy()
	s.invalidateT3CodeStatus()
	s.closeMagpieProxy()
	s.invalidateMagpieStatus()
	s.invalidateHerdrStatus()
	s.invalidateAgentProbeCache()
	s.closeProjectShares()
	if _, err := os.Stat(filepath.Join(s.instanceDir(), "lima.yaml")); !containerenv.Configured(s.profile) && os.IsNotExist(err) {
		return nil
	}
	// Internal restarts keep the already validated core alive. Explicit stop
	// releases it above only after the environment has stopped successfully.
	if err := s.runCage("stop", "--keep-proxy"); err != nil {
		return err
	}
	return nil
}
func (s *Server) apply(v savedProfile, validate bool) error {
	old, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	// Network endpoints are generated by the node manager, including restore.
	v.Config.ProxyURL, v.Config.ProxyRevision = old.ProxyURL, old.ProxyRevision
	s.mu.Lock()
	oldPrefs := s.preferences
	resumeVersion := s.safety.pauseVersion
	if execution := s.jobs.execution(); execution.id != "" {
		resumeVersion = execution.pauseVersion
	}
	wasReady := vmAlive(s.profile, s.environmentDir())
	if s.windowsGuest != nil {
		wasReady = s.windowsState.Running
	} else if s.macosGuest != nil {
		wasReady = macOSInstanceRunning(s.instanceDir(), s.macosGuest.Backend)
	}
	s.mu.Unlock()
	changed := old != v.Config || oldPrefs.AutoTimezone != v.Preferences.AutoTimezone || oldPrefs.AutoLanguage != v.Preferences.AutoLanguage
	if changed && validate && old.RequireManagedProxy(s.profile) == nil {
		s.setMessage("正在验证新出口，现有环境保持运行…")
		loc, e := probeEnvironmentLocation(s.operationContext(), v.Config, v.Preferences)
		if err := s.finishLocationProbe(v.Config, loc, e); err != nil {
			return err
		}
		if e != nil {
			if v.Preferences.AutoTimezone || v.Preferences.AutoLanguage || loc.IP == "" {
				return e
			}
		} else {
			v.Config = autoRegionSettings(v.Config, v.Preferences, loc)
		}
		v.Config.ExitPolicy = v.Config.ExitPolicy.BindMissing(loc.exitLocation())
		observed := loc.exitLocation()
		if result, message := v.Config.ExitPolicy.Evaluate(loc.IP, &observed); result != cage.CheckPassed {
			if result == cage.CheckUnknown && e != nil && !v.Preferences.AutoTimezone && !v.Preferences.AutoLanguage {
				return errors.New("时区和语言已选择手动设置，但所有出口仍须确认国家或地区。请重试位置检测或切换节点；静态 IP 和不限地区不能跳过大陆出口检查，原配置保持不变")
			}
			return errors.New(message)
		}
	}
	if changed {
		if s.windowsGuest != nil && wasReady && (old.Timezone != v.Config.Timezone || old.Locale != v.Config.Locale) {
			if err := s.closeProxyGate("正在应用 Windows 系统设置，外网等待重新检查"); err != nil {
				return err
			}
			if err := s.applyWindowsGuestSettings(s.operationContext(), v.Config); err != nil {
				return err
			}
		}
		if err := s.resumeNetworkIntentAt(resumeVersion); err != nil {
			return err
		}
		if s.operationContext().Err() != nil {
			return s.operationContext().Err()
		}
		if err = writePrivate(filepath.Join(s.profile, "previous-profile.json"), savedProfile{old, oldPrefs}); err != nil {
			return err
		}
		if err = s.stopEnvironment(); err != nil {
			return err
		}
	}
	// A project is a live directory grant in the same VM, never a lifecycle change.
	if v.Preferences.Workspace != "" && s.vmRunning() {
		s.setMessage("正在连接项目文件夹…")
		if _, err = s.connectProject(v.Preferences.Workspace, true); err != nil {
			return err
		}
	}
	if v.Config.RequireManagedProxy(s.profile) == nil {
		err = s.saveExitPolicyConfig(v.Config)
	} else {
		err = writePrivate(s.configPath, v.Config)
	}
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(s.profile, "workbench.json"), v.Preferences); err != nil {
		_ = writePrivate(s.configPath, old)
		return err
	}
	s.mu.Lock()
	s.preferences = v.Preferences
	token, ready := s.credentials.Token, s.ready
	s.mu.Unlock()
	s.jobs.execution().settingsSaved()
	if !changed && ready && browserHomeURL(oldPrefs) != browserHomeURL(v.Preferences) {
		ctx, cancel := s.controlContext()
		defer cancel()
		if err = s.setBrowserHome(ctx, token, browserHomeURL(v.Preferences)); err != nil {
			return fmt.Errorf("设置已保存，但浏览器暂时未能应用默认网站，请恢复连接后重试：%w", err)
		}
	}
	if changed && wasReady {
		s.setMessage("正在重新连接客机…")
		if err = s.startEnvironment(false); err != nil {
			var network networkCheckError
			if errors.As(err, &network) {
				return fmt.Errorf("设置已保存，环境已启动。外网仍关闭，修复以下问题后可重新检测：%w", err)
			}
			return fmt.Errorf("设置已保存，环境尚未恢复就绪。请核对客机状态后重试启动：%w", err)
		}
	}
	return nil
}

// Interface recovery uses the ordinary cancellable job queue. Location changes
// remain governed by the saved exit policy and never replace its baseline.
// Runs with the operation lock held. A step added here that can act must
// also make networkRoundWanted report true.
func (s *Server) monitorNetworkTick() {
	timing := s.startMonitorTiming()
	defer s.recordMonitorTiming("network", timing)
	if s.storageRecoveryStatus().Required {
		timing.result = "storage_recovery_pending"
		return
	}
	if s.macSetupPending() {
		timing.result = "setup_pending"
		return
	}
	s.monitorProxyInterface()
	timing.stage("physical_interface")
	s.checkProxyHealth()
	timing.stage("proxy_health")
	s.monitorSafety()
	timing.stage("safety")
	s.recoverNetwork()
	timing.stage("recovery")
	s.refreshProxySubscriptions()
	timing.stage("subscriptions")
}

func (s *Server) monitorNetwork(ctx context.Context) {
	s.runNetworkMonitorWith(ctx, s.networkRoundWanted, s.monitorNetworkTick)
}

// Keep scheduling and lock ownership independent of the network probes so
// concurrency tests can exercise the real loop without external I/O.
func (s *Server) runNetworkMonitor(ctx context.Context, tick func()) {
	s.runNetworkMonitorWith(ctx, nil, tick)
}

func (s *Server) networkMonitorIdle() bool {
	// Call vmRunning before s.mu.Lock to avoid nested lock on Windows guests.
	running := s.vmRunning()
	s.mu.Lock()
	ready, lost, recovering, storageErr := s.ready, s.proxyNetworkLost, s.recovery.RunID != "", s.storageRecoveryError != ""
	s.mu.Unlock()
	if ready || running || lost || recovering || storageErr || s.busy() || s.storageRecoveryStatus().Required || s.macSetupPending() {
		return false
	}
	return true
}

// runNetworkMonitorWith runs a round every 5 s while the VM or network recovery
// is active. When the VM is stopped and no network work is pending (wanted set),
// it sleeps on monitorWake instead of spawning external interface probes every
// 5 s. wanted, when set, first reads the round's inputs without the operation
// lock and reports whether the locked tick could act; a steady round then leaves
// the lock to user actions, which are refused while it is held.
func (s *Server) runNetworkMonitorWith(ctx context.Context, wanted func(*monitorTiming) bool, tick func()) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	skipped := 0
	for ctx.Err() == nil {
		if wanted != nil {
			wake := s.monitorWakeCh()
			if s.networkMonitorIdle() {
				skipped = 0
				select {
				case <-ctx.Done():
					return
				case <-wake:
					continue
				}
			}
		}
		s.networkMonitorRound(&skipped, wanted, tick)
		if !waitMonitor(ctx, s.monitorWakeCh(), timer, 5*time.Second) {
			return
		}
	}
}

func (s *Server) networkMonitorRound(skipped *int, wanted func(*monitorTiming) bool, tick func()) {
	if wanted != nil {
		timing := s.startMonitorTiming()
		if !wanted(timing) {
			*skipped = 0
			s.recordMonitorTiming("network", timing)
			return
		}
	}
	s.mu.Lock()
	// Publish ownership with acquisition and give project requests
	// waiting on this round a chance before the next tick takes the lock.
	yielded := s.projectOperationWaiters > 0
	acquired := !yielded && s.op.TryLock()
	if acquired {
		s.networkMonitorDone = make(chan struct{})
	}
	s.mu.Unlock()
	if acquired {
		*skipped = 0
		tick()
		s.mu.Lock()
		s.op.Unlock()
		close(s.networkMonitorDone)
		s.networkMonitorDone = nil
		s.mu.Unlock()
	} else if yielded {
		*skipped = 0
		timing := s.startMonitorTiming()
		timing.result = "project_operations_waiting"
		s.recordMonitorTiming("network", timing)
	} else {
		*skipped++
		timing := s.startMonitorTiming()
		timing.result = "operation_lock_busy"
		timing.stages = append(timing.stages, fmt.Sprintf("consecutive_lock_skips=%d", *skipped))
		s.recordMonitorTiming("network", timing)
	}
}

// proxyCoreRunning is managedproxy.Running; tests replace it.
var proxyCoreRunning = managedproxy.Running

// networkRoundWanted reports whether the locked network round could act:
// close the gate, mark the proxy network lost or submit a job. It reads the
// same inputs as monitorNetworkTick without the operation lock, and any
// error, unknown or change answers true so that the locked round decides on
// its own fresh reading. Steady rounds record result=steady.
//
// Keep it in step with monitorNetworkTick: every step there that can act
// must make this report true in that case. monitorSafety is not needed here:
// it runs on its own without the operation lock.
func (s *Server) networkRoundWanted(timing *monitorTiming) bool {
	s.mu.Lock()
	lost, recovering := s.proxyNetworkLost, s.recovery.RunID != ""
	s.mu.Unlock()
	// The locked round returns early for these; its record says why.
	if lost || recovering || s.busy() || s.storageRecoveryStatus().Required || s.macSetupPending() {
		return true
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil || cfg.RequireManagedProxy(s.profile) != nil {
		return true
	}
	name, err := physicalProxyInterface(s.lifetimeContext())
	timing.stage("physical_interface")
	if err != nil {
		return true
	}
	bound, err := managedproxy.BoundInterface(filepath.Join(managedproxy.Dir(s.profile), "config.yaml"))
	if err != nil || bound != name {
		return true
	}
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		return true
	}
	if s.vmRunning() {
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), 2*time.Second)
		running := proxyCoreRunning(ctx, s.profile, p)
		cancel()
		timing.stage("proxy_health")
		if !running {
			return true
		}
	}
	// The condition refreshProxySubscriptions submits an update for.
	for _, src := range p.Sources {
		if src.URL != "" && src.Interval > 0 && time.Since(src.Updated) >= time.Duration(src.Interval)*time.Hour && time.Since(src.Attempted) > 15*time.Minute {
			return true
		}
	}
	timing.result = "steady"
	return false
}

func vmAlive(profile, workspace string) bool {
	if containerenv.Configured(profile) {
		environment, err := containerenv.Load(profile)
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		state, err := environment.State(ctx)
		return err == nil && state.Running
	}
	if workspace == "" {
		return false
	}
	b, e := os.ReadFile(filepath.Join(profile, "state", "lima", cage.InstanceName(profile, workspace), "cage-running.json"))
	if e != nil {
		return false
	}
	var record struct {
		PID     int    `json:"pid"`
		Started string `json:"started"`
	}
	if json.Unmarshal(b, &record) != nil || record.PID < 1 {
		return false
	}
	return processExists(record.PID) && cage.ProcessStartMatches(record.PID, record.Started)
}
func (s *Server) vmRunning() bool {
	if s.windowsGuest != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.windowsState.Running
	}
	if s.macosGuest != nil {
		return macOSInstanceRunning(s.instanceDir(), s.macosGuest.Backend)
	}
	return vmAlive(s.profile, s.environmentDir())
}
