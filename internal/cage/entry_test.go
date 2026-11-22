package cage

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The wrapper never rides in the component start script: adding it there
// would change the guest revision and ask every guest for a component
// update, also where the entry stays off.
func TestEntryWrapperIsNotPartOfGuestProvisioning(t *testing.T) {
	config := DefaultConfig()
	for name, text := range map[string]string{
		"start":        config.StartScript(),
		"setup":        config.SetupScript(),
		"environment":  config.EnvironmentScript(),
		"tools":        ToolInstallPrelude(),
		"tools update": ToolsUpdateScript(),
		"dependencies": guestDependencies,
		"env":          guestEnv,
		"components":   componentStartScript(),
	} {
		if strings.Contains(text, "cage-entry") || strings.Contains(text, EntryRunPath) || strings.Contains(text, "CAGE_ENTRY_RUN") {
			t.Fatalf("the %s script carries the entry's wrapper", name)
		}
	}
}

func TestEntryInstallScriptInstallsOnceAsRoot(t *testing.T) {
	script := EntryInstallScript(EntryRunPath, EntryHerdrProfilePath)
	for _, want := range []string{EntryRunDigest(), "sudo install -d -o root -g root -m 0755 /usr/local/lib/cage-entry\n", "sudo install -o root -g root -m 0755 \"$tmp\" /usr/local/lib/cage-entry/run\n", "<<'CAGE_ENTRY_RUN'\n" + EntryRunScript() + "CAGE_ENTRY_RUN\n",
		"sudo install -o root -g root -m 0644 \"$tmp\" /etc/profile.d/cage-herdr.sh\n", "<<'CAGE_ENTRY_PROFILE'\n" + EntryHerdrProfile() + "CAGE_ENTRY_PROFILE\n"} {
		if !strings.Contains(script, want) {
			t.Fatalf("install script lacks %q", want)
		}
	}
	if strings.Count(EntryRunScript(), "\nCAGE_ENTRY_RUN") != 0 || strings.Count(EntryHerdrProfile(), "\nCAGE_ENTRY_PROFILE") != 0 {
		t.Fatal("the wrapper would end its own heredoc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	// Run it with stand-ins for sudo (drops the root ownership a test cannot
	// set and logs each call) and sha256sum.
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o700)
	log := filepath.Join(dir, "sudo.log")
	sudo := "#!/bin/sh\necho \"$*\" >> " + log + "\nargs=\nfor a in \"$@\"; do case \"$a\" in root) ;; -o|-g) ;; *) args=\"$args '$a'\";; esac; done\neval \"set -- $args\"\nexec \"$@\"\n"
	_ = os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0o700)
	if _, err := exec.LookPath("sha256sum"); err != nil {
		_ = os.WriteFile(filepath.Join(bin, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0o700)
	}
	target := filepath.Join(dir, "lib", "cage-entry", "run")
	profile := filepath.Join(dir, "profile.d", "cage-herdr.sh")
	run := func() {
		t.Helper()
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", EntryInstallScript(target, profile))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + dir, "TMPDIR=" + dir}
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("install: %v %q", err, out)
		}
	}
	run()
	installed, err := os.ReadFile(target)
	if err != nil || string(installed) != EntryRunScript() {
		t.Fatal("installed copy differs", err)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode())
	}
	if installed, err := os.ReadFile(profile); err != nil || string(installed) != EntryHerdrProfile() {
		t.Fatal("installed profile differs", err)
	}
	if info, _ := os.Stat(profile); info.Mode().Perm() != 0o644 {
		t.Fatalf("profile mode %v", info.Mode())
	}
	calls, _ := os.ReadFile(log)
	if bytes.Count(calls, []byte("\n")) != 4 {
		t.Fatalf("sudo calls %q", calls)
	}
	run()
	if again, _ := os.ReadFile(log); !bytes.Equal(again, calls) {
		t.Fatalf("a current copy was installed again: %q", again)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "tmp.*")); len(leftovers) != 0 {
		t.Fatal("temporary file left", leftovers)
	}
}

