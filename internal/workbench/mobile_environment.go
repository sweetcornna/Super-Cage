package workbench

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

const mobileProxyNodes = 2000

// The phone sees only the operation's outcome, never its diagnostic log.
type mobileJobView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Running  bool   `json:"running"`
	Message  string `json:"message"`
	Error    string `json:"error"`
	Recovery string `json:"recovery"`
}

func mobileJob(job Job) mobileJobView {
	return mobileJobView{job.ID, job.Kind, job.Running, job.Message, job.Error, job.Recovery}
}

func mobileJobReply(w http.ResponseWriter, job Job) { jsonReply(w, mobileJob(job)) }

// mobileNetwork follows the desktop status bar: only an unexpired open or
// checking grant of a running guest counts; everything else is closed.
func mobileNetwork(running bool, safety safetyState, now time.Time) map[string]any {
	view := map[string]any{"mode": cage.GateClosed, "reason": "", "until": nil, "exit_ip": safety.ExitIP, "verified_at": nil}
	if !safety.VerifiedAt.IsZero() {
		view["verified_at"] = safety.VerifiedAt
	}
	if gate := safety.Gate; gate != nil {
		view["reason"] = gate.Reason
		if !gate.Until.IsZero() {
			view["until"] = gate.Until
		}
		if running && gate.Deadline().After(now) && (gate.Mode == cage.GateOpen || gate.Mode == cage.GateChecking) {
			view["mode"] = gate.Mode
		}
	}
	// Why networking is closed or at risk and what happens next; absent while
	// nothing is pending. Older apps ignore it.
	if running && safety.Explanation != nil {
		view["explanation"] = safety.Explanation
	}
	return view
}

func (s *Server) mobileGuestSystem() string {
	switch {
	case s.windowsGuest != nil && s.windowsGuest.Remote():
		return "windows-remote"
	case s.windowsGuest != nil:
		return guestos.Windows11
	case s.macosGuest != nil:
		return guestos.MacOS
	}
	system, _ := s.installedLinuxSystem()
	return system
}

func (s *Server) mobileProxySummary(ctx context.Context) map[string]any {
	view := map[string]any{"state": "unavailable", "state_label": "代理配置无法读取", "selected": "", "selected_name": "", "route": []string{}, "network_open": false}
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		return view
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return view
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	state, label, _, _ := s.proxyState(ctx, p, cfg)
	node, _ := p.Find(p.Selected)
	view["state"], view["state_label"], view["selected"], view["selected_name"] = state, label, p.Selected, node.Name
	view["route"], view["network_open"] = proxyRoute(p), state == "ready"
	return view
}

func proxyRoute(p managedproxy.Store) []string {
	route, _ := p.Route()
	if route == nil {
		route = append([]string{}, p.Chain...)
	}
	return route
}

func (s *Server) mobileEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.windowsGuest != nil && !s.storageRecoveryStatus().Required {
		s.refreshWindowsComponents()
	}
	running := s.vmRunning()
	system := s.mobileGuestSystem()
	proxy := s.mobileProxySummary(r.Context())
	var job *mobileJobView
	if current := s.jobs.snapshot(); current.ID != "" {
		view := mobileJob(current)
		job = &view
	}
	s.mu.Lock()
	ready := s.ready && !s.closing
	s.refreshNetworkExplanationLocked(time.Now())
	view := map[string]any{
		"ready": ready, "vm_running": running, "guest_system": system,
		"workload_ready":              ready && running && system != "windows-remote",
		"environment_update_required": s.guestUpdateRequired, "job": job,
		"network": mobileNetwork(running, s.safetyViewLocked(time.Now()), time.Now()), "network_error": s.currentNetworkErrorLocked(),
		"proxy": proxy,
	}
	s.mu.Unlock()
	jsonReply(w, view)
}

// mobileStartable refuses what only the desktop setup can continue: a
// pending guest installation or an unreadable environment record.
func (s *Server) mobileStartable(w http.ResponseWriter) bool {
	if err := s.mobileStartError(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return false
	}
	return true
}

func (s *Server) mobileStartError() error {
	if err := s.requireStorageReady(); err != nil || s.windowsGuest != nil || s.macosGuest != nil {
		return err
	}
	pending, err := guestos.LoadInstallation(s.profile)
	if err != nil {
		return errors.New("客机安装记录无法读取，请在电脑上处理")
	}
	if pending != nil {
		return errors.New("客机尚未完成安装，请在电脑上的 Super Cage 继续安装")
	}
	_, err = s.installedLinuxSystem()
	return err
}

