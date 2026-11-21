package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// Claude Science phase 2a, workbench side: when host rendering is on, the
// workload runs a front (science_front.go) and the Windows shell frames it in
// the desktop entry's document. Off is the default, and off changes nothing:
// no listener, frame-src 'none', no request treated differently.
//
// The actions that change host rendering (host-render, ticket, release) are
// the window's alone: they need the fetch metadata of a same-origin script
// request (scienceFromWindow), which the agent transport's client and the
// phone do not send.
//
// Contract additions (all behind the existing session, Origin and account
// checks of /api; errors are plain Chinese text):
//
//	GET /api/claude-science -> host_render (scienceHostRenderState):
//	  available / unavailable_code ("platform" | "client" | "guest") /
//	  unavailable_reason, enabled, running, error, audit_note, and the
//	  front's refused / blocked / audit / session / signin fields. The
//	  front port is never in it: the Windows shell reads it from control.json.
//	GET /api/claude-science/host-render -> host_render alone, without
//	  reading the guest: the pane polls it while its frame is open. A new
//	  ticket clears session and refused, so session=true afterwards means
//	  that ticket's document passed the probe.
//	POST /api/claude-science/host-render {"enabled": bool} -> host_render
//	  409 when turning on is unavailable here.
//	POST /api/claude-science/ticket {"host": {timezone, offset, languages}}
//	  -> {"url": "http://<label>.localhost:<port>/_cage/enter"}; 60 s, single
//	  use, bound to this workbench session's account epoch. host is what the
//	  workbench document measures (the shell never changes it).
//	  409 unavailable, off, environment or Claude Science not running, or the
//	  guest browser's identity unreadable.
//	POST /api/claude-science/release {} -> 204: the pane closed its frame;
//	  the front ends the session and forgets the daemon credential. When
//	  the release cannot arrive (the window switched to another environment
//	  or closed), the session ends after 90 s without the pane's poll or
//	  the page's requests (scienceFront.dropOrphan).
//
// In the desktop entry, host-render and ticket are refused unless the
// selected environment is this computer's (scienceEntryRefuses).

// scienceHostOS is where host rendering may run (tests replace it).
var scienceHostOS = runtime.GOOS

type scienceHost struct {
	mu    sync.Mutex
	front *scienceFront
	err   string
	stop  context.CancelFunc
	// vmRunning replaces Server.vmRunning in tests.
	vmRunning func() bool
}

