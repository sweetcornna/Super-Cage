//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/localorca"
)

const (
	crashProfileEnv = "CAGE_TEST_CRASH_PROFILE"
	crashPassword   = "Fixture-only password 2026!"
)

type crashCommand struct {
	Method string
	Path   string
	Body   string
}

type crashReply struct {
	Code int
	Body string
}

// The helper owns one controller lifetime. Its synthetic guest lives in the
// parent, which can independently change what the next controller observes.
func TestWorkbenchCrashRecoveryChild(t *testing.T) {
	profile := os.Getenv(crashProfileEnv)
	if profile == "" {
		t.Skip("subprocess fixture")
	}
	account, err := loadLocalAccount(profile)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(profile, "config.json")
	var preferences Preferences
	data, err := os.ReadFile(filepath.Join(profile, "workbench.json"))
	if err != nil || json.Unmarshal(data, &preferences) != nil {
		t.Fatalf("read fixture preferences: %v", err)
	}
	s := &Server{profile: profile, configPath: config, account: account,
		origin: "http://localhost:17777", token: randomToken(), session: randomToken(),
		proofKey: randomToken(), preferences: preferences, keepAwake: fakeKeepAwake, networkPaused: true}
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/homepage" {
			return nil, fmt.Errorf("unexpected browser fixture request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	}))
	s.orca.localDetect = func() localorca.Installation { return localorca.Installation{} }
	local := &crashLocalClient{path: filepath.Join(filepath.Dir(profile), "external-client.json")}
	if _, err := os.Stat(local.path); err == nil {
		install := localorca.Installation{Installed: true, CLIPath: "synthetic-orca"}
		s.orca.localDetect = func() localorca.Installation { return install }
		s.orca.localClient = &localorca.Client{Install: install, Run: local.run}
		openExternalDeepSeek = local.openDeepSeek
	}
	guest := &crashOrcaGuest{path: filepath.Join(filepath.Dir(profile), "orca-guest.json")}
	s.orca.restorePortProbe = guest.portFree
	s.agentCLIs.runtimeHook = guest.call

	// attach() still goes through the real SSH client, connection validation
	// and ready transition. Its fixture snapshot resides entirely in the
	// synthetic guest home.
	guestConnectionSnapshot = `cat "$HOME/connection.json"`
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.monitorWorkbenchRuntime(ctx)

	input, output := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var command crashCommand
		if err := input.Decode(&command); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		var reply crashReply
		switch command.Method {
		case "attach":
			reply = crashError(s.attach())
		case "settled":
			reply = crashError(waitCrashRecovery(ctx, s))
		default:
			r := httptest.NewRequest(command.Method, s.origin+command.Path, strings.NewReader(command.Body))
			r.Header.Set("Origin", s.origin)
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
			if command.Path == externalSessionRoute {
				r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
			} else {
				// A remote Workbench may start a managed service but cannot
				// open a desktop app or create its local forwarding tunnel.
				r.Header.Set("X-Cage-Remote-Client", "1")
			}
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, r)
			reply = crashReply{Code: w.Code, Body: w.Body.String()}
		}
		if err := output.Encode(reply); err != nil {
			t.Fatal(err)
		}
	}
}

func crashError(err error) crashReply {
	if err != nil {
		return crashReply{Code: http.StatusInternalServerError, Body: err.Error()}
	}
	return crashReply{Code: http.StatusOK}
}

func waitCrashRecovery(ctx context.Context, s *Server) error {
	ctx, cancel := context.WithTimeout(ctx, 22*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		settled := s.runnableRuntimeRestoreLocked() == 0 && s.runtimeRestore.active == 0
		s.mu.Unlock()
		if settled {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type crashController struct {
	cmd    *exec.Cmd
	input  *json.Encoder
	output *json.Decoder
	stderr bytes.Buffer
	waited bool
}

func startCrashController(t *testing.T, profile string) *crashController {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	executable, err := os.Executable()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestWorkbenchCrashRecoveryChild$", "-test.timeout=65s")
	cmd.Env = append(os.Environ(), crashProfileEnv+"="+profile)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	child := &crashController{cmd: cmd, input: json.NewEncoder(input), output: json.NewDecoder(output)}
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if !child.waited {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
		}
		if t.Failed() && child.stderr.Len() > 0 {
			t.Logf("controller %d stderr: %s", child.cmd.Process.Pid, child.stderr.String())
		}
	})
	return child
}

func (c *crashController) run(t *testing.T, method, path, body string, want int) string {
	t.Helper()
	if err := c.input.Encode(crashCommand{Method: method, Path: path, Body: body}); err != nil {
		t.Fatalf("send %s %s: %v", method, path, err)
	}
	var reply crashReply
	if err := c.output.Decode(&reply); err != nil {
		t.Fatalf("receive %s %s: %v", method, path, err)
	}
	if reply.Code != want {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, reply.Code, want, reply.Body)
	}
	return reply.Body
}

