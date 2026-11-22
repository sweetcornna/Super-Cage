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
// hosts, default keys, agent and control sockets. So every real ssh these
// tests run, and every one a program they start runs (a Herdr core, scp,
// sftp, rsync), is a shim first: sshExecutable() is the shim, the shim's
// folder comes first on PATH (here and in each environment a test builds),
// and scp and sftp get it with -S. The shim logs each call (and each
// refusal), and:
//
//   - refuses a configuration (-F, and the files its Include lines name),
//     key, certificate, known hosts file, control socket or agent the
//     command line names outside a temporary folder (none and /dev/null are
//     fine, and so is an Include of the system's /etc/ssh);
//   - runs the real ssh with -F <its own empty configuration> first (the
//     caller's -F, given later, wins);
//   - asks ssh -G what that command line would use, and sets on the command
//     line what would come from outside a temporary folder: its own known
//     hosts (with those a test named, testSSHKnown), GlobalKnownHostsFile
//     /dev/null, ControlPath none, IdentityAgent none (no agent named means
//     $SSH_AUTH_SOCK's), and for ssh's default keys (nothing names a key, or
//     -i names one that is not there) a key that does not exist; what the
//     command line or the configuration sets inside a temporary folder stays;
//   - asks ssh -G again and refuses anything still outside one.
//
// An ssh -G only prints a configuration: it runs as given, and only with -F.
// What ssh -G itself refuses (a configuration gone), the shim answers as ssh
// does, with ssh's words and exit status 255.

var testSSH struct {
	dir     string
	control string // short folder for control sockets
	mu      sync.Mutex
	known   []string
	shims   map[string]string // real ssh -> its shim
}

// testSSHShimEnv names the shim's folder for the test binary's helper
// processes, which use the parent's shim.
const testSSHShimEnv = "CAGE_TEST_SSH_SHIM"

// testSSHControlEnv names the shim's control socket folder the same way.
const testSSHControlEnv = "CAGE_TEST_SSH_CONTROL"

// TestMain puts the shim in place before any test runs and removes it after.
func TestMain(m *testing.M) {
	if dir := os.Getenv(testSSHShimEnv); dir != "" {
		testSSH.dir, testSSH.control = dir, os.Getenv(testSSHControlEnv)
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
	control, err := os.MkdirTemp("/tmp", "csc-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	testSSH.dir, testSSH.control = dir, control
	_ = os.Setenv(testSSHShimEnv, dir)
	_ = os.Setenv(testSSHControlEnv, control)
	sshProgram = testSSHShim("/usr/bin/ssh")
	_ = os.Setenv("PATH", filepath.Dir(sshProgram)+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	// CAGE_TEST_SSH_LOG keeps a copy of every call the shims logged.
	if keep := os.Getenv("CAGE_TEST_SSH_LOG"); keep != "" {
		data, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		_ = os.WriteFile(keep, data, 0o600)
	}
	_ = os.RemoveAll(dir)
	_ = os.RemoveAll(control)
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
	script := fmt.Sprintf(testSSHShimScript, testSSH.dir, real, testSSH.control)
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
control='%[3]s'
printf '%%s %%s\n' "$real" "$*" >> "$shim/calls.log"
set -f
refuse() {
	echo "ssh shim: $1" >&2
	printf 'ssh shim refused: %%s\n' "$1" >> "$shim/calls.log"
	exit 96
}
# one: none, /dev/null or a file inside a temporary folder, never climbing
# out of it; safe: each of a list of them.
one() {
	case "$(printf '%%s' "$1" | tr -d '"')" in
	*/../*|*/..) return 1 ;;
	none|/dev/null|/tmp/*|/private/tmp/*|/var/folders/*|/private/var/folders/*) return 0 ;;
	esac
	return 1
}
safe() {
	for p in $(printf '%%s' "$1" | tr -d '"'); do
		one "$p" || return 1
	done
}
# config: a configuration file inside a temporary folder whose Include lines
# stay inside one too (ssh reads ~ as the user database's home, ${HOME} as
# the variable) or name the system's own files.
config() {
	one "$1" || refuse "a configuration outside a temporary folder: $1"
	includes=$(sed -n 's/^[[:space:]]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][[:space:]=][[:space:]=]*//p' "$1" 2>/dev/null |
		awk '{ h = ENVIRON["HOME"]; while ((i = index($0, "${HOME}")) > 0) $0 = substr($0, 1, i - 1) h substr($0, i + 7); print }')
	while IFS= read -r include; do
		case "$include" in
		""|/etc/ssh/*|\"/etc/ssh/*) continue ;;
		\"*) one "$include" ;;
		*) safe "$include" ;;
		esac || refuse "an Include outside a temporary folder: $include"
	done <<EOF
$includes
EOF
}
option() {
	opt=$1
	name=$(printf '%%s' "$1" | sed 's/[ =].*//' | tr 'A-Z' 'a-z')
	value=$(printf '%%s' "$1" | sed 's/^[^ =]*[ =]//')
	case "$name" in
	userknownhostsfile) safe "$value" || refuse "known hosts outside a temporary folder: $value" ;;
	globalknownhostsfile) safe "$value" || refuse "global known hosts outside a temporary folder: $value" ;;
	controlpath) one "$value" || opt="ControlPath=$control/%%C" ;;
	identityagent) one "$value" || refuse "an agent outside a temporary folder: $value" ;;
	identityfile) one "$value" || refuse "a key outside a temporary folder: $value" ;;
	certificatefile) one "$value" || refuse "a certificate outside a temporary folder: $value" ;;
	esac
}
# Each argument is checked; a control socket outside a temporary folder is
# moved into the tests' own short folder (a socket path has ~100 bytes).
g= f= prev= n=$#
for a in "$@"; do
	b=$a
	case "$prev" in
	-S) one "$a" || b="$control/%%C" ;;
	-o) option "$a"; b=$opt ;;
	-F) config "$a" ;;
	-i) one "$a" || refuse "a key outside a temporary folder: $a" ;;
	*)
		case "$a" in
		-G) g=1 ;;
		-F) f=1 ;;
		-F?*) f=1; config "${a#-F}" ;;
		-i?*) one "${a#-i}" || refuse "a key outside a temporary folder: ${a#-i}" ;;
		-o?*) option "${a#-o}"; b="-o$opt" ;;
		esac ;;
	esac
	set -- "$@" "$b"
	prev=$a
