package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func withScienceHostArch(t *testing.T, arch string) {
	t.Helper()
	saved := scienceHostArch
	scienceHostArch = arch
	t.Cleanup(func() { scienceHostArch = saved })
}

func decodeScienceState(t *testing.T, body []byte) claudeScienceState {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var state claudeScienceState
	if err := decoder.Decode(&state); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return state
}

// Copied from the Claude Desktop boundary: session, Origin, strict bodies,
// a running environment, and the account lock.
func TestClaudeScienceControlBoundary(t *testing.T) {
	withScienceHostArch(t, "amd64")
	s := testServer(t)
	if w := request(s, "GET", "/api/claude-science", "", "", false); w.Code != 401 {
		t.Fatalf("status without session: %d", w.Code)
	}
	if w := request(s, "GET", "/api/claude-science", "", "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "请先启动") {
		t.Fatalf("status must require a running environment: %d %s", w.Code, w.Body)
	}
	for _, action := range []string{"install", "update", "start", "stop", "open"} {
		path := "/api/claude-science/" + action
		if w := request(s, "POST", path, s.origin, "{}", false); w.Code != 401 {
			t.Fatalf("%s without session: %d", path, w.Code)
		}
		for _, origin := range []string{"", "https://claude.ai", "http://localhost:47800", "http://127.0.0.1:18080"} {
			if w := request(s, "POST", path, origin, "{}", true); w.Code != 403 {
				t.Fatalf("%s from %q: %d", path, origin, w.Code)
			}
		}
		if w := request(s, "POST", path, s.origin, `{"command":"touch /tmp/untrusted"}`, true); w.Code != 400 {
			t.Fatalf("%s accepted arbitrary input: %d", path, w.Code)
		}
		if w := request(s, "POST", path, s.origin, `{"url":"http://localhost:47800/?nonce=0123456789abcdef"}`, true); w.Code != 400 {
			t.Fatalf("%s accepted a caller's link: %d", path, w.Code)
		}
		if w := request(s, "POST", path, s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "请先启动") {
			t.Fatalf("%s must require a running environment: %d %s", path, w.Code, w.Body)
		}
	}
	if w := request(s, "POST", "/api/claude-science/install", s.origin, `{"fresh":true}`, true); w.Code != 400 {
		t.Fatalf("fresh accepted outside open: %d", w.Code)
	}
	if w := request(s, "POST", "/api/claude-science/open", s.origin, `{"fresh":true}`, true); w.Code != 409 {
		t.Fatalf("fresh open: %d", w.Code)
	}
	for _, path := range []string{"/api/claude-science/uninstall", "/api/claude-science/signin"} {
		if w := request(s, "POST", path, s.origin, "{}", true); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w := request(s, "POST", "/api/claude-science/", s.origin, "{}", true); w.Code != 404 && w.Code != 405 {
		t.Fatalf("empty action: %d", w.Code)
	}
	if w := request(s, "GET", "/api/claude-science/install", "", "", true); w.Code < 400 {
		t.Fatalf("action reachable by GET: %d", w.Code)
	}
	s.account.lock()
	if w := request(s, "GET", "/api/claude-science", "", "", true); w.Code != 423 {
		t.Fatalf("locked status: %d", w.Code)
	}
	if w := request(s, "POST", "/api/claude-science/open", s.origin, "{}", true); w.Code != 423 {
		t.Fatalf("locked open: %d", w.Code)
	}
}

func TestClaudeScienceUnsupportedHost(t *testing.T) {
	withScienceHostArch(t, "arm64")
	s := testServer(t)
	w := request(s, "GET", "/api/claude-science", "", "", true)
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	state := decodeScienceState(t, w.Body.Bytes())
	if state.Supported || state.UnsupportedCode != "architecture" || state.UnsupportedReason != scienceReasonArchitecture || !strings.Contains(state.UnsupportedReason, "arm64") || state.PinnedVersion != cage.ClaudeSciencePin().Version {
		t.Fatalf("%+v", state)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"signed_in":null`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"update_required":null`)) {
		t.Fatalf("unknown states must be explicit nulls: %s", w.Body)
	}
	for _, action := range []string{"install", "update", "start", "stop", "open"} {
		if w := request(s, "POST", "/api/claude-science/"+action, s.origin, "{}", true); w.Code != 409 || strings.TrimSpace(w.Body.String()) != scienceReasonArchitecture {
			t.Fatalf("%s on arm64: %d %s", action, w.Code, w.Body)
		}
	}
}

