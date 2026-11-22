package cage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agents"
	"github.com/sylvinus/agent-vm/internal/guestprofile"
)

// The installer runs as written, after the real prelude and the input
// agents.ScriptInput builds, inside the maintenance job's shape (a subshell
// read from stdin). Only the fixed paths at its top are rewritten to a test
// root, and the guest's commands are stand-ins on PATH; sudo's stand-in runs
// a command as the test user and refuses any absolute path outside the root.
// The download is real curl over TLS to a local server.

const agentEntryName = "codex-aarch64-unknown-linux-musl"

type agentServer struct {
	mu     sync.Mutex
	files  map[string][]byte
	gets   map[string]int
	status int
	ca     string
	port   string
}

// The test URLs name .invalid hosts, which never resolve; the test home's
// .curlrc connects them to this server. If that redirection were ignored the
// download fails rather than reaching a real host.
func newAgentServer(t *testing.T, dir string) *agentServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "agent install test"},
		DNSNames:  []string{"github.invalid", "release-assets.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	s := &agentServer{files: map[string][]byte{}, gets: map[string]int{}, ca: filepath.Join(dir, "ca.pem")}
	if err := os.WriteFile(s.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, s.port, _ = net.SplitHostPort(server.Listener.Addr().String())
	return s
}

func (s *agentServer) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	switch host {
	case "github.invalid":
		http.Redirect(w, r, "https://release-assets.invalid"+r.URL.Path, http.StatusFound)
	case "release-assets.invalid":
		s.mu.Lock()
		body, ok := s.files[r.URL.Path]
		status := s.status
		if r.Method == http.MethodGet {
			s.gets[r.URL.Path]++
		}
		s.mu.Unlock()
		switch {
		case status != 0:
			http.Error(w, "unavailable", status)
		case !ok:
			http.NotFound(w, r)
		default:
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		}
	default:
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
	}
}

func (s *agentServer) publish(e agents.Entry, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[agentURLPath(e)] = body
}

func (s *agentServer) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *agentServer) downloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, count := range s.gets {
		n += count
	}
	return n
}

func agentURLPath(e agents.Entry) string {
	return strings.TrimPrefix(e.Artifacts[agents.ARM64].URL, "https://github.com")
}

type agentRig struct {
	t                                       *testing.T
	bash, python                            string
	root, home, bin                         string
	lib, state, launchers, etc, proc, cache string
	server                                  *agentServer
}

func newAgentRig(t *testing.T) *agentRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the installer runs in Linux guests")
	}
	tools := map[string]string{}
	for _, tool := range []string{"bash", "python3", "curl", "stat"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skip(tool + " unavailable")
		}
		tools[tool] = path
	}
	if out, err := exec.Command(tools["python3"], "-c", "import sys; sys.exit(sys.version_info < (3, 8))").CombinedOutput(); err != nil {
		t.Skipf("python3 is older than 3.8: %s", out)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &agentRig{
		t: t, bash: tools["bash"], python: tools["python3"], root: root,
		home: filepath.Join(root, "home"), bin: filepath.Join(root, "standins"),
		lib: filepath.Join(root, "usr/local/lib/agent-clients"), state: filepath.Join(root, "var/lib/agent-clients"),
		launchers: filepath.Join(root, "usr/local/bin"), etc: filepath.Join(root, "etc"), proc: filepath.Join(root, "proc"),
	}
	r.cache = filepath.Join(r.home, ".cache/cage/downloads")
	for _, dir := range []string{r.home, r.bin, filepath.Join(root, "tmp"), filepath.Dir(r.lib), filepath.Dir(r.state), r.launchers, r.etc, r.proc} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.write(filepath.Join(r.launchers, "client_runtime.py"), ClientRuntimeHelper, 0o644)
	standins := map[string]string{
		"sudo": `#!/bin/sh
printf '%s\n' "$*" >>"$TEST_ROOT/sudo.log"
case "$1" in
  chown) exit 0 ;;
  env) exec "$@" ;;
esac
for argument in "$@"; do
  case "$argument" in
    "$TEST_ROOT"/*) ;;
    /*) echo "sudo stand-in: $argument is outside the test root" >&2; exit 97 ;;
  esac
done
exec "$@"
`,
		"uname":      "#!/bin/sh\nif [ \"$1\" = -m ]; then echo \"${TEST_MACHINE:-aarch64}\"; else exec /usr/bin/uname \"$@\"; fi\n",
		"flock":      "#!/bin/sh\nexit \"${TEST_FLOCK_STATUS:-0}\"\n",
		"timeout":    "#!/bin/sh\nif [ \"$1\" = -k ]; then shift 2; fi\nshift\nexec \"$@\"\n",
		"unshare":    "#!/bin/sh\nexit \"${TEST_USERNS_STATUS:-0}\"\n",
		"getconf":    "#!/bin/sh\nif [ \"$1\" = GNU_LIBC_VERSION ] && [ -n \"${TEST_GLIBC-2.41}\" ]; then echo \"glibc ${TEST_GLIBC-2.41}\"; exit 0; fi\nexit 1\n",
		"dpkg":       "#!/bin/sh\n[ \"$1\" = --compare-versions ] || exit 2\nexec \"$TEST_PYTHON\" -c 'import re, sys\nkey = lambda v: [int(x) for x in re.findall(r\"[0-9]+\", v.split(\"-\")[0])]\na, op, b = sys.argv[1:4]\nsys.exit(0 if {\"gt\": key(a) > key(b), \"ge\": key(a) >= key(b), \"lt\": key(a) < key(b), \"le\": key(a) <= key(b), \"eq\": key(a) == key(b)}[op] else 1)' \"$2\" \"$3\" \"$4\"\n",
		"dpkg-query": "#!/bin/sh\nfor package in ${TEST_INSTALLED-bubblewrap}; do if [ \"$package\" = \"$3\" ]; then printf 'install ok installed'; exit 0; fi; done\nexit 1\n",
		"apt-get":    "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$TEST_ROOT/apt.log\"\n",
		"df": `#!/bin/sh
echo 'Filesystem 1024-blocks Used Available Capacity Mounted on'
n=0
for argument in "$@"; do
  case "$argument" in -*) continue ;; esac
  n=$((n + 1))
  if [ "$n" = 1 ]; then echo "${TEST_HOME_FS:-/dev/vda1} 41943040 1000 ${TEST_HOME_FREE_KB:-52428800} 1% /"
  else echo "${TEST_LIB_FS:-/dev/vda1} 41943040 1000 ${TEST_LIB_FREE_KB:-52428800} 1% /"; fi
done
`,
		"sleep":   "#!/bin/sh\nexit 0\n",
		"python3": "#!/bin/sh\nexec \"$TEST_PYTHON\" \"$@\"\n",
	}
	for _, sum := range []string{"sha256", "sha512"} {
		if _, err := exec.LookPath(sum + "sum"); err != nil || runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("shasum"); err != nil {
				t.Skip("no SHA tool")
			}
			standins[sum+"sum"] = "#!/bin/sh\nexec shasum -a " + strings.TrimPrefix(sum, "sha") + " \"$@\"\n"
		}
	}
	for name, body := range standins {
		r.write(filepath.Join(r.bin, name), body, 0o755)
	}
	r.server = newAgentServer(t, root)
	r.write(filepath.Join(r.home, ".curlrc"), "connect-to = github.invalid:443:127.0.0.1:"+r.server.port+"\nconnect-to = release-assets.invalid:443:127.0.0.1:"+r.server.port+"\n", 0o600)
	return r
}