type scienceHostRenderState struct {
	Available         bool   `json:"available"`
	UnavailableCode   string `json:"unavailable_code,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Enabled           bool   `json:"enabled"`
	Running           bool   `json:"running"`
	Error             string `json:"error,omitempty"`
	AuditNote         string `json:"audit_note,omitempty"`
	scienceFrontState
}

const (
	scienceReasonHostPlatform = "本机渲染目前只在 Windows 版 Super Cage 窗口中提供：只有它的网页组件能把页面的时区、语言和浏览器标识换成环境的。"
	scienceReasonHostClient   = "本机渲染只能在这台电脑的 Super Cage 窗口中使用；浏览器、手机和远程连接继续使用隔离浏览器。"
	scienceReasonHostGuest    = "这个环境不能运行 Claude Science。"
)

// scienceHostPlatform: what this workbench process allows regardless of the
// requester.
func (s *Server) scienceHostPlatform() (string, string) {
	switch {
	case scienceHostOS != "windows":
		return "platform", scienceReasonHostPlatform
	case s.desktopEntry || s.clientOnly:
		return "client", scienceReasonHostClient
	case s.windowsGuest != nil || s.macosGuest != nil || scienceHostArch != "amd64":
		return "guest", scienceReasonHostGuest
	}
	return "", ""
}

// scienceHostAvailability adds the requester: only the local desktop window
// (its requests arrive through the desktop entry with the trusted-view
// proof). The phone, a tenant gateway, a remote desktop and a browser opened
// on this workbench directly are refused.
func (s *Server) scienceHostAvailability(r *http.Request) (string, string) {
	if code, reason := s.scienceHostPlatform(); code != "" {
		return code, reason
	}
	if !s.hasTrustedView(r) || s.pairedPhoneRoom(r) != nil {
		return "client", scienceReasonHostClient
	}
	return "", ""
}

func (s *Server) scienceFrontRunning() *scienceFront {
	s.scienceHost.mu.Lock()
	defer s.scienceHost.mu.Unlock()
	return s.scienceHost.front
}

func (s *Server) scienceHostRender(r *http.Request) *scienceHostRenderState {
	state := &scienceHostRenderState{}
	state.UnavailableCode, state.UnavailableReason = s.scienceHostAvailability(r)
	state.Available = state.UnavailableCode == ""
	s.mu.Lock()
	state.Enabled = s.preferences.ScienceHostRender
	s.mu.Unlock()
	s.scienceHost.mu.Lock()
	front, startErr := s.scienceHost.front, s.scienceHost.err
	s.scienceHost.mu.Unlock()
	state.Error = startErr
	if front != nil {
		state.Running = true
		state.scienceFrontState = front.snapshot()
		if audit := state.Audit; audit.Known && !audit.Timezone && !audit.Offset && !audit.Locale {
			state.AuditNote = "本机与环境的时区、UTC 偏移和语言相同，审计无从区分；页面看到的仍是环境的值。"
		} else if audit.Known && !audit.Timezone && !audit.Offset {
			state.AuditNote = "本机与环境的时区相同，审计只检查语言。"
		}
	}
	return state
}

// syncScienceFront starts or stops the front to match the setting.
func (s *Server) syncScienceFront() {
	s.mu.Lock()
	enabled, closing := s.preferences.ScienceHostRender, s.closing
	s.mu.Unlock()
	code, _ := s.scienceHostPlatform()
	want := enabled && code == "" && !closing
	h := &s.scienceHost
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case want && h.front == nil:
		listener, port, err := listenScienceFront(s.profile)
		if err != nil {
			h.err = err.Error()
			return
		}
		h.err = ""
		front := newScienceFront(listener, port, s.scienceFrontHooks())
		go front.serve()
		h.front = front
		ctx, cancel := context.WithCancel(s.lifetimeContext())
		h.stop = cancel
		go s.watchScienceFront(ctx, front)
		if err := s.publishScienceControl(port, nil, false); err != nil {
			h.err = "无法记录本机渲染端口：" + err.Error()
		}
	case !want && h.front != nil:
		h.stop()
		h.front.close()
		h.front, h.stop = nil, nil
		_ = s.publishScienceControl(0, nil, true)
	}
}

func (s *Server) stopScienceFront() {
	h := &s.scienceHost
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.front != nil {
		h.stop()
		h.front.close()
		h.front, h.stop = nil, nil
	}
}

// scienceWatchInterval is how often the front's watchdog looks (tests
// shorten it).
var scienceWatchInterval = 3 * time.Second

// watchScienceFront drops every session and the daemon credential when the
// account locks, the environment stops or the daemon stops or restarts, and
// the credential alone after scienceIdleDrop without use.
func (s *Server) watchScienceFront(ctx context.Context, front *scienceFront) {
	ticker := time.NewTicker(scienceWatchInterval)
	defer ticker.Stop()
	started := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		front.mu.Lock()
		active := front.upstream != nil || front.session != ""
		front.mu.Unlock()
		if !active {
			continue
		}
		if _, unlocked := s.account.accessFor(true, false); !unlocked {
			front.reset("Super Cage 已锁定，本机渲染已关闭。")
			continue
		}
		if !s.scienceVMRunning() {
			front.reset("环境已停止，本机渲染已关闭。")
			continue
		}
		if last, ok := s.scienceLast.get(); ok {
			if !last.Ready || (started != "" && last.StartedAt != started) {
				// Stopped, or restarted with a new credential.
				front.reset("")
			}
			started = last.StartedAt
		}
		front.dropOrphan()
		front.dropIdle()
	}
}

// scienceVMRunning is vmRunning, replaceable in tests.
func (s *Server) scienceVMRunning() bool {
	if s.scienceHost.vmRunning != nil {
		return s.scienceHost.vmRunning()
	}
	return s.vmRunning()
}

// resetScienceFront ends the frame's session and forgets the daemon
// credential (Claude Science stopping or updating, the pane closing).
func (s *Server) resetScienceFront() {
	if front := s.scienceFrontRunning(); front != nil {
		front.reset("")
	}
}

func (s *Server) scienceFrontHooks() scienceFrontHooks {
	var readyMu sync.Mutex
	var readyAt time.Time
	var readyErr error
	return scienceFrontHooks{
		ready: func() error {
			readyMu.Lock()
			defer readyMu.Unlock()
			if time.Since(readyAt) < time.Second {
				return readyErr
			}
			readyAt = time.Now()
			readyErr = nil
			switch {
			case !s.scienceVMRunning():
				readyErr = errors.New("请先启动 Super Cage 环境")
			case s.claudeScienceOperation() != "":
				readyErr = errors.New("Claude Science 正在安装、更新、启动或关闭，请稍候。")
			default:
				if last, ok := s.scienceLast.get(); !ok || !last.Ready {
					readyErr = errors.New("Claude Science 尚未启动，请在工作台启动后再打开。")
				}
			}
			return readyErr
		},
		ancestor: s.scienceAncestor,
		upstream: func() (*scienceUpstream, error) {
			if !s.scienceVMRunning() {
				return nil, errors.New("请先启动 Super Cage 环境")
			}
			port := s.sciencePort()
			return newScienceUpstream(s.newScienceDialer(port), port, s.scienceLink), nil
		},
		signIn: func() (claudeScienceTab, error) {
			ctx, cancel := context.WithTimeout(s.lifetimeContext(), 20*time.Second)
			defer cancel()
			s.mu.Lock()
			ready, token := s.ready, s.credentials.Token
			s.mu.Unlock()
			if !ready {
				return claudeScienceTab{}, errors.New("隔离浏览器仍在启动，请稍后再登录。")
			}
			output, err := s.guestQuick(ctx, claudeScienceLinkScript)
			if err != nil {
				return claudeScienceTab{}, errors.New("暂时无法打开登录页，请重试。")
			}
			tab, _, err := s.openClaudeScienceLink(ctx, token, output, true)
			return tab, err
		},
	}
}

// sciencePort is the daemon's port as its status reports it (the unit
// starts it on cage.ClaudeSciencePort).
func (s *Server) sciencePort() int {
	if last, ok := s.scienceLast.get(); ok && last.Port >= 1024 && last.Port <= 65535 {
		return last.Port
	}
	return cage.ClaudeSciencePort
}

// scienceLink mints the daemon's single-use link in the guest; the value
// stays in memory.
func (s *Server) scienceLink(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := s.guestQuick(ctx, claudeScienceLinkScript)
	if err != nil {
		return "", errors.New("无法连接环境内的 Claude Science，请确认它仍在运行")
	}
	link, err := parseClaudeScienceLink(output)
	if err != nil {
		return "", err
	}
	return link.url, nil
}

// scienceRoot is the default local profile that owns desktop-entry/ and vms/.
func scienceRoot(profile string) string {
	if filepath.Base(filepath.Dir(profile)) == cage.LocalVMDir {
		return filepath.Dir(filepath.Dir(profile))
	}
	return profile
}

// scienceAncestor is the desktop entry's origin: the only document allowed to
// frame the front.
func (s *Server) scienceAncestor() string {
	data, err := os.ReadFile(filepath.Join(scienceRoot(s.profile), "desktop-entry", "control.json"))
	if err != nil {
		return ""
	}
	var record controlFile
	if json.Unmarshal(data, &record) != nil || record.Port < 1024 || record.Port > 65535 {
		return ""
	}
	return "http://localhost:" + strconv.Itoa(record.Port)
}

// publishScienceControl records the front port (and the identity the shell
// applies) in this workload's control.json, which only the native shell and
// the desktop entry read. Port 0 removes both.
func (s *Server) publishScienceControl(port int, identity *scienceIdentity, keepIdentity bool) error {
	if !s.controlPublished {
		return nil
	}
	path := filepath.Join(s.profile, "control.json")
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record controlFile
		if err = json.Unmarshal(data, &record); err != nil {
			return err
		}
		record.SciencePort = port
		if port == 0 {
			record.ScienceIdentity = nil
		} else if identity != nil || !keepIdentity {
			record.ScienceIdentity = identity
		}
		// A reader holding the file open on Windows makes the replace fail
		// for a moment; retry.
		if last = writePrivate(path, record); last == nil {
			return nil
		}
	}
	return last
}

// localScienceFronts is the desktop entry's view: the fronts that local
// workloads published in their control files. Read when a document is
// served, never on a timer.
func (s *Server) localScienceFronts() []int {
	if scienceHostOS != "windows" || !s.desktopEntry || s.remote == nil || s.remote.desktop == nil || s.remote.desktop.localVMs == nil {
		return nil
	}
	root := s.remote.desktop.localVMs.defaultProfile
	profiles := []string{root}
	if matches, err := filepath.Glob(filepath.Join(root, cage.LocalVMDir, "*")); err == nil {
		profiles = append(profiles, matches...)
	}
	var ports []int
	for _, profile := range profiles {
		data, err := os.ReadFile(filepath.Join(profile, "control.json"))
		if err != nil || len(data) > 64<<10 {
			continue
		}
		var record controlFile
		if json.Unmarshal(data, &record) == nil && record.SciencePort >= 1024 && record.SciencePort <= 65535 {
			ports = append(ports, record.SciencePort)
		}
	}
	return ports
}

// scienceFrameSources is the workbench document's frame-src: 'none' unless a
// local front runs (only the desktop entry's document frames it). Only a
// document navigation needs it; every other response keeps 'none' and
// reads nothing.
func (s *Server) scienceDocumentSources(r *http.Request) (frames, connect string) {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "document" {
		return "'none'", ""
	}
	ports := s.localScienceFronts()
	if len(ports) == 0 {
		return "'none'", ""
	}
	sources := make([]string, 0, len(ports))
	for _, port := range ports {
		sources = append(sources, scienceFrameSource(port))
	}
	return strings.Join(sources, " "), " " + scienceProxyCheckOrigin
}

// scienceProxyCheckOrigin is the address the window's proxy proof loads
// (ScienceFrame.ProxyCheckURL): .invalid never resolves. The workbench
// document may connect to it only while a front runs, since the check is
// held to the document's connect-src.
const scienceProxyCheckOrigin = "http://cage-proxy-check.invalid"

// scienceBridgeContract is the scienceFrame bridge version this server's
// pane needs (claude-science-state.mjs hostRenderContract; the window's
// ScienceFrame.Contract): 2 scopes the identity to the frame's targets.
const scienceBridgeContract = 2

const (
	scienceReasonUpdateWindows = "请更新 Windows 应用后再开启本机渲染：这个窗口还不能把环境的身份只交给 Claude Science 页面。"
	scienceReasonProxyUnproven = "窗口没能证明它的代理开关已生效（本机渲染需要它挡住页面的外部连接），本机渲染未打开。请重新载入窗口后重试。"
)

// scienceFromWindow: a script request of the workbench document itself
// (fetch metadata, which browsers always send to loopback). The agent
// transport's client, the phone link and other programs send none; a local
// program could forge it.
func scienceFromWindow(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") == "same-origin" && r.Header.Get("Sec-Fetch-Mode") == "cors" && r.Header.Get("Sec-Fetch-Dest") == "empty"
}

// scienceWindowOnly answers 403 when the request is not the window's own.
func scienceWindowOnly(w http.ResponseWriter, r *http.Request) bool {
	if scienceFromWindow(r) {
		return false
	}
	http.Error(w, "本机渲染只能在 Super Cage 窗口中操作。", http.StatusForbidden)
	return true
}

// scienceEntryRefuses runs in the desktop entry before a request goes to the
// selected workload: the front lives on this computer, so a remote host's
// switch and tickets are not offered here. The workload cannot tell a remote
// desktop client from the local window (both carry the trusted-view proof).
func (s *Server) scienceEntryRefuses(w http.ResponseWriter, r *http.Request) bool {
	if !s.desktopEntry || r.Method != http.MethodPost || (r.URL.Path != "/api/claude-science/host-render" && r.URL.Path != "/api/claude-science/ticket") {
		return false
	}
	if scienceHostOS == "windows" && s.remote != nil && s.remote.desktop != nil && s.remote.desktop.context().Mode == "local" {
		return false
	}
	http.Error(w, scienceReasonHostClient, http.StatusConflict)
	return true
}

func (s *Server) scienceReleaseAction(w http.ResponseWriter, r *http.Request) {
	if scienceWindowOnly(w, r) {
		return
	}
	var input struct{}
	if !readJSON(w, r, &input) {
		return
	}
	s.resetScienceFront()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scienceHostRenderAction(w http.ResponseWriter, r *http.Request) {
	if scienceWindowOnly(w, r) {
		return
	}
	var input struct {
		Enabled  *bool `json:"enabled"`
		Contract int   `json:"contract"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Enabled == nil {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
		return
	}
	if *input.Enabled {
		if code, reason := s.scienceHostAvailability(r); code != "" {
			http.Error(w, reason, http.StatusConflict)
			return
		}
		// F8: a window whose bridge predates the frame contract would put
		// the guest's identity on the whole workbench.
		if input.Contract < scienceBridgeContract {
			http.Error(w, scienceReasonUpdateWindows, http.StatusConflict)
			return
		}
	}
	s.mu.Lock()
	next := s.preferences
	next.ScienceHostRender = *input.Enabled
	err := writePrivate(filepath.Join(s.profile, "workbench.json"), next)
	if err == nil {
		s.preferences.ScienceHostRender = *input.Enabled
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "无法保存本机渲染设置："+err.Error(), http.StatusInternalServerError)
		return
	}
	s.syncScienceFront()
	jsonReply(w, s.scienceHostRender(r))
}

