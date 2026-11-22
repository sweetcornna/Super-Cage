package cage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/guestprofile"
)

func TestClaudeScienceScriptsParse(t *testing.T) {
	for name, script := range map[string]string{"install": ClaudeScienceInstallScript(), "provision": ClaudeScienceProvisionScript(), "start": claudeScienceStart} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestClaudeSciencePinMatchesInstaller(t *testing.T) {
	pin := ClaudeSciencePin()
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(pin.Version) || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(pin.Build) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pin.SHA256) {
		t.Fatalf("pin is not reviewable: %+v", pin)
	}
	if !strings.Contains(claudeScienceInstall, "/operon-releases/$CAGE_CLAUDE_SCIENCE_BUILD/operon-linux-x64") || strings.Contains(claudeScienceInstall, "install-claude-science.sh") {
		t.Fatal("installer must fetch the pinned build itself, never the vendor's curl | bash script")
	}
}

type scienceInstallCase struct {
	arch, free, candidate, active string
	fsType                        string
	installed                     []string
	bwrapOld                      bool
	payload, payloadVersion       string
	pinPayload                    string
	existing                      string
}

type scienceInstallResult struct {
	out, home, target string
	err               error
	downloads, apt    string
}

// The installer runs as written, after the real prelude, with the guest's
// commands replaced on PATH and the download replaced at cage_curl_download.
func runScienceInstall(t *testing.T, home string, tc scienceInstallCase) scienceInstallResult {
	t.Helper()
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", "#!/bin/sh\nif [ \"$1\" = -m ]; then echo \"$TEST_ARCH\"; else exec /usr/bin/uname \"$@\"; fi\n")
	write("flock", "#!/bin/sh\nexit 0\n")
	write("systemctl", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/systemctl-requests\"\nif [ \"$1\" = is-active ] && [ \"$TEST_ACTIVE\" = active ]; then exit 0; fi\nexit 3\n")
	write("sudo", "#!/bin/sh\nif [ \"$1\" = env ]; then exec \"$@\"; fi\necho blocked-privileged-operation >&2\nexit 97\n")
	write("apt-get", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/apt-requests\"\n")
	write("apt-cache", "#!/bin/sh\nprintf 'bubblewrap:\\n  Installed: (none)\\n  Candidate: %s\\n' \"$TEST_CANDIDATE\"\n")
	write("dpkg-query", "#!/bin/sh\nfor package in $TEST_INSTALLED; do if [ \"$package\" = \"$3\" ]; then printf 'install ok installed'; exit 0; fi; done\nexit 1\n")
	write("dpkg", "#!/bin/sh\n[ \"$1\" = --compare-versions ] || exit 2\nexec python3 -c 'import re, sys\nkey = lambda v: [int(x) for x in re.findall(r\"[0-9]+\", v.split(\"-\")[0])]\na, op, b = sys.argv[1:4]\nsys.exit(0 if {\"gt\": key(a) > key(b), \"ge\": key(a) >= key(b), \"lt\": key(a) < key(b), \"le\": key(a) <= key(b), \"eq\": key(a) == key(b)}[op] else 1)' \"$2\" \"$3\" \"$4\"\n")
	write("df", "#!/bin/sh\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\n/dev/vda1 41943040 1000 %s 1%% /\\n' \"$TEST_FREE_KB\"\n")
	write("bwrap", "#!/bin/sh\nif [ \"$TEST_BWRAP_OLD\" = 1 ]; then echo 'bwrap: Unknown option --disable-userns' >&2; exit 1; fi\nexit 0\n")
	write("stat", "#!/bin/sh\nif [ \"$1\" = -f ]; then echo \"$TEST_FSTYPE\"; exit 0; fi\nexit 2\n")
	if _, err := exec.LookPath("shasum"); err == nil {
		write("sha256sum", "#!/bin/sh\nexec shasum -a 256 \"$@\"\n")
	}
	payload := filepath.Join(bin, "payload")
	if err := os.WriteFile(payload, []byte(tc.payload), 0755); err != nil {
		t.Fatal(err)
	}
	pinned := tc.pinPayload
	if pinned == "" {
		pinned = tc.payload
	}
	sum := sha256.Sum256([]byte(pinned))
	target := filepath.Join(home, ".local", "bin", "claude-science")
	if tc.existing != "" {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(tc.existing), 0755); err != nil {
			t.Fatal(err)
		}
	}
	installer := strings.NewReplacer("/usr/bin/bwrap", filepath.Join(bin, "bwrap"), "/lib/libc.musl-x86_64.so.1", filepath.Join(home, "no-musl")).Replace(claudeScienceInstall)
	fake := "CAGE_CLAUDE_SCIENCE_AMD64_SHA256=" + hex.EncodeToString(sum[:]) + "\n" +
		"cage_curl_download() { printf '%s\\n' \"$*\" >> \"$HOME/downloads\"; cat \"$TEST_PAYLOAD\" > \"$1\"; printf 200; }\n"
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+ToolInstallPrelude()+fake+installer)
	free := tc.free
	if free == "" {
		free = "52428800"
	}
	arch := tc.arch
	if arch == "" {
		arch = "x86_64"
	}
	installed := tc.installed
	if installed == nil {
		installed = []string{"ca-certificates", "curl", "bubblewrap", "socat", "python3"}
	}
	fsType := tc.fsType
	if fsType == "" {
		fsType = "ef53"
	}
	bwrapOld := "0"
	if tc.bwrapOld {
		bwrapOld = "1"
	}
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ARCH="+arch, "TEST_FREE_KB="+free, "TEST_CANDIDATE="+tc.candidate,
		"TEST_INSTALLED="+strings.Join(installed, " "), "TEST_BWRAP_OLD="+bwrapOld, "TEST_ACTIVE="+tc.active, "TEST_FSTYPE="+fsType, "TEST_PAYLOAD="+payload, "TEST_PAYLOAD_VERSION="+tc.payloadVersion)
	out, err := cmd.CombinedOutput()
	downloads, _ := os.ReadFile(filepath.Join(home, "downloads"))
	apt, _ := os.ReadFile(filepath.Join(home, "apt-requests"))
	return scienceInstallResult{out: string(out), home: home, target: target, err: err, downloads: string(downloads), apt: string(apt)}
}

