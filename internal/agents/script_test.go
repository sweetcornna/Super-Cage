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
				_, err := ScriptInput(e, arch, action, nil)
				_, pinned := e.Artifact(arch)
				want := e.Optional() && (action == Remove || pinned && e.Status != Withdrawn)
				if (err == nil) != want {
					t.Errorf("%s %s %s: err=%v, want success=%v", e.ID, arch, action, err, want)
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
		name   string
		e      Entry
		arch   Arch
		action Action
		data   []string
		want   string
	}{
		{"builtin", lookup(t, "claude-code"), ARM64, Install, nil, "comes with the environment"},
		{"builtin remove", lookup(t, "claude-code"), ARM64, Remove, nil, "comes with the environment"},
		{"unknown action", codex, ARM64, Action("repair"), nil, "unknown action"},
		{"unknown arch", codex, Arch("riscv64"), Install, nil, "unknown architecture"},
		{"withdrawn install", withdrawn, ARM64, Install, nil, "withdrawn"},
		{"withdrawn update", withdrawn, ARM64, Update, nil, "withdrawn"},
		{"no artifact", noArm, ARM64, Install, nil, "no arm64 artifact"},
		{"entry with a space", spaced, ARM64, Install, nil, "characters the installer does not take"},
		{"invalid entry", invalid, ARM64, Install, nil, "launch.command"},
		{"app", app, ARM64, Install, nil, "only terminal clients"},
		{"data on install", codex, ARM64, Install, []string{"~/.codex"}, "only for a remove"},
		{"data not declared", codex, ARM64, Remove, []string{"~/.ssh"}, "not one of its data directories"},
		{"data listed twice", codex, ARM64, Remove, []string{"~/.codex", "~/.codex"}, "listed twice"},
		{"data parent", codex, ARM64, Remove, []string{"~/.local/share/cage"}, "not one of its data directories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ScriptInput(tc.e, tc.arch, tc.action, tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := ScriptInput(withdrawn, ARM64, Remove, []string{"~/.codex"}); err != nil {
		t.Fatalf("a withdrawn entry must stay removable: %v", err)
	}
	if _, err := ScriptInput(noArm, ARM64, Remove, nil); err != nil {
		t.Fatalf("an entry without this architecture must stay removable: %v", err)
	}
}

var assignment = regexp.MustCompile(`^(AGENT_[A-Z0-9_]+)=`)

func TestScriptInputShape(t *testing.T) {
	input, err := ScriptInput(lookup(t, "codex"), ARM64, Install, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	remove, err := ScriptInput(lookup(t, "antigravity-cli"), AMD64, Remove, nil)
	if err != nil || !strings.Contains(remove, "AGENT_URL=''\n") || !strings.Contains(remove, "AGENT_ENTRY=''\n") ||
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
			data = []string{hostileDir, "~/.codex"}
		}
		input, err := ScriptInput(e, ARM64, action, data)
		if err != nil {
			t.Fatal(err)
		}
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
