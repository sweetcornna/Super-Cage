package workbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/agents"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/version"
)

// 设置 → 其他客户端: optional agent clients added to, updated in and removed
// from the environment as host jobs (design 4). The installer is
// internal/cage/agent-install.sh (docs/agent-install.md); this file is its
// host side. Nothing here runs, parses the catalog or reads the guest until
// a request of these routes arrives (guarantee G, design 9.1).
//
// Contract for the window (all routes need the workbench session, writes the
// workbench Origin; a locked account gets 423; errors are plain Chinese text):
//
//	GET /api/agents[?verify=1]
//	  200 {"agents": [entry…], "environment": {running, arch, guest_system,
//	      disk_free_mb, clients_known}}. Without verify no guest is read.
//	      verify=1 (opening the section) reads the guest once (presence,
//	      installed versions, free disk), at most 4 s, when the environment
//	      runs and no job owns it.
//	  entry: id, name, vendor, kind, summary, with_environment, person_only,
//	      status, pinned_version, present ({by: "cage"|"own", version, state:
//	      "ok"|"drifted"|"unknown"} or null), update_available, supported,
//	      unsupported_code, unsupported_reason, download_mb, disk_mb,
//	      hosts{install, signin, run, telemetry}, telemetry_note, region_note,
//	      terms_url, data_dirs, allowed, operation ("install"|"update"|"remove"
//	      while a job of this entry runs), last_failure ({code, message} of
//	      this entry's last failed job in this session, or null), mark.
//	  Hidden entries are listed only in a developer build started with
//	  CAGE_AGENTS_HIDDEN=1.
//
//	POST /api/agents/{id}/install, /update   body {}
//	POST /api/agents/{id}/remove             body {} or {"remove_data": true}
//	  200 Job, kind agent-<action>:<id>; follow it in /api/status. Its
//	      setup_progress has phase "agent" and operation check, download,
//	      digest, unpack, verify, done or failed; a failed job's error is the
//	      installer's line for the person.
//	  400 malformed body (unknown field; remove_data outside remove)
//	  404 unknown id or action (a hidden entry in a release build is unknown)
//	  409 unsupported (the reason: unsupported codes guest_system, kind,
//	      architecture, withdrawn, keyring), environment stopped, the client
//	      already installed by the person (install), another job running
//	  503 the workbench is closing
//	  Window only: cage agent gets native_transport_required.
//
//	POST /api/agents/cancel   body {"job": "<job id>"}
//	  204 canceled (or already ended); 404 no such job; 409 the running job
//	      is not an agent job. Window only.

// agentsHostArch is the guest's architecture: a local guest has the host's.
var agentsHostArch = runtime.GOARCH

// agentsHiddenAllowed: a developer build started with CAGE_AGENTS_HIDDEN=1
// lists and installs hidden entries (their real-guest acceptance, design
// 11 step 1.8). A release build never does.
var agentsHiddenAllowed = func() bool {
	return strings.Contains(version.Version, "-dev") && os.Getenv("CAGE_AGENTS_HIDDEN") == "1"
}

// agentMaintenance runs one job's script through the maintenance channel;
// agentGuestRead is the one read of presence (and versions) after a job and
// on verify. Tests replace both: they never reach ssh.
var (
	agentMaintenance = RunMaintenanceScriptWithHosts
	agentGuestRead   = (*Server).guestQuick
)

// agentJobActions are the job kinds' actions: agent-<action>:<id>.
var agentJobActions = []string{"install", "update", "remove"}

// agentJobKind splits a job kind of this feature.
func agentJobKind(kind string) (action, id string, ok bool) {
	rest, found := strings.CutPrefix(kind, "agent-")
	if !found {
		return "", "", false
	}
	action, id, found = strings.Cut(rest, ":")
	return action, id, found && slices.Contains(agentJobActions, action) && clientIDPattern.MatchString(id)
}

// agentJobRenewsLease: a long download must not let the gate lease lapse
// (safety.go networkMonitorBlockedLocked).
func agentJobRenewsLease(kind string) bool {
	_, _, ok := agentJobKind(kind)
	return ok
}

func agentJobName(action, id string) string {
	name := "客户端"
	if e, ok, err := agents.Lookup(id); err == nil && ok {
		name = e.Name
	}
	return map[string]string{"install": "添加 ", "update": "更新 ", "remove": "移除 "}[action] + name
}