// scienceHostRenderRead is the pane's poll while its frame is open: the
// front's state only, without reading the guest.
func (s *Server) scienceHostRenderRead(w http.ResponseWriter, r *http.Request) {
	// Only the window's own poll keeps a session alive.
	if front := s.scienceFrontRunning(); front != nil && scienceFromWindow(r) {
		front.heard()
	}
	jsonReply(w, s.scienceHostRender(r))
}

func (s *Server) scienceTicketAction(w http.ResponseWriter, r *http.Request) {
	if scienceWindowOnly(w, r) {
		return
	}
	var input struct {
		Host     scienceHostIdentity `json:"host"`
		Proxy    string              `json:"proxy"`
		Contract int                 `json:"contract"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !input.Host.valid() {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
		return
	}
	if input.Contract < scienceBridgeContract {
		http.Error(w, scienceReasonUpdateWindows, http.StatusConflict)
		return
	}
	// F5: the shell proved, in this WebView, that a request to an address
	// nobody can serve ends at the switched proxy (ScienceFrame.ProxyCheck).
	if input.Proxy != "verified" {
		http.Error(w, scienceReasonProxyUnproven, http.StatusConflict)
		return
	}
	if code, reason := s.scienceHostAvailability(r); code != "" {
		http.Error(w, reason, http.StatusConflict)
		return
	}
	front := s.scienceFrontRunning()
	if front == nil {
		http.Error(w, "本机渲染尚未开启。", http.StatusConflict)
		return
	}
	front.heard()
	if s.claudeScienceOperation() != "" {
		http.Error(w, "Claude Science 正在安装、更新、启动或关闭，请等待完成后再打开。", http.StatusConflict)
		return
	}
	if last, ok := s.scienceLast.get(); !ok || !last.Ready {
		http.Error(w, "Claude Science 尚未启动，请先启动后再打开。", http.StatusConflict)
		return
	}
	epoch, _ := r.Context().Value(accountEpochKey{}).(context.Context)
	if epoch == nil || epoch.Err() != nil {
		http.Error(w, "Super Cage 已锁定，请先登录", http.StatusLocked)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	identity, err := s.readScienceIdentity(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.publishScienceControl(front.port, &identity, true); err != nil {
		http.Error(w, "无法交给 Super Cage 窗口环境身份，请重试。", http.StatusInternalServerError)
		return
	}
	jsonReply(w, map[string]string{"url": front.issue(scienceTicket{epoch: epoch, identity: identity, host: input.Host})})
}