func (s *Server) mobileEnvironmentStart(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}, 1<<10) || !s.mobileAllowed(w, r) || !s.mobileStartable(w) {
		return
	}
	s.launchJob(w, "start", s.startRun(nil, ""), mobileJobReply)
}

func (s *Server) mobileEnvironmentStop(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}, 1<<10) || !s.mobileAllowed(w, r) {
		return
	}
	s.requestStopWith(w, false, mobileJobReply)
}

// A restart is one operation: no other operation can start between its stop
// and start. Like other internal restarts it keeps the proxy core running.
func (s *Server) mobileEnvironmentRestart(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}, 1<<10) || !s.mobileAllowed(w, r) || !s.mobileStartable(w) {
		return
	}
	start := s.startRun(nil, "")
	s.launchJob(w, "restart", func() error {
		if s.macosGuest != nil {
			// Stopping the guest revokes its current grant, as a stop does.
			_ = s.revokeNetworkNow("正在重启环境", false)
		}
		if s.vmRunning() {
			if err := s.stopEnvironment(); err != nil {
				return err
			}
		}
		return start()
	}, mobileJobReply)
}

// Source errors are shown only when they cannot carry a subscription address.
func mobileProxyError(message string) string {
	if message == "" {
		return ""
	}
	if strings.Contains(message, "://") {
		return "订阅更新失败，请在电脑上查看详情"
	}
	if len(message) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "…"
	}
	return message
}

func (s *Server) mobileProxy(w http.ResponseWriter, r *http.Request) {
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		http.Error(w, "无法读取代理配置", http.StatusServiceUnavailable)
		return
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		http.Error(w, "无法读取代理配置", http.StatusServiceUnavailable)
		return
	}
	sources := []map[string]any{}
	total, truncated := 0, false
	for _, src := range p.Sources {
		nodes := []map[string]string{}
		for _, n := range src.Nodes {
			if total >= mobileProxyNodes {
				truncated = true
				break
			}
			nodes = append(nodes, map[string]string{"id": n.ID, "name": n.Name, "type": n.Type})
			total++
		}
		sources = append(sources, map[string]any{"id": src.ID, "name": src.Name, "error": mobileProxyError(src.Error), "nodes": nodes})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	state, label, _, _ := s.proxyState(ctx, p, cfg)
	jsonReply(w, map[string]any{"state": state, "state_label": label, "selected": p.Selected, "route": proxyRoute(p), "sources": sources, "truncated": truncated})
}

func readMobileNode(w http.ResponseWriter, r *http.Request) (string, bool) {
	var input struct {
		NodeID string `json:"node_id"`
	}
	if !readJSON(w, r, &input, 4<<10) {
		return "", false
	}
	if input.NodeID == "" || len(input.NodeID) > 512 || !utf8.ValidString(input.NodeID) {
		http.Error(w, "请选择节点", http.StatusBadRequest)
		return "", false
	}
	return input.NodeID, true
}

func (s *Server) mobileProxySelect(w http.ResponseWriter, r *http.Request) {
	id, ok := readMobileNode(w, r)
	if !ok || !s.mobileAllowed(w, r) {
		return
	}
	p, err := managedproxy.Load(s.profile)
	if err != nil {
		http.Error(w, "无法读取代理配置", http.StatusServiceUnavailable)
		return
	}
	if _, found := p.Find(id); !found {
		http.Error(w, "所选节点已不存在，请刷新列表", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	pauseVersion := s.safety.pauseVersion
	s.mu.Unlock()
	s.launchJob(w, "proxy", func() error { return s.changeProxy("select", proxyInput{NodeID: id}, &pauseVersion) }, mobileJobReply)
}

func (s *Server) mobileProxyDelay(w http.ResponseWriter, r *http.Request) {
	id, ok := readMobileNode(w, r)
	if !ok {
		return
	}
	delay, err := s.proxyNodeDelay(r.Context(), id)
	switch {
	case errors.Is(err, errProxyNodeMissing):
		http.Error(w, "所选节点已不存在，请刷新列表", http.StatusNotFound)
	case errors.Is(err, errProxyDelay):
		jsonReply(w, map[string]int{"delay": 0})
	case err != nil:
		http.Error(w, "无法读取代理配置", http.StatusServiceUnavailable)
	default:
		jsonReply(w, map[string]int{"delay": delay})
	}
}
