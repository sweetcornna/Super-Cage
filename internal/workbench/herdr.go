package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
)

const herdrStatusTTL = 10 * time.Second

var validHerdrThemes = []string{
	"default",
	"terminal",
	"catppuccin",
	"tokyo-night",
	"dracula",
	"nord",
	"gruvbox",
	"one-dark",
	"solarized",
	"kanagawa",
	"rose-pine",
	"vesper",
}

func validHerdrTheme(theme string) bool {
	return slices.Contains(validHerdrThemes, theme)
}

// herdrConfig persists the user's opt-in Herdr terminal orchestration preferences.
// Disabled by default (zero overhead until enabled).
type herdrConfig struct {
	Enabled           bool   `json:"enabled"`
	TakeoverTerminals bool   `json:"takeover_terminals"`       // when installed & enabled, route workbench terminals into Herdr
	Theme             string `json:"theme"`                    // default | terminal | catppuccin | tokyo-night | dracula | nord | gruvbox | one-dark | solarized | kanagawa | rose-pine | vesper
	InstalledHint     bool   `json:"installed_hint,omitempty"` // fast hint set on install/probe so terminal launch needs 0 extra SSH RTT
}

func defaultHerdrConfig() herdrConfig {
	return herdrConfig{
		Enabled:           false,
		TakeoverTerminals: true,
		Theme:             "default",
	}
}

func (c herdrConfig) normalized() herdrConfig {
	c.Theme = strings.TrimSpace(strings.ToLower(c.Theme))
	if !validHerdrTheme(c.Theme) {
		c.Theme = "default"
	}
	return c
}

type herdrGuestStatus struct {
	Installed          bool   `json:"installed"`
	BinaryPath         string `json:"binary_path,omitempty"`
	Version            string `json:"version,omitempty"`
	ServerRunning      bool   `json:"server_running"`
	WorkspacesCount    int    `json:"workspaces_count,omitempty"`
	AgentsCount        int    `json:"agents_count,omitempty"`
	BlockedAgentsCount int    `json:"blocked_agents_count,omitempty"`
	// SettingsKeys is how a client opens Herdr's own settings (official_settings.go).
	SettingsKeys *herdrSettingsKeys `json:"settings_keys,omitempty"`
}

type herdrManager struct {
	opMu         sync.Mutex
	mu           sync.Mutex
	configLoaded bool
	config       herdrConfig
	statusCache  herdrGuestStatus
	statusAt     time.Time
}

