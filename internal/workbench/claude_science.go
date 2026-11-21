package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

// Claude Science, phase 1: the vendor daemon runs in a Linux x86-64 guest on
// guest loopback, and its own web page opens as a tab of the guest browser
// pane. Sign-in is the vendor's documented flow inside that tab. No host
// process talks to the daemon or to Anthropic, and no daemon port is
// forwarded to the host. Phase 2 reuses this lifecycle unchanged.
//
// Contract for the workbench UI (all routes need the workbench session,
// writes need the workbench Origin, and a locked account gets 423; errors
// are plain Chinese text, never JSON):
//
//	GET /api/claude-science
//	  200 claudeScienceState (below). supported:false means the entry is
//	      unavailable here; unsupported_reason says why in Chinese and
//	      unsupported_code is one of:
//	        "architecture"  guest or host is not x86-64 (no linux-arm64 build)
//	        "guest_system"  macOS or Windows guest
//	        "libc"          musl guest
//	        "storage"       the guest home is on tmpfs, overlayfs or ramfs,
//	                        which the daemon refuses to keep its data on
//	        "bubblewrap"    the guest's bubblewrap is older than 0.8
//	                        (Ubuntu 22.04): the daemon cannot start its sandbox
//	      These answers need no running environment.
//	  409 "请先启动 Super Cage 环境" (x86-64 host, environment stopped)
//	  502 the guest could not be read
//
//	  Fields: installed/version describe the guest's binary; pinned_version
//	  is the build this Super Cage release installs. update_available means
//	  installed < pinned: POST update installs the pin. running is the
//	  systemd unit; ready means the daemon answers (then port and started_at
//	  are set). failed with failure_reason "oom-kill", "port_in_use" or
//	  "exit" describes the last start. operation is set while a Cage job of
//	  this feature runs ("install", "update", "start", "stop"); follow it
//	  in /api/status like any job. disk_free_mb and disk_required_mb are
//	  the guest home's free space and what install asks for (about 10 GB
//	  for the first start's environments).
//
//	  signed_in and update_required are tri-state: null means unknown.
//	  Phase 1 always reports null for both: the daemon tells them only to a
//	  bearer holder, and the Science page itself shows sign-in and update
//	  state. When update_required is true, required_version is the floor,
//	  update_hint is Chinese advice and update_via is "claude-science"
//	  (POST update fixes it) or "super-cage" (a newer Super Cage release is
//	  needed because the pinned build is below the floor).
//
//	POST /api/claude-science/{install|update|start|stop}, body {}
//	  200 Job (as /api/claude-desktop/*), kinds claude-science-<action>.
//	  install: downloads the pinned build (sha256-checked) and writes the
//	  service. update: stops the daemon, then install. start: writes the
//	  service and waits until the daemon answers. stop: stops it.
//	  400 unknown fields; 404 unknown action; 409 unsupported host or
//	  guest system (reason as in GET), environment stopped, or another job
//	  running. What only the guest knows (architecture, libc, storage,
//	  bubblewrap, free disk, a running daemon) fails the job before any
//	  change; the job error is the generic maintenance failure and the
//	  Chinese reason is the last line of the job log, as for Claude Desktop.
//
//	POST /api/claude-science/open, body {} or {"fresh": true}
//	  200 {"id": tab id, "kind": "science", "title": "Claude Science",
//	      "reused": bool}. The tab is in the guest browser pane: switch to
//	  it like any browser tab. A fresh single-use link is minted each time;
//	  an existing Science tab of the same daemon run that shows the app or is
//	  signing in is only brought forward (reused:true) unless fresh is set.
//	  The link never reaches the response, job logs or diagnostics.
//	  409 not installed, not running, a job of this feature is running, or
//	  the browser is not ready; 502 the guest or the browser agent failed.
type claudeScienceState struct {
	Supported         bool   `json:"supported"`
	UnsupportedCode   string `json:"unsupported_code,omitempty"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
	Architecture      string `json:"architecture,omitempty"`
	Installed         bool   `json:"installed"`
	Version           string `json:"version"`
	PinnedVersion     string `json:"pinned_version"`
	UpdateAvailable   bool   `json:"update_available"`
	UpdateRequired    *bool  `json:"update_required"`
	RequiredVersion   string `json:"required_version,omitempty"`
	UpdateVia         string `json:"update_via,omitempty"`
	UpdateHint        string `json:"update_hint,omitempty"`
	Running           bool   `json:"running"`
	Ready             bool   `json:"ready"`
	Failed            bool   `json:"failed"`
	FailureReason     string `json:"failure_reason,omitempty"`
	Port              int    `json:"port,omitempty"`
	StartedAt         string `json:"started_at,omitempty"`
	SignedIn          *bool  `json:"signed_in"`
	DiskFreeMB        int64  `json:"disk_free_mb,omitempty"`
	DiskRequiredMB    int64  `json:"disk_required_mb,omitempty"`
	Operation         string `json:"operation,omitempty"`
	// Phase 2 (science_host.go); absent on hosts and clients that cannot use it.
	HostRender *scienceHostRenderState `json:"host_render,omitempty"`
}

const (
	scienceReasonArchitecture = "Claude Science 暂无 Linux arm64 官方版本，只能在 x86-64 Linux 客机中使用。"
	scienceReasonGuestSystem  = "Claude Science 目前只能在 x86-64 Linux 客机中使用，macOS 与 Windows 客机暂不支持。"
	scienceReasonLibc         = "Claude Science 的 Linux 版本需要 glibc，这个客机使用 musl，无法安装。"
	scienceReasonStorage      = "这个客机的用户目录位于重启后会清空的临时文件系统（tmpfs、overlayfs 或 ramfs），Claude Science 拒绝把项目和会话保存在这里，因此无法安装。请使用带持久磁盘的环境。"
	scienceReasonBubblewrap   = "Claude Science 的沙箱需要 bubblewrap 0.8 或更高版本，这个客机系统自带的版本过旧（Ubuntu 22.04 即是如此）。请改用 Debian 13 客机。"
)

// The host's architecture is the local guest's (internal/hostimage maps
// amd64 to x86_64 and arm64 to aarch64). Tests replace it.
var scienceHostArch = runtime.GOARCH

// Guest facts, read as the guest user. Cheap enough for polling: one
// systemctl and one ss process. The 170 MB binary runs for "status" once per
// run of the unit, when its port first listens (kept by the unit's
// InvocationID), and for --version once per installed file (cached by size
// and mtime).
const claudeScienceSnapshot = `python3 - <<'CAGE_SCIENCE_STATE'
import json, os, re, shutil, subprocess
from pathlib import Path
home = Path.home()
binary = home / '.local/bin/claude-science'
def run(args, timeout=4):
    try:
        return subprocess.run(args, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL, env=dict(os.environ, DO_NOT_TRACK='1')).stdout.strip()
    except Exception:
        return ''
def version(text):
    match = re.search(r'(?:^|\s)v?([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)', text)
    return match.group(1) if match else ''
facts = {'architecture': run(['uname', '-m']), 'musl': Path('/lib/libc.musl-x86_64.so.1').exists(), 'home_fs': run(['stat', '-f', '-c', '%t', str(home)])}
facts['installed'] = binary.is_file() and os.access(binary, os.X_OK)
if not facts['installed']:
    bwrap = '/usr/bin/bwrap'
    if os.access(bwrap, os.X_OK):
        try:
            probe = subprocess.run([bwrap, '--unshare-user', '--disable-userns', '--dev-bind', '/', '/', '--', '/bin/true'], capture_output=True, text=True, timeout=5)
            facts['bubblewrap'] = 'old' if re.search(r'unrecognized|unknown.*option', probe.stderr, re.I) else 'ok'
        except Exception:
            facts['bubblewrap'] = 'unknown'
    else:
        candidate = re.search(r'Candidate:\s*([0-9]+)\.([0-9]+)', run(['apt-cache', 'policy', 'bubblewrap']))
        facts['bubblewrap'] = 'unknown' if not candidate else 'old' if (int(candidate.group(1)), int(candidate.group(2))) < (0, 8) else 'ok'
unit = 'cage-claude-science.service'
state = {}
for line in run(['systemctl', 'show', '--property=ActiveState', '--property=Result', '--property=InvocationID', unit]).splitlines():
    key, _, value = line.partition('=')
    state[key] = value
facts['unit'] = state.get('ActiveState', '')
if facts['unit'] == 'failed':
    facts['result'] = state.get('Result', '')
listening = bool(run(['ss', '-Hltn', 'sport = :47800']))
if facts['installed']:
    if facts['unit'] in ('active', 'activating', 'reloading', 'deactivating') and listening:
        invocation = state.get('InvocationID', '')
        cache = home / '.cache/cage/claude-science-status.json'
        try:
            saved = json.loads(cache.read_text())
        except Exception:
            saved = {}
        daemon = saved.get('daemon') if invocation and saved.get('invocation') == invocation else None
        if not isinstance(daemon, dict):
            try:
                daemon = json.loads(run([str(binary), 'status']) or '{}')
            except ValueError:
                daemon = {}
            if isinstance(daemon, dict):
                daemon = {k: daemon[k] for k in ('running', 'port', 'started_at', 'version') if k in daemon}
                if invocation and daemon.get('running') is True:
                    try:
                        cache.parent.mkdir(parents=True, exist_ok=True)
                        cache.write_text(json.dumps({'invocation': invocation, 'daemon': daemon}))
                    except OSError:
                        pass
        if isinstance(daemon, dict):
            facts['daemon'] = daemon
            facts['version'] = version(str(daemon.get('version', '')))
    if not facts.get('version'):
        info = binary.stat()
        key = '%d:%d:%d' % (info.st_ino, info.st_size, info.st_mtime_ns)
        cache = home / '.cache/cage/claude-science-version.json'
        try:
            saved = json.loads(cache.read_text())
        except Exception:
            saved = {}
        if saved.get('key') == key:
            facts['version'] = saved.get('version', '')
        else:
            facts['version'] = version(run([str(binary), '--version'], timeout=8))
            try:
                cache.parent.mkdir(parents=True, exist_ok=True)
                cache.write_text(json.dumps({'key': key, 'version': facts['version']}))
            except OSError:
                pass
if facts['unit'] != 'active':
    facts['port_in_use'] = listening
try:
    facts['disk_free_mb'] = shutil.disk_usage(home).free // (1024 * 1024)
except OSError:
    pass
facts['disk_required_mb'] = 1024 if (home / '.claude-science/conda').is_dir() else 11264
print(json.dumps(facts))
CAGE_SCIENCE_STATE`

type claudeScienceGuest struct {
	Architecture   string `json:"architecture"`
	Musl           bool   `json:"musl"`
	HomeFS         string `json:"home_fs"`
	Bubblewrap     string `json:"bubblewrap"`
	Installed      bool   `json:"installed"`
	Version        string `json:"version"`
	Unit           string `json:"unit"`
	Result         string `json:"result"`
	PortInUse      bool   `json:"port_in_use"`
	DiskFreeMB     int64  `json:"disk_free_mb"`
	DiskRequiredMB int64  `json:"disk_required_mb"`
	Daemon         struct {
		Running   bool   `json:"running"`
		Port      int    `json:"port"`
		StartedAt string `json:"started_at"`
	} `json:"daemon"`
}

func claudeScienceFromGuest(guest claudeScienceGuest, pinned string) claudeScienceState {
	state := claudeScienceState{Supported: true, Architecture: guest.Architecture, PinnedVersion: pinned, DiskFreeMB: guest.DiskFreeMB, DiskRequiredMB: guest.DiskRequiredMB}
	switch {
	case guest.Architecture != "x86_64" && guest.Architecture != "amd64":
		state.Supported, state.UnsupportedCode, state.UnsupportedReason = false, "architecture", scienceReasonArchitecture
	case guest.Musl:
		state.Supported, state.UnsupportedCode, state.UnsupportedReason = false, "libc", scienceReasonLibc
	case guest.HomeFS == "1021994" || guest.HomeFS == "794c7630" || guest.HomeFS == "858458f6":
		state.Supported, state.UnsupportedCode, state.UnsupportedReason = false, "storage", scienceReasonStorage
	case !guest.Installed && guest.Bubblewrap == "old":
		state.Supported, state.UnsupportedCode, state.UnsupportedReason = false, "bubblewrap", scienceReasonBubblewrap
	}
	state.Installed = guest.Installed
	if guest.Installed {
		state.Version = guest.Version
		state.UpdateAvailable = guest.Version != "" && compareScienceVersions(guest.Version, pinned) < 0
	}
	state.Running = guest.Unit == "active"
	state.Failed = guest.Unit == "failed"
	if state.Running && guest.Daemon.Running && guest.Daemon.Port == cage.ClaudeSciencePort {
		state.Ready, state.Port, state.StartedAt = true, guest.Daemon.Port, guest.Daemon.StartedAt
	}
	switch {
	case state.Failed && guest.Result == "oom-kill":
		state.FailureReason = "oom-kill"
	case !state.Running && guest.PortInUse:
		state.FailureReason = "port_in_use"
	case state.Failed:
		state.FailureReason = "exit"
	}
	return state
}

// Advice for a daemon that reports a minimum version (phase 2 reads it).
func (state *claudeScienceState) requireVersion(required string) {
	if required == "" || state.Version == "" || compareScienceVersions(state.Version, required) >= 0 {
		return
	}
	yes := true
	state.UpdateRequired, state.RequiredVersion = &yes, required
	if compareScienceVersions(state.PinnedVersion, required) >= 0 {
		state.UpdateVia = "claude-science"
		state.UpdateHint = fmt.Sprintf("Anthropic 要求 Claude Science %s 或更高版本才能继续使用。请点击“更新”，安装 Super Cage 提供的 %s。", required, state.PinnedVersion)
		return
	}
	state.UpdateVia = "super-cage"
	state.UpdateHint = fmt.Sprintf("Anthropic 要求 Claude Science %s 或更高版本才能继续使用，当前 Super Cage 提供的是 %s。请先更新 Super Cage，再在这里更新 Claude Science。", required, state.PinnedVersion)
}

// Numeric dotted versions; a missing or non-numeric part counts as zero.
func compareScienceVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(left), len(right)); i++ {
		var x, y int
		if i < len(left) {
			x, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			y, _ = strconv.Atoi(right[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Unsupported without asking the guest: macOS and Windows guests, and hosts
// whose local guests cannot be x86-64. The guest itself decides otherwise.
func (s *Server) claudeScienceUnsupported() (claudeScienceState, bool) {
	pinned := cage.ClaudeSciencePin().Version
	if s.windowsGuest != nil || s.macosGuest != nil {
		return claudeScienceState{UnsupportedCode: "guest_system", UnsupportedReason: scienceReasonGuestSystem, PinnedVersion: pinned}, true
	}
	if scienceHostArch != "amd64" {
		return claudeScienceState{UnsupportedCode: "architecture", UnsupportedReason: scienceReasonArchitecture, PinnedVersion: pinned}, true
	}
	return claudeScienceState{}, false
}

func (s *Server) claudeScienceOperation() string {
	job := s.jobs.snapshot()
	if !job.Running {
		return ""
	}
	operation, _ := strings.CutPrefix(job.Kind, "claude-science-")
	if operation == job.Kind {
		return ""
	}
	return operation
}

func (s *Server) claudeScienceStatus(w http.ResponseWriter, r *http.Request) {
	// A local guest has the host's architecture, so the guest is not asked.
	if unsupported, refused := s.claudeScienceUnsupported(); refused {
		unsupported.HostRender = s.scienceHostRender(r)
		jsonReply(w, unsupported)
		return
	}
	if !s.vmRunning() {
		s.scienceLast.set(nil)
		http.Error(w, "请先启动 Super Cage 环境", http.StatusConflict)
		return
	}
	// A poll is a lock-free guest call: while a job owns the guest, or it is
	// not ready, the last state read stands, with the job's operation.
	if !s.guestAdmitsLockFreeCalls() {
		state, ok := s.scienceLast.get()
		if !ok {
			message := "环境尚未就绪，连接完成后再查看 Claude Science。"
			if s.busy() {
				message = "环境正在处理其他操作，完成后再查看 Claude Science。"
			}
			http.Error(w, message, http.StatusConflict)
			return
		}
		state.Operation = s.claudeScienceOperation()
		state.HostRender = s.scienceHostRender(r)
		jsonReply(w, state)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	output, err := s.guestQuick(ctx, claudeScienceSnapshot)
	if err != nil {
		_ = diagnostics.New(s.profile).Record("warn", "claude-science", fmt.Sprintf("read state: %v", err))
		http.Error(w, usererror.Wrap("读取 Claude Science 状态", err, "请确认环境仍在运行；重新连接后再打开。持续失败时导出诊断包。").Error(), http.StatusBadGateway)
		return
	}
	var guest claudeScienceGuest
	if json.Unmarshal(output, &guest) != nil {
		http.Error(w, "读取 Claude Science 状态失败：环境返回的状态格式无效。请更新环境组件；仍失败时导出诊断包。", http.StatusBadGateway)
		return
	}
	state := claudeScienceFromGuest(guest, cage.ClaudeSciencePin().Version)
	s.scienceLast.set(&state)
	state.Operation = s.claudeScienceOperation()
	state.HostRender = s.scienceHostRender(r)
	jsonReply(w, state)
}

// claudeScienceLast is the last state a status poll read from the guest.
type claudeScienceLast struct {
	mu    sync.Mutex
	state *claudeScienceState
}

func (l *claudeScienceLast) set(state *claudeScienceState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if state == nil {
		l.state = nil
		return
	}
	saved := *state
	l.state = &saved
}

func (l *claudeScienceLast) get() (claudeScienceState, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		return claudeScienceState{}, false
	}
	return *l.state, true
}

func (s *Server) claudeScienceAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	switch action {
	case "host-render":
		s.scienceHostRenderAction(w, r)
		return
	case "ticket":
		s.scienceTicketAction(w, r)
		return
	case "release":
		s.scienceReleaseAction(w, r)
		return
	case "install", "update", "start", "stop", "open":
	default:
		http.NotFound(w, r)
		return
	}
	var input struct {
		Fresh bool `json:"fresh"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Fresh && action != "open" {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
		return
	}
	if action == "stop" || action == "update" {
		// The daemon goes away: so do the frame's session and credential.
		s.resetScienceFront()
	}
	if state, refused := s.claudeScienceUnsupported(); refused {
		http.Error(w, state.UnsupportedReason, http.StatusConflict)
		return
	}
	if !s.vmRunning() {
		http.Error(w, "请先启动 Super Cage 环境", http.StatusConflict)
		return
	}
	if action == "open" {
		s.openClaudeScience(w, r, input.Fresh)
		return
	}
	s.launch(w, "claude-science-"+action, func() error {
		switch action {
		case "install", "update":
			s.mu.Lock()
			allowed := s.toolsNetworkAllowedLocked()
			s.mu.Unlock()
			if !allowed {
				return errors.New("工作台正在退出，请稍后重试安装")
			}
			return s.withToolsNetworkMonitor(func() error {
				s.setMessage("正在下载并安装 Claude Science，可在操作详情查看进度…")
				script := cage.ClaudeScienceInstallScript()
				if action == "update" {
					script = "set -e\nsudo systemctl stop " + cage.ClaudeScienceUnit + "\n" + script
				}
				return RunMaintenanceScript(s.operationContext(), s.configPath, script, s.jobs.execution(), s.jobs.execution())
			})
		case "start":
			s.setMessage("正在启动 Claude Science…")
			script := "test -x /home/cage/.local/bin/claude-science || { echo '请先安装 Claude Science。' >&2; exit 1; }\n" + cage.ClaudeScienceProvisionScript() + "\n/usr/local/bin/cage-claude-science-start\n"
			return s.runClaudeScienceScript(script, 3*time.Minute)
		default:
			s.setMessage("正在关闭 Claude Science…")
			return s.runClaudeScienceScript("sudo systemctl stop "+cage.ClaudeScienceUnit, 45*time.Second)
		}
	})
}

