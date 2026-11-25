package workbench

import (
	"encoding/json"
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
	want := claudeLaunchSchedule{Mode: "sequential", Accounts: []claudeLaunchCandidate{
		{id('3'), claudeIdentityHash("three@example.test", ""), true},
		{id('1'), claudeIdentityHash("one@example.test", "org"), false},
		{id('7'), claudeIdentityHash("six@example.test", ""), false},
	}}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("schedule = %s, want %s", a, b)
	}
	// Nothing to schedule still publishes an empty list, so the guest stops
	// using an account that was removed.
	if a, _ := json.Marshal(claudeLaunchScheduleOf(claudeAccountList{})); string(a) != `{"mode":"balanced","accounts":[]}` {
		t.Fatalf("empty schedule = %s", a)
	}
}
