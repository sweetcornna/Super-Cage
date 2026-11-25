package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/agentcatalog"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

const (
	agentProbeTTL        = 30 * time.Second
	agentDefaultProbeTTL = 20 * time.Second
	agentFlushDelay      = 2 * time.Second
	maxRecentWorkspaces  = 128
)

// agentConfig stores user-customized terminal & install settings for one agent.
type agentConfig struct {
	LaunchCommand    string `json:"launch_command,omitempty"`
	ExtraArgs        string `json:"extra_args,omitempty"`
	PreferredChannel string `json:"preferred_channel,omitempty"`
	ImagePaste       *bool  `json:"image_paste,omitempty"`
	ShiftEnterMode   string `json:"shift_enter_mode,omitempty"`
	HiddenInMenu     bool   `json:"hidden_in_menu,omitempty"`
}

type agentRuntimeState struct {
	Installed       bool   `json:"installed"`
	BinaryPath      string `json:"binary_path,omitempty"`
	Version         string `json:"version,omitempty"`
	DefaultLoggedIn bool   `json:"default_logged_in,omitempty"`
}

type prerequisiteState struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type agentProbeSnapshot struct {
	Prerequisites map[string]prerequisiteState `json:"prerequisites"`
	Agents        map[string]agentRuntimeState `json:"agents"`
	ProbedAt      int64                        `json:"probed_at,omitempty"`
}

type agentStoreData struct {
	CustomSpecs []agentcatalog.AgentSpec     `json:"custom_specs"`
	Configs     map[string]agentConfig       `json:"configs"`
	LastProbe   map[string]agentRuntimeState `json:"last_probe,omitempty"`
}

type agentProbeCall struct {
	done     chan struct{}
	forced   bool
	snapshot agentProbeSnapshot
	err      error
}

type agentDefaultProbeEntry struct {
	checkedAt time.Time
	snap      agentGuestSnapshot
	err       error
	inflight  *agentDefaultProbeCall
}

type agentDefaultProbeCall struct {
	done chan struct{}
	snap agentGuestSnapshot
	err  error
}

type agentConfirmCall struct {
	done chan struct{}
	err  error
}

type agentManager struct {
	mu               sync.Mutex
	flushMu          sync.Mutex
	storeLoaded      bool
	store            agentStoreData
	poolsLoaded      bool
	pools            map[string]*agentAccountPool
	poolsDirty       bool
	poolsGen         uint64
	lastWrittenGen   uint64
	closed           bool
	flushTimer       *time.Timer
	probeCache       agentProbeSnapshot
	probeAt          time.Time
	probeGen         uint64
	probeInflight    *agentProbeCall
	bgProbing        bool
	defaultCache     map[string]*agentDefaultProbeEntry
	confirmInflight  map[string]*agentConfirmCall
	pendingAttempted map[string]time.Time
	lastPick         map[string]string
	recent           map[string]map[string]claudeProjectPick
	loginNotice      map[string]string
	// Test hooks allow deterministic unit tests without an SSH VM.
	probeHook   func(context.Context, []agentcatalog.AgentSpec, bool) (agentProbeSnapshot, error)
	runtimeHook func(context.Context, string, any, any) error
}

var agentRuntimeSpec = newGuestScriptSpec(".local/share/cage/agent-runtime", 0o700, cage.AgentRuntimeHelper)

var agentRuntimeHash = agentRuntimeSpec.hash

func (s *Server) guestPythonBinary() string {
	if s.macosGuest != nil {
		return "/usr/local/libexec/cage-cli/bin/python3"
	}
	return "python3"
}

func (s *Server) guestShellWrapper(script string) string {
	return s.guestShellScript("exec " + script)
}

// agentRuntimeCommand returns a constant-size command that runs the
// SHA-256-pinned cage-agent-runtime installed under
// ~/.local/share/cage/agent-runtime/<hash>.py. Callers must ensure the script
// is installed on the guest via ensureAgentRuntime / runWithGuestScript.
func agentRuntimeCommand(subcommand string) string {
	return agentRuntimeCommandWithPython("python3", subcommand)
}

