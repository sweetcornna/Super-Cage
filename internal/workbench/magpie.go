package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
)

const (
	defaultMagpiePort        = 3430
	defaultMagpieGatewayPort = 3425
	magpieStatusTTL          = 10 * time.Second
)

// magpieConfig persists the user's opt-in Magpie (yetone/magpie) integration preferences.
// Disabled by default (zero overhead until enabled).
type magpieConfig struct {
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"`                   // managed | external
	ExternalURL string `json:"external_url,omitempty"` // when Mode == "external"
	Port        int    `json:"port"`                   // managed web UI port (default 3430)
	GatewayOnly bool   `json:"gateway_only"`           // pass --gateway to magpie web
	OpenMode    string `json:"open_mode"`              // workbench | direct
	FocusChrome bool   `json:"focus_chrome"`           // 36px distraction-free header
}

func defaultMagpieConfig() magpieConfig {
	return magpieConfig{
		Enabled:     false,
		Mode:        "managed",
		Port:        defaultMagpiePort,
		GatewayOnly: false,
		OpenMode:    "workbench",
		FocusChrome: true,
	}
}

func (c magpieConfig) normalized() magpieConfig {
	if c.Mode != "managed" && c.Mode != "external" {
		c.Mode = "managed"
	}
	if c.Port < 1024 || c.Port > 65535 || c.Port == defaultMagpieGatewayPort {
		c.Port = defaultMagpiePort
	}
	if c.OpenMode != "workbench" && c.OpenMode != "direct" {
		c.OpenMode = "workbench"
	}
	return c
}

func validateMagpieExternalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 1024 || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return "", errors.New("Magpie 网页地址格式无效")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("Magpie 地址须为有效的 HTTP(S) URL 且不能含用户名密码")
	}
	host := strings.ToLower(u.Hostname())
	isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if u.Scheme == "http" && !isLoopback {
		return "", errors.New("非本机回环地址的 Magpie 实例必须使用 HTTPS")
	}
	if strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, `\`) {
		return "", errors.New("Magpie 网页路径格式无效")
	}
	u.Fragment = ""
	if u.RawQuery == "" {
		return strings.TrimRight(u.String(), "/") + "/", nil
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// sanitizeMagpieDisplayURL strips query auth secrets (e.g. ?k=... or ?token=...)
// from URLs echoed back to UI headers/tab state while preserving the origin+path.
func sanitizeMagpieDisplayURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Has("k") || q.Has("token") {
		u.RawQuery = ""
		u.Fragment = ""
		if u.Path == "" {
			u.Path = "/"
		}
	}
	return u.String()
}

type magpieGuestStatus struct {
	Installed      bool   `json:"installed"`
	BinaryPath     string `json:"binary_path,omitempty"`
	Version        string `json:"version,omitempty"`
	Running        bool   `json:"running"`
	PID            int    `json:"pid,omitempty"`
	Port           int    `json:"port,omitempty"`
	GatewayPort    int    `json:"gateway_port,omitempty"`
	GatewayRunning bool   `json:"gateway_running"`
	GatewayOnly    bool   `json:"gateway_only,omitempty"`
}

type magpieManager struct {
	opMu         sync.Mutex
	mu           sync.Mutex
	configLoaded bool
	config       magpieConfig
	statusCache  magpieGuestStatus
	statusAt     time.Time
	proxy        *t3CodeProxy
}

func (s *Server) loadMagpieConfigLocked() (magpieConfig, error) {
	m := &s.magpie
	if m.configLoaded {
		return m.config.normalized(), nil
	}
	cfg := defaultMagpieConfig()
	data, err := os.ReadFile(filepath.Join(s.profile, "magpie.json"))
	if os.IsNotExist(err) {
		m.config, m.configLoaded = cfg, true
		return cfg, nil
	}
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return cfg, errors.New("Magpie 配置文件无法读取，请检查配置目录")
	}
	cfg = cfg.normalized()
	m.config = cfg
	m.configLoaded = true
	return cfg, nil
}

func (s *Server) saveMagpieConfigLocked(cfg magpieConfig) error {
	clean := cfg.normalized()
	if err := writePrivate(filepath.Join(s.profile, "magpie.json"), clean); err != nil {
		return errors.New("Magpie 设置未能保存，请检查磁盘空间和权限")
	}
	s.magpie.config = clean
	s.magpie.configLoaded = true
	return nil
}

func (s *Server) closeMagpieProxy() {
	s.closeDirectWebProxy(&s.magpie.mu, &s.magpie.proxy)
}

func (s *Server) invalidateMagpieStatus() {
	s.magpie.mu.Lock()
	s.magpie.statusAt = time.Time{}
	s.magpie.mu.Unlock()
}

func (s *Server) ensureMagpieProxy(ctx context.Context, cfg magpieConfig) (*t3CodeProxy, error) {
	return s.ensureDirectWebProxy(ctx, &s.magpie.mu, &s.magpie.proxy, directProxySpec{
		appName:      "Magpie",
		authPath:     "/_cage_magpie_auth",
		cookiePrefix: "cage_magpie_session_",
		mode:         cfg.Mode,
		port:         cfg.Port,
		externalURL:  cfg.ExternalURL,
	})
}

func (s *Server) probeMagpieStatus(ctx context.Context, port int, force bool) (magpieGuestStatus, error) {
	s.magpie.mu.Lock()
	if !force && !s.magpie.statusAt.IsZero() && time.Since(s.magpie.statusAt) < magpieStatusTTL && s.magpie.statusCache.Port == port {
		cached := s.magpie.statusCache
		s.magpie.mu.Unlock()
		return cached, nil
	}
	s.magpie.mu.Unlock()

	var st magpieGuestStatus
	err := s.runAgentRuntime(ctx, "magpie-status", map[string]any{
		"port":         port,
		"gateway_port": defaultMagpieGatewayPort,
	}, &st)
	if err != nil {
		return st, err
	}
	if st.Port == 0 {
		st.Port = port
	}
	if st.GatewayPort == 0 {
		st.GatewayPort = defaultMagpieGatewayPort
	}
	s.magpie.mu.Lock()
	s.magpie.statusCache = st
	s.magpie.statusAt = time.Now()
	s.magpie.mu.Unlock()
	return st, nil
}

func (s *Server) magpieStatusPayload(r *http.Request, force bool) (map[string]any, error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	s.magpie.mu.Lock()
	cfg, err := s.loadMagpieConfigLocked()
	s.magpie.mu.Unlock()
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
	directAvailable := s.t3CodeDirectAvailable(r)

	var st magpieGuestStatus
	probeErr := ""
	if cfg.Mode == "managed" && ready && runningVM && supportsManaged {
		pCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		st, err = s.probeMagpieStatus(pCtx, cfg.Port, force)
		cancel()
		if err != nil {
			probeErr = err.Error()
		}
	}

	displayURL := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	if cfg.Mode == "external" {
		displayURL = sanitizeMagpieDisplayURL(cfg.ExternalURL)
		st.Running = cfg.ExternalURL != ""
	}
	gatewayPort := st.GatewayPort
	if gatewayPort == 0 {
		gatewayPort = defaultMagpieGatewayPort
	}
	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", gatewayPort)

	return map[string]any{
		"enabled":          cfg.Enabled,
		"mode":             cfg.Mode,
		"external_url":     cfg.ExternalURL,
		"port":             cfg.Port,
		"gateway_port":     gatewayPort,
		"gateway_only":     cfg.GatewayOnly,
		"open_mode":        cfg.OpenMode,
		"focus_chrome":     cfg.FocusChrome,
		"installed":        st.Installed,
		"version":          st.Version,
		"running":          st.Running,
		"gateway_running":  st.GatewayRunning,
		"url":              displayURL,
		"gateway_url":      gatewayURL,
		"error":            probeErr,
		"vm_running":       runningVM,
		"ready":            ready,
		"supports_managed": supportsManaged,
		"direct_available": directAvailable,
	}, nil
}

func (s *Server) magpieStatusHTTP(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	payload, err := s.magpieStatusPayload(r, force)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) magpieConfigHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled     *bool   `json:"enabled"`
		Mode        string  `json:"mode"`
		ExternalURL *string `json:"external_url"`
		Port        *int    `json:"port"`
		GatewayOnly *bool   `json:"gateway_only"`
		OpenMode    string  `json:"open_mode"`
		FocusChrome *bool   `json:"focus_chrome"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	// Serialize config transitions with start/stop/restart/open so host state and
	// guest runtime cannot interleave.
	s.magpie.opMu.Lock()
	defer s.magpie.opMu.Unlock()

	s.magpie.mu.Lock()
	cfg, err := s.loadMagpieConfigLocked()
	s.magpie.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	prevEnabled, prevMode, prevPort, prevGatewayOnly := cfg.Enabled, cfg.Mode, cfg.Port, cfg.GatewayOnly
	var stopProxy bool
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
		if !cfg.Enabled {
			stopProxy = true
		}
	}
	if in.Mode != "" {
		if in.Mode != "managed" && in.Mode != "external" {
			requestError(w, errors.New("Magpie 接入模式无效"))
			return
		}
		if cfg.Mode != in.Mode {
			stopProxy = true
		}
		cfg.Mode = in.Mode
	}
	if in.ExternalURL != nil {
		clean, err := validateMagpieExternalURL(*in.ExternalURL)
		if err != nil {
			requestError(w, err)
			return
		}
		if cfg.ExternalURL != clean {
			stopProxy = true
		}
		cfg.ExternalURL = clean
	}
	if in.Port != nil {
		if *in.Port < 1024 || *in.Port > 65535 {
			requestError(w, errors.New("端口须在 1024 到 65535 之间"))
			return
		}
		if *in.Port == defaultMagpieGatewayPort {
			requestError(w, fmt.Errorf("端口 %d 为 Magpie API 网关保留端口，请选择其他端口", defaultMagpieGatewayPort))
			return
		}
		if cfg.Port != *in.Port {
			stopProxy = true
		}
		cfg.Port = *in.Port
	}
	if in.GatewayOnly != nil {
		if cfg.GatewayOnly != *in.GatewayOnly {
			stopProxy = true
		}
		cfg.GatewayOnly = *in.GatewayOnly
	}
	if in.OpenMode != "" {
		if in.OpenMode != "workbench" && in.OpenMode != "direct" {
			requestError(w, errors.New("打开模式无效"))
			return
		}
		cfg.OpenMode = in.OpenMode
	}
	if in.FocusChrome != nil {
		cfg.FocusChrome = *in.FocusChrome
	}

	if cfg.Enabled && cfg.Mode == "managed" && s.windowsGuest != nil {
		requestError(w, errors.New("当前 Windows 原生客机暂不支持托管 Magpie，请切换为外部地址接入"))
		return
	}
	if cfg.Enabled && cfg.Mode == "external" && cfg.ExternalURL == "" {
		requestError(w, errors.New("外部接入模式请先填写 Magpie 网页地址"))
		return
	}

	needStopManaged := prevMode == "managed" && ((prevEnabled && !cfg.Enabled) || cfg.Mode != "managed" || cfg.Port != prevPort || cfg.GatewayOnly != prevGatewayOnly)
	if needStopManaged && s.vmRunning() && s.windowsGuest == nil {
		stopCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		stopErr := s.runAgentRuntime(stopCtx, "magpie-stop", map[string]any{"port": prevPort}, nil)
		cancel()
		if stopErr != nil {
			http.Error(w, fmt.Sprintf("停止原托管 Magpie 服务失败，配置未变更: %v", stopErr), http.StatusConflict)
			return
		}
	}

	s.magpie.mu.Lock()
	saveErr := s.saveMagpieConfigLocked(cfg)
	s.magpie.mu.Unlock()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	if stopProxy || needStopManaged {
		s.closeMagpieProxy()
	}
	s.invalidateMagpieStatus()
	payload, err := s.magpieStatusPayload(r, false)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) magpieInstallHTTP(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "无效的 Magpie 安装渠道", http.StatusBadRequest)
		return
	}

	running := s.vmRunning()
	s.mu.Lock()
	ready, paused, closing := s.ready, s.networkPaused, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境并等待就绪后再安装 Magpie", http.StatusConflict)
		return
	}
	if paused {
		http.Error(w, "网络已暂停，请先恢复网络连接后再安装 Magpie", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键远程安装 Magpie，可使用外部实例接入", http.StatusConflict)
		return
	}

	s.launch(w, "magpie-install", func() error {
		s.magpie.opMu.Lock()
		defer s.magpie.opMu.Unlock()
		s.setMessage("正在通过官方渠道安装 Magpie…")
		payloadMap := map[string]any{}
		if channel != "" {
			payloadMap["channel"] = channel
		}
		payload, _ := json.Marshal(payloadMap)
		script := s.guestShellWrapper(agentRuntimeCommandWithPython(s.guestPythonBinary(), "magpie-install"))
		cmd := s.sshCommand(s.operationContext(), "-T", s.sshTarget(), "--", script)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = s.jobs.execution()
		cmd.Stderr = s.jobs.execution()
		if err := cmd.Run(); err != nil {
			return errors.New("安装 Magpie 失败，请查看操作详情排查网络或运行环境")
		}
		// Automatically enable managed Magpie after a successful install.
		s.magpie.mu.Lock()
		if cfg, err := s.loadMagpieConfigLocked(); err == nil {
			cfg.Enabled = true
			cfg.Mode = "managed"
			_ = s.saveMagpieConfigLocked(cfg)
		}
		s.magpie.statusAt = time.Time{}
		s.magpie.mu.Unlock()
		s.setMessage("Magpie 安装完成，正在验证状态…")
		return nil
	})
}

