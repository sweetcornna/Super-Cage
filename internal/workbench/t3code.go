package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sylvinus/agent-vm/internal/containerenv"
)

const (
	defaultT3CodePort      = 13773
	t3CodeStatusTTL        = 10 * time.Second
	t3CodeDirectGrantTTL   = 30 * time.Second
)

// t3CodeConfig persists the user's opt-in T3 Code integration preferences.
// Disabled by default (zero overhead until enabled).
type t3CodeConfig struct {
	Enabled         bool              `json:"enabled"`
	Mode            string            `json:"mode"`                       // managed | external
	ExternalURL     string            `json:"external_url,omitempty"`     // used when Mode == "external"
	Port            int               `json:"port"`                       // managed guest loopback port (default 13773)
	OpenMode        string            `json:"open_mode"`                  // workbench | direct
	FocusChrome     bool              `json:"focus_chrome"`               // distraction-free 36px app header in workbench
	AccountBindings map[string]string `json:"account_bindings,omitempty"` // agentID -> "auto" | "default" | <accountID>
}

func defaultT3CodeConfig() t3CodeConfig {
	return t3CodeConfig{
		Enabled:     false,
		Mode:        "managed",
		Port:        defaultT3CodePort,
		OpenMode:    "workbench",
		FocusChrome: true,
		AccountBindings: map[string]string{
			"claude":   "auto",
			"codex":    "auto",
			"opencode": "auto",
			"gemini":   "auto",
		},
	}
}

func (c t3CodeConfig) normalized() t3CodeConfig {
	if c.Mode != "managed" && c.Mode != "external" {
		c.Mode = "managed"
	}
	if c.Port < 1024 || c.Port > 65535 {
		c.Port = defaultT3CodePort
	}
	if c.OpenMode != "workbench" && c.OpenMode != "direct" {
		c.OpenMode = "workbench"
	}
	if c.AccountBindings == nil {
		c.AccountBindings = map[string]string{}
	}
	return c
}

func validateT3CodeExternalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 1024 || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return "", errors.New("T3 Code 网页地址格式无效")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("T3 Code 地址须为有效的 HTTP(S) URL 且不能含用户名密码")
	}
	host := strings.ToLower(u.Hostname())
	isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if u.Scheme == "http" && !isLoopback {
		return "", errors.New("非本机回环地址的 T3 Code 实例必须使用 HTTPS")
	}
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/") + "/", nil
}

type t3CodeGuestStatus struct {
	Installed  bool   `json:"installed"`
	BinaryPath string `json:"binary_path,omitempty"`
	Version    string `json:"version,omitempty"`
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	Port       int    `json:"port,omitempty"`
	HasPairURL bool   `json:"has_pair_url,omitempty"`
}

type t3CodeProbeCall struct {
	done   chan struct{}
	port   int
	forced bool
	status t3CodeGuestStatus
	err    error
}

type t3CodeManager struct {
	opMu           sync.Mutex // serializes config transitions and managed guest lifecycle actions
	mu             sync.Mutex
	configLoaded   bool
	config         t3CodeConfig
	statusCache    t3CodeGuestStatus
	statusAt       time.Time
	probeInflight  *t3CodeProbeCall
	activeBindings map[string]string
	proxy          *t3CodeProxy
}

func (s *Server) loadT3CodeConfigLocked() (t3CodeConfig, error) {
	m := &s.t3code
	if m.configLoaded {
		cp := m.config.normalized()
		cp.AccountBindings = maps.Clone(cp.AccountBindings)
		return cp, nil
	}
	cfg := defaultT3CodeConfig()
	data, err := os.ReadFile(filepath.Join(s.profile, "t3code.json"))
	if os.IsNotExist(err) {
		m.config, m.configLoaded = cfg, true
		cp := cfg
		cp.AccountBindings = maps.Clone(cfg.AccountBindings)
		return cp, nil
	}
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return cfg, errors.New("T3 Code 配置文件无法读取，请检查配置目录")
	}
	cfg = cfg.normalized()
	m.config = cfg
	m.configLoaded = true
	cp := cfg
	cp.AccountBindings = maps.Clone(cfg.AccountBindings)
	return cp, nil
}

func (s *Server) saveT3CodeConfigLocked(cfg t3CodeConfig) error {
	clean := cfg.normalized()
	clean.AccountBindings = maps.Clone(clean.AccountBindings)
	if err := writePrivate(filepath.Join(s.profile, "t3code.json"), clean); err != nil {
		return errors.New("T3 Code 设置未能保存，请检查磁盘空间和权限")
	}
	s.t3code.config = clean
	s.t3code.configLoaded = true
	return nil
}