// with is the rig for a subtest: its failures belong to t.
func (r *agentRig) with(t *testing.T) *agentRig {
	c := *r
	c.t = t
	return &c
}

func (r *agentRig) write(path, body string, mode os.FileMode) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *agentRig) read(path string) string {
	r.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data)
}

// The child's environment is built from nothing: no CURL_HOME or proxy of
// the machine running the test leaks in. The proxy variables are the
// maintenance job's (internal/workbench/maintenance.go), with a credential
// the verify step must never see; the .invalid hosts bypass them.
func (r *agentRig) env(extra ...string) []string {
	proxy := "http://cage-install:proxy-secret@127.0.0.1:9"
	return append([]string{
		"HOME=" + r.home, "PATH=" + r.bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + filepath.Join(r.root, "tmp"),
		"TEST_ROOT=" + r.root, "TEST_PYTHON=" + r.python, "CURL_CA_BUNDLE=" + r.server.ca,
		"CAGE_INSTALL_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy, "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy,
		"NO_PROXY=invalid", "no_proxy=invalid", "CAGE_APT_CONFIG=" + filepath.Join(r.root, "apt.conf"), "CAGE_TEST_SECRET=1",
	}, extra...)
}

func (r *agentRig) compose(input string, rewrites ...[2]string) string {
	r.t.Helper()
	script := AgentInstallScript(input)
	rewrites = append([][2]string{
		{"agent_lib=/usr/local/lib/agent-clients\n", "agent_lib=" + r.lib + "\n"},
		{"agent_state=/var/lib/agent-clients\n", "agent_state=" + r.state + "\n"},
		{"agent_bin=/usr/local/bin\n", "agent_bin=" + r.launchers + "\n"},
		{"agent_runtime=/usr/local/bin/client_runtime.py\n", "agent_runtime=" + filepath.Join(r.launchers, "client_runtime.py") + "\n"},
		{"agent_proc=/proc\n", "agent_proc=" + r.proc + "\n"},
		{"agent_python=/usr/bin/python3\n", "agent_python=" + r.python + "\n"},
		{"agent_etc=/etc\n", "agent_etc=" + r.etc + "\n"},
	}, rewrites...)
	for _, rewrite := range rewrites {
		if strings.Count(script, rewrite[0]) != 1 {
			r.t.Fatalf("the installer must hold %q exactly once", rewrite[0])
		}
		script = strings.Replace(script, rewrite[0], rewrite[1], 1)
	}
	script = strings.ReplaceAll(script, "https://github.com/", "https://github.invalid/")
	// internal/workbench/maintenance.go maintenanceScript: a subshell fed on stdin to bash -se.
	return "set -euo pipefail\numask 077\n(\numask 022\n" + script + "\n)\n"
}

type agentResult struct {
	out, failed, done string
	code              int
	removed           bool
}

func (r *agentRig) run(input string, env ...string) agentResult {
	r.t.Helper()
	return r.runScript(r.compose(input), env...)
}

func (r *agentRig) runScript(script string, env ...string) agentResult {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bash, "-se")
	cmd.Stdin = strings.NewReader(script)
	cmd.Dir = r.root
	cmd.Env = r.env(env...)
	out, err := cmd.CombinedOutput()
	return r.result(string(out), err)
}

func (r *agentRig) result(out string, err error) agentResult {
	r.t.Helper()
	result := agentResult{out: out}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.code = exit.ExitCode()
	} else if err != nil {
		r.t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(result.out, "\n")
	for i, line := range lines {
		switch fields := strings.Fields(line); {
		case strings.HasPrefix(line, "CAGE_AGENT_FAILED "):
			result.failed = fields[1]
			if i+1 >= len(lines) || lines[i+1] == "" {
				r.t.Errorf("no message follows the failure line:\n%s", result.out)
			}
		case strings.HasPrefix(line, "CAGE_AGENT_DONE "):
			result.done = fields[2]
		case strings.HasPrefix(line, "CAGE_AGENT_REMOVED "):
			result.removed = true
		}
	}
	return result
}

type tarMember struct {
	name, body, link string
	typ              byte
	mode             int64
}

func agentTarGz(t *testing.T, members ...tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Typeflag: m.typ, Mode: m.mode, Linkname: m.link, ModTime: time.Unix(1700000000, 0)}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(m.body))
		}
		if h.Typeflag == tar.TypeChar {
			h.Devmajor, h.Devminor = 1, 3
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The fake client writes its environment where the test can read it: at
// --version (the verify step) and at a normal start (through the launcher).
func (r *agentRig) clientBody(version string) string {
	return "#!/bin/sh\nif [ \"$1\" = --version ]; then\n  env >'" + r.root + "/verify-env'\n  echo 'codex-cli " + version + "'\n  exit 0\nfi\n" +
		"env >'" + r.root + "/run-env'\nprintf '%s\\n' \"$@\" >'" + r.root + "/run-args'\n"
}

func (r *agentRig) client(version string) []byte {
	return agentTarGz(r.t, tarMember{name: agentEntryName, body: r.clientBody(version), mode: 0o755})
}

// entry is the shipped Codex entry with its arm64 pin moved to body.
func (r *agentRig) entry(version string, body []byte) agents.Entry {
	r.t.Helper()
	e, ok, err := agents.Lookup("codex")
	if err != nil || !ok {
		r.t.Fatal("codex entry", err)
	}
	unpacked := int64(0)
	if gz, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
		tr := tar.NewReader(gz)
		for h, err := tr.Next(); err == nil; h, err = tr.Next() {
			unpacked += max(h.Size, 0)
		}
	}
	sum := sha256.Sum256(body)
	e.Version = version
	e.Env = map[string]string{"CODEX_TEST_SWITCH": "off"}
	e.Artifacts = map[agents.Arch]agents.Artifact{agents.ARM64: {
		Format: agents.FormatTarGz, URL: "https://github.com/openai/codex/releases/download/rust-v" + version + "/" + agentEntryName + ".tar.gz",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), Unpacked: max(unpacked, 1), Entry: agentEntryName,
	}}
	return e
}

