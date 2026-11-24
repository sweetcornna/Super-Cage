package localterm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The cage binary of the app bundle has a space in its path.
var attachCommand = Command{"/Applications/Super Cage.app/Contents/MacOS/cage", "attach", "--terminal", "0123456789abcdef"}

// installed is an application of the table as if found at its usual place.
func installed(t *testing.T, id string) Installed {
	t.Helper()
	paths := map[string]string{
		"herdr":    "/Applications/Herdr.app",
		"ghostty":  "/Applications/Ghostty.app",
		"terminal": "/System/Applications/Utilities/Terminal.app",
	}
	for _, app := range macApps {
		if app.ID == id {
			return Installed{App: app, Path: paths[id]}
		}
	}
	t.Fatal("unknown app", id)
	return Installed{}
}

func planFor(t *testing.T, id string) (Plan, string) {
	t.Helper()
	scratch := t.TempDir()
	plan, err := PlanLaunch(PlanInput{App: installed(t, id), Command: attachCommand, Name: "0123456789abcdef", Scratch: scratch})
	if err != nil {
		t.Fatal(id, err)
	}
	return plan, filepath.Join(scratch, "super-cage-0123456789abcdef.command")
}

// The table keeps three applications in priority order (Herdr, Ghostty,
// system Terminal), and each opens its window its own way: Herdr uses its own
// control CLI (no Automation prompt), Ghostty gets a window through AppleScript
// (after macOS's Automation question), and Terminal runs the script as a document.
func TestPlansPerApplication(t *testing.T) {
	if got := len(macApps); got != 3 || macApps[0].ID != "herdr" || macApps[1].ID != "ghostty" || macApps[2].ID != "terminal" {
		t.Fatalf("the table is %+v", macApps)
	}
	if macApps[0].Automation || !macApps[1].Automation || macApps[2].Automation {
		t.Fatalf("unexpected automation flags: %+v", macApps)
	}
	quotedCage := "'/Applications/Super Cage.app/Contents/MacOS/cage'"
	for _, row := range []struct {
		id     string
		prompt bool
		files  int
		steps  func(script string) [][]string
	}{
		{"herdr", false, 2, func(s string) [][]string {
			helper := strings.TrimSuffix(s, ".command") + "-herdr.sh"
			return [][]string{{"/bin/sh", helper}}
		}},
		{"terminal", false, 1, func(s string) [][]string {
			return [][]string{{"/usr/bin/open", "-a", "/System/Applications/Utilities/Terminal.app", s}}
		}},
		{"ghostty", true, 1, func(s string) [][]string {
			return [][]string{
				{"/usr/bin/osascript", "-e", "tell application id \"com.mitchellh.ghostty\"\nset cfg to new surface configuration\nset command of cfg to \"" + s + "\"\nnew window with configuration cfg\nend tell"},
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
			if step.Prompt != row.prompt {
				t.Errorf("%s step %d: prompt %v, want %v", row.id, i, step.Prompt, row.prompt)
			}
		}
		if len(plan.Files) != row.files || plan.Files[0].Path != script {
			t.Fatalf("%s: files %+v", row.id, plan.Files)
		}
		if f := plan.Files[0]; f.Mode != 0o700 || !strings.Contains(f.Content, "rm -f -- \"$0\"\n"+quotedCage+" attach --terminal 0123456789abcdef\nstatus=$?\n") || !strings.Contains(f.Content, "\t"+quotedCage+" attach --reset 2>/dev/null\n") {
			t.Errorf("%s script %o %q", row.id, f.Mode, f.Content)
		}
		if row.id == "herdr" {
			helper := plan.Files[1]
			wantHelper := strings.TrimSuffix(script, ".command") + "-herdr.sh"
			argvJSON, _ := json.Marshal([]string{"/bin/sh", script})
			if helper.Path != wantHelper || helper.Mode != 0o700 {
				t.Fatalf("herdr helper: %+v", helper)
			}
			for _, snippet := range []string{
				"rm -f -- \"$0\"\n",
				"/Applications/Herdr.app/Contents/MacOS/Herdr",
				"/Applications/Herdr.app/Contents/MacOS/herdr-app",
				"$HOME/.local/bin/herdr-app",
				`do terminal.open --name "Super Cage" --argv ` + ShellQuote(string(argvJSON)),
				"/usr/bin/open -a /Applications/Herdr.app",
				`"$herdr_bin" status`,
			} {
				if !strings.Contains(helper.Content, snippet) {
					t.Errorf("herdr helper missing %q in:\n%s", snippet, helper.Content)
				}
			}
		}
	}
	// An application this build no longer knows cannot be planned.
	gone := Installed{App: App{ID: "iterm2", Name: "iTerm2", BundleIDs: []string{"com.googlecode.iterm2"}}, Path: "/Applications/iTerm.app"}
	if _, err := PlanLaunch(PlanInput{App: gone, Command: attachCommand, Name: "x", Scratch: "/tmp"}); err == nil {
		t.Fatal("an unknown application was planned")
	}
}

func TestHerdrHelperScriptQuotingAndExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	root := t.TempDir()
	scratch := filepath.Join(root, "it's scratch dir")
	appDir := filepath.Join(root, "My Herdr.app")
	binDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(root, "herdr-args.txt")
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + ShellQuote(argsLog) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "Herdr"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	herdrApp := installed(t, "herdr")
	herdrApp.Path = appDir
	plan, err := PlanLaunch(PlanInput{App: herdrApp, Command: attachCommand, Name: "0123456789abcdef", Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if err := Launch(context.Background(), plan, ExecRunner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plan.Files[1].Path); !os.IsNotExist(err) {
		t.Fatalf("herdr helper script did not remove itself: %v", err)
	}
	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(gotArgs) != 6 || gotArgs[0] != "do" || gotArgs[1] != "terminal.open" || gotArgs[2] != "--name" || gotArgs[3] != "Super Cage" || gotArgs[4] != "--argv" {
		t.Fatalf("unexpected herdr-app args: %q", gotArgs)
	}
	var decoded []string
	if err := json.Unmarshal([]byte(gotArgs[5]), &decoded); err != nil {
		t.Fatalf("invalid --argv JSON %q: %v", gotArgs[5], err)
	}
	if len(decoded) != 2 || decoded[0] != "/bin/sh" || decoded[1] != plan.Files[0].Path {
		t.Fatalf("decoded argv = %q, want [/bin/sh %q]", decoded, plan.Files[0].Path)
	}

	// Fallback to $HOME/.local/bin/herdr-app when bundle binary is absent.
	if err := os.Remove(filepath.Join(binDir, "Herdr")); err != nil {
		t.Fatal(err)
	}
	fakeHome := filepath.Join(root, "home")
	localBin := filepath.Join(fakeHome, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fallbackLog := filepath.Join(root, "fallback-args.txt")
	fallbackStub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + ShellQuote(fallbackLog) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(localBin, "herdr-app"), []byte(fallbackStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	plan2, err := PlanLaunch(PlanInput{App: herdrApp, Command: attachCommand, Name: "fedcba9876543210", Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if err := Launch(context.Background(), plan2, ExecRunner{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fallbackLog); err != nil || !strings.Contains(string(data), "terminal.open") {
		t.Fatalf("fallback herdr-app did not run: %q %v", data, err)
	}
}

// Herdr opens the window when it is installed, else Ghostty, else the system
// Terminal: there is nothing to choose, however many others are installed.
func TestPickPrefersHerdrThenGhosttyElseTerminal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS table")
	}
	herdr, ghostty, terminal := installed(t, "herdr"), installed(t, "ghostty"), installed(t, "terminal")
	for _, row := range []struct {
		name  string
		found []Installed
		want  string
	}{
		{"all three reverse", []Installed{terminal, ghostty, herdr}, "herdr"},
		{"all three in table order", []Installed{herdr, ghostty, terminal}, "herdr"},
		{"Herdr and Ghostty", []Installed{ghostty, herdr}, "herdr"},
		{"Herdr and Terminal", []Installed{terminal, herdr}, "herdr"},
		{"Herdr alone", []Installed{herdr}, "herdr"},
		{"Ghostty and Terminal", []Installed{terminal, ghostty}, "ghostty"},
		{"Ghostty and Terminal in table order", []Installed{ghostty, terminal}, "ghostty"},
		{"Ghostty alone", []Installed{ghostty}, "ghostty"},
		{"Terminal alone", []Installed{terminal}, "terminal"},
		{"neither", nil, ""},
	} {
		got, ok := Pick(row.found)
		if row.want == "" && ok || row.want != "" && (!ok || got.ID != row.want) {
			t.Errorf("%s: picked %+v (%v), want %q", row.name, got, ok, row.want)
		}
	}
	// Whatever else the owner has installed is not even looked for.
	if _, ok := Pick([]Installed{{App: App{ID: "kitty", Name: "kitty"}, Path: "/Applications/kitty.app"}}); ok {
		t.Error("an application outside the table was picked")
	}
}

func TestPlanRefusesWhatItCouldMisread(t *testing.T) {
	app := installed(t, "terminal")
	for _, command := range []Command{{"cage", "attach"}, {"/bin/cage", "attach\n--terminal"}, {"/bin/cage", ""}, {}} {
		if _, err := PlanLaunch(PlanInput{App: app, Command: command, Name: "a", Scratch: "/tmp"}); err == nil {
			t.Errorf("accepted %q", command)
		}
	}
	if _, err := PlanLaunch(PlanInput{App: Installed{App: app.App, Path: "Terminal.app"}, Command: attachCommand, Name: "a", Scratch: "/tmp"}); err == nil {
		t.Error("accepted a relative application path")
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
	plan, script := planFor(t, "terminal")
	// A plan's later steps are fallbacks: the first one was refused.
	plan.Steps = append(plan.Steps, Step{Argv: []string{"/usr/bin/true"}})
	runner := &recordingRunner{fail: 1}
	if err := Launch(context.Background(), plan, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.steps) != 2 || runner.steps[1][0] != "/usr/bin/true" {
		t.Fatal(runner.steps)
	}
	info, err := os.Stat(script)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal(info, err)
	}
	// Scripts never run are removed later; recent ones and other files stay.
	dir := filepath.Dir(script)
	stale := filepath.Join(dir, "super-cage-0123456789abcdef.command")
	staleHerdr := filepath.Join(dir, "super-cage-0123456789abcdef-herdr.sh")
	kept := []string{
		filepath.Join(dir, "super-cage-fresh.command"),
		filepath.Join(dir, "super-cage-fresh-herdr.sh"),
		filepath.Join(dir, "my-own.command"),
		filepath.Join(dir, "my-own-herdr.sh"),
		filepath.Join(dir, "super-cage-other.yaml"),
	}
	for _, path := range append([]string{staleHerdr}, kept...) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for _, path := range []string{stale, staleHerdr, kept[2], kept[3], kept[4]} {
		_ = os.Chtimes(path, old, old)
	}
	PruneScratch(dir, 10*time.Minute)
	for _, gone := range []string{stale, staleHerdr} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("stale script kept: %s", gone)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removed", path)
		}
	}
	runner = &recordingRunner{fail: 9}
	if err := Launch(context.Background(), plan, runner); err == nil {
		t.Fatal("all steps failed but launch succeeded")
	}
}

func TestLaunchEnvironmentKeepsOnlySessionBasics(t *testing.T) {
	got := LaunchEnvironment([]string{"HOME=/Users/o", "PATH=/usr/bin", "LANG=zh_CN.UTF-8", "LC_CTYPE=UTF-8", "SHELL=/bin/zsh",
		"CAGE_CONFIG=/x/config.json", "CAGE_DESKTOP_START_ID=abc", "HTTPS_PROXY=http://127.0.0.1:7890", "GITHUB_TOKEN=secret", "broken"})
	if strings.Join(got, " ") != "HOME=/Users/o PATH=/usr/bin LANG=zh_CN.UTF-8 LC_CTYPE=UTF-8 SHELL=/bin/zsh" {
		t.Fatal(got)
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

func TestDetectFindsFreshHerdrBundleInHomeApplications(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS fallback detection")
	}
	fakeHome := t.TempDir()
	herdrBin := filepath.Join(fakeHome, "Applications", "Herdr.app", "Contents", "MacOS", "herdr-app")
	if err := os.MkdirAll(filepath.Dir(herdrBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(herdrBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	found := Detect()
	picked, ok := Pick(found)
	if !ok || picked.ID != "herdr" || !strings.HasSuffix(picked.Path, "/Herdr.app") {
		t.Fatalf("expected freshly installed Herdr.app to be picked, got %+v (%v), found=%+v", picked, ok, found)
	}
}

func TestSettingsDefaults(t *testing.T) {
	dir := t.TempDir()
	settings, err := LoadSettings(dir)
	if err != nil || settings != DefaultSettings() || !settings.ImageUpload || !settings.ClipboardWrite {
		t.Fatal(settings, err)
	}
	// The settings are the two switches and nothing else.
	if data, err := json.Marshal(settings); err != nil || string(data) != "{\"image_upload\":true,\"clipboard_write\":true}" {
		t.Fatalf("%s %v", data, err)
	}
}

// A file written by an earlier build holds the application the owner chose
// then: it is ignored without an error, and the other switches are kept.
func TestSettingsIgnoreAnEarlierAppChoice(t *testing.T) {
	for _, stored := range []string{"ghostty", "kitty", "no-such-app", ""} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, SettingsFile), []byte("{\"app\":\""+stored+"\",\"image_upload\":false,\"clipboard_write\":false}"), 0o600); err != nil {
			t.Fatal(err)
		}
		settings, err := LoadSettings(dir)
		if err != nil || settings != (Settings{}) {
			t.Fatalf("%q: %+v, %v", stored, settings, err)
		}
	}
}

func TestCommandRefusesEveryControl(t *testing.T) {
	for _, arg := range []string{"a\x1bb", "a\x7fb", "a\u0085b", "a\u009bb", "a\xffb", "a\tb"} {
		if err := (Command{"/bin/cage", "--project", arg}).Validate(); err == nil {
			t.Errorf("%q accepted", arg)
		}
	}
	if err := (Command{"/bin/cage", "--project", "/work/项目 一"}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestAppleScriptErrorsInTheOwnersWords(t *testing.T) {
	for _, row := range []struct {
		output   string
		timedOut bool
		want     string
	}{
		{"36:120: execution error: Not authorized to send Apple events to Ghostty. (-1743)", false, "自动化"},
		{"", true, "超时"},
		{"execution error: Ghostty got an error: AppleEvent timed out. (-1712)", false, "没有及时回应"},
		{"62:83: syntax error: Expected end of line but found identifier. (-2741)", false, "1.3"},
		{"something else", false, "something else"},
	} {
		if err := appleScriptError([]byte(row.output), row.timedOut, errors.New("exit status 1")); !strings.Contains(err.Error(), row.want) {
			t.Errorf("%q: %v", row.output, err)
		}
	}
}

// Settings that cannot be read are not taken as permission: clipboard writes
// are off, the rest is the defaults. A missing file is the defaults.
func TestUnreadableSettingsTurnClipboardWritesOff(t *testing.T) {
	for name, write := range map[string]func(path string) error{
		"corrupt":    func(path string) error { return os.WriteFile(path, []byte("{\"clipboard_write\":"), 0o600) },
		"too large":  func(path string) error { return os.WriteFile(path, make([]byte, 65<<10), 0o600) },
		"not a file": func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		dir := t.TempDir()
		if err := write(filepath.Join(dir, SettingsFile)); err != nil {
			t.Fatal(err)
		}
		settings, err := LoadSettings(dir)
		if err == nil || settings.ClipboardWrite || !settings.ImageUpload || settings != UnreadableSettings() {
			t.Errorf("%s: %+v, %v", name, settings, err)
		}
	}
}
