package workbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agents"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/version"
)

// agentClientServer is a test server whose environment runs (a live marker
// of this test process), whose maintenance channel and guest reads are
// stand-ins, and whose hidden entries are shown as in a developer build.
func agentClientServer(t *testing.T, hidden bool, maintenance func(script string, out io.Writer) error, presence string) *Server {
	t.Helper()
	s := testServer(t)
	marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if !s.vmRunning() {
		t.Fatal("the running-environment fixture is not seen")
	}
	s.clientsTracked = true
	restore := []func(){}
	oldHidden, oldMaintenance, oldRead, oldArch := agentsHiddenAllowed, agentMaintenance, agentGuestRead, agentsHostArch
	t.Cleanup(func() {
		agentsHiddenAllowed, agentMaintenance, agentGuestRead, agentsHostArch = oldHidden, oldMaintenance, oldRead, oldArch
		for _, f := range restore {
			f()
		}
	})
	agentsHiddenAllowed = func() bool { return hidden }
	agentsHostArch = "arm64"
	agentMaintenance = func(ctx context.Context, configPath, script string, hosts []string, stdout, stderr io.Writer) error {
		if len(hosts) != 0 {
			t.Errorf("codex needs no host beyond the shared list: %v", hosts)
		}
		return maintenance(script, stdout)
	}
	agentGuestRead = func(_ *Server, ctx context.Context, script string) ([]byte, error) {
		if !strings.Contains(script, clientPresenceMarker) {
			t.Errorf("an unexpected guest read: %s", script)
		}
		return []byte(presence), nil
	}
	return s
}

func waitAgentJob(t *testing.T, s *Server) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if job := s.jobs.snapshot(); !job.Running {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the job did not end")
	return Job{}
}

type agentsReply struct {
	Entries     []agentView          `json:"agents"`
	Environment agentEnvironmentView `json:"environment"`
}

