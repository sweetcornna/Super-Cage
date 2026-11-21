package appupdate

import (
	"errors"
	"testing"
	"time"
)

// An unattended download fetches only the files the workbench judged, and
// only once they are old enough; its failures say whether a later try can
// help.
func TestUnattendedDownloadChecksTheJudgedRelease(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	c := candidate{Version: "0.1.16", Release: "0123456789abcdef0123456789abcdef", ChangedAt: now.Add(-48 * time.Hour).Format(time.RFC3339)}
	if err := unattendedRelease(c, c.Release, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*candidate)
		release string
		code    string
	}{
		{"files replaced since the decision", func(*candidate) {}, "ffffffffffffffffffffffffffffffff", "changed"},
		{"a file uploaded an hour ago", func(c *candidate) { c.ChangedAt = now.Add(-time.Hour).Format(time.RFC3339) }, c.Release, "too_new"},
		{"no upload time", func(c *candidate) { c.ChangedAt = "" }, c.Release, "too_new"},
		{"a time in the future", func(c *candidate) { c.ChangedAt = now.Add(time.Hour).Format(time.RFC3339) }, c.Release, "too_new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := c
			tc.mutate(&changed)
			err := unattendedRelease(changed, tc.release, now)
			var state State
			state.fail(err)
			if err == nil || state.Code != tc.code {
				t.Fatalf("%v: code %q", err, state.Code)
			}
		})
	}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{&integrityError{errors.New("发行包 SHA-256 校验失败，请重新下载")}, "verification"},
		{&transientError{errors.New("连接中断")}, "transient"},
		{errors.New("更新源返回 HTTP 404"), "transient"},
	} {
		var state State
		state.fail(tc.err)
		if state.Code != tc.code {
			t.Fatalf("%v: %q", tc.err, state.Code)
		}
	}
	if !releaseKeyPattern.MatchString(c.Release) || releaseKeyPattern.MatchString("../x") {
		t.Fatal("release key form")
	}
}