const sciencePayload = "#!/bin/sh\nif [ \"$1\" = --version ]; then echo \"claude-science ${TEST_PAYLOAD_VERSION:-0.1.73} (release, public)\"; fi\n"

func scienceTarget(t *testing.T, r scienceInstallResult) string {
	t.Helper()
	data, err := os.ReadFile(r.target)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertNothingStaged(t *testing.T, r scienceInstallResult) {
	t.Helper()
	staged, _ := filepath.Glob(filepath.Join(r.home, ".local", "bin", ".claude-science-new-*"))
	if len(staged) != 0 {
		t.Fatalf("staged copies left behind: %v", staged)
	}
}

func TestClaudeScienceInstallRefusesBeforeChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		tc   scienceInstallCase
		want string
	}{
		{"aarch64", scienceInstallCase{arch: "aarch64"}, "暂无 Linux arm64 官方版本"},
		{"other-architecture", scienceInstallCase{arch: "riscv64"}, "只提供 x86-64 Linux 版本"},
		{"low-disk", scienceInstallCase{free: "1048576"}, "磁盘空间不足"},
		{"tmpfs-home", scienceInstallCase{fsType: "1021994"}, "临时文件系统"},
		{"overlay-home", scienceInstallCase{fsType: "794c7630"}, "临时文件系统"},
		{"running", scienceInstallCase{active: "active"}, "请先关闭 Claude Science"},
		{"old-bubblewrap-candidate", scienceInstallCase{installed: []string{"curl"}, candidate: "0.6.1-1ubuntu0.1"}, "bubblewrap 0.8"},
		{"old-bubblewrap-installed", scienceInstallCase{bwrapOld: true}, "bubblewrap 0.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.tc.payload = sciencePayload
			r := runScienceInstall(t, t.TempDir(), tc.tc)
			if r.err == nil || !strings.Contains(r.out, tc.want) {
				t.Fatalf("not refused: %v %s", r.err, r.out)
			}
			if r.downloads != "" || r.apt != "" || scienceTarget(t, r) != "" {
				t.Fatalf("refusal changed the guest: downloads=%q apt=%q", r.downloads, r.apt)
			}
			assertNothingStaged(t, r)
		})
	}
}

func TestClaudeScienceInstallChecksumMismatchInstallsNothing(t *testing.T) {
	r := runScienceInstall(t, t.TempDir(), scienceInstallCase{payload: sciencePayload + "# tampered\n", pinPayload: sciencePayload})
	if r.err == nil || !strings.Contains(r.out, "checksum mismatch") {
		t.Fatalf("tampered build accepted: %v %s", r.err, r.out)
	}
	if scienceTarget(t, r) != "" {
		t.Fatal("tampered build installed")
	}
	assertNothingStaged(t, r)
	cached, _ := filepath.Glob(filepath.Join(r.home, ".cache", "cage", "downloads", "*", "operon-linux-x64*"))
	if len(cached) != 0 {
		t.Fatalf("unverified download kept: %v", cached)
	}
}

func TestClaudeScienceInstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	first := runScienceInstall(t, home, scienceInstallCase{payload: sciencePayload, installed: []string{"curl"}, candidate: "0.11.0-2"})
	if first.err != nil || scienceTarget(t, first) != sciencePayload || !strings.Contains(first.out, "已安装") {
		t.Fatalf("first install: %v %s", first.err, first.out)
	}
	if info, err := os.Stat(first.target); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("installed binary mode: %v %v", info, err)
	}
	if strings.Count(first.downloads, "\n") != 1 || !strings.Contains(first.downloads, "https://storage.googleapis.com/operon-dist-cf94a20e-f71c-413c-bd00-9e12b1fedf59/operon-releases/"+ClaudeSciencePin().Build+"/operon-linux-x64") {
		t.Fatalf("download request: %q", first.downloads)
	}
	if !strings.Contains(first.apt, "--no-install-recommends ca-certificates bubblewrap socat python3") {
		t.Fatalf("missing packages not requested: %q", first.apt)
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".cache", "cage", "downloads", "*", "operon-linux-x64")); len(left) != 0 {
		t.Fatalf("installed download kept in the cache: %v", left)
	}
	second := runScienceInstall(t, home, scienceInstallCase{payload: sciencePayload})
	if second.err != nil || !strings.Contains(second.out, "无需重新下载") || second.downloads != first.downloads || scienceTarget(t, second) != sciencePayload {
		t.Fatalf("reinstall was not a no-op: %v %s %q", second.err, second.out, second.downloads)
	}
}

func TestClaudeScienceInstallUpdatesButNeverDowngrades(t *testing.T) {
	older := strings.Replace(sciencePayload, "${TEST_PAYLOAD_VERSION:-0.1.73}", "0.1.72", 1)
	r := runScienceInstall(t, t.TempDir(), scienceInstallCase{payload: sciencePayload, existing: older})
	if r.err != nil || scienceTarget(t, r) != sciencePayload || r.downloads == "" {
		t.Fatalf("older build not updated: %v %s", r.err, r.out)
	}
	newer := strings.Replace(sciencePayload, "${TEST_PAYLOAD_VERSION:-0.1.73}", "0.1.80", 1)
	r = runScienceInstall(t, t.TempDir(), scienceInstallCase{payload: sciencePayload, existing: newer})
	if r.err != nil || scienceTarget(t, r) != newer || r.downloads != "" || !strings.Contains(r.out, "保留现有版本") {
		t.Fatalf("newer build replaced: %v %s", r.err, r.out)
	}
	same := sciencePayload + "# modified locally\n"
	r = runScienceInstall(t, t.TempDir(), scienceInstallCase{payload: sciencePayload, existing: same})
	if r.err != nil || scienceTarget(t, r) != sciencePayload {
		t.Fatalf("modified build of the pinned version not repaired: %v %s", r.err, r.out)
	}
}

func TestClaudeScienceInstallChecksTheBuildItRuns(t *testing.T) {
	r := runScienceInstall(t, t.TempDir(), scienceInstallCase{payload: sciencePayload, payloadVersion: "0.1.70"})
	if r.err == nil || !strings.Contains(r.out, "不一致") || scienceTarget(t, r) != "" {
		t.Fatalf("build reporting another version installed: %v %s", r.err, r.out)
	}
	assertNothingStaged(t, r)
}

