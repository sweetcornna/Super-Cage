package policy

import (
	"strings"
	"testing"
	"time"
)

// Ported from the third review's experiments
// (review3-update/tree/internal/appupdate/policy/review_experiment_test.go).

func TestHoldIsTheSameForTickAndDecision(t *testing.T) {
	var h HoldTracker
	h.Observe(Snapshot{At: now, Fingerprint: "f"})
	almost := now.Add(HoldPeriod - 2*time.Second)
	if !h.Held(almost, "f") {
		t.Fatal("59m58s of idle is not held")
	}
	if h.Held(now.Add(HoldPeriod-HoldSlack-time.Second), "f") {
		t.Fatal("held before the slack")
	}
	f := silentFacts()
	f.Hold, f.Now = h, almost
	f.Snapshot.Fingerprint = "f"
	if d := Decide(Request{Trigger: SilentUpdate, Version: "0.1.16"}, f); !d.Allowed() {
		t.Fatalf("decision disagrees with the tick: %+v", d)
	}
}

func TestRollbackAfterDoneIsRecordedAsAFailure(t *testing.T) {
	var l Ledger
	l.Begin(Attempt{ID: "a", Version: "0.1.17", From: "0.1.16", Trigger: SilentUpdate, StartedAt: now})
	l.Finish("a", "0.1.17", true, StepDone, "", now)
	c := &Chain{Schema: ChainSchema, ID: "a", From: "0.1.16", To: "0.1.17", Step: StepDone, UpdatedAt: now}
	if r := Resume(c, "0.1.16", now, nil); !r.Undone {
		t.Fatalf("old version after done: %+v", r)
	}
	if !l.Undo("a", "0.1.17", "rolled back", now) || !l.FailedBefore("0.1.17") {
		t.Fatalf("undone version still eligible: %+v", l)
	}
}

func TestSilentRetryScheduleAndVerification(t *testing.T) {
	var l Ledger
	if next, ok := l.SilentRetry("0.1.16", "r1", now); !ok || !next.Equal(now) {
		t.Fatal("first request waits")
	}
	at := now
	for i, wait := range RetrySchedule {
		l.Request("q"+string(rune('a'+i)), "0.1.16", "0.1.15", "r1", at)
		next, ok := l.SilentRetry("0.1.16", "r1", at)
		if !ok || !next.Equal(at.Add(wait)) {
			t.Fatalf("after request %d: %v %v", i+1, next, ok)
		}
		at = next
	}
	l.Request("qz", "0.1.16", "0.1.15", "r1", at)
	if _, ok := l.SilentRetry("0.1.16", "r1", at.Add(1000*time.Hour)); ok {
		t.Fatal("retried past the schedule")
	}
	if _, ok := l.SilentRetry("0.1.16", "r2", at); !ok {
		t.Fatal("new files of the release are blocked by the old ones")
	}
	// Requests that reached the decision or met changed files do not count.
	var m Ledger
	m.Request("a", "0.1.16", "0.1.15", "r1", now)
	m.Report("a", StageDecided, "", now)
	m.Request("b", "0.1.16", "0.1.15", "r1", now)
	m.Report("b", StageChanged, "", now)
	if next, ok := m.SilentRetry("0.1.16", "r1", now); !ok || !next.Equal(now) {
		t.Fatalf("decided or changed requests counted: %v %v", next, ok)
	}
	m.Request("c", "0.1.16", "0.1.15", "r1", now)
	if !m.Report("c", StageVerify, "签名无效", now) || m.Report("unknown", StageVerify, "", now) {
		t.Fatal("report matching")
	}
	if _, ok := m.SilentRetry("0.1.16", "r1", now.Add(1000*time.Hour)); ok {
		t.Fatal("files that failed verification fetched again")
	}
	if m.FailedBefore("0.1.16") || m.Last() != nil {
		t.Fatal("requests read as decided attempts")
	}
}

func TestSaveLedgerKeepsDecisionsAgainstManyRequests(t *testing.T) {
	profile := t.TempDir()
	var l Ledger
	l.Begin(Attempt{ID: "x", Version: "0.1.16", From: "0.1.15", StartedAt: now})
	for i := 0; i < 3*ledgerLimit; i++ {
		l.Request(strings.Repeat("r", i+1), "0.1.17", "0.1.15", "k", now)
	}
	if err := SaveLedger(profile, l); err != nil {
		t.Fatal(err)
	}
	read, err := LoadLedger(profile)
	if err != nil || !read.FailedBefore("0.1.16") || len(read.Attempts) != ledgerLimit+1 {
		t.Fatalf("requests pushed out a failed version: %d %v", len(read.Attempts), err)
	}
}

func TestLostHistoryWaitsForASuccessfulUpdate(t *testing.T) {
	profile := t.TempDir()
	l, err := SetLedgerAside(profile, now)
	if err != nil || !l.LostHistory {
		t.Fatalf("%+v %v", l, err)
	}
	f := silentFacts()
	f.Ledger = l
	if d := Decide(Request{Trigger: SilentUpdate, Version: "0.1.16"}, f); d.Code != Unreadable {
		t.Fatalf("silent with a lost history: %+v", d)
	}
	l.Begin(Attempt{ID: "c", Version: "0.1.16", StartedAt: now})
	l.Finish("c", "0.1.16", true, StepDone, "", now)
	if l.LostHistory {
		t.Fatal("a successful update did not restore silent mode")
	}
}

func TestHostConditionsAreNotNow(t *testing.T) {
	for _, tc := range []struct {
		host *HostState
		code Code
	}{
		{nil, HostUnknown},
		{&HostState{DarkWake: true}, DarkWake},
		{&HostState{LidClosed: true}, LidClosed},
		{&HostState{OnBattery: true, Battery: MinBattery - 1}, LowBattery},
		{&HostState{DisplayHeld: []string{"zoom.us"}}, DisplayHeld},
		{&HostState{OnBattery: true, Battery: MinBattery}, Allowed},
		{&HostState{}, Allowed},
	} {
		f := silentFacts()
		f.Host = tc.host
		if d := Decide(Request{Trigger: SilentUpdate, Version: "0.1.16"}, f); d.Code != tc.code {
			t.Fatalf("%+v: %+v", tc.host, d)
		}
	}
	// A click is the person at the computer: none of this applies.
	if d := Decide(Request{Trigger: Click, Version: "0.1.16"}, Facts{Current: "0.1.15"}); !d.Allowed() {
		t.Fatalf("click: %+v", d)
	}
}

func TestSilentGateClosedByDefault(t *testing.T) {
	for _, goos := range []string{"darwin", "windows", "linux", ""} {
		if SilentVerified(goos) {
			t.Fatalf("%q verified in this tree", goos)
		}
	}
	saved := verifiedPlatforms
	defer func() { verifiedPlatforms = saved }()
	verifiedPlatforms = "windows, darwin"
	if !SilentVerified("darwin") || !SilentVerified("windows") || SilentVerified("linux") {
		t.Fatal("ldflags list not read")
	}
}