// entryRunScript is the wrapper with the guest's environment file moved.
func entryRunScript(t *testing.T, environment string) string {
	t.Helper()
	script := strings.Replace(EntryRunScript(), "environment=/etc/claude-cage/environment", "environment="+environment, 1)
	if script == EntryRunScript() {
		t.Fatal("the wrapper's environment path moved")
	}
	path := filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEntryRunScriptModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper runs in the Linux guest")
	}
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	_ = os.WriteFile(environment, []byte("TZ=Asia/Tokyo\nLANG=ja_JP.UTF-8\nLC_ALL=ja_JP.UTF-8\nCAGE_LANGUAGE=ja\n"), 0o600)
	run := entryRunScript(t, environment)
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)
	call := func(stdin string, args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command("/bin/sh", append([]string{run}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "SHELL=/bin/sh", "LANG=C", "TZ=UTC"}
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	}
	out, _, code := call("", "exec", `printf '%s|%s|%s|%s|%s' "$TZ" "$LANG" "$LC_ALL" "$CAGE_WORKBENCH" "$(pwd)"; exit 4`)
	if out != "Asia/Tokyo|ja_JP.UTF-8|ja_JP.UTF-8|1|"+home || code != 4 {
		t.Fatalf("exec: %q %d", out, code)
	}
	if out, _, code := call("printf '%s' \"$TZ\"\n", "shell"); out != "Asia/Tokyo" || code != 0 {
		t.Fatalf("shell: %q %d", out, code)
	}
	for _, args := range [][]string{{}, {"exec"}, {"exec", "a", "b"}, {"shell", "x"}, {"sftp", "x"}, {"connect"}} {
		if _, _, code := call("", args...); code != 2 {
			t.Fatalf("%q: %d", args, code)
		}
	}
	// Without the environment file the command still runs, in the client's
	// absence of a locale.
	_ = os.Remove(environment)
	if out, _, code := call("", "exec", "echo ok"); out != "ok\n" || code != 0 {
		t.Fatalf("without the environment file: %q %d", out, code)
	}
}

