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
	return ""
}