func agentsGet(t *testing.T, s *Server, query string) agentsReply {
	t.Helper()
	w := request(s, "GET", "/api/agents"+query, "", "", true)
	if w.Code != 200 {
		t.Fatalf("GET /api/agents%s: %d %s", query, w.Code, w.Body)
	}
	var reply agentsReply
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func agentEntryView(t *testing.T, reply agentsReply, id string) agentView {
	t.Helper()
	for _, v := range reply.Entries {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no %s in %+v", id, reply.Entries)
	return agentView{}
}

func TestAgentClientRoutesBoundary(t *testing.T) {
	s := agentClientServer(t, false, func(string, io.Writer) error { t.Error("a job ran"); return nil }, "")
	for _, path := range []string{"/api/agents/codex/install", "/api/agents/codex/update", "/api/agents/codex/remove", "/api/agents/cancel"} {
		if w := request(s, "POST", path, s.origin, "{}", false); w.Code != 401 {
			t.Fatalf("%s without session: %d", path, w.Code)
		}
		for _, origin := range []string{"", "https://attacker.example", "http://127.0.0.1:18080"} {
			if w := request(s, "POST", path, origin, "{}", true); w.Code != 403 {
				t.Fatalf("%s from %q: %d", path, origin, w.Code)
			}
		}
	}
	// A release build: hidden entries are unknown, in the list and at the route.
	reply := agentsGet(t, s, "")
	for _, v := range reply.Entries {
		if v.Status == "hidden" {
			t.Fatalf("a release build lists %s", v.ID)
		}
	}
	if len(reply.Entries) != 1 || reply.Entries[0].ID != "claude-code" || reply.Entries[0].Present != nil || !reply.Environment.Running {
		t.Fatalf("%+v", reply)
	}
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/api/agents/codex/install", "{}", 404},
		{"/api/agents/antigravity-cli/install", "{}", 404},
		{"/api/agents/nope/install", "{}", 404},
		{"/api/agents/codex/repair", "{}", 404},
		{"/api/agents/claude-code/remove", "{}", 409},
		{"/api/agents/codex/install", `{"command":"touch /tmp/x"}`, 400},
		{"/api/agents/codex/install", `{"remove_data":true}`, 400},
		{"/api/agents/cancel", `{"job":"nope"}`, 404},
		{"/api/agents/cancel", `{"job":"x","kind":"stop"}`, 400},
	} {
		if w := request(s, "POST", tc.path, s.origin, tc.body, true); w.Code != tc.code {
			t.Errorf("%s %s: %d %s, want %d", tc.path, tc.body, w.Code, w.Body, tc.code)
		}
	}
	if w := request(s, "GET", "/api/agents/codex/install", "", "", true); w.Code < 400 {
		t.Fatalf("an action reachable by GET: %d", w.Code)
	}
	s.account.lock()
	for _, path := range []string{"/api/agents", "/api/agents/codex/install"} {
		method := map[bool]string{true: "GET", false: "POST"}[path == "/api/agents"]
		if w := request(s, method, path, s.origin, "{}", true); w.Code != 423 {
			t.Fatalf("locked %s: %d", path, w.Code)
		}
	}
}

// Admission: what the host knows is refused before any job, with its code
// in the list; a running job keeps the single slot.
func TestAgentClientAdmission(t *testing.T) {
	s := agentClientServer(t, true, func(string, io.Writer) error { t.Error("a job ran"); return nil }, "")
	reply := agentsGet(t, s, "")
	agy := agentEntryView(t, reply, "antigravity-cli")
	if agy.Supported || agy.UnsupportedCode != "keyring" || agy.Operation != "" || agy.Present != nil {
		t.Fatalf("antigravity-cli: %+v", agy)
	}
	codex := agentEntryView(t, reply, "codex")
	if !codex.Supported || codex.PinnedVersion != "0.171.0" || codex.DownloadMB != 119 || codex.DiskMB != 295 || len(codex.DataDirs) != 3 || !codex.Allowed {
		t.Fatalf("codex: %+v", codex)
	}
	if reply.Environment.ClientsKnown || reply.Environment.Arch != "arm64" || reply.Environment.GuestSystem != "linux" || reply.Environment.DiskFreeMB != nil {
		t.Fatalf("environment: %+v", reply.Environment)
	}
	if w := request(s, "POST", "/api/agents/antigravity-cli/install", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "密钥环") {
		t.Fatalf("keyring: %d %s", w.Code, w.Body)
	}
	agentsHostArch = "riscv64"
	if w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "riscv64") {
		t.Fatalf("architecture: %d %s", w.Code, w.Body)
	}
	if v := agentEntryView(t, agentsGet(t, s, ""), "codex"); v.Supported || v.UnsupportedCode != "architecture" {
		t.Fatalf("architecture in the list: %+v", v)
	}
	agentsHostArch = "arm64"
	// The person's own Codex (found without a receipt) is not overwritten.
	s.recordClientPresence(clientPresence{Known: true, Clients: []string{"codex"}, Receipts: []string{}})
	if w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "自行安装") {
		t.Fatalf("own install: %d %s", w.Code, w.Body)
	}
	if v := agentEntryView(t, agentsGet(t, s, ""), "codex"); v.Present == nil || v.Present.By != "own" {
		t.Fatalf("own install in the list: %+v", v.Present)
	}
	s.recordClientPresence(clientPresence{Known: true, Clients: []string{}, Receipts: []string{}})
	s.setJobForTest(Job{ID: "other", Kind: "environment-tools", Running: true})
	if w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "另一个操作") {
		t.Fatalf("busy: %d %s", w.Code, w.Body)
	}
	if w := request(s, "POST", "/api/agents/cancel", s.origin, `{"job":"other"}`, true); w.Code != 409 {
		t.Fatalf("canceled a job that is not an agent job: %d", w.Code)
	}
	s.setJobForTest(Job{ID: "agent", Kind: "agent-update:codex", Running: true})
	if v := agentEntryView(t, agentsGet(t, s, ""), "codex"); v.Operation != "update" {
		t.Fatalf("operation: %+v", v)
	}
	s.setJobForTest(Job{})
	codexEntry, _, _ := agents.Lookup("codex")
	s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"}
	if code, reason := s.agentUnsupported(codexEntry, "remove"); code != "guest_system" || !strings.Contains(reason, "macOS") {
		t.Fatalf("macOS guest: %s %s", code, reason)
	}
	s.macosGuest = nil
	s.windowsGuest = &guestos.WindowsProfile{}
	if code, _ := s.agentUnsupported(codexEntry, "install"); code != "guest_system" {
		t.Fatalf("Windows guest: %s", code)
	}
	s.windowsGuest = nil
}