func agentRuntimeCommandWithPython(pythonBin, subcommand string) string {
	return agentRuntimeSpec.shellCommand(pythonBin, subcommand)
}

func (s *Server) agentRuntimeSSHCommand(ctx context.Context, subcommand string) *exec.Cmd {
	command := s.guestShellScript(agentRuntimeSpec.guardedShellBody(s.guestPythonBinary(), subcommand))
	return s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
}

var (
	statusProbeTimeout          = 6 * time.Second
	coldEnsureTimeout           = 15 * time.Second
	errEnsureAgentRuntimeNeeded = errors.New("agent runtime ensure needed")
)

func (s *Server) agentRuntimeInstalled() bool {
	if s.agentCLIs.runtimeHook != nil || s.windowsGuest != nil {
		return true
	}
	return s.guestScripts.has(agentRuntimeSpec.cacheKey())
}

func (s *Server) statusProbeWaitTimeout() time.Duration {
	if s.agentRuntimeInstalled() {
		return statusProbeTimeout
	}
	return coldEnsureTimeout + statusProbeTimeout
}

func (s *Server) ensureAgentRuntime(ctx context.Context) error {
	if s.agentCLIs.runtimeHook != nil || s.windowsGuest != nil {
		return nil
	}
	return s.ensureGuestScript(ctx, agentRuntimeSpec, s.guestPythonBinary())
}

func (s *Server) runAgentRuntimeJob(ctx context.Context, subcommand string, input any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return s.runWithGuestScript(ctx, agentRuntimeSpec, s.guestPythonBinary(), func() (int, error) {
		cmd := s.agentRuntimeSSHCommand(ctx, subcommand)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = s.jobs.execution()
		cmd.Stderr = s.jobs.execution()
		err := cmd.Run()
		return exitCodeOf(err), err
	})
}

func cloneAgentStore(store agentStoreData) agentStoreData {
	cp := agentStoreData{
		CustomSpecs: make([]agentcatalog.AgentSpec, len(store.CustomSpecs)),
		Configs:     maps.Clone(store.Configs),
		LastProbe:   maps.Clone(store.LastProbe),
	}
	for i, spec := range store.CustomSpecs {
		cp.CustomSpecs[i] = spec.Clone()
	}
	if cp.Configs == nil {
		cp.Configs = map[string]agentConfig{}
	}
	if cp.LastProbe == nil {
		cp.LastProbe = map[string]agentRuntimeState{}
	}
	return cp
}

func cloneAgentProbeSnapshot(snap agentProbeSnapshot) agentProbeSnapshot {
	return agentProbeSnapshot{
		Prerequisites: maps.Clone(snap.Prerequisites),
		Agents:        maps.Clone(snap.Agents),
		ProbedAt:      snap.ProbedAt,
	}
}

func (s *Server) loadAgentStoreLocked() (agentStoreData, error) {
	m := &s.agentCLIs
	if m.storeLoaded {
		return cloneAgentStore(m.store), nil
	}
	store := agentStoreData{
		CustomSpecs: []agentcatalog.AgentSpec{},
		Configs:     map[string]agentConfig{},
		LastProbe:   map[string]agentRuntimeState{},
	}
	data, err := os.ReadFile(filepath.Join(s.profile, "agents.json"))
	if os.IsNotExist(err) {
		m.store, m.storeLoaded = cloneAgentStore(store), true
		return store, nil
	}
	if err != nil || json.Unmarshal(data, &store) != nil {
		return store, errors.New("Agent 配置文件无法读取，请检查配置目录")
	}
	if store.CustomSpecs == nil {
		store.CustomSpecs = []agentcatalog.AgentSpec{}
	}
	if store.Configs == nil {
		store.Configs = map[string]agentConfig{}
	}
	if store.LastProbe == nil {
		store.LastProbe = map[string]agentRuntimeState{}
	}
	m.store, m.storeLoaded = cloneAgentStore(store), true
	return cloneAgentStore(store), nil
}