func (r *agentRig) input(e agents.Entry, action agents.Action, data ...string) string {
	r.t.Helper()
	input, err := agents.ScriptInput(e, agents.ARM64, action, data)
	if err != nil {
		r.t.Fatal(err)
	}
	return input
}

// install publishes a client of version and installs it.
func (r *agentRig) install(version string) (agents.Entry, agentResult) {
	r.t.Helper()
	body := r.client(version)
	e := r.entry(version, body)
	r.server.publish(e, body)
	result := r.run(r.input(e, agents.Install))
	if result.code != 0 || result.done != version {
		r.t.Fatalf("install %s: exit %d failed=%q\n%s", version, result.code, result.failed, result.out)
	}
	return e, result
}

func (r *agentRig) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// absent: nothing of the feature in the guest, as before the first install.
func (r *agentRig) absent(result agentResult) {
	r.t.Helper()
	for _, path := range []string{r.lib, r.state, filepath.Join(r.launchers, "codex"), filepath.Join(r.etc, "codex")} {
		if r.exists(path) {
			r.t.Fatalf("%s is left behind\n%s", path, result.out)
		}
	}
}

func (r *agentRig) receipt() map[string]any {
	r.t.Helper()
	var receipt map[string]any
	if err := json.Unmarshal([]byte(r.read(filepath.Join(r.state, "codex.json"))), &receipt); err != nil {
		r.t.Fatal(err)
	}
	return receipt
}

func (r *agentRig) current() string {
	link, _ := os.Readlink(filepath.Join(r.lib, "codex", "current"))
	return link
}

func (r *agentRig) stages() []string {
	stages, _ := filepath.Glob(filepath.Join(r.lib, ".stage-*"))
	return stages
}

