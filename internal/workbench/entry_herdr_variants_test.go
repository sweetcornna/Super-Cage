//go:build !windows

package workbench

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// Variants of E14 (TestEntryHerdrRealCores) for what an owner meets on first
// use, with real cores from CAGE_HERDR_CORE (a scratch copy) and the
// endpoint as built. They record rather than assert: every ssh command line
// the owner's core runs (through the shim), what the endpoint makes of each
// (entryHerdrClassify), and what ran in the environment. Knobs:
//
//	CAGE_HERDR_CORE=<scratch herdr-core>  required
//	CAGE_HERDR_SANDBOX=<sandbox-exec profile>  the cores and the environment's
//	    commands run with no network but localhost
//	CAGE_HERDR_TTY=y      add the machine in a pty, answering Herdr's [Y/n]
//	CAGE_HERDR_PLAIN=1    the Include form (hc machine add cage-default)
//	CAGE_HERDR_GUEST_UNAME="Linux aarch64"  what the environment's uname says
//	CAGE_HERDR_RELEASES=http://127.0.0.1:<port>  Herdr's release server
//
//	go test -c -o /tmp/w.test ./internal/workbench && cd internal/workbench &&
//	CAGE_HERDR_CORE=... CAGE_HERDR_SANDBOX=... /tmp/w.test -test.run 'TestEntryHerdrReal(FirstInstall|PlainForm)' -test.v

// entryHerdrStepName is what the endpoint does with an exec line.
func entryHerdrStepName(command string) string {
	step, session := entryHerdrClassify(command)
	switch step {
	case entryHerdrConnect:
		return "Herdr link through the filter (session " + session + ")"
	case entryHerdrInstall:
		return "herdr-exec (Herdr's environment, unfiltered)"
	case entryHerdrTUI:
		return "refused: Herdr TUI bridge"
	case entryHerdrDrift:
		return "refused: a connect line Super Cage does not know"
	}
	return "ordinary exec (unfiltered)"
}

// entryHerdrLogCalls logs the shim's calls after the first n, each with what
// the endpoint makes of its remote command.
func entryHerdrLogCalls(t *testing.T, n int, alias string) {
	t.Helper()
	all := testSSHCalls()
	var b strings.Builder
	for _, call := range all[min(n, len(all)):] {
		b.WriteString("  ssh: " + call + "\n")
		if i := strings.Index(call, " "+alias+" "); i >= 0 {
			b.WriteString("    endpoint: " + entryHerdrStepName(call[i+len(alias)+2:]) + "\n")
		}
	}
	t.Logf("ssh calls:\n%s", b.String())
}

// entryHerdrMasters is every process whose command line names the shim's
// control socket folder: ssh masters ("ssh: <path> [mux]") and their clients.
func entryHerdrMasters(marks ...string) map[int]string {
	out, _ := exec.Command("/bin/ps", "-axo", "pid=,command=").Output()
	found := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		named := strings.Contains(line, testSSH.control)
		for _, mark := range marks {
			named = named || strings.Contains(line, mark)
		}
		if !named || strings.Contains(line, "/bin/ps") {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if pid, err := strconv.Atoi(fields[0]); err == nil && len(fields) == 2 {
			found[pid] = fields[1]
		}
	}
	return found
}