func (s *Server) saveAgentStoreLocked(store agentStoreData) error {
	clean := cloneAgentStore(store)
	if err := writePrivate(filepath.Join(s.profile, "agents.json"), clean); err != nil {
		return errors.New("Agent 配置未能保存，请检查配置目录权限和磁盘空间")
	}
	s.agentCLIs.store = clean
	s.agentCLIs.storeLoaded = true
	return nil
}

func (s *Server) lookupAgentSpec(id string) (agentcatalog.AgentSpec, agentConfig, bool) {
	s.agentCLIs.mu.Lock()
	store, err := s.loadAgentStoreLocked()
	s.agentCLIs.mu.Unlock()
	if err != nil {
		spec, ok := agentcatalog.Lookup(id, nil)
		return spec, agentConfig{}, ok
	}
	spec, ok := agentcatalog.Lookup(id, store.CustomSpecs)
	return spec, store.Configs[id], ok
}

func (s *Server) allAgentOAuthDomains() []string {
	s.agentCLIs.mu.Lock()
	store, _ := s.loadAgentStoreLocked()
	s.agentCLIs.mu.Unlock()
	return agentcatalog.AllOAuthDomains(store.CustomSpecs)
}

func (s *Server) effectiveAgentLaunchCommand(spec agentcatalog.AgentSpec, cfg agentConfig) string {
	if spec.ID == "claude" {
		s.mu.Lock()
		cmd := claudeTerminalCommand(s.preferences)
		s.mu.Unlock()
		return cmd
	}
	cmd := strings.TrimSpace(cfg.LaunchCommand)
	if cmd == "" {
		cmd = spec.DefaultLaunchCommand
	}
	if extra := strings.TrimSpace(cfg.ExtraArgs); extra != "" {
		cmd = cmd + " " + extra
	}
	return cmd
}

func agentRuntimeUsesSharedSession(subcommand string) bool {
	switch subcommand {
	case "probe-all", "snapshot", "t3code-status", "magpie-status", "herdr-status", "herdr-list":
		return true
	default:
		return false
	}
}

func (s *Server) runAgentRuntime(ctx context.Context, subcommand string, input any, output any) error {
	if hook := s.agentCLIs.runtimeHook; hook != nil {
		return hook(ctx, subcommand, input, output)
	}
	if s.windowsGuest != nil {
		return errors.New("当前 Windows 原生客机暂未支持通用 Agent 后台辅助命令，请在 PowerShell 终端中操作")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	var out []byte
	var stderr fileControlStderr
	useShared := s.macosGuest == nil && agentRuntimeUsesSharedSession(subcommand) && len(data) <= 48<<10
	err = s.runWithGuestScript(ctx, agentRuntimeSpec, s.guestPythonBinary(), func() (int, error) {
		stderr.data = stderr.data[:0]
		if useShared {
			script := agentRuntimeSpec.guardedSharedScript(s.guestPythonBinary(), data, subcommand)
			reply, ok, sharedErr := s.guestShared(ctx, script)
			if ok {
				if sharedErr == nil {
					if reply.code == guestScriptMissingExitCode {
						return guestScriptMissingExitCode, fmt.Errorf("exit status %d", reply.code)
					}
					if len(reply.stdout) <= 64<<10 {
						out = reply.stdout
						_, _ = stderr.Write(reply.stderr)
						if reply.code != 0 {
							return reply.code, fmt.Errorf("exit status %d", reply.code)
						}
						return 0, nil
					}
					// Oversized reply (> 64 KiB): fall through to dedicated SSH below.
				} else if ctx.Err() != nil {
					return -1, sharedErr
				}
			} else if ctx.Err() != nil {
				return -1, ctx.Err()
			}
		}
		cmd := s.agentRuntimeSSHCommand(ctx, subcommand)
		cmd.Stdin = bytes.NewReader(data)
		cmd.Stderr = &stderr
		cmd.WaitDelay = 2 * time.Second
		var runErr error
		out, runErr = cmd.Output()
		return exitCodeOf(runErr), runErr
	})
	if err != nil {
		msg := strings.TrimSpace(diagnostics.Redact(string(stderr.data)))
		if msg != "" && len(msg) <= 240 {
			return errors.New(msg)
		}
		return errors.New("无法与运行环境中的 Agent 辅助程序通信，请确认环境已启动")
	}
	if output != nil && len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, output); err != nil {
			return errors.New("运行环境返回了无法解析的 Agent 状态")
		}
	}
	return nil
}