func TestClaudeScienceUnsupportedNativeGuests(t *testing.T) {
	withScienceHostArch(t, "amd64")
	for name, setup := range map[string]func(*Server){
		"windows": func(s *Server) { s.windowsGuest = &guestos.WindowsProfile{} },
		"macos":   func(s *Server) { s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			setup(s)
			w := httptest.NewRecorder()
			s.claudeScienceStatus(w, httptest.NewRequest("GET", s.origin+"/api/claude-science", nil))
			state := decodeScienceState(t, w.Body.Bytes())
			if w.Code != 200 || state.Supported || state.UnsupportedCode != "guest_system" || state.UnsupportedReason != scienceReasonGuestSystem {
				t.Fatalf("%d %+v", w.Code, state)
			}
			w = httptest.NewRecorder()
			r := httptest.NewRequest("POST", s.origin+"/api/claude-science/install", strings.NewReader("{}"))
			r.Header.Set("Content-Type", "application/json")
			r.SetPathValue("action", "install")
			s.claudeScienceAction(w, r)
			if w.Code != 409 || strings.TrimSpace(w.Body.String()) != scienceReasonGuestSystem {
				t.Fatalf("install on %s guest: %d %s", name, w.Code, w.Body)
			}
		})
	}
	// The status answers on native guests; the actions never reach them.
	if !windowsWorkbenchRoute("/api/claude-science") || !macOSWorkbenchRoute("/api/claude-science") {
		t.Fatal("native guests cannot read the unsupported status")
	}
	for _, path := range []string{"/api/claude-science/install", "/api/claude-science/open", "/api/claude-sciencex"} {
		if windowsWorkbenchRoute(path) || macOSWorkbenchRoute(path) {
			t.Fatalf("%s admitted on a native guest", path)
		}
	}
}

type scienceSnapshotCase struct {
	arch, unit, result, status, version, candidate, fsType, invocation string
	installed, conda, listener, bwrapOld                               bool
}

