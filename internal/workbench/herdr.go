package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// Herdr is herdr-core (hc) in the guest: the environment's one terminal
// store. Cage's terminals are hc terminals, every hc client in the
// environment sees them, and Cage lists the hc terminals other clients
// started (herdr_terminals.go). The guest side is agent-runtime.py herdr-*.

const herdrStatusTTL = 10 * time.Second

// herdrConfig persists the user's opt-in Herdr preferences. Disabled by
// default: nothing asks the guest about Herdr until it is enabled. A theme
// saved by Super Cage 0.1.21-0.1.23 (the Herdr TUI's) is ignored.
type herdrConfig struct {
	Enabled           bool `json:"enabled"`
	TakeoverTerminals bool `json:"takeover_terminals"`       // when installed & enabled, workbench terminals are hc terminals
	InstalledHint     bool `json:"installed_hint,omitempty"` // fast hint set on install/probe so terminal launch needs 0 extra SSH RTT
}

func defaultHerdrConfig() herdrConfig {
	return herdrConfig{Enabled: false, TakeoverTerminals: true}
}

type herdrGuestStatus struct {
	Installed      bool   `json:"installed"`
	BinaryPath     string `json:"binary_path,omitempty"`
	Version        string `json:"version,omitempty"`
	ServerRunning  bool   `json:"server_running"`
	TerminalsCount int    `json:"terminals_count,omitempty"`
	// KeepsRunning is hc's keeps_running_after_logout: whether the guest
	// keeps programs after the last login ends (systemd lingering); nil when
	// hc does not know.
	KeepsRunning    *bool `json:"keeps_running_after_logout,omitempty"`
	LegacyInstalled bool  `json:"legacy_installed,omitempty"`
}

type herdrProbeCall struct {
	done   chan struct{}
	forced bool
	status herdrGuestStatus
	err    error
}

type herdrManager struct {
	opMu          sync.Mutex
	mu            sync.Mutex
	configLoaded  bool
	config        herdrConfig
	statusCache   herdrGuestStatus
	statusAt      time.Time
	probeInflight *herdrProbeCall
	// The hc terminal list and Cage's own records (herdr_terminals.go):
	// fetchMu lets one list read run at a time, listMu guards the rest.
	fetchMu   sync.Mutex
	listMu    sync.Mutex
	list      herdrList
	listAt    time.Time
	records   map[string]herdrRecord
	recordsOK bool
}

func (s *Server) loadHerdrConfigLocked() (herdrConfig, error) {
	m := &s.herdr
	if m.configLoaded {
		return m.config, nil
	}
	cfg := defaultHerdrConfig()
	data, err := os.ReadFile(filepath.Join(s.profile, "herdr.json"))
	if os.IsNotExist(err) {
		m.config, m.configLoaded = cfg, true
		return cfg, nil
	}
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return cfg, errors.New("Herdr 配置文件无法读取，请检查配置目录")
	}
	m.config = cfg
	m.configLoaded = true
	return cfg, nil
}

func (s *Server) saveHerdrConfigLocked(cfg herdrConfig) error {
	if err := writePrivate(filepath.Join(s.profile, "herdr.json"), cfg); err != nil {
		return errors.New("Herdr 设置未能保存，请检查磁盘空间和权限")
	}
	s.herdr.config = cfg
	s.herdr.configLoaded = true
	return nil
}

func (s *Server) herdrConfig() (herdrConfig, error) {
	s.herdr.mu.Lock()
	defer s.herdr.mu.Unlock()
	return s.loadHerdrConfigLocked()
}

func (s *Server) invalidateHerdrStatus() {
	s.herdr.mu.Lock()
	s.herdr.statusAt = time.Time{}
	s.herdr.mu.Unlock()
	s.invalidateHerdrList()
}