// A job runs the installer through the maintenance channel; its lines
// become progress, its result the job's state and a fresh presence read.
func TestAgentClientJobRunsTheInstaller(t *testing.T) {
	var script string
	var s *Server
	var mid *SetupProgress
	s = agentClientServer(t, true, func(sc string, out io.Writer) error {
		script = sc
		_, _ = io.WriteString(out, "CAGE_AGENT_PROGRESS check 0 0\nCAGE_AGENT_PROGRESS download 0 124436451\nCAGE_AGENT_PROGRESS download 62218225 124436451\n正在下载 Codex 0.171.0（约 119 MB）…\n")
		mid = s.jobs.snapshot().SetupProgress
		_, _ = io.WriteString(out, "CAGE_AGENT_PROGRESS done 0 0\nCAGE_AGENT_DONE codex 0.171.0\n")
		return nil
	}, "agent-clients/1 present=codex receipts=codex\n")
	w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", true)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var started Job
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	if started.Kind != "agent-install:codex" || !started.Running {
		t.Fatalf("%+v", started)
	}
	job := waitAgentJob(t, s)
	if job.Error != "" || job.Message != "已完成" || job.SetupProgress != nil {
		t.Fatalf("job: %+v %+v", job, job.SetupProgress)
	}
	if mid == nil || mid.Phase != "agent" || mid.Operation != "download" || mid.Label != "下载" || mid.Received == nil || *mid.Received != 62218225 || mid.Total != 124436451 {
		t.Fatalf("progress during the job: %+v", mid)
	}
	for _, want := range []string{"AGENT_ACTION=install\n", "AGENT_ID=codex\n", "cage_download_tool", "super-cage-agent-launcher"} {
		if !strings.Contains(script, want) {
			t.Errorf("the job's script lacks %q", want)
		}
	}
	if strings.Contains(job.Log, "CAGE_AGENT_PROGRESS") || !strings.Contains(job.Log, "正在下载 Codex") {
		t.Fatalf("log: %q", job.Log)
	}
	v := agentEntryView(t, agentsGet(t, s, ""), "codex")
	if v.Present == nil || v.Present.By != "cage" || v.Present.Version != "0.171.0" || v.Present.State != "ok" || v.UpdateAvailable || v.LastFailure != nil {
		t.Fatalf("after install: %+v %+v", v, v.Present)
	}
	// Remove with data: the input names the catalog's folders, never the caller's.
	w = request(s, "POST", "/api/agents/codex/remove", s.origin, `{"remove_data":true}`, true)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	waitAgentJob(t, s)
	if !strings.Contains(script, "AGENT_ACTION=remove\n") || !strings.Contains(script, "AGENT_REMOVE_DATA='~/.codex\n") {
		t.Fatalf("remove input:\n%s", script[:min(len(script), 2000)])
	}
}

func TestAgentClientJobFailureIsTheInstallersLine(t *testing.T) {
	s := agentClientServer(t, true, func(_ string, out io.Writer) error {
		_, _ = io.WriteString(out, "CAGE_AGENT_PROGRESS check 0 0\nCAGE_AGENT_FAILED disk\n客机磁盘空间不足：需要 400 MB，当前只有 100 MB。\u202e\n")
		return errors.New("exit status 1")
	}, "agent-clients/1 present= receipts=\n")
	if w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", true); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	job := waitAgentJob(t, s)
	if !strings.Contains(job.Error, "客机磁盘空间不足：需要 400 MB，当前只有 100 MB。") || strings.Contains(job.Error, "\u202e") || job.Message != "添加 Codex失败" {
		t.Fatalf("job: %+v", job)
	}
	v := agentEntryView(t, agentsGet(t, s, ""), "codex")
	if v.LastFailure == nil || v.LastFailure.Code != "disk" || v.Present != nil {
		t.Fatalf("after failure: %+v", v)
	}
	// Exit 0 without a result line is a failure too.
	agentMaintenance = func(context.Context, string, string, []string, io.Writer, io.Writer) error { return nil }
	request(s, "POST", "/api/agents/codex/update", s.origin, "{}", true)
	if job := waitAgentJob(t, s); job.Error == "" {
		t.Fatalf("a job without a result line succeeded: %+v", job)
	}
}