// agentState is what the host knows beyond presence (client_presence.go):
// the last outcome of each entry's job and the last verify reading.
type agentState struct {
	mu       sync.Mutex
	outcomes map[string]agentOutcome
	guest    map[string]agentInstalled
	freeKB   int64
	read     bool
}

type agentOutcome struct {
	done, removed bool
	version       string
	failure       *agentFailureView
}

type agentInstalled struct{ version, state string }

func (a *agentState) record(id string, outcome agentOutcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.outcomes == nil {
		a.outcomes = map[string]agentOutcome{}
	}
	a.outcomes[id] = outcome
	if outcome.removed {
		delete(a.guest, id)
	} else if outcome.done {
		if a.guest == nil {
			a.guest = map[string]agentInstalled{}
		}
		a.guest[id] = agentInstalled{version: outcome.version, state: "ok"}
	}
}

type agentPresentView struct {
	By      string `json:"by"`
	Version string `json:"version"`
	State   string `json:"state"`
}

type agentFailureView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentHostsView struct {
	Install   []string `json:"install"`
	Signin    []string `json:"signin"`
	Run       []string `json:"run"`
	Telemetry []string `json:"telemetry"`
}

type agentView struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Vendor            string            `json:"vendor"`
	Kind              string            `json:"kind"`
	Summary           string            `json:"summary"`
	WithEnvironment   bool              `json:"with_environment"`
	PersonOnly        bool              `json:"person_only"`
	Status            string            `json:"status"`
	PinnedVersion     string            `json:"pinned_version"`
	Present           *agentPresentView `json:"present"`
	UpdateAvailable   bool              `json:"update_available"`
	Supported         bool              `json:"supported"`
	UnsupportedCode   string            `json:"unsupported_code,omitempty"`
	UnsupportedReason string            `json:"unsupported_reason,omitempty"`
	DownloadMB        int64             `json:"download_mb"`
	DiskMB            int64             `json:"disk_mb"`
	Hosts             agentHostsView    `json:"hosts"`
	TelemetryNote     string            `json:"telemetry_note"`
	RegionNote        string            `json:"region_note"`
	TermsURL          string            `json:"terms_url"`
	DataDirs          []string          `json:"data_dirs"`
	Allowed           bool              `json:"allowed"`
	Operation         string            `json:"operation"`
	LastFailure       *agentFailureView `json:"last_failure"`
	Mark              agents.Mark       `json:"mark"`
}

type agentEnvironmentView struct {
	Running      bool   `json:"running"`
	Arch         string `json:"arch"`
	GuestSystem  string `json:"guest_system"`
	DiskFreeMB   *int64 `json:"disk_free_mb"`
	ClientsKnown bool   `json:"clients_known"`
}

// agentEntries are the entries this build shows.
func agentEntries() ([]agents.Entry, error) {
	if agentsHiddenAllowed() {
		return agents.All()
	}
	return agents.Visible()
}

func agentLookup(id string) (agents.Entry, bool, error) {
	e, ok, err := agents.Lookup(id)
	if err != nil || !ok || (e.Status == agents.Hidden && !agentsHiddenAllowed()) {
		return agents.Entry{}, false, err
	}
	return e, true, nil
}

func (s *Server) agentGuestSystem() string {
	switch {
	case s.macosGuest != nil:
		return "macos"
	case s.windowsGuest != nil:
		return "windows"
	}
	return "linux"
}

// agentUnsupported is what the host knows before asking the guest. The
// guest refuses the rest (libc, user namespaces, disk) in the job itself.
func (s *Server) agentUnsupported(e agents.Entry, action string) (code, reason string) {
	switch s.agentGuestSystem() {
	case "macos":
		return "guest_system", "macOS 客机暂不支持其他客户端。"
	case "windows":
		return "guest_system", "Windows 客机暂不支持其他客户端。"
	}
	if e.Kind != agents.Terminal {
		return "kind", e.Name + " 是图形应用，暂不支持添加。"
	}
	if action == "remove" {
		return "", ""
	}
	arch, ok := agents.ParseArch(agentsHostArch)
	if _, pinned := e.Artifact(arch); !ok || !pinned {
		return "architecture", e.Name + " 没有适用于 " + agentsHostArch + " 的版本，无法添加。"
	}
	if e.Status == agents.Withdrawn {
		return "withdrawn", e.Name + " 已不再受 Super Cage 支持，只能移除。"
	}
	if e.Requires.Keyring {
		return "keyring", e.Name + " 需要的密钥环支持尚未完成，暂时无法添加。"
	}
	return "", ""
}