// entryHerdrRunScript is the wrapper with the guest's environment file and
// Herdr's runtime folder moved (a socket path must stay short).
func entryHerdrRunScript(t *testing.T, environment, runtimeDir string) string {
	t.Helper()
	path := entryRunScript(t, environment)
	data, _ := os.ReadFile(path)
	script := strings.Replace(string(data), "herdr_runtime=\"$HOME/.cache/cage-herdr/run\"", "herdr_runtime="+runtimeDir, 1)
	if script == string(data) {
		t.Fatal("the wrapper's Herdr runtime folder moved")
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEntryRunScriptHerdr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper runs in the Linux guest")
	}
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	_ = os.WriteFile(environment, []byte("TZ=Asia/Tokyo\nLANG=ja_JP.UTF-8\n"), 0o600)
	runtimeDir, err := os.MkdirTemp("", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	run := entryHerdrRunScript(t, environment, runtimeDir)
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)
	calls := filepath.Join(dir, "core.log")
	call := func(args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command("/bin/sh", append([]string{run}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "SHELL=/bin/sh", "LANG=C", "TZ=UTC", "XDG_RUNTIME_DIR=/run/user/1000", "HC_SESSION=leaked"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	}
	// No core: Herdr's own words and status, so hc machine add offers its install.
	if _, stderr, code := call("herdr", "default"); code != 127 || stderr != "herdr-core is not installed here\n" {
		t.Fatalf("no core: %d %q", code, stderr)
	}
	for _, session := range []string{"", "Work", "-x", "../x", "a b", strings.Repeat("a", 33)} {
		if _, _, code := call("herdr", session); code != 2 {
			t.Fatalf("session %q: %d", session, code)
		}
	}
	// A stand-in core that logs how it was called.
	bin := filepath.Join(home, ".local", "bin")
	_ = os.MkdirAll(bin, 0o700)
	status := filepath.Join(dir, "status.json")
	core := "#!/bin/sh\necho \"$* runtime=$XDG_RUNTIME_DIR session=${HC_SESSION-none} tz=$TZ pid=$$\" >> " + calls + "\n" +
		"case \"$*\" in 'server status --json') cat " + status + ";; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr-core"), []byte(core), 0o755); err != nil {
		t.Fatal(err)
	}
	logged := func() []string {
		data, _ := os.ReadFile(calls)
		_ = os.Remove(calls)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	if _, _, code := call("herdr", "default"); code != 0 {
		t.Fatal(code)
	}
	line := logged()
	if len(line) != 1 || !strings.HasPrefix(line[0], "connect runtime="+runtimeDir+" session=none tz=Asia/Tokyo pid=") {
		t.Fatalf("connect: %q", line)
	}
	pid := strings.TrimPrefix(line[0], "connect runtime="+runtimeDir+" session=none tz=Asia/Tokyo pid=")
	if _, err := os.Stat(filepath.Join(runtimeDir, "cage-lines", "default."+pid)); err != nil {
		t.Fatal("no line mark for the connect process", err)
	}
	if _, _, code := call("herdr", "work"); code != 0 || !strings.Contains(logged()[0], "session=work") {
		t.Fatal("named session")
	}
	// An install runs in the same environment.
	if out, _, code := call("herdr-exec", "printf '%s|%s' \"$XDG_RUNTIME_DIR\" \"$TZ\""); code != 0 || out != runtimeDir+"|Asia/Tokyo" {
		t.Fatalf("herdr-exec: %q %d", out, code)
	}
	// The idle check: a live line keeps the core; a stale mark goes; no
	// socket asks nothing; an idle core stops; a busy one stays.
	lines := filepath.Join(runtimeDir, "cage-lines")
	_ = os.RemoveAll(lines)
	_ = os.MkdirAll(lines, 0o700)
	live := filepath.Join(lines, "default."+strconv.Itoa(os.Getpid()))
	_ = os.WriteFile(live, nil, 0o600)
	if _, _, code := call("herdr-idle-check", "default"); code != 0 || logged()[0] != "" {
		t.Fatal("a live line did not keep the core")
	}
	_ = os.Remove(live)
	stale := filepath.Join(lines, "default.999999")
	_ = os.WriteFile(stale, nil, 0o600)
	if _, _, code := call("herdr-idle-check", "default"); code != 0 || logged()[0] != "" {
		t.Fatal("no socket, yet the core was asked")
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a stale line mark stayed")
	}
	_ = os.MkdirAll(filepath.Join(runtimeDir, "herdr-core"), 0o700)
	listener, err := net.Listen("unix", filepath.Join(runtimeDir, "herdr-core", "default.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = os.WriteFile(status, []byte("{\"finished\":0,\"live\":2,\"pid\":1}"), 0o600)
	_ = os.WriteFile(status, []byte("{\"finished\":0,\"live\":0,\"pid\":1}"), 0o600)
	call("herdr-idle-check", "default")
	if got := commands(logged()); got != "server status --json;server stop" {
		t.Fatalf("idle core: %q", got)
	}
	_ = os.WriteFile(status, []byte("{\"finished\":0,\"live\":3,\"pid\":1}"), 0o600)
	call("herdr-idle-check", "default")
	if got := commands(logged()); got != "server status --json" {
		t.Fatalf("busy core: %q", got)
	}
	// Arming without systemd (this host): a waiting process runs the check.
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("systemd-run"); err == nil {
			return
		}
	}
	_ = os.WriteFile(status, []byte("{\"finished\":0,\"live\":0,\"pid\":1}"), 0o600)
	if _, _, code := call("herdr-idle", "default", "1"); code != 0 {
		t.Fatal(code)
	}
	waiter := filepath.Join(runtimeDir, "cage-idle-default.pid")
	if _, err := os.Stat(waiter); err != nil {
		t.Fatal("no waiter", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(calls)
		if strings.Contains(string(data), "server stop") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the armed check did not stop the idle core: %q", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(waiter); err == nil {
		t.Fatal("the waiter's mark stayed")
	}
	for _, args := range [][]string{{"herdr-idle", "default"}, {"herdr-idle", "default", "1m"}, {"herdr-idle", "Bad", "1"}, {"herdr-exec"}, {"herdr"}} {
		if _, _, code := call(args...); code != 2 {
			t.Fatalf("%q: %d", args, code)
		}
	}
}

// commands are the logged calls without their environment.
func commands(lines []string) string {
	var out []string
	for _, line := range lines {
		if i := strings.Index(line, " runtime="); i >= 0 {
			out = append(out, line[:i])
		}
	}
	return strings.Join(out, ";")
}