// statKey is what the launcher's one stat call prints for the entry binary:
// GNU stat -c %i:%s:%Y on Linux, its BSD equivalent here.
func (r *agentRig) statKey(path string) string {
	r.t.Helper()
	args := []string{"-L", "-c", "%i:%s:%Y", path}
	if runtime.GOOS == "darwin" {
		args = []string{"-L", "-f", "%i:%z:%m", path}
	}
	out, err := exec.Command("stat", args...).Output()
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestAgentInstallScriptParsesAndIsNotHashed(t *testing.T) {
	e, _, _ := agents.Lookup("codex")
	input, err := agents.ScriptInput(e, agents.ARM64, agents.Install, nil)
	if err != nil {
		t.Fatal(err)
	}
	script := AgentInstallScript(input)
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	// guestRevision hashes exactly these; the installer travels with each job instead.
	for name, hashed := range map[string]string{"dependencies": guestDependencies, "environment": guestEnv, "start": componentStartScript(), "prelude": ToolInstallPrelude()} {
		for _, marker := range []string{"super-cage-agent-launcher", "CAGE_AGENT_", "agent-clients"} {
			if strings.Contains(hashed, marker) {
				t.Errorf("the hashed %s script holds %q", name, marker)
			}
		}
	}
	if !strings.Contains(agentInstall, "\nagent_bin="+agents.LauncherDir+"\n") {
		t.Fatalf("launchers go to %s, the catalog's LauncherDir", agents.LauncherDir)
	}
	// The download filter recognises this line of cage_download_tool.
	if !strings.Contains(guestTools, "echo 'Tool checksum mismatch;") {
		t.Fatal("guest-tools.sh no longer prints the checksum line the installer reads")
	}
}

// The maintenance job renders every script for the guest account; nothing in
// the installer or its input may be rewritten by that.
func TestAgentInstallFollowsGuestAccount(t *testing.T) {
	p, err := guestprofile.New(guestprofile.Options{Username: "alex", BrowserUsername: "web", WorkHome: "/home/projects"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := agents.All()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		for _, action := range []agents.Action{agents.Install, agents.Remove} {
			var data []string
			if action == agents.Remove {
				for _, d := range e.DataDirs {
					data = append(data, d.Path)
				}
			}
			input, err := agents.ScriptInput(e, agents.ARM64, action, data)
			if err != nil {
				continue
			}
			checked++
			// The prelude is rendered as for every other job; the installer and its input must come through unchanged.
			script := input + agentInstall
			if rendered := p.Linux(script); rendered != script {
				got, want := strings.Split(rendered, "\n"), strings.Split(script, "\n")
				for i := range min(len(got), len(want)) {
					if got[i] != want[i] {
						t.Errorf("%s %s: the guest profile rewrites %q to %q", e.ID, action, want[i], got[i])
						break
					}
				}
			}
		}
	}
	if checked < 4 {
		t.Fatalf("only %d scripts checked", checked)
	}
}

func TestAgentInstallFromTLSServer(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	result := r.run(r.input(e, agents.Install))
	if result.code != 0 || result.done != "0.171.0" || result.failed != "" {
		t.Fatalf("exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
	if r.server.downloads() != 1 {
		t.Fatalf("downloads = %d", r.server.downloads())
	}
	var phases []string
	for _, line := range strings.Split(result.out, "\n") {
		if fields := strings.Fields(line); len(fields) == 4 && fields[0] == "CAGE_AGENT_PROGRESS" && !slices.Contains(phases, fields[1]) {
			phases = append(phases, fields[1])
		}
	}
	if !slices.Equal(phases, []string{"check", "download", "digest", "unpack", "verify", "done"}) {
		t.Fatalf("phases %v\n%s", phases, result.out)
	}
	size := strconv.Itoa(len(body))
	if !strings.Contains(result.out, "CAGE_AGENT_PROGRESS download "+size+" "+size+"\n") {
		t.Fatalf("no complete download progress:\n%s", result.out)
	}
	binary := filepath.Join(r.lib, "codex", "0.171.0", agentEntryName)
	if got := r.read(binary); got != r.clientBody("0.171.0") || r.current() != "0.171.0" {
		t.Fatalf("installed %q, current %q", got, r.current())
	}
	if info, _ := os.Stat(binary); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode())
	}
	if !strings.Contains(r.read(filepath.Join(r.root, "sudo.log")), "chown -hR 0:0 -- "+r.lib+"/.stage-") {
		t.Fatal("the unpacked tree was not handed to root")
	}
	if len(r.stages()) != 0 || r.exists(filepath.Join(r.cache, e.Artifacts[agents.ARM64].SHA256)) {
		t.Fatal("stage or download cache left behind")
	}
	launcher := r.read(filepath.Join(r.launchers, "codex"))
	wantLauncher := "#!/bin/sh\n# super-cage-agent-launcher codex\n# Written by Super Cage (设置 → 其他客户端); updating or removing the client there replaces it.\n" +
		"export CODEX_TEST_SWITCH='off'\nexec '" + r.python + "' -I '" + filepath.Join(r.launchers, "client_runtime.py") + "' '" + filepath.Join(r.lib, "codex/current", agentEntryName) + "' \"$@\"\n"
	if launcher != wantLauncher {
		t.Fatalf("launcher:\n%s\nwant:\n%s", launcher, wantLauncher)
	}
	receipt := r.receipt()
	entrySum := sha256.Sum256([]byte(r.clientBody("0.171.0")))
	launcherSum := sha256.Sum256([]byte(launcher))
	stat := r.statKey(binary)
	for key, want := range map[string]any{
		"schema": float64(1), "id": "codex", "version": "0.171.0", "arch": "arm64", "command": "codex", "entry": agentEntryName,
		"entry_sha256": hex.EncodeToString(entrySum[:]), "launcher_sha256": hex.EncodeToString(launcherSum[:]), "stat_key": stat,
	} {
		if receipt[key] != want {
			t.Errorf("receipt %s = %v, want %v", key, receipt[key], want)
		}
	}
	if artifact, _ := receipt["artifact"].(map[string]any); len(artifact) != 1 || artifact["sha256"] != e.Artifacts[agents.ARM64].SHA256 {
		t.Errorf("receipt artifact %v", receipt["artifact"])
	}
	// The sidecar is what the launcher reads: one line, one read in plain sh.
	if got, err := exec.Command("sh", "-c", "IFS= read -r key <\"$1\" && printf %s \"$key\"", "sh", filepath.Join(r.state, "codex.stat")).Output(); err != nil || string(got) != stat {
		t.Fatalf("stat sidecar %q %v, want %q", got, err, stat)
	}
	config := r.read(filepath.Join(r.etc, "codex/config.toml"))
	if config != "check_for_update_on_startup = false\n\n[analytics]\nenabled = false\n\n[feedback]\nenabled = false\n" {
		t.Fatalf("codex system settings:\n%s", config)
	}
	settings, _ := receipt["settings"].([]any)
	if len(settings) != 1 {
		t.Fatalf("receipt settings %v", receipt["settings"])
	}
	configSum := sha256.Sum256([]byte(config))
	if s := settings[0].(map[string]any); s["file"] != "/etc/codex/config.toml" || s["created"] != true || s["dir_created"] != true || s["sha256"] != hex.EncodeToString(configSum[:]) {
		t.Fatalf("receipt setting %v", s)
	}
	// The verify step saw an empty environment: no proxy, no credential, nothing of Cage's.
	verifyEnv := r.read(filepath.Join(r.root, "verify-env"))
	for _, line := range strings.Split(verifyEnv, "\n") {
		key, _, _ := strings.Cut(line, "=")
		if strings.Contains(strings.ToLower(key), "proxy") || strings.HasPrefix(key, "CAGE_") || strings.Contains(line, "proxy-secret") || key == "CURL_CA_BUNDLE" || key == "TEST_ROOT" {
			t.Errorf("the verify step saw %s", line)
		}
	}
	for _, want := range []string{"HOME=" + r.home + "\n", "PATH=/usr/bin:/bin\n", "CODEX_TEST_SWITCH=off\n"} {
		if !strings.Contains(verifyEnv, want) {
			t.Errorf("the verify step lacks %q:\n%s", want, verifyEnv)
		}
	}
	// The launcher starts the client through client_runtime.py: arguments
	// pass, CAGE_* is removed, the entry's switches are set.
	run := exec.Command(filepath.Join(r.launchers, "codex"), "exec", "it's $(x)")
	run.Env = r.env()
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v %s", err, out)
	}
	runEnv := r.read(filepath.Join(r.root, "run-env"))
	if strings.Contains(runEnv, "CAGE_") || !strings.Contains(runEnv, "CODEX_TEST_SWITCH=off\n") {
		t.Fatalf("client environment:\n%s", runEnv)
	}
	if args := r.read(filepath.Join(r.root, "run-args")); args != "exec\nit's $(x)\n" {
		t.Fatalf("client arguments %q", args)
	}
}

func TestAgentInstallIsIdempotent(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	receipt := r.read(filepath.Join(r.state, "codex.json"))
	launcher, _ := os.Stat(filepath.Join(r.launchers, "codex"))
	again := r.run(r.input(e, agents.Install))
	if again.code != 0 || again.done != "0.171.0" || !strings.Contains(again.out, "已是最新") {
		t.Fatalf("exit %d\n%s", again.code, again.out)
	}
	if r.server.downloads() != 1 {
		t.Fatal("an up-to-date install downloaded again")
	}
	if r.read(filepath.Join(r.state, "codex.json")) != receipt {
		t.Fatal("an up-to-date install rewrote the receipt")
	}
	if after, _ := os.Stat(filepath.Join(r.launchers, "codex")); !after.ModTime().Equal(launcher.ModTime()) {
		t.Fatal("an up-to-date install rewrote the launcher")
	}
	update := r.run(r.input(e, agents.Update))
	if update.code != 0 || update.done != "0.171.0" || r.server.downloads() != 1 {
		t.Fatalf("update to the same pin: exit %d\n%s", update.code, update.out)
	}
}

func TestAgentInstallUpdatesAndNeverDowngrades(t *testing.T) {
	r := newAgentRig(t)
	older, _ := r.install("0.171.0")
	newer, _ := r.install("0.172.0")
	if r.current() != "0.172.0" || r.exists(filepath.Join(r.lib, "codex", "0.171.0")) || r.receipt()["version"] != "0.172.0" {
		t.Fatalf("update left current=%q", r.current())
	}
	downloads := r.server.downloads()
	back := r.run(r.input(older, agents.Install))
	if back.code != 0 || back.done != "0.172.0" || !strings.Contains(back.out, "保留现有版本") || r.current() != "0.172.0" || r.server.downloads() != downloads {
		t.Fatalf("an older pin replaced a newer install: exit %d\n%s", back.code, back.out)
	}
	if again := r.run(r.input(newer, agents.Update)); again.code != 0 || again.done != "0.172.0" {
		t.Fatalf("%s", again.out)
	}
}

func TestAgentInstallSHA512(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	sum512 := sha512.Sum512(body)
	a := e.Artifacts[agents.ARM64]
	a.SHA256, a.SHA512 = "", hex.EncodeToString(sum512[:])
	e.Artifacts[agents.ARM64] = a
	r.server.publish(e, body)
	if result := r.run(r.input(e, agents.Install)); result.code != 0 || result.done != "0.171.0" {
		t.Fatalf("sha512 pin: exit %d\n%s", result.code, result.out)
	}
	if artifact, _ := r.receipt()["artifact"].(map[string]any); artifact["sha512"] != a.SHA512 || len(artifact) != 1 {
		t.Fatalf("receipt artifact %v", artifact)
	}
	// With both pinned, the download checks sha512 and the unpack step both:
	// a wrong sha256 alone still refuses.
	body = r.client("0.172.0")
	e = r.entry("0.172.0", body)
	sum512 = sha512.Sum512(body)
	a = e.Artifacts[agents.ARM64]
	a.SHA512, a.SHA256 = hex.EncodeToString(sum512[:]), strings.Repeat("0", 64)
	e.Artifacts[agents.ARM64] = a
	r.server.publish(e, body)
	result := r.run(r.input(e, agents.Install))
	if result.failed != "digest" || r.current() != "0.171.0" {
		t.Fatalf("a wrong sha256 beside a right sha512 was accepted: %q\n%s", result.failed, result.out)
	}
	if left, _ := filepath.Glob(filepath.Join(r.cache, a.SHA512, "codex.tar.gz")); len(left) != 0 {
		t.Fatal("a download that failed a digest stays in the cache")
	}
}

func TestAgentInstallWrongDigestKeepsTheInstalledVersion(t *testing.T) {
	r := newAgentRig(t)
	r.install("0.171.0")
	pinned := r.client("0.172.0")
	e := r.entry("0.172.0", pinned)
	r.server.publish(e, append(slices.Clone(pinned), 0))
	result := r.run(r.input(e, agents.Update))
	if result.code != 1 || result.failed != "digest" || !strings.Contains(result.out, "安装文件校验失败，已停止安装，原有版本保持不变。") {
		t.Fatalf("exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
	if r.current() != "0.171.0" || r.receipt()["version"] != "0.171.0" || len(r.stages()) != 0 {
		t.Fatal("the installed version was touched")
	}
	if left, _ := filepath.Glob(filepath.Join(r.cache, "*", "codex.tar.gz*")); len(left) != 0 {
		t.Fatalf("an unverified download is kept: %v", left)
	}
}

func TestAgentInstallPinGone(t *testing.T) {
	r := newAgentRig(t)
	e := r.entry("0.171.0", r.client("0.171.0"))
	result := r.run(r.input(e, agents.Install))
	if result.code != 1 || result.failed != "gone" || !strings.Contains(result.out, "OpenAI 已不再提供 Codex 0.171.0 的下载。请更新 Super Cage 以获取新的固定版本。") {
		t.Fatalf("exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
	r.absent(result)
}

func TestAgentInstallDownloadInterrupted(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	r.server.fail(http.StatusServiceUnavailable)
	result := r.run(r.input(e, agents.Install))
	if result.failed != "download" || !strings.Contains(result.out, "下载中断，已保存进度。重试会继续下载。") || r.server.downloads() != 4 {
		t.Fatalf("failed=%q downloads=%d\n%s", result.failed, r.server.downloads(), result.out)
	}
	r.absent(result)
}

func TestAgentInstallRefusesHostileArchives(t *testing.T) {
	r := newAgentRig(t)
	entry := tarMember{name: agentEntryName, body: r.clientBody("0.171.0"), mode: 0o755}
	for _, tc := range []struct {
		name    string
		members []tarMember
		body    []byte
		want    string
	}{
		{"dot-dot", []tarMember{entry, {name: "../evil", body: "x"}}, nil, "leaves the archive"},
		{"dot-dot-inside", []tarMember{entry, {name: "lib/../../evil", body: "x"}}, nil, "leaves the archive"},
		{"absolute", []tarMember{entry, {name: "/tmp/evil", body: "x"}}, nil, "absolute path"},
		{"symlink-absolute", []tarMember{entry, {name: "evil", typ: tar.TypeSymlink, link: "/etc/passwd"}}, nil, "outside its own directory"},
		{"symlink-up", []tarMember{entry, {name: "evil", typ: tar.TypeSymlink, link: "../../evil"}}, nil, "outside its own directory"},
		{"hard-link-out", []tarMember{entry, {name: "evil", typ: tar.TypeLink, link: "/etc/passwd"}}, nil, "hard link"},
		{"hard-link-in", []tarMember{entry, {name: "evil", typ: tar.TypeLink, link: agentEntryName}}, nil, "hard link"},
		{"device", []tarMember{entry, {name: "evil", typ: tar.TypeChar}}, nil, "device file"},
		{"fifo", []tarMember{entry, {name: "evil", typ: tar.TypeFifo}}, nil, "named pipe"},
		{"setuid", []tarMember{{name: agentEntryName, body: entry.body, mode: 0o4755}}, nil, "setuid"},
		{"setgid-directory", []tarMember{{name: "lib/", typ: tar.TypeDir, mode: 0o2755}, entry}, nil, "setuid, setgid or sticky"},
		{"under-a-link", []tarMember{entry, {name: "sub/", typ: tar.TypeDir, mode: 0o755}, {name: "lib", typ: tar.TypeSymlink, link: "sub"}, {name: "lib/evil", body: "x"}}, nil, "not a directory"},
		{"listed-twice", []tarMember{entry, entry}, nil, "listed twice"},
		{"no-program", []tarMember{{name: "evil", body: "x"}}, nil, "is not a file in the archive"},
		{"program-is-a-link", []tarMember{{name: "real", body: entry.body, mode: 0o755}, {name: agentEntryName, typ: tar.TypeSymlink, link: "real"}}, nil, "is not a file in the archive"},
		{"control-character", []tarMember{entry, {name: "evil\nname", body: "x"}}, nil, "not plain"},
		{"not-utf-8", []tarMember{entry, {name: "evil\xff", body: "x"}}, nil, "not plain"},
		{"not-gzip", nil, []byte("this is not a gzip stream\n"), "damaged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := r.with(t)
			body := tc.body
			if body == nil {
				body = agentTarGz(t, tc.members...)
			}
			e := r.entry("0.171.0", body)
			r.server.publish(e, body)
			result := r.run(r.input(e, agents.Install))
			if result.code != 1 || result.failed != "archive" || !strings.Contains(result.out, tc.want) {
				t.Fatalf("exit %d failed=%q, want archive %q\n%s", result.code, result.failed, tc.want, result.out)
			}
			r.absent(result)
			_ = filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
				if err == nil && strings.HasPrefix(info.Name(), "evil") {
					t.Errorf("%s was written", path)
				}
				return nil
			})
		})
	}
	t.Run("larger-than-pinned", func(t *testing.T) {
		r := r.with(t)
		body := agentTarGz(t, entry, tarMember{name: "data", body: strings.Repeat("x", 4096)})
		e := r.entry("0.171.0", body)
		a := e.Artifacts[agents.ARM64]
		a.Unpacked = int64(len(entry.body)) + 100
		e.Artifacts[agents.ARM64] = a
		r.server.publish(e, body)
		result := r.run(r.input(e, agents.Install))
		if result.failed != "archive" || !strings.Contains(result.out, "more than the pinned") {
			t.Fatalf("failed=%q\n%s", result.failed, result.out)
		}
		r.absent(result)
	})
}

func TestAgentInstallChecksTheProgramItRuns(t *testing.T) {
	r := newAgentRig(t)
	for name, body := range map[string][]byte{
		"other-version": r.client("0.170.0"),
		"not-runnable":  agentTarGz(t, tarMember{name: agentEntryName, body: "\x7fELF garbage", mode: 0o755}),
	} {
		t.Run(name, func(t *testing.T) {
			r := r.with(t)
			e := r.entry("0.171.0", body)
			r.server.publish(e, body)
			result := r.run(r.input(e, agents.Install))
			if result.code != 1 || result.failed != "verify" {
				t.Fatalf("exit %d failed=%q\n%s", result.code, result.failed, result.out)
			}
			r.absent(result)
		})
	}
}

func TestAgentInstallRefusesBeforeAnyChange(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	base := r.entry("0.171.0", body)
	r.server.publish(base, body)
	glibc := base
	glibc.Libc, glibc.MinGlibc = "glibc", "2.39"
	keyring := base
	keyring.Requires.Keyring = true
	big := base
	a := big.Artifacts[agents.ARM64]
	a.Unpacked = 300 << 20
	big.Artifacts = map[agents.Arch]agents.Artifact{agents.ARM64: a}
	for _, tc := range []struct {
		name  string
		e     agents.Entry
		env   []string
		setup func()
		input func(string) string
		code  string
		want  string
	}{
		{name: "architecture", e: base, env: []string{"TEST_MACHINE=x86_64"}, code: "arch", want: "这个客机的架构是 x86_64"},
		{name: "no-glibc", e: glibc, env: []string{"TEST_GLIBC="}, code: "libc", want: "需要 glibc"},
		{name: "old-glibc", e: glibc, env: []string{"TEST_GLIBC=2.36"}, code: "libc", want: "glibc 2.39 或更高版本，这个客机是 2.36"},
		{name: "keyring", e: keyring, code: "keyring", want: "密钥环"},
		{name: "user-namespaces", e: base, env: []string{"TEST_USERNS_STATUS=1"}, code: "userns", want: "命名空间"},
		{name: "withdrawn", e: base, input: func(s string) string {
			return strings.Replace(s, "AGENT_STATUS=hidden\n", "AGENT_STATUS=withdrawn\n", 1)
		}, code: "withdrawn", want: "不再受 Super Cage 支持"},
		{name: "own-install-in-local-bin", e: base, setup: func() {
			r.write(filepath.Join(r.home, ".local/bin/codex"), "#!/bin/sh\necho own\n", 0o755)
		}, code: "own-install", want: "已经安装了 Codex（自行安装，" + r.home + "/.local/bin/codex），Super Cage 不会覆盖它。"},
		{name: "own-install-in-launcher-place", e: base, setup: func() {
			r.write(filepath.Join(r.launchers, "codex"), "#!/bin/sh\n# someone else's\n", 0o755)
		}, code: "own-install", want: "自行安装"},
		{name: "disk", e: big, env: []string{"TEST_HOME_FREE_KB=102400"}, code: "disk", want: "客机磁盘空间不足：需要 331 MB，当前只有 100 MB。"},
		{name: "disk-separate-filesystem", e: big, env: []string{"TEST_LIB_FS=/dev/vdb1", "TEST_LIB_FREE_KB=204800"}, code: "disk", want: "需要 330 MB，当前只有 200 MB"},
		{name: "settings-cannot-merge", e: base, setup: func() {
			r.write(filepath.Join(r.etc, "codex/config.toml"), "analytics = { enabled = true }\n", 0o644)
		}, code: "settings", want: "无法合并 Codex 的设置文件"},
		{name: "busy", e: base, env: []string{"TEST_FLOCK_STATUS=1"}, code: "busy", want: "仍在进行"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := r.with(t)
			for _, path := range []string{r.lib, r.state, filepath.Join(r.home, ".local"), filepath.Join(r.launchers, "codex"), filepath.Join(r.etc, "codex"), filepath.Join(r.root, "apt.log")} {
				_ = os.RemoveAll(path)
			}
			if tc.setup != nil {
				tc.setup()
			}
			input := r.input(tc.e, agents.Install)
			if tc.input != nil {
				input = tc.input(input)
			}
			downloads := r.server.downloads()
			result := r.run(input, append([]string{"TEST_INSTALLED="}, tc.env...)...)
			if result.code != 1 || result.failed != tc.code || !strings.Contains(result.out, tc.want) {
				t.Fatalf("exit %d failed=%q, want %s %q\n%s", result.code, result.failed, tc.code, tc.want, result.out)
			}
			if r.server.downloads() != downloads || r.exists(filepath.Join(r.root, "apt.log")) || r.current() != "" {
				t.Fatalf("the refusal downloaded or installed something\n%s", result.out)
			}
			if tc.code != "busy" && (r.exists(r.lib) || r.exists(r.state)) {
				t.Fatalf("the refusal left the feature's directories\n%s", result.out)
			}
			if tc.code == "settings" && r.read(filepath.Join(r.etc, "codex/config.toml")) != "analytics = { enabled = true }\n" {
				t.Fatal("an unmergeable settings file was changed")
			}
		})
	}
}

func TestAgentInstallMissingPackages(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	if result := r.run(r.input(e, agents.Install), "TEST_INSTALLED="); result.code != 0 {
		t.Fatalf("%s", result.out)
	}
	if apt := r.read(filepath.Join(r.root, "apt.log")); !strings.Contains(apt, "update\n") || !strings.Contains(apt, "install -y --no-install-recommends bubblewrap\n") {
		t.Fatalf("apt requests:\n%s", apt)
	}
}

func TestAgentInstallRejectsMalformedInput(t *testing.T) {
	r := newAgentRig(t)
	e := r.entry("0.171.0", r.client("0.171.0"))
	install := r.input(e, agents.Install)
	remove := r.input(e, agents.Remove)
	for name, input := range map[string]string{
		"id":          strings.Replace(install, "AGENT_ID=codex\n", "AGENT_ID='../x'\n", 1),
		"entry":       strings.Replace(install, "AGENT_ENTRY="+agentEntryName+"\n", "AGENT_ENTRY='../"+agentEntryName+"'\n", 1),
		"url":         strings.Replace(install, "AGENT_URL=https://", "AGENT_URL=http://", 1),
		"env":         strings.Replace(install, "AGENT_ENV=CODEX_TEST_SWITCH=off\n", "AGENT_ENV='LD_PRELOAD=/tmp/x.so'\n", 1),
		"settings":    strings.Replace(install, "AGENT_SETTINGS='", "AGENT_SETTINGS='{\"file\":", 1),
		"remove-data": strings.Replace(remove, "AGENT_REMOVE_DATA=''\n", "AGENT_REMOVE_DATA='~/../etc'\n", 1),
		"no-digest":   strings.Replace(install, "AGENT_SHA256="+e.Artifacts[agents.ARM64].SHA256+"\n", "AGENT_SHA256=''\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			r := r.with(t)
			if input == install || input == remove {
				t.Fatal("the test did not change the input")
			}
			result := r.run(input)
			if result.code != 2 || result.failed != "input" {
				t.Fatalf("exit %d failed=%q\n%s", result.code, result.failed, result.out)
			}
			r.absent(result)
			if r.server.downloads() != 0 {
				t.Fatal("downloaded")
			}
		})
	}
}

