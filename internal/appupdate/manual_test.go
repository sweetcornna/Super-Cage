package appupdate

import (
	"context"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/appupdate/catalog"
)

// A check against the assets exactly as 0.1.15 published them reports a manual
// update with its reason on macOS and Windows, never "current" and never an
// installable candidate.
func TestCheckReportsManualForUnnotarizedAndUnsignedAssets(t *testing.T) {
	base := "https://github.com/" + repository + "/releases/download/v0.1.15/"
	body := `[{"tag_name":"v0.1.15","draft":false,"prerelease":false,"published_at":"2026-11-20T12:00:00Z","assets":[` +
		`{"name":"Super-Cage-0.1.15-macos-arm64-signed.dmg","browser_download_url":"` + base + `Super-Cage-0.1.15-macos-arm64-signed.dmg","size":193580835,"digest":"sha256:767d2e4f4d89bd1d3e07ceae8de855bc9058b81c06a7fdd37007e6ed6d8b941f","state":"uploaded"},` +
		`{"name":"Super-Cage-0.1.15-windows-amd64-setup-unsigned.exe","browser_download_url":"` + base + `Super-Cage-0.1.15-windows-amd64-setup-unsigned.exe","size":487839059,"digest":"sha256:a45e17e083de05aebc82d8b1bf3ff3bf3ecc010b319534e29c0ff559949d21c3","state":"uploaded"},` +
		`{"name":"Super-Cage-0.1.15-linux-amd64.deb","browser_download_url":"` + base + `Super-Cage-0.1.15-linux-amd64.deb","size":124296676,"digest":"sha256:659b44598ca92d67b8364f81427fb9d6dbeeb2ba24f9d00a47177b0980d1f766","state":"uploaded"}]}]`
	for _, tc := range []struct {
		goos, goarch, reason string
		installable          bool
	}{
		{"darwin", "arm64", "未经 Apple 公证", false},
		{"windows", "amd64", "没有发布者签名", false},
		{"linux", "amd64", "", true},
	} {
		_, releases, err := findRelease(context.Background(), responseClient(body, 200), "0.1.14")
		if err != nil {
			t.Fatal(err)
		}
		c, err := catalog.Select(releases, "0.1.14", tc.goos, tc.goarch)
		if err != nil {
			t.Fatal(err)
		}
		got := available(c, releases, "0.1.14", tc.goos, tc.goarch)
		if got == nil || got.Version != "0.1.15" || got.Installable != tc.installable || !strings.Contains(got.Reason, tc.reason) {
			t.Fatalf("%s: %#v", tc.goos, got)
		}
	}
	if got := available(nil, nil, "0.1.15", "darwin", "arm64"); got != nil {
		t.Fatalf("no release must stay current: %#v", got)
	}
}