func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) {
	entries, err := agentEntries()
	if err != nil {
		http.Error(w, "无法读取其他客户端目录，请重新安装 Super Cage。", http.StatusInternalServerError)
		return
	}
	running := s.vmRunning()
	if r.URL.Query().Get("verify") == "1" && running && s.agentGuestSystem() == "linux" && s.guestAdmitsLockFreeCalls() {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		s.readAgentGuest(ctx, entries)
		cancel()
	}
	job := s.jobs.snapshot()
	jobAction, jobID, jobRunning := agentJobKind(job.Kind)
	jobRunning = jobRunning && job.Running
	s.mu.Lock()
	presence := s.clients
	s.mu.Unlock()
	s.agentState.mu.Lock()
	defer s.agentState.mu.Unlock()
	views := make([]agentView, 0, len(entries))
	for _, e := range entries {
		v := agentView{
			ID: e.ID, Name: e.Name, Vendor: e.Vendor, Kind: string(e.Kind), Summary: e.SummaryZH,
			WithEnvironment: e.WithEnvironment, PersonOnly: e.PersonOnly, Status: string(e.Status), PinnedVersion: e.Version,
			Hosts:         agentHostsView{Install: nonNil(e.Hosts.Install), Signin: nonNil(e.Hosts.Signin), Run: nonNil(e.Hosts.Run), Telemetry: nonNil(e.Hosts.Telemetry)},
			TelemetryNote: e.Telemetry.NoteZH, RegionNote: e.Regions.NoteZH, TermsURL: e.Terms.URL, DataDirs: []string{}, Allowed: true,
			Mark: e.Mark,
		}
		for _, d := range e.DataDirs {
			v.DataDirs = append(v.DataDirs, d.Path)
		}
		if arch, ok := agents.ParseArch(agentsHostArch); ok {
			if a, pinned := e.Artifact(arch); pinned {
				v.DownloadMB, v.DiskMB = (a.Size+1<<20-1)>>20, (a.Unpacked+1<<20-1)>>20
			}
		}
		if e.Optional() {
			v.UnsupportedCode, v.UnsupportedReason = s.agentUnsupported(e, "install")
			v.Present = s.agentState.presentLocked(e.ID, presence)
			if v.Present != nil && v.Present.By == "cage" && v.Present.Version != "" && e.Version != "" {
				v.UpdateAvailable = !version.AtLeast(v.Present.Version, e.Version)
			}
			if outcome, ok := s.agentState.outcomes[e.ID]; ok {
				v.LastFailure = outcome.failure
			}
			if jobRunning && jobID == e.ID {
				v.Operation = jobAction
			}
		}
		v.Supported = v.UnsupportedCode == ""
		views = append(views, v)
	}
	env := agentEnvironmentView{Running: running, Arch: agentsHostArch, GuestSystem: s.agentGuestSystem(), ClientsKnown: presence.Known}
	if s.agentState.read {
		mb := s.agentState.freeKB / 1024
		env.DiskFreeMB = &mb
	}
	jsonReply(w, map[string]any{"agents": views, "environment": env})
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// presentLocked combines presence (the command found, receipts) with what
// the jobs and the last verify read said about versions and drift.
func (a *agentState) presentLocked(id string, presence clientPresence) *agentPresentView {
	installed, read := a.guest[id]
	outcome, ran := a.outcomes[id]
	if !presence.Known {
		if ran && outcome.done {
			return &agentPresentView{By: "cage", Version: outcome.version, State: "ok"}
		}
		return nil
	}
	found, receipt := slices.Contains(presence.Clients, id), slices.Contains(presence.Receipts, id)
	switch {
	case found && !receipt:
		return &agentPresentView{By: "own", State: "unknown"}
	case receipt:
		p := &agentPresentView{By: "cage", State: "unknown"}
		if read {
			p.Version, p.State = installed.version, installed.state
		}
		if !found {
			// Cage's receipt without its launcher on the PATH: repair.
			p.State = "drifted"
		}
		return p
	}
	return nil
}

func (s *Server) agentsAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !slices.Contains(agentJobActions, action) {
		http.NotFound(w, r)
		return
	}
	var input struct {
		RemoveData bool `json:"remove_data"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.RemoveData && action != "remove" {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
		return
	}
	e, ok, err := agentLookup(r.PathValue("id"))
	if err != nil {
		http.Error(w, "无法读取其他客户端目录，请重新安装 Super Cage。", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "没有这个客户端", http.StatusNotFound)
		return
	}
	if !e.Optional() {
		http.Error(w, e.Name+" 随环境提供，不在这里添加、更新或移除。", http.StatusConflict)
		return
	}
	if _, reason := s.agentUnsupported(e, action); reason != "" {
		http.Error(w, reason, http.StatusConflict)
		return
	}
	if !s.vmRunning() {
		http.Error(w, "请先启动 Super Cage 环境", http.StatusConflict)
		return
	}
	if action == "install" {
		s.mu.Lock()
		presence := s.clients
		s.mu.Unlock()
		if presence.Known && slices.Contains(presence.Clients, e.ID) && !slices.Contains(presence.Receipts, e.ID) {
			http.Error(w, "已经安装了 "+e.Name+"（自行安装），Super Cage 不会覆盖它。", http.StatusConflict)
			return
		}
	}
	s.launch(w, "agent-"+action+":"+e.ID, func() error { return s.runAgentJob(e, agents.Action(action), input.RemoveData) })
}

func (s *Server) agentsCancel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Job string `json:"job"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	found, canceled := s.jobs.cancelJob(input.Job, func(kind string) bool { _, _, ok := agentJobKind(kind); return ok })
	switch {
	case !found:
		http.Error(w, "没有这个操作", http.StatusNotFound)
	case !canceled:
		http.Error(w, "这里只能取消其他客户端的添加、更新或移除。", http.StatusConflict)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// agentJobError is a job the installer refused or failed: its code and the
// line it wrote for the person.
type agentJobError struct{ code, message string }

func (e *agentJobError) Error() string { return e.message }

func (s *Server) runAgentJob(e agents.Entry, action agents.Action, removeData bool) error {
	s.mu.Lock()
	allowed := s.toolsNetworkAllowedLocked()
	s.mu.Unlock()
	if !allowed {
		return errors.New("工作台正在退出，请稍后重试。")
	}
	arch, ok := agents.ParseArch(agentsHostArch)
	if !ok && action != agents.Remove {
		return fmt.Errorf("这个电脑的架构（%s）没有 %s 的版本。", agentsHostArch, e.Name)
	} else if !ok {
		arch = agents.AMD64
	}
	input, err := agents.ScriptInput(e, agents.Job{Action: action, Arch: arch, RemoveData: removeData, Hidden: agentsHiddenAllowed()})
	if err != nil {
		return err
	}
	job := s.jobs.execution()
	job.setSetupProgress(&SetupProgress{Phase: "agent", Operation: "check", Label: agentPhaseLabels["check"]})
	// A finished job carries no progress: the window reads progress on a
	// finished job as an environment install.
	defer job.setSetupProgress(nil)
	s.setMessage(map[agents.Action]string{agents.Install: "正在添加 ", agents.Update: "正在更新 ", agents.Remove: "正在移除 "}[action] + e.Name + "，可在操作详情查看进度…")
	out := &agentOutput{id: e.ID, next: job}
	err = s.withToolsNetworkMonitor(func() error {
		return agentMaintenance(s.operationContext(), s.configPath, cage.AgentInstallScript(input), agents.ExtraInstallHosts(e), out, out)
	})
	outcome := out.outcome()
	if err == nil && !outcome.done && !outcome.removed {
		err = errors.New("客户端的安装程序没有报告结果。打开“操作详情”查看记录。")
	}
	if err != nil && outcome.failure == nil && s.operationContext().Err() == nil {
		outcome.failure = &agentFailureView{Code: "failed", Message: err.Error()}
	}
	outcome.done, outcome.removed = outcome.done && err == nil, outcome.removed && err == nil
	s.agentState.record(e.ID, outcome)
	if s.operationContext().Err() == nil {
		s.refreshAgentPresence()
	}
	if canceled := s.operationContext().Err(); canceled != nil {
		return canceled
	}
	if err != nil && outcome.failure != nil && outcome.failure.Code != "failed" {
		return &agentJobError{code: outcome.failure.Code, message: outcome.failure.Message}
	}
	return err
}

// refreshAgentPresence reads presence once after a job (design 4.2 step 6).
func (s *Server) refreshAgentPresence() {
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 4*time.Second)
	defer cancel()
	out, err := agentGuestRead(s, ctx, clientPresenceProbe(clientReceiptDir))
	if p, ok := parseClientPresence(strings.TrimSpace(string(out))); err == nil && ok {
		s.recordClientPresence(p)
	}
}

// agentGuestMarker heads the verify read's second line: the free disk of the
// home and, per installed entry, the version current points at and whether
// the entry's stat key still matches its receipt's sidecar.
const agentGuestMarker = "agent-guest/1"

// agentGuestProbe is the presence line plus one line of versions, in one
// guest read. Ids, entries and paths come from the catalog, quoted; lib and
// state are /usr/local/lib/agent-clients and /var/lib/agent-clients.
func agentGuestProbe(entries []agents.Entry, arch agents.Arch, lib, state string) string {
	var b strings.Builder
	b.WriteString(clientPresenceProbe(state) + "\n")
	b.WriteString("f=$(df -Pk \"$HOME\" 2>/dev/null | awk 'NR==2 {print $4}'); printf '" + agentGuestMarker + " free_kb=%s' \"$f\"\n")
	for _, e := range entries {
		a, ok := e.Artifact(arch)
		if !e.Optional() || !ok || a.Entry == "" {
			continue
		}
		dir, sidecar := shellescape.Quote(lib+"/"+e.ID), shellescape.Quote(state+"/"+e.ID+".stat")
		b.WriteString("if v=$(readlink " + dir + "/current 2>/dev/null); then " +
			"k=$(stat -L -c %i:%s:%Y " + dir + "/current/" + shellescape.Quote(a.Entry) + " 2>/dev/null); " +
			"s=; if [ -r " + sidecar + " ]; then read -r s < " + sidecar + " || s=; fi; " +
			"if [ -n \"$k\" ] && [ \"$k\" = \"$s\" ]; then t=ok; else t=drifted; fi; " +
			"printf ' %s=%s:%s' " + shellescape.Quote(e.ID) + " \"$v\" \"$t\"; fi\n")
	}
	b.WriteString("echo\n")
	return b.String()
}

var agentGuestItem = regexp.MustCompile(`^([a-z][a-z0-9-]{1,31})=([0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(?:-[0-9A-Za-z][0-9A-Za-z.-]{0,31})?):(ok|drifted)$`)

// parseAgentGuest reads the versions line; ok is false for anything but a
// well-formed line naming catalog ids.
func parseAgentGuest(line string, entries []agents.Entry) (map[string]agentInstalled, int64, bool) {
	fields := strings.Fields(line)
	if len(line) > 4096 || len(fields) < 2 || fields[0] != agentGuestMarker {
		return nil, 0, false
	}
	free, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "free_kb="), 10, 64)
	if !strings.HasPrefix(fields[1], "free_kb=") || err != nil || free < 0 {
		return nil, 0, false
	}
	installed := map[string]agentInstalled{}
	for _, item := range fields[2:] {
		m := agentGuestItem.FindStringSubmatch(item)
		if m == nil || !slices.ContainsFunc(entries, func(e agents.Entry) bool { return e.ID == m[1] }) {
			return nil, 0, false
		}
		installed[m[1]] = agentInstalled{version: m[2], state: m[3]}
	}
	return installed, free, true
}

