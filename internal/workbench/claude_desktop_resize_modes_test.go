//go:build !windows

package workbench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stand-in for xrandr with the server's state in files: the modes that
// exist, the modes added to VNC-0 and the mode in use. Every call is logged.
// It refuses what the real tool refuses: an existing name, an unknown mode, a
// mode that is on the output already, and removing the mode in use.
const fakeRandR = `#!/bin/sh
echo "$*" >> "$CAGE_RESIZE_FIXTURE/calls"
state="$CAGE_RESIZE_FIXTURE/randr"
touch "$state/server" "$state/output" "$state/current"
case "$1" in
--query)
  echo "Screen 0: minimum 64 x 64, current 1000 x 800"
  echo "VNC-0 connected primary"
  while read -r mode; do echo "   $mode     60.01*+"; done < "$state/output"
  echo "   1024x768     60.00" ;;
--newmode) if grep -qx -- "$2" "$state/server"; then echo "BadName" >&2; exit 1; fi; echo "$2" >> "$state/server" ;;
--addmode) grep -qx -- "$3" "$state/server" || exit 1; if grep -qx -- "$3" "$state/output"; then echo "BadMatch" >&2; exit 1; fi; echo "$3" >> "$state/output" ;;
--output) grep -qx -- "$4" "$state/output" || { echo "xrandr: cannot find mode $4" >&2; exit 1; }; echo "$4" > "$state/current" ;;
--delmode) if [ "$(cat "$state/current")" = "$3" ]; then exit 1; fi; grep -vx -- "$3" "$state/output" > "$state/output.new"; mv "$state/output.new" "$state/output" ;;
--rmmode) if grep -qx -- "$2" "$state/output"; then exit 1; fi; grep -vx -- "$2" "$state/server" > "$state/server.new"; mv "$state/server.new" "$state/server" ;;
esac
exit 0
`

type desktopResizeFixture struct {
	t                   *testing.T
	bash, root, desktop string
	bin                 string
}

func newDesktopResizeFixture(t *testing.T) *desktopResizeFixture {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required")
	}
	// go test already executes its test binary from this filesystem; TMPDIR can be noexec.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(filepath.Dir(executable), "cage-resize-modes-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := &desktopResizeFixture{t: t, bash: bash, root: root, desktop: filepath.Join(root, "desktop"), bin: filepath.Join(root, "bin")}
	for _, directory := range []string{f.desktop, f.bin, filepath.Join(root, "randr")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CAGE_RESIZE_FIXTURE", root)
	f.write(filepath.Join(f.bin, "xrandr"), fakeRandR, 0o700)
	// Not every platform has flock; the script only needs it to return.
	f.write(filepath.Join(f.bin, "flock"), "#!/bin/sh\nexit 0\n", 0o700)
	return f
}

