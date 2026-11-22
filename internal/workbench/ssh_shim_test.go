//go:build !windows

package workbench

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The system ssh takes its home from the user database, not from $HOME:
// any run of it without -F reads the owner's real ~/.ssh/config, known
// hosts, default keys, agent and control sockets. So every real ssh these
// tests run, and every one a program they start runs (scp, sftp, rsync, an
// ssh's ProxyCommand), is a shim first: sshExecutable() is the shim, and the
// shim's folder comes first on PATH (here and in each environment a test
// builds). scp and sftp run /usr/bin/ssh by its path unless -S names another,
// so the shim's folder also holds an scp and an sftp that always give -S the
// shim (TestSSHShimNoTestNamesTheSystemTools keeps tests to them). The shim
// logs each call (and each refusal), and:
//
//   - reads the command line as ssh does (options, the destination, options
//     again until the command or --) and refuses an option that takes a
//     value bundled after another (-TF <path>: ssh reads it as -F);
//   - refuses a configuration (-F, and every file its Include lines name, at
//     every depth), key, certificate, known hosts file or agent the command
//     line names outside a temporary folder (none and /dev/null are fine, and
//     so is an Include of the system's /etc/ssh, whose own Includes are
//     checked the same way);
//   - moves a control socket named outside a temporary folder into the
//     tests' own short folder;
//   - runs the real ssh with -F <its own empty configuration> first (the
//     caller's -F, given later, wins);
//   - asks ssh -G what that command line would use, and sets on the command
//     line what would come from outside a temporary folder: its own known
//     hosts (with those a test named, testSSHKnown), GlobalKnownHostsFile
//     /dev/null, ControlPath in its folder, IdentityAgent none (no agent
//     named means $SSH_AUTH_SOCK's), and for ssh's default keys (nothing
//     names a key, or -i names one that is not there) a key that does not
//     exist; what the command line or the configuration sets inside a
//     temporary folder stays;
//   - asks ssh -G again and refuses anything still outside one, and a
//     ProxyJump or a ProxyCommand that runs ssh (another ssh, found on a PATH
//     that may not start with the shim).
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

// testSSHEnv is what a helper process of the test binary needs, in an
// environment a test builds itself, to use this run's shim.
func testSSHEnv() []string {
	return []string{testSSHShimEnv + "=" + testSSH.dir, testSSHControlEnv + "=" + testSSH.control}
}

