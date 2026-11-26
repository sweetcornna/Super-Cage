//go:build darwin && !cgo

package localorca

import (
	"os"
	"path/filepath"
	"regexp"
)

const macOSBundleID = "com.stablyai.orca"

var plistIdentifier = regexp.MustCompile(`<key>CFBundleIdentifier</key>\s*<string>([^<]+)</string>`)

func detectLocal() Installation {
	appPath := findMacOSAppNoCGO()
	if appPath == "" {
		return Installation{}
	}
	cliPath := filepath.Join(appPath, "Contents", "Resources", "bin", "orca")
	if info, err := os.Stat(cliPath); err != nil || info.IsDir() {
		cliPath = ""
	}
	ver := readBuildVersion(filepath.Join(appPath, "Contents", "Resources", "orca-local-build.json"))
	return Installation{
		Installed: cliPath != "" || appPath != "",
		AppPath:   appPath,
		CLIPath:   cliPath,
		Version:   ver,
	}
}

func findMacOSAppNoCGO() string {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".app" {
				continue
			}
			bundle := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(filepath.Join(bundle, "Contents", "Info.plist"))
			if err != nil || len(data) > 1<<20 {
				continue
			}
			if match := plistIdentifier.FindSubmatch(data); match != nil && string(match[1]) == macOSBundleID {
				return bundle
			}
		}
	}
	for _, cand := range []string{filepath.Join(home, "Applications", "Orca.app"), "/Applications/Orca.app"} {
		if info, err := os.Stat(filepath.Join(cand, "Contents", "Info.plist")); err == nil && !info.IsDir() {
			return cand
		}
	}
	return ""
}