// probeAllAgents runs a single coalesced batched probe across all built-in and
// custom agents, cached for 30s so UI reads are sub-millisecond.
func (s *Server) probeAllAgents(parent context.Context, specs []agentcatalog.AgentSpec, force bool) (agentProbeSnapshot, error) {
	m := &s.agentCLIs
	for {
		m.mu.Lock()
		if !force && !m.probeAt.IsZero() && time.Since(m.probeAt) < agentProbeTTL {
			snap := cloneAgentProbeSnapshot(m.probeCache)
			m.mu.Unlock()
			return snap, nil
		}
		if call := m.probeInflight; call != nil {
			if !force || call.forced {
				m.mu.Unlock()
				select {
				case <-parent.Done():
					return agentProbeSnapshot{}, parent.Err()
				case <-call.done:
					return cloneAgentProbeSnapshot(call.snapshot), call.err
				}
			}
			// A forced refresh waits for the non-forced in-flight call to finish,
			// then starts a fresh forced probe so stale results aren't reused.
			m.mu.Unlock()
			select {
			case <-parent.Done():
				return agentProbeSnapshot{}, parent.Err()
			case <-call.done:
				continue
			}
		}
		call := &agentProbeCall{done: make(chan struct{}), forced: force}
		gen := m.probeGen
		m.probeInflight = call
		m.mu.Unlock()

		var snap agentProbeSnapshot
		var err error
		finished := false
		defer func() {
			if !finished {
				m.mu.Lock()
				if m.probeInflight == call {
					m.probeInflight = nil
				}
				close(call.done)
				m.mu.Unlock()
			}
		}()

		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		if hook := m.probeHook; hook != nil {
			snap, err = hook(ctx, specs, force)
		} else {
			err = s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": specs, "force": force}, &snap)
		}
		cancel()

		now := time.Now()
		if err == nil {
			snap.ProbedAt = now.Unix()
		}
		resultSnap := cloneAgentProbeSnapshot(snap)

		m.mu.Lock()
		if err == nil && m.probeGen == gen {
			m.probeCache = cloneAgentProbeSnapshot(snap)
			m.probeAt = now
			if store, loadErr := s.loadAgentStoreLocked(); loadErr == nil {
				if !maps.Equal(store.LastProbe, snap.Agents) {
					store.LastProbe = maps.Clone(snap.Agents)
					_ = s.saveAgentStoreLocked(store)
				}
			}
		}
		call.snapshot, call.err = resultSnap, err
		if m.probeInflight == call {
			m.probeInflight = nil
		}
		close(call.done)
		finished = true
		m.mu.Unlock()
		return resultSnap, err
	}
}

func (s *Server) invalidateAgentProbeCache() {
	s.agentCLIs.mu.Lock()
	s.agentCLIs.probeAt = time.Time{}
	s.agentCLIs.probeGen++
	s.agentCLIs.mu.Unlock()
}

func (m *agentManager) resetGuestCaches() {
	m.mu.Lock()
	m.defaultCache = nil
	m.pendingAttempted = nil
	m.loginNotice = nil
	m.mu.Unlock()
}

