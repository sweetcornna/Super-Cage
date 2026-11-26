package cage

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/guestprofile"
)

// The governor runs as root in the guest; its own tests use a fake cgroup
// root, /run directory and systemctl.
func TestResourceGovernorPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the governor and its fakes are for Linux guests")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	output, err := exec.Command(python, "-I", "resource-governor.test.py").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "OK") {
		t.Fatalf("governor tests: %v\n%s", err, output)
	}
}

// The governor's report is what the probe parses.
func TestGovernorReportFormatMatchesTheProbe(t *testing.T) {
	report := []string{"governor 1 123.5", "class control 419430400 0.50 603979776 0 0", "class sessions 537919488 0.00 872415232 0 0", "class browser 1782579200 31.00 0 4294967296 4697620480"}
	classes := parseGovernor(report, 125)
	if len(classes) != 3 || classes["browser"].High != 4294967296 || classes["control"].Protected != 603979776 || classes["browser"].Stall != 31 {
		t.Fatalf("parsed %+v", classes)
	}
}

// A governor that cannot resolve its accounts reports no classes and a line
// saying so: the probe (this one, and the previous host agent's, which shares
// the parsing) reads the whole guest's pressure, as for a guest without one.
func TestUnresolvedGovernorReportFallsBackToTheWholeGuest(t *testing.T) {
	probe := fmt.Sprintf("MemTotal: %d kB\nMemFree: 65536 kB\nMemAvailable: 131072 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\noom_kill 0\npswpin 0\nsome avg10=20.00 avg60=0 total=1\ncpu 10 0 5 20 2 0 0 0 0 0\nboot 12345678-1234-1234-1234-123456789012\nuptime 100.5 1.0\ngovernor 1 100.0\naccounts unresolved session browser\n", (3<<30)>>10)
	sample, err := parseResources([]byte(probe))
	if err != nil || sample.Protected || sample.Stall != 20 || len(sample.classes) != 0 {
		t.Fatalf("sample %+v, %v", sample, err)
	}
	if pressure, _ := guestPressure(sample); !pressure {
		t.Fatal("the whole guest's stall is not pressure")
	}
}

// The rename rules for the governor's accounts match only its unit's two
// lines: no other template text changes with them.
func TestGovernorAccountRulesMatchOnlyItsUnit(t *testing.T) {
	for name, script := range map[string]string{"setup": GuestSetup, "start": componentStartScript()} {
		unit := heredoc(t, script, "cage-resource-governor.service >/dev/null <<'UNIT'\n", "\nUNIT\n")
		for _, line := range []string{"Environment=CAGE_SESSION_USER=cage\n", "Environment=CAGE_BROWSER_USER=cage-browser\n"} {
			if !strings.Contains(unit+"\n", line) {
				t.Fatalf("%s: the unit lacks %q", name, line)
			}
		}
		for _, prefix := range []string{"CAGE_SESSION_USER=", "CAGE_BROWSER_USER="} {
			if count := strings.Count(script, prefix); count != 1 {
				t.Fatalf("%s: %q occurs %d times; the rename rule for it would change other text", name, prefix, count)
			}
		}
	}
}

// renderedGovernorRun runs a rendered governor once in a fake guest whose
// accounts are users (name to uid), under the environment of its rendered
// unit, and returns the systemctl calls and the state it wrote.
const renderedGovernorRun = `import importlib.util, json, os, pwd, sys
from pathlib import Path
path, root, users, environment = sys.argv[1], Path(sys.argv[2]), json.loads(sys.argv[3]), json.loads(sys.argv[4])
os.environ.update(environment)
class Entry:
    def __init__(self, uid):
        self.pw_uid = uid
def getpwnam(name):
    if name in users:
        return Entry(users[name])
    raise KeyError(name)
pwd.getpwnam = getpwnam
spec = importlib.util.spec_from_file_location('governor', path)
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)
for name in ('cgroup', 'run/in', 'proc/self'):
    (root / name).mkdir(parents=True, exist_ok=True)
(root / 'cgroup' / 'cgroup.controllers').write_text('cpu memory\n')
for unit, size in (('system.slice', 400), ('user.slice', 1600), ('cage.slice/cage-browser.slice', 100), ('user.slice/user-1000.slice', 400), ('user.slice/user-1001.slice', 1200)):
    directory = root / 'cgroup' / unit
    directory.mkdir(parents=True, exist_ok=True)
    (directory / 'memory.current').write_text('%d\n' % (size << 20))
    (directory / 'memory.pressure').write_text('some avg10=0.00 avg60=0.00 avg300=0.00 total=1\n')
(root / 'proc' / 'meminfo').write_text('MemTotal: %d kB\n' % (4 << 20))
(root / 'proc' / 'uptime').write_text('100.0 50.0\n')
(root / 'proc' / 'self' / 'mounts').write_text('cgroup2 %s cgroup2 rw,nsdelegate,memory_recursiveprot 0 0\n' % (root / 'cgroup'))
(root / 'proc' / 'mounts').symlink_to('self/mounts')
calls = root / 'calls'
recorder = root / 'systemctl'
recorder.write_text('#!/bin/sh\nprintf "%%s\\n" "$*" >> %s\n' % calls)
recorder.chmod(0o755)
g.CGROUP, g.RUN, g.MEMINFO, g.UPTIME, g.MOUNTS = str(root / 'cgroup'), str(root / 'run'), str(root / 'proc' / 'meminfo'), str(root / 'proc' / 'uptime'), str(root / g.MOUNTS.lstrip('/'))
g.SYSTEMCTL = [str(recorder)]
g.Governor().once()
print(json.dumps({'calls': calls.read_text().splitlines() if calls.exists() else [], 'state': (root / 'run' / 'state').read_text().splitlines()}))
`