// scienceGuest replaces the guest's commands on PATH for the snapshot. It
// returns the snapshot as the guest runs it and the environment to run it in.
func scienceGuest(t *testing.T, home string, tc scienceSnapshotCase) (string, []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("snapshot executes in the Linux guest")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	bin := t.TempDir()
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "uname"), "#!/bin/sh\necho x >> \"$HOME/snapshot-runs\"\necho \"$TEST_ARCH\"\n")
	write(filepath.Join(bin, "systemctl"), "#!/bin/sh\nif [ \"$1\" = is-active ]; then echo \"$TEST_UNIT\"; exit 0; fi\nfor arg; do if [ \"$arg\" = --value ]; then echo \"$TEST_RESULT\"; exit 0; fi; done\nprintf 'ActiveState=%s\\nResult=%s\\nInvocationID=%s\\n' \"$TEST_UNIT\" \"$TEST_RESULT\" \"$TEST_INVOCATION\"\n")
	write(filepath.Join(bin, "ss"), "#!/bin/sh\nif [ -n \"$TEST_LISTENER\" ]; then echo 'LISTEN 0 511 127.0.0.1:47800 0.0.0.0:*'; fi\n")
	write(filepath.Join(bin, "apt-cache"), "#!/bin/sh\nprintf 'bubblewrap:\\n  Installed: (none)\\n  Candidate: %s\\n' \"$TEST_CANDIDATE\"\n")
	write(filepath.Join(bin, "bwrap"), "#!/bin/sh\nif [ -n \"$TEST_BWRAP_OLD\" ]; then echo 'bwrap: Unknown option --disable-userns' >&2; exit 1; fi\n")
	write(filepath.Join(bin, "stat"), "#!/bin/sh\nif [ \"$1\" = -f ]; then echo \"$TEST_FSTYPE\"; exit 0; fi\nexit 2\n")
	fsType := tc.fsType
	if fsType == "" {
		fsType = "ef53"
	}
	binary := filepath.Join(home, ".local", "bin", "claude-science")
	if _, err := os.Stat(binary); tc.installed && err != nil {
		write(binary, "#!/bin/sh\ncase \"$1\" in\n  status) echo x >> \"$HOME/status-runs\"; printf '%s' \"$TEST_STATUS\" ;;\n  --version) echo x >> \"$HOME/version-runs\"; echo \"claude-science $TEST_VERSION (release, public)\" ;;\nesac\n")
	}
	if tc.conda {
		if err := os.MkdirAll(filepath.Join(home, ".claude-science", "conda"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	bwrap := filepath.Join(bin, "missing-bwrap")
	if tc.candidate == "" {
		bwrap = filepath.Join(bin, "bwrap")
	}
	script := strings.NewReplacer("/usr/bin/bwrap", bwrap, "/lib/libc.musl-x86_64.so.1", filepath.Join(home, "no-musl")).Replace(claudeScienceSnapshot)
	listener, old := "", ""
	if tc.listener {
		listener = "1"
	}
	if tc.bwrapOld {
		old = "1"
	}
	return script, []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH"), "TEST_ARCH=" + tc.arch, "TEST_UNIT=" + tc.unit, "TEST_RESULT=" + tc.result,
		"TEST_STATUS=" + tc.status, "TEST_VERSION=" + tc.version, "TEST_CANDIDATE=" + tc.candidate, "TEST_LISTENER=" + listener, "TEST_BWRAP_OLD=" + old, "TEST_FSTYPE=" + fsType, "TEST_INVOCATION=" + tc.invocation}
}

// The snapshot runs as written, with the guest's commands replaced on PATH.
func runScienceSnapshot(t *testing.T, home string, tc scienceSnapshotCase) (claudeScienceGuest, string) {
	t.Helper()
	script, env := scienceGuest(t, home, tc)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var guest claudeScienceGuest
	if err != nil || json.Unmarshal(out, &guest) != nil {
		t.Fatalf("snapshot: %v %s", err, out)
	}
	return guest, string(out)
}

func TestClaudeScienceSnapshot(t *testing.T) {
	pinned := cage.ClaudeSciencePin().Version
	running := `{"running":true,"pid":42,"port":47800,"version":"` + pinned + `","started_at":"2026-11-20T09:30:00.000Z","health":{"uptime_s":3}}`
	for _, tc := range []struct {
		name  string
		guest scienceSnapshotCase
		check func(claudeScienceState) bool
	}{
		{"aarch64", scienceSnapshotCase{arch: "aarch64", unit: "inactive"}, func(s claudeScienceState) bool {
			return !s.Supported && s.UnsupportedCode == "architecture" && s.UnsupportedReason == scienceReasonArchitecture && !s.Installed
		}},
		{"ubuntu-22.04-bubblewrap", scienceSnapshotCase{arch: "x86_64", unit: "inactive", candidate: "0.6.1-1ubuntu0.1"}, func(s claudeScienceState) bool {
			return !s.Supported && s.UnsupportedCode == "bubblewrap" && strings.Contains(s.UnsupportedReason, "0.8")
		}},
		{"overlay-home", scienceSnapshotCase{arch: "x86_64", unit: "inactive", fsType: "794c7630", installed: true, version: "0.1.73"}, func(s claudeScienceState) bool {
			return !s.Supported && s.UnsupportedCode == "storage" && s.UnsupportedReason == scienceReasonStorage && s.Installed
		}},
		{"installed-bubblewrap-too-old", scienceSnapshotCase{arch: "x86_64", unit: "inactive", bwrapOld: true}, func(s claudeScienceState) bool {
			return !s.Supported && s.UnsupportedCode == "bubblewrap"
		}},
		{"not-installed", scienceSnapshotCase{arch: "x86_64", unit: "inactive", candidate: "0.11.0-2"}, func(s claudeScienceState) bool {
			return s.Supported && !s.Installed && s.DiskRequiredMB == 11264 && s.DiskFreeMB > 0 && !s.Running && s.FailureReason == ""
		}},
		{"running", scienceSnapshotCase{arch: "x86_64", unit: "active", installed: true, status: running, conda: true, listener: true, invocation: "run-1"}, func(s claudeScienceState) bool {
			return s.Supported && s.Installed && s.Version == pinned && !s.UpdateAvailable && s.Running && s.Ready && s.Port == 47800 &&
				s.StartedAt == "2026-11-20T09:30:00.000Z" && s.DiskRequiredMB == 1024 && s.SignedIn == nil && s.UpdateRequired == nil
		}},
		{"starting", scienceSnapshotCase{arch: "x86_64", unit: "active", installed: true, status: `{"running":false}`, version: pinned}, func(s claudeScienceState) bool {
			return s.Running && !s.Ready && s.Port == 0 && s.Version == pinned
		}},
		{"stopped-older-build", scienceSnapshotCase{arch: "x86_64", unit: "inactive", installed: true, version: "0.1.72"}, func(s claudeScienceState) bool {
			return s.Installed && s.Version == "0.1.72" && s.UpdateAvailable && !s.Running && !s.Ready
		}},
		{"oom", scienceSnapshotCase{arch: "x86_64", unit: "failed", result: "oom-kill", installed: true, version: pinned}, func(s claudeScienceState) bool {
			return s.Failed && s.FailureReason == "oom-kill"
		}},
		{"exited", scienceSnapshotCase{arch: "x86_64", unit: "failed", result: "exit-code", installed: true, version: pinned}, func(s claudeScienceState) bool {
			return s.Failed && s.FailureReason == "exit"
		}},
		{"port-taken", scienceSnapshotCase{arch: "x86_64", unit: "inactive", installed: true, version: pinned, listener: true}, func(s claudeScienceState) bool {
			return !s.Running && s.FailureReason == "port_in_use"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest, raw := runScienceSnapshot(t, t.TempDir(), tc.guest)
			state := claudeScienceFromGuest(guest, pinned)
			if state.PinnedVersion != pinned || !tc.check(state) {
				t.Fatalf("%+v from %s", state, raw)
			}
		})
	}
}

// The 170 MB binary runs for --version once per installed file.
func TestClaudeScienceSnapshotCachesVersion(t *testing.T) {
	home := t.TempDir()
	tc := scienceSnapshotCase{arch: "x86_64", unit: "inactive", installed: true, version: "0.1.72"}
	for range 3 {
		if guest, _ := runScienceSnapshot(t, home, tc); guest.Version != "0.1.72" {
			t.Fatalf("version %q", guest.Version)
		}
	}
	if runs, _ := os.ReadFile(filepath.Join(home, "version-runs")); string(runs) != "x\n" {
		t.Fatalf("--version ran %d times", strings.Count(string(runs), "x"))
	}
	// A replaced binary is read again.
	binary := filepath.Join(home, ".local", "bin", "claude-science")
	data, _ := os.ReadFile(binary)
	if err := os.WriteFile(binary, append(data, []byte("# new build\n")...), 0755); err != nil {
		t.Fatal(err)
	}
	tc.version = "0.1.73"
	if guest, _ := runScienceSnapshot(t, home, tc); guest.Version != "0.1.73" {
		t.Fatalf("stale version %q", guest.Version)
	}
}

// Polls do not run the 170 MB binary: the unit state comes from systemctl,
// the daemon's port from ss, and its status is read once per run of the unit.
func TestClaudeScienceSnapshotReadsTheDaemonOncePerRun(t *testing.T) {
	pinned := cage.ClaudeSciencePin().Version
	home := t.TempDir()
	tc := scienceSnapshotCase{arch: "x86_64", unit: "active", installed: true, listener: true, invocation: "run-1",
		status: `{"running":true,"port":47800,"version":"` + pinned + `","started_at":"2026-11-20T09:30:00.000Z"}`}
	runs := func() int {
		data, _ := os.ReadFile(filepath.Join(home, "status-runs"))
		return strings.Count(string(data), "x")
	}
	for range 3 {
		guest, raw := runScienceSnapshot(t, home, tc)
		if state := claudeScienceFromGuest(guest, pinned); !state.Ready || state.Port != 47800 || state.StartedAt != "2026-11-20T09:30:00.000Z" || state.Version != pinned {
			t.Fatalf("%+v from %s", state, raw)
		}
	}
	if runs() != 1 {
		t.Fatalf("status ran %d times for three polls of one daemon run", runs())
	}
	// A new run of the unit is read again.
	tc.invocation = "run-2"
	runScienceSnapshot(t, home, tc)
	if runs() != 2 {
		t.Fatalf("status ran %d times; a restarted daemon was not read", runs())
	}
	// While the daemon starts (nothing listens yet) it is not asked at all.
	tc.invocation, tc.listener, tc.version = "run-3", false, pinned
	for range 2 {
		guest, raw := runScienceSnapshot(t, home, tc)
		if state := claudeScienceFromGuest(guest, pinned); !state.Running || state.Ready || state.Version != pinned {
			t.Fatalf("%+v from %s", state, raw)
		}
	}
	if runs() != 2 {
		t.Fatalf("status ran %d times while the daemon was starting", runs())
	}
}

func TestClaudeScienceUpdateAdvice(t *testing.T) {
	for _, tc := range []struct {
		installed, pinned, required, via string
	}{
		{"0.1.73", "0.1.73", "0.1.80", "super-cage"},
		{"0.1.72", "0.1.73", "0.1.73", "claude-science"},
		{"0.1.72", "0.1.80", "0.1.74", "claude-science"},
		{"0.1.80", "0.1.73", "0.1.80", ""},
		{"0.1.73", "0.1.73", "", ""},
		{"", "0.1.73", "0.1.80", ""},
	} {
		state := claudeScienceState{Version: tc.installed, PinnedVersion: tc.pinned}
		state.requireVersion(tc.required)
		if tc.via == "" {
			if state.UpdateRequired != nil || state.UpdateHint != "" {
				t.Fatalf("%+v: %+v", tc, state)
			}
			continue
		}
		if state.UpdateRequired == nil || !*state.UpdateRequired || state.RequiredVersion != tc.required || state.UpdateVia != tc.via || !strings.Contains(state.UpdateHint, tc.required) {
			t.Fatalf("%+v: %+v", tc, state)
		}
		if (tc.via == "super-cage") != strings.Contains(state.UpdateHint, "请先更新 Super Cage") {
			t.Fatalf("hint does not name the right update: %s", state.UpdateHint)
		}
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{{"0.1.73", "0.1.73", 0}, {"0.1.9", "0.1.73", -1}, {"0.2.0", "0.1.99", 1}, {"1.0", "1.0.0", 0}, {"", "0.1.0", -1}} {
		if got := compareScienceVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compare %s %s = %d", tc.a, tc.b, got)
		}
	}
}

func TestClaudeScienceLinkParsing(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	status := `{"running":true,"port":47800,"started_at":"2026-11-20T09:30:00.000Z"}`
	for _, host := range []string{"localhost", "127.0.0.1"} {
		link, err := parseClaudeScienceLink([]byte("http://" + host + ":47800/?nonce=" + nonce + "\n" + status))
		if err != nil || link.url != "http://"+host+":47800/?nonce="+nonce || link.startedAt != "2026-11-20T09:30:00.000Z" {
			t.Fatalf("%s: %+v %v", host, link, err)
		}
	}
	if link, err := parseClaudeScienceLink([]byte("http://localhost:47800/?nonce=" + nonce + "\nnot json")); err != nil || link.startedAt != "" {
		t.Fatalf("status is optional: %+v %v", link, err)
	}
	if link, err := parseClaudeScienceLink([]byte("http://localhost:47800/?nonce=" + nonce + "\n{\"started_at\":\"" + strings.Repeat("9", 65) + "\"}")); err != nil || link.startedAt != "" {
		t.Fatalf("oversized start time kept: %+v %v", link, err)
	}
	for _, output := range []string{
		"http://localhost:8000/?nonce=" + nonce,
		"http://localhost:47801/?nonce=" + nonce,
		"http://example.com:47800/?nonce=" + nonce,
		"https://localhost:47800/?nonce=" + nonce,
		"http://localhost:47800/?nonce=" + nonce + "&dest=/",
		"http://localhost:47800/api/auth/login?nonce=" + nonce,
		"http://localhost:47800/?nonce=short",
		" http://localhost:47800/?nonce=" + nonce,
		"",
	} {
		_, err := parseClaudeScienceLink([]byte(output + "\n" + status))
		if err == nil || strings.Contains(err.Error(), nonce) {
			t.Fatalf("%q: %v", output, err)
		}
	}
	if _, err := parseClaudeScienceLink([]byte("not-installed\n")); err != errScienceNotInstalled {
		t.Fatal(err)
	}
	if _, err := parseClaudeScienceLink([]byte("not-running\n")); err != errScienceNotRunning {
		t.Fatal(err)
	}
}

// The link is printed with shell builtins only: never an argument of a
// process other guest users could list.
func TestClaudeScienceLinkScript(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	binary := filepath.Join(home, ".local", "bin", "claude-science")
	run := func() string {
		t.Helper()
		out, err := exec.Command("bash", "-c", claudeScienceLinkScript).CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return string(out)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if out := run(); out != "not-installed\n" {
		t.Fatalf("%q", out)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncase \"$1\" in\n  status) printf '{\"running\":%s,\\n\"started_at\":\"t1\"}' \"$TEST_RUNNING\" ;;\n  url) [ \"$TEST_RUNNING\" = true ] || exit 4; echo 'http://localhost:47800/?nonce=0123456789abcdef' ;;\nesac\n"
	if err := os.WriteFile(binary, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_RUNNING", "false")
	if out := run(); out != "not-running\n" {
		t.Fatalf("%q", out)
	}
	t.Setenv("TEST_RUNNING", "true")
	link, err := parseClaudeScienceLink([]byte(run()))
	if err != nil || link.url != "http://localhost:47800/?nonce=0123456789abcdef" || link.startedAt != "t1" {
		t.Fatalf("%+v %v", link, err)
	}
}

// The UI fixtures are replies of this contract, decoded by the server's type.
func TestClaudeScienceFixturesMatchContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "ui", "scripts", "claude-science-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Text   string          `json:"text"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	states := map[string]claudeScienceState{}
	for _, name := range []string{"unsupported-architecture", "not-installed", "installing", "stopped", "running-signed-out", "running-signed-in", "running-host-render-available", "running-host-render", "running-host-render-elsewhere", "update-required", "error", "unreachable"} {
		fixture, ok := fixtures[name]
		if !ok {
			t.Fatalf("fixture %s missing", name)
		}
		if fixture.Status != 200 {
			if fixture.Status != 502 || fixture.Text == "" || fixture.Body != nil {
				t.Fatalf("%s: %+v", name, fixture)
			}
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(fixture.Body, &keys); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"signed_in", "update_required", "pinned_version", "supported", "installed", "running", "ready"} {
			if _, ok := keys[key]; !ok {
				t.Fatalf("%s lacks %s", name, key)
			}
		}
		states[name] = decodeScienceState(t, fixture.Body)
		if states[name].PinnedVersion != cage.ClaudeSciencePin().Version {
			t.Fatalf("%s pins %s", name, states[name].PinnedVersion)
		}
	}
	if s := states["unsupported-architecture"]; s.Supported || s.UnsupportedReason != scienceReasonArchitecture {
		t.Fatalf("%+v", s)
	}
	if s := states["installing"]; s.Operation != "install" || s.Installed {
		t.Fatalf("%+v", s)
	}
	if s := states["running-signed-out"]; !s.Ready || s.SignedIn == nil || *s.SignedIn || s.Port != cage.ClaudeSciencePort {
		t.Fatalf("%+v", s)
	}
	if s := states["running-signed-in"]; !s.Ready || s.SignedIn == nil || !*s.SignedIn {
		t.Fatalf("%+v", s)
	}
	if s := states["error"]; !s.Failed || s.FailureReason == "" {
		t.Fatalf("%+v", s)
	}
	// Host rendering (science_host.go): the pane's three cases.
	if r := states["running-host-render-available"].HostRender; r == nil || !r.Available || r.Enabled || r.Running {
		t.Fatalf("%+v", r)
	}
	if r := states["running-host-render"].HostRender; r == nil || !r.Available || !r.Enabled || !r.Running || !r.Session || !r.Audit.Known {
		t.Fatalf("%+v", r)
	}
	if r := states["running-host-render-elsewhere"].HostRender; r == nil || r.Available || r.UnavailableCode != "platform" || r.UnavailableReason != scienceReasonHostPlatform {
		t.Fatalf("%+v", r)
	}
	// The update fixture is exactly what the server says for that floor.
	got := states["update-required"]
	want := got
	want.UpdateRequired, want.RequiredVersion, want.UpdateVia, want.UpdateHint = nil, "", "", ""
	want.requireVersion(got.RequiredVersion)
	if want.UpdateRequired == nil || got.UpdateRequired == nil || *got.UpdateRequired != *want.UpdateRequired || got.UpdateVia != want.UpdateVia || got.UpdateHint != want.UpdateHint {
		t.Fatalf("fixture %+v, server %+v", got, want)
	}
}

func TestClaudeScienceJobNames(t *testing.T) {
	for kind, name := range map[string]string{"claude-science-install": "安装 Claude Science", "claude-science-update": "更新 Claude Science", "claude-science-start": "启动 Claude Science", "claude-science-stop": "关闭 Claude Science"} {
		if jobName(kind) != name {
			t.Fatalf("%s: %s", kind, jobName(kind))
		}
	}
	s := testServer(t)
	for kind, operation := range map[string]string{"claude-science-update": "update", "claude-desktop-install": "", "environment-tools": ""} {
		s.setJobForTest(Job{ID: "job", Kind: kind, Running: true})
		if got := s.claudeScienceOperation(); got != operation {
			t.Fatalf("%s: %q", kind, got)
		}
	}
	s.setJobForTest(Job{ID: "job", Kind: "claude-science-install", Running: false})
	if got := s.claudeScienceOperation(); got != "" {
		t.Fatalf("finished job reported: %q", got)
	}
}

// The agent gets the link, the start time and fresh; the caller gets the tab.
// Agent refusals arrive in Chinese and never carry the link.
func TestClaudeScienceOpenHandsTheLinkToTheAgent(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	output := []byte("http://localhost:47800/?nonce=" + nonce + "\n" + `{"running":true,"port":47800,"started_at":"t1"}`)
	for _, tc := range []struct {
		name   string
		status int
		reply  string
		fresh  bool
		want   int
		text   string
	}{
		{"opened", 200, `{"id":"T1","kind":"science","title":"Claude Science","reused":false}`, true, 200, ""},
		{"reused", 200, `{"id":"T1","kind":"science","title":"Claude Science","reused":true}`, false, 200, ""},
		{"no-tab", 200, `{}`, false, 502, "没有返回 Claude Science 标签页"},
		{"agent-refused-link", 400, `{"error":"Claude Science 登录链接无效"}`, false, 502, "打开 Claude Science失败：Claude Science 返回的登录链接无效"},
		{"agent-not-provisioned", 400, `{"error":"Claude Science 尚未安装或配置"}`, false, 502, "请重新启动 Claude Science"},
		{"old-agent", 400, `{"error":"Unknown browser action"}`, false, 502, "请更新环境组件"},
		{"browser-starting", 503, `{"error":"Browser is starting"}`, false, 502, "浏览器仍在启动"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			var got map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/science" || r.Header.Get("Authorization") != "Bearer agent-token" {
					t.Errorf("unexpected browser request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer server.Close()
			s.browserControl = newBrowserClient(t.Context())
			t.Cleanup(s.browserControl.close)
			s.browserControl.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
			}
			tab, status, err := s.openClaudeScienceLink(t.Context(), "agent-token", output, tc.fresh)
			if got["url"] != "http://localhost:47800/?nonce="+nonce || got["started_at"] != "t1" || got["fresh"] != tc.fresh || len(got) != 3 {
				t.Fatalf("agent request %v", got)
			}
			if status != tc.want {
				t.Fatalf("status %d, %v", status, err)
			}
			if tc.want != 200 {
				if err == nil || !strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), nonce) {
					t.Fatalf("error %v", err)
				}
				return
			}
			if err != nil || tab.ID != "T1" || tab.Kind != "science" || tab.Title != "Claude Science" || tab.Reused != (tc.name == "reused") {
				t.Fatalf("%+v %v", tab, err)
			}
		})
	}
	// Nothing reaches the agent when the guest has no usable link.
	s := testServer(t)
	s.browserControl = newBrowserClient(t.Context())
	t.Cleanup(s.browserControl.close)
	s.browserControl.http.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("browser agent contacted")
		return nil, errors.New("unexpected")
	}
	for output, want := range map[string]int{"not-installed\n": 409, "not-running\n": 409, "http://localhost:8000/?nonce=" + nonce + "\n{}": 502} {
		if _, status, err := s.openClaudeScienceLink(t.Context(), "agent-token", []byte(output), false); status != want || err == nil || strings.Contains(err.Error(), nonce) {
			t.Fatalf("%q: %d %v", output, status, err)
		}
	}
}