func (s *Server) invalidateT3CodeStatus() {
	s.t3code.mu.Lock()
	s.t3code.statusAt = time.Time{}
	s.t3code.mu.Unlock()
}

func (s *Server) probeT3CodeStatus(ctx context.Context, port int, force bool) (t3CodeGuestStatus, error) {
	m := &s.t3code
	for {
		m.mu.Lock()
		if !force && !m.statusAt.IsZero() && time.Since(m.statusAt) < t3CodeStatusTTL && m.statusCache.Port == port {
			cached := m.statusCache
			m.mu.Unlock()
			return cached, nil
		}
		if call := m.probeInflight; call != nil && call.port == port {
			if !force || call.forced {
				m.mu.Unlock()
				select {
				case <-ctx.Done():
					return t3CodeGuestStatus{}, ctx.Err()
				case <-call.done:
					return call.status, call.err
				}
			}
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return t3CodeGuestStatus{}, ctx.Err()
			case <-call.done:
				continue
			}
		}
		call := &t3CodeProbeCall{done: make(chan struct{}), port: port, forced: force}
		m.probeInflight = call
		m.mu.Unlock()

		ensureCtx, ensureCancel := context.WithTimeout(s.lifetimeContext(), coldEnsureTimeout)
		err := s.ensureAgentRuntime(ensureCtx)
		ensureCancel()
		var st t3CodeGuestStatus
		if err == nil {
			probeCtx, cancel := context.WithTimeout(s.lifetimeContext(), statusProbeTimeout)
			err = s.runAgentRuntime(probeCtx, "t3code-status", map[string]any{"port": port}, &st)
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

// t3CodeDirectAvailable reports whether the calling browser can open and reach
// a host-local 127.0.0.1 ephemeral proxy window directly. Native desktop WebViews
// (which suppress window.open popups by design), remote SSH clients, paired
// phones, remote Windows guests, and headless containers use workbench mode.
func (s *Server) t3CodeDirectAvailable(r *http.Request) bool {
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
		// Native Cage Desktop WebViews (WKWebView / WebView2 / WebKitGTK) intentionally
		// suppress window.open popups, so direct new-window mode is unavailable there.
		if s.hasTrustedView(r) {
			return false
		}
	}
	return true
}

// resolveT3CodeBindings translates user-configured bindings ("auto", "default",
// or specific account ID) into guest isolation specs and human-readable labels.
// Explicit account bindings fail loudly when missing or not ready rather than
// silently falling back to default credentials.
func (s *Server) resolveT3CodeBindings(cfg t3CodeConfig, workspace string) (map[string]any, map[string]string, error) {
	return s.resolveToolAccountBindings(cfg.AccountBindings, workspace, "T3 Code")
}

func (s *Server) t3CodeStatusPayload(r *http.Request, force bool) (map[string]any, error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	s.t3code.mu.Lock()
	cfg, err := s.loadT3CodeConfigLocked()
	activeBindings := maps.Clone(s.t3code.activeBindings)
	s.t3code.mu.Unlock()
	if err != nil {
		return nil, err
	}

	runningVM := s.vmRunning()
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.mu.Unlock()

	supportsManaged := s.windowsGuest == nil
	directAvailable := s.t3CodeDirectAvailable(r)

	var st t3CodeGuestStatus
	probeErr := ""
	// Disabled by default (zero guest overhead until opted in or explicitly forced).
	if (cfg.Enabled || force) && cfg.Mode == "managed" && ready && runningVM && supportsManaged {
		pCtx, cancel := context.WithTimeout(ctx, s.statusProbeWaitTimeout())
		st, err = s.probeT3CodeStatus(pCtx, cfg.Port, force)
		cancel()
		if err != nil {
			probeErr = err.Error()
		}
	}

	displayURL := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	if cfg.Mode == "external" {
		displayURL = cfg.ExternalURL
		st.Running = displayURL != ""
	}
	return map[string]any{
		"enabled":          cfg.Enabled,
		"mode":             cfg.Mode,
		"external_url":     cfg.ExternalURL,
		"port":             cfg.Port,
		"open_mode":        cfg.OpenMode,
		"focus_chrome":     cfg.FocusChrome,
		"account_bindings": cfg.AccountBindings,
		"installed":        st.Installed,
		"version":          st.Version,
		"running":          st.Running,
		"url":              displayURL,
		"active_bindings":  activeBindings,
		"magpie_takeover":  s.magpieTakesOverScheduling(),
		"error":            probeErr,
		"vm_running":       runningVM,
		"ready":            ready,
		"supports_managed": supportsManaged,
		"direct_available": directAvailable,
	}, nil
}

func (s *Server) t3CodeStatusHTTP(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	payload, err := s.t3CodeStatusPayload(r, force)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) t3CodeConfigHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled         *bool             `json:"enabled"`
		Mode            string            `json:"mode"`
		ExternalURL     *string           `json:"external_url"`
		Port            *int              `json:"port"`
		OpenMode        string            `json:"open_mode"`
		FocusChrome     *bool             `json:"focus_chrome"`
		AccountBindings map[string]string `json:"account_bindings"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	// Serialize config transitions with start/stop/restart/open so host state and
	// guest runtime cannot interleave.
	s.t3code.opMu.Lock()
	defer s.t3code.opMu.Unlock()

	s.t3code.mu.Lock()
	cfg, err := s.loadT3CodeConfigLocked()
	s.t3code.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	prevEnabled, prevMode, prevPort := cfg.Enabled, cfg.Mode, cfg.Port
	var stopProxy bool
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
		if !cfg.Enabled {
			stopProxy = true
		}
	}
	if in.Mode != "" {
		if in.Mode != "managed" && in.Mode != "external" {
			requestError(w, errors.New("T3 Code 接入模式无效"))
			return
		}
		if cfg.Mode != in.Mode {
			stopProxy = true
		}
		cfg.Mode = in.Mode
	}
	if in.ExternalURL != nil {
		clean, err := validateT3CodeExternalURL(*in.ExternalURL)
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
		if cfg.Port != *in.Port {
			stopProxy = true
		}
		cfg.Port = *in.Port
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
	if cfg.Enabled && cfg.Mode == "managed" && s.windowsGuest != nil {
		requestError(w, errors.New("当前 Windows 原生客机暂不支持托管 T3 Code，请切换为外部地址接入"))
		return
	}
	if cfg.Enabled && cfg.Mode == "external" && cfg.ExternalURL == "" {
		requestError(w, errors.New("外部接入模式请先填写 T3 Code 网页地址"))
		return
	}

	needStopManaged := prevMode == "managed" && ((prevEnabled && !cfg.Enabled) || cfg.Mode != "managed" || cfg.Port != prevPort)
	if needStopManaged && s.vmRunning() && s.windowsGuest == nil {
		stopCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		stopErr := s.runAgentRuntime(stopCtx, "t3code-stop", map[string]any{"port": prevPort}, nil)
		cancel()
		if stopErr != nil {
			http.Error(w, fmt.Sprintf("停止原托管 T3 Code 服务失败，配置未变更: %v", stopErr), http.StatusConflict)
			return
		}
	}

	s.t3code.mu.Lock()
	saveErr := s.saveT3CodeConfigLocked(cfg)
	if saveErr == nil && (needStopManaged || stopProxy) {
		s.t3code.activeBindings = nil
	}
	s.t3code.mu.Unlock()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	if stopProxy || needStopManaged {
		s.closeT3CodeProxy()
	}
	s.invalidateT3CodeStatus()
	payload, err := s.t3CodeStatusPayload(r, false)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, payload)
}

func (s *Server) t3CodeInstallHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Channel string `json:"channel"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	channel := strings.TrimSpace(in.Channel)
	if channel == "" {
		channel = "npm"
	}
	if channel != "npm" && channel != "script" {
		http.Error(w, "无效的 T3 Code 官方安装渠道", http.StatusBadRequest)
		return
	}
	running := s.vmRunning()
	s.mu.Lock()
	ready, paused, closing := s.ready, s.networkPaused, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境并等待就绪后再安装 T3 Code", http.StatusConflict)
		return
	}
	if paused {
		http.Error(w, "网络已暂停，请先恢复网络连接后再安装 T3 Code", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键远程安装 T3 Code，可使用外部实例接入", http.StatusConflict)
		return
	}

	s.launch(w, "t3code-install", func() error {
		s.setMessage("正在通过官方渠道安装原版 T3 Code…")
		if err := s.runAgentRuntimeJob(s.operationContext(), "t3code-install", map[string]any{"channel": channel}); err != nil {
			return errors.New("安装 T3 Code 失败，请查看操作详情排查网络或 Node.js 环境")
		}
		// Automatically enable managed T3 Code after a successful install.
		s.t3code.mu.Lock()
		if cfg, err := s.loadT3CodeConfigLocked(); err == nil {
			cfg.Enabled = true
			cfg.Mode = "managed"
			_ = s.saveT3CodeConfigLocked(cfg)
		}
		s.t3code.statusAt = time.Time{}
		s.t3code.mu.Unlock()
		s.setMessage("T3 Code 安装完成，正在验证状态…")
		return nil
	})
}

