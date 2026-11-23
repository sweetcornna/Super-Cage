package tenant

import (
	"slices"
	"testing"
	"time"
)

// startBoundRun drives the gateway's rounds (boundStarts, then
// allocateMemory) over three VMs: a, a stopped environment's VM at the
// guarantee that publishes a start request whenever asks says so; b and c, two
// workspaces in use, b with a working set that follows bWorking. The host's
// own processes use 1 GiB and the VMs' limits are resident (page cache fills
// them).
type startBoundRun struct {
	state   MemoryState
	results []allocationResult
	expired []time.Duration // when a's request went over its window
	grew    []time.Duration // when a grew
}

func runStartBound(host int64, rounds int, bound bool, asks func(time.Duration) bool, bWorking func(time.Duration) int64) startBoundRun {
	a, b, c := vmID(1), vmID(2), vmID(3)
	start := time.Unix(1e9, 0)
	limits := map[string]int64{a: 2048 * testMiB, b: 6 * testGiB, c: 3 * testGiB}
	held, seen := map[string]demandState{}, map[string]startSight{}
	var run startBoundRun
	for round := range rounds {
		elapsed := time.Duration(round) * 15 * time.Second
		now := start.Add(elapsed)
		entry := stoppedVM(limits[a])
		if asks(elapsed) {
			entry.NestedShort, entry.NestedStart, entry.NestedPressure = 6912*testMiB, true, true
		}
		used := limits[a] + limits[b] + limits[c] + testGiB
		run.state = MemoryState{HostTotal: host, HostAvailable: max(0, host-used), Ceiling: 9728 * testMiB, Instances: map[string]InstanceMemory{
			a: entry, b: guestVM(limits[b], bWorking(elapsed), 0), c: guestVM(limits[c], 1536*testMiB, 0)}}
		markActive(run.state, b, c)
		if bound {
			var expired []string
			if seen, expired = boundStarts(now, run.state, seen, held); slices.Contains(expired, a) {
				run.expired = append(run.expired, elapsed)
			}
		}
		result := allocate(run.state, held, now)
		held = result.held
		for _, change := range result.changes {
			limits[change.id] = change.to
			if change.id == a && change.to > change.from {
				run.grew = append(run.grew, elapsed)
			}
		}
		run.results = append(run.results, result)
	}
	for id, limit := range limits {
		entry := run.state.Instances[id]
		entry.Limit = limit
		run.state.Instances[id] = entry
	}
	return run
}

func TestStartRequestThatNeverEndsBecomesOrdinaryDemand(t *testing.T) {
	// a rewrites machine-memory.json with reason "start" every 30 s for ten
	// minutes (fresh at every round); at five minutes b's working set grows to
	// 6 GiB. 16 GiB host: 13.5 GiB to share.
	a, b := vmID(1), vmID(2)
	always := func(time.Duration) bool { return true }
	bWorking := func(elapsed time.Duration) int64 {
		if elapsed >= 5*time.Minute {
			return 6 * testGiB
		}
		return 2 * testGiB
	}
	run := runStartBound(16*testGiB, 41, true, always, bWorking)
	// Honoured as a start for its window, then not at all: one expiry, at the
	// first round past 150 s, and a never grows after it.
	if !slices.Equal(run.expired, []time.Duration{165 * time.Second}) || len(run.grew) == 0 || run.grew[len(run.grew)-1] > 150*time.Second {
		t.Fatalf("expired at %v, a grew at %v", run.expired, run.grew)
	}
	// From the round after the expiry a's held demand is its own working set
	// plus headroom (the guarantee), so the idle return takes the grant back.
	if held := run.results[12].report[a].Held; held != DefaultMemoryMin {
		t.Fatalf("a's held demand after its window: %d MiB", held>>20)
	}
	if limit := run.state.Instances[a].Limit; limit >= DefaultMemoryMin+memoryShrinkStep {
		t.Fatalf("a still holds %d MiB after ten minutes", limit>>20)
	}
	// Nothing is taken from b or c for a after its window: no shrink between
	// the expiry and b's own growth at five minutes.
	for round := 12; round < 20; round++ {
		for _, change := range run.results[round].changes {
			if change.id != a && change.to < change.from {
				t.Fatalf("at %s: %+v after a's window", time.Duration(round)*15*time.Second, change)
			}
		}
	}
	// b is not held below its demand for a: it has all it asks for.
	final := run.results[len(run.results)-1].report[b]
	if limit := run.state.Instances[b].Limit; limit < final.Held || final.Fair < final.Held {
		t.Fatalf("b: limit %d MiB, held demand %d MiB, fair %d MiB", limit>>20, final.Held>>20, final.Fair>>20)
	}
	t.Logf("bounded: a %d MiB, b %d MiB (demand %d MiB) after ten minutes", run.state.Instances[a].Limit>>20, run.state.Instances[b].Limit>>20, final.Held>>20)
	// Without the bound (0.1.17 before it): a keeps its start demand for ever,
	// shares the host with b at the max-min level, and b stays below its own
	// demand.
	unbounded := runStartBound(16*testGiB, 41, false, always, bWorking)
	last := unbounded.results[len(unbounded.results)-1].report[b]
	if unbounded.state.Instances[a].Limit < 4*testGiB || unbounded.state.Instances[b].Limit >= last.Held {
		t.Fatalf("the scenario does not show the problem: a %d MiB, b %d MiB of %d MiB", unbounded.state.Instances[a].Limit>>20, unbounded.state.Instances[b].Limit>>20, last.Held>>20)
	}
	t.Logf("unbounded: a %d MiB, b %d MiB (demand %d MiB)", unbounded.state.Instances[a].Limit>>20, unbounded.state.Instances[b].Limit>>20, last.Held>>20)
}