// The installer downloads through the maintenance proxy, which only admits
// listed software sources.
func TestClaudeScienceDownloadIsAMaintenanceSource(t *testing.T) {
	script := cage.ClaudeScienceInstallScript()
	match := regexp.MustCompile(`https://([a-z0-9.-]+)/operon-dist-[0-9a-f-]+/operon-releases/`).FindStringSubmatch(script)
	if match == nil {
		t.Fatal("installer has no pinned download URL")
	}
	if !maintenanceDestination(match[1]+":443", true) || maintenanceDestination(match[1]+":80", false) || maintenanceDestination(match[1]+".evil.test:443", true) {
		t.Fatalf("maintenance proxy policy for %s", match[1])
	}
}

// The daemon prints its link into the guest journal; diagnostics keep none.
func TestClaudeScienceLinkNeverReachesDiagnostics(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	for _, line := range []string{
		"[claude-science] Web UI: http://localhost:47800/?nonce=" + nonce,
		"open http://127.0.0.1:47800/?nonce=" + nonce + " in a browser",
	} {
		if got := diagnostics.Redact(line); strings.Contains(got, nonce) {
			t.Fatalf("diagnostics keep the link: %s", got)
		}
	}
}

// Through the real middleware: native guests read the unsupported status and
// never reach an action.
func TestClaudeScienceNativeGuestsThroughTheWorkbench(t *testing.T) {
	withScienceHostArch(t, "amd64")
	for name, setup := range map[string]func(*Server){
		"windows": func(s *Server) { s.windowsGuest = &guestos.WindowsProfile{} },
		"macos":   func(s *Server) { s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			setup(s)
			w := windowsBrowserRequestForTest(s, "GET", "/api/claude-science", nil)
			if w.Code != 200 {
				t.Fatalf("status: %d %s", w.Code, w.Body)
			}
			if state := decodeScienceState(t, w.Body.Bytes()); state.Supported || state.UnsupportedCode != "guest_system" {
				t.Fatalf("%+v", state)
			}
			for _, action := range []string{"install", "start", "open"} {
				w := windowsBrowserRequestForTest(s, "POST", "/api/claude-science/"+action, []byte("{}"))
				// The handler would answer with its own reason; the route
				// allowlist stops the request before it.
				if w.Code < 400 || strings.Contains(w.Body.String(), scienceReasonGuestSystem) {
					t.Fatalf("%s reached on a %s guest: %d %s", action, name, w.Code, w.Body)
				}
			}
		})
	}
}
