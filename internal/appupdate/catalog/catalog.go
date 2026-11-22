// Package catalog selects verifiable release assets for native application updates.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Repository is the renamed Super Cage repository. GitHub serves asset URLs
// under the current name, so download URLs under the old name are not trusted.
const Repository = "sweetcornna/Super-Cage"
const ReleasesURL = "https://api.github.com/repos/" + Repository + "/releases?per_page=100"

// Releases must stay below the limit of every published App that still updates
// in place; scripts/prepare-release-assets.mjs enforces that smaller limit.
const MaxArchive = 2 << 30

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	State  string `json:"state"`
	// UpdatedAt is when the file was last uploaded. Replacing a file of an
	// old release moves it, while the release's publication time stays.
	UpdatedAt string `json:"updated_at"`
}
type Release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	// PublishedAt is GitHub's RFC 3339 publication time. Unattended updates
	// require a minimum age; an absent or invalid time is never old enough.
	PublishedAt string  `json:"published_at"`
	Assets      []Asset `json:"assets"`
}
type Candidate struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
	// ChangedAt and Release are Release.ChangedAt and Release.Key.
	ChangedAt string `json:"changed_at,omitempty"`
	Release   string `json:"release,omitempty"`
	Archive   Asset  `json:"archive"`
	Checksums Asset  `json:"checksums"`
}