func TestClaudeScienceUnit(t *testing.T) {
	for _, line := range []string{
		"User=cage", "Type=simple", "EnvironmentFile=/etc/claude-cage/environment", "Environment=DO_NOT_TRACK=1", "LogNamespace=cage", "Restart=on-failure",
		"ExecStart=/usr/local/bin/cage-log-run /home/cage/.local/bin/claude-science serve --no-browser --no-auto-update --host 127.0.0.1 --port 47800 --sandbox-port 47801",
		"ExecStop=/home/cage/.local/bin/claude-science stop",
	} {
		if !strings.Contains("\n"+claudeScienceUnit, "\n"+line+"\n") {
			t.Errorf("unit lacks %q", line)
		}
	}
	// Opened by the user, never at boot; never reachable beyond guest loopback.
	for _, forbidden := range []string{"[Install]", "0.0.0.0", "--allow-origin", "--dangerously", "--base-path", "PROXY"} {
		if strings.Contains(claudeScienceUnit, forbidden) {
			t.Errorf("unit contains %q", forbidden)
		}
	}
	// The start script is only written here; it runs when the user opens it.
	provision := strings.Replace(ClaudeScienceProvisionScript(), claudeScienceStart, "", 1)
	for _, command := range []string{"systemctl enable", "systemctl start", "systemctl restart"} {
		if strings.Contains(provision, command) {
			t.Errorf("provisioning runs %q", command)
		}
	}
	if regexp.MustCompile(`(?m)^\s*(sudo\s+)?/usr/local/bin/cage-claude-science-start\b`).MatchString(provision) {
		t.Error("provisioning runs the start script")
	}
	if !strings.Contains(claudeScienceStart, "port=47800\n") || ClaudeSciencePort != 47800 || ClaudeScienceSandboxPort != 47801 {
		t.Error("start script and unit disagree on the port")
	}
}

// Provisioning runs as written; sudo writes below a scratch root instead of /.
func TestClaudeScienceProvisionWritesFiles(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	sudo := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$TEST_ROOT/sudo-requests\"\n" +
		"case \"$1\" in\n" +
		"  install) shift; while [ \"$1\" = -d ] || [ \"$1\" = -m ]; do if [ \"$1\" = -m ]; then shift; fi; shift; done; for d in \"$@\"; do mkdir -p \"$TEST_ROOT$d\"; done ;;\n" +
		"  tee) mkdir -p \"$(dirname \"$TEST_ROOT$2\")\"; cat > \"$TEST_ROOT$2\" ;;\n" +
		"  chmod) chmod \"$2\" \"$TEST_ROOT$3\" ;;\n" +
		"  sysctl|systemctl) ;;\n" +
		"  *) echo \"unexpected privileged command: $*\" >&2; exit 97 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(ClaudeScienceProvisionScript())
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	read := func(path string, mode os.FileMode) string {
		t.Helper()
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if mode != 0 && info.Mode().Perm() != mode {
			t.Fatalf("%s mode %v", path, info.Mode().Perm())
		}
		data, _ := os.ReadFile(filepath.Join(root, path))
		return string(data)
	}
	if got := read("/etc/systemd/system/cage-claude-science.service", 0); got != claudeScienceUnit {
		t.Fatalf("unit written:\n%s", got)
	}
	if got := read("/usr/local/bin/cage-claude-science-start", 0755); got != claudeScienceStart+"\n" {
		t.Fatal("start script differs from the embedded one")
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(read("/etc/claude-cage/science.json", 0644)), &config); err != nil || config["port"] != float64(47800) || config["sandbox_port"] != float64(47801) || len(config) != 2 {
		t.Fatalf("browser-agent config: %v %v", config, err)
	}
	if got := read("/etc/sysctl.d/60-cage-claude-science.conf", 0); got != "net.ipv4.ip_local_reserved_ports = 47800-47801\n" {
		t.Fatalf("reserved ports: %q", got)
	}
	requests, _ := os.ReadFile(filepath.Join(root, "sudo-requests"))
	if !strings.HasSuffix(string(requests), "systemctl daemon-reload\n") {
		t.Fatalf("privileged requests: %s", requests)
	}
}

