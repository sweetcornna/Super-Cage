package localterm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// The cage binary of the app bundle has a space in its path.
var attachCommand = Command{"/Applications/Super Cage.app/Contents/MacOS/cage", "attach", "--terminal", "0123456789abcdef"}

func installed(t *testing.T, id string) Installed {
	t.Helper()
	for _, app := range macApps {
		if app.ID == id {
			return Installed{App: app, Path: "/Applications/" + app.Name + ".app"}
		}
	}
	t.Fatal("unknown app", id)
	return Installed{}
}

func planFor(t *testing.T, id string) (Plan, string) {
	t.Helper()
	scratch := t.TempDir()
	plan, err := PlanLaunch(PlanInput{App: installed(t, id), Command: attachCommand, Name: "0123456789abcdef", Scratch: scratch, Home: "/Users/owner name"})
	if err != nil {
		t.Fatal(id, err)
	}
	return plan, filepath.Join(scratch, "super-cage-0123456789abcdef.command")
}

func TestPlansPerApplication(t *testing.T) {
	quotedCage := "'/Applications/Super Cage.app/Contents/MacOS/cage'"
	for _, row := range []struct {
		id    string
		steps func(script string) [][]string
	}{
		{"terminal", func(s string) [][]string { return [][]string{{"/usr/bin/open", "-a", "/Applications/终端.app", s}} }},
		{"iterm2", func(s string) [][]string {
			return [][]string{{"/usr/bin/osascript", "-e", "tell application id \"com.googlecode.iterm2\" to create window with default profile command \"" + s + "\""}}
		}},
		{"ghostty", func(s string) [][]string {
			return [][]string{
				{"/usr/bin/osascript", "-e", "tell application id \"com.mitchellh.ghostty\"\nset cfg to new surface configuration\nset command of cfg to \"" + s + "\"\nnew window with configuration cfg\nend tell"},
				{"/usr/bin/open", "-na", "/Applications/Ghostty.app", "--args", "-e", s},
			}
		}},
		{"warp", func(string) [][]string {
			return [][]string{{"/usr/bin/open", "warp://launch/super-cage-0123456789abcdef.yaml"}}
		}},
		{"wezterm", func(string) [][]string {
			return [][]string{append([]string{"/Applications/WezTerm.app/Contents/MacOS/wezterm", "start", "--"}, attachCommand...)}
		}},
		{"kitty", func(string) [][]string {
			return [][]string{append([]string{"/Applications/kitty.app/Contents/MacOS/kitty", "--single-instance"}, attachCommand...)}
		}},
		{"alacritty", func(string) [][]string {
			return [][]string{
				append([]string{"/Applications/Alacritty.app/Contents/MacOS/alacritty", "msg", "create-window", "-e"}, attachCommand...),
				append([]string{"/usr/bin/open", "-na", "/Applications/Alacritty.app", "--args", "-e"}, attachCommand...),
			}
		}},
	} {
		plan, script := planFor(t, row.id)
		want := row.steps(script)
		if len(plan.Steps) != len(want) {
			t.Fatalf("%s: %d steps, want %d", row.id, len(plan.Steps), len(want))
		}
		for i, step := range plan.Steps {
			if strings.Join(step.Argv, "\x00") != strings.Join(want[i], "\x00") {
				t.Errorf("%s step %d:\n got %q\nwant %q", row.id, i, step.Argv, want[i])
			}
		}
		for _, f := range plan.Files {
			if strings.HasSuffix(f.Path, ".command") {
				if f.Mode != 0o700 || !strings.Contains(f.Content, "rm -f -- \"$0\"\nexec "+quotedCage+" attach --terminal 0123456789abcdef\n") {
					t.Errorf("%s script %o %q", row.id, f.Mode, f.Content)
				}
			}
		}
	}
	// Warp's launch configuration is valid YAML that runs the script.
	plan, script := planFor(t, "warp")
	var config struct {
		Windows []struct {
			Tabs []struct {
				Layout struct {
					Cwd      string `yaml:"cwd"`
					Commands []struct {
						Exec string `yaml:"exec"`
					} `yaml:"commands"`
				} `yaml:"layout"`
			} `yaml:"tabs"`
		} `yaml:"windows"`
	}
	if len(plan.Files) != 2 || plan.Files[1].Path != "/Users/owner name/.warp/launch_configurations/super-cage-0123456789abcdef.yaml" {
		t.Fatal(plan.Files)
	}
	if err := yaml.Unmarshal([]byte(plan.Files[1].Content), &config); err != nil || config.Windows[0].Tabs[0].Layout.Commands[0].Exec != script || config.Windows[0].Tabs[0].Layout.Cwd != "/Users/owner name" {
		t.Fatalf("%v %+v", err, config)
	}
	// Herdr is detected but cannot host cage attach yet.
	if _, err := PlanLaunch(PlanInput{App: installed(t, "herdr"), Command: attachCommand, Name: "x", Scratch: "/tmp", Home: "/Users/x"}); err == nil {
		t.Fatal("herdr planned")
	}
}