done
shift "$n"
# Without -F of the caller's, the configuration is the shim's own empty one,
# or the one a test names in CAGE_TEST_SSH_CONFIG (an owner's ~/.ssh/config
# stand-in).
cfg="$shim/config"
if [ -n "${CAGE_TEST_SSH_CONFIG:-}" ]; then
	config "$CAGE_TEST_SSH_CONFIG"
	cfg=$CAGE_TEST_SSH_CONFIG
fi
if [ -n "$g" ]; then
	[ -n "$f" ] || refuse "-G without -F"
	exec "$real" "$@"
fi
# What ssh would use for this command line and its configuration, from
# ssh -G, and one value of it.
look() {
	effective=$("$real" -G -F "$cfg" "$@" 2>&1) && return
	# What ssh itself refuses (a configuration gone, say) it says as ssh.
	printf '%%s\n' "$effective" >&2
	printf 'ssh shim: ssh -G failed\n' >> "$shim/calls.log"
	exit 255
}
value() {
	printf '%%s\n' "$effective" | sed -n "s/^$1 //p"
}
look "$@"
# What ssh would take from outside a temporary folder the shim sets itself:
# known hosts (its own and those the tests named), no global ones, no
# control socket, no agent (none named is $SSH_AUTH_SOCK's), and for ssh's
# default keys in the user database's home (nothing names a key, or -i names
# one that is not there, which ssh drops), a key that does not exist. What
# the command line or the configuration set inside one stays.
changed=
list="$shim/known_hosts"
if [ -f "$shim/known.list" ]; then
	while IFS= read -r line; do
		[ -n "$line" ] && list="$list $line"
	done < "$shim/known.list"
fi
v=$(value userknownhostsfile)
[ -n "$v" ] && safe "$v" || { set -- -o "UserKnownHostsFile=$list" "$@"; changed=1; }
v=$(value globalknownhostsfile)
[ -n "$v" ] && safe "$v" || { set -- -o GlobalKnownHostsFile=/dev/null "$@"; changed=1; }
v=$(value controlpath)
[ -z "$v" ] || one "$v" || { set -- -o "ControlPath=$control/%%C" "$@"; changed=1; }
v=$(value identityagent)
[ -n "$v" ] && one "$v" || { set -- -o IdentityAgent=none "$@"; changed=1; }
case "$(value identityfile)" in
"~/.ssh/id_"*) set -- -o "IdentityFile=$shim/no-identity" "$@"; changed=1 ;;
esac
[ -z "$changed" ] || look "$@"
while read -r key v; do
	case "$key" in
	userknownhostsfile|globalknownhostsfile) safe "$v" || refuse "ssh would use $key $v" ;;
	identityfile|certificatefile|controlpath|identityagent|revokedhostkeys) one "$v" || refuse "ssh would use $key $v" ;;
	esac