func (s *Server) probeHerdrStatus(ctx context.Context, force bool) (herdrGuestStatus, error) {
	m := &s.herdr
	for {
		m.mu.Lock()
		if !force && !m.statusAt.IsZero() && time.Since(m.statusAt) < herdrStatusTTL {
			cached := m.statusCache
			m.mu.Unlock()
			return cached, nil
		}
		if call := m.probeInflight; call != nil {
			if !force || call.forced {
				m.mu.Unlock()
				select {
				case <-ctx.Done():
					return herdrGuestStatus{}, ctx.Err()
				case <-call.done:
					return call.status, call.err
				}
			}
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return herdrGuestStatus{}, ctx.Err()
			case <-call.done:
				continue
			}
		}
		call := &herdrProbeCall{done: make(chan struct{}), forced: force}
		m.probeInflight = call
		m.mu.Unlock()

		ensureCtx, ensureCancel := context.WithTimeout(s.lifetimeContext(), coldEnsureTimeout)
		err := s.ensureAgentRuntime(ensureCtx)
		ensureCancel()
		var st herdrGuestStatus
		if err == nil {
			probeCtx, cancel := context.WithTimeout(s.lifetimeContext(), statusProbeTimeout)
			err = s.runAgentRuntime(probeCtx, "herdr-status", map[string]any{}, &st)
			cancel()
		}
		if err == nil {
			st.BinaryPath = herdrText(st.BinaryPath, 512)
			st.Version = herdrText(st.Version, 32)
		}
		m.mu.Lock()
		if err == nil {
			m.statusCache = st
			m.statusAt = time.Now()
			if cfg, loadErr := s.loadHerdrConfigLocked(); loadErr == nil && cfg.InstalledHint != st.Installed {
				cfg.InstalledHint = st.Installed
				_ = s.saveHerdrConfigLocked(cfg)
			}
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

func (s *Server) herdrStatusPayload(r *http.Request, force bool) (map[string]any, error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	cfg, err := s.herdrConfig()
	if err != nil {
		return nil, err
	}

	// CRITICAL: call s.vmRunning() BEFORE s.mu.Lock() — on Windows guests
	// vmRunning() acquires s.mu internally, which would deadlock if nested.
	runningVM := s.vmRunning()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()

	supportsManaged := s.windowsGuest == nil

	var st herdrGuestStatus
	probeErr := ""
	// Disabled means no guest call at all.
	if cfg.Enabled && ready && runningVM && supportsManaged {
		pCtx, cancel := context.WithTimeout(ctx, s.statusProbeWaitTimeout())
		st, err = s.probeHerdrStatus(pCtx, force)
		cancel()
		if err != nil {
			probeErr = err.Error()
		}
	}

	return map[string]any{
		"enabled":                    cfg.Enabled,
		"takeover_terminals":         cfg.TakeoverTerminals,
		"installed":                  st.Installed,
		"version":                    st.Version,
		"binary_path":                st.BinaryPath,
		"server_running":             st.ServerRunning,
		"terminals_count":            st.TerminalsCount,
		"keeps_running_after_logout": st.KeepsRunning,
		"legacy_installed":           st.LegacyInstalled,
		"error":                      probeErr,
		"vm_running":                 runningVM,
		"ready":                      ready,
		"supports_managed":           supportsManaged,
	}, nil
}

func (s *Server) herdrStatusHTTP(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	payload, err := s.herdrStatusPayload(r, force)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) herdrConfigHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled           *bool `json:"enabled"`
		TakeoverTerminals *bool `json:"takeover_terminals"`
		// Theme was the Herdr TUI's; an older client may still send it.
		Theme *string `json:"theme"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.herdr.opMu.Lock()
	defer s.herdr.opMu.Unlock()

	cfg, err := s.herdrConfig()
	if err != nil {
		requestError(w, err)
		return
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.TakeoverTerminals != nil {
		cfg.TakeoverTerminals = *in.TakeoverTerminals
	}
	if cfg.Enabled && s.windowsGuest != nil {
		requestError(w, errors.New("当前 Windows 原生客机暂不支持托管 Herdr 终端"))
		return
	}

	s.herdr.mu.Lock()
	saveErr := s.saveHerdrConfigLocked(cfg)
	s.herdr.mu.Unlock()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	s.invalidateHerdrList()

	payload, err := s.herdrStatusPayload(r, false)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

// herdrJSONBody reads an optional JSON body into v: absent is fine.
func herdrJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength != 0 || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return readJSON(w, r, v)
	}
	return true
}

func (s *Server) herdrInstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Channel string `json:"channel"`
		Update  bool   `json:"update"`
	}
	if !herdrJSONBody(w, r, &in) {
		return
	}
	channel := strings.TrimSpace(in.Channel)
	if channel != "" && channel != "official" {
		http.Error(w, "无效的 Herdr 安装渠道", http.StatusBadRequest)
		return
	}

	running := s.vmRunning()
	s.mu.Lock()
	ready, paused, closing := s.ready, s.networkPaused, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境并等待就绪后再安装 Herdr", http.StatusConflict)
		return
	}
	if paused {
		http.Error(w, "网络已暂停，请先恢复网络连接后再安装 Herdr", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键远程安装 Herdr", http.StatusConflict)
		return
	}

	s.launch(w, "herdr-install", func() error {
		s.herdr.opMu.Lock()
		defer s.herdr.opMu.Unlock()
		s.setMessage("正在通过官方发布渠道安装 Herdr（herdr-core）…")
		if err := s.runAgentRuntimeJob(s.operationContext(), "herdr-install", map[string]any{"update": in.Update}); err != nil {
			return errors.New("安装 Herdr 失败，请查看操作详情排查网络或运行环境")
		}
		s.herdr.mu.Lock()
		if cfg, err := s.loadHerdrConfigLocked(); err == nil {
			cfg.Enabled = true
			cfg.InstalledHint = true
			_ = s.saveHerdrConfigLocked(cfg)
		}
		s.herdr.statusAt = time.Time{}
		s.herdr.mu.Unlock()
		s.setMessage("Herdr 安装完成，正在验证状态…")
		return nil
	})
}

func (s *Server) herdrUninstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Force bool `json:"force"`
	}
	if !herdrJSONBody(w, r, &in) {
		return
	}
	running := s.vmRunning()
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境并等待就绪后再卸载 Herdr", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键卸载 Herdr", http.StatusConflict)
		return
	}

	s.launch(w, "herdr-uninstall", func() error {
		s.herdr.opMu.Lock()
		defer s.herdr.opMu.Unlock()
		s.setMessage("正在卸载 Herdr（保留终端记录与设置）…")
		if err := s.runAgentRuntimeJob(s.operationContext(), "herdr-uninstall", map[string]any{"force": in.Force}); err != nil {
			return errors.New("卸载 Herdr 失败，请查看操作详情")
		}
		s.herdr.mu.Lock()
		if cfg, err := s.loadHerdrConfigLocked(); err == nil {
			cfg.Enabled = false
			cfg.InstalledHint = false
			_ = s.saveHerdrConfigLocked(cfg)
		}
		s.herdr.statusCache = herdrGuestStatus{}
		s.herdr.statusAt = time.Time{}
		s.herdr.mu.Unlock()
		s.invalidateHerdrList()
		s.setMessage("Herdr 已卸载")
		return nil
	})
}

func (s *Server) herdrActionHTTP(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimSpace(r.PathValue("action"))
	if action != "start" && action != "stop" && action != "restart" {
		http.NotFound(w, r)
		return
	}
	// force: stop even while terminals run (stopping the core ends them).
	var in struct {
		Force bool `json:"force"`
	}
	if !herdrJSONBody(w, r, &in) {
		return
	}
	s.herdr.opMu.Lock()
	defer s.herdr.opMu.Unlock()

	if s.windowsGuest != nil {
		http.Error(w, "Windows 原生客机暂不支持托管 Herdr 服务", http.StatusConflict)
		return
	}
	cfg, err := s.herdrConfig()
	if err != nil {
		requestError(w, err)
		return
	}
	if action != "stop" && !cfg.Enabled {
		http.Error(w, "请先在「设置 → Agent CLI」中启用 Herdr 后再启动服务", http.StatusConflict)
		return
	}
	if !s.vmRunning() {
		if action == "stop" {
			jsonReplyHerdr(s, w, r)
			return
		}
		http.Error(w, "请先启动 Cage 环境后再运行 Herdr", http.StatusConflict)
		return
	}
	timeout := 15 * time.Second
	if action == "restart" {
		timeout = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	runErr := s.runAgentRuntime(ctx, "herdr-"+action, map[string]any{"force": in.Force}, nil)
	cancel()
	if runErr != nil {
		http.Error(w, runErr.Error(), http.StatusConflict)
		return
	}
	s.invalidateHerdrStatus()
	jsonReplyHerdr(s, w, r)
}

func jsonReplyHerdr(s *Server, w http.ResponseWriter, r *http.Request) {
	payload, err := s.herdrStatusPayload(r, true)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

// herdrLegacyHTTP reports the Herdr TUI (herdr) Super Cage 0.1.21 installed,
// which may still run with live panes beside herdr-core.
func (s *Server) herdrLegacyHTTP(w http.ResponseWriter, r *http.Request) {
	if s.windowsGuest != nil || !s.vmRunning() {
		jsonReply(w, map[string]any{"installed": false})
		return
	}
	var st map[string]any
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	err := s.runAgentRuntime(ctx, "herdr-legacy-status", map[string]any{}, &st)
	cancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, herdrLegacyView(st))
}

// herdrLegacyUninstallHTTP stops the old Herdr TUI when it holds no panes
// (or when forced) and removes its program; ~/.config/herdr stays.
func (s *Server) herdrLegacyUninstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Force bool `json:"force"`
	}
	if !herdrJSONBody(w, r, &in) {
		return
	}
	if s.windowsGuest != nil || !s.vmRunning() {
		http.Error(w, "请先启动 Cage 环境", http.StatusConflict)
		return
	}
	s.herdr.opMu.Lock()
	defer s.herdr.opMu.Unlock()
	var st map[string]any
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	err := s.runAgentRuntime(ctx, "herdr-legacy-uninstall", map[string]any{"force": in.Force}, &st)
	cancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.invalidateHerdrStatus()
	jsonReply(w, herdrLegacyView(st))
}

// herdrLegacyView keeps only the fields the page shows, cleaned.
func herdrLegacyView(st map[string]any) map[string]any {
	view := map[string]any{"installed": st["installed"] == true, "server_running": st["server_running"] == true, "busy": st["busy"] == true, "panes": 0}
	if panes, ok := st["panes"].(float64); ok && panes >= 0 && panes < 10000 {
		view["panes"] = int(panes)
	}
	if version, ok := st["version"].(string); ok {
		view["version"] = herdrText(version, 32)
	}
	return view
}

// shouldTakeoverWithHerdr reports whether newly launched workbench terminals
// should be hc terminals, with zero blocking SSH calls.
func (s *Server) shouldTakeoverWithHerdr() bool {
	if s.windowsGuest != nil {
		return false
	}
	s.herdr.mu.Lock()
	cfg, err := s.loadHerdrConfigLocked()
	probedRecently := !s.herdr.statusAt.IsZero() && time.Since(s.herdr.statusAt) < herdrStatusTTL
	cachedInstalled := s.herdr.statusCache.Installed
	s.herdr.mu.Unlock()
	if err != nil || !cfg.Enabled || !cfg.TakeoverTerminals {
		return false
	}
	if probedRecently {
		return cachedInstalled
	}
	return cfg.InstalledHint
}

// herdrAttach is what one terminal asks agent-runtime.py herdr-attach for.
// Mode create starts the hc terminal Name (unless it lives) running Inner,
// then views it; attach views the live Target (a name or tN) and never
// starts the core; picker opens hc's own picker.
type herdrAttach struct {
	Mode, Name, Target string
	Workspace, Project string
	Title, Kind, Inner string
}

// herdrAttachScript builds an injection-safe POSIX command that passes every
// value as a shellescape-quoted environment variable to herdr-attach.
func (s *Server) herdrAttachScript(a herdrAttach) string {
	shellBin := "/bin/bash"
	if s.macosGuest != nil {
		shellBin = "/bin/zsh"
	}
	clean := func(v string) string {
		return strings.ReplaceAll(v, "\x00", "")
	}
	return "exec env " +
		"TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 " +
		"CAGE_HERDR_MODE=" + shellescape.Quote(clean(a.Mode)) + " " +
		"CAGE_HERDR_NAME=" + shellescape.Quote(clean(a.Name)) + " " +
		"CAGE_HERDR_TARGET=" + shellescape.Quote(clean(a.Target)) + " " +
		"CAGE_HERDR_WORKSPACE=" + shellescape.Quote(clean(a.Workspace)) + " " +
		"CAGE_HERDR_PROJECT_LABEL=" + shellescape.Quote(clean(a.Project)) + " " +
		"CAGE_HERDR_TAB_LABEL=" + shellescape.Quote(clean(a.Title)) + " " +
		"CAGE_HERDR_KIND=" + shellescape.Quote(clean(a.Kind)) + " " +
		"CAGE_HERDR_INNER_CMD=" + shellescape.Quote(clean(a.Inner)) + " " +
		"CAGE_HERDR_SHELL=" + shellescape.Quote(shellBin) + " " +
		agentRuntimeCommandWithPython(s.guestPythonBinary(), "herdr-attach")
}