// triggerBackgroundAgentProbe spawns at most one async probe refresh so
// stale-while-revalidate callers never pile up goroutines. It registers as a
// server activity so closeService waits for it before returning.
func (s *Server) triggerBackgroundAgentProbe(specs []agentcatalog.AgentSpec) {
	m := &s.agentCLIs
	m.mu.Lock()
	if m.closed || m.probeInflight != nil || m.bgProbing {
		m.mu.Unlock()
		return
	}
	m.bgProbing = true
	m.mu.Unlock()

	actCtx, finish, err := s.beginActivity(s.lifetimeContext())
	if err != nil {
		m.mu.Lock()
		m.bgProbing = false
		m.mu.Unlock()
		return
	}

	specsCopy := slices.Clone(specs)
	go func() {
		defer finish()
		defer func() {
			m.mu.Lock()
			m.bgProbing = false
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(actCtx, 15*time.Second)
		defer cancel()
		_, _ = s.probeAllAgents(ctx, specsCopy, false)
	}()
}

type agentAccountSummary struct {
	Total       int    `json:"total"`
	Ready       int    `json:"ready"`
	Enabled     int    `json:"enabled"`
	CoolingDown int    `json:"cooling_down"`
	Active      int    `json:"active"`
	Strategy    string `json:"strategy"`
}

type agentCatalogEntry struct {
	Spec     agentcatalog.AgentSpec `json:"spec"`
	Runtime  agentRuntimeState      `json:"runtime"`
	Config   agentConfig            `json:"config"`
	Accounts agentAccountSummary    `json:"accounts"`
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	s.agentCLIs.mu.Lock()
	store, err := s.loadAgentStoreLocked()
	cachedProbe := cloneAgentProbeSnapshot(s.agentCLIs.probeCache)
	probeAge := time.Since(s.agentCLIs.probeAt)
	hasWarmCache := !s.agentCLIs.probeAt.IsZero()
	s.agentCLIs.mu.Unlock()
	if err != nil {
		requestError(w, err)
		return
	}
	specs := agentcatalog.All(store.CustomSpecs)

	// CRITICAL: call s.vmRunning() BEFORE s.mu.Lock() — on Windows guests
	// vmRunning() acquires s.mu internally, which would deadlock if nested.
	running := s.vmRunning()
	s.mu.Lock()
	ready := s.ready && !s.closing
	claudeCmd := claudeTerminalCommand(s.preferences)
	s.mu.Unlock()

	var probe agentProbeSnapshot
	probeError := ""
	if ready && running && s.windowsGuest == nil {
		if !force && hasWarmCache && probeAge < agentProbeTTL {
			probe = cachedProbe
		} else if !force && (hasWarmCache || len(store.LastProbe) > 0) {
			// Stale-while-revalidate (even on cold start when persisted LastProbe exists):
			// serve immediately (<0.5ms) and run at most one background refresh.
			if hasWarmCache {
				probe = cachedProbe
			} else {
				probe = agentProbeSnapshot{Agents: maps.Clone(store.LastProbe)}
			}
			s.triggerBackgroundAgentProbe(specs)
		} else {
			var pErr error
			probe, pErr = s.probeAllAgents(r.Context(), specs, force)
			if pErr != nil {
				probeError = pErr.Error()
			}
		}
	}

	// Zero-allocation per-agent live terminal counts.
	liveByAgent := s.terminals.allAgentCounts()

	// Read Claude account summary cleanly without triggering upstream probes.
	s.claudeAccounts.mu.Lock()
	claudeList, _ := s.loadClaudeAccounts()
	s.claudeAccounts.mu.Unlock()

	// Build non-Claude account summaries strictly inside s.agentCLIs.mu (no map/slice race).
	now := time.Now().Unix()
	poolSummaries := make(map[string]agentAccountSummary, len(specs))
	s.agentCLIs.mu.Lock()
	pools, _ := s.loadAgentPoolsLocked()
	for id, pool := range pools {
		if pool == nil {
			continue
		}
		sum := agentAccountSummary{
			Total:    len(pool.Accounts),
			Strategy: pool.Scheduling.normalized().Mode,
		}
		for _, a := range pool.Accounts {
			if a.Status == "ready" {
				sum.Ready++
			}
			if a.Enabled {
				sum.Enabled++
			}
			if a.CooldownUntil > now {
				sum.CoolingDown++
			}
		}
		poolSummaries[id] = sum
	}
	s.agentCLIs.mu.Unlock()

	items := make([]agentCatalogEntry, 0, len(specs))
	for _, spec := range specs {
		rt := store.LastProbe[spec.ID]
		if live, ok := probe.Agents[spec.ID]; ok {
			rt = live
		}
		cfg := store.Configs[spec.ID]
		if spec.ID == "claude" {
			cfg.LaunchCommand = claudeCmd
		}
		summary := agentAccountSummary{Active: liveByAgent[spec.ID], Strategy: "smart"}
		if spec.ID == "claude" {
			summary.Total = len(claudeList.Accounts)
			summary.Strategy = claudeList.Scheduling.Mode
			for _, a := range claudeList.Accounts {
				if a.Status == "ready" {
					summary.Ready++
				}
				if a.Enabled {
					summary.Enabled++
				}
			}
		} else if ps, ok := poolSummaries[spec.ID]; ok {
			ps.Active = liveByAgent[spec.ID]
			summary = ps
		}
		items = append(items, agentCatalogEntry{
			Spec:     spec,
			Runtime:  rt,
			Config:   cfg,
			Accounts: summary,
		})
	}
	jsonReply(w, map[string]any{
		"vm_running":    running,
		"ready":         ready,
		"probed_at":     probe.ProbedAt,
		"probe_error":   probeError,
		"prerequisites": probe.Prerequisites,
		"agents":        items,
	})
}

func (s *Server) installAgentHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		Channel string `json:"channel"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	spec, _, ok := s.lookupAgentSpec(in.AgentID)
	if !ok {
		http.Error(w, "未找到指定的 Agent CLI", http.StatusNotFound)
		return
	}
	ch, ok := spec.FindChannel(in.Channel)
	if !ok {
		http.Error(w, "未找到可用的官方安装渠道", http.StatusBadRequest)
		return
	}
	if err := agentcatalog.ValidateInstallChannel(ch); err != nil {
		requestError(w, err)
		return
	}
	running := s.vmRunning()
	s.mu.Lock()
	ready, paused, closing := s.ready, s.networkPaused, s.closing
	s.mu.Unlock()
	if closing || !ready || !running {
		http.Error(w, "请先启动 Cage 环境并等待就绪后再安装 Agent CLI", http.StatusConflict)
		return
	}
	if paused && ch.Kind != "existing" {
		http.Error(w, "网络已暂停，请先恢复网络连接后再安装或更新 Agent CLI", http.StatusConflict)
		return
	}
	if ch.Kind == "builtin" && spec.ID == "claude" {
		s.updateEnvironmentTools(w, r)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键远程安装，请在终端中使用官方命令安装", http.StatusConflict)
		return
	}
	s.agentCLIs.mu.Lock()
	if store, err := s.loadAgentStoreLocked(); err == nil {
		cfg := store.Configs[spec.ID]
		cfg.PreferredChannel = ch.ID
		store.Configs[spec.ID] = cfg
		_ = s.saveAgentStoreLocked(store)
	}
	s.agentCLIs.mu.Unlock()

	s.launch(w, "agent-install", func() error {
		s.setMessage(fmt.Sprintf("正在通过官方渠道安装 %s…", spec.Name))
		if err := s.runAgentRuntimeJob(s.operationContext(), "install", map[string]any{"spec": spec, "channel": ch}); err != nil {
			return fmt.Errorf("安装 %s 失败，请查看操作详情排查网络或依赖问题", spec.Name)
		}
		s.invalidateAgentProbeCache()
		s.setMessage(fmt.Sprintf("%s 安装完成，正在同步状态…", spec.Name))
		s.agentCLIs.mu.Lock()
		store, _ := s.loadAgentStoreLocked()
		s.agentCLIs.mu.Unlock()
		_, _ = s.probeAllAgents(s.operationContext(), agentcatalog.All(store.CustomSpecs), true)
		return nil
	})
}

func (s *Server) uninstallAgentHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		Channel string `json:"channel"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	spec, cfg, ok := s.lookupAgentSpec(in.AgentID)
	if !ok {
		http.Error(w, "未找到指定的 Agent CLI", http.StatusNotFound)
		return
	}
	if spec.ID == "claude" {
		http.Error(w, "Claude Code 为 Cage 内置核心组件，不可在此卸载", http.StatusConflict)
		return
	}
	if !s.vmRunning() {
		http.Error(w, "请先启动 Cage 环境后再卸载", http.StatusConflict)
		return
	}
	if s.windowsGuest != nil {
		http.Error(w, "当前 Windows 原生客机暂不支持一键卸载", http.StatusConflict)
		return
	}
	if s.terminals.allAgentCounts()[spec.ID] > 0 {
		http.Error(w, fmt.Sprintf("请先关闭仍在运行的 %s 终端后再卸载", spec.Name), http.StatusConflict)
		return
	}
	channelID := in.Channel
	if channelID == "" {
		channelID = cfg.PreferredChannel
	}
	ch, _ := spec.FindChannel(channelID)

	s.launch(w, "agent-uninstall", func() error {
		s.setMessage(fmt.Sprintf("正在卸载 %s（保留账号与项目数据）…", spec.Name))
		if err := s.runAgentRuntimeJob(s.operationContext(), "uninstall", map[string]any{"spec": spec, "channel": ch}); err != nil {
			return fmt.Errorf("卸载 %s 失败，请查看操作详情", spec.Name)
		}
		s.invalidateAgentProbeCache()
		s.agentCLIs.mu.Lock()
		store, _ := s.loadAgentStoreLocked()
		s.agentCLIs.mu.Unlock()
		_, _ = s.probeAllAgents(s.operationContext(), agentcatalog.All(store.CustomSpecs), true)
		return nil
	})
}