func TestPlanRefusesWhatItCouldMisread(t *testing.T) {
	app := installed(t, "terminal")
	for _, command := range []Command{{"cage", "attach"}, {"/bin/cage", "attach\n--terminal"}, {"/bin/cage", ""}, {}} {
		if _, err := PlanLaunch(PlanInput{App: app, Command: command, Name: "a", Scratch: "/tmp"}); err == nil {
			t.Errorf("accepted %q", command)
		}
	}
	for _, name := range []string{"", "../x", "A", "a b"} {
		if _, err := PlanLaunch(PlanInput{App: app, Command: attachCommand, Name: name, Scratch: "/tmp"}); err == nil {
			t.Errorf("accepted name %q", name)
		}
	}
}

// Each quoting layer is checked by its own interpreter.
func TestQuotingLayers(t *testing.T) {
	words := []string{"/Applications/Super Cage.app/Contents/MacOS/cage", "it's", "a\"b", "$HOME", "`id`", "中文 路径", "\\back", "*", "plain-word"}
	line := Command(words).ShellLine()
	out, err := exec.Command("/bin/sh", "-c", "for a in "+line+"; do printf '%s\\n' \"$a\"; done").Output()
	if err != nil || string(out) != strings.Join(words, "\n")+"\n" {
		t.Fatalf("shell: %q %v", out, err)
	}
	if runtime.GOOS == "darwin" {
		// osascript only evaluates a string here; no application is addressed.
		for _, word := range words {
			out, err := exec.Command("/usr/bin/osascript", "-e", "return "+AppleScriptString(word)).Output()
			if err != nil || strings.TrimSuffix(string(out), "\n") != word {
				t.Errorf("AppleScript %q: %q %v", word, out, err)
			}
		}
	}
	for _, word := range append(words, "tab\there", "line\nbreak") {
		var holder struct {
			V string `yaml:"v"`
		}
		if err := yaml.Unmarshal([]byte("v: "+YAMLString(word)), &holder); err != nil || holder.V != word {
			t.Errorf("YAML %q: %q %v", word, holder.V, err)
		}
	}
}

type recordingRunner struct {
	steps [][]string
	fail  int
}

func (r *recordingRunner) Run(_ context.Context, step Step) error {
	r.steps = append(r.steps, step.Argv)
	if len(r.steps) <= r.fail {
		return errors.New("refused")
	}
	return nil
}

func TestLaunchWritesPrivateFilesAndFallsBack(t *testing.T) {
	plan, script := planFor(t, "ghostty")
	runner := &recordingRunner{fail: 1} // the owner refused Automation
	if err := Launch(context.Background(), plan, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.steps) != 2 || runner.steps[1][1] != "-na" {
		t.Fatal(runner.steps)
	}
	info, err := os.Stat(script)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal(info, err)
	}
	// Scripts never run are removed later.
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(script, old, old)
	PruneScratch(filepath.Dir(script), 10*time.Minute)
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Fatal("stale script kept")
	}
	runner = &recordingRunner{fail: 9}
	if err := Launch(context.Background(), plan, runner); err == nil {
		t.Fatal("all steps failed but launch succeeded")
	}
}

func TestDetectWithoutPermission(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS table")
	}
	start := time.Now()
	found := Detect()
	elapsed := time.Since(start)
	var terminal bool
	for _, app := range found {
		terminal = terminal || app.ID == "terminal" && strings.HasSuffix(app.Path, "/Terminal.app")
	}
	if !terminal {
		t.Fatalf("Terminal.app not detected: %+v", found)
	}
	t.Logf("detected %d of %d known applications in %v: %+v", len(found), len(macApps), elapsed, found)
}

func TestSettingsDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	settings, err := LoadSettings(dir)
	if err != nil || settings != DefaultSettings() || !settings.ImageUpload || !settings.ClipboardWrite || settings.App != "" {
		t.Fatal(settings, err)
	}
	if err := os.WriteFile(filepath.Join(dir, SettingsFile), []byte("{\"app\":\"ghostty\",\"clipboard_write\":false}"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, _ = LoadSettings(dir)
	if runtime.GOOS == "darwin" && (settings.App != "ghostty" || settings.ClipboardWrite || !settings.ImageUpload) {
		t.Fatal(settings)
	}
	if (Settings{App: "no-such-app"}).Validate() == nil {
		t.Fatal("unknown app accepted")
	}
}