// Cancel reaches only an agent job, and the job ends canceled.
func TestAgentClientCancel(t *testing.T) {
	started := make(chan struct{})
	s := agentClientServer(t, true, nil, "")
	agentMaintenance = func(ctx context.Context, _ string, _ string, _ []string, out io.Writer, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		_, _ = io.WriteString(out, "CAGE_AGENT_FAILED canceled\n已取消，已下载的部分会在下次继续使用。\n")
		return ctx.Err()
	}
	w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", true)
	var job Job
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	<-started
	if w := request(s, "POST", "/api/agents/cancel", s.origin, `{"job":"`+job.ID+`"}`, true); w.Code != 204 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if ended := waitAgentJob(t, s); !ended.Canceled {
		t.Fatalf("%+v", ended)
	}
	if w := request(s, "POST", "/api/agents/cancel", s.origin, `{"job":"`+job.ID+`"}`, true); w.Code != 204 {
		t.Fatalf("cancel of an ended job: %d", w.Code)
	}
}

// The gate lease renews during an agent job, as during 更新环境工具.
func TestAgentClientJobRenewsTheLease(t *testing.T) {
	s := testServer(t)
	for kind, blocked := range map[string]bool{
		"agent-install:codex": false, "agent-update:codex": false, "agent-remove:antigravity-cli": false,
		"agent-repair:codex": true, "agent-install:": true, "agent-install:../x": true, "agent-installcodex": true, "start": true,
	} {
		s.setJobForTest(Job{ID: "j", Kind: kind, Running: true})
		s.mu.Lock()
		got := s.networkMonitorBlockedLocked()
		s.mu.Unlock()
		if got != blocked {
			t.Errorf("%s: blocked=%v, want %v", kind, got, blocked)
		}
	}
}

func TestAgentClientJobNames(t *testing.T) {
	for kind, name := range map[string]string{
		"agent-install:codex": "添加 Codex", "agent-update:codex": "更新 Codex", "agent-remove:antigravity-cli": "移除 Antigravity CLI",
		"agent-remove:unknown-client": "移除 客户端", "agent-repair:codex": "本次操作",
	} {
		if got := jobName(kind); got != name {
			t.Errorf("%s: %q, want %q", kind, got, name)
		}
	}
}

func TestAgentClientProgressLines(t *testing.T) {
	for line, want := range map[string]string{
		"CAGE_AGENT_PROGRESS download 10 100": "download 下载 10/100",
		"CAGE_AGENT_PROGRESS verify 0 0":      "verify 验证 -",
		"CAGE_AGENT_DONE codex 0.171.0":       "done 完成 -",
		"CAGE_AGENT_REMOVED codex":            "done 完成 -",
		"CAGE_AGENT_FAILED own-install":       "failed 未完成 -",
	} {
		p := parseSetupProgress(line)
		if p == nil {
			t.Fatalf("%s: not progress", line)
		}
		got := p.Operation + " " + p.Label + " -"
		if p.Received != nil {
			got = p.Operation + " " + p.Label + " " + strconv.FormatInt(*p.Received, 10) + "/" + strconv.FormatInt(p.Total, 10)
		}
		if p.Phase != "agent" || got != want {
			t.Errorf("%s: %q (%s), want %q", line, got, p.Phase, want)
		}
	}
	for _, line := range []string{
		"CAGE_AGENT_PROGRESS download 101 100", "CAGE_AGENT_PROGRESS rm 0 0", "CAGE_AGENT_PROGRESS download -1 0", "CAGE_AGENT_PROGRESS download x 1",
		"CAGE_AGENT_DONE ../x 1.0.0", "CAGE_AGENT_DONE codex 1.0", "CAGE_AGENT_REMOVED", "CAGE_AGENT_FAILED Disk", "CAGE_AGENT_FAILED disk extra",
		"CAGE_AGENT_SOMETHING x",
	} {
		if p := parseSetupProgress(line); p != nil {
			t.Errorf("%s: %+v", line, p)
		}
	}
}