func (s *Server) loadHerdrConfigLocked() (herdrConfig, error) {
	m := &s.herdr
	if m.configLoaded {
		return m.config.normalized(), nil
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
	cfg = cfg.normalized()
	m.config = cfg
	m.configLoaded = true
	return cfg, nil
}

func (s *Server) saveHerdrConfigLocked(cfg herdrConfig) error {
	clean := cfg.normalized()
	if err := writePrivate(filepath.Join(s.profile, "herdr.json"), clean); err != nil {
		return errors.New("Herdr 设置未能保存，请检查磁盘空间和权限")
	}
	s.herdr.config = clean
	s.herdr.configLoaded = true
	return nil
}

func (s *Server) invalidateHerdrStatus() {
	s.herdr.mu.Lock()
	s.herdr.statusAt = time.Time{}
	s.herdr.mu.Unlock()
}

func (s *Server) probeHerdrStatus(ctx context.Context, force bool) (herdrGuestStatus, error) {
	s.herdr.mu.Lock()
	if !force && !s.herdr.statusAt.IsZero() && time.Since(s.herdr.statusAt) < herdrStatusTTL {
		cached := s.herdr.statusCache
		s.herdr.mu.Unlock()
		return cached, nil
	}
	s.herdr.mu.Unlock()

	var st herdrGuestStatus
	err := s.runAgentRuntime(ctx, "herdr-status", map[string]any{}, &st)
	if err != nil {
		return st, err
	}
	s.herdr.mu.Lock()
	s.herdr.statusCache = st
	s.herdr.statusAt = time.Now()
	if cfg, loadErr := s.loadHerdrConfigLocked(); loadErr == nil && cfg.InstalledHint != st.Installed {
		cfg.InstalledHint = st.Installed
		_ = s.saveHerdrConfigLocked(cfg)
	}
	s.herdr.mu.Unlock()
	return st, nil
}

func (s *Server) herdrStatusPayload(r *http.Request, force bool) (map[string]any, error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	s.herdr.mu.Lock()
	cfg, err := s.loadHerdrConfigLocked()
	s.herdr.mu.Unlock()
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
	if ready && runningVM && supportsManaged {
		pCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		st, err = s.probeHerdrStatus(pCtx, force)
		cancel()
		if err != nil {
			probeErr = err.Error()
		}
	}

	return map[string]any{
		"enabled":              cfg.Enabled,
		"takeover_terminals":   cfg.TakeoverTerminals,
		"theme":                cfg.Theme,
		"installed":            st.Installed,
		"version":              st.Version,
		"binary_path":          st.BinaryPath,
		"server_running":       st.ServerRunning,
		"workspaces_count":     st.WorkspacesCount,
		"agents_count":         st.AgentsCount,
		"blocked_agents_count": st.BlockedAgentsCount,
		"settings_keys":        st.SettingsKeys.sanitized(),
		"error":                probeErr,
		"vm_running":           runningVM,
		"ready":                ready,
		"supports_managed":     supportsManaged,
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
		Enabled           *bool  `json:"enabled"`
		TakeoverTerminals *bool  `json:"takeover_terminals"`
		Theme             string `json:"theme"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.herdr.opMu.Lock()
	defer s.herdr.opMu.Unlock()

	s.herdr.mu.Lock()
	cfg, err := s.loadHerdrConfigLocked()
	cachedInstalled := cfg.InstalledHint || (!s.herdr.statusAt.IsZero() && s.herdr.statusCache.Installed)
	s.herdr.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}

	prevTheme := cfg.Theme
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.TakeoverTerminals != nil {
		cfg.TakeoverTerminals = *in.TakeoverTerminals
	}
	if in.Theme != "" {
		theme := strings.TrimSpace(strings.ToLower(in.Theme))
		if !validHerdrTheme(theme) {
			requestError(w, errors.New("Herdr 主题名称无效"))
			return
		}
		cfg.Theme = theme
	}
	if cfg.Enabled && s.windowsGuest != nil {
		requestError(w, errors.New("当前 Windows 原生客机暂不支持托管 Herdr 终端"))
		return
	}

	if cfg.Theme != prevTheme && s.vmRunning() && s.windowsGuest == nil {
		installed := cachedInstalled
		if !installed {
			pCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			if st, probeErr := s.probeHerdrStatus(pCtx, false); probeErr == nil {
				installed = st.Installed
			}
			cancel()
		}
		if installed {
			tCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			applyErr := s.runAgentRuntime(tCtx, "herdr-apply-theme", map[string]any{"theme": cfg.Theme}, nil)
			cancel()
			if applyErr != nil {
				http.Error(w, fmt.Sprintf("应用 Herdr 主题失败: %v", applyErr), http.StatusConflict)
				return
			}
		}
	}

	s.herdr.mu.Lock()
	saveErr := s.saveHerdrConfigLocked(cfg)
	s.herdr.mu.Unlock()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}

	payload, err := s.herdrStatusPayload(r, false)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) herdrInstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Channel string `json:"channel"`
	}
	if r.ContentLength != 0 || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if !readJSON(w, r, &in) {
			return
		}
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
		s.setMessage("正在通过官方渠道安装 Herdr…")
		payloadMap := map[string]any{}
		if channel != "" {
			payloadMap["channel"] = channel
		}
		payload, _ := json.Marshal(payloadMap)
		script := s.guestShellWrapper(agentRuntimeCommandWithPython(s.guestPythonBinary(), "herdr-install"))
		cmd := s.sshCommand(s.operationContext(), "-T", s.sshTarget(), "--", script)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = s.jobs.execution()
		cmd.Stderr = s.jobs.execution()
		if err := cmd.Run(); err != nil {
			return errors.New("安装 Herdr 失败，请查看操作详情排查网络或运行环境")
		}
		var themeToApply string
		s.herdr.mu.Lock()
		if cfg, err := s.loadHerdrConfigLocked(); err == nil {
			cfg.Enabled = true
			cfg.TakeoverTerminals = true
			cfg.InstalledHint = true
			themeToApply = cfg.Theme
			_ = s.saveHerdrConfigLocked(cfg)
		}
		s.herdr.statusAt = time.Time{}
		s.herdr.mu.Unlock()
		if themeToApply != "" && themeToApply != "default" {
			tCtx, cancel := context.WithTimeout(s.operationContext(), 8*time.Second)
			_ = s.runAgentRuntime(tCtx, "herdr-apply-theme", map[string]any{"theme": themeToApply}, nil)
			cancel()
		}
		s.setMessage("Herdr 安装完成，正在验证状态…")
		return nil
	})
}

func (s *Server) herdrUninstallHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct{}
		if !readJSON(w, r, &in) {
			return
		}
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
		s.setMessage("正在卸载 Herdr（保留项目数据）…")
		script := s.guestShellWrapper(agentRuntimeCommandWithPython(s.guestPythonBinary(), "herdr-uninstall"))
		cmd := s.sshCommand(s.operationContext(), "-T", s.sshTarget(), "--", script)
		cmd.Stdin = bytes.NewReader([]byte("{}"))
		cmd.Stdout = s.jobs.execution()
		cmd.Stderr = s.jobs.execution()
		if err := cmd.Run(); err != nil {
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
	s.herdr.opMu.Lock()
	defer s.herdr.opMu.Unlock()

	if s.windowsGuest != nil {
		http.Error(w, "Windows 原生客机暂不支持托管 Herdr 服务", http.StatusConflict)
		return
	}

	s.herdr.mu.Lock()
	cfg, err := s.loadHerdrConfigLocked()
	s.herdr.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}

	switch action {
	case "start":
		if !cfg.Enabled {
			http.Error(w, "请先在「设置 → Agent CLI」中启用 Herdr 接入后再启动服务", http.StatusConflict)
			return
		}
		if !s.vmRunning() {
			http.Error(w, "请先启动 Cage 环境后再运行 Herdr", http.StatusConflict)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		runErr := s.runAgentRuntime(ctx, "herdr-start", map[string]any{"workspace": s.cliWorkspace(s.workspace())}, nil)
		cancel()
		if runErr != nil {
			http.Error(w, runErr.Error(), http.StatusConflict)
			return
		}
	case "stop":
		if s.vmRunning() {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			runErr := s.runAgentRuntime(ctx, "herdr-stop", map[string]any{}, nil)
			cancel()
			if runErr != nil {
				http.Error(w, runErr.Error(), http.StatusConflict)
				return
			}
		}
	case "restart":
		if !cfg.Enabled {
			http.Error(w, "请先在「设置 → Agent CLI」中启用 Herdr 接入后再重启服务", http.StatusConflict)
			return
		}
		if !s.vmRunning() {
			http.Error(w, "请先启动 Cage 环境后再运行 Herdr", http.StatusConflict)
			return
		}
		stopCtx, stopCancel := context.WithTimeout(r.Context(), 10*time.Second)
		stopErr := s.runAgentRuntime(stopCtx, "herdr-stop", map[string]any{}, nil)
		stopCancel()
		if stopErr != nil {
			http.Error(w, stopErr.Error(), http.StatusConflict)
			return
		}
		startCtx, startCancel := context.WithTimeout(r.Context(), 15*time.Second)
		startErr := s.runAgentRuntime(startCtx, "herdr-start", map[string]any{"workspace": s.cliWorkspace(s.workspace())}, nil)
		startCancel()
		if startErr != nil {
			http.Error(w, startErr.Error(), http.StatusConflict)
			return
		}
	}

	s.invalidateHerdrStatus()
	payload, err := s.herdrStatusPayload(r, true)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

// shouldTakeoverWithHerdr reports whether newly launched workbench terminals
// should route into Herdr with zero blocking SSH calls.
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

// herdrAttachScript constructs an injection-safe POSIX shell command that
// passes workspace, tab metadata, and the inner launch script strictly via
// shellescape-quoted environment variables into `agent-runtime.py herdr-attach`.
func (s *Server) herdrAttachScript(workspace, projectLabel, tabLabel, kind, innerScript string) string {
	shellBin := "/bin/bash"
	if s.macosGuest != nil {
		shellBin = "/bin/zsh"
	}
	clean := func(v string) string {
		return strings.ReplaceAll(v, "\x00", "")
	}
	return "exec env " +
		"TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 " +
		"CAGE_HERDR_WORKSPACE=" + shellescape.Quote(clean(workspace)) + " " +
		"CAGE_HERDR_PROJECT_LABEL=" + shellescape.Quote(clean(projectLabel)) + " " +
		"CAGE_HERDR_TAB_LABEL=" + shellescape.Quote(clean(tabLabel)) + " " +
		"CAGE_HERDR_KIND=" + shellescape.Quote(clean(kind)) + " " +
		"CAGE_HERDR_INNER_CMD=" + shellescape.Quote(clean(innerScript)) + " " +
		"CAGE_HERDR_SHELL=" + shellescape.Quote(shellBin) + " " +
		agentRuntimeCommandWithPython(s.guestPythonBinary(), "herdr-attach")
}