func (s *Server) ensureManagedT3CodeRunning(ctx context.Context, restart bool, workspace string) (t3CodeConfig, t3CodeGuestStatus, error) {
	s.t3code.mu.Lock()
	cfg, err := s.loadT3CodeConfigLocked()
	s.t3code.mu.Unlock()
	if err != nil {
		return cfg, t3CodeGuestStatus{}, err
	}
	if !cfg.Enabled {
		return cfg, t3CodeGuestStatus{}, errors.New("请先在「设置 → Agent CLI」中启用 T3 Code 接入后再启动服务")
	}
	if cfg.Mode != "managed" {
		return cfg, t3CodeGuestStatus{Running: cfg.ExternalURL != ""}, nil
	}
	if !s.vmRunning() {
		return cfg, t3CodeGuestStatus{}, errors.New("请先启动 Cage 环境后再运行 T3 Code")
	}
	if s.windowsGuest != nil {
		return cfg, t3CodeGuestStatus{}, errors.New("Windows 原生客机请切换到外部地址模式接入 T3 Code")
	}

	if !restart {
		if st, err := s.probeT3CodeStatus(ctx, cfg.Port, false); err == nil && st.Running {
			return cfg, st, nil
		}
	}

	if workspace == "" {
		workspace = s.workspace()
	}
	guestBindings, labels, err := s.resolveT3CodeBindings(cfg, workspace)
	if err != nil {
		return cfg, t3CodeGuestStatus{}, err
	}
	subcmd := "t3code-start"
	if restart {
		subcmd = "t3code-restart"
	}
	startCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var st t3CodeGuestStatus
	err = s.runAgentRuntime(startCtx, subcmd, map[string]any{
		"port":      cfg.Port,
		"workspace": s.cliWorkspace(workspace),
		"bindings":  guestBindings,
	}, &st)
	if err != nil {
		return cfg, st, err
	}
	st.Port = cfg.Port
	st.Installed = true
	s.t3code.mu.Lock()
	s.t3code.statusCache = st
	s.t3code.statusAt = time.Now()
	s.t3code.activeBindings = labels
	s.t3code.mu.Unlock()
	return cfg, st, nil
}

