//go:build !windows

package workbench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The system ssh takes its home from the user database, not from $HOME:
// any run of it without -F reads the owner's real ~/.ssh/config, known
// hosts, agent and control sockets. So every real ssh these tests run, and
// every one a program they start runs (a Herdr core, scp, sftp, rsync), is
// a shim first: sshExecutable() is the shim, the shim's folder comes first
// on PATH (here and in each environment a test builds), and scp and sftp get
// it with -S. The shim logs each call and runs the real ssh with
//
//	-F <its own empty configuration>        (the caller's -F, given later, wins)
//	-o UserKnownHostsFile=<temporary files> (its own and the fixtures')
//	-o GlobalKnownHostsFile=/dev/null
//	-o ControlPath=none
//	-o IdentityAgent=none
//
// unless the command line sets one of these itself to none, /dev/null or
// files inside a temporary folder (a test's own agent, a -S socket); set to
// anything else, the shim refuses to run. An ssh -G only prints a
// configuration: it runs as given, and only with -F.

var testSSH struct {
	dir   string
	mu    sync.Mutex
	known []string
	shims map[string]string // real ssh -> its shim
}

// testSSHShimEnv names the shim's folder for the test binary's helper
// processes, which use the parent's shim.
const testSSHShimEnv = "CAGE_TEST_SSH_SHIM"

// TestMain puts the shim in place before any test runs and removes it after.
func TestMain(m *testing.M) {
	if dir := os.Getenv(testSSHShimEnv); dir != "" {
		testSSH.dir = dir
		sshProgram = testSSHShim("/usr/bin/ssh")
		_ = os.Setenv("PATH", filepath.Dir(sshProgram)+string(os.PathListSeparator)+os.Getenv("PATH"))
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "cage-ssh-shim-")
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "config"), nil, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	testSSH.dir = dir
	_ = os.Setenv(testSSHShimEnv, dir)
	sshProgram = testSSHShim("/usr/bin/ssh")
	_ = os.Setenv("PATH", filepath.Dir(sshProgram)+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testSSHShim is the shim for one real ssh, made on first use.
func testSSHShim(real string) string {
	testSSH.mu.Lock()
	defer testSSH.mu.Unlock()
	if shim := testSSH.shims[real]; shim != "" {
		return shim
	}
	folder := filepath.Join(testSSH.dir, "bin"+strings.ReplaceAll(real, "/", "_"))
	shim := filepath.Join(folder, "ssh")
	script := fmt.Sprintf(testSSHShimScript, testSSH.dir, real)
	// A helper process finds its parent's shim in place.
	if data, err := os.ReadFile(shim); err != nil || string(data) != script {
		if err := os.MkdirAll(folder, 0o700); err != nil {
			panic(err)
		}
		if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
			panic(err)
		}
	}
	if testSSH.shims == nil {
		testSSH.shims = map[string]string{}
	}
	testSSH.shims[real] = shim
	return shim
}

const testSSHShimScript = `#!/bin/sh
# The workbench tests' ssh (ssh_shim_test.go): never the owner's ~/.ssh.
shim='%[1]s'
real='%[2]s'
printf '%%s %%s\n' "$real" "$*" >> "$shim/calls.log"
set -f
refuse() { echo "ssh shim: $1" >&2; exit 96; }
# safe: none, /dev/null or files inside a temporary folder.
safe() {
	for p in $(printf '%%s' "$1" | tr -d '"'); do
		case "$p" in
		none|/dev/null|/tmp/*|/private/tmp/*|/var/folders/*|/private/var/folders/*) ;;
		*) return 1 ;;
		esac
	done
}
known= global= control= agent=
option() {
	name=$(printf '%%s' "$1" | sed 's/[ =].*//' | tr 'A-Z' 'a-z')
	value=$(printf '%%s' "$1" | sed 's/^[^ =]*[ =]//')
	case "$name" in
	userknownhostsfile) safe "$value" || refuse "known hosts outside a temporary folder: $value"; known=1 ;;
	globalknownhostsfile) safe "$value" || refuse "global known hosts outside a temporary folder: $value"; global=1 ;;
	controlpath) safe "$value" || refuse "a control socket outside a temporary folder: $value"; control=1 ;;
	identityagent) safe "$value" || refuse "an agent outside a temporary folder: $value"; agent=1 ;;
	esac
}
g= f= prev=
for a in "$@"; do
	case "$prev" in
	-S) safe "$a" || refuse "a control socket outside a temporary folder: $a"; control=1 ;;
	-o) option "$a" ;;
	esac
	case "$a" in
	-G) g=1 ;;
	-F*) f=1 ;;
	-o?*) option "${a#-o}" ;;
	esac
	prev=$a
done
if [ -n "$g" ]; then
	[ -n "$f" ] || refuse "-G without -F"
	exec "$real" "$@"
fi
list="$shim/known_hosts"
if [ -f "$shim/known.list" ]; then
	while IFS= read -r line; do
		[ -n "$line" ] && list="$list $line"
	done < "$shim/known.list"
fi
# What the caller set to something safe stays; the rest is the shim's.
[ -n "$agent" ] || set -- -o IdentityAgent=none "$@"
[ -n "$control" ] || set -- -o ControlPath=none "$@"
[ -n "$global" ] || set -- -o GlobalKnownHostsFile=/dev/null "$@"
[ -n "$known" ] || set -- -o "UserKnownHostsFile=$list" "$@"
exec "$real" -F "$shim/config" "$@"
`

