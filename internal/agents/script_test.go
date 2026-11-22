package agents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func lookup(t *testing.T, id string) Entry {
	t.Helper()
	e, ok, err := Lookup(id)
	if err != nil || !ok {
		t.Fatalf("lookup %s: %v %v", id, ok, err)
	}
	return e
}

// Every shipped optional entry can be scripted for every architecture it
// pins: the installer's stricter rules must not strand an entry that passes
// the catalog validator.
func TestScriptInputEveryShippedEntry(t *testing.T) {
	entries, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		for _, arch := range []Arch{ARM64, AMD64} {
			for _, action := range []Action{Install, Update, Remove} {
				for _, hidden := range []bool{false, true} {
					_, err := ScriptInput(e, Job{Action: action, Arch: arch, Hidden: hidden})
					_, pinned := e.Artifact(arch)
					want := e.Optional() && (action == Remove || pinned && e.Status != Withdrawn && (hidden || e.Status != Hidden))
					if (err == nil) != want {
						t.Errorf("%s %s %s (hidden allowed: %v): err=%v, want success=%v", e.ID, arch, action, hidden, err, want)
					}
				}
			}
		}
	}
}

func TestScriptInputRefuses(t *testing.T) {
	codex := lookup(t, "codex")
	withdrawn := codex.clone()
	withdrawn.Status = Withdrawn
	noArm := codex.clone()
	delete(noArm.Artifacts, ARM64)
	spaced := codex.clone()
	a := spaced.Artifacts[ARM64]
	a.Entry = "bin/codex aarch64"
	spaced.Artifacts[ARM64] = a
	invalid := codex.clone()
	invalid.Launch.Command = "sudo"
	app := codex.clone()
	app.Kind = App
	app.Session = SessionGeneric
	app.Mark = Mark{Monogram: "Cx"}
	for _, tc := range []struct {
		name string
		e    Entry
		job  Job
		want string
	}{
		{"builtin", lookup(t, "claude-code"), Job{Action: Install, Arch: ARM64}, "comes with the environment"},
		{"builtin remove", lookup(t, "claude-code"), Job{Action: Remove, Arch: ARM64}, "comes with the environment"},
		{"unknown action", codex, Job{Action: Action("repair"), Arch: ARM64, Hidden: true}, "unknown action"},
		{"unknown arch", codex, Job{Action: Install, Arch: Arch("riscv64"), Hidden: true}, "unknown architecture"},
		{"withdrawn install", withdrawn, Job{Action: Install, Arch: ARM64, Hidden: true}, "withdrawn"},
		{"withdrawn update", withdrawn, Job{Action: Update, Arch: ARM64, Hidden: true}, "withdrawn"},
		{"hidden install", codex, Job{Action: Install, Arch: ARM64}, "hidden"},
		{"hidden update", codex, Job{Action: Update, Arch: ARM64}, "hidden"},
		{"no artifact", noArm, Job{Action: Install, Arch: ARM64, Hidden: true}, "no arm64 artifact"},
		{"entry with a space", spaced, Job{Action: Install, Arch: ARM64, Hidden: true}, "characters the installer does not take"},
		{"invalid entry", invalid, Job{Action: Install, Arch: ARM64, Hidden: true}, "launch.command"},
		{"app", app, Job{Action: Install, Arch: ARM64, Hidden: true}, "only terminal clients"},
		{"data on install", codex, Job{Action: Install, Arch: ARM64, Hidden: true, RemoveData: true}, "only for a remove"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ScriptInput(tc.e, tc.job); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	for _, e := range []Entry{withdrawn, noArm, codex} {
		if _, err := ScriptInput(e, Job{Action: Remove, Arch: ARM64, RemoveData: true}); err != nil {
			t.Fatalf("every entry stays removable, hidden, withdrawn or without this architecture: %v", err)
		}
	}
}

// F8: the data directories a remove deletes are decided here, never by the
// caller: a shared one, or one another catalog entry declares, stays.
func TestRemovableData(t *testing.T) {
	codex := lookup(t, "codex")
	got, err := RemovableData(codex)
	if err != nil || !slices.Equal(got, []string{"~/.codex", "~/.local/share/cage/codex-accounts", "~/.local/share/cage/codex-sessions"}) {
		t.Fatalf("codex: %v %v", got, err)
	}
	agy := lookup(t, "antigravity-cli")
	got, err = RemovableData(agy)
	if err != nil || slices.Contains(got, "~/.gemini/config") || !slices.Contains(got, "~/.gemini/antigravity-cli") {
		t.Fatalf("antigravity-cli: the shared ~/.gemini/config must stay: %v %v", got, err)
	}
	// A directory another entry declares stays, whichever of the two is the parent.
	overlapping := codex.clone()
	overlapping.ID = "codex-next"
	overlapping.DataDirs = []DataDir{{Path: "~/.codex/next"}, {Path: "~/.gemini"}, {Path: "~/.config/codex-next"}}
	got, err = RemovableData(overlapping)
	if err != nil || !slices.Equal(got, []string{"~/.config/codex-next"}) {
		t.Fatalf("overlaps: %v %v", got, err)
	}
	input, err := ScriptInput(codex, Job{Action: Remove, Arch: ARM64, RemoveData: true})
	if err != nil || !strings.Contains(input.String(), "AGENT_REMOVE_DATA='~/.codex\n~/.local/share/cage/codex-accounts\n~/.local/share/cage/codex-sessions'\n") {
		t.Fatalf("remove input: %v\n%s", err, input)
	}
	if input, _ := ScriptInput(codex, Job{Action: Remove, Arch: ARM64}); !strings.Contains(input.String(), "AGENT_REMOVE_DATA=''\n") {
		t.Fatalf("a remove without data names none:\n%s", input)
	}
}

var assignment = regexp.MustCompile(`^(AGENT_[A-Z0-9_]+)=`)

func TestScriptInputShape(t *testing.T) {
	script, err := ScriptInput(lookup(t, "codex"), Job{Action: Install, Arch: ARM64, Hidden: true})
	if err != nil {
		t.Fatal(err)
	}
	input := script.String()
	var names []string
	for _, line := range strings.Split(strings.TrimSuffix(input, "\n"), "\n") {
		m := assignment.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("not an assignment: %q", line)
		}
		names = append(names, m[1])
	}
	if !slices.Equal(names, ScriptInputNames) {
		t.Fatalf("names %v", names)
	}
	for _, want := range []string{
		"AGENT_ACTION=install\n", "AGENT_ID=codex\n", "AGENT_VERSION=0.171.0\n", "AGENT_ARCH=arm64\n",
		"AGENT_SHA256=bcf50b836385e51bd2976ab2bbe92b1b08a1cbe37712eb8dbe378aa78d24871c\n", "AGENT_SHA512=''\n",
		"AGENT_SIZE=124436451\n", "AGENT_UNPACKED=309207448\n", "AGENT_ENTRY=codex-aarch64-unknown-linux-musl\n",
		"AGENT_PACKAGES=bubblewrap\n", "AGENT_USERNS=1\n", "AGENT_KEYRING=0\n", "AGENT_COMMAND=codex\n",
		"AGENT_URL=https://github.com/openai/codex/releases/download/rust-v0.171.0/codex-aarch64-unknown-linux-musl.tar.gz\n",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("input lacks %q:\n%s", want, input)
		}
	}
	removeScript, err := ScriptInput(lookup(t, "antigravity-cli"), Job{Action: Remove, Arch: AMD64})
	remove := removeScript.String()
	if err != nil || !strings.Contains(remove, "AGENT_URL=''\n") || !strings.Contains(remove, "AGENT_ENTRY=antigravity\n") ||
		!strings.Contains(remove, "AGENT_SHA512=83311d3f") || !strings.Contains(remove, "AGENT_ENV=AGY_CLI_DISABLE_AUTO_UPDATE=true\n") {
		t.Fatalf("remove input carries only what a remove needs: %v\n%s", err, remove)
	}
}

