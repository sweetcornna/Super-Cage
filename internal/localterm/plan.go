package localterm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Plan is what opening one window takes: files written first, then the
// steps tried in order until one succeeds (later steps are fallbacks).
type Plan struct {
	Files []PlanFile
	Steps []Step
}

// PlanFile is a file a plan writes before its steps run.
type PlanFile struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// Step is one program to run. Detach marks a program that may become the
// application itself (it keeps running): it is started on its own and
// counted as done once it is still running after a moment.
type Step struct {
	Argv   []string
	Detach bool
}

// PlanInput is everything a plan is built from.
type PlanInput struct {
	App     Installed
	Command Command
	// Name makes the plan's files unique, e.g. the terminal id.
	Name string
	// Scratch is a private directory (0700) for the one-shot launch script.
	Scratch string
	// Home is the owner's home directory, for Warp's launch configurations.
	Home string
}

var planName = regexp.MustCompile("^[a-z0-9-]{1,64}$")

// ScriptPrefix names the one-shot launch scripts in the scratch directory.
const ScriptPrefix = "super-cage-"

// PlanLaunch builds the steps that open a window of in.App running
// in.Command. It runs nothing.
func PlanLaunch(in PlanInput) (Plan, error) {
	if err := in.Command.Validate(); err != nil {
		return Plan{}, err
	}
	if !planName.MatchString(in.Name) || !filepath.IsAbs(in.Scratch) || !filepath.IsAbs(in.App.Path) {
		return Plan{}, errors.New("启动信息无效")
	}
	if !in.App.Hosts {
		return Plan{}, errors.New(in.App.Name + " 暂时不能在本机终端中打开 Super Cage 终端")
	}
	// Every application runs a one-shot script, so the command needs no
	// quoting beyond the script's own. It removes itself, then runs cage
	// attach as a child: a clean exit closes the window at once, any other
	// keeps cage attach's message on screen until a key is pressed.
	script := filepath.Join(in.Scratch, ScriptPrefix+in.Name+".command")
	scriptFile := PlanFile{Path: script, Mode: 0o700, Content: launchScript(in.Command)}
	shScript := []string{"/bin/sh", script}
	bundle := in.App.Path
	binary := func(name string) string { return filepath.Join(bundle, "Contents", "MacOS", name) }
	switch in.App.ID {
	case "terminal":
		// Terminal runs a .command file in a new window; no permission is asked.
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{{Argv: []string{"/usr/bin/open", "-a", bundle, script}}}}, nil
	case "iterm2":
		source := "tell application id " + AppleScriptString(in.App.bundleID()) + " to create window with default profile command " + AppleScriptString(ShellQuote(script))
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{{Argv: []string{"/usr/bin/osascript", "-e", source}}}}, nil
	case "ghostty":
		// Ghostty 1.3 and later: a new window with a surface configuration.
		// Without Automation permission, a new instance whose first window
		// runs the script (-e sets initial-command).
		source := strings.Join([]string{
			"tell application id " + AppleScriptString(in.App.bundleID()),
			"set cfg to new surface configuration",
			"set command of cfg to " + AppleScriptString(ShellQuote(script)),
			"new window with configuration cfg",
			"end tell",
		}, "\n")
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{
			{Argv: []string{"/usr/bin/osascript", "-e", source}},
			{Argv: []string{"/usr/bin/open", "-na", bundle, "--args", "-e", script}},
		}}, nil
	case "warp":
		if !filepath.IsAbs(in.Home) {
			return Plan{}, errors.New("启动信息无效")
		}
		config := ScriptPrefix + in.Name + ".yaml"
		scheme := "warp"
		if in.App.bundleID() == "dev.warp.Warp-Preview" {
			scheme = "warppreview"
		}
		yaml := "---\nname: " + YAMLString("Super Cage "+in.Name) + "\nwindows:\n  - tabs:\n      - title: \"Super Cage\"\n        layout:\n          cwd: " + YAMLString(in.Home) + "\n          commands:\n            - exec: " + YAMLString(ShellQuote(script)) + "\n"
		return Plan{Files: []PlanFile{scriptFile, {Path: filepath.Join(warpConfigDir(in.Home), config), Content: yaml, Mode: 0o600}},
			Steps: []Step{{Argv: []string{"/usr/bin/open", scheme + "://launch/" + config}}}}, nil
	case "wezterm":
		// wezterm start asks a running WezTerm for a new window, or starts one.
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{{Argv: append([]string{binary("wezterm"), "start", "--"}, shScript...), Detach: true}}}, nil
	case "kitty":
		// --single-instance hands the window to a running kitty.
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{{Argv: append([]string{binary("kitty"), "--single-instance"}, shScript...), Detach: true}}}, nil
	case "alacritty":
		// A running Alacritty opens a window over its socket; otherwise a new one.
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{
			{Argv: append([]string{binary("alacritty"), "msg", "create-window", "-e"}, shScript...)},
			{Argv: append([]string{"/usr/bin/open", "-na", bundle, "--args", "-e"}, shScript...)},
		}}, nil
	}
	return Plan{}, errors.New("不支持这个本机终端应用")
}