func (s *Server) t3CodeActionHTTP(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimSpace(r.PathValue("action"))
	s.t3code.opMu.Lock()
	defer s.t3code.opMu.Unlock()
	switch action {
	case "start", "restart":
		_, _, err := s.ensureManagedT3CodeRunning(r.Context(), action == "restart", s.workspace())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if action == "restart" {
			s.closeT3CodeProxy()
		}
		payload, err := s.t3CodeStatusPayload(r, false)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	case "stop":
		s.closeT3CodeProxy()
		s.t3code.mu.Lock()
		cfg, _ := s.loadT3CodeConfigLocked()
		s.t3code.mu.Unlock()
		if cfg.Mode == "managed" && s.vmRunning() && s.windowsGuest == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			err := s.runAgentRuntime(ctx, "t3code-stop", map[string]any{"port": cfg.Port}, nil)
			cancel()
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		s.t3code.mu.Lock()
		s.t3code.activeBindings = nil
		s.t3code.statusAt = time.Time{}
		s.t3code.mu.Unlock()
		payload, err := s.t3CodeStatusPayload(r, true)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, payload)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) consumeT3CodeOpenURL(ctx context.Context, cfg t3CodeConfig) string {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	if cfg.Mode == "external" {
		return cfg.ExternalURL
	}
	var res struct {
		URL string `json:"url"`
	}
	// Give the guest runtime plenty of headroom for `t3 pair` minting + `pair.once` fallback.
	pCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := s.runAgentRuntime(pCtx, "t3code-open-url", map[string]any{"port": cfg.Port, "consume": true}, &res); err == nil && res.URL != "" {
		return res.URL
	}
	return baseURL
}

