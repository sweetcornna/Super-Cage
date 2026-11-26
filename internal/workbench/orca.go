package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/localorca"
)

const (
	orcaStatusTTL   = 10 * time.Second
	orcaOpenMinGap  = 1500 * time.Millisecond
)

var (
	orcaMemLimitRe = regexp.MustCompile(`^\d+[KMGT]$`)
	orcaVersionRe  = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
)

// orcaConfig persists the user's opt-in Orca Server (stablyai/orca server-only
// orcad) preferences. Disabled by default: zero guest SSH calls, zero tunnels,
// and zero background overhead until enabled.
type orcaConfig struct {
	Enabled           bool              `json:"enabled"`
	GuestPort         int               `json:"guest_port"`
	HostTunnelPort    int               `json:"host_tunnel_port,omitempty"`
	BindMode          string            `json:"bind_mode"` // loopback | tailscale
	ServerName        string            `json:"server_name,omitempty"`
	AutoSyncProjects  bool              `json:"auto_sync_projects"`
	TerminalMemoryMax string            `json:"terminal_memory_max,omitempty"`
	AccountBindings   map[string]string `json:"account_bindings,omitempty"`
	PairedEnvID       string            `json:"paired_env_id,omitempty"`
}

func defaultOrcaServerName(profileDir string) string {
	base := strings.TrimSpace(filepath.Base(profileDir))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	if clean == "" || clean == "." || clean == ".." {
		return "Super Cage"
	}
	name := "Super Cage (" + clean + ")"
	if len(name) > 64 {
		name = name[:64]
	}
	if _, err := localorca.ValidateEnvironmentName(name); err != nil {
		return "Super Cage"
	}
	return name
}

func defaultOrcaConfig(profileDir string) orcaConfig {
	return orcaConfig{
		Enabled:          false,
		GuestPort:        defaultOrcaPort,
		HostTunnelPort:   0,
		BindMode:         "loopback",
		ServerName:       defaultOrcaServerName(profileDir),
		AutoSyncProjects: true,
		AccountBindings: map[string]string{
			"claude":      "auto",
			"codex":       "auto",
			"antigravity": "auto",
			"opencode":    "auto",
		},
	}
}

func (c orcaConfig) normalized(profileDir string) orcaConfig {
	if c.GuestPort < 1024 || c.GuestPort > 65535 {
		c.GuestPort = defaultOrcaPort
	}
	if c.HostTunnelPort != 0 && (c.HostTunnelPort < 1024 || c.HostTunnelPort > 65535) {
		c.HostTunnelPort = 0
	}
	if c.BindMode != "loopback" && c.BindMode != "tailscale" {
		c.BindMode = "loopback"
	}
	if strings.TrimSpace(c.ServerName) == "" {
		c.ServerName = defaultOrcaServerName(profileDir)
	}
	if c.AccountBindings == nil {
		c.AccountBindings = map[string]string{}
	}
	return c
}