func (s *Server) magpieUninstallHTTP(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "请先启动 Cage 环境并等待就绪后再卸载 Magpie", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键卸载 Magpie", http.StatusConflict)
		return
	}

	s.launch(w, "magpie-uninstall", func() error {
		s.magpie.opMu.Lock()
		defer s.magpie.opMu.Unlock()
		s.closeMagpieProxy()
		s.setMessage("正在卸载 Magpie（保留项目与配置数据）…")
		s.magpie.mu.Lock()
		cfg, _ := s.loadMagpieConfigLocked()
		s.magpie.mu.Unlock()
		payload, _ := json.Marshal(map[string]any{"port": cfg.Port})
		script := s.guestShellWrapper(agentRuntimeCommandWithPython(s.guestPythonBinary(), "magpie-uninstall"))
		cmd := s.sshCommand(s.operationContext(), "-T", s.sshTarget(), "--", script)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = s.jobs.execution()
		cmd.Stderr = s.jobs.execution()
		if err := cmd.Run(); err != nil {
			return errors.New("卸载 Magpie 失败，请查看操作详情")
		}
		s.closeMagpieProxy()
		s.magpie.mu.Lock()
		if cfg, err := s.loadMagpieConfigLocked(); err == nil {
			cfg.Enabled = false
			_ = s.saveMagpieConfigLocked(cfg)
		}
		s.magpie.statusCache = magpieGuestStatus{}
		s.magpie.statusAt = time.Time{}
		s.magpie.mu.Unlock()
		s.setMessage("Magpie 已卸载")
		return nil
	})
}