// heredoc returns the body of the here-document that starts with start.
func heredoc(t *testing.T, script, start, end string) string {
	t.Helper()
	_, rest, found := strings.Cut(script, start)
	body, _, closed := strings.Cut(rest, end)
	if !found || !closed {
		t.Fatalf("no %q here-document in the rendered script", start)
	}
	return body
}

// The governor learns its two accounts from its unit, which the same
// rendering that renames every other account reference (guestprofile Linux)
// writes: for the default accounts (no profile) and for random ones, in the
// setup script and in the component start script, the rendered governor run
// under its rendered unit finds both users' slices.
func TestRenderedGovernorFindsTheProfileUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the governor and its fakes are for Linux guests")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	random, err := guestprofile.New(guestprofile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := guestprofile.New(guestprofile.Options{Username: "alex", BrowserUsername: "jordan"})
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []*guestprofile.Profile{nil, chosen, random} {
		config := Config{GuestProfile: profile}
		for name, script := range map[string]string{"setup": config.SetupScript(), "start": config.StartScript()} {
			session, browser := profile.WorkUser(), profile.BrowserUser()
			governor := heredoc(t, script, "<<'CAGE_RESOURCE_GOVERNOR'\n", "\nCAGE_RESOURCE_GOVERNOR\n")
			unit := heredoc(t, script, "cage-resource-governor.service >/dev/null <<'UNIT'\n", "\nUNIT\n")
			if !strings.Contains(unit, "install -d -o "+session+" -g "+session+" -m 0755 /run/cage-resources/in") {
				t.Fatalf("%s %s: the limits directory is not the session user's:\n%s", name, session, unit)
			}
			environment := map[string]string{}
			for _, line := range strings.Split(unit, "\n") {
				if assignment, ok := strings.CutPrefix(line, "Environment="); ok {
					key, value, _ := strings.Cut(assignment, "=")
					environment[key] = value
				}
			}
			directory := t.TempDir()
			path := filepath.Join(directory, "governor.py")
			if err := os.WriteFile(path, []byte(governor), 0o600); err != nil {
				t.Fatal(err)
			}
			users, _ := json.Marshal(map[string]int{session: 1000, browser: 1001})
			env, _ := json.Marshal(environment)
			command := exec.Command(python, "-I", "-c", renderedGovernorRun, path, filepath.Join(directory, "guest"), string(users), string(env))
			var stderr strings.Builder
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				t.Fatalf("%s %s/%s: %v\n%s%s", name, session, browser, err, output, stderr.String())
			}
			var result struct{ Calls, State []string }
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("%s: %v\n%s", name, err, output)
			}
			calls := strings.Join(result.Calls, "\n")
			for _, want := range []string{"user-1001.slice MemoryHigh=", "user-1001.slice MemoryMax=", "user-1000.slice MemoryLow="} {
				if !strings.Contains(calls, "set-property --runtime "+want) {
					t.Errorf("%s %s/%s: no %q in\n%s\nstate %q", name, session, browser, want, calls, result.State)
				}
			}
			// The browser user's 1200 MiB count as browser, not as sessions.
			state := strings.Join(result.State, "\n")
			if !strings.Contains(state, "class sessions 419430400 ") || !strings.Contains(state, "class browser 1363148800 ") {
				t.Errorf("%s %s/%s: classes\n%s", name, session, browser, state)
			}
		}
	}
}