// launchScript runs command in the new window. A clean exit (the terminal
// ended, or the owner detached) closes it at once. Any other exit resets the
// window (cage attach does too, unless it was killed), keeps cage attach's
// message on screen, explains a signal, and waits for one key.
func launchScript(command Command) string {
	return "#!/bin/sh\n" +
		"# Super Cage: opens one terminal of the environment, then removes itself.\n" +
		"rm -f -- \"$0\"\n" +
		command.ShellLine() + "\n" +
		"status=$?\n" +
		"if [ \"$status\" -ne 0 ]; then\n" +
		"\t" + command.ResetCommand().ShellLine() + " 2>/dev/null\n" +
		"\tif [ \"$status\" -gt 128 ]; then\n" +
		"\t\tprintf '\\r\\n本机终端连接意外结束（退出码 %s）。\\r\\n' \"$status\"\n" +
		"\tfi\n" +
		"\tprintf '\\r\\n按任意键关闭'\n" +
		"\tsaved=$(stty -g 2>/dev/null)\n" +
		"\tstty -icanon -echo min 1 time 0 2>/dev/null\n" +
		"\tdd bs=1 count=1 >/dev/null 2>&1\n" +
		"\tif [ -n \"$saved\" ]; then stty \"$saved\" 2>/dev/null; fi\n" +
		"fi\n" +
		"exit \"$status\"\n"
}

// bundleID is the release channel that was found, else the main one.
func (a Installed) bundleID() string {
	if a.BundleID != "" {
		return a.BundleID
	}
	return a.BundleIDs[0]
}

// warpConfigDir is where Warp reads launch configurations on macOS.
func warpConfigDir(home string) string { return filepath.Join(home, ".warp", "launch_configurations") }

// Runner runs one step. Tests record steps instead of running them.
type Runner interface {
	Run(ctx context.Context, step Step) error
}

// Launch writes a plan's files and runs its steps until one succeeds.
func Launch(ctx context.Context, plan Plan, run Runner) error {
	for _, f := range plan.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(f.Path, []byte(f.Content), f.Mode); err != nil {
			return err
		}
		if err := os.Chmod(f.Path, f.Mode); err != nil {
			return err
		}
	}
	var failure error
	for _, step := range plan.Steps {
		if failure = run.Run(ctx, step); failure == nil {
			return nil
		}
	}
	if failure == nil {
		failure = errors.New("没有可用的启动方式")
	}
	return failure
}

// PruneScratch removes what earlier launches left behind once it is older
// than age: scripts that were never run (the owner refused the Automation
// permission, say) and Warp launch configurations, which Warp would
// otherwise keep listing.
func PruneScratch(dir, home string, age time.Duration) {
	pruneFiles(dir, ".command", age)
	if filepath.IsAbs(home) {
		pruneFiles(warpConfigDir(home), ".yaml", age)
	}
}

func pruneFiles(dir, suffix string, age time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ScriptPrefix) || !strings.HasSuffix(entry.Name(), suffix) || !entry.Type().IsRegular() {
			continue
		}
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