func (s *Server) ensureManagedMagpieRunning(ctx context.Context, restart bool, workspace string) (magpieConfig, magpieGuestStatus, error) {
	s.magpie.mu.Lock()
	cfg, err := s.loadMagpieConfigLocked()
	s.magpie.mu.Unlock()
	if err != nil {
		return cfg, magpieGuestStatus{}, err
	}
	if !cfg.Enabled {
		return cfg, magpieGuestStatus{}, errors.New("请先在「设置 → Agent CLI」中启用 Magpie 接入后再启动服务")
	}
	if cfg.Mode != "managed" {
		return cfg, magpieGuestStatus{Running: cfg.ExternalURL != ""}, nil
	}
	if !s.vmRunning() {
		return cfg, magpieGuestStatus{}, errors.New("请先启动 Cage 环境后再运行 Magpie")
	}
	if s.windowsGuest != nil {
		return cfg, magpieGuestStatus{}, errors.New("Windows 原生客机请切换到外部地址模式接入 Magpie")
	}

	if !restart {
		if st, err := s.probeMagpieStatus(ctx, cfg.Port, false); err == nil && st.Running && st.GatewayOnly == cfg.GatewayOnly {
			return cfg, st, nil
		}
	}

	if workspace == "" {
		workspace = s.workspace()
	}
	subcmd := "magpie-start"
	if restart {
		subcmd = "magpie-restart"
	}
	startCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var st magpieGuestStatus
	err = s.runAgentRuntime(startCtx, subcmd, map[string]any{
		"port":         cfg.Port,
		"gateway_port": defaultMagpieGatewayPort,
		"gateway_only": cfg.GatewayOnly,
		"workspace":    s.cliWorkspace(workspace),
	}, &st)
	if err != nil {
		return cfg, st, err
	}
	if st.Port == 0 {
		st.Port = cfg.Port
	}
	if st.GatewayPort == 0 {
		st.GatewayPort = defaultMagpieGatewayPort
	}
	st.Installed = true
	s.magpie.mu.Lock()
	s.magpie.statusCache = st
	s.magpie.statusAt = time.Now()
	s.magpie.mu.Unlock()
	return cfg, st, nil
}