// TestMain puts the shim in place before any test runs and removes it after.
func TestMain(m *testing.M) {
	// A helper process uses its parent's shim, given both of its folders.
	if dir, control := os.Getenv(testSSHShimEnv), os.Getenv(testSSHControlEnv); dir != "" && control != "" {
		testSSH.dir, testSSH.control = dir, control
		testSSHInstall()
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
	testSSHInstall()
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

// testSSHInstall makes the shim, and the scp and sftp beside it, and puts
// their folder first on PATH.
func testSSHInstall() {
	sshProgram = testSSHShim("/usr/bin/ssh")
	for _, tool := range []string{"scp", "sftp"} {
		testSSHWrite(testSSHTool(tool), fmt.Sprintf(testSSHToolScript, testSSH.dir, "/usr/bin/"+tool, sshProgram, testSSHToolTakes[tool]))
	}
	_ = os.Setenv("PATH", filepath.Dir(sshProgram)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// testSSHTool is the scp or sftp the tests run: its ssh is the shim.
func testSSHTool(name string) string {
	return filepath.Join(filepath.Dir(sshProgram), name)
}

// testSSHWrite writes a script unless it is there already: a helper process
// finds its parent's in place.
func testSSHWrite(path, script string) {
	if data, err := os.ReadFile(path); err == nil && string(data) == script {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		panic(err)
	}
}

// testSSHShim is the shim for one real ssh, made on first use.
func testSSHShim(real string) string {
	testSSH.mu.Lock()
	defer testSSH.mu.Unlock()
	if shim := testSSH.shims[real]; shim != "" {
		return shim
	}
	shim := filepath.Join(testSSH.dir, "bin"+strings.ReplaceAll(real, "/", "_"), "ssh")
	testSSHWrite(shim, fmt.Sprintf(testSSHShimScript, testSSH.dir, real, testSSH.control, testSSHTakes))
	if testSSH.shims == nil {
		testSSH.shims = map[string]string{}
	}
	testSSH.shims[real] = shim
	return shim
}

// The options of OpenSSH's ssh, scp and sftp that take a value (their
// getopt strings; ssh -<letter> alone says "requires an argument").
const testSSHTakes = "bceilmopwBDEFIJLOPQRSW"

var testSSHToolTakes = map[string]string{"scp": "ciloDFJMPSX", "sftp": "bcilosBDFJPRSX"}

const testSSHShimScript = `#!/bin/sh
# The workbench tests' ssh (ssh_shim_test.go): never the owner's ~/.ssh.
shim='%[1]s'
real='%[2]s'
control='%[3]s'
takes='%[4]s'
printf '%%s %%s\n' "$real" "$*" >> "$shim/calls.log"
set -f
refuse() {
	echo "ssh shim: $1" >&2
	printf 'ssh shim refused: %%s\n' "$1" >> "$shim/calls.log"
	exit 96
}
# one: none, /dev/null or a file inside a temporary folder, never climbing
# out of it; safe: each of a list of them; system: the system's own
# configuration.
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
system() {
	case "$1" in
	*/../*|*/..) return 1 ;;
	/etc/ssh/*|/private/etc/ssh/*) return 0 ;;
	esac
	return 1
}
# config: a configuration file inside a temporary folder; includes: the files
# its Include lines name, and theirs in turn, each inside a temporary folder
# or the system's own (ssh reads ~ as the user database's home, ${HOME} as
# the variable, and follows 16 levels).
config() {
	one "$1" || refuse "a configuration outside a temporary folder: $1"
	includes "$1" 1
}
includes() {
	[ "$2" -le 16 ] || refuse "Includes deeper than ssh follows: $1"
	local patterns pattern file
	patterns=$(sed -n 's/^[[:space:]]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][[:space:]=][[:space:]=]*//p' "$1" 2>/dev/null |
		awk '{ h = ENVIRON["HOME"]; while ((i = index($0, "${HOME}")) > 0) $0 = substr($0, 1, i - 1) h substr($0, i + 7); print }' | tr -d '"')
	for pattern in $patterns; do
		system "$pattern" || one "$pattern" || refuse "an Include outside a temporary folder: $pattern"
		set +f
		for file in $pattern; do
			set -f
			if [ -f "$file" ]; then
				includes "$file" $(($2 + 1))
			fi
		done
		set -f
	done
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
# given: option $1 with the value $2 (its own argument, or the rest of the
# one the option starts); b is what goes on to ssh in its place.
given() {
	case "$1" in
	S) one "$2" || b="$control/%%C" ;;
	o) option "$2"; b=$opt ;;
	F) f=1; config "$2" ;;
	i) one "$2" || refuse "a key outside a temporary folder: $2" ;;
	esac
}
# Each argument is read as ssh reads it: options, the destination, options
# again until the command or --. An option that takes a value comes alone
# or first in its group. A control socket outside a temporary folder moves
# into the tests' own short folder (a socket path has ~100 bytes).
g= f= host= stop= ended= expect= n=$#
for a in "$@"; do
	b=$a
	if [ -n "$expect" ]; then
		given "$expect" "$a"
		expect=
	elif [ -n "$ended" ]; then
		:
	elif [ -n "$stop" ]; then
		[ -z "$host" ] && host=$a || ended=1
	else
		case "$a" in
		--) stop=1 ;;
		-?*)
			rest=${a#-} first=1
			while [ -n "$rest" ]; do
				c=${rest%%"${rest#?}"}
				rest=${rest#?}
				case "$takes" in
				*"$c"*)
					[ -n "$first" ] || refuse "an option with a value bundled after another: $a"
					if [ -n "$rest" ]; then
						given "$c" "$rest"
						[ "$b" = "$a" ] || b="-$c$b"
					else
						expect=$c
					fi
					break ;;
				esac
				[ "$c" != G ] || g=1
				first=
			done ;;
		*) [ -z "$host" ] && host=$a || ended=1 ;;
		esac
	fi
	set -- "$@" "$b"
done
shift "$n"
cfg="$shim/config"
if [ -n "$g" ]; then
	[ -n "$f" ] || refuse "-G without -F"
	exec "$real" "$@"
fi
# What ssh would use for this command line and its configuration, from
# ssh -G, and one setting of it.
look() {
	effective=$("$real" -G -F "$cfg" "$@" 2>&1) && return
	# What ssh itself refuses (a configuration gone, say) it says as ssh.
	printf '%%s\n' "$effective" >&2
	printf 'ssh shim: ssh -G failed\n' >> "$shim/calls.log"
	exit 255
}
setting() {
	printf '%%s\n' "$effective" | sed -n "s/^$1 //p"
}
look "$@"
# What ssh would take from outside a temporary folder the shim sets itself:
# known hosts (its own and those the tests named), no global ones, control
# sockets in its folder, no agent (none named is $SSH_AUTH_SOCK's), and for
# ssh's default keys in the user database's home (nothing names a key, or -i
# names one that is not there, which ssh drops), a key that does not exist.
# What the command line or the configuration set inside one stays.
changed=
list="$shim/known_hosts"
if [ -f "$shim/known.list" ]; then
	while IFS= read -r line; do
		[ -n "$line" ] && list="$list $line"
	done < "$shim/known.list"
fi
v=$(setting userknownhostsfile)
[ -n "$v" ] && safe "$v" || { set -- -o "UserKnownHostsFile=$list" "$@"; changed=1; }
v=$(setting globalknownhostsfile)
[ -n "$v" ] && safe "$v" || { set -- -o GlobalKnownHostsFile=/dev/null "$@"; changed=1; }
v=$(setting controlpath)
[ -z "$v" ] || one "$v" || { set -- -o "ControlPath=$control/%%C" "$@"; changed=1; }
v=$(setting identityagent)
[ -n "$v" ] && one "$v" || { set -- -o IdentityAgent=none "$@"; changed=1; }
case "$(setting identityfile)" in
"~/.ssh/id_"*) set -- -o "IdentityFile=$shim/no-identity" "$@"; changed=1 ;;
esac
[ -z "$changed" ] || look "$@"
while read -r key v; do
	case "$key" in
	userknownhostsfile|globalknownhostsfile) safe "$v" || refuse "ssh would use $key $v" ;;
	identityfile|certificatefile|controlpath|identityagent|revokedhostkeys) one "$v" || refuse "ssh would use $key $v" ;;
	proxyjump) [ "$v" = none ] || refuse "ssh would run another ssh for ProxyJump $v" ;;
	proxycommand)
		case " $v " in
		*[[:space:]\;\|\&\(/]ssh[[:space:]]*) refuse "ssh would run another ssh for ProxyCommand $v" ;;
		esac ;;
	esac
done <<EOF
$effective
EOF
[ -n "$(setting identityagent)" ] || refuse "ssh would use the agent of SSH_AUTH_SOCK"
exec "$real" -F "$cfg" "$@"
`

// testSSHToolScript is the scp or sftp of the tests: they run /usr/bin/ssh by
// its path unless -S names another, so -S is always the shim. A -S of the
// caller's must name the shim too; an option that takes a value comes alone
// or first in its group, as for ssh.
const testSSHToolScript = `#!/bin/sh
# The workbench tests' %[2]s (ssh_shim_test.go): its ssh is the shim.
shim='%[1]s'
real='%[2]s'
ssh='%[3]s'
takes='%[4]s'
printf '%%s %%s\n' "$real" "$*" >> "$shim/calls.log"
set -f
refuse() {
	echo "ssh shim: $1" >&2
	printf 'ssh shim refused: %%s\n' "$1" >> "$shim/calls.log"
	exit 96
}
expect=
for a in "$@"; do
	if [ -n "$expect" ]; then
		[ "$expect" != S ] || [ "$a" = "$ssh" ] || refuse "$real -S $a: the tests' ssh is the shim"
		expect=
		continue
	fi
	case "$a" in
	--) break ;;
	-?*) ;;
	*) break ;;
	esac
	rest=${a#-} first=1
	while [ -n "$rest" ]; do
		c=${rest%%"${rest#?}"}
		rest=${rest#?}
		case "$takes" in
		*"$c"*)
			[ -n "$first" ] || refuse "an option with a value bundled after another: $a"
			if [ -z "$rest" ]; then
				expect=$c
			elif [ "$c" = S ] && [ "$rest" != "$ssh" ]; then
				refuse "$real -S$rest: the tests' ssh is the shim"
			fi
			break ;;
		esac
		first=
	done
done
exec "$real" -S "$ssh" "$@"
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
// PATH, with the scp and sftp beside it. It logs each call, runs ssh -G only
// with -F and refuses a configuration or an agent outside a temporary folder.
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
	for _, tool := range []string{"ssh", "scp", "sftp"} {
		want := testSSHTool(tool)
		if tool == "ssh" {
			want = sshExecutable()
		}
		found, err := exec.Command("/bin/sh", "-c", "command -v "+tool).Output()
		if err != nil || strings.TrimSpace(string(found)) != want {
			t.Fatalf("%s on PATH: %q %v", tool, found, err)
		}
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
// that climbs out of it with ..), an Include of one at any depth, and the
// default keys ssh tries from the home in the user database when nothing
// names a key for the host. What ssh would use is read from ssh -G itself,
// with the real ssh standing in for -G and echo for the run.
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
	// Includes at every depth: the second level names the owner's file, in
	// a glob, after a temporary one on the same line, inside a Match block.
	deeper := filepath.Join(dir, "deeper")
	if err := os.Mkdir(deeper, 0o700); err != nil {
		t.Fatal(err)
	}
	write("deeper/b.conf", "Host probe\n  Include /Users/someone/.ssh/config\n")
	nested := write("nested", "Include "+deeper+"/*.conf\nHost probe\n HostName probe.invalid\n")
	write("deeper/c.inc", "Match all\n  Include "+dir+"/key ~/.ssh/config\n")
	mixed := write("mixed", "Include \""+deeper+"/c.inc\"\n")
	write("deeper/d.inc", "Include="+dir+"/deeper/e.inc\n")
	write("deeper/e.inc", "include ../../../Users/someone/.ssh/config\n")
	third := write("third", "Include "+deeper+"/d.inc\n")
	for _, args := range [][]string{
		{"-F", "/Users/someone/.ssh/config", "probe"},
		{"-F/Users/someone/.ssh/config", "-G", "probe"},
		{"-i", "/Users/someone/.ssh/id_ed25519", "-F", keyed, "probe"},
		{"-o", "IdentityFile=~/.ssh/id_rsa", "-F", keyed, "probe"},
		{"-o", "CertificateFile=/Users/someone/.ssh/id_rsa-cert.pub", "-F", keyed, "probe"},
		{"-F", owners, "probe"},
		{"-F", includes, "probe"},
		{"-G", "-F", includes, "probe"},
		{"-F", nested, "probe"},
		{"-G", "-F", nested, "probe"},
		{"-F", mixed, "probe"},
		{"-F", third, "probe"},
	} {
		if out, err := exec.Command(shim, args...).CombinedOutput(); err == nil || !strings.Contains(string(out), "ssh shim:") {
			t.Errorf("%q ran: %v %s", args, err, out)
		}
	}
	// A loop of Includes ends at ssh's 16 levels.
	loop := write("loop", "Include "+filepath.Join(dir, "loop")+"\n")
	if out, err := exec.Command(shim, "-G", "-F", loop, "probe").CombinedOutput(); err == nil || !strings.Contains(string(out), "deeper than ssh follows") {
		t.Errorf("a loop of Includes: %v %s", err, out)
	}
	noKey := "-o IdentityFile=" + filepath.Join(testSSH.dir, "no-identity") + " "
	for config, wantNoKey := range map[string]bool{keyed: false, bare: true, elsewhere: true} {
		out, err := exec.Command(shim, "-F", config, "probe", "true").Output()
		if err != nil || strings.Contains(string(out), noKey) != wantNoKey || !strings.HasSuffix(string(out), "-F "+config+" probe true\n") {
			t.Errorf("%s: the shim ran %q (%v)", filepath.Base(config), out, err)
		}
	}
	// Files a configuration names inside a temporary folder stay: its own
	// known hosts, Includes through ${HOME} (the test's) and two levels
	// down, and the system's own configuration.
	known := write("known", "")
	write("deeper/f.conf", "Include "+dir+"/deeper/g.inc\n")
	write("deeper/g.inc", "# nothing\n")
	own := write("own", "Include \"${HOME}/deeper/*.conf\"\nInclude /etc/ssh/ssh_config\nHost probe\n HostName probe.invalid\n IdentityFile "+key+"\n UserKnownHostsFile "+known+"\n GlobalKnownHostsFile /dev/null\n")
	if err := os.Remove(filepath.Join(deeper, "b.conf")); err != nil {
		t.Fatal(err)
	}
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
	// Another ssh, which would start without the shim, is refused.
	for _, args := range [][]string{
		{"-J", "jump.invalid", "probe"},
		{"-o", "ProxyCommand=ssh -W %h:%p jump.invalid", "probe"},
		{"-o", "ProxyCommand=exec /usr/bin/ssh -W %h:%p jump.invalid", "probe"},
	} {
		if out, err := exec.Command(shim, append([]string{"-F", bare}, args...)...).CombinedOutput(); err == nil || !strings.Contains(string(out), "another ssh") {
			t.Errorf("%q ran: %v %s", args, err, out)
		}
	}
	if out, err := exec.Command(shim, "-F", bare, "-o", "ProxyCommand=/tmp/cage-proxy --to sshd-probe", "probe").Output(); err != nil || !strings.Contains(string(out), "ProxyCommand=/tmp/cage-proxy") {
		t.Errorf("a ProxyCommand that runs no ssh: %q %v", out, err)
	}
}

// The command line as ssh reads it (getopt, then options again after the
// destination until the command): an option with a value bundled after
// another is refused, since ssh reads -TF <path> as -T -F <path>; options
// after the destination are checked like the others; nothing after the
// command starts is ssh's, so a command's own options pass as they are.
func TestSSHShimReadsTheCommandLineAsSSHDoes(t *testing.T) {
	dir := t.TempDir()
	shim := testSSHEcho(t)
	config := filepath.Join(dir, "config")
	key := filepath.Join(dir, "key")
	for _, path := range []string{config, key} {
		if err := os.WriteFile(path, []byte("Host probe\n HostName probe.invalid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"-TF", config, "probe"},
		{"-TF", "/Users/someone/.ssh/config", "probe"},
		{"-qTF/Users/someone/.ssh/config", "probe"},
		{"-Gi", key, "-F", config, "probe"},
		{"-NS", "/Users/someone/.ssh/cm", "probe"},
		{"-to", "IdentityFile=/Users/someone/.ssh/id_rsa", "probe"},
		{"-F", config, "probe", "-F", "/Users/someone/.ssh/config", "true"},
		{"-F", config, "probe", "-tF", config, "true"},
		{"-F", config, "probe", "-i", "/Users/someone/.ssh/id_rsa"},
		{"-F", config, "--", "probe", "x"}, // and then the ones below run
	} {
		out, err := exec.Command(shim, args...).CombinedOutput()
		refused := err != nil && strings.Contains(string(out), "ssh shim:")
		if refused == (args[len(args)-1] == "x") {
			t.Errorf("%q: %v %s", args, err, out)
		}
	}
	for _, c := range []struct{ args []string }{
		{[]string{"-tt", "-F", config, "probe", "ls", "-al", "-F", "/Users/someone"}},
		{[]string{"-F", config, "-NT", "probe"}},
		{[]string{"-F" + config, "-oBatchMode=yes", "probe", "rsync", "--server", "-vlogDtpre.iLsfxCIvu", ".", "dest/"}},
		{[]string{"-F", config, "-l", "cage", "-s", "--", "probe", "sftp"}},
		{[]string{"-F", config, "probe", "-tt", "--", "tail", "-F", "/Users/someone/log"}},
	} {
		out, err := exec.Command(shim, c.args...).Output()
		if err != nil || !strings.HasSuffix(string(out), " "+strings.Join(c.args, " ")+"\n") {
			t.Errorf("%q: the shim ran %q (%v)", c.args, out, err)
		}
	}
}

// A control socket the command line or a configuration puts outside a
// temporary folder moves into the tests' short folder: the system ssh would
// otherwise make it in the owner's home.
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
		{[]string{"-S/Users/someone/.ssh/cm", "probe"}, "-S" + testSSH.control + "/%C probe\n"},
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
}

// scp and sftp run /usr/bin/ssh by its path unless -S names another: the
// tests' scp and sftp always give the shim, and refuse a -S of another ssh.
// Run for real, an scp started without -S reaches a closed loopback port
// through the shim.
func TestSSHShimScpAndSftpRunTheShim(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"scp", "sftp"} {
		fake := filepath.Join(dir, "echo-"+tool)
		if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(dir, tool)
		testSSHWrite(wrapper, fmt.Sprintf(testSSHToolScript, testSSH.dir, fake, sshExecutable(), testSSHToolTakes[tool]))
		for _, args := range [][]string{
			{"-F", "/dev/null", "-q", "a", "probe:b"},
			{"-S", sshExecutable(), "a", "probe:b"},
			{"-S" + sshExecutable(), "a", "probe:b"},
			{"-r", "--", "-S", "/usr/bin/ssh"},
			{"-oBatchMode=yes", "a", "-S", "/usr/bin/ssh"},
		} {
			out, err := exec.Command(wrapper, args...).Output()
			if err != nil || string(out) != "-S "+sshExecutable()+" "+strings.Join(args, " ")+"\n" {
				t.Errorf("%s %q ran %q (%v)", tool, args, out, err)
			}
		}
		for _, args := range [][]string{
			{"-S", "/usr/bin/ssh", "a", "probe:b"},
			{"-S/usr/bin/ssh", "a", "probe:b"},
			{"-qS", "/usr/bin/ssh", "a", "probe:b"},
			{"-qS/usr/bin/ssh", "a", "probe:b"},
			{"-S", filepath.Join(dir, "other-ssh"), "a", "probe:b"},
		} {
			if out, err := exec.Command(wrapper, args...).CombinedOutput(); err == nil || !strings.Contains(string(out), "ssh shim:") {
				t.Errorf("%s %q ran: %v %s", tool, args, err, out)
			}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("Host probe\n HostName 127.0.0.1\n Port "+strconv.Itoa(port)+"\n BatchMode yes\n ConnectTimeout 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "payload")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := len(testSSHCalls())
	cmd := exec.Command("scp", "-F", config, local, "probe:payload")
	cmd.Env = append(os.Environ(), "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out), "ssh shim:") {
		t.Fatalf("scp to a closed port: %v %s", err, out)
	}
	testSSHCalledSince(t, n, "/usr/bin/scp -F "+config)
	testSSHCalledSince(t, n, "/usr/bin/ssh ")
	if !slices.ContainsFunc(testSSHCalls()[n:], func(call string) bool {
		return strings.HasPrefix(call, "/usr/bin/ssh ") && strings.Contains(call, " -F "+config+" ") && strings.HasSuffix(call, "probe sftp")
	}) {
		t.Fatalf("scp's ssh did not run through the shim: %q", testSSHCalls()[n:])
	}
}

// No test names the system's ssh, scp or sftp for a program to run: scp and
// sftp started that way would run /usr/bin/ssh past the shim, and an ssh
// started that way is not the shim. The shim's own file names them.
func TestSSHShimNoTestNamesTheSystemTools(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	tools := regexp.MustCompile(`"/usr/bin/(scp|sftp)"|exec\.Command(Context)?\([^)]*"/usr/bin/ssh"`)
	for _, name := range files {
		if name == "ssh_shim_test.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if tools.MatchString(line) {
				t.Errorf("%s:%d runs the system's tool past the shim: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