done <<EOF
$effective
EOF
[ -n "$(value identityagent)" ] || refuse "ssh would use the agent of SSH_AUTH_SOCK"
exec "$real" -F "$cfg" "$@"
`

// testSSHEcho is a shim whose ssh prints what it was given, except that
// ssh -G is the real one: what the shim hands ssh, seen without a server.
func testSSHEcho(t *testing.T) string {
	t.Helper()
	real := filepath.Join(t.TempDir(), "echo-ssh")
	if err := os.WriteFile(real, []byte("#!/bin/sh\ncase \"$1\" in -G) exec /usr/bin/ssh \"$@\" ;; esac\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return testSSHShim(real)
}

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
	for _, args := range [][]string{{"-oUserKnownHostsFile=/etc/ssh/known"}, {"-o", "IdentityAgent=SSH_AUTH_SOCK"}} {
		if out, err := exec.Command(sshExecutable(), append(args, "probe")...).CombinedOutput(); err == nil || !strings.Contains(string(out), "outside a temporary folder") {
			t.Fatalf("%q: %v %s", args, err, out)
		}
	}
	found, err := exec.Command("/bin/sh", "-c", "command -v ssh").Output()
	if err != nil || strings.TrimSpace(string(found)) != sshExecutable() {
		t.Fatalf("ssh on PATH: %q %v", found, err)
	}
	// What the shim hands the real ssh, seen through echo in its place.
	echo := testSSHEcho(t)
	out, err = exec.Command(echo, "-F", config, "probe", "true").Output()
	for _, want := range []string{" -o UserKnownHostsFile=" + filepath.Join(testSSH.dir, "known_hosts"), " -o GlobalKnownHostsFile=/dev/null ", " -o IdentityAgent=none ", " -o IdentityFile=" + filepath.Join(testSSH.dir, "no-identity") + " "} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the shim ran %q (%v), without %q", out, err, want)
		}
	}
	if err != nil || !strings.HasPrefix(string(out), "-F "+filepath.Join(testSSH.dir, "config")+" -o ") || strings.Contains(string(out), "ControlPath") || !strings.HasSuffix(string(out), " -F "+config+" probe true\n") {
		t.Fatalf("the shim ran %q (%v)", out, err)
	}
	// A test's own agent and control socket, inside a temporary folder, stay.
	agent, control := filepath.Join(dir, "agent"), filepath.Join(dir, "cm")
	out, err = exec.Command(echo, "-o", "IdentityAgent="+agent, "-S", control, "probe").Output()
	if err != nil || strings.Contains(string(out), "IdentityAgent=none") || strings.Contains(string(out), "ControlPath=none") || !strings.HasSuffix(string(out), "-o IdentityAgent="+agent+" -S "+control+" probe\n") {
		t.Fatalf("the shim ran %q (%v)", out, err)
	}
}

// The shim's other guards, each a way the owner's ~/.ssh could still be read:
// a configuration, key or path given outside a temporary folder (also one
// that climbs out of it with ..), and the default keys ssh tries from the
// home in the user database when nothing names a key for the host. What
// ssh would use is read from ssh -G itself, with the real ssh standing in
// for -G and echo for the run.
func TestSSHShimKeepsToTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	shim := testSSHEcho(t)
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	key := write("key", "")
	keyed := write("keyed", "Host probe\n HostName probe.invalid\n IdentityFile "+key+"\n")
	bare := write("bare", "Host probe\n HostName probe.invalid\n")
	elsewhere := write("elsewhere", "Host other\n IdentityFile "+key+"\nHost probe\n HostName probe.invalid\n")
	owners := write("owners", "Host probe\n IdentityFile ~/.ssh/id_ed25519\n")
	includes := write("includes", "Include \""+dir+"/*.conf\"\nInclude ~/.ssh/config\nHost probe\n HostName probe.invalid\n")
	for _, args := range [][]string{
		{"-F", "/Users/someone/.ssh/config", "probe"},
		{"-F/Users/someone/.ssh/config", "-G", "probe"},
		{"-i", "/Users/someone/.ssh/id_ed25519", "-F", keyed, "probe"},
		{"-o", "IdentityFile=~/.ssh/id_rsa", "-F", keyed, "probe"},
		{"-o", "CertificateFile=/Users/someone/.ssh/id_rsa-cert.pub", "-F", keyed, "probe"},
		{"-F", owners, "probe"},
		{"-F", includes, "probe"},
		{"-G", "-F", includes, "probe"},
	} {
		if out, err := exec.Command(shim, args...).CombinedOutput(); err == nil || !strings.Contains(string(out), "ssh shim:") {
			t.Errorf("%q ran: %v %s", args, err, out)
		}
	}
	noKey := "-o IdentityFile=" + filepath.Join(testSSH.dir, "no-identity") + " "
	for config, wantNoKey := range map[string]bool{keyed: false, bare: true, elsewhere: true} {
		out, err := exec.Command(shim, "-F", config, "probe", "true").Output()
		if err != nil || strings.Contains(string(out), noKey) != wantNoKey || !strings.HasSuffix(string(out), "-F "+config+" probe true\n") {
			t.Errorf("%s: the shim ran %q (%v)", filepath.Base(config), out, err)
		}
	}
	// Files a configuration names inside a temporary folder stay: its own
	// known hosts, an Include through ${HOME} (the test's) and the
	// system's own configuration.
	known := write("known", "")
	own := write("own", "Include \"${HOME}/*.conf\"\nInclude /etc/ssh/ssh_config\nHost probe\n HostName probe.invalid\n IdentityFile "+key+"\n UserKnownHostsFile "+known+"\n GlobalKnownHostsFile /dev/null\n")
	cmd := exec.Command(shim, "-F", own, "probe", "true")
	cmd.Env = append(os.Environ(), "HOME="+dir)
	if out, err := cmd.Output(); err != nil || strings.Contains(string(out), "KnownHostsFile=") || strings.Contains(string(out), "IdentityFile=") || !strings.Contains(string(out), " -o IdentityAgent=none ") {
		t.Errorf("own files: the shim ran %q (%v)", out, err)
	}
	// With no -F at all, the shim's own configuration names no key either;
	// nor does -i with a key that is not there (ssh drops it).
	if out, err := exec.Command(shim, "probe", "true").Output(); err != nil || !strings.Contains(string(out), noKey) {
		t.Errorf("no -F: the shim ran %q (%v)", out, err)
	}
	if out, err := exec.Command(shim, "-F", "/dev/null", "-i", filepath.Join(dir, "missing"), "probe").Output(); err != nil || !strings.Contains(string(out), noKey) {
		t.Errorf("-i missing: the shim ran %q (%v)", out, err)
	}
	if out, err := exec.Command(shim, "-F", "/dev/null", "-i", key, "probe").Output(); err != nil || strings.Contains(string(out), noKey) {
		t.Errorf("-i key: the shim ran %q (%v)", out, err)
	}
	// What ssh -G refuses, ssh's words and exit.
	if out, err := exec.Command(shim, "-F", filepath.Join(dir, "gone"), "probe").CombinedOutput(); err == nil || err.(*exec.ExitError).ExitCode() != 255 || !strings.Contains(string(out), "gone") {
		t.Errorf("a configuration gone: %v %s", err, out)
	}
}

// A control socket the command line or a configuration puts outside a
// temporary folder (a Herdr core's own ControlPath, say) moves into the
// tests' short folder: the system ssh would otherwise make it in the
// owner's home. Without -F of its own, ssh reads CAGE_TEST_SSH_CONFIG when a
// test names one.
func TestSSHShimForcesControlSocketsIntoTheTestsFolder(t *testing.T) {
	dir := t.TempDir()
	shim := testSSHEcho(t)
	forced := "ControlPath=" + testSSH.control + "/%C"
	if !strings.HasPrefix(testSSH.control, "/tmp/") || len(testSSH.control)+1+40+17 > 100 {
		t.Fatalf("control folder %q", testSSH.control)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-o", "ControlMaster=auto", "-o", "ControlPath=~/.ssh/cm-%C", "-o", "ControlPersist=600", "probe"}, "-o " + forced + " -o ControlPersist=600 probe\n"},
		{[]string{"-oControlPath=/Users/someone/.ssh/cm", "probe"}, "-o" + forced + " probe\n"},
		{[]string{"-S", "/Users/someone/.ssh/cm", "probe"}, "-S " + testSSH.control + "/%C probe\n"},
		{[]string{"-o", "ControlPath=" + dir + "/../../../Users/someone/.ssh/cm", "probe"}, "-o " + forced + " probe\n"},
	} {
		out, err := exec.Command(shim, c.args...).Output()
		if err != nil || !strings.HasSuffix(string(out), c.want) || strings.Contains(string(out), ".ssh") {
			t.Errorf("%q: the shim ran %q (%v)", c.args, out, err)
		}
	}
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("Host probe\n ControlMaster auto\n ControlPath ~/.ssh/cm-%r@%h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(shim, "-F", config, "probe").Output(); err != nil || !strings.Contains(string(out), " -o "+forced+" ") {
		t.Errorf("a configuration's control socket: the shim ran %q (%v)", out, err)
	}
	cmd := exec.Command(shim, "probe")
	cmd.Env = append(os.Environ(), "CAGE_TEST_SSH_CONFIG="+config)
	if out, err := cmd.Output(); err != nil || !strings.HasPrefix(string(out), "-F "+config+" ") || !strings.Contains(string(out), " -o "+forced+" ") {
		t.Errorf("CAGE_TEST_SSH_CONFIG: the shim ran %q (%v)", out, err)
	}
	cmd = exec.Command(shim, "probe")
	cmd.Env = append(os.Environ(), "CAGE_TEST_SSH_CONFIG=/Users/someone/.ssh/config")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "outside a temporary folder") {
		t.Errorf("CAGE_TEST_SSH_CONFIG outside: %v %s", err, out)
	}
}
