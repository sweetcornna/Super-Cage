package workbench

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestClaudeLaunchScheduleFollowsTheScheduler(t *testing.T) {
	id := func(c byte) string {
		b := make([]byte, 24)
		for i := range b {
			b[i] = c
		}
		return string(b)
	}
	list := claudeAccountList{
		Scheduling: claudeScheduling{Mode: "sequential", Preferred: id('3')},
		Accounts: []claudeAccount{
			{ID: id('1'), Source: "managed", Status: "ready", Enabled: true, Email: "one@example.test", Organization: "org"},
			{ID: id('2'), Source: "managed", Status: "expired", Enabled: true, Email: "two@example.test"},
			{ID: id('3'), Source: "managed", Status: "ready", Enabled: false, Email: "three@example.test"},
			{ID: id('4'), Source: "default", Status: "ready", Enabled: true, Email: "default@example.test"},
			{ID: id('5'), Source: "managed", Status: "ready", Enabled: false, Email: "five@example.test"},
			{ID: id('6'), Source: "managed", Status: "ready", Enabled: true, Email: "six@example.test", ProfileID: id('7')},
		},
	}
	got := claudeLaunchScheduleOf(list)
	want := claudeLaunchSchedule{Mode: "exhaust", Managed: true, SelectedAccountID: id('3'),
		AccountOrder: []string{id('1'), id('2'), id('3'), "default", id('5'), id('7')}, Accounts: []claudeLaunchCandidate{
			{id('3'), claudeIdentityHash("three@example.test", ""), true},
			{id('1'), claudeIdentityHash("one@example.test", "org"), false},
			{"default", claudeIdentityHash("default@example.test", ""), false},
			{id('7'), claudeIdentityHash("six@example.test", ""), false},
		}, HistoryAccounts: []cliHistoryAccount{
			{id('1'), claudeIdentityHash("one@example.test", "org"), ""},
			{id('2'), claudeIdentityHash("two@example.test", ""), ""},
			{id('3'), claudeIdentityHash("three@example.test", ""), ""},
			{id('5'), claudeIdentityHash("five@example.test", ""), ""},
			{id('7'), claudeIdentityHash("six@example.test", ""), ""},
		}}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("schedule = %s, want %s", a, b)
	}
	// Nothing to schedule still publishes an empty list, so the guest stops
	// using an account that was removed.
	if a, _ := json.Marshal(claudeLaunchScheduleOf(claudeAccountList{})); string(a) != `{"mode":"smart","managed":false,"accounts":[]}` {
		t.Fatalf("empty schedule = %s", a)
	}
}

func TestNativeHistorySchedulePreservesOriginalProfiles(t *testing.T) {
	const id = "111111111111111111111111"
	const profile = "222222222222222222222222"
	const label = "Original profile"
	const email = "old@example.test"
	subject := claudeIdentityHash(email, "")
	claude := claudeLaunchScheduleOf(claudeAccountList{Accounts: []claudeAccount{
		{ID: id, Source: "managed", ProfileID: profile, Email: email, Label: label, Status: "signed_out"},
	}})
	codex := codexLaunchScheduleOf(codexAccountList{Accounts: []codexAccount{
		{ID: id, Email: email, Label: label, Status: "ready", Enabled: false},
	}})
	for _, result := range []struct {
		schedule claudeLaunchSchedule
		want     []cliHistoryAccount
	}{
		{claude, []cliHistoryAccount{{profile, subject, label}, {"default", subject, "默认配置 · " + label}}},
		{codex, []cliHistoryAccount{{id, subject, label}}},
	} {
		if len(result.schedule.Accounts) != 0 || !slices.Equal(result.schedule.HistoryAccounts, result.want) {
			t.Fatalf("schedule = %+v, want history %v with no rotation candidates", result.schedule, result.want)
		}
	}
}