func TestAgentInstallInUse(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	process := filepath.Join(r.proc, "4242")
	if err := os.MkdirAll(process, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.lib, "codex/0.171.0", agentEntryName), filepath.Join(process, "exe")); err != nil {
		t.Fatal(err)
	}
	body := r.client("0.172.0")
	newer := r.entry("0.172.0", body)
	r.server.publish(newer, body)
	if result := r.run(r.input(newer, agents.Update)); result.failed != "in-use" || !strings.Contains(result.out, "请先结束正在运行的 Codex 终端") {
		t.Fatalf("update while running: %q\n%s", result.failed, result.out)
	}
	if result := r.run(r.input(e, agents.Remove)); result.failed != "in-use" || r.current() != "0.171.0" {
		t.Fatalf("remove while running: %q\n%s", result.failed, result.out)
	}
	if result := r.run(r.input(e, agents.Install)); result.code != 0 || !strings.Contains(result.out, "已是最新") {
		t.Fatalf("an up-to-date check changes nothing and needs no exclusive use:\n%s", result.out)
	}
	if err := os.RemoveAll(process); err != nil {
		t.Fatal(err)
	}
	if result := r.run(r.input(newer, agents.Update)); result.code != 0 || r.current() != "0.172.0" {
		t.Fatalf("%s", result.out)
	}
}