// readAgentGuest is the verify read: presence (recorded as after a job) and
// the versions line. A failed or malformed read changes nothing.
func (s *Server) readAgentGuest(ctx context.Context, entries []agents.Entry) {
	arch, ok := agents.ParseArch(agentsHostArch)
	if !ok {
		return
	}
	out, err := agentGuestRead(s, ctx, agentGuestProbe(entries, arch, "/usr/local/lib/agent-clients", clientReceiptDir))
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return
	}
	if p, ok := parseClientPresence(strings.TrimSpace(lines[0])); ok {
		s.recordClientPresence(p)
	}
	if installed, free, ok := parseAgentGuest(strings.TrimSpace(lines[1]), entries); ok {
		s.agentState.mu.Lock()
		s.agentState.guest, s.agentState.freeKB, s.agentState.read = installed, free, true
		s.agentState.mu.Unlock()
	}
}

// agentPhaseLabels are the progress labels of CAGE_AGENT_PROGRESS phases.
var agentPhaseLabels = map[string]string{
	"check": "检查环境", "download": "下载", "digest": "校验", "unpack": "解包", "verify": "验证", "done": "完成",
}

var (
	agentFailureCode = regexp.MustCompile(`^[a-z][a-z-]{1,31}$`)
	agentVersionWord = regexp.MustCompile(`^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(-[0-9A-Za-z][0-9A-Za-z.-]{0,31})?$`)
)

