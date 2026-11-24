package localterm

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// App is one terminal application this build can open a window in.
type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// BundleIDs are the macOS bundle identifiers of its release channels,
	// each checked against the application's own build files or its
	// Homebrew cask (Evidence).
	BundleIDs []string `json:"-"`
	Evidence  string   `json:"-"`
	// Automation: opening a window asks macOS once for permission to
	// control the application, and the owner should be told first.
	Automation bool `json:"automation"`
}

// macApps is macOS's table, in the order of preference: there is no choice
// to make. Herdr opens the window when it is installed, else Ghostty, else
// the system Terminal.
var macApps = []App{
	{ID: "herdr", Name: "Herdr", BundleIDs: []string{"dev.herdr.app"},
		Evidence: "herdr-app CFBundleIdentifier=dev.herdr.app (installs to ~/Applications/Herdr.app or /Applications/Herdr.app)"},
	{ID: "ghostty", Name: "Ghostty", BundleIDs: []string{"com.mitchellh.ghostty"}, Automation: true,
		Evidence: "ghostty macos/Ghostty.xcodeproj/project.pbxproj PRODUCT_BUNDLE_IDENTIFIER (release; the tip channel ships the same id per Homebrew cask ghostty@tip)"},
	{ID: "terminal", Name: "系统终端", BundleIDs: []string{"com.apple.Terminal"},
		Evidence: "detected on this Mac at /System/Applications/Utilities/Terminal.app (experiment E11)"},
}

// Apps are the applications this build can use on this operating system, in
// the order of preference. Windows and Linux tables arrive with their
// desktops (design step 5).
func Apps() []App {
	if runtime.GOOS == "darwin" {
		return macApps
	}
	return nil
}

// Installed is a detected application: where its bundle is and which of its
// release channels (bundle identifiers) was found.
type Installed struct {
	App
	Path     string `json:"path,omitempty"`
	BundleID string `json:"-"`
}

// Detect finds the applications of the table installed on this computer,
// without asking for any permission and without starting anything.
func Detect() []Installed {
	var found []Installed
	for _, app := range Apps() {
		for _, id := range app.BundleIDs {
			if path := applicationPath(id); path != "" {
				found = append(found, Installed{App: app, Path: path, BundleID: id})
				break
			}
		}
	}
	return found
}

// Pick is the application "在本机终端中打开" uses among those found: the
// first of the table (Herdr), else Ghostty, else the system Terminal. It
// reports false when none is installed.
func Pick(found []Installed) (Installed, bool) {
	for _, app := range Apps() {
		for _, candidate := range found {
			if candidate.ID == app.ID {
				return candidate, true
			}
		}
	}
	return Installed{}, false
}

// Command is what the window runs: an absolute program and its arguments.
// It holds only ids, never a secret.
type Command []string

// Validate refuses a command that a launch could misread.
func (c Command) Validate() error {
	if len(c) == 0 || !filepath.IsAbs(c[0]) {
		return errors.New("启动命令必须是绝对路径")
	}
	for _, arg := range c {
		if arg == "" || termfilter.Controls(arg) {
			return errors.New("启动命令含有无效参数")
		}
	}
	return nil
}

// ResetCommand is the same program asked only to reset the window:
// cage-attach --reset, or cage attach --reset.
func (c Command) ResetCommand() Command {
	n := 1
	if len(c) > 1 && c[1] == "attach" {
		n = 2
	}
	return append(append(Command{}, c[:n]...), "--reset")
}

// ShellQuote quotes one word for /bin/sh.
func ShellQuote(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,@+%") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// ShellLine quotes a whole command for /bin/sh.
func (c Command) ShellLine() string {
	quoted := make([]string, len(c))
	for i, word := range c {
		quoted[i] = ShellQuote(word)
	}
	return strings.Join(quoted, " ")
}

// AppleScriptString quotes text as an AppleScript string literal.
func AppleScriptString(text string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(text) + "\""
}