type orcaGuestStatus struct {
	Installed   bool   `json:"installed"`
	BinaryPath  string `json:"binary_path,omitempty"`
	Version     string `json:"version,omitempty"`
	Running     bool   `json:"running"`
	PID         int    `json:"pid,omitempty"`
	Port        int    `json:"port,omitempty"`
	BindIP      string `json:"bind_ip,omitempty"`
	BindMode    string `json:"bind_mode,omitempty"`
	HasRollback bool   `json:"has_rollback,omitempty"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Platform    string `json:"platform,omitempty"`
}

type orcaProbeCall struct {
	done   chan struct{}
	port   int
	forced bool
	status orcaGuestStatus
	err    error
}

type orcaManager struct {
	opMu           sync.Mutex
	mu             sync.Mutex
	configLoaded   bool
	config         orcaConfig
	statusCache    orcaGuestStatus
	statusAt       time.Time
	probeInflight  *orcaProbeCall
	activeBindings map[string]string
	tunnel         *orcaTunnel
	lastOpenAt     time.Time
	// Test seams (nil in production).
	localDetect    func() localorca.Installation
	localClient    *localorca.Client
	tunnelDialHook func(ctx context.Context, guestPort int) (string, error)
}

func (s *Server) detectLocalOrca() localorca.Installation {
	if fn := s.orca.localDetect; fn != nil {
		return fn()
	}
	return localorca.Detect()
}

func (s *Server) localOrcaClient(inst localorca.Installation) *localorca.Client {
	if c := s.orca.localClient; c != nil {
		return c
	}
	return localorca.NewClient(inst)
}

func (s *Server) loadOrcaConfigLocked() (orcaConfig, error) {
	m := &s.orca
	if m.configLoaded {
		cp := m.config.normalized(s.profile)
		cp.AccountBindings = maps.Clone(cp.AccountBindings)
		return cp, nil
	}
	cfg := defaultOrcaConfig(s.profile)
	data, err := os.ReadFile(filepath.Join(s.profile, "orca.json"))
	if os.IsNotExist(err) {
		m.config, m.configLoaded = cfg, true
		cp := cfg
		cp.AccountBindings = maps.Clone(cfg.AccountBindings)
		return cp, nil
	}
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return cfg, errors.New("Orca 配置文件无法读取，请检查配置目录")
	}
	cfg = cfg.normalized(s.profile)
	m.config = cfg
	m.configLoaded = true
	cp := cfg
	cp.AccountBindings = maps.Clone(cfg.AccountBindings)
	return cp, nil
}

func (s *Server) saveOrcaConfigLocked(cfg orcaConfig) error {
	clean := cfg.normalized(s.profile)
	clean.AccountBindings = maps.Clone(clean.AccountBindings)
	if err := writePrivate(filepath.Join(s.profile, "orca.json"), clean); err != nil {
		return errors.New("Orca 设置未能保存，请检查磁盘空间和权限")
	}
	s.orca.config = clean
	s.orca.configLoaded = true
	return nil
}

func (s *Server) invalidateOrcaStatus() {
	s.orca.mu.Lock()
	s.orca.statusAt = time.Time{}
	s.orca.mu.Unlock()
}

func (s *Server) orcaLocalDesktopAllowed(r *http.Request) bool {
	if s.clientOnly || containerenv.Configured(s.profile) {
		return false
	}
	if s.windowsGuest != nil && s.windowsGuest.Remote() {
		return false
	}
	if r != nil {
		if r.Header.Get("X-Cage-Remote-Client") == "1" {
			return false
		}
		if _, phoneRoom := s.pairedDevice(r); phoneRoom != nil {
			return false
		}
	}
	return true
}

func (s *Server) ensureOrcaHostTunnelPort(ctx context.Context, cfg *orcaConfig) bool {
	if cfg.HostTunnelPort >= 1024 && cfg.HostTunnelPort <= 65535 {
		return false
	}
	reserved := map[int]bool{}
	if inst := s.detectLocalOrca(); inst.Installed && inst.CLIPath != "" {
		scanCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		reserved = s.localOrcaClient(inst).UsedLoopbackPorts(scanCtx)
		cancel()
	}
	cfg.HostTunnelPort = pickFreeOrcaTunnelPort(defaultOrcaPort, reserved)
	return true
}

func (s *Server) orcaProjectGuestPaths() []string {
	paths, err := s.projectPaths()
	if err != nil {
		if cur := s.workspace(); cur != "" {
			paths = []string{cur}
		}
	}
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, hostPath := range paths {
		if strings.TrimSpace(hostPath) == "" {
			continue
		}
		guestPath := strings.TrimSpace(s.cliWorkspace(hostPath))
		if guestPath == "" || !strings.HasPrefix(guestPath, "/") || seen[guestPath] {
			continue
		}
		seen[guestPath] = true
		out = append(out, guestPath)
	}
	return out
}

func (s *Server) probeOrcaStatus(ctx context.Context, port int, force bool) (orcaGuestStatus, error) {
	m := &s.orca
	for {
		m.mu.Lock()
		if !force && !m.statusAt.IsZero() && time.Since(m.statusAt) < orcaStatusTTL && m.statusCache.Port == port {
			cached := m.statusCache
			m.mu.Unlock()
			return cached, nil
		}
		if call := m.probeInflight; call != nil && call.port == port {
			if !force || call.forced {
				m.mu.Unlock()
				select {
				case <-ctx.Done():
					return orcaGuestStatus{}, ctx.Err()
				case <-call.done:
					return call.status, call.err
				}
			}
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return orcaGuestStatus{}, ctx.Err()
			case <-call.done:
				continue
			}
		}
		call := &orcaProbeCall{done: make(chan struct{}), port: port, forced: force}
		m.probeInflight = call
		m.mu.Unlock()

		ensureCtx, ensureCancel := context.WithTimeout(s.lifetimeContext(), coldEnsureTimeout)
		err := s.ensureAgentRuntime(ensureCtx)
		ensureCancel()
		var st orcaGuestStatus
		if err == nil {
			probeCtx, cancel := context.WithTimeout(s.lifetimeContext(), statusProbeTimeout)
			err = s.runAgentRuntime(probeCtx, "orca-status", map[string]any{"port": port}, &st)
			cancel()
		}
		if err == nil && st.Port == 0 {
			st.Port = port
		}
		m.mu.Lock()
		if err == nil {
			m.statusCache = st
			m.statusAt = time.Now()
		}
		call.status, call.err = st, err
		if m.probeInflight == call {
			m.probeInflight = nil
		}
		close(call.done)
		m.mu.Unlock()
		return st, err
	}
}

func (s *Server) orcaStatusPayload(r *http.Request, force bool) (map[string]any, error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	s.orca.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	activeBindings := maps.Clone(s.orca.activeBindings)
	s.orca.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// CRITICAL: call s.vmRunning() BEFORE s.mu.Lock() to prevent deadlock on Windows guests.
	runningVM := s.vmRunning()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()

	supportsManaged := s.windowsGuest == nil
	localAllowed := s.orcaLocalDesktopAllowed(r)

	var localInst localorca.Installation
	if localAllowed {
		localInst = s.detectLocalOrca()
	}
	canAutoPair := localAllowed && localInst.Installed && localInst.CLIPath != "" && supportsManaged

	var st orcaGuestStatus
	probeErr := ""
	// Zero guest overhead while disabled unless caller explicitly forces a check.
	if (cfg.Enabled || force) && ready && runningVM && supportsManaged {
		pCtx, cancel := context.WithTimeout(ctx, s.statusProbeWaitTimeout())
		st, err = s.probeOrcaStatus(pCtx, cfg.GuestPort, force)
		cancel()
		if err != nil {
			probeErr = err.Error()
		}
	}

	tunnelActive, tunnelPort := s.orcaTunnelSnapshot()
	effectiveTunnelPort := cfg.HostTunnelPort
	if tunnelPort > 0 {
		effectiveTunnelPort = tunnelPort
	}
	tunnelEndpoint := ""
	if effectiveTunnelPort > 0 {
		tunnelEndpoint = fmt.Sprintf("ws://127.0.0.1:%d", effectiveTunnelPort)
	}

	paired := cfg.PairedEnvID != ""
	pairedName := ""
	if canAutoPair && cfg.Enabled && tunnelEndpoint != "" {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if env, findErr := s.localOrcaClient(localInst).FindEnvironmentByEndpoint(checkCtx, tunnelEndpoint); findErr == nil && env != nil {
			paired = true
			pairedName = env.Name
		} else if findErr == nil {
			paired = false
		}
		cancel()
	}

	return map[string]any{
		"enabled":             cfg.Enabled,
		"guest_port":          cfg.GuestPort,
		"host_tunnel_port":    cfg.HostTunnelPort,
		"bind_mode":           cfg.BindMode,
		"server_name":         cfg.ServerName,
		"auto_sync_projects":  cfg.AutoSyncProjects,
		"terminal_memory_max": cfg.TerminalMemoryMax,
		"account_bindings":    cfg.AccountBindings,
		"installed":           st.Installed,
		"version":             st.Version,
		"running":             st.Running,
		"bind_ip":             st.BindIP,
		"tailscale_ip":        st.TailscaleIP,
		"has_rollback":        st.HasRollback,
		"tunnel_active":       tunnelActive,
		"tunnel_port":         effectiveTunnelPort,
		"tunnel_endpoint":     tunnelEndpoint,
		"paired":              paired,
		"paired_name":         pairedName,
		"local_desktop":       localInst,
		"can_auto_pair":       canAutoPair,
		"download_url":        localorca.DownloadURL,
		"active_bindings":     activeBindings,
		"magpie_takeover":     s.magpieTakesOverScheduling(),
		"error":               probeErr,
		"vm_running":          runningVM,
		"ready":               ready,
		"supports_managed":    supportsManaged,
		"is_macos_guest":      s.macosGuest != nil,
	}, nil
}

func (s *Server) orcaStatusHTTP(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	payload, err := s.orcaStatusPayload(r, force)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) orcaConfigHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled           *bool             `json:"enabled"`
		GuestPort         *int              `json:"guest_port"`
		HostTunnelPort    *int              `json:"host_tunnel_port"`
		BindMode          string            `json:"bind_mode"`
		ServerName        *string           `json:"server_name"`
		AutoSyncProjects  *bool             `json:"auto_sync_projects"`
		TerminalMemoryMax *string           `json:"terminal_memory_max"`
		AccountBindings   map[string]string `json:"account_bindings"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.orca.opMu.Lock()
	defer s.orca.opMu.Unlock()

	s.orca.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	s.orca.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}

	prevEnabled, prevGuestPort, prevHostPort, prevBind := cfg.Enabled, cfg.GuestPort, cfg.HostTunnelPort, cfg.BindMode
	var closeTunnel bool

	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
		if !cfg.Enabled {
			closeTunnel = true
		}
	}
	if in.GuestPort != nil {
		if *in.GuestPort < 1024 || *in.GuestPort > 65535 {
			requestError(w, errors.New("Orca Server 客机端口须在 1024 到 65535 之间"))
			return
		}
		if cfg.GuestPort != *in.GuestPort {
			closeTunnel = true
		}
		cfg.GuestPort = *in.GuestPort
	}
	if in.HostTunnelPort != nil {
		if *in.HostTunnelPort != 0 && (*in.HostTunnelPort < 1024 || *in.HostTunnelPort > 65535) {
			requestError(w, errors.New("本机隧道端口须为 0（自动分配）或在 1024 到 65535 之间"))
			return
		}
		if cfg.HostTunnelPort != *in.HostTunnelPort {
			closeTunnel = true
		}
		cfg.HostTunnelPort = *in.HostTunnelPort
	}
	if in.BindMode != "" {
		if in.BindMode != "loopback" && in.BindMode != "tailscale" {
			requestError(w, errors.New("网络绑定模式无效，仅支持 loopback 或 tailscale"))
			return
		}
		if cfg.BindMode != in.BindMode {
			closeTunnel = true
		}
		cfg.BindMode = in.BindMode
	}
	if in.ServerName != nil {
		cleanName, nameErr := localorca.ValidateEnvironmentName(*in.ServerName)
		if nameErr != nil {
			requestError(w, nameErr)
			return
		}
		cfg.ServerName = cleanName
	}
	if in.AutoSyncProjects != nil {
		cfg.AutoSyncProjects = *in.AutoSyncProjects
	}
	if in.TerminalMemoryMax != nil {
		mem := strings.ToUpper(strings.TrimSpace(*in.TerminalMemoryMax))
		if mem != "" && !orcaMemLimitRe.MatchString(mem) {
			requestError(w, errors.New("终端内存上限格式无效（例如 2G、512M，留空为不限制）"))
			return
		}
		cfg.TerminalMemoryMax = mem
	}
	if in.AccountBindings != nil {
		cleanBindings := map[string]string{}
		for k, v := range in.AccountBindings {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k != "" && v != "" && len(k) <= 32 && len(v) <= 64 {
				cleanBindings[k] = v
			}
		}
		cfg.AccountBindings = cleanBindings
	}
	if cfg.Enabled && s.windowsGuest != nil {
		requestError(w, errors.New("当前 Windows 原生客机暂不支持托管 Orca Server"))
		return
	}
	if cfg.Enabled && cfg.HostTunnelPort == 0 {
		s.ensureOrcaHostTunnelPort(r.Context(), &cfg)
	}

	needStopGuest := (prevEnabled && !cfg.Enabled) || cfg.GuestPort != prevGuestPort || cfg.BindMode != prevBind
	if needStopGuest && s.vmRunning() && s.windowsGuest == nil {
		stopCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		stopErr := s.runAgentRuntime(stopCtx, "orca-stop", map[string]any{"port": prevGuestPort}, nil)
		cancel()
		if stopErr != nil {
			http.Error(w, fmt.Sprintf("停止原托管 Orca Server 失败，配置未变更: %v", stopErr), http.StatusConflict)
			return
		}
	}

	s.orca.mu.Lock()
	saveErr := s.saveOrcaConfigLocked(cfg)
	if saveErr == nil && (needStopGuest || closeTunnel || prevHostPort != cfg.HostTunnelPort) {
		s.orca.activeBindings = nil
	}
	s.orca.mu.Unlock()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	if closeTunnel || needStopGuest {
		s.closeOrcaTunnel()
	}
	s.invalidateOrcaStatus()
	payload, err := s.orcaStatusPayload(r, false)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) orcaInstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	version := strings.TrimPrefix(strings.TrimSpace(in.Version), "v")
	if strings.EqualFold(version, "latest") {
		version = ""
	} else if strings.EqualFold(version, "match_desktop") || version == "" {
		if s.orcaLocalDesktopAllowed(r) {
			if inst := s.detectLocalOrca(); inst.Version != "" {
				version = inst.Version
			} else {
				version = ""
			}
		} else {
			version = ""
		}
	}
	if version != "" && !orcaVersionRe.MatchString(version) {
		http.Error(w, "无效的 Orca Server 版本号", http.StatusBadRequest)
		return
	}

	running := s.vmRunning()
	s.mu.Lock()
	ready, paused, closing := s.ready, s.networkPaused, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境并等待就绪后再安装 Orca Server", http.StatusConflict)
		return
	}
	if paused && s.macosGuest == nil {
		http.Error(w, "网络已暂停，请先恢复网络连接后再安装 Orca Server", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键托管安装 Orca Server", http.StatusConflict)
		return
	}

	s.launch(w, "orca-install", func() error {
		s.orca.opMu.Lock()
		defer s.orca.opMu.Unlock()
		if version != "" {
			s.setMessage(fmt.Sprintf("正在下载并校验官方 Orca Server v%s（服务端专用版）…", version))
		} else {
			s.setMessage("正在下载并校验最新官方 Orca Server（服务端专用版）…")
		}
		if err := s.runAgentRuntimeJob(s.operationContext(), "orca-install", map[string]any{"version": version}); err != nil {
			return errors.New("安装 Orca Server 失败，请查看操作详情排查网络或校验信息")
		}
		s.orca.mu.Lock()
		if cfg, err := s.loadOrcaConfigLocked(); err == nil {
			cfg.Enabled = true
			if cfg.HostTunnelPort == 0 {
				s.orca.mu.Unlock()
				s.ensureOrcaHostTunnelPort(s.operationContext(), &cfg)
				s.orca.mu.Lock()
			}
			_ = s.saveOrcaConfigLocked(cfg)
		}
		s.orca.statusAt = time.Time{}
		s.orca.mu.Unlock()
		s.setMessage("Orca Server 安装完成，正在验证状态…")
		return nil
	})
}