func TestAgentRemove(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	r.write(filepath.Join(r.home, ".codex/auth.json"), "{}", 0o600)
	result := r.run(r.input(e, agents.Remove))
	if result.code != 0 || !result.removed {
		t.Fatalf("exit %d\n%s", result.code, result.out)
	}
	r.absent(result)
	if !r.exists(filepath.Join(r.home, ".codex/auth.json")) {
		t.Fatal("remove without data deleted the client's data")
	}
	again := r.run(r.input(e, agents.Remove))
	if again.code != 0 || !again.removed {
		t.Fatalf("removing what is not installed: %s", again.out)
	}
	r.absent(again)
}

func TestAgentRemoveWithData(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	outside := filepath.Join(r.root, "outside")
	for _, path := range []string{".codex/auth.json", ".local/share/cage/codex-accounts/a/auth.json", ".local/share/cage/other/keep"} {
		r.write(filepath.Join(r.home, path), "x", 0o600)
	}
	r.write(filepath.Join(outside, "sessions/keep"), "x", 0o600)
	if err := os.Symlink(filepath.Join(outside, "sessions"), filepath.Join(r.home, ".local/share/cage/codex-sessions")); err != nil {
		t.Fatal(err)
	}
	result := r.run(r.input(e, agents.Remove, "~/.codex", "~/.local/share/cage/codex-accounts", "~/.local/share/cage/codex-sessions"))
	if result.code != 0 || !result.removed {
		t.Fatalf("exit %d\n%s", result.code, result.out)
	}
	r.absent(result)
	for path, want := range map[string]bool{
		filepath.Join(r.home, ".codex"): false, filepath.Join(r.home, ".local/share/cage/codex-accounts"): false,
		filepath.Join(r.home, ".local/share/cage/codex-sessions"): false, filepath.Join(r.home, ".local/share/cage/other/keep"): true,
		filepath.Join(outside, "sessions/keep"): true,
	} {
		if r.exists(path) != want {
			t.Errorf("%s exists=%v, want %v", path, !want, want)
		}
	}
	// A link above a data directory that leaves the home: nothing is deleted through it.
	if err := os.RemoveAll(filepath.Join(r.home, ".local")); err != nil {
		t.Fatal(err)
	}
	r.write(filepath.Join(outside, "local/share/cage/codex-accounts/keep"), "x", 0o600)
	if err := os.Symlink(filepath.Join(outside, "local"), filepath.Join(r.home, ".local")); err != nil {
		t.Fatal(err)
	}
	result = r.run(r.input(e, agents.Remove, "~/.local/share/cage/codex-accounts"))
	if result.code != 0 || !r.exists(filepath.Join(outside, "local/share/cage/codex-accounts/keep")) || !strings.Contains(result.out, "位于用户目录之外") {
		t.Fatalf("deleted through a link that leaves the home:\n%s", result.out)
	}
}