func (s *Server) updateAgentConfigHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID          string `json:"agent_id"`
		LaunchCommand    string `json:"launch_command"`
		ExtraArgs        string `json:"extra_args"`
		PreferredChannel string `json:"preferred_channel"`
		ImagePaste       *bool  `json:"image_paste"`
		ShiftEnterMode   string `json:"shift_enter_mode"`
		HiddenInMenu     *bool  `json:"hidden_in_menu"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	spec, _, ok := s.lookupAgentSpec(in.AgentID)
	if !ok {
		http.Error(w, "未找到指定的 Agent CLI", http.StatusNotFound)
		return
	}
	if err := agentcatalog.ValidateLaunchCommand(in.LaunchCommand); err != nil {
		requestError(w, err)
		return
	}
	if err := agentcatalog.ValidateLaunchCommand(in.ExtraArgs); err != nil {
		requestError(w, err)
		return
	}
	if in.ShiftEnterMode != "" && !slices.Contains([]string{"bracketed_newline", "csi_u", "plain"}, in.ShiftEnterMode) {
		http.Error(w, "无效的 Shift+Enter 模式", http.StatusBadRequest)
		return
	}
	if in.PreferredChannel != "" {
		if _, ok := spec.FindChannel(in.PreferredChannel); !ok {
			http.Error(w, "无效的安装渠道", http.StatusBadRequest)
			return
		}
	}

	var saveErr error
	func() {
		s.op.Lock()
		defer s.op.Unlock()

		if spec.ID == "claude" {
			s.mu.Lock()
			nextPrefs := s.preferences
			nextPrefs.ClaudeTerminalCommand = strings.TrimSpace(in.LaunchCommand)
			if nextPrefs.ClaudeTerminalCommand == "" {
				nextPrefs.ClaudeTerminalCommand = "claude"
			}
			s.mu.Unlock()
			if err := writePrivate(filepath.Join(s.profile, "workbench.json"), nextPrefs); err != nil {
				saveErr = errors.New("无法保存 Claude 启动命令")
				return
			}
			s.mu.Lock()
			s.preferences = nextPrefs
			s.mu.Unlock()
		}

		s.agentCLIs.mu.Lock()
		defer s.agentCLIs.mu.Unlock()
		store, err := s.loadAgentStoreLocked()
		if err != nil {
			saveErr = err
			return
		}
		cfg := store.Configs[spec.ID]
		cfg.LaunchCommand = strings.TrimSpace(in.LaunchCommand)
		cfg.ExtraArgs = strings.TrimSpace(in.ExtraArgs)
		if in.PreferredChannel != "" {
			cfg.PreferredChannel = in.PreferredChannel
		}
		if in.ImagePaste != nil {
			cfg.ImagePaste = in.ImagePaste
		}
		if in.ShiftEnterMode != "" {
			cfg.ShiftEnterMode = in.ShiftEnterMode
		}
		if in.HiddenInMenu != nil {
			cfg.HiddenInMenu = *in.HiddenInMenu
		}
		store.Configs[spec.ID] = cfg
		saveErr = s.saveAgentStoreLocked(store)
	}()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	// Release s.op BEFORE listAgents so any probe never stalls terminal creation.
	s.listAgents(w, r)
}

func (s *Server) saveCustomAgentHTTP(w http.ResponseWriter, r *http.Request) {
	var in agentcatalog.AgentSpec
	if !readJSON(w, r, &in) {
		return
	}
	spec, err := agentcatalog.ValidateCustomSpec(in)
	if err != nil {
		requestError(w, err)
		return
	}
	var saveErr error
	func() {
		s.op.Lock()
		defer s.op.Unlock()

		s.agentCLIs.mu.Lock()
		defer s.agentCLIs.mu.Unlock()
		store, err := s.loadAgentStoreLocked()
		if err != nil {
			saveErr = err
			return
		}
		replaced := false
		for i := range store.CustomSpecs {
			if store.CustomSpecs[i].ID == spec.ID {
				store.CustomSpecs[i] = spec
				replaced = true
				break
			}
		}
		if !replaced {
			if len(store.CustomSpecs) >= 32 {
				saveErr = errors.New("最多添加 32 个自定义 Agent")
				return
			}
			store.CustomSpecs = append(store.CustomSpecs, spec)
		}
		saveErr = s.saveAgentStoreLocked(store)
	}()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	s.invalidateAgentProbeCache()
	s.listAgents(w, r)
}

func (s *Server) deleteCustomAgentHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if agentcatalog.IsBuiltIn(id) {
		http.Error(w, "内置 Agent 不能删除", http.StatusConflict)
		return
	}
	if s.terminals.allAgentCounts()[id] > 0 {
		http.Error(w, "请先关闭该自定义 Agent 仍在运行的终端", http.StatusConflict)
		return
	}
	var saveErr error
	func() {
		s.op.Lock()
		defer s.op.Unlock()

		s.agentCLIs.mu.Lock()
		defer s.agentCLIs.mu.Unlock()
		store, err := s.loadAgentStoreLocked()
		if err != nil {
			saveErr = err
			return
		}
		before := len(store.CustomSpecs)
		store.CustomSpecs = slices.DeleteFunc(store.CustomSpecs, func(item agentcatalog.AgentSpec) bool {
			return item.ID == id
		})
		if len(store.CustomSpecs) == before {
			saveErr = errors.New("未找到指定的自定义 Agent")
			return
		}
		delete(store.Configs, id)
		delete(store.LastProbe, id)
		delete(s.agentCLIs.defaultCache, id)
		delete(s.agentCLIs.recent, id)
		delete(s.agentCLIs.lastPick, id)
		delete(s.agentCLIs.loginNotice, id)
		saveErr = s.saveAgentStoreLocked(store)
	}()
	if saveErr != nil {
		requestError(w, saveErr)
		return
	}
	s.invalidateAgentProbeCache()
	s.listAgents(w, r)
}