func (s *Server) t3CodeOpenHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Workspace string `json:"workspace"`
		ForceMode string `json:"force_mode"` // "" | "workbench" | "direct"
		Page      string `json:"page"`       // "" | "settings": T3 Code's own settings
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !validOfficialPage(in.Page) {
		http.Error(w, "不支持打开 T3 Code 的这个页面", http.StatusBadRequest)
		return
	}
	settings := in.Page == officialSettingsPage
	s.t3code.opMu.Lock()
	defer s.t3code.opMu.Unlock()

	s.t3code.mu.Lock()
	cfg, err := s.loadT3CodeConfigLocked()
	s.t3code.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	if !cfg.Enabled {
		http.Error(w, "请先在「设置 → Agent CLI」中启用 T3 Code 接入", http.StatusConflict)
		return
	}
	mode := cfg.OpenMode
	if in.ForceMode == "workbench" || in.ForceMode == "direct" {
		mode = in.ForceMode
	}
	if mode == "direct" && !s.t3CodeDirectAvailable(r) {
		mode = "workbench"
	}
	if settings {
		// Pairing always lands on T3 Code's home page; only a workbench tab
		// can be moved on to Settings after it.
		mode = "workbench"
	}

	if cfg.Mode == "managed" {
		if _, _, err := s.ensureManagedT3CodeRunning(r.Context(), false, in.Workspace); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	} else if cfg.ExternalURL == "" {
		http.Error(w, "请先配置外部 T3 Code 网页地址", http.StatusConflict)
		return
	}

	publicBase := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	if cfg.Mode == "external" {
		publicBase = cfg.ExternalURL
	}

	if mode == "direct" {
		// Ensure proxy & tunnel succeed BEFORE consuming/minting a pairing URL.
		proxy, err := s.ensureT3CodeProxy(r.Context(), cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		openURL := s.consumeT3CodeOpenURL(r.Context(), cfg)
		directURL, err := proxy.issueDirectURL(openURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		jsonReply(w, map[string]any{
			"mode":       "direct",
			"direct_url": directURL,
			"url":        publicBase,
			"title":      "T3 Code",
		})
		return
	}

	// Workbench mode: reuse existing tab matching the EXACT target origin FIRST
	// so activating an already-paired tab never consumes a fresh pairing URL.
	if existingID := s.findExistingT3CodeTab(r.Context(), publicBase); existingID != "" {
		if _, err := s.browserAction(r.Context(), "activate", &browserInput{ID: existingID}); err == nil {
			reply := map[string]any{
				"mode":   "workbench",
				"tab_id": existingID,
				"url":    publicBase,
				"title":  "T3 Code",
			}
			if settings {
				target := t3CodeSettingsURL(publicBase)
				reply["settings_opened"] = target != "" && s.navigateBrowserTab(r.Context(), existingID, target) == nil
			}
			jsonReply(w, reply)
			return
		}
	}
	openURL := s.consumeT3CodeOpenURL(r.Context(), cfg)
	data, err := s.browserAction(r.Context(), "new", &browserInput{URL: openURL})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var created struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &created) != nil || !validBrowserTabID(created.ID) {
		http.Error(w, "Cage 浏览器未能打开 T3 Code 标签页，请检查浏览器连接后重试", http.StatusBadGateway)
		return
	}
	reply := map[string]any{
		"mode":   "workbench",
		"tab_id": created.ID,
		"url":    publicBase,
		"title":  "T3 Code",
	}
	if settings {
		reply["settings_opened"] = s.openT3CodeSettingsAfterPairing(r.Context(), created.ID, publicBase)
	}
	jsonReply(w, reply)
}

func validBrowserTabID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) < 0
}

func (s *Server) findExistingT3CodeTab(ctx context.Context, baseURL string) string {
	targetOrigin := t3CodeOriginOf(baseURL)
	if targetOrigin == "" {
		return ""
	}
	data, err := s.browserAction(ctx, "tabs", nil)
	if err != nil {
		return ""
	}
	var state struct {
		Tabs []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"tabs"`
	}
	if json.Unmarshal(data, &state) != nil {
		return ""
	}
	for _, tab := range state.Tabs {
		if validBrowserTabID(tab.ID) && t3CodeOriginOf(tab.URL) == targetOrigin {
			return tab.ID
		}
	}
	return ""
}

func t3CodeOriginOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