func (s *Server) orcaUninstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PurgeData     bool `json:"purge_data"`
		RemovePaired  bool `json:"remove_paired"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	running := s.vmRunning()
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境后再卸载 Orca Server", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机不支持此操作", http.StatusConflict)
		return
	}
	localAllowed := s.orcaLocalDesktopAllowed(r)

	s.launch(w, "orca-uninstall", func() error {
		s.orca.opMu.Lock()
		defer s.orca.opMu.Unlock()
		s.closeOrcaTunnel()
		if in.PurgeData {
			s.setMessage("正在停止并彻底卸载 Orca Server（含服务端数据）…")
		} else {
			s.setMessage("正在卸载 Orca Server（保留历史数据与配置）…")
		}
		if err := s.runAgentRuntimeJob(s.operationContext(), "orca-uninstall", map[string]any{"purge_data": in.PurgeData}); err != nil {
			return errors.New("卸载 Orca Server 失败，请查看操作详情")
		}
		var pairedEnvID string
		s.orca.mu.Lock()
		if cfg, err := s.loadOrcaConfigLocked(); err == nil {
			pairedEnvID = cfg.PairedEnvID
			cfg.Enabled = false
			if in.RemovePaired || in.PurgeData {
				cfg.PairedEnvID = ""
			}
			_ = s.saveOrcaConfigLocked(cfg)
		}
		s.orca.activeBindings = nil
		s.orca.statusAt = time.Time{}
		s.orca.mu.Unlock()

		if in.RemovePaired && localAllowed && pairedEnvID != "" {
			if inst := s.detectLocalOrca(); inst.Installed && inst.CLIPath != "" {
				_ = s.localOrcaClient(inst).RemoveEnvironment(s.operationContext(), pairedEnvID)
			}
		}
		return nil
	})
}

func (s *Server) ensureManagedOrcaRunning(ctx context.Context, restart bool, workspace string) (orcaConfig, orcaGuestStatus, error) {
	s.orca.mu.Lock()
	cfg, err := s.loadOrcaConfigLocked()
	s.orca.mu.Unlock()
	if err != nil {
		return cfg, orcaGuestStatus{}, err
	}
	if !cfg.Enabled {
		return cfg, orcaGuestStatus{}, errors.New("请先在「设置 → Agent CLI」中启用 Orca Server 后再启动服务")
	}
	if !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
		return cfg, orcaGuestStatus{}, errors.New("请先启动 Cage 环境后再运行 Orca Server")
	}
	if s.windowsGuest != nil {
		return cfg, orcaGuestStatus{}, errors.New("当前 Windows 原生客机暂不支持托管 Orca Server")
	}
	if cfg.HostTunnelPort == 0 {
		s.ensureOrcaHostTunnelPort(ctx, &cfg)
		s.orca.mu.Lock()
		_ = s.saveOrcaConfigLocked(cfg)
		s.orca.mu.Unlock()
	}

	if workspace == "" {
		workspace = s.workspace()
	}
	guestBindings, labels, err := s.resolveToolAccountBindings(cfg.AccountBindings, workspace, "Orca Server")
	if err != nil {
		return cfg, orcaGuestStatus{}, err
	}

	var projects []string
	if cfg.AutoSyncProjects {
		projects = s.orcaProjectGuestPaths()
	}

	subcmd := "orca-start"
	if restart {
		subcmd = "orca-restart"
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var st orcaGuestStatus
	err = s.runAgentRuntime(startCtx, subcmd, map[string]any{
		"port":                cfg.GuestPort,
		"bind_mode":           cfg.BindMode,
		"workspace":           s.cliWorkspace(workspace),
		"bindings":            guestBindings,
		"projects":            projects,
		"terminal_memory_max": cfg.TerminalMemoryMax,
	}, &st)
	if err != nil {
		return cfg, st, err
	}
	st.Port = cfg.GuestPort
	st.Installed = true
	s.orca.mu.Lock()
	s.orca.statusCache = st
	s.orca.statusAt = time.Now()
	s.orca.activeBindings = labels
	s.orca.mu.Unlock()
	return cfg, st, nil
}

func (s *Server) orcaOpenHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.orcaLocalDesktopAllowed(r) {
		http.Error(w, "仅支持在本机桌面窗口一键连接本机 Orca 客户端；远程访问请使用「配对码 / 二维码」手动连接", http.StatusForbidden)
		return
	}
	var in struct {
		Workspace   string `json:"workspace"`
		ForceRepair bool   `json:"force_repair"`
	}
	if !readJSON(w, r, &in) {
		return
	}

	s.orca.opMu.Lock()
	defer s.orca.opMu.Unlock()

	s.orca.mu.Lock()
	if !s.orca.lastOpenAt.IsZero() && time.Since(s.orca.lastOpenAt) < orcaOpenMinGap {
		s.orca.mu.Unlock()
		http.Error(w, "正在打开本机 Orca 客户端，请稍候…", http.StatusTooManyRequests)
		return
	}
	s.orca.lastOpenAt = time.Now()
	s.orca.mu.Unlock()

	inst := s.detectLocalOrca()
	if !inst.Installed || inst.CLIPath == "" {
		http.Error(w, "本机未检测到 Orca 桌面客户端，请先安装官方 Orca Desktop（https://orca.so）后重试，或点击「配对码」在远程设备上连接", http.StatusConflict)
		return
	}

	cfg, _, err := s.ensureManagedOrcaRunning(r.Context(), false, in.Workspace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	tun, err := s.ensureOrcaTunnel(r.Context(), cfg.HostTunnelPort, cfg.GuestPort)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	endpoint := fmt.Sprintf("ws://127.0.0.1:%d", tun.hostPort)
	client := s.localOrcaClient(inst)

	existing, findErr := client.FindEnvironmentByEndpoint(r.Context(), endpoint)
	if findErr != nil {
		http.Error(w, fmt.Sprintf("无法读取本机 Orca 已保存服务器列表: %v", findErr), http.StatusBadGateway)
		return
	}

	newlyPaired := false
	envID := ""
	envName := cfg.ServerName
	if existing != nil && !in.ForceRepair {
		envID = existing.ID
		envName = existing.Name
		if cfg.PairedEnvID != envID {
			cfg.PairedEnvID = envID
			s.orca.mu.Lock()
			_ = s.saveOrcaConfigLocked(cfg)
			s.orca.mu.Unlock()
		}
	} else {
		if existing != nil {
			_ = client.RemoveEnvironment(r.Context(), existing.ID)
		} else if cfg.PairedEnvID != "" {
			_ = client.RemoveEnvironment(r.Context(), cfg.PairedEnvID)
		}
		var pairRes struct {
			Available  bool   `json:"available"`
			PairingURL string `json:"pairing_url"`
			Endpoint   string `json:"endpoint"`
		}
		pairCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		pairErr := s.runAgentRuntime(pairCtx, "orca-pair", map[string]any{
			"mobile":  false,
			"rotate":  in.ForceRepair,
			"address": fmt.Sprintf("127.0.0.1:%d", tun.hostPort),
		}, &pairRes)
		cancel()
		if pairErr != nil || !pairRes.Available || pairRes.PairingURL == "" {
			if pairErr == nil {
				pairErr = errors.New("客机 Orca Server 未返回有效配对链接")
			}
			http.Error(w, fmt.Sprintf("生成 Orca 配对码失败: %v", pairErr), http.StatusBadGateway)
			return
		}
		added, addErr := client.AddEnvironment(r.Context(), cfg.ServerName, pairRes.PairingURL)
		if addErr != nil {
			http.Error(w, fmt.Sprintf("自动配对本机 Orca 客户端失败: %v", addErr), http.StatusBadGateway)
			return
		}
		newlyPaired = true
		envID = added.ID
		if added.Name != "" {
			envName = added.Name
		}
		if envID != "" {
			cfg.PairedEnvID = envID
			s.orca.mu.Lock()
			_ = s.saveOrcaConfigLocked(cfg)
			s.orca.mu.Unlock()
		}
	}

	if err := client.OpenDesktop(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("已配对服务器「%s」，但自动拉起本机 Orca 窗口失败: %v", envName, err), http.StatusBadGateway)
		return
	}

	jsonReply(w, map[string]any{
		"opened":           true,
		"paired":           true,
		"newly_paired":     newlyPaired,
		"environment_id":   envID,
		"environment_name": envName,
		"endpoint":         endpoint,
		"tunnel_port":      tun.hostPort,
	})
}

func (s *Server) orcaPairHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mobile  bool   `json:"mobile"`
		Rotate  bool   `json:"rotate"`
		Address string `json:"address"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.orca.opMu.Lock()
	defer s.orca.opMu.Unlock()

	cfg, st, err := s.ensureManagedOrcaRunning(r.Context(), false, s.workspace())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	addr := strings.TrimSpace(in.Address)
	if addr == "" {
		if in.Mobile || cfg.BindMode == "tailscale" {
			if st.TailscaleIP != "" {
				addr = fmt.Sprintf("%s:%d", st.TailscaleIP, cfg.GuestPort)
			} else if in.Mobile {
				http.Error(w, "手机端 Orca Mobile 配对需要先启用并连接 Tailscale 私有网络，或手动指定可达地址", http.StatusConflict)
				return
			}
		}
		if addr == "" {
			tun, tunErr := s.ensureOrcaTunnel(r.Context(), cfg.HostTunnelPort, cfg.GuestPort)
			if tunErr != nil {
				http.Error(w, tunErr.Error(), http.StatusBadGateway)
				return
			}
			addr = fmt.Sprintf("127.0.0.1:%d", tun.hostPort)
		}
	} else if strings.HasPrefix(addr, "127.0.0.1:") || strings.HasPrefix(addr, "localhost:") {
		if _, tunErr := s.ensureOrcaTunnel(r.Context(), cfg.HostTunnelPort, cfg.GuestPort); tunErr != nil {
			http.Error(w, tunErr.Error(), http.StatusBadGateway)
			return
		}
	}

	var pairRes map[string]any
	pairCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	err = s.runAgentRuntime(pairCtx, "orca-pair", map[string]any{
		"mobile":  in.Mobile,
		"rotate":  in.Rotate,
		"address": addr,
	}, &pairRes)
	cancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, pairRes)
}

