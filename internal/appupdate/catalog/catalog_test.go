package catalog

import (
	"strings"
	"testing"
)

// The 0.1.15 assets exactly as published (gh release view v0.1.15). The
// in-app updater must not install either desktop package, and must not
// pretend that no newer version exists.
func published0115() []Release {
	base := "https://github.com/" + Repository + "/releases/download/v0.1.15/"
	asset := func(name string) Asset {
		return Asset{Name: name, URL: base + name, Size: 1024, Digest: "sha256:" + strings.Repeat("ab", 32), State: "uploaded"}
	}
	return []Release{{Tag: "v0.1.15", PublishedAt: "2026-11-20T12:00:00Z", Assets: []Asset{
		asset("Super-Cage-0.1.15-macos-arm64-signed.dmg"),
		asset("Super-Cage-0.1.15-windows-amd64-setup-unsigned.exe"),
		asset("Super-Cage-0.1.15-linux-amd64.deb"),
		asset("SHA256SUMS"),
	}}}
}

func TestPublishedDesktopPackagesAreManualNotCurrent(t *testing.T) {
	releases := published0115()
	for _, tc := range []struct {
		goos, goarch, reason string
		installable          bool
	}{
		{"darwin", "arm64", "未经 Apple 公证", false},
		{"windows", "amd64", "没有发布者签名", false},
		// Linux learns the package; its shell still installs nothing.
		{"linux", "amd64", "", true},
	} {
		c, err := Select(releases, "0.1.14", tc.goos, tc.goarch)
		if err != nil || (c != nil) != tc.installable {
			t.Fatalf("%s: select %#v %v", tc.goos, c, err)
		}
		newer, err := Latest(releases, "0.1.14", tc.goos, tc.goarch)
		if err != nil || newer == nil || newer.Version != "0.1.15" || newer.PublishedAt != "2026-11-20T12:00:00Z" {
			t.Fatalf("%s: latest %#v %v", tc.goos, newer, err)
		}
		if tc.installable != (newer.Reason == "") || !strings.Contains(newer.Reason, tc.reason) {
			t.Fatalf("%s: reason %q", tc.goos, newer.Reason)
		}
		if tc.installable && c.PublishedAt != "2026-11-20T12:00:00Z" {
			t.Fatalf("%s: candidate lost its publication time: %#v", tc.goos, c)
		}
	}
}

func TestLatestKeepsChannelAndNeverDowngrades(t *testing.T) {
	releases := published0115()
	releases = append(releases, Release{Tag: "v0.1.16-rc.1", Prerelease: true}, Release{Tag: "v0.1.17", Draft: true}, Release{Tag: "v0.1.13"})
	for _, tc := range []struct{ current, want string }{
		{"0.1.14", "0.1.15"},           // stable never moves to a prerelease or a draft
		{"0.1.15", ""},                 // the running version is not an update
		{"0.1.16", ""},                 // never a downgrade
		{"0.1.15-rc.1", "0.1.16-rc.1"}, // a prerelease follows its channel
	} {
		newer, err := Latest(releases, tc.current, "darwin", "arm64")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if newer != nil {
			got = newer.Version
		}
		if got != tc.want {
			t.Fatalf("current %s: got %q want %q", tc.current, got, tc.want)
		}
	}
	if _, err := Latest(releases, "dev", "darwin", "arm64"); err == nil {
		t.Fatal("invalid current version accepted")
	}
}