// The same lease renewal as a Claude Desktop installation: monitoring keeps
// the network open while a long download runs as a job.
func (s *Server) withToolsNetworkMonitor(run func() error) error {
	monitorContext, stopMonitor := context.WithCancel(s.operationContext())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-monitorContext.Done():
				return
			case <-ticker.C:
				s.monitorSafety()
			}
		}
	}()
	defer func() {
		stopMonitor()
		s.mu.Lock()
		if s.monitorCancel != nil {
			s.monitorCancel()
		}
		s.mu.Unlock()
		<-done
	}()
	return run()
}

// Like runDesktopScript, in its own guest unit so the two never collide.
func (s *Server) runClaudeScienceScript(script string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(s.operationContext(), timeout)
	defer cancel()
	command := fmt.Sprintf("sudo systemd-run --quiet --pipe --wait --collect --unit=cage-science-operation --property=User=cage --property=RuntimeMaxSec=%d --property=KillMode=control-group --setenv=HOME=/home/cage --working-directory=/home/cage /bin/bash -se", int(timeout.Seconds()))
	var err error
	if command, err = s.linuxTemplate(command); err != nil {
		return err
	}
	if script, err = s.linuxTemplate(script); err != nil {
		return err
	}
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = s.jobs.execution(), s.jobs.execution()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			cleanup, finish := context.WithTimeout(context.Background(), 5*time.Second)
			defer finish()
			_, _ = s.guest(cleanup, "sudo systemctl stop cage-science-operation.service")
			return ctx.Err()
		}
		return executionFailure("启动或关闭 Claude Science", err, s.jobs.snapshot().Log)
	}
	return nil
}