// Values that look like shell code reach the script as data, through bash and
// a POSIX sh alike, and run nothing.
func TestScriptInputQuotesHostileValues(t *testing.T) {
	dir := t.TempDir()
	// Commands in the values touch ./pwned, relative to the shell's directory.
	canary, touch := filepath.Join(dir, "pwned"), "pwned"
	e := lookup(t, "codex")
	e.Name = "Codex'; touch " + touch + "; echo '"
	e.Vendor = "$(touch " + touch + ") `touch " + touch + "` \"\\ ${HOME}"
	hostileDir := "~/.codex it's $(touch " + touch + ")"
	e.DataDirs = append(e.DataDirs, DataDir{Path: hostileDir})
	e.Settings = append(e.Settings, Setting{File: hostileDir + "/a'b.json", Format: "json", Set: map[string]any{
		"note": "it's \"quoted\" $(touch " + touch + ") `id` \\ ; rm -rf ~",
	}})
	if err := (Catalog{Schema: SchemaVersion, Entries: []Entry{e}}).Validate(); err != nil {
		t.Fatalf("the hostile values must pass the catalog rules for this test to mean anything: %v", err)
	}
	for _, action := range []Action{Install, Remove} {
		var data []string
		if action == Remove {
			data, _ = RemovableData(e)
			if !slices.Contains(data, hostileDir) {
				t.Fatalf("the hostile directory is not removable: %v", data)
			}
		}
		script, err := ScriptInput(e, Job{Action: action, Arch: ARM64, RemoveData: action == Remove, Hidden: true})
		if err != nil {
			t.Fatal(err)
		}
		input := script.String()
		for _, shell := range []string{"bash", "sh"} {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Logf("%s unavailable", shell)
				continue
			}
			var print strings.Builder
			for _, name := range ScriptInputNames {
				print.WriteString("printf '%s\\0' \"$" + name + "\"\n")
			}
			cmd := exec.Command(path, "-c", input+print.String())
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: %v", shell, err)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			settings, _ := json.Marshal(e.Settings)
			want := map[string]string{
				"AGENT_NAME": e.Name, "AGENT_VENDOR": e.Vendor, "AGENT_SETTINGS": string(settings),
				"AGENT_REMOVE_DATA": strings.Join(data, "\n"), "AGENT_ACTION": string(action),
			}
			if len(got) != len(ScriptInputNames) {
				t.Fatalf("%s %s: %d values for %d names", shell, action, len(got), len(ScriptInputNames))
			}
			for i, name := range ScriptInputNames {
				if value, ok := want[name]; ok && got[i] != value {
					t.Errorf("%s %s: %s = %q, want %q", shell, action, name, got[i], value)
				}
			}
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("%s ran a value as code", shell)
			}
		}
	}
}