// parseAgentProgress reads the installer's own lines (docs/agent-install.md):
// progress, done, removed and failed. Anything else is not progress.
func parseAgentProgress(line string) *SetupProgress {
	fields := strings.Fields(line)
	switch {
	case len(fields) == 4 && fields[0] == "CAGE_AGENT_PROGRESS":
		label := agentPhaseLabels[fields[1]]
		received, err := strconv.ParseInt(fields[2], 10, 64)
		total, te := strconv.ParseInt(fields[3], 10, 64)
		if label == "" || err != nil || te != nil || received < 0 || total < 0 || total > 0 && received > total {
			return nil
		}
		p := &SetupProgress{Phase: "agent", Operation: fields[1], Label: label}
		if total > 0 {
			p.Received, p.Total = &received, total
		}
		return p
	case len(fields) == 3 && fields[0] == "CAGE_AGENT_DONE" && clientIDPattern.MatchString(fields[1]) && agentVersionWord.MatchString(fields[2]):
		return &SetupProgress{Phase: "agent", Operation: "done", Label: "完成"}
	case len(fields) == 2 && fields[0] == "CAGE_AGENT_REMOVED" && clientIDPattern.MatchString(fields[1]):
		return &SetupProgress{Phase: "agent", Operation: "done", Label: "完成"}
	case len(fields) == 2 && fields[0] == "CAGE_AGENT_FAILED" && agentFailureCode.MatchString(fields[1]):
		return &SetupProgress{Phase: "agent", Operation: "failed", Label: "未完成"}
	}
	return nil
}

