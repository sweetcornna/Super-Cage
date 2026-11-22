//go:build !windows

package workbench

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/version"
)

// TestEntryInstalledLayout runs the entry in the installed layout: this
// test binary built as cage with a build stamp, the slim cage-attach of the
// same stamp beside it, and a temporary HOME. The test binary is the
// controller, with the harness's guest stand-in since no VM runs here; the
// client is the system ssh (through the shim), and its ProxyCommand the real
// cage-attach entry proxy, which finds the controller, the device key and the
// ssh folder from HOME as an installed one does and accepts the controller
// only as the companion it verified (controlclient.SameCompanion). It turns
// the entry on, opens a pty session through it and, with CAGE_HERDR_CORE, a
// Herdr link from a real owner core. It runs only when CAGE_ENTRY_INSTALLED
// is set; scripts/entry-installed-layout.sh builds the layout:
//
//	go test -c -tags <the Makefile's> -ldflags "<the Makefile's, one stamp>" -o L/cage ./internal/workbench
//	CGO_ENABLED=0 go build -ldflags "<the same>" -o L/cage-attach ./cmd/cage-attach
//	HOME=<temporary> CAGE_ENTRY_INSTALLED=1 L/cage -test.run '^TestEntryInstalledLayout$'
func TestEntryInstalledLayout(t *testing.T) {
	if os.Getenv("CAGE_ENTRY_INSTALLED") == "" {
		t.Skip("set CAGE_ENTRY_INSTALLED in a layout built as the comment says")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		t.Fatal(err)
	}
	companion := attach.Beside(exe)
	if version.Stamp == "" || companion == "" || filepath.Base(exe) != "cage" {
		t.Fatalf("not an installed layout: %s, stamp %q, companion %q", exe, version.Stamp, companion)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if current, err := user.Current(); err != nil || filepath.Clean(home) == filepath.Clean(current.HomeDir) || !strings.HasPrefix(home, "/private/tmp/") && !strings.HasPrefix(home, "/private/var/folders/") {
		t.Fatalf("HOME must be a temporary folder (resolved), not %s", home)
	}
	entryRealHomeGuard(t)
	s := entryTestControllerOn(t, agentTestControllerOn(t, testServerAt(t, filepath.Join(home, ".claude-cage"))))
	f := newEntryFixtureFor(t, s, true)
	// The paths an installed controller uses, from HOME, except that the
	// block names its files by their full paths: OpenSSH expands ~ from the
	// user database, which is the real home.
	f.paths, err = defaultEntryPaths(s.profile, exe)
	if err != nil {
		t.Fatal(err)
	}
	f.paths.HomeSSH, f.paths.SystemConfig = false, ""
	environment := entryEnvironmentID(s.profile)
	testSSHKnown(t, f.paths.knownHosts(environment))
	if proxy := f.paths.Proxy(environment); proxy != shellescape.Quote(companion)+" entry proxy "+environment {
		t.Fatalf("ProxyCommand %q", proxy)
	}
	f.on()
	config := f.paths.clientFile()
	text, err := os.ReadFile(f.paths.hostFile(environment))
	if err != nil || !strings.Contains(string(text), "ProxyCommand "+shellescape.Quote(companion)+" entry proxy "+environment) {
		t.Fatalf("%s: %v\n%s", f.paths.hostFile(environment), err, text)
	}
	alias := entryAliasIn(string(text))
	t.Logf("layout %s: stamp %s, %s beside it; block %s for %s", filepath.Dir(exe), version.Stamp, filepath.Base(companion), alias, environment)

	// A pty session through the slim proxy.
	calls := len(testSSHCalls())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshExecutable(), "-F", config, "-tt", alias, "tty >/dev/null && echo \"PTY=yes $TERM\"; exit 5")
	cmd.Env = []string{testSSHPath("/usr/bin:/bin:/usr/sbin:/sbin"), "HOME=" + home, "TERM=xterm-256color"}
	out, stderr, code := entryCombined(cmd)
	if code != 5 || !strings.Contains(out, "PTY=yes xterm-256color") {
		t.Fatalf("ssh -tt: %d %q %q", code, out, stderr)
	}
	testSSHCalledSince(t, calls, "/usr/bin/ssh -F "+config+" -tt "+alias)
	t.Logf("ssh -tt %s through %s: %q, exit %d", alias, filepath.Base(companion), strings.TrimSpace(out), code)

	source := os.Getenv("CAGE_HERDR_CORE")
	if source == "" {
		t.Log("no CAGE_HERDR_CORE: no Herdr link")
		return
	}
	// A Herdr link from a real owner core, with the command the page
	// prints. The core's ssh, and the cage-attach it runs, see HOME.
	h := newEntryHerdrCores(t, f, source)
	for i, value := range h.ownerEnv {
		if strings.HasPrefix(value, "HOME=") {
			h.ownerEnv[i] = "HOME=" + home
		}
	}
	view := f.s.entryView()
	if view.Herdr == nil || view.Herdr.Command == "" {
		t.Fatalf("herdr: %+v", view.Herdr)
	}
	calls = len(testSSHCalls())
	if out, code := h.shell(view.Herdr.Command); code != 0 || !strings.Contains(out, "\"name\":\"cage-default\"") {
		t.Fatalf("%s: %d %s", view.Herdr.Command, code, out)
	}
	entryAwait(t, 20*time.Second, "the machine link", func() bool {
		v := f.s.entryView()
		return v.Herdr != nil && v.Herdr.State == "connected" && v.Connections.Herdr == 1
	})
	entryAwait(t, 5*time.Second, "cage-attach entry proxy running", func() bool {
		ps, _ := exec.Command("/bin/ps", "-axo", "command").Output()
		return strings.Contains(string(ps), companion+" entry proxy "+environment)
	})
	out, code = h.hc(false, "run", "--json", "--on", "cage-default", "--", "sh", "-c", "echo ok-$((1+1))")
	if code != 0 || !strings.Contains(out, "ok-2") {
		t.Fatalf("run --on: %d %s", code, out)
	}
	testSSHCalledSince(t, calls, "/usr/bin/ssh -F "+f.paths.cageSSHFile()+" -T cage-default")
	t.Logf("%s, then hc run --on cage-default: ok-2", view.Herdr.Command)
	if err := f.call("POST", "/api/entry/off", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 2*time.Second, "the cut", func() bool { return f.s.entry.registry.counts().Herdr == 0 })
}

// entryAliasIn is the first Host of a generated configuration.
func entryAliasIn(config string) string {
	for _, line := range strings.Split(config, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "Host" {
			return fields[1]
		}
	}
	return ""
}