// Prints the daemon's single-use link and its status on separate lines, or
// a marker. The link stays in this process's memory and stdout only.
const claudeScienceLinkScript = `binary="$HOME/.local/bin/claude-science"
if ! test -x "$binary"; then echo not-installed; exit 0; fi
status="$(DO_NOT_TRACK=1 "$binary" status </dev/null 2>/dev/null)" || status='{}'
if ! link="$(DO_NOT_TRACK=1 "$binary" url </dev/null 2>/dev/null)"; then echo not-running; exit 0; fi
printf '%s\n' "$link" | head -n 1
printf '%s\n' "$status" | tr -d '\n'`

var scienceLinkPattern = regexp.MustCompile(`^http://(?:localhost|127\.0\.0\.1):([0-9]{4,5})/\?nonce=[A-Za-z0-9_-]{16,128}$`)

type claudeScienceLink struct {
	url, startedAt string
}

var (
	errScienceNotInstalled = errors.New("请先安装 Claude Science。")
	errScienceNotRunning   = errors.New("Claude Science 尚未启动，请先启动后再打开。")
)

func parseClaudeScienceLink(output []byte) (claudeScienceLink, error) {
	first, rest, _ := strings.Cut(strings.TrimRight(string(output), "\n"), "\n")
	switch first {
	case "not-installed":
		return claudeScienceLink{}, errScienceNotInstalled
	case "not-running":
		return claudeScienceLink{}, errScienceNotRunning
	}
	match := scienceLinkPattern.FindStringSubmatch(first)
	if match == nil || match[1] != strconv.Itoa(cage.ClaudeSciencePort) {
		// Never echo the output: it may hold a valid link for another port.
		return claudeScienceLink{}, errors.New("Claude Science 返回的登录链接无效。请关闭 Claude Science 后重新打开；仍失败时导出诊断包。")
	}
	var status struct {
		StartedAt string `json:"started_at"`
	}
	if json.Unmarshal([]byte(rest), &status) != nil || len(status.StartedAt) > 64 {
		status.StartedAt = ""
	}
	return claudeScienceLink{url: first, startedAt: status.StartedAt}, nil
}