// The verify read: presence and installed versions in one guest call.
func TestAgentClientVerifyRead(t *testing.T) {
	s := agentClientServer(t, true, nil, "agent-clients/1 present=codex receipts=codex\nagent-guest/1 free_kb=4194304 codex=0.170.0:drifted\n")
	s.ready = true
	reply := agentsGet(t, s, "?verify=1")
	v := agentEntryView(t, reply, "codex")
	if v.Present == nil || v.Present.Version != "0.170.0" || v.Present.State != "drifted" || !v.UpdateAvailable {
		t.Fatalf("%+v %+v", v, v.Present)
	}
	if reply.Environment.DiskFreeMB == nil || *reply.Environment.DiskFreeMB != 4096 || !reply.Environment.ClientsKnown {
		t.Fatalf("%+v", reply.Environment)
	}
	entries, _ := agents.All()
	for _, line := range []string{
		"agent-guest/1", "agent-guest/1 free_kb=-1", "agent-guest/1 free_kb=1 nope=1.0.0:ok", "agent-guest/1 free_kb=1 codex=1.0:ok",
		"agent-guest/1 free_kb=1 codex=1.0.0:bad", "agent-guest/2 free_kb=1", "agent-guest/1 free_kb=1 codex=1.0.0:ok\nx",
	} {
		if _, _, ok := parseAgentGuest(line, entries); ok {
			t.Errorf("accepted %q", line)
		}
	}
}

// The verify probe as the guest runs it, against a tree like the
// installer's: one entry in place, one drifted, one never installed.
func TestAgentClientVerifyProbeInBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	python, perr := exec.LookPath("python3")
	if err != nil || perr != nil {
		t.Skip("bash or python3 unavailable")
	}
	root := t.TempDir()
	home, bin, lib, state := filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "lib"), filepath.Join(root, "state")
	for _, dir := range []string{filepath.Join(home, ".local/bin"), bin, filepath.Join(lib, "codex/0.171.0"), filepath.Join(lib, "antigravity-cli/1.5.14"), state} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	// GNU stat -c %i:%s:%Y, as on the guest.
	write(filepath.Join(bin, "stat"), "#!/bin/sh\n[ \"$1 $2\" = '-L -c' ] || exit 2\nexec '"+python+"' -c 'import os, sys; s = os.stat(sys.argv[1]); print(\"%d:%d:%d\" % (s.st_ino, s.st_size, int(s.st_mtime)))' \"$4\"\n", 0o755)
	codexEntry := filepath.Join(lib, "codex/0.171.0/codex-aarch64-unknown-linux-musl")
	write(codexEntry, "#!/bin/sh\n", 0o755)
	write(filepath.Join(lib, "antigravity-cli/1.5.14/antigravity"), "#!/bin/sh\n", 0o755)
	for id, version := range map[string]string{"codex": "0.171.0", "antigravity-cli": "1.5.14"} {
		if err := os.Symlink(version, filepath.Join(lib, id, "current")); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(state, "codex.stat"), statKeyOf(t, codexEntry)+"\n", 0o644)
	write(filepath.Join(state, "codex.json"), "{}", 0o644)
	write(filepath.Join(home, ".local/bin/codex"), "#!/bin/sh\n", 0o755)
	cmd := exec.Command(bash, "-c", agentGuestProbe(mustAgents(t), agents.ARM64, lib, state))
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] != "agent-clients/1 present=codex receipts=codex" {
		t.Fatalf("%q", out)
	}
	installed, free, ok := parseAgentGuest(lines[1], mustAgents(t))
	if !ok || free <= 0 || installed["codex"] != (agentInstalled{version: "0.171.0", state: "ok"}) ||
		installed["antigravity-cli"] != (agentInstalled{version: "1.5.14", state: "drifted"}) || len(installed) != 2 {
		t.Fatalf("%q: %v %d %v", lines[1], installed, free, ok)
	}
}