// A refused sysctl (container mode) only warns.
func TestClaudeScienceProvisionToleratesRefusedSysctl(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	sudo := "#!/bin/sh\nif [ \"$1\" = sysctl ]; then echo 'permission denied' >&2; exit 255; fi\nif [ \"$1\" = tee ]; then mkdir -p \"$(dirname \"$TEST_ROOT$2\")\"; exec cat > \"$TEST_ROOT$2\"; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(ClaudeScienceProvisionScript())
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "端口未能保留") {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestClaudeScienceTemplatesFollowGuestAccount(t *testing.T) {
	p, err := guestprofile.New(guestprofile.Options{Username: "alex", BrowserUsername: "web", WorkHome: "/home/projects"})
	if err != nil {
		t.Fatal(err)
	}
	provision := p.Linux(ClaudeScienceProvisionScript())
	for _, want := range []string{"User=alex\n", "WorkingDirectory=/home/projects\n", "Environment=HOME=/home/projects\n", "/home/projects/.local/bin/claude-science serve", "binary=/home/projects/.local/bin/claude-science\n", "/etc/claude-cage/science.json", "cage-claude-science.service"} {
		if !strings.Contains(provision, want) {
			t.Errorf("rendered provisioning lacks %q", want)
		}
	}
	if strings.Contains(provision, "/home/cage") || strings.Contains(provision, "User=cage\n") {
		t.Error("rendered provisioning still names the default account")
	}
}

func TestBrowserScienceModuleShipsWithTheAgent(t *testing.T) {
	services := browserServices()
	module := strings.Index(services, "/usr/local/lib/browser-science.mjs")
	agent := strings.Index(services, "/usr/local/lib/cage-browser-agent.mjs")
	if module < 0 || agent < 0 || module > agent {
		t.Fatal("the browser agent would start without the module it imports")
	}
	if !strings.Contains(browserAgent, `from "./browser-science.mjs"`) || !strings.Contains(browserAgent, `req.url === "/science"`) {
		t.Fatal("browser agent lacks the Claude Science endpoint")
	}
}

func TestBrowserScienceNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, args := range [][]string{{"--test", "browser-science.test.mjs"}, {"--check", "browser-agent.mjs"}} {
		if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
			t.Fatalf("node %v: %v\n%s", args, err, out)
		}
	}
}

type scienceStartResult struct {
	out      string
	err      error
	requests string
}

// The start script runs as written with the daemon, systemd and ss replaced.
// TEST_STATUS is what claude-science status prints once a start was asked for.
func runScienceStart(t *testing.T, env ...string) scienceStartResult {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write("claude-science", "#!/bin/sh\nif [ \"$1\" = status ]; then if [ -f \"$TEST_DIR/started\" ]; then printf '%s' \"$TEST_STATUS\"; else printf '%s' \"$TEST_BEFORE\"; fi; fi\n")
	write("systemctl", "#!/bin/sh\ncase \"$1\" in\n  is-active) [ \"$TEST_ACTIVE\" = active ] ;;\n  is-failed) [ -f \"$TEST_DIR/started\" ] && [ \"$TEST_FAILS\" = 1 ] ;;\n  *) exit 1 ;;\nesac\n")
	write("sudo", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_DIR/requests\"\nif [ \"$1 $2\" = 'systemctl start' ]; then touch \"$TEST_DIR/started\"; fi\nif [ \"$1\" = journalctl ]; then printf 'listening on http://localhost:47800/?nonce=0123456789abcdef0123456789abcdef\\nGET /oauth/callback?code=fedcba9876543210code&state=fedcba9876543210state HTTP/1.1\\ncage-claude-science.service: Main process exited, code=exited, status=1/FAILURE\\nsandbox: bwrap failed\\n'; fi\n")
	write("ss", "#!/bin/sh\nif [ -n \"$TEST_LISTENER\" ]; then printf 'LISTEN 0 511 127.0.0.1:47800 0.0.0.0:*\\n'; fi\n")
	script := strings.ReplaceAll(claudeScienceStart, "/home/cage/.local/bin/claude-science", filepath.Join(dir, "claude-science"))
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_DIR="+dir, "TEST_BEFORE={\"running\":false}"), env...)
	out, err := cmd.CombinedOutput()
	requests, _ := os.ReadFile(filepath.Join(dir, "requests"))
	return scienceStartResult{string(out), err, string(requests)}
}