func (s *Server) openClaudeScience(w http.ResponseWriter, r *http.Request, fresh bool) {
	if s.claudeScienceOperation() != "" {
		http.Error(w, "Claude Science 正在安装、更新、启动或关闭，请等待完成后再打开。", http.StatusConflict)
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		http.Error(w, "浏览器仍在启动。请等待启动完成后再打开 Claude Science。", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	output, err := s.guestQuick(ctx, claudeScienceLinkScript)
	if err != nil {
		http.Error(w, usererror.Wrap("打开 Claude Science", err, "请确认环境仍在运行；重新连接后再打开。").Error(), http.StatusBadGateway)
		return
	}
	tab, status, err := s.openClaudeScienceLink(ctx, token, output, fresh)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	jsonReply(w, tab)
}

type claudeScienceTab struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Reused bool   `json:"reused"`
}

// Hands what claudeScienceLinkScript printed to the browser agent, which
// checks the link again against its own provisioned port.
func (s *Server) openClaudeScienceLink(ctx context.Context, token string, output []byte, fresh bool) (claudeScienceTab, int, error) {
	link, err := parseClaudeScienceLink(output)
	if errors.Is(err, errScienceNotInstalled) || errors.Is(err, errScienceNotRunning) {
		return claudeScienceTab{}, http.StatusConflict, err
	}
	if err != nil {
		return claudeScienceTab{}, http.StatusBadGateway, err
	}
	body, _ := json.Marshal(map[string]any{"url": link.url, "started_at": link.startedAt, "fresh": fresh})
	data, err := s.guestAPI(ctx, token, http.MethodPost, "/science", body)
	if err != nil {
		return claudeScienceTab{}, http.StatusBadGateway, err
	}
	var tab claudeScienceTab
	if json.Unmarshal(data, &tab) != nil || tab.ID == "" {
		return claudeScienceTab{}, http.StatusBadGateway, errors.New("浏览器没有返回 Claude Science 标签页。请更新环境组件后再试。")
	}
	return claudeScienceTab{ID: tab.ID, Kind: "science", Title: "Claude Science", Reused: tab.Reused}, http.StatusOK, nil
}
