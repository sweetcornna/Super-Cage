//go:build linux

package localorca

import (
	"os"
	"os/exec"
	"path/filepath"
)

// detectLocal locates Orca Desktop / CLI on a Linux host without ever matching
// GNOME's /usr/bin/orca screen reader.
func detectLocal() Installation {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "orca-ide"),
		"/opt/Orca/resources/bin/orca-ide",
		"/opt/Orca/resources/bin/orca",
		"/usr/local/bin/orca-ide",
	}
	for _, cli := range candidates {
		if cli == "" {
			continue
		}
		if info, err := os.Stat(cli); err == nil && !info.IsDir() {
			resDir := filepath.Dir(filepath.Dir(cli))
			ver := readBuildVersion(filepath.Join(resDir, "orca-local-build.json"))
			return Installation{Installed: true, AppPath: filepath.Dir(resDir), CLIPath: cli, Version: ver}
		}
	}
	if cli, err := exec.LookPath("orca-ide"); err == nil && cli != "" {
		return Installation{Installed: true, CLIPath: cli}
	}
	return Installation{}
}
