//go:build !windows

package workbench

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the second round of the security review of the local SSH entry
// (review3-ssh, findings-round2.md). Each names its finding.

// N1: an Include path is followed the way ssh follows it: ${VAR} and the %
// tokens it can expand with certainty; anything else makes the owner's
// names unknown and the alias takes the collision-proof form. The
// reviewer's two experiments (an environment variable and %u) are the
// first cases, checked against ssh -G too.
func TestEntryAliasFollowsIncludesLikeSSH(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, include, file, want string
	}{
		{"home variable", "Include ${HOME}/inc/e.conf", "e.conf", "cage-lab1-defaul"},
		// Any other variable may differ between the owner's shell and the
		// controller's environment, even when the controller has it.
		{"another variable", "Include ${CAGE_TEST_INC}/e.conf", "e.conf", "super-cage-lab1-default"},
		{"user token", "Include HOMEDIR/inc/%u.conf", me.Username + ".conf", "cage-lab1-defaul"},
		{"unknown variable", "Include ${CAGE_TEST_ELSEWHERE}/e.conf", "", "super-cage-lab1-default"},
		{"host token", "Include ~/.ssh/hosts/%h.conf", "", "super-cage-lab1-default"},
		{"another user's home", "Include ~root/e.conf", "", "super-cage-lab1-default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths := entryTestPaths(t)
			home := filepath.Dir(paths.SSHDir)
			// ssh expands ${HOME} from its environment.
			t.Setenv("HOME", home)
			t.Setenv("CAGE_TEST_INC", filepath.Join(home, "inc"))
			if err := os.MkdirAll(filepath.Join(home, "inc"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if c.file != "" {
				if err := os.WriteFile(filepath.Join(home, "inc", c.file), []byte("Host cage-lab1\n  HostName owner-target.example.org\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			include := strings.ReplaceAll(c.include, "HOMEDIR", home)
			if err := os.WriteFile(paths.userConfig(), []byte(include+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			alias, err := entryWriteClient(paths, defaultLocalVMID, "lab1", entryTestHostKey(t))
			if err != nil {
				t.Fatal(err)
			}
			if alias != c.want {
				t.Fatalf("alias %q, want %q", alias, c.want)
			}
			if c.file == "" || strings.HasPrefix(c.want, "super-cage-") {
				// Nothing to compare: ssh (here with an environment of its own)
				// may not see the owner's file either.
				return
			}
			// ssh agrees: with Super Cage's Include first, the owner's host
			// keeps its target and the generated alias reaches the entry.
			owner := filepath.Join(paths.SSHDir, "owner_config")
			if err := os.WriteFile(owner, []byte("Include "+entryQuote(paths.clientFile())+"\n"+include+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			theirs, stderr, code := sshGWith(t, "/usr/bin/ssh", owner, "cage-lab1")
			ours, _, code2 := sshGWith(t, "/usr/bin/ssh", owner, alias)
			if code != 0 || code2 != 0 || theirs["hostname"] != "owner-target.example.org" || !strings.Contains(ours["proxycommand"], "entry proxy default") {
				t.Fatalf("ssh -G: %d %q %v / %d %v", code, stderr, theirs["hostname"], code2, ours["proxycommand"])
			}
		})
	}
}

// N2: the files the owner's ssh reads hold only options every OpenSSH since
// the stated baseline (7.6) knows, and no IgnoreUnknown, whether or not the
// owner's configuration has one: the first one ssh reads wins, so Super
// Cage's would replace one the owner adds later.
func TestEntryIncludedFilesUseOnlyBaselineOptions(t *testing.T) {
	if len(entryBaselineOptions) == 0 {
		t.Fatal("no baseline option list")
	}
	for _, owner := range []string{"", "Host *\n  IgnoreUnknown UseKeychain\n  UseKeychain yes\n"} {
		paths := entryTestPaths(t)
		if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.userConfig(), []byte(owner), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := entryWriteClient(paths, defaultLocalVMID, "lab1", entryTestHostKey(t)); err != nil {
			t.Fatal(err)
		}
		var ignored []string
		for _, path := range []string{paths.clientFile(), paths.hostFile(defaultLocalVMID)} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(data), "\n") {
				keyword, args := entryConfigLine(line)
				switch {
				case keyword == "":
				case keyword == "IgnoreUnknown":
					ignored = strings.Split(strings.Join(args, ","), ",")
				case keyword == "Host" || keyword == "Include":
				case entryBaselineOptions[keyword] != "":
				default:
					if !containsFold(ignored, keyword) {
						t.Errorf("owner config %q: %s writes %s without IgnoreUnknown", owner, filepath.Base(path), keyword)
					}
				}
			}
			if path == paths.hostFile(defaultLocalVMID) && !strings.Contains(string(data), "  VerifyHostKeyDNS no\n") {
				t.Errorf("no VerifyHostKeyDNS no in\n%s", data)
			}
		}
		if len(ignored) != 0 {
			t.Errorf("owner config %q: Super Cage's IgnoreUnknown %v would replace the owner's", owner, ignored)
		}
	}
}

func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return true
		}
	}
	return false
}

// N2: an option the owner's ssh does not know (a stand-in for an older ssh
// the controller cannot see) does not make every ssh fail. The reviewer's
// experiment: before, ssh -G of any host exited 255 with "Bad configuration
// option".
func TestEntryNewerOptionNeverBreaksOtherHosts(t *testing.T) {
	entryNewerOptions["FooBarOption"] = "no"
	entryNewerOptionOrder = append(entryNewerOptionOrder, "FooBarOption")
	t.Cleanup(func() {
		delete(entryNewerOptions, "FooBarOption")
		entryNewerOptionOrder = entryNewerOptionOrder[:len(entryNewerOptionOrder)-1]
	})
	for _, owner := range []string{"Host example.org\n  User me\n", "IgnoreUnknown UseKeychain\nUseKeychain yes\nHost example.org\n  User me\n"} {
		paths := entryTestPaths(t)
		if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.userConfig(), []byte(owner), 0o600); err != nil {
			t.Fatal(err)
		}
		alias, err := entryWriteClient(paths, defaultLocalVMID, "lab1", entryTestHostKey(t))
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(paths.SSHDir, "owner_config")
		if err := os.WriteFile(config, []byte("Include "+entryQuote(paths.clientFile())+"\n"+owner), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"example.org", alias} {
			if _, stderr, code := sshGWith(t, "/usr/bin/ssh", config, host); code != 0 {
				t.Fatalf("owner config %q, ssh -G %s: %d %s", owner, host, code, stderr)
			}
		}
	}
}

// N2: cage ssh applies the newer options through its own -F file, whatever
// the owner's configuration says.
func TestEntryCageSSHAppliesNewerOptions(t *testing.T) {
	paths := entryTestPaths(t)
	if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.userConfig(), []byte("IgnoreUnknown UseKeychain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias, err := entryWriteClient(paths, defaultLocalVMID, "lab1", entryTestHostKey(t))
	if err != nil {
		t.Fatal(err)
	}
	args, err := entrySSHCommand(paths, defaultLocalVMID, []string{"-v"})
	if err != nil || len(args) != 5 || args[1] != "-F" || args[3] != alias || args[4] != "-v" {
		t.Fatalf("cage ssh %v %v", args, err)
	}
	values, stderr, code := sshGWith(t, "/usr/bin/ssh", args[2], alias)
	if code != 0 || values["obscurekeystroketiming"] != "no" || values["sessiontype"] != "default" || !strings.Contains(values["proxycommand"], "entry proxy default") {
		t.Fatalf("cage ssh's file: %d %s %v", code, stderr, values)
	}
}

// N3: a subsystem on a channel with a pty is refused; sftp, scp and rsync
// never ask for one.
func TestEntryRefusesSubsystemOnAPty(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	session, err := f.client().NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RequestPty("xterm-256color", 30, 100, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.RequestSubsystem("sftp"); err == nil {
		t.Fatal("sftp ran on a channel with a pty")
	}
}

// N3: the help says exactly what is filtered: sessions with a terminal go
// through cage attach's filter, the others pass the environment's bytes.
func TestEntryHelpSaysExactlyWhatIsFiltered(t *testing.T) {
	var stderr strings.Builder
	EntryMain(nil, filepath.Join(t.TempDir(), "config.json"), nil, io.Discard, &stderr)
	text := stderr.String()
	for _, want := range []string{"ssh -T", "scp", "sftp", "rsync", "不经过滤", "ssh -t -s", "和 cage attach 一样的过滤", "拖入的图片不上传"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "会被拒绝") {
		t.Errorf("the help still says terminal sessions are refused:\n%s", text)
	}
}

// N4: a revoked.json that exists but cannot be read refuses every key and is
// never rewritten from an empty list.
func TestEntryUnreadableRevocationsRefuseEveryKey(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	path := filepath.Join(f.paths.DeviceDir, entryRevokedFile)
	garbage := []byte("{not json")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if client, err := f.dial(f.device); err == nil {
		client.Close()
		t.Fatal("a key was accepted while the revocations could not be read")
	} else if !strings.Contains(entryClientError(err), "撤销记录") {
		t.Fatalf("dial: %v", err)
	}
	if err := entryRevokeKey(f.paths, f.computer.Public); err == nil {
		t.Fatal("revoked into an unreadable list")
	}
	if err := f.call("POST", "/api/entry/on", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "撤销记录") {
		t.Fatalf("on with unreadable revocations: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(garbage) {
		t.Fatalf("revoked.json was rewritten: %q", data)
	}
}

// N4: a revocation that cannot reach a running environment says so; one
// that does not run refuses the key at its next dial.
func TestEntryRevokeReportsAnEnvironmentThatDidNotAnswer(t *testing.T) {
	a := newEntryFixture(t, true)
	a.on()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var held atomic.Int32
	b := entrySecondEnvironment(t, a, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/entry/devices/") && held.Load() == 1 {
				// With the body read, the server sees the caller hang up.
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	b.on()
	held.Store(1)
	var view entryStatusView
	start := time.Now()
	if err := a.call("POST", "/api/entry/devices/"+a.computer.ID+"/revoke", map[string]any{}, &view); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("revoke answered after %v with unreached %v", took, view.Unreached)
	if len(view.Unreached) != 1 || view.Unreached[0] != entryEnvironmentName(b.s.profile) || took > 4*time.Second {
		t.Fatalf("unreached %v after %v", view.Unreached, took)
	}
}

// N5: ~/.ssh/config is changed only when the user owns it and can really
// write it (an immutable flag leaves the mode bits alone); the preview warns
// about a BOM.
func TestEntryIncludeChecksOwnerAndWritability(t *testing.T) {
	paths := entryTestPaths(t)
	if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := entryWriteClient(paths, defaultLocalVMID, "lab1", entryTestHostKey(t)); err != nil {
		t.Fatal(err)
	}
	original := "Host example.org\n  User me\n"
	if err := os.WriteFile(paths.userConfig(), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := entryCurrentUID
	entryCurrentUID = func() int { return os.Getuid() + 1 }
	_, err := entryIncludeApply(paths, entryIncludePreview(paths).Token)
	entryCurrentUID = previous
	if err == nil || !strings.Contains(err.Error(), "Super Cage 不会") || !strings.Contains(err.Error(), paths.includeLine()) {
		t.Fatalf("a file of another user: %v", err)
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("chflags", "uchg", paths.userConfig()).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
		t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", paths.userConfig()).Run() })
		if _, err := entryIncludeApply(paths, entryIncludePreview(paths).Token); err == nil || !strings.Contains(err.Error(), "Super Cage 不会") {
			t.Fatalf("an immutable file: %v", err)
		}
		_ = exec.Command("chflags", "nouchg", paths.userConfig()).Run()
	}
	if data, _ := os.ReadFile(paths.userConfig()); string(data) != original {
		t.Fatalf("changed: %q", data)
	}
	if err := os.WriteFile(paths.userConfig(), []byte("\ufeff"+original), 0o600); err != nil {
		t.Fatal(err)
	}
	if preview := entryIncludePreview(paths); !strings.Contains(preview.Warning, "BOM") {
		t.Fatalf("no BOM warning: %+v", preview)
	}
}

// Kept ssh terminals have their own cap, apart from the shared limit, and a
// kept terminal with no viewer, no output and no running command ends after
// entryKeptIdle; no timer runs while nothing is kept.
func TestEntryKeptTerminalsHaveTheirOwnCapAndExpire(t *testing.T) {
	previousLimit, previousIdle := entryKeptLimit, entryKeptIdle
	entryKeptLimit, entryKeptIdle = 1, 600*time.Millisecond
	t.Cleanup(func() { entryKeptLimit, entryKeptIdle = previousLimit, previousIdle })
	f := newEntryFixture(t, true)
	f.on()
	// Registered after the fixture, so it runs before the limits are put
	// back: nothing is kept and the idle check has stopped by then.
	t.Cleanup(func() {
		for _, session := range f.s.terminals.keptSessions() {
			f.s.terminals.remove(session.id)
		}
		f.s.entryUnkept()
		for deadline := time.Now().Add(10 * time.Second); f.s.entryKeptTimerRunning() && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		if f.s.entryKeptTimerRunning() {
			t.Error("the idle check still runs with nothing kept")
		}
	})
	n := 0
	seen := map[string]bool{}
	open := func(command string) (*entryShell, string) {
		t.Helper()
		n++
		shell := entryOpenShell(t, f.client(), 100, 30, "")
		marker := fmt.Sprintf("R%d=", n)
		shell.send(command + "printf '" + marker + "%s\\n' ready\n")
		shell.expect(marker)
		for _, summary := range entrySSHTerminals(f.s) {
			if id := summary["id"].(string); !seen[id] {
				seen[id] = true
				return shell, id
			}
		}
		t.Fatal("no new ssh terminal")
		return nil, ""
	}
	keep := func(id string) error { return f.call("POST", "/api/terminals/"+id+"/keep", map[string]any{}, nil) }
	busyShell, busy := open("sleep 30 & ")
	if err := keep(busy); err != nil {
		t.Fatal(err)
	}
	_, second := open("")
	if err := keep(second); err == nil {
		t.Fatal("kept more terminals than the cap")
	}
	// A kept terminal does not count against the shared limit.
	if f.s.terminals.keptCount() != 1 {
		t.Fatalf("kept %d", f.s.terminals.keptCount())
	}
	// The busy one (a background sleep) outlives the idle time without a
	// viewer.
	busyShell.session.Close()
	time.Sleep(3 * entryKeptIdle)
	if f.s.terminals.get(busy) == nil {
		t.Fatal("a kept terminal with a running command ended")
	}
	f.s.terminals.remove(busy)
	f.s.terminals.remove(second)
	idleShell, idle := open("")
	if err := keep(idle); err != nil {
		t.Fatal(err)
	}
	idleShell.session.Close()
	entryAwait(t, 10*time.Second, "the idle kept terminal to end", func() bool { return f.s.terminals.get(idle) == nil })
	entryAwait(t, 2*time.Second, "no timer once nothing is kept", func() bool { return !f.s.entryKeptTimerRunning() })
}

// A lock keeps the terminals it cuts within entryKeptLimit; the others end
// and say why. Which ones stay does not depend on the order the connections
// notice the lock: the most recent input wins, then the newest terminal.
func TestEntryLockBeyondTheKeptCapEndsTheTerminal(t *testing.T) {
	previous := entryKeptLimit
	entryKeptLimit = 1
	t.Cleanup(func() { entryKeptLimit = previous })
	f := newEntryFixture(t, true)
	f.on()
	t.Cleanup(func() {
		for _, session := range f.s.terminals.keptSessions() {
			f.s.terminals.remove(session.id)
		}
		f.s.entryUnkept()
		for deadline := time.Now().Add(10 * time.Second); f.s.entryKeptTimerRunning() && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
	})
	client, err := f.dial(f.device)
	if err != nil {
		t.Fatal(err)
	}
	var shells []*entryShell
	var ids []string
	seen := map[string]bool{}
	for i := range 2 {
		shell := entryOpenShell(t, client, 100, 30, "")
		marker := fmt.Sprintf("L%d=", i)
		shell.send("printf '" + marker + "%s\\n' ready\n")
		shell.expect(marker)
		for _, summary := range entrySSHTerminals(f.s) {
			if id := summary["id"].(string); !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		shells = append(shells, shell)
	}
	// The older terminal has the most recent input, so it stays.
	shells[0].send("printf 'AGAIN=%s\\n' yes\n")
	shells[0].expect("AGAIN=")
	ended := make(chan struct{})
	go func() { _ = client.Wait(); close(ended) }()
	f.s.account.lock()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock did not cut the connection")
	}
	entryAwait(t, 5*time.Second, "one terminal left", func() bool { return len(entrySSHTerminals(f.s)) == 1 })
	if kept := f.s.terminals.keptSessions(); len(kept) != 1 || kept[0].id != ids[0] {
		t.Fatalf("kept %d terminals, not the one with the latest input", len(kept))
	}
	if text := shells[0].text(); !strings.Contains(text, entryLockedNotice) {
		t.Errorf("the kept terminal: %q", text[max(0, len(text)-300):])
	}
	if text := shells[1].text(); !strings.Contains(text, "最多保留 1 个本机终端") || !strings.Contains(text, "这个终端随连接结束") {
		t.Errorf("the ended terminal: %q", text[max(0, len(text)-300):])
	}
}

// The order a lock keeps terminals in: most recent input first, then the
// newest.
func TestEntryKeepRankPrefersRecentInputThenNewest(t *testing.T) {
	at := time.Now()
	session := func(id string, sequence int, input time.Time) *terminalSession {
		s := newTerminalSession(nil)
		s.id, s.sequence, s.lastInput = id, sequence, input
		return s
	}
	ranked := entryKeepRank([]*terminalSession{
		session("a", 1, at),
		session("b", 2, at.Add(time.Second)),
		session("c", 3, at.Add(time.Second)),
		session("d", 4, time.Time{}),
	})
	var order []string
	for _, s := range ranked {
		order = append(order, s.id)
	}
	if strings.Join(order, "") != "cbad" {
		t.Fatalf("order %v", order)
	}
}