func (c *crashController) kill(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err := c.cmd.Wait()
	c.waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("controller %d did not die by SIGKILL: %v", c.cmd.Process.Pid, err)
	}
	if bytes.Contains(c.stderr.Bytes(), []byte("WARNING: DATA RACE")) {
		t.Fatalf("controller %d reported a race before its SIGKILL", c.cmd.Process.Pid)
	}
}

func (c *crashController) createAccount(t *testing.T) {
	t.Helper()
	c.run(t, "POST", "/api/account/create", fmt.Sprintf(`{"username":"crash_fixture","password":%q,"confirm_password":%q}`, crashPassword, crashPassword), http.StatusOK)
}

func (c *crashController) unlock(t *testing.T) {
	t.Helper()
	c.run(t, "POST", "/api/account/unlock", fmt.Sprintf(`{"password":%q}`, crashPassword), http.StatusOK)
}

type crashOrcaState struct {
	Status    orcaGuestStatus
	Starts    int
	Stops     int
	PortBusy  bool
	Workspace string
}

// A separate disk file stands in for the guest runtime. It is never used by
// production recovery to infer intent, and can disagree with the controller.
type crashOrcaGuest struct {
	mu   sync.Mutex
	path string
}

func (g *crashOrcaGuest) read() (crashOrcaState, error) {
	var state crashOrcaState
	data, err := os.ReadFile(g.path)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func (g *crashOrcaGuest) portFree(context.Context, string, int) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, err := g.read()
	return !state.PortBusy, err
}

func (g *crashOrcaGuest) call(_ context.Context, operation string, input, output any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch operation {
	case "orca-status", "orca-start", "orca-restart", "orca-stop":
	default:
		return nil // Account publication and unrelated integrations have no guest effects.
	}
	state, err := g.read()
	if err != nil {
		return err
	}
	switch operation {
	case "orca-status":
		*output.(*orcaGuestStatus) = state.Status
		return nil
	case "orca-start", "orca-restart":
		state.Starts++
		payload := input.(map[string]any)
		state.Workspace = payload["workspace"].(string)
		state.Status = orcaGuestStatus{Installed: true, Port: payload["port"].(int),
			ConfigKey: payload["config_key"].(string), Accounts: orcaFixedAccounts(payload["bindings"].(map[string]any))}
		state.Status.Running, state.Status.PID = true, 200+state.Starts
		*output.(*orcaGuestStatus) = state.Status
	case "orca-stop":
		state.Stops++
		state.Status.Running, state.Status.PID = false, 0
	}
	return writePrivate(g.path, state)
}

type crashFixture struct {
	profile string
	project string
	home    string
	orca    *crashOrcaGuest
}

