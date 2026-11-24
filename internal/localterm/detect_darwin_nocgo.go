//go:build darwin && !cgo

package localterm

import (
	"os"
	"path/filepath"
	"regexp"
)

// Without cgo (a cross-compiled build) the standard application folders are
// read instead of LaunchServices. Each Info.plist is checked for the bundle
// identifier when it is XML.
var plistIdentifier = regexp.MustCompile("<key>CFBundleIdentifier</key>\\s*<string>([^<]+)</string>")

func applicationPath(bundleID string) string {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{"/Applications", "/Applications/Utilities", "/System/Applications", "/System/Applications/Utilities", filepath.Join(home, "Applications")} {
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
			if match := plistIdentifier.FindSubmatch(data); match != nil && string(match[1]) == bundleID {
				return bundle
			}
		}
	}
	return fallbackBundlePath(bundleID)
}

func fallbackBundlePath(bundleID string) string {
	if bundleID != "dev.herdr.app" {
		return ""
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "Herdr.app"))
	}
	candidates = append(candidates, "/Applications/Herdr.app")
	for _, candidate := range candidates {
		for _, marker := range []string{
			filepath.Join(candidate, "Contents", "MacOS", "Herdr"),
			filepath.Join(candidate, "Contents", "MacOS", "herdr-app"),
			filepath.Join(candidate, "Contents", "Info.plist"),
		} {
			if info, err := os.Stat(marker); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return ""
}