func (f *desktopResizeFixture) write(path, content string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *desktopResizeFixture) read(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func (f *desktopResizeFixture) resize(width, height int) {
	f.t.Helper()
	script := strings.NewReplacer("/run/cage-desktop", f.desktop, "/run/cage-phone-desktop", filepath.Join(f.root, "phone")).Replace(desktopResizeScript(width, height, ""))
	cmd := exec.Command(f.bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"))
	// The caller's guest session waits for this output to close.
	done := make(chan struct{})
	var output []byte
	var err error
	go func() { output, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		f.t.Fatal("the resize script did not finish; a background step holds its output")
	}
	if err != nil {
		f.t.Fatalf("resize %dx%d: %v\n%s", width, height, err, output)
	}
}

// callList returns the xrandr invocations since the last call to it.
func (f *desktopResizeFixture) callList() []string {
	path := filepath.Join(f.root, "calls")
	text := strings.TrimSpace(f.read(path))
	_ = os.Remove(path)
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (f *desktopResizeFixture) current() string {
	return strings.TrimSpace(f.read(filepath.Join(f.root, "randr", "current")))
}

func (f *desktopResizeFixture) serverModes() []string {
	return strings.Fields(f.read(filepath.Join(f.root, "randr", "server")))
}

func TestDesktopResizeScriptSwitchesWithFewXrandrCalls(t *testing.T) {
	f := newDesktopResizeFixture(t)
	hasPrefix := func(calls []string, prefix string) bool {
		for _, call := range calls {
			if strings.HasPrefix(call, prefix) {
				return true
			}
		}
		return false
	}

	// A new size takes three calls and no query, and removes nothing yet. The
	// first run without a list also sweeps once, after it, for modes that an
	// earlier version left behind.
	f.resize(1000, 800)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.read(filepath.Join(f.root, "calls")), "--query") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	calls := f.callList()
	if len(calls) != 4 || calls[3] != "--query" || !strings.HasPrefix(calls[0], "--newmode cage-1000x800 ") || calls[1] != "--addmode VNC-0 cage-1000x800" || calls[2] != "--output VNC-0 --mode cage-1000x800" {
		t.Fatalf("first size: %q", calls)
	}
	f.resize(900, 700)
	if calls = f.callList(); len(calls) != 3 || hasPrefix(calls, "--query") || hasPrefix(calls, "--delmode") {
		t.Fatalf("second size: %q", calls)
	}
	// Going back to a size that is still defined is one call: a sidebar that
	// opens and closes must not rebuild a mode each time.
	f.resize(1000, 800)
	if calls = f.callList(); len(calls) != 1 || calls[0] != "--output VNC-0 --mode cage-1000x800" {
		t.Fatalf("returning to a defined size: %q", calls)
	}
	f.resize(900, 700)
	if calls = f.callList(); len(calls) != 1 || calls[0] != "--output VNC-0 --mode cage-900x700" {
		t.Fatalf("toggling: %q", calls)
	}
	if got := strings.Fields(f.read(filepath.Join(f.desktop, "resize-modes"))); strings.Join(got, " ") != "cage-1000x800 cage-900x700" {
		t.Fatalf("recent sizes: %q", got)
	}
}

func TestDesktopResizeScriptRemovesOldModesAfterTheSwitch(t *testing.T) {
	f := newDesktopResizeFixture(t)
	for _, size := range [][2]int{{1000, 800}, {900, 700}, {800, 600}, {700, 500}} {
		f.resize(size[0], size[1])
	}
	// The first run with no list sweeps once for modes an earlier version left.
	f.callList()
	waitFor := func(want []string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for strings.Join(f.serverModes(), " ") != strings.Join(want, " ") && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := f.serverModes(); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("modes on the server: %q, want %q", got, want)
		}
	}
	waitFor([]string{"cage-1000x800", "cage-900x700", "cage-800x600", "cage-700x500"})
	// A fifth size evicts the oldest. The switch itself is three calls; the
	// removal is not among them.
	f.resize(600, 400)
	var calls []string
	waitFor([]string{"cage-900x700", "cage-800x600", "cage-700x500", "cage-600x400"})
	calls = f.callList()
	if len(calls) < 3 || !strings.HasPrefix(calls[0], "--newmode cage-600x400 ") || calls[1] != "--addmode VNC-0 cage-600x400" || calls[2] != "--output VNC-0 --mode cage-600x400" {
		t.Fatalf("switch before the removal: %q", calls)
	}
	if f.current() != "cage-600x400" {
		t.Fatalf("mode in use: %q", f.current())
	}
}

// A guest whose Claude Desktop session still sits at a mode an earlier version
// made has the mode on the output and no list. The first resize there must
// not depend on how X treats adding a mode twice.
func TestDesktopResizeScriptAcceptsAModeThatIsAlreadyOnTheOutput(t *testing.T) {
	f := newDesktopResizeFixture(t)
	f.write(filepath.Join(f.root, "randr", "server"), "cage-1000x800\ncage-640x480\n", 0o600)
	f.write(filepath.Join(f.root, "randr", "output"), "cage-1000x800\ncage-640x480\n", 0o600)
	f.write(filepath.Join(f.root, "randr", "current"), "cage-640x480\n", 0o600)
	f.resize(1000, 800)
	if f.current() != "cage-1000x800" {
		t.Fatalf("the mode already on the output was not used: %q", f.current())
	}
	// The sweep for modes of an earlier version removes the one that is not in use.
	deadline := time.Now().Add(5 * time.Second)
	for strings.Contains(strings.Join(f.serverModes(), " "), "cage-640x480") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.serverModes(); strings.Join(got, " ") != "cage-1000x800" {
		t.Fatalf("modes after the sweep: %q", got)
	}
}

func TestDesktopResizeScriptRecoversWhenTheServerLostItsModes(t *testing.T) {
	f := newDesktopResizeFixture(t)
	f.resize(1000, 800)
	f.callList()
	// The display restarted: the list says the mode exists, the server disagrees.
	for _, name := range []string{"server", "output", "current"} {
		if err := os.Remove(filepath.Join(f.root, "randr", name)); err != nil {
			t.Fatal(err)
		}
	}
	f.resize(1000, 800)
	if f.current() != "cage-1000x800" {
		t.Fatalf("size not applied after the server lost its modes: %q", f.current())
	}
	// A mode created earlier without being added must not wedge later resizes.
	f.write(filepath.Join(f.root, "randr", "output"), "", 0o600)
	if err := os.Remove(filepath.Join(f.desktop, "resize-modes")); err != nil {
		t.Fatal(err)
	}
	f.resize(1000, 800)
	if f.current() != "cage-1000x800" {
		t.Fatalf("an existing but unadded mode blocked the resize: %q", f.current())
	}
}
