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
// anything else, the shim refuses to run. A configuration (-F), key (-i,
// IdentityFile) or certificate outside a temporary folder is refused too,
// and so is a configuration whose Include names a file outside one.
// Before the real run the shim asks ssh -G what the command line would use:
// ssh's default keys in the user database's home (nothing named a key for
// the host) give way to a key that does not exist, and any other file
// outside a temporary folder is refused. An ssh -G only prints a
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
	// CAGE_TEST_SSH_LOG keeps a copy of every call the shims logged.
	if keep := os.Getenv("CAGE_TEST_SSH_LOG"); keep != "" {
		data, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		_ = os.WriteFile(keep, data, 0o600)
	}
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
known= global= control= agent= identity=
# config: a configuration file inside a temporary folder whose Include lines
# stay inside one too (ssh reads ~ as the user database's home).
config() {
	one "$1" || refuse "a configuration outside a temporary folder: $1"
	includes=$(sed -n 's/^[[:space:]]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][[:space:]=][[:space:]=]*//p' "$1" 2>/dev/null)
	while IFS= read -r include; do
		[ -n "$include" ] || continue
		case "$include" in
		\"*) one "$include" ;;
		*) safe "$include" ;;
		esac || refuse "an Include outside a temporary folder: $include"
	done <<EOF
$includes
EOF
}
option() {
	name=$(printf '%%s' "$1" | sed 's/[ =].*//' | tr 'A-Z' 'a-z')
	value=$(printf '%%s' "$1" | sed 's/^[^ =]*[ =]//')
	case "$name" in
	userknownhostsfile) safe "$value" || refuse "known hosts outside a temporary folder: $value"; known=1 ;;
	globalknownhostsfile) safe "$value" || refuse "global known hosts outside a temporary folder: $value"; global=1 ;;
	controlpath) one "$value" || refuse "a control socket outside a temporary folder: $value"; control=1 ;;
	identityagent) one "$value" || refuse "an agent outside a temporary folder: $value"; agent=1 ;;
	identityfile) one "$value" || refuse "a key outside a temporary folder: $value"; identity=1 ;;
	certificatefile) one "$value" || refuse "a certificate outside a temporary folder: $value" ;;
	esac
}
g= f= prev=
for a in "$@"; do
	case "$prev" in
	-S) one "$a" || refuse "a control socket outside a temporary folder: $a"; control=1 ;;
	-o) option "$a" ;;
	-F) config "$a" ;;
	-i) one "$a" || refuse "a key outside a temporary folder: $a"; identity=1 ;;
	esac
	case "$a" in
	-G) g=1 ;;
	-F) f=1 ;;
	-F?*) f=1; config "${a#-F}" ;;
	-i?*) one "${a#-i}" || refuse "a key outside a temporary folder: ${a#-i}"; identity=1 ;;
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
# What ssh would use, as ssh -G reads this command line and its
# configuration: the first file outside a temporary folder, if any.
unsafe() {
	effective=$("$real" -G -F "$shim/config" "$@" 2>/dev/null) || { echo "a command line ssh -G refuses"; return; }
	while read -r key value; do
		case "$key" in
		userknownhostsfile|globalknownhostsfile)
			safe "$value" || { printf '%%s %%s\n' "$key" "$value"; return; } ;;
		identityfile|certificatefile|controlpath|identityagent|revokedhostkeys)
			one "$value" || { printf '%%s %%s\n' "$key" "$value"; return; } ;;
		esac
	done <<EOF
$effective
EOF
}
bad=$(unsafe "$@")
# With no key named for the host, ssh tries its default keys in the home
# of the user database, the owner's: a key that does not exist takes
# their place.
case "$bad" in
"identityfile ~/.ssh/id_"*)
	if [ -z "$identity" ]; then
		set -- -o "IdentityFile=$shim/no-identity" "$@"
		bad=$(unsafe "$@")
	fi ;;
esac
[ -z "$bad" ] || refuse "ssh would use $bad"
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

// The shim's other guards, each a way the owner's ~/.ssh could still be read:
// a configuration, key or path given outside a temporary folder (also one
// that climbs out of it with ..), and the default keys ssh tries from the
// home in the user database when nothing names a key for the host. What
// ssh would use is read from ssh -G itself, with the real ssh standing in
// for -G and echo for the run.
func TestSSHShimKeepsToTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-ssh")
	if err := os.WriteFile(real, []byte("#!/bin/sh\ncase \"$1\" in -G) exec /usr/bin/ssh \"$@\" ;; esac\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := testSSHShim(real)
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
		{"-o", "ControlPath=" + dir + "/../../../Users/someone/.ssh/cm", "-F", keyed, "probe"},
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
	// With no -F at all, the shim's own configuration names no key either.
	if out, err := exec.Command(shim, "probe", "true").Output(); err != nil || !strings.Contains(string(out), noKey) {
		t.Errorf("no -F: the shim ran %q (%v)", out, err)
	}
}
