package localterm

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// App is one terminal application this build knows.
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
	// Hosts: the application can show "cage attach" in this build. Herdr
	// cannot (it needs the SSH entry of a later step).
	Hosts bool   `json:"hosts"`
	Note  string `json:"note,omitempty"`
}

// macApps is the table of design section 5.8, macOS.
var macApps = []App{
	{ID: "ghostty", Name: "Ghostty", BundleIDs: []string{"com.mitchellh.ghostty"}, Automation: true, Hosts: true,
		Evidence: "ghostty macos/Ghostty.xcodeproj/project.pbxproj PRODUCT_BUNDLE_IDENTIFIER (release; the tip channel ships the same id per Homebrew cask ghostty@tip)"},
	{ID: "iterm2", Name: "iTerm2", BundleIDs: []string{"com.googlecode.iterm2"}, Automation: true, Hosts: true,
		Evidence: "iTerm2.xcodeproj/project.pbxproj PRODUCT_BUNDLE_IDENTIFIER = com.googlecode.iterm2; Homebrew cask iterm2 uninstall quit; AppleScript 'create window with default profile command' in the iTerm2 scripting docs"},
	{ID: "terminal", Name: "终端", BundleIDs: []string{"com.apple.Terminal"}, Hosts: true,
		Evidence: "detected on this Mac at /System/Applications/Utilities/Terminal.app (experiment E11)"},
	{ID: "warp", Name: "Warp", BundleIDs: []string{"dev.warp.Warp-Stable", "dev.warp.Warp-Preview"}, Hosts: true,
		Evidence: "Homebrew casks warp and warp@preview: ~/Library/Preferences/dev.warp.Warp-Stable.plist, dev.warp.Warp-Preview.plist"},
	{ID: "wezterm", Name: "WezTerm", BundleIDs: []string{"com.github.wez.wezterm"}, Hosts: true,
		Evidence: "wezterm assets/macos/WezTerm.app/Contents/Info.plist CFBundleIdentifier; Homebrew casks wezterm and wezterm@nightly"},
	{ID: "kitty", Name: "kitty", BundleIDs: []string{"net.kovidgoyal.kitty"}, Hosts: true,
		Evidence: "kitty setup.py CFBundleIdentifier=f'net.kovidgoyal.{appname}', kitty/constants.py appname = 'kitty'; Homebrew cask kitty (Contents/MacOS/kitty); --single-instance in kitty/simple_cli_definitions.py"},
	{ID: "alacritty", Name: "Alacritty", BundleIDs: []string{"org.alacritty"}, Hosts: true,
		Evidence: "alacritty extra/osx/Alacritty.app/Contents/Info.plist CFBundleIdentifier; Homebrew cask alacritty"},
	{ID: "herdr", Name: "Herdr", BundleIDs: []string{"dev.herdr.app"}, Hosts: false,
		Note:     "Herdr 通过本机终端的 SSH 入口连接，稍后的版本提供",
		Evidence: "herdr-app scripts/package CFBundleIdentifier; detected on this Mac at /Applications/Herdr.app (E11)"},
}

// Apps are the applications known on this operating system. Windows and
// Linux tables arrive with their desktops (design step 5).
func Apps() []App {
	if runtime.GOOS == "darwin" {
		return macApps
	}
	return nil
}

// Lookup finds a known application by id.
func Lookup(id string) *App {
	for i := range Apps() {
		if Apps()[i].ID == id {
			return &Apps()[i]
		}
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

// Detect finds the known applications installed on this computer, without
// asking for any permission and without starting anything.
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

// Command is what the window runs: an absolute program and its arguments.
// It holds only ids, never a secret.
type Command []string

// Validate refuses a command that a launch could misread.
func (c Command) Validate() error {
	if len(c) == 0 || !filepath.IsAbs(c[0]) {
		return errors.New("启动命令必须是绝对路径")
	}
	for _, arg := range c {
		if arg == "" || strings.ContainsAny(arg, "\x00\r\n") {
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

// YAMLString quotes text as a double-quoted YAML scalar.
func YAMLString(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