// statKeyOf is what the installer records for an entry: GNU stat's
// %i:%s:%Y.
func statKeyOf(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("python3", "-I", "-c", "import os, sys; s = os.stat(sys.argv[1]); print('%d:%d:%d' % (s.st_ino, s.st_size, int(s.st_mtime)))", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// Hidden entries need both a development version and the switch; a
// release build never lists or installs them.
func TestAgentClientHiddenSwitch(t *testing.T) {
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	for _, c := range []struct {
		version, env string
		want         bool
	}{
		{"0.1.15", "1", false},
		{"0.1.16-dev.agents", "", false},
		{"0.1.16-dev.agents", "0", false},
		{"0.1.16-dev.agents", "1", true},
	} {
		version.Version = c.version
		t.Setenv("CAGE_AGENTS_HIDDEN", c.env)
		if got := agentsHiddenSwitch(); got != c.want {
			t.Errorf("%s with %q: %v", c.version, c.env, got)
		}
	}
}

// Neither a paired phone's room nor any /api/mobile route reaches these
// routes (design S12): a phone has no session cookie for them.
func TestAgentClientRoutesAreNotPhoneRoutes(t *testing.T) {
	for _, path := range []string{"/api/agents", "/api/agents/codex/install", "/api/agents/cancel"} {
		if phoneRoomRoute(path) {
			t.Errorf("%s is a phone room route", path)
		}
	}
	s := testServer(t)
	if w := request(s, "POST", "/api/agents/codex/install", s.origin, "{}", false); w.Code != 401 {
		t.Fatalf("without the window's session: %d", w.Code)
	}
}

// The per-job hosts reach only that job's channel, CONNECT 443 only.
func TestAgentClientJobHostsStayWithTheirJob(t *testing.T) {
	dialed := []string{}
	proxy := func(hosts []string) *maintenanceProxy {
		return &maintenanceProxy{token: "t", ctx: context.Background(), hosts: hosts, connections: map[net.Conn]bool{},
			dial: func(_ context.Context, _, address string) (net.Conn, error) {
				dialed = append(dialed, address)
				return nil, errors.New("no network in tests")
			}}
	}
	connect := func(p *maintenanceProxy, host string) int {
		r := httptest.NewRequest(http.MethodConnect, "http://"+host, nil)
		r.Host = host
		r.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("cage-install:t")))
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w.Code
	}
	if code := connect(proxy([]string{"downloads.vendor.example"}), "downloads.vendor.example:443"); code != 502 {
		t.Fatalf("the job's own host was refused: %d", code)
	}
	if code := connect(proxy(nil), "downloads.vendor.example:443"); code != 403 {
		t.Fatalf("another job's host was reached: %d", code)
	}
	if code := connect(proxy([]string{"downloads.vendor.example"}), "downloads.vendor.example:80"); code != 403 {
		t.Fatalf("a job host on port 80: %d", code)
	}
	if len(dialed) != 1 {
		t.Fatalf("dialed %v", dialed)
	}
	for _, e := range mustAgents(t) {
		if hosts := agents.ExtraInstallHosts(e); len(hosts) != 0 {
			t.Fatalf("%s needs hosts beyond today's channel: %v (a security review)", e.ID, hosts)
		}
	}
}

func mustAgents(t *testing.T) []agents.Entry {
	t.Helper()
	entries, err := agents.All()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A diagnostic bundle reads nothing of a client's data (design 6, S13): only
// Cage's journal namespace, and the job channel's credential is redacted.
func TestAgentClientDataStaysOutOfDiagnostics(t *testing.T) {
	if !strings.Contains(diagnosticGuestJournal, "'--namespace=cage'") {
		t.Fatal("the guest collection is no longer Cage's journal only")
	}
	for _, e := range mustAgents(t) {
		for _, d := range e.DataDirs {
			if strings.Contains(diagnosticGuestJournal, strings.TrimPrefix(d.Path, "~/")) {
				t.Errorf("diagnostics read %s's data folder %s", e.ID, d.Path)
			}
		}
	}
	for _, word := range []string{"agent-clients", "HOME", "~", "/home/"} {
		if strings.Contains(diagnosticGuestJournal, word) {
			t.Errorf("the guest collection names %q", word)
		}
	}
	line := "curl: (56) CONNECT tunnel failed via http://cage-install:0123456789abcdef0123456789abcdef@127.0.0.1:18961"
	if got := diagnostics.Redact(line); strings.Contains(got, "0123456789abcdef0123456789abcdef") {
		t.Fatalf("diagnostics keep the channel credential: %s", got)
	}
}