func (s *Server) magpieActionHTTP(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimSpace(r.PathValue("action"))
	s.magpie.opMu.Lock()
	defer s.magpie.opMu.Unlock()
	switch action {
	case "start", "restart":
		_, _, err := s.ensureManagedMagpieRunning(r.Context(), action == "restart", s.workspace())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if action == "restart" {
			s.closeMagpieProxy()
		}
		payload, err := s.magpieStatusPayload(r, false)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	case "stop":
		s.closeMagpieProxy()
		s.magpie.mu.Lock()
		cfg, _ := s.loadMagpieConfigLocked()
		s.magpie.mu.Unlock()
		if cfg.Mode == "managed" && s.vmRunning() && s.windowsGuest == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			err := s.runAgentRuntime(ctx, "magpie-stop", map[string]any{"port": cfg.Port}, nil)
			cancel()
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		s.magpie.mu.Lock()
		s.magpie.statusAt = time.Time{}
		s.magpie.mu.Unlock()
		payload, err := s.magpieStatusPayload(r, true)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) consumeMagpieOpenURL(ctx context.Context, cfg magpieConfig) string {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	if cfg.Mode == "external" {
		return cfg.ExternalURL
	}
	var res struct {
		URL string `json:"url"`
	}
	pCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := s.runAgentRuntime(pCtx, "magpie-open-url", map[string]any{"port": cfg.Port, "consume": true}, &res); err == nil && res.URL != "" {
		return res.URL
	}
	return baseURL
}

func (s *Server) magpieOpenHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Workspace string `json:"workspace"`
		ForceMode string `json:"force_mode"` // "" | "workbench" | "direct"
		Page      string `json:"page"`       // "" | "settings": Magpie's own settings
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !validOfficialPage(in.Page) {
		http.Error(w, "不支持打开 Magpie 的这个页面", http.StatusBadRequest)
		return
	}
	settings := in.Page == officialSettingsPage
	// withPage opens target on Magpie's settings when they were asked for.
	withPage := func(target string) string {
		if settings {
			if moved := magpieSettingsURL(target); moved != "" {
				return moved
			}
		}
		return target
	}
	s.magpie.opMu.Lock()
	defer s.magpie.opMu.Unlock()

	s.magpie.mu.Lock()
	cfg, err := s.loadMagpieConfigLocked()
	s.magpie.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	if !cfg.Enabled {
		http.Error(w, "请先在「设置 → Agent CLI」中启用 Magpie 接入", http.StatusConflict)
		return
	}
	mode := cfg.OpenMode
	if in.ForceMode == "workbench" || in.ForceMode == "direct" {
		mode = in.ForceMode
	}
	if mode == "direct" && !s.t3CodeDirectAvailable(r) {
		mode = "workbench"
	}

	var managedStatus magpieGuestStatus
	if cfg.Mode == "managed" {
		var startErr error
		if _, managedStatus, startErr = s.ensureManagedMagpieRunning(r.Context(), false, in.Workspace); startErr != nil {
			http.Error(w, startErr.Error(), http.StatusConflict)
			return
		}
	} else if cfg.ExternalURL == "" {
		http.Error(w, "请先配置外部 Magpie 网页地址", http.StatusConflict)
		return
	}

	publicBase := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	if cfg.Mode == "external" {
		publicBase = sanitizeMagpieDisplayURL(cfg.ExternalURL)
	}

	if mode == "direct" {
		// Ensure proxy & tunnel succeed BEFORE fetching/consuming the auth URL.
		proxy, err := s.ensureMagpieProxy(r.Context(), cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		openURL := withPage(s.consumeMagpieOpenURL(r.Context(), cfg))
		directURL, err := proxy.issueDirectURL(openURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		reply := map[string]any{
			"mode":       "direct",
			"direct_url": directURL,
			"url":        publicBase,
			"title":      "Magpie",
		}
		if cfg.Mode == "managed" {
			reply["gateway_running"] = managedStatus.GatewayRunning
		}
		jsonReply(w, reply)
		return
	}

	// Workbench mode: reuse existing tab matching the EXACT target origin FIRST
	// so activating an already-signed-in tab never re-consumes an auth URL.
	if existingID := s.findExistingT3CodeTab(r.Context(), publicBase); existingID != "" {
		if _, err := s.browserAction(r.Context(), "activate", &browserInput{ID: existingID}); err == nil {
			reply := map[string]any{
				"mode":   "workbench",
				"tab_id": existingID,
				"url":    publicBase,
				"title":  "Magpie",
			}
			if settings {
				// The tab is signed in already: its address needs no key.
				target := magpieSettingsURL(publicBase)
				reply["settings_opened"] = target != "" && s.navigateBrowserTab(r.Context(), existingID, target) == nil
			}
			if cfg.Mode == "managed" {
				reply["gateway_running"] = managedStatus.GatewayRunning
			}
			jsonReply(w, reply)
			return
		}
	}
	openURL := withPage(s.consumeMagpieOpenURL(r.Context(), cfg))
	data, err := s.browserAction(r.Context(), "new", &browserInput{URL: openURL})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var created struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &created) != nil || !validBrowserTabID(created.ID) {
		http.Error(w, "Cage 浏览器未能打开 Magpie 标签页，请检查浏览器连接后重试", http.StatusBadGateway)
		return
	}
	reply := map[string]any{
		"mode":   "workbench",
		"tab_id": created.ID,
		"url":    publicBase,
		"title":  "Magpie",
	}
	if cfg.Mode == "managed" {
		reply["gateway_running"] = managedStatus.GatewayRunning
	}
	jsonReply(w, reply)
}

// magpieTUIScript builds the interactive guest terminal command to launch
// `magpie tui` in workspace with an enriched PATH and truecolor terminal env,
// falling back to a login shell once Magpie TUI exits.
func (s *Server) magpieTUIScript(workspace string) string {
	return buildMagpieTUIScript(workspace, s.macosGuest != nil)
}

func buildMagpieTUIScript(workspace string, macos bool) string {
	shellName := "/bin/bash"
	fallbackExec := "exec /bin/bash -il"
	if macos {
		shellName = "/bin/zsh"
		fallbackExec = "exec /bin/zsh -il"
	}
	cleanWorkspace := strings.ReplaceAll(strings.TrimSpace(workspace), "\x00", "")
	cdTarget := `"$HOME"`
	if cleanWorkspace != "" {
		cdTarget = shellescape.Quote(cleanWorkspace)
	}
	body := `export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`if command -v magpie >/dev/null 2>&1; then magpie tui; else echo "未找到 magpie 命令，请先在「设置 → Agent CLI」中安装 Magpie。" >&2; fi; ` +
		fallbackExec
	return "cd -- " + cdTarget +
		" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 " +
		shellName + " -lic " + shellescape.Quote(body)
}
