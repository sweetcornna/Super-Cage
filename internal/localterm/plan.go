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

// PlanFile is a file a plan writes before its steps run. Its directory is
// made private (0700, this user's, not a link).
type PlanFile struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// Step is one program to run. Prompt marks an AppleScript step, which waits
// while macOS asks the owner whether Super Cage may control the application.
type Step struct {
	Argv   []string
	Prompt bool
}

// PlanInput is everything a plan is built from.
type PlanInput struct {
	App     Installed
	Command Command
	// Name makes the plan's files unique, e.g. the terminal id.
	Name string
	// Scratch is a private directory (0700) for the one-shot launch script.
	Scratch string
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
	// Every application runs a one-shot script, so the command needs no
	// quoting beyond the script's own. It removes itself, then runs cage
	// attach as a child: a clean exit closes the window at once, any other
	// keeps cage attach's message on screen until a key is pressed.
	script := filepath.Join(in.Scratch, ScriptPrefix+in.Name+".command")
	scriptFile := PlanFile{Path: script, Mode: 0o700, Content: launchScript(in.Command)}
	switch in.App.ID {
	case "terminal":
		// Terminal runs a .command file in a new window; no permission is asked.
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{{Argv: []string{"/usr/bin/open", "-a", in.App.Path, script}}}}, nil
	case "ghostty":
		// Ghostty 1.3 and later: a new window with a surface configuration.
		// Without Automation permission it fails and says so: a second
		// Ghostty instance would not be the owner's.
		source := strings.Join([]string{
			"tell application id " + AppleScriptString(in.App.bundleID()),
			"set cfg to new surface configuration",
			"set command of cfg to " + AppleScriptString(ShellQuote(script)),
			"new window with configuration cfg",
			"end tell",
		}, "\n")
		return Plan{Files: []PlanFile{scriptFile}, Steps: []Step{{Argv: []string{"/usr/bin/osascript", "-e", source}, Prompt: true}}}, nil
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

// Runner runs one step. Tests record steps instead of running them.
type Runner interface {
	Run(ctx context.Context, step Step) error
}

// launchFileLife is how long a launch's files stay when the application
// does not take them: a script never run (the owner refused Automation, say).
var launchFileLife = 10 * time.Minute

// Launch writes a plan's files and runs its steps until one succeeds. Files
// are created only where nothing was, never through a link; a launch that
// fails removes them, one that succeeds removes them after launchFileLife
// if they are still there.
func Launch(ctx context.Context, plan Plan, run Runner) error {
	var written []writtenFile
	removeAll := func() {
		for _, w := range written {
			w.remove()
		}
	}
	for _, f := range plan.Files {
		w, err := writeExclusive(f)
		if err != nil {
			removeAll()
			return err
		}
		written = append(written, w)
	}
	var failure error
	for _, step := range plan.Steps {
		if failure = run.Run(ctx, step); failure == nil {
			for _, w := range written {
				time.AfterFunc(launchFileLife, w.remove)
			}
			return nil
		}
	}
	removeAll()
	if failure == nil {
		failure = errors.New("没有可用的启动方式")
	}
	return failure
}

// writtenFile is a file a launch created, as it was created.
type writtenFile struct {
	path string
	info os.FileInfo
}

// remove deletes the file if it is still the one created: a script that ran
// has removed itself, and anything put there since stays.
func (w writtenFile) remove() {
	if info, err := os.Lstat(w.path); err == nil && os.SameFile(info, w.info) {
		_ = os.Remove(w.path)
	}
}

func writeExclusive(f PlanFile) (writtenFile, error) {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return writtenFile{}, err
	}
	if err := privateDir(dir); err != nil {
		return writtenFile{}, err
	}
	file, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|openNoFollow, f.Mode)
	if err != nil {
		return writtenFile{}, errors.New("无法写入启动文件：" + err.Error())
	}
	_, err = file.WriteString(f.Content)
	if err == nil {
		err = file.Chmod(f.Mode)
	}
	info, statErr := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = statErr
	}
	if err != nil {
		_ = os.Remove(f.Path)
		return writtenFile{}, err
	}
	return writtenFile{path: f.Path, info: info}, nil
}

// privateDir checks that dir is a directory of this user, not a link, and
// makes it 0700.
func privateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || !ownedByMe(info) {
		return errors.New("启动文件目录不安全：" + dir)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// PruneScratch removes what earlier launches left behind once it is older
// than age: scripts that were never run (the owner refused the Automation
// permission, say).
func PruneScratch(dir string, age time.Duration) {
	pruneFiles(dir, ".command", age)
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