func (s *Server) orcaSyncProjectsHTTP(w http.ResponseWriter, r *http.Request) {
	s.orca.opMu.Lock()
	defer s.orca.opMu.Unlock()
	if _, _, err := s.ensureManagedOrcaRunning(r.Context(), false, s.workspace()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	projects := s.orcaProjectGuestPaths()
	var res map[string]any
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	err := s.runAgentRuntime(ctx, "orca-sync-repos", map[string]any{"projects": projects}, &res)
	cancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if res == nil {
		res = map[string]any{}
	}
	res["total"] = len(projects)
	jsonReply(w, res)
}

func (s *Server) orcaActionHTTP(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimSpace(r.PathValue("action"))
	s.orca.opMu.Lock()
	defer s.orca.opMu.Unlock()
	switch action {
	case "start", "restart":
		cfg, _, err := s.ensureManagedOrcaRunning(r.Context(), action == "restart", s.workspace())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if action == "restart" {
			s.closeOrcaTunnel()
		}
		if cfg.BindMode == "loopback" && s.orcaLocalDesktopAllowed(r) {
			_, _ = s.ensureOrcaTunnel(r.Context(), cfg.HostTunnelPort, cfg.GuestPort)
		}
		payload, err := s.orcaStatusPayload(r, false)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	case "stop":
		s.closeOrcaTunnel()
		s.orca.mu.Lock()
		cfg, _ := s.loadOrcaConfigLocked()
		s.orca.mu.Unlock()
		if (s.vmRunning() || s.agentCLIs.runtimeHook != nil) && s.windowsGuest == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			err := s.runAgentRuntime(ctx, "orca-stop", map[string]any{"port": cfg.GuestPort}, nil)
			cancel()
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		s.orca.mu.Lock()
		s.orca.activeBindings = nil
		s.orca.statusAt = time.Time{}
		s.orca.mu.Unlock()
		payload, err := s.orcaStatusPayload(r, true)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	case "reset-identity":
		s.closeOrcaTunnel()
		if (s.vmRunning() || s.agentCLIs.runtimeHook != nil) && s.windowsGuest == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			err := s.runAgentRuntime(ctx, "orca-reset-identity", map[string]any{}, nil)
			cancel()
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		s.orca.mu.Lock()
		if cfg, err := s.loadOrcaConfigLocked(); err == nil {
			cfg.PairedEnvID = ""
			_ = s.saveOrcaConfigLocked(cfg)
		}
		s.orca.activeBindings = nil
		s.orca.statusAt = time.Time{}
		s.orca.mu.Unlock()
		payload, err := s.orcaStatusPayload(r, true)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	default:
		http.NotFound(w, r)
	}
}