// ChangedAt is the newest of the release's publication time and every
// file's upload time, in RFC 3339: the moment the release last changed. It
// is empty when any of them is missing or invalid, which the unattended age
// rule reads as "too new".
func (r Release) ChangedAt() string {
	newest, err := time.Parse(time.RFC3339, r.PublishedAt)
	if err != nil {
		return ""
	}
	for _, a := range r.Assets {
		t, err := time.Parse(time.RFC3339, a.UpdatedAt)
		if err != nil {
			return ""
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest.UTC().Format(time.RFC3339)
}

// Key identifies the published files of a release: it changes when any file
// is added, removed or replaced.
func (r Release) Key() string {
	assets := append([]Asset(nil), r.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\n", r.Tag, r.PublishedAt)
	for _, a := range assets {
		fmt.Fprintf(hash, "%s\x00%d\x00%s\x00%s\x00%s\n", a.Name, a.Size, a.Digest, a.UpdatedAt, a.State)
	}
	return hex.EncodeToString(hash.Sum(nil))[:32]
}

// Newer is the newest release on the current channel, whether or not it has a
// package this platform may install in place.
type Newer struct {
	Version     string
	PublishedAt string
	// Reason explains, in the product's words, why the release can only be
	// installed by hand. It is empty when Select would choose this release.
	Reason string
}

// archiveNames lists the accepted package names, current name first. Releases
// before the Super Cage rename used the "Cage-" prefix.
func archiveNames(version, goos, goarch string) ([]string, error) {
	var suffix string
	switch {
	case goos == "darwin" && goarch == "arm64":
		suffix = "-macos-arm64.dmg"
	case goos == "windows" && goarch == "amd64":
		suffix = "-windows-amd64-setup.exe"
	// Linux only learns that a newer package exists; the system installs it.
	case goos == "linux" && goarch == "amd64":
		suffix = "-linux-amd64.deb"
	case goos == "android" && goarch == "arm64":
		suffix = "-android-arm64.apk"
	default:
		return nil, errors.New("此平台尚无 Super Cage 应用内更新发行包")
	}
	return []string{"Super-Cage-" + version + suffix, "Cage-" + version + suffix}, nil
}

// onChannel reports whether r is a published release newer than current on
// current's channel: a stable version never moves to a prerelease.
func onChannel(r Release, currentTag string) bool {
	if r.Draft || !strings.HasPrefix(r.Tag, "v") || !semver.IsValid(r.Tag) || semver.Canonical(r.Tag) != r.Tag || semver.Compare(r.Tag, currentTag) <= 0 {
		return false
	}
	pre := r.Prerelease || semver.Prerelease(r.Tag) != ""
	return semver.Prerelease(currentTag) != "" || !pre
}

func currentTag(current string) (string, error) {
	tag := "v" + current
	if !semver.IsValid(tag) || semver.Canonical(tag) != tag {
		return "", errors.New("当前应用版本无效，无法安全比较更新")
	}
	return tag, nil
}

// Latest returns the newest release on current's channel and, when Select
// would not install it in place, the reason a person must install it. A
// newer release without a usable package is therefore reported instead of
// looking like "no update". It never widens what Select accepts.
func Latest(releases []Release, current, goos, goarch string) (*Newer, error) {
	tag, err := currentTag(current)
	if err != nil {
		return nil, err
	}
	var newest *Release
	for i, r := range releases {
		if onChannel(r, tag) && (newest == nil || semver.Compare(r.Tag, newest.Tag) > 0) {
			newest = &releases[i]
		}
	}
	if newest == nil {
		return nil, nil
	}
	version := strings.TrimPrefix(newest.Tag, "v")
	n := &Newer{Version: version, PublishedAt: newest.PublishedAt}
	if c, err := Select([]Release{*newest}, current, goos, goarch); err != nil || c == nil {
		n.Reason = manualReason(*newest, version, goos, goarch)
	}
	return n, nil
}

// manualReason names the published package a person can install by hand.
// Unnotarized macOS images and unsigned Windows installers are published for
// manual installation only; the in-app updater keeps refusing them.
func manualReason(r Release, version, goos, goarch string) string {
	has := func(name string) bool {
		for _, a := range r.Assets {
			if a.Name == name && a.State == "uploaded" {
				return true
			}
		}
		return false
	}
	switch {
	case goos == "darwin" && goarch == "arm64" && has("Super-Cage-"+version+"-macos-arm64-signed.dmg"):
		return "此版本的 macOS 安装包未经 Apple 公证，暂不能在应用内安装。请下载 DMG，退出 Super Cage 后替换应用。"
	case goos == "windows" && goarch == "amd64" && has("Super-Cage-"+version+"-windows-amd64-setup-unsigned.exe"):
		return "此版本的 Windows 安装包没有发布者签名，不能在应用内安装。请保存工作、停止环境并退出 Super Cage 后运行安装包。"
	}
	return "此版本没有可在应用内安装的本平台安装包，请从发布页面下载。"
}

func Select(releases []Release, current, goos, goarch string) (*Candidate, error) {
	if _, err := archiveNames(current, goos, goarch); err != nil {
		return nil, err
	}
	currentTag, err := currentTag(current)
	if err != nil {
		return nil, err
	}
	var best *Candidate
	for _, r := range releases {
		if !onChannel(r, currentTag) {
			continue
		}
		v := strings.TrimPrefix(r.Tag, "v")
		if best != nil && semver.Compare(r.Tag, "v"+best.Version) <= 0 {
			continue
		}
		c := Candidate{Version: v, PublishedAt: r.PublishedAt, ChangedAt: r.ChangedAt(), Release: r.Key()}
		names, _ := archiveNames(v, goos, goarch)
		rank := len(names)
		for _, a := range r.Assets {
			if a.State != "uploaded" {
				continue
			}
			for i, name := range names {
				if i < rank && a.Name == name && a.Size > 0 && a.Size <= MaxArchive {
					c.Archive, rank = a, i
				}
			}
			if a.Name == "SHA256SUMS" && a.Size > 0 && a.Size <= 64<<10 {
				c.Checksums = a
			}
		}
		if c.Archive.URL == "" {
			continue
		}
		base := "https://github.com/" + Repository + "/releases/download/" + r.Tag + "/"
		if c.Archive.URL != base+c.Archive.Name || (c.Checksums.URL != "" && c.Checksums.URL != base+"SHA256SUMS") {
			continue
		}
		if c.Archive.Digest == "" && c.Checksums.URL == "" {
			continue
		}
		best = &c
	}
	return best, nil
}

func DigestValue(raw string) (string, error) {
	if !strings.HasPrefix(raw, "sha256:") {
		return "", errors.New("发行包缺少有效的 SHA-256 摘要")
	}
	h := strings.TrimPrefix(raw, "sha256:")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return "", errors.New("发行包 SHA-256 摘要无效")
	}
	return strings.ToLower(h), nil
}

func (c Candidate) SHA256(b []byte) (string, error) {
	var hash string
	if c.Archive.Digest != "" {
		var err error
		hash, err = DigestValue(c.Archive.Digest)
		if err != nil {
			return "", err
		}
	}
	if c.Checksums.URL == "" {
		if hash == "" {
			return "", errors.New("发行包缺少校验信息")
		}
		return hash, nil
	}
	found := ""
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != c.Archive.Name {
			continue
		}
		h, err := DigestValue("sha256:" + fields[0])
		if err != nil || found != "" {
			return "", errors.New("发行包校验清单无效或重复")
		}
		found = h
	}
	if found == "" || (hash != "" && hash != found) {
		return "", errors.New("发行包与校验清单的 SHA-256 不一致")
	}
	return found, nil
}