func newCrashFixture(t *testing.T) crashFixture {
	t.Helper()
	s := transferMountServer(t, false, "current")
	home := t.TempDir()
	f := crashFixture{profile: s.profile, project: s.environmentDir(), home: home,
		orca: &crashOrcaGuest{path: filepath.Join(filepath.Dir(s.profile), "orca-guest.json")}}
	// The socket bound to loopback accepts only the generated SSH identity.
	mobileTestSSHRewrite(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"}, f.guestCommand)
	f.writeFile(t, filepath.Join(s.profile, "workbench.json"), Preferences{Workspace: f.project})
	// An applied proxy would give recovery a legitimate network operation.
	// This fixture only tests local restoration.
	f.writeFile(t, s.configPath, cage.DefaultConfig())
	f.writeFile(t, f.orca.path, crashOrcaState{})
	if err := os.WriteFile(filepath.Join(home, "connection.json"), []byte(makeValidGuestConnectionPayload(t)), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f crashFixture) writeFile(t *testing.T, path string, value any) {
	t.Helper()
	if err := writePrivate(path, value); err != nil {
		t.Fatal(err)
	}
}

func (f crashFixture) guestCommand(command string) string {
	switch {
	case strings.Contains(command, "CAGE-GUEST-SESSION"):
		// Fall back to dedicated SSH. The dedicated fixture admits only the
		// known snapshot and Codex helper operations below.
		return "exit 86"
	case strings.Contains(command, "connection.json"):
		return `cat "$HOME/connection.json"`
	case strings.Contains(command, "codex-session-code"):
		// Translate the leading guest project path on PTY creation only. Keep
		// the guest-relative cwd inside the resume payload for assertions.
		return strings.Replace(command, "cd "+guestPath(f.project)+" ", "cd "+f.project+" ", 1)
	default:
		return "exit 78"
	}
}

func TestWorkbenchCrashRecoveryOrca(t *testing.T) {
	f := newCrashFixture(t)
	accountID := strings.Repeat("a", 24)
	f.writeFile(t, filepath.Join(f.profile, "agent-accounts.json"), map[string]*agentAccountPool{
		"opencode": {Accounts: []agentAccount{{ID: accountID, Source: "managed", AuthMode: "oauth", Enabled: true, Status: "ready"}}},
	})
	state := func() crashOrcaState {
		t.Helper()
		got, err := f.orca.read()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	owner := startCrashController(t, f.profile)
	owner.createAccount(t)
	owner.run(t, "attach", "", "", http.StatusOK)
	owner.run(t, "settled", "", "", http.StatusOK)
	owner.run(t, "POST", "/api/orca/config", fmt.Sprintf(`{"enabled":true,"bind_mode":"loopback","auto_sync_projects":false,"account_bindings":{"opencode":%q}}`, accountID), http.StatusOK)
	owner.run(t, "POST", "/api/orca/start", "{}", http.StatusOK)
	if got := state(); !got.Status.Running || got.Starts != 1 || got.Workspace != guestPath(f.project) || got.Status.Accounts["opencode"] != accountID {
		t.Fatalf("normal Orca start did not establish the fixture: %+v", got)
	}
	owner.kill(t)

	// Externally, the guest service may have died too. Simulate an independent
	// listener so restoration cannot assume ownership of a busy port.
	external := state()
	external.Status = orcaGuestStatus{Installed: true}
	external.PortBusy = true
	f.writeFile(t, f.orca.path, external)
	reopened := startCrashController(t, f.profile)
	reopened.run(t, "attach", "", "", http.StatusOK)
	reopened.run(t, "GET", "/api/orca", "", http.StatusLocked)
	reopened.unlock(t)
	reopened.run(t, "settled", "", "", http.StatusOK)
	if got := state(); got.Starts != 1 || got.Status.Running {
		t.Fatalf("recovery started on a port owned by another service: %+v", got)
	}
	reopened.kill(t)

	free := state()
	free.PortBusy = false
	f.writeFile(t, f.orca.path, free)
	resumed := startCrashController(t, f.profile)
	resumed.unlock(t)
	if got := state(); got.Starts != 1 {
		t.Fatalf("unlock started Orca before environment readiness: %+v", got)
	}
	resumed.run(t, "attach", "", "", http.StatusOK)
	resumed.run(t, "settled", "", "", http.StatusOK)
	if got := state(); !got.Status.Running || got.Starts != 2 || got.Workspace != guestPath(f.project) || got.Status.Accounts["opencode"] != accountID {
		t.Fatalf("ready lifecycle did not restore the recorded Orca service: %+v", got)
	}
	resumed.kill(t)

	live := startCrashController(t, f.profile)
	live.run(t, "attach", "", "", http.StatusOK)
	live.unlock(t)
	live.run(t, "settled", "", "", http.StatusOK)
	if got := state(); got.Starts != 2 || !got.Status.Running {
		t.Fatalf("live guest process was restarted after controller crash: %+v", got)
	}
	live.run(t, "POST", "/api/orca/stop", "{}", http.StatusOK)
	if got := state(); got.Stops != 1 || got.Status.Running {
		t.Fatalf("normal stop did not stop the fixture: %+v", got)
	}
	live.kill(t)

	stopped := startCrashController(t, f.profile)
	stopped.run(t, "attach", "", "", http.StatusOK)
	stopped.unlock(t)
	stopped.run(t, "settled", "", "", http.StatusOK)
	if got := state(); got.Starts != 2 || got.Stops != 1 || got.Status.Running {
		t.Fatalf("deliberately stopped Orca was brought back: %+v", got)
	}
	stopped.kill(t)
}

type crashCodexHistory struct {
	Thread     string `json:"thread_id"`
	History    bool   `json:"history_found"`
	Workspace  string `json:"workspace"`
	Account    string `json:"account"`
	Connection string `json:"connection"`
}

func (f crashFixture) seedCodex(t *testing.T) (map[string]crashCodexHistory, map[string]string, string) {
	t.Helper()
	first, next := strings.Repeat("a", 24), strings.Repeat("b", 24)
	roster := codexAccountList{
		Accounts:   []codexAccount{codexFixture(first, "original@example.test"), codexFixture(next, "next@example.test")},
		Scheduling: claudeScheduling{Mode: "manual", SelectedAccountID: first},
	}
	f.writeFile(t, filepath.Join(f.profile, "codex-accounts.json"), roster)
	var store mobileCLIStore
	history, ids := map[string]crashCodexHistory{}, map[string]string{}
	for _, name := range []string{"running", "stopped", "missing"} {
		thread, connection := uuid.NewString(), uuid.NewString()
		view, err := store.createProvider(f.profile, f.project, uuid.NewString(), codexProvider, func(record *mobileRecord) {
			record.CLIID, record.Connection, record.Resume = thread, connection, true
			record.CodexAccount, record.CodexSubject = first, roster.Accounts[0].subject()
			record.Terminal, record.DesktopMenu, record.GuestWorkspace = true, true, guestPath(f.project)
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = view.ID
		history[thread] = crashCodexHistory{Thread: thread, History: true, Workspace: guestPath(f.project), Account: first, Connection: connection}
	}
	f.writeFile(t, filepath.Join(f.home, "codex-history.json"), history)
	helper := filepath.Join(f.home, codexSessionsSpec.entryRelPath())
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	// The login-shell invocation and PTY are real. The helper only checks
	// synthetic IDs and holds a PTY open; it has no provider or default login.
	const fake = `import json, os, sys
from pathlib import Path
home = Path(os.environ["HOME"])
mode = sys.argv[1]
req = json.load(sys.stdin) if sys.argv[2] == "-" else json.loads(sys.argv[2])
with (home / "codex-calls.jsonl").open("a") as stream:
    stream.write(json.dumps({"mode": mode, "request": req}) + "\n")
if mode == "view":
    row = json.loads((home / "codex-history.json").read_text()).get(req.get("thread"), {})
    if any(req.get(key) != row.get(key) for key in ("connection", "account", "workspace")):
        print(json.dumps({"error": "fixture refused changed Codex identity"}))
    else:
        print(json.dumps(row))
elif mode == "tui-launch":
    print("fixture Codex PTY", flush=True)
    sys.stdin.read()
elif mode == "stop":
    print('{"ok":true}')
elif mode == "probe":
    print('{"complete":true,"resume":true}')
else:
    print('{}')
`
	if err := os.WriteFile(helper, []byte(fake), 0600); err != nil {
		t.Fatal(err)
	}
	return history, ids, next
}

func (f crashFixture) codexLaunches(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.home, "codex-calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var launches []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call codexGuestCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("decode synthetic guest call: %v", err)
		}
		if call.Mode == "tui-launch" {
			launches = append(launches, call.Request)
		}
	}
	return launches
}

func TestWorkbenchCrashRecoveryCodex(t *testing.T) {
	f := newCrashFixture(t)
	history, ids, next := f.seedCodex(t)
	path := func(name string) string { return "/api/cli/sessions/" + ids[name] + "/open" }
	open := func(c *crashController, name string) mobileSession {
		t.Helper()
		var view mobileSession
		body := c.run(t, "POST", path(name), "{}", http.StatusOK)
		if err := json.Unmarshal([]byte(body), &view); err != nil || view.DesktopTerminalID == "" || !view.Running {
			t.Fatalf("open %s did not create a terminal: %v %s", name, err, body)
		}
		return view
	}
	owner := startCrashController(t, f.profile)
	owner.createAccount(t)
	owner.run(t, "attach", "", "", http.StatusOK)
	owner.run(t, "settled", "", "", http.StatusOK)
	running := open(owner, "running")
	stopped := open(owner, "stopped")
	missing := open(owner, "missing")
	// Wait for all three actual fixture processes to reach their resume entry,
	// rather than trusting the optimistic immediate open response.
	wait := time.NewTicker(15 * time.Millisecond)
	defer wait.Stop()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	for len(f.codexLaunches(t)) < 3 {
		select {
		case <-wait.C:
		case <-deadline.C:
			t.Fatal("old PTYs did not reach the native resume boundary")
		}
	}
	owner.run(t, "DELETE", "/api/terminals/"+stopped.DesktopTerminalID, "", http.StatusOK)
	owner.run(t, "POST", "/api/codex-accounts", fmt.Sprintf(`{"action":"select","id":%q}`, next), http.StatusOK)
	owner.kill(t)

	var record mobileRecord
	data, err := os.ReadFile(mobileRecordPath(f.profile, ids["running"]))
	if err != nil || json.Unmarshal(data, &record) != nil || !record.TerminalRunning || record.TerminalID != running.DesktopTerminalID {
		t.Fatalf("running state was not durably present at crash: %v", err)
	}
	var stale mobileRecord
	data, err = os.ReadFile(mobileRecordPath(f.profile, ids["missing"]))
	if err != nil || json.Unmarshal(data, &stale) != nil || !stale.TerminalRunning || stale.TerminalID != missing.DesktopTerminalID {
		t.Fatalf("stale record was not genuinely active at crash: %v", err)
	}
	staleHistory := history[stale.CLIID]
	staleHistory.History = false
	history[stale.CLIID] = staleHistory
	f.writeFile(t, filepath.Join(f.home, "codex-history.json"), history)
	// A reopened desktop has no selected project. The earlier attach
	// authorized the original project in the persistent project list.
	f.writeFile(t, filepath.Join(f.profile, "workbench.json"), Preferences{})

	replacement := startCrashController(t, f.profile)
	replacement.run(t, "attach", "", "", http.StatusOK)
	replacement.run(t, "GET", "/api/terminals", "", http.StatusLocked)
	replacement.unlock(t)
	replacement.run(t, "settled", "", "", http.StatusOK)
	var terminals []struct {
		ID        string `json:"id"`
		Session   string `json:"cli_session_id"`
		Account   string `json:"codex_account_id"`
		Workspace string `json:"workspace"`
		Closed    bool   `json:"closed"`
	}
	body := replacement.run(t, "GET", "/api/terminals", "", http.StatusOK)
	if err := json.Unmarshal([]byte(body), &terminals); err != nil || len(terminals) != 1 {
		t.Fatalf("want just the recoverable PTY (not stopped or missing): %v %s", err, body)
	}
	got := terminals[0]
	if got.ID != running.DesktopTerminalID || got.Session != running.ID || got.Account != record.CodexAccount || got.Workspace != f.project || got.Closed {
		t.Fatalf("replacement changed the original terminal binding: %+v", got)
	}
	if another := open(replacement, "running"); another.DesktopTerminalID != got.ID {
		t.Fatalf("repeat open created another PTY: %s != %s", another.DesktopTerminalID, got.ID)
	}
	replacement.run(t, "POST", path("missing"), fmt.Sprintf(`{"recover_terminal_id":%q}`, missing.DesktopTerminalID), http.StatusConflict)
	replacement.run(t, "POST", path("stopped"), "{}", http.StatusGone)

	wait.Reset(15 * time.Millisecond)
	deadline.Reset(6 * time.Second)
	for len(f.codexLaunches(t)) < 4 {
		select {
		case <-wait.C:
		case <-deadline.C:
			t.Fatal("replacement PTY did not reach the native resume boundary")
		}
	}
	launches := f.codexLaunches(t)
	if len(launches) != 4 {
		t.Fatalf("duplicate or missing native starts: %d, want 4", len(launches))
	}
	last := launches[len(launches)-1]
	if last["thread"] != record.CLIID || last["connection"] != record.Connection || last["account"] != record.CodexAccount ||
		last["subject"] != record.CodexSubject || last["cwd"] != record.GuestWorkspace {
		t.Fatalf("native resume did not retain original thread/client/account/project: %+v", last)
	}
	if lease, _ := last["lease"].(string); lease == "" || lease == record.CodexLease {
		t.Fatalf("replacement did not claim a new guest lease: %v", last)
	}
	for _, key := range []string{"prompt", "text", "input", "command"} {
		if _, replayed := last[key]; replayed {
			t.Fatalf("recovery replayed a %s field: %+v", key, last)
		}
	}
	replacement.kill(t)
}