func TestNormalStartKeepsItsGrantWithTheBound(t *testing.T) {
	// A real start: the host agent publishes for at most its 75 s wait. It is
	// granted at the first round, never expires, and the grant is held for
	// memoryDemandHold as before (the bound drops a hold only on expiry).
	a := vmID(1)
	run := runStartBound(32*testGiB, 41, true, func(elapsed time.Duration) bool { return elapsed < 75*time.Second },
		func(time.Duration) int64 { return 2 * testGiB })
	if len(run.expired) != 0 || len(run.grew) == 0 || run.grew[0] != 0 {
		t.Fatalf("expired %v, grew %v", run.expired, run.grew)
	}
	grant := run.results[0].changes[0].to
	if grant-310*testMiB < 7*testGiB+256*testMiB {
		t.Fatalf("grant %d MiB does not map a 7 GiB guest", grant>>20)
	}
	for round := 1; round <= 16; round++ { // up to 4 minutes
		for _, change := range run.results[round].changes {
			if change.id == a {
				t.Fatalf("at %s: %+v during the hold", time.Duration(round)*15*time.Second, change)
			}
		}
	}
	if limit := run.state.Instances[a].Limit; limit >= DefaultMemoryMin+memoryShrinkStep {
		t.Fatalf("the abandoned start still holds %d MiB after ten minutes", limit>>20)
	}
}

func TestStartRequestRearmsAfterItHasBeenGone(t *testing.T) {
	a := vmID(1)
	start := time.Unix(1e9, 0)
	seen, held := map[string]startSight{}, map[string]demandState{}
	// round returns whether a's request, present or not at elapsed, is still
	// honoured as a start after the bound.
	round := func(elapsed time.Duration, present bool) bool {
		entry := stoppedVM(2048 * testMiB)
		if present {
			entry.NestedShort, entry.NestedStart, entry.NestedPressure = 6912*testMiB, true, true
		}
		state := MemoryState{Instances: map[string]InstanceMemory{a: entry}}
		seen, _ = boundStarts(start.Add(elapsed), state, seen, held)
		got := state.Instances[a]
		return got.NestedStart && got.NestedPressure && got.NestedShort == 6912*testMiB
	}
	step := 15 * time.Second
	at := time.Duration(0)
	// A start for 60 s, gone 5 minutes and a round: a second start later is a
	// start again.
	for ; at <= 60*time.Second; at += step {
		if !round(at, true) {
			t.Fatalf("first start not honoured at %s", at)
		}
	}
	for ; at < 60*time.Second+startRearm+step; at += step {
		round(at, false)
	}
	if !round(at, true) {
		t.Fatalf("second start at %s not honoured", at)
	}
	// One that outlives its window, gone for 2 minutes only: still ignored;
	// gone for startRearm: honoured again.
	seen = map[string]startSight{}
	for at = 0; at <= 180*time.Second; at += step {
		if honoured := round(at, true); honoured != (at <= startWindow) {
			t.Fatalf("at %s honoured %v", at, honoured)
		}
	}
	last := at - step
	for ; at < last+2*time.Minute; at += step {
		round(at, false)
	}
	if round(at, true) {
		t.Fatalf("an expired request came back after 2 minutes and was honoured")
	}
	last = at
	for at += step; at < last+startRearm; at += step {
		round(at, false)
	}
	if !round(at, true) {
		t.Fatalf("a request gone for %s after its expiry was not honoured", at-last)
	}
	// A running guest's own report (no reason "start") is never touched.
	state := MemoryState{Instances: map[string]InstanceMemory{a: {Status: "Running", NestedShort: testGiB}}}
	if seen, _ = boundStarts(start.Add(time.Hour), state, map[string]startSight{}, held); len(seen) != 0 || state.Instances[a].NestedShort != testGiB {
		t.Fatalf("a running guest's report changed: %+v %+v", state.Instances[a], seen)
	}
}
