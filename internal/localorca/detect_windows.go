//go:build windows

package localorca

import (
	"os"
	"os/exec"
	"path/filepath"
)

func detectLocal() Installation {
	localAppData := os.Getenv("LOCALAPPDATA")
	candidates := []string{}
	if localAppData != "" {
		candidates = append(candidates,
			filepath.Join(localAppData, "Programs", "Orca"),
			filepath.Join(localAppData, "Orca"),
		)
	}
	for _, dir := range candidates {
		cli := filepath.Join(dir, "resources", "bin", "orca.exe")
		if info, err := os.Stat(cli); err == nil && !info.IsDir() {
			ver := readBuildVersion(filepath.Join(dir, "resources", "orca-local-build.json"))
			return Installation{Installed: true, AppPath: dir, CLIPath: cli, Version: ver}
		}
		exe := filepath.Join(dir, "Orca.exe")
		if info, err := os.Stat(exe); err == nil && !info.IsDir() {
			ver := readBuildVersion(filepath.Join(dir, "resources", "orca-local-build.json"))
			return Installation{Installed: true, AppPath: exe, Version: ver}
		}
	}
	if cli, err := exec.LookPath("orca.exe"); err == nil && cli != "" {
		return Installation{Installed: true, CLIPath: cli}
	}
	return Installation{}
}