// testSSHKnown adds a test's known_hosts files to the shim's, for as long as
// the test runs. They are temporary files without spaces.
func testSSHKnown(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if strings.ContainsAny(path, " \t\n") || !filepath.IsAbs(path) {
			t.Fatalf("a known_hosts file the shim cannot name: %q", path)
		}
	}
	testSSH.mu.Lock()
	testSSH.known = append(testSSH.known, paths...)
	testSSHWriteKnown()
	testSSH.mu.Unlock()
	t.Cleanup(func() {
		testSSH.mu.Lock()
		defer testSSH.mu.Unlock()
		for _, path := range paths {
			if i := slices.Index(testSSH.known, path); i >= 0 {
				testSSH.known = slices.Delete(testSSH.known, i, i+1)
			}
		}
		testSSHWriteKnown()
	})
}

func testSSHWriteKnown() {
	_ = os.WriteFile(filepath.Join(testSSH.dir, "known.list"), []byte(strings.Join(testSSH.known, "\n")+"\n"), 0o600)
}

// testSSHCalls is every call the shims logged so far.
func testSSHCalls() []string {
	data, _ := os.ReadFile(filepath.Join(testSSH.dir, "calls.log"))
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// testSSHPath is a PATH with the shim first, for an environment a test
// builds itself.
func testSSHPath(rest string) string {
	return "PATH=" + filepath.Dir(sshProgram) + ":" + rest
}

// testSSHCalledSince fails unless the shim logged a call containing want
// after the first n calls.
func testSSHCalledSince(t *testing.T, n int, want string) {
	t.Helper()
	calls := testSSHCalls()
	for _, call := range calls[min(n, len(calls)):] {
		if strings.Contains(call, want) {
			return
		}
	}
	t.Fatalf("the ssh shim logged no call with %q; its last calls: %q", want, calls[max(0, len(calls)-5):])
}

// The shim is the ssh the tests run: sshExecutable() and the first ssh on
// PATH. It logs each call, runs ssh -G only with -F and refuses a control
// socket outside a temporary folder.
func TestSSHShimIsTheSSHTheTestsRun(t *testing.T) {
	if !strings.HasPrefix(sshExecutable(), testSSH.dir+string(filepath.Separator)) {
		t.Fatalf("sshExecutable() is %s", sshExecutable())
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("Host probe\n HostName probe.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := len(testSSHCalls())
	out, err := exec.Command(sshExecutable(), "-G", "-F", config, "probe").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "hostname probe.invalid") {
		t.Fatalf("-G through the shim: %v %s", err, out)
	}
	testSSHCalledSince(t, n, "/usr/bin/ssh -G -F "+config+" probe")
	if out, err := exec.Command(sshExecutable(), "-G", "probe").CombinedOutput(); err == nil || !strings.Contains(string(out), "-G without -F") {
		t.Fatalf("-G without -F: %v %s", err, out)
	}
	for _, args := range [][]string{{"-S", "/Users/someone/.ssh/cm"}, {"-o", "ControlPath=~/.ssh/cm-%r"}, {"-oUserKnownHostsFile=/etc/ssh/known"}, {"-o", "IdentityAgent=SSH_AUTH_SOCK"}} {
		if out, err := exec.Command(sshExecutable(), append(args, "probe")...).CombinedOutput(); err == nil || !strings.Contains(string(out), "outside a temporary folder") {
			t.Fatalf("%q: %v %s", args, err, out)
		}
	}
	found, err := exec.Command("/bin/sh", "-c", "command -v ssh").Output()
	if err != nil || strings.TrimSpace(string(found)) != sshExecutable() {
		t.Fatalf("ssh on PATH: %q %v", found, err)
	}
	// What the shim hands the real ssh, seen through echo in its place.
	echo := testSSHShim("/bin/echo")
	out, err = exec.Command(echo, "-F", config, "probe", "true").Output()
	want := "-F " + filepath.Join(testSSH.dir, "config") + " -o UserKnownHostsFile=" + filepath.Join(testSSH.dir, "known_hosts")
	if err != nil || !strings.HasPrefix(string(out), want) || !strings.HasSuffix(string(out), " -o GlobalKnownHostsFile=/dev/null -o ControlPath=none -o IdentityAgent=none -F "+config+" probe true\n") {
		t.Fatalf("the shim ran %q (%v)", out, err)
	}
	// A test's own agent and control socket, inside a temporary folder, stay.
	agent, control := filepath.Join(dir, "agent"), filepath.Join(dir, "cm")
	out, err = exec.Command(echo, "-o", "IdentityAgent="+agent, "-S", control, "probe").Output()
	if err != nil || strings.Contains(string(out), "IdentityAgent=none") || strings.Contains(string(out), "ControlPath=none") || !strings.HasSuffix(string(out), "-o IdentityAgent="+agent+" -S "+control+" probe\n") {
		t.Fatalf("the shim ran %q (%v)", out, err)
	}
}
