package policy

import (
	"errors"
	"time"

	"golang.org/x/mod/semver"
)

var (
	errNotNewer     = errors.New("只能升级到比当前更高的版本")
	errInvalid      = errors.New("版本号无效")
	errPrerelease   = errors.New("正式版不会升级到预发行版")
	errCurrentValue = errors.New("当前应用版本无效，无法安全比较更新")
)

// Eligible checks the version an installer was asked to install against the
// running version: canonical semver, strictly newer, and a prerelease only
// when the running version is itself a prerelease. It repeats the release
// catalog's channel rule so that a tampered caller cannot widen it.
func Eligible(version, current string) error {
	candidate, running := "v"+version, "v"+current
	if !semver.IsValid(running) || semver.Canonical(running) != running {
		return errCurrentValue
	}
	if !semver.IsValid(candidate) || semver.Canonical(candidate) != candidate {
		return errInvalid
	}
	if semver.Compare(candidate, running) <= 0 {
		return errNotNewer
	}
	if semver.Prerelease(candidate) != "" && semver.Prerelease(running) == "" {
		return errPrerelease
	}
	return nil
}

// OldEnough reports whether a release published at publishedAt (RFC 3339)
// is at least ReleaseAge old. An absent or invalid time is never old enough.
func OldEnough(publishedAt string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, publishedAt)
	return err == nil && !t.After(now) && now.Sub(t) >= ReleaseAge
}