// entryHerdrKillMasters ends the masters this test's cores started (their
// command lines name this test binary's own socket folder).
func entryHerdrKillMasters(t *testing.T) {
	t.Cleanup(func() {
		for pid, args := range entryHerdrMasters() {
			t.Logf("ending %d: %s", pid, args)
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	})
}

// entryHerdrWrapCopies puts scp and sftp before the system's on the owner
// core's PATH, each running the ssh shim (they run /usr/bin/ssh by its path
// otherwise, which reads the owner's ~/.ssh), and logging that they ran.
func entryHerdrWrapCopies(h *entryHerdrCores) {
	for _, name := range []string{"scp", "sftp"} {
		h.script(filepath.Join(filepath.Dir(h.ownerCore), name), "#!/bin/sh\nprintf '%s\\n' \"/usr/bin/"+name+" $*\" >> "+shellescape.Quote(filepath.Join(testSSH.dir, "calls.log"))+"\nexec /usr/bin/"+name+" -S "+shellescape.Quote(sshExecutable())+" \"$@\"\n")
	}
}

// entryHerdrSandbox runs the owner's core and every command of the
// environment's stand-in inside sandbox-exec with the profile
// CAGE_HERDR_SANDBOX names (no network but localhost): a download fails
// there. The owner's hc and herdr-core become wrappers around the real ones,
// which keep their names (Herdr reads argv[0]).
func entryHerdrSandbox(t *testing.T, h *entryHerdrCores, f *entryFixture) {
	t.Helper()
	profile := os.Getenv("CAGE_HERDR_SANDBOX")
	if profile == "" {
		return
	}
	bin := filepath.Dir(h.ownerCore)
	real := filepath.Join(bin, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(h.ownerCore, filepath.Join(real, "herdr-core")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("herdr-core", filepath.Join(real, "hc")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(bin, "hc"))
	for _, name := range []string{"herdr-core", "hc"} {
		h.script(filepath.Join(bin, name), "#!/bin/sh\nexec /usr/bin/sandbox-exec -f "+shellescape.Quote(profile)+" "+shellescape.Quote(filepath.Join(real, name))+" \"$@\"\n")
	}
	shim := filepath.Join(f.guest.dir, "shim.sh")
	data, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(data), "exec /bin/sh -c", "exec /usr/bin/sandbox-exec -f "+shellescape.Quote(profile)+" /bin/sh -c", 1)
	if moved == string(data) {
		t.Fatal("the guest shim changed")
	}
	h.script(shim, moved)
}

// entryHerdrInteractive runs a command line as the owner would in a
// terminal (script(1) gives it a pty) and answers each [Y/n] with answer.
func entryHerdrInteractive(t *testing.T, h *entryHerdrCores, line, answer string) (string, int) {
	t.Helper()
	cmd := h.command(false, "/usr/bin/script", "-q", "/dev/null", "/bin/sh", "-c", line)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	buf := make([]byte, 4096)
	answered := 0
	for {
		n, err := stdout.Read(buf)
		out.Write(buf[:n])
		if strings.Count(out.String(), "[Y/n]") > answered {
			answered++
			_, _ = io.WriteString(stdin, answer+"\n")
		}
		if err != nil {
			break
		}
	}
	_ = stdin.Close()
	err = cmd.Wait()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	return out.String(), code
}

func entryHerdrListing(dir string) string {
	var b strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err.Error()
	}
	for _, e := range entries {
		info, _ := os.Lstat(filepath.Join(dir, e.Name()))
		line := info.Mode().String() + " " + strconv.FormatInt(info.Size(), 10) + " " + e.Name()
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(filepath.Join(dir, e.Name()))
			line += " -> " + target
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// First use: the environment has no herdr-core yet. CAGE_HERDR_GUEST_UNAME
// (e.g. "Linux aarch64") makes the environment's uname say so, as a Cage
// guest's would.
func TestEntryHerdrRealFirstInstall(t *testing.T) {
	source := os.Getenv("CAGE_HERDR_CORE")
	if source == "" {
		t.Skip("set CAGE_HERDR_CORE to a herdr-core binary for this computer")
	}
	f := newEntryFixture(t, true)
	h := newEntryHerdrCores(t, f, source)
	entryHerdrKillMasters(t)
	entryHerdrWrapCopies(h)
	entryHerdrSandbox(t, h, f)
	if err := os.Remove(h.guestCore); err != nil {
		t.Fatal(err)
	}
	// CAGE_HERDR_RELEASES points Herdr's release server (HERDR_RELEASES_URL)
	// at a local listener, to see what the core downloads and where.
	if releases := os.Getenv("CAGE_HERDR_RELEASES"); releases != "" {
		h.ownerEnv = append(h.ownerEnv, "HERDR_RELEASES_URL="+releases)
	}
	if arch := os.Getenv("CAGE_HERDR_GUEST_UNAME"); arch != "" {
		fake := filepath.Join(f.guest.dir, "fakebin")
		if err := os.MkdirAll(fake, 0o700); err != nil {
			t.Fatal(err)
		}
		h.script(filepath.Join(fake, "uname"), "#!/bin/sh\necho "+shellescape.Quote(arch)+"\n")
		shim := filepath.Join(f.guest.dir, "shim.sh")
		data, err := os.ReadFile(shim)
		if err != nil {
			t.Fatal(err)
		}
		moved := strings.Replace(string(data), "cd \"$HOME\"", "export PATH="+shellescape.Quote(fake)+":$PATH\ncd \"$HOME\"", 1)
		if moved == string(data) {
			t.Fatal("the guest shim changed")
		}
		h.script(shim, moved)
	}
	f.on()
	if os.Getenv("CAGE_HERDR_PLAIN") != "" {
		// The plain form: the Include in the owner's ~/.ssh/config (a
		// stand-in the shim reads when the core gives no -F).
		if _, err := os.Stat(f.paths.userConfig()); err != nil {
			_ = os.WriteFile(f.paths.userConfig(), nil, 0o600)
		}
		if _, err := entryIncludeApply(f.paths, entryIncludePreview(f.paths).Token); err != nil {
			t.Fatal(err)
		}
		h.ownerEnv = append(h.ownerEnv, "CAGE_TEST_SSH_CONFIG="+f.paths.userConfig())
	}
	view := f.s.entryView()
	guestLog := filepath.Join(f.guest.dir, "commands.log")
	before, _ := os.ReadFile(guestLog)
	calls := len(testSSHCalls())
	start := time.Now()
	var out string
	var code int
	if answer := os.Getenv("CAGE_HERDR_TTY"); answer != "" {
		// In a terminal, as the owner runs it: Herdr asks before it copies.
		out, code = entryHerdrInteractive(t, h, view.Herdr.Command, answer)
	} else {
		out, code = h.shell(view.Herdr.Command)
	}
	t.Logf("%s: exit %d after %v\n%s", view.Herdr.Command, code, time.Since(start).Round(time.Millisecond), out)
	entryHerdrLogCalls(t, calls, "cage-default")
	after, _ := os.ReadFile(guestLog)
	t.Logf("ran in the environment (the inner hop's commands):\n%s", after[min(len(before), len(after)):])
	t.Logf("the environment's ~/.local/bin:\n%s", entryHerdrListing(filepath.Dir(h.guestCore)))
	if _, err := os.Stat(h.guestCore); err == nil {
		version, _ := h.hc(true, "--version")
		t.Logf("the environment's core: %s", strings.TrimSpace(version))
	}
	if code == 0 {
		out, code = h.hc(false, "run", "--json", "--on", "cage-default", "--", "sh", "-c", "echo ok-$((1+1))")
		t.Logf("hc run --on: exit %d %s", code, out)
	}
	t.Logf("endpoint: %+v", f.s.entry.registry.counts())
}

// The plain form: with the Include in ~/.ssh/config, the page prints
// "hc machine add cage-default" and the core runs ssh without -F. The
// shim's CAGE_TEST_SSH_CONFIG stands in for ~/.ssh/config, and any control
// socket the core asks for lands in the shim's folder. Then the entry is
// cut (off, lock) while a master may hold the connection.
func TestEntryHerdrRealPlainForm(t *testing.T) {
	source := os.Getenv("CAGE_HERDR_CORE")
	if source == "" {
		t.Skip("set CAGE_HERDR_CORE to a herdr-core binary for this computer")
	}
	post := func(f *entryFixture, path string) {
		if err := f.call("POST", path, map[string]any{}, nil); err != nil {
			f.t.Error(err)
		}
	}
	for _, c := range []struct {
		name string
		fire func(*entryFixture)
	}{
		{"off", func(f *entryFixture) { post(f, "/api/entry/off") }},
		{"lock", func(f *entryFixture) { f.s.account.lock() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newEntryFixture(t, true)
			h := newEntryHerdrCores(t, f, source)
			entryHerdrKillMasters(t)
			entryHerdrWrapCopies(h)
			entryHerdrSandbox(t, h, f)
			f.on()
			if _, err := os.Stat(f.paths.userConfig()); err != nil {
				if err := os.WriteFile(f.paths.userConfig(), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := entryIncludeApply(f.paths, entryIncludePreview(f.paths).Token); err != nil {
				t.Fatal(err)
			}
			config, _ := os.ReadFile(f.paths.userConfig())
			t.Logf("the owner's ~/.ssh/config stand-in %s:\n%s", f.paths.userConfig(), config)
			h.ownerEnv = append(h.ownerEnv, "CAGE_TEST_SSH_CONFIG="+f.paths.userConfig())
			view := f.s.entryView()
			if view.Herdr == nil || view.Herdr.Command != "hc machine add cage-default" {
				t.Fatalf("the page's command: %+v", view.Herdr)
			}
			calls := len(testSSHCalls())
			out, code := h.shell(view.Herdr.Command)
			t.Logf("%s: exit %d\n%s", view.Herdr.Command, code, out)
			entryHerdrLogCalls(t, calls, "cage-default")
			if code != 0 {
				t.Fatal("the plain form did not add the machine")
			}
			time.Sleep(time.Second)
			sockets, _ := filepath.Glob(filepath.Join(testSSH.control, "*"))
			t.Logf("control sockets: %q\nprocesses naming them: %v\nendpoint: %+v", sockets, entryHerdrMasters(), f.s.entry.registry.counts())
			calls = len(testSSHCalls())
			out, code = h.hc(false, "run", "--json", "--on", "cage-default", "--", "sh", "-c", "echo ok-$((1+1))")
			t.Logf("hc run --on: exit %d %s\nendpoint after: %+v", code, out, f.s.entry.registry.counts())
			entryHerdrLogCalls(t, calls, "cage-default")
			start := time.Now()
			c.fire(f)
			entryAwait(t, 10*time.Second, "the endpoint's connections to end", func() bool { return f.s.entry.registry.counts().Total == 0 })
			t.Logf("%s: the endpoint had no connection %v after the event", c.name, time.Since(start).Round(time.Millisecond))
			time.Sleep(2 * time.Second)
			sockets, _ = filepath.Glob(filepath.Join(testSSH.control, "*"))
			t.Logf("after %s: control sockets %q, processes naming them %v", c.name, sockets, entryHerdrMasters())
			calls = len(testSSHCalls())
			out, code = h.hc(false, "run", "--json", "--on", "cage-default", "--", "sh", "-c", "echo after-cut")
			t.Logf("hc run --on after %s: exit %d %s", c.name, code, out)
			entryHerdrLogCalls(t, calls, "cage-default")
		})
	}
}

// A master connection already open where the core looks for one: in the
// plain form the core dials with -S <its runtime folder>/herdr-core/ssh-<hash>
// and ControlMaster=no, so a master there (one an interactive login with
// ControlPersist=600 would leave) carries its line without a new dial. Then
// the entry is cut (off, lock, revoke): does the master end, and does the
// next dial meet the endpoint's checks?
func TestEntryHerdrRealPlainFormThroughAMaster(t *testing.T) {
	source := os.Getenv("CAGE_HERDR_CORE")
	if source == "" {
		t.Skip("set CAGE_HERDR_CORE to a herdr-core binary for this computer")
	}
	post := func(f *entryFixture, path string) {
		if err := f.call("POST", path, map[string]any{}, nil); err != nil {
			f.t.Error(err)
		}
	}
	for _, c := range []struct {
		name string
		fire func(*entryFixture)
	}{
		{"off", func(f *entryFixture) { post(f, "/api/entry/off") }},
		{"lock", func(f *entryFixture) { f.s.account.lock() }},
		{"revoke", func(f *entryFixture) { post(f, "/api/entry/devices/"+f.computer.ID+"/revoke") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newEntryFixture(t, true)
			h := newEntryHerdrCores(t, f, source)
			entryHerdrWrapCopies(h)
			entryHerdrSandbox(t, h, f)
			socket := filepath.Join(h.short, "o", "herdr-core", "ssh-41cd5178f9e9")
			t.Cleanup(func() {
				for pid, args := range entryHerdrMasters(socket) {
					t.Logf("ending %d: %s", pid, args)
					_ = syscall.Kill(pid, syscall.SIGTERM)
				}
			})
			f.on()
			if _, err := os.Stat(f.paths.userConfig()); err != nil {
				_ = os.WriteFile(f.paths.userConfig(), nil, 0o600)
			}
			if _, err := entryIncludeApply(f.paths, entryIncludePreview(f.paths).Token); err != nil {
				t.Fatal(err)
			}
			h.ownerEnv = append(h.ownerEnv, "CAGE_TEST_SSH_CONFIG="+f.paths.userConfig())
			if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
				t.Fatal(err)
			}
			// The master, as a login would open it.
			master := h.command(false, sshExecutable(), "-S", socket, "-o", "ControlMaster=yes", "-o", "ControlPersist=600", "-o", "BatchMode=yes", "-T", "cage-default", "true")
			out, stderr, code := entryCombined(master)
			t.Logf("master: exit %d %q %q; endpoint %+v; processes %v", code, out, stderr, f.s.entry.registry.counts(), entryHerdrMasters(socket))
			calls := len(testSSHCalls())
			out, code = h.shell("hc machine add cage-default")
			t.Logf("hc machine add cage-default: exit %d %s", code, out)
			entryHerdrLogCalls(t, calls, "cage-default")
			if !strings.Contains(strings.Join(testSSHCalls()[calls:], "\n"), "-S "+socket+" ") {
				t.Fatalf("the core's control socket is not %s", socket)
			}
			time.Sleep(time.Second)
			t.Logf("with the line open: endpoint %+v; processes %v", f.s.entry.registry.counts(), entryHerdrMasters(socket))
			out, code = h.hc(false, "run", "--json", "--on", "cage-default", "--", "sh", "-c", "echo ok-$((1+1))")
			t.Logf("hc run --on: exit %d %s", code, strings.TrimSpace(out))
			start := time.Now()
			c.fire(f)
			entryAwait(t, 10*time.Second, "the endpoint's connections to end", func() bool { return f.s.entry.registry.counts().Total == 0 })
			cut := time.Since(start)
			entryAwait(t, 10*time.Second, "the master to end", func() bool { return len(entryHerdrMasters(socket)) == 0 })
			_, gone := os.Stat(socket)
			t.Logf("%s: no connection %v after the event, master gone %v after (socket removed: %v)", c.name, cut.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), os.IsNotExist(gone))
			again := h.command(false, sshExecutable(), "-S", socket, "-o", "ControlMaster=no", "-o", "BatchMode=yes", "-T", "cage-default", "true")
			out, stderr, code = entryCombined(again)
			t.Logf("a dial with the core's options after %s: exit %d %q %q", c.name, code, out, strings.TrimSpace(stderr))
			if code == 0 {
				t.Errorf("a dial after %s went through", c.name)
			}
		})
	}
}