func TestClaudeScienceStart(t *testing.T) {
	running := "TEST_STATUS={\"running\":true,\"port\":47800,\"pid\":42,\"version\":\"0.1.73\"}"
	t.Run("starts-and-waits", func(t *testing.T) {
		r := runScienceStart(t, running)
		if r.err != nil || r.requests != "systemctl start --no-block cage-claude-science.service\n" {
			t.Fatalf("%v %s %q", r.err, r.out, r.requests)
		}
	})
	t.Run("already-running", func(t *testing.T) {
		r := runScienceStart(t, "TEST_ACTIVE=active", "TEST_BEFORE={\"running\":true,\"port\":47800}")
		if r.err != nil || r.requests != "" {
			t.Fatalf("%v %s %q", r.err, r.out, r.requests)
		}
	})
	t.Run("port-taken", func(t *testing.T) {
		r := runScienceStart(t, running, "TEST_LISTENER=1")
		if r.err == nil || !strings.Contains(r.out, "占用了 Claude Science 的端口 47800") || r.requests != "" {
			t.Fatalf("%v %s %q", r.err, r.out, r.requests)
		}
	})
	t.Run("unmanaged-daemon", func(t *testing.T) {
		r := runScienceStart(t, running, "TEST_BEFORE={\"running\":true,\"port\":8000}")
		if r.err == nil || !strings.Contains(r.out, "不由 Super Cage 管理") || r.requests != "" {
			t.Fatalf("%v %s %q", r.err, r.out, r.requests)
		}
	})
	t.Run("other-port", func(t *testing.T) {
		r := runScienceStart(t, "TEST_STATUS={\"running\":true,\"port\":8000}")
		if r.err == nil || !strings.Contains(r.out, "端口 8000") {
			t.Fatalf("%v %s", r.err, r.out)
		}
	})
	t.Run("unit-fails", func(t *testing.T) {
		r := runScienceStart(t, "TEST_STATUS={\"running\":false}", "TEST_FAILS=1")
		if r.err == nil || !strings.Contains(r.out, "未能启动") || !strings.Contains(r.out, "bwrap failed") {
			t.Fatalf("%v %s", r.err, r.out)
		}
		if strings.Contains(r.out, "0123456789abcdef") {
			t.Fatal("sign-in link leaked into the job log")
		}
		if strings.Contains(r.out, "fedcba9876543210") || !strings.Contains(r.out, "callback?code=…&state=…") {
			t.Fatalf("an OAuth code or state leaked into the job log: %s", r.out)
		}
		if !strings.Contains(r.out, "code=exited, status=1/FAILURE") {
			t.Fatalf("systemd's exit line was redacted: %s", r.out)
		}
	})
	t.Run("not-installed", func(t *testing.T) {
		script := strings.ReplaceAll(claudeScienceStart, "/home/cage/.local/bin/claude-science", filepath.Join(t.TempDir(), "missing"))
		out, err := exec.Command("bash", "-c", script).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "请先安装 Claude Science") {
			t.Fatalf("%v %s", err, out)
		}
	})
}

func TestClaudeScienceConfigKeepsUserSettings(t *testing.T) {
	if exec.Command("python3", "-c", "import tomllib").Run() != nil {
		t.Skip("python3 with tomllib unavailable")
	}
	for name, tc := range map[string]struct{ before, want string }{
		"fresh": {"", `{"disable_telemetry": true, "update": {"auto_update": false}}`},
		"user-keys": {
			"# mine\ndata_dir = \"/data/science\"\ndisable_telemetry = false\n\n[update]\nrelease_channel = \"stable\"\nauto_update = true\n\n[sandbox.network]\nenabled = true\n",
			`{"data_dir": "/data/science", "disable_telemetry": true, "update": {"release_channel": "stable", "auto_update": false}, "sandbox": {"network": {"enabled": true}}}`,
		},
		"tables-only": {
			"[network]\nproxy = \"http://proxy.example:8080\"\n[update.extra]\nx = 1\n",
			`{"disable_telemetry": true, "network": {"proxy": "http://proxy.example:8080"}, "update": {"extra": {"x": 1}, "auto_update": false}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			config := filepath.Join(home, ".claude-science", "config.toml")
			if tc.before != "" {
				if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, []byte(tc.before), 0640); err != nil {
					t.Fatal(err)
				}
			}
			// An already installed pinned build: only the configuration runs.
			first := runScienceInstall(t, home, scienceInstallCase{payload: sciencePayload})
			r := runScienceInstall(t, home, scienceInstallCase{payload: sciencePayload})
			if first.err != nil || r.err != nil {
				t.Fatalf("%v %v %s", first.err, r.err, r.out)
			}
			out, err := exec.Command("python3", "-c", "import json, sys, tomllib\nprint(json.dumps(tomllib.load(open(sys.argv[1], 'rb')), sort_keys=True))", config).CombinedOutput()
			if err != nil {
				data, _ := os.ReadFile(config)
				t.Fatalf("config no longer parses: %v %s\n%s", err, out, data)
			}
			var got, want any
			if json.Unmarshal(out, &got) != nil || json.Unmarshal([]byte(tc.want), &want) != nil {
				t.Fatal("decode")
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("config:\n got %s\nwant %s", gotJSON, wantJSON)
			}
			info, _ := os.Stat(config)
			if wantMode := os.FileMode(0600); tc.before != "" {
				wantMode = 0640
				if info.Mode().Perm() != wantMode {
					t.Fatalf("mode %v", info.Mode().Perm())
				}
			} else if info.Mode().Perm() != wantMode {
				t.Fatalf("mode %v", info.Mode().Perm())
			}
		})
	}
}