func TestAgentRemoveKeepsSettingsItDidNotWrite(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	config := filepath.Join(r.etc, "codex/config.toml")
	r.write(config, r.read(config)+"model = \"mine\"\n", 0o644)
	result := r.run(r.input(e, agents.Remove))
	if result.code != 0 || !r.exists(config) || !strings.Contains(result.out, "已保留") {
		t.Fatalf("a changed settings file was removed:\n%s", result.out)
	}
	// A file the person had before the install keeps its keys and stays.
	r.write(config, "# mine\nmodel = \"o3\"\ncheck_for_update_on_startup = true\n\n[analytics]\nenabled = true\nother = 1\n\n[profiles.x]\nmodel = \"y\"\n", 0o640)
	r.install("0.171.0")
	merged := r.read(config)
	if merged != "# mine\nmodel = \"o3\"\ncheck_for_update_on_startup = false\n\n[analytics]\nenabled = false\nother = 1\n\n[profiles.x]\nmodel = \"y\"\n\n[feedback]\nenabled = false\n" {
		t.Fatalf("merged:\n%s", merged)
	}
	if info, _ := os.Stat(config); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", info.Mode())
	}
	if result := r.run(r.input(e, agents.Remove)); result.code != 0 || r.read(config) != merged {
		t.Fatalf("remove deleted a settings file Cage did not create:\n%s", result.out)
	}
}

