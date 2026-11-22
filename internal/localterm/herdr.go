package localterm

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// Herdr (design 5.8) is not a host for cage attach. With Herdr chosen,
// "在本机终端中打开" on a project asks the owner's own hc for a new shell on
// the environment, which this computer's Herdr added as a machine (cage
// entry herdr); Herdr's window shows it with its other terminals.

// HCPaths are where an hc is looked for, in order: the links herdr-core's
// own installer makes in ~/.local/bin (its HC_INSTALL_DIR default), then
// Homebrew's and Nix's. A desktop app does not have the login shell's PATH.
func HCPaths(home string) []string {
	return []string{filepath.Join(home, ".local", "bin", "hc"), "/opt/homebrew/bin/hc", "/usr/local/bin/hc", filepath.Join(home, ".nix-profile", "bin", "hc")}
}

// FindHC returns the first executable file of HCPaths, or "".
func FindHC(home string) string {
	if !filepath.IsAbs(home) {
		return ""
	}
	return firstExecutable(HCPaths(home))
}

func firstExecutable(paths []string) string {
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// herdrMachine is a Host alias of the local SSH entry, as Herdr names the
// machine (cage-default, cage-<slug>).
var herdrMachine = regexp.MustCompile("^[a-z0-9][a-z0-9._-]{0,63}$")

// PlanHerdr is the one step that opens a new shell of the environment in
// Herdr: hc run in the background on that machine, in the project there.
func PlanHerdr(hc, machine, project string) (Plan, error) {
	if !filepath.IsAbs(hc) || !herdrMachine.MatchString(machine) || !strings.HasPrefix(project, "/") || termfilter.Controls(project) {
		return Plan{}, errors.New("启动信息无效")
	}
	return Plan{Steps: []Step{{Argv: []string{hc, "run", "--bg", "--on", machine, "--cwd", project, "--", "bash", "-l"}}}}, nil
}
