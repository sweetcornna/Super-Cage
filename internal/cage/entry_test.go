package cage

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	script := EntryInstallScript(EntryRunPath)
	for _, want := range []string{"want=" + EntryRunDigest(), "sudo install -d -o root -g root -m 0755 /usr/local/lib/cage-entry\n", "sudo install -o root -g root -m 0755 \"$tmp\" /usr/local/lib/cage-entry/run\n", "<<'CAGE_ENTRY_RUN'\n" + EntryRunScript() + "CAGE_ENTRY_RUN\n"} {
		if !strings.Contains(script, want) {
			t.Fatalf("install script lacks %q", want)
		}
	}
	if strings.Count(EntryRunScript(), "\nCAGE_ENTRY_RUN") != 0 {
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
	run := func() {
		t.Helper()
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", EntryInstallScript(target))
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
	calls, _ := os.ReadFile(log)
	if bytes.Count(calls, []byte("\n")) != 2 {
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