func TestAgentInstallSettingsWithoutTOMLParser(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	noTOML := [2]string{"    import tomllib\n", "    import tomllib_unavailable_in_this_test as tomllib\n"}
	// A new file is written whole, so it needs no parser.
	if result := r.runScript(r.compose(r.input(e, agents.Install), noTOML)); result.code != 0 || !r.exists(filepath.Join(r.etc, "codex/config.toml")) {
		t.Fatalf("%s", result.out)
	}
	// An existing one is never merged blind.
	r.run(r.input(e, agents.Remove))
	r.write(filepath.Join(r.etc, "codex/config.toml"), "model = \"o3\"\n", 0o644)
	result := r.runScript(r.compose(r.input(e, agents.Install), noTOML))
	if result.failed != "settings" || !strings.Contains(result.out, "cannot read TOML") || r.read(filepath.Join(r.etc, "codex/config.toml")) != "model = \"o3\"\n" {
		t.Fatalf("failed=%q\n%s", result.failed, result.out)
	}
}

func TestAgentInstallMergesJSONSettings(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	e.Settings = append(e.Settings, agents.Setting{File: "~/.codex/cage-test.json", Format: "json", Set: map[string]any{"ui.theme": "dark", "telemetry": false}})
	r.server.publish(e, body)
	path := filepath.Join(r.home, ".codex/cage-test.json")
	r.write(path, "{\"keep\": 1, \"ui\": {\"font\": \"serif\"}}\n", 0o640)
	if result := r.run(r.input(e, agents.Install)); result.code != 0 {
		t.Fatalf("%s", result.out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(r.read(path)), &got); err != nil {
		t.Fatal(err)
	}
	ui, _ := got["ui"].(map[string]any)
	if got["keep"] != float64(1) || got["telemetry"] != false || ui["font"] != "serif" || ui["theme"] != "dark" {
		t.Fatalf("merged %v", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", info.Mode())
	}
	// Home settings stay with the client's data on remove.
	if result := r.run(r.input(e, agents.Remove)); result.code != 0 || !r.exists(path) {
		t.Fatalf("%s", result.out)
	}
}

func TestAgentInstallRepairsATamperedInstall(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	receiptPath := filepath.Join(r.state, "codex.json")
	good := r.receipt()
	for _, tc := range []struct {
		name   string
		tamper func()
	}{
		{"receipt-digest", func() {
			receipt := r.receipt()
			receipt["entry_sha256"] = strings.Repeat("0", 64)
			data, _ := json.Marshal(receipt)
			r.write(receiptPath, string(data), 0o644)
		}},
		{"receipt-garbage", func() { r.write(receiptPath, "{\"schema\": 1, \"id\": ", 0o644) }},
		{"receipt-symlink", func() {
			_ = os.Remove(receiptPath)
			r.write(filepath.Join(r.root, "elsewhere.json"), r.read(filepath.Join(r.root, "ca.pem")), 0o644)
			if err := os.Symlink(filepath.Join(r.root, "elsewhere.json"), receiptPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"program-changed", func() {
			binary := filepath.Join(r.lib, "codex/0.171.0", agentEntryName)
			r.write(binary, r.read(binary)+"# self-updated\n", 0o755)
		}},
		{"extra-file", func() { r.write(filepath.Join(r.lib, "codex/0.171.0/planted"), "x", 0o755) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := r.with(t)
			tc.tamper()
			downloads := r.server.downloads()
			result := r.run(r.input(e, agents.Install))
			if result.code != 0 || result.done != "0.171.0" || !strings.Contains(result.out, "不一致") || r.server.downloads() != downloads+1 {
				t.Fatalf("not repaired: exit %d downloads %d→%d\n%s", result.code, downloads, r.server.downloads(), result.out)
			}
			if info, err := os.Lstat(receiptPath); err != nil || !info.Mode().IsRegular() {
				t.Fatal("the receipt is not a regular file again")
			}
			receipt := r.receipt()
			for _, key := range []string{"entry_sha256", "tree_sha256", "launcher_sha256"} {
				if receipt[key] != good[key] {
					t.Errorf("%s = %v, want %v", key, receipt[key], good[key])
				}
			}
			if r.exists(filepath.Join(r.lib, "codex/0.171.0/planted")) || r.read(filepath.Join(r.lib, "codex/0.171.0", agentEntryName)) != r.clientBody("0.171.0") {
				t.Fatal("the repaired tree is not the pinned one")
			}
		})
	}
	// A receipt can claim any file; remove deletes only the catalog's.
	sentinel := filepath.Join(r.etc, "passwd")
	r.write(sentinel, "root:x:0:0\n", 0o644)
	sum := sha256.Sum256([]byte("root:x:0:0\n"))
	receipt := r.receipt()
	receipt["settings"] = append(receipt["settings"].([]any), map[string]any{"file": "/etc/passwd", "sha256": hex.EncodeToString(sum[:]), "created": true, "dir_created": true})
	data, _ := json.Marshal(receipt)
	r.write(receiptPath, string(data), 0o644)
	if result := r.run(r.input(e, agents.Remove)); result.code != 0 || !r.exists(sentinel) {
		t.Fatalf("remove followed a path from the receipt:\n%s", result.out)
	}
}

func TestAgentInstallClearsAStaleStage(t *testing.T) {
	r := newAgentRig(t)
	r.write(filepath.Join(r.lib, ".stage-abc123/tree", agentEntryName), "partial", 0o755)
	r.install("0.171.0")
	if len(r.stages()) != 0 {
		t.Fatalf("stale stage kept: %v", r.stages())
	}
}
