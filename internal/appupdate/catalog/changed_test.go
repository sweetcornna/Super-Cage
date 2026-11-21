package catalog

import (
	"testing"
)

// M6 of the third review: the unattended age counts from the newest file,
// and any change to a file changes the release's identity.
func TestChangedAtCoversReplacedFiles(t *testing.T) {
	r := Release{Tag: "v0.1.16", PublishedAt: "2026-11-01T00:00:00Z", Assets: []Asset{
		{Name: "SHA256SUMS", Size: 10, UpdatedAt: "2026-11-01T00:05:00Z", State: "uploaded"},
		{Name: "Super-Cage-0.1.16-macos-arm64.dmg", Size: 20, UpdatedAt: "2026-11-01T00:04:00Z", State: "uploaded"},
	}}
	if got := r.ChangedAt(); got != "2026-11-01T00:05:00Z" {
		t.Fatalf("changed at %q", got)
	}
	key := r.Key()
	replaced := r
	replaced.Assets = append([]Asset(nil), r.Assets...)
	replaced.Assets[1].UpdatedAt = "2026-11-20T00:00:00Z"
	if got := replaced.ChangedAt(); got != "2026-11-20T00:00:00Z" {
		t.Fatalf("a replaced image did not move the age: %q", got)
	}
	if replaced.Key() == key {
		t.Fatal("a replaced image kept the release's identity")
	}
	reordered := r
	reordered.Assets = []Asset{r.Assets[1], r.Assets[0]}
	if reordered.Key() != key {
		t.Fatal("the identity depends on the listing order")
	}
	missing := r
	missing.Assets = []Asset{{Name: "SHA256SUMS", Size: 10, State: "uploaded"}}
	if missing.ChangedAt() != "" {
		t.Fatal("a file without an upload time counts as old")
	}
	if (Release{PublishedAt: "yesterday"}).ChangedAt() != "" {
		t.Fatal("an invalid publication time counts as old")
	}
}