// agentOutput passes the job's output on unchanged and keeps the result
// lines: DONE and REMOVED for this entry, FAILED with its code and the one
// line for the person after it.
type agentOutput struct {
	id      string
	next    io.Writer
	mu      sync.Mutex
	partial []byte
	result  agentOutcome
	failing bool
}

const agentMessageLimit = 300

func (o *agentOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	o.partial = append(o.partial, data...)
	for {
		end := bytes.IndexByte(o.partial, '\n')
		if end < 0 {
			break
		}
		o.line(string(o.partial[:end]))
		o.partial = o.partial[end+1:]
	}
	if len(o.partial) > 64<<10 {
		o.partial = o.partial[:0]
	}
	o.mu.Unlock()
	return o.next.Write(data)
}

func (o *agentOutput) line(line string) {
	line = strings.TrimRight(line, "\r")
	if o.failing {
		o.failing = false
		o.result.failure.Message = agentPlainLine(line)
		return
	}
	fields := strings.Fields(line)
	switch {
	case len(fields) == 3 && fields[0] == "CAGE_AGENT_DONE" && fields[1] == o.id && agentVersionWord.MatchString(fields[2]):
		o.result.done, o.result.version = true, fields[2]
	case len(fields) == 2 && fields[0] == "CAGE_AGENT_REMOVED" && fields[1] == o.id:
		o.result.removed = true
	case len(fields) == 2 && fields[0] == "CAGE_AGENT_FAILED" && agentFailureCode.MatchString(fields[1]):
		o.result.failure = &agentFailureView{Code: fields[1], Message: "安装程序没有说明原因。打开“操作详情”查看记录。"}
		o.failing = true
	}
}

func (o *agentOutput) outcome() agentOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := o.result
	if result.failure != nil {
		failure := *result.failure
		result.failure = &failure
	}
	return result
}

// agentPlainLine bounds the person's line and drops control and format
// characters: it is shown as the job's error.
func agentPlainLine(line string) string {
	line = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, line)
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > agentMessageLimit {
		line = string([]rune(line)[:agentMessageLimit]) + "…"
	}
	if line == "" {
		return "安装程序没有说明原因。打开“操作详情”查看记录。"
	}
	return line
}
