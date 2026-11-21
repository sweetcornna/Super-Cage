package tenant

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func vmID(n int) string { return strings.Repeat(fmt.Sprintf("%x", n%16), 32) }

// guestVM is a running VM whose guest uses working bytes and holds cache.
func guestVM(limit, working, cache int64) InstanceMemory {
	total := limit - 200*testMiB
	return InstanceMemory{Status: "Running", Limit: limit, GuestTotal: total, GuestAvailable: max(0, total-working)}
}

func allocate(state MemoryState, held map[string]demandState, now time.Time) allocationResult {
	return allocateMemory(allocationInput{now: now, state: state, minimum: DefaultMemoryMin}, held)
}

// settle applies rounds until nothing changes, the guests giving back cache.
func settle(t *testing.T, state *MemoryState, now *time.Time, rounds int) (map[string]demandState, int) {
	t.Helper()
	held := map[string]demandState{}
	for round := range rounds {
		result := allocate(*state, held, *now)
		held = result.held
		if len(result.changes) == 0 {
			return held, round
		}
		for _, change := range result.changes {
			applyChange(state, change)
		}
		*now = now.Add(15 * time.Second)
	}
	t.Fatalf("no steady state within %d rounds: %+v", rounds, state.Instances)
	return held, rounds
}

func TestWaterFillTable(t *testing.T) {
	for _, test := range []struct {
		capacity          int64
		guarantee, demand []int64
		want              []int64
	}{
		{10, []int64{1, 1}, []int64{3, 4}, []int64{3, 4}},         // everyone met
		{6, []int64{1, 1}, []int64{3, 8}, []int64{3, 3}},          // smallest unmet demand first
		{7, []int64{1, 1}, []int64{3, 8}, []int64{3, 4}},          // the rest goes to the short one
		{5, []int64{1, 1, 1}, []int64{9, 9, 9}, []int64{2, 2, 1}}, // leftover units at the level
		{2, []int64{2, 2}, []int64{5, 5}, []int64{2, 2}},          // guarantees exceed capacity
		{8, []int64{4, 1}, []int64{4, 9}, []int64{4, 4}},          // a guarantee above the level
		{8, []int64{1, 1}, []int64{0, 9}, []int64{1, 7}},          // demand below guarantee
	} {
		if got := waterFill(test.capacity, test.guarantee, test.demand); !slices.Equal(got, test.want) {
			t.Errorf("waterFill(%d, %v, %v) = %v, want %v", test.capacity, test.guarantee, test.demand, got, test.want)
		}
	}
}

func TestWaterFillProperties(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		n := 1 + random.IntN(6)
		guarantee, demand := make([]int64, n), make([]int64, n)
		var floor int64
		for i := range n {
			guarantee[i] = int64(random.IntN(8))
			demand[i] = int64(random.IntN(40))
			floor += guarantee[i]
		}
		capacity := int64(random.IntN(120))
		f := waterFill(capacity, guarantee, demand)
		var sum int64
		short := false
		for i := range n {
			sum += f[i]
			if f[i] < guarantee[i] || (f[i] > max(demand[i], guarantee[i])) {
				t.Fatalf("bounds: %d %v %v -> %v", capacity, guarantee, demand, f)
			}
			if f[i] < demand[i] {
				short = true
			}
		}
		if floor <= capacity && sum > capacity {
			t.Fatalf("over capacity: %d %v %v -> %v", capacity, guarantee, demand, f)
		}
		if floor <= capacity && short && sum != capacity {
			t.Fatalf("not work-conserving: %d %v %v -> %v", capacity, guarantee, demand, f)
		}
		// Max-min fair: a consumer short of its demand has at least as much as
		// any other consumer above its own guarantee, within one unit.
		if floor <= capacity {
			for i := range n {
				for j := range n {
					if f[i] < demand[i] && f[j] > guarantee[j] && f[j] > f[i]+1 {
						t.Fatalf("not max-min fair: %d %v %v -> %v", capacity, guarantee, demand, f)
					}
				}
			}
		}
		// Monotone: more demand never gives a consumer less.
		k := random.IntN(n)
		more := slices.Clone(demand)
		more[k] += 1 + int64(random.IntN(10))
		if g := waterFill(capacity, guarantee, more); g[k] < f[k] {
			t.Fatalf("not monotone: %v -> %v, %v -> %v", demand, f, more, g)
		}
	}
}

// simulateGuests sets each guest's readings from what it wants to use: what
// does not fit stalls it.
func simulateGuests(state *MemoryState, wants map[string]int64) {
	for id, want := range wants {
		entry := state.Instances[id]
		used := min(want, entry.GuestTotal-64*testMiB)
		entry.GuestAvailable, entry.GuestStall = entry.GuestTotal-used, 0
		if used < want {
			entry.GuestStall = 30
		}
		state.Instances[id] = entry
	}
}

func TestAllocationTwoVMsBothGrowing(t *testing.T) {
	// 16 GiB host, reserve 2.4 GiB: 13.6 GiB to share. Both workspaces in use
	// and both growing to a 5.5 GiB working set: each settles at its fair
	// half, above its working set, and the first one does not keep its lead.
	a, b := vmID(10), vmID(11)
	now := time.Unix(1e9, 0)
	state := MemoryState{HostTotal: 16 * testGiB, HostAvailable: 10 * testGiB, Ceiling: 12 * testGiB, Instances: map[string]InstanceMemory{
		a: guestVM(8*testGiB, 0, 0), b: guestVM(3*testGiB, 0, 0),
	}}
	markActive(state, a, b)
	wants := map[string]int64{a: 5632 * testMiB, b: 5632 * testMiB}
	held := map[string]demandState{}
	for range 80 {
		simulateGuests(&state, wants)
		result := allocate(state, held, now)
		held = result.held
		for _, change := range result.changes {
			if change.to < change.from && change.from-change.to > memoryShrinkStep {
				t.Fatalf("step too large %+v", change)
			}
			applyChange(&state, change)
		}
		now = now.Add(15 * time.Second)
	}
	la, lb := state.Instances[a].Limit, state.Instances[b].Limit
	capacity := 16*testGiB - memoryReserve(0, 16*testGiB)
	if la+lb > capacity || la < 5632*testMiB+memoryMinHeadroom || lb < 5632*testMiB+memoryMinHeadroom || la-lb > memoryUnit || lb-la > memoryUnit {
		t.Fatalf("two growing VMs settled at %d and %d MiB (capacity %d MiB)", la>>20, lb>>20, capacity>>20)
	}
}

func TestAllocationIgnoresPageCache(t *testing.T) {
	// A guest that reads a lot of files: MemAvailable stays high, so its
	// demand stays at the working set plus headroom, and the idle VM returns
	// what it does not use.
	id := vmID(1)
	now := time.Unix(1e9, 0)
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, Ceiling: 16 * testGiB, Instances: map[string]InstanceMemory{
		id: {Status: "Running", Limit: 16 * testGiB, GuestTotal: 16*testGiB - 200*testMiB, GuestAvailable: 14 * testGiB},
	}}
	result := allocate(state, nil, now)
	if report := result.report[id]; report.Demand > 4*testGiB {
		t.Fatalf("page cache counted as demand: %+v", report)
	}
	_, _ = settle(t, &state, &now, 100)
	if limit := state.Instances[id].Limit; limit > 4*testGiB {
		t.Fatalf("idle cache hog kept %d MiB", limit>>20)
	}
}

func TestAllocationHoldsDemandBeforeReturningIdleMemory(t *testing.T) {
	id := vmID(2)
	now := time.Unix(1e9, 0)
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, Ceiling: 16 * testGiB, Instances: map[string]InstanceMemory{
		// Sized at its demand: 7 GiB + 200 MiB kernel + 1792 MiB headroom.
		id: guestVM(9216*testMiB, 7*testGiB, 0),
	}}
	held := allocate(state, nil, now).held
	// The working set falls to 2 GiB: nothing is returned within the hold.
	entry := state.Instances[id]
	entry.GuestAvailable = entry.GuestTotal - 2*testGiB
	state.Instances[id] = entry
	for elapsed := time.Duration(0); elapsed < memoryDemandHold; elapsed += 15 * time.Second {
		result := allocate(state, held, now.Add(elapsed))
		if len(result.changes) != 0 {
			t.Fatalf("shrank %s after the demand fell: %+v", elapsed, result.changes)
		}
		held = result.held
	}
	result := allocate(state, held, now.Add(memoryDemandHold+time.Second))
	if len(result.changes) != 1 || result.changes[0].from-result.changes[0].to != memoryShrinkStep || result.changes[0].reason != "idle memory returned" {
		t.Fatalf("idle return after the hold: %+v", result.changes)
	}
	// An in-use VM keeps its excess while nobody needs it.
	markActive(state, id)
	if result := allocate(state, held, now.Add(memoryDemandHold+time.Second)); len(result.changes) != 0 {
		t.Fatalf("an in-use VM gave back without need: %+v", result.changes)
	}
}

// Being in use only orders who gives and spares a VM idle return; it never
// keeps memory another VM needs. The gateway counts every proxied chunk,
// terminal heartbeats included, as use, so a workspace with an open
// terminal is in use for good: it still yields what lies above its fair
// allocation to a VM that grows (and, under host pressure, what lies above
// its working set: TestAllocationHostPressureTakesIdleFirstAndNeverTheWorkingSet).
func TestAllocationActiveVMStillYieldsToAGrowingOne(t *testing.T) {
	busy, growing := vmID(5), vmID(6)
	now := time.Unix(1e9, 0)
	// 16 GiB host, 13.6 GiB to share. The busy VM was sized up while it was
	// alone and now holds 10 GiB, mostly page cache over a 2 GiB working
	// set; the other one starts at 3 GiB and needs 6 GiB.
	state := MemoryState{HostTotal: 16 * testGiB, HostAvailable: 8 * testGiB, Ceiling: 12 * testGiB, Instances: map[string]InstanceMemory{
		busy: guestVM(10*testGiB, 2*testGiB, 0), growing: guestVM(3*testGiB, 0, 0),
	}}
	held := map[string]demandState{}
	yielded := false
	for round := range 60 {
		// Both are in use every round, the busy one through its heartbeat.
		markActive(state, busy, growing)
		simulateGuests(&state, map[string]int64{busy: 2 * testGiB, growing: 6 * testGiB})
		result := allocate(state, held, now)
		held = result.held
		for _, change := range result.changes {
			if change.id == busy && change.to < change.from {
				if change.reason != "others need memory" || change.to < result.report[busy].Fair {
					t.Fatalf("round %d: %+v (fair %d MiB)", round, change, result.report[busy].Fair>>20)
				}
				yielded = true
			}
			applyChange(&state, change)
		}
		now = now.Add(15 * time.Second)
	}
	lb, lg := state.Instances[busy].Limit, state.Instances[growing].Limit
	if !yielded || lg < 6*testGiB+memoryMinHeadroom {
		t.Fatalf("in-use VM kept memory a growing VM needs: busy %d MiB, growing %d MiB", lb>>20, lg>>20)
	}
	// The grower has its demand, and nobody is short: the busy VM keeps
	// the rest, even with the limits together above the capacity, as a limit
	// is not use. Once its page cache makes the host short, it gives.
	state.HostAvailable = 3 * testGiB
	markActive(state, busy, growing)
	result := allocate(state, held, now)
	if change, ok := changedIDs(result.changes)[busy]; !ok || change.to >= change.from || change.reason != "host memory is low" {
		t.Fatalf("in-use VM kept its excess while the host is short: %+v", result.changes)
	}
}

func TestAllocationHostPressureTakesIdleFirstAndNeverTheWorkingSet(t *testing.T) {
	idle, busy := vmID(3), vmID(4)
	now := time.Unix(1e9, 0)
	// 16 GiB host with 3 GiB available (twice the reserve is 4.8 GiB).
	state := MemoryState{HostTotal: 16 * testGiB, HostAvailable: 3 * testGiB, Ceiling: 8 * testGiB, Instances: map[string]InstanceMemory{
		idle: guestVM(6*testGiB, 2*testGiB, 0), busy: guestVM(6*testGiB, 2*testGiB, 0),
	}}
	markActive(state, busy)
	result := allocate(state, nil, now)
	changes := changedIDs(result.changes)
	if _, ok := changes[idle]; !ok || changes[idle].reason != "host memory is low" {
		t.Fatalf("idle VM must give first under pressure: %+v", result.changes)
	}
	if _, ok := changes[busy]; ok {
		t.Fatalf("in-use VM shrank while the idle one had slack: %+v", result.changes)
	}
	// Lasting pressure: both end at their working set plus the minimum
	// headroom at the lowest, never below.
	for range 40 {
		result = allocate(state, result.held, now)
		if len(result.changes) == 0 {
			break
		}
		for _, change := range result.changes {
			if change.from-change.to > memoryShrinkStep {
				t.Fatalf("step too large: %+v", change)
			}
			applyChange(&state, change)
		}
	}
	for _, id := range []string{idle, busy} {
		entry := state.Instances[id]
		if working := entry.GuestTotal - entry.GuestAvailable; entry.Limit < working+memoryMinHeadroom || entry.Limit < DefaultMemoryMin {
			t.Fatalf("%s went below its working set: %+v", id, entry)
		}
	}
}

func TestAllocationSampleFailureHolds(t *testing.T) {
	id, other := vmID(5), vmID(6)
	now := time.Unix(1e9, 0)
	state := MemoryState{HostTotal: 16 * testGiB, HostAvailable: 2 * testGiB, Ceiling: 8 * testGiB, Instances: map[string]InstanceMemory{
		id:    {Status: "Running", Limit: 8 * testGiB, GuestSilent: true},
		other: guestVM(4*testGiB, testGiB, 0),
	}}
	result := allocate(state, nil, now)
	if _, ok := changedIDs(result.changes)[id]; ok {
		t.Fatalf("a failed sample changed the VM: %+v", result.changes)
	}
}

func TestAllocationNestedGuestWantGrowsTheMachine(t *testing.T) {
	// A machine parked at its working set plus headroom whose nested guest
	// wants 3 GiB more than the machine can back: one round covers it.
	id := vmID(7)
	now := time.Unix(1e9, 0)
	entry := guestVM(4352*testMiB, 3*testGiB, 0)
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, Ceiling: 16 * testGiB, Instances: map[string]InstanceMemory{id: entry}}
	if result := allocate(state, nil, now); len(result.changes) != 0 {
		t.Fatalf("parked machine resized: %+v", result.changes)
	}
	entry.NestedShort = 3 * testGiB
	state.Instances[id] = entry
	result := allocate(state, nil, now)
	if len(result.changes) != 1 || result.changes[0].to < (4352+3072)*testMiB || result.changes[0].reason != "nested guest needs memory" {
		t.Fatalf("nested want not covered: %+v", result.changes)
	}
}

func TestAllocationShortGuestGrowsAtOnce(t *testing.T) {
	id := vmID(8)
	now := time.Unix(1e9, 0)
	// Stalled, with little available: demand is at least a quarter more.
	entry := guestVM(4*testGiB, 3800*testMiB, 0)
	entry.GuestStall = 25
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, Ceiling: 16 * testGiB, Instances: map[string]InstanceMemory{id: entry}}
	result := allocate(state, nil, now)
	if len(result.changes) != 1 || result.changes[0].to < 5*testGiB {
		t.Fatalf("short guest plan %+v", result.changes)
	}
	// Swap-ins alone are a shortage too.
	entry.GuestStall = 0
	entry.GuestAvailable = entry.GuestTotal - 2*testGiB
	state.Instances[id] = entry
	plain := allocate(state, nil, now)
	swapping := allocateMemory(allocationInput{now: now, state: state, minimum: DefaultMemoryMin, swapIn: map[string]bool{id: true}}, nil)
	if plain.report[id].Demand >= swapping.report[id].Demand {
		t.Fatalf("swap-ins did not raise demand: %+v %+v", plain.report[id], swapping.report[id])
	}
}

// randomState is a host with a few VMs in random states.
func randomState(random *rand.Rand) MemoryState {
	host := int64(8+random.IntN(56)) * testGiB
	state := MemoryState{HostTotal: host, HostAvailable: int64(random.IntN(int(host>>20))) * testMiB, Ceiling: int64(2+random.IntN(30)) * testGiB,
		HostStall: float64(random.IntN(3)) * 6, Instances: map[string]InstanceMemory{}}
	for i := range 1 + random.IntN(5) {
		limit := int64(1+random.IntN(int(state.Ceiling>>28))) * memoryUnit
		limit = min(max(limit, MinimumMemory), state.Ceiling)
		total := max(limit-int64(random.IntN(400))*testMiB, 512*testMiB)
		entry := InstanceMemory{Status: "Running", Limit: limit, GuestTotal: total, GuestAvailable: int64(random.IntN(int(total>>20))) * testMiB,
			Active: random.IntN(2) == 0, GuestStall: float64(random.IntN(4)) * 5}
		switch random.IntN(10) {
		case 0:
			entry = InstanceMemory{Status: "Frozen", Limit: limit}
		case 1:
			entry.GuestSilent, entry.GuestTotal, entry.GuestAvailable = true, 0, 0
		}
		state.Instances[vmID(i)] = entry
	}
	return state
}

func TestAllocationProperties(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 4))
	now := time.Unix(1e9, 0)
	for range 5000 {
		state := randomState(random)
		result := allocate(state, nil, now)
		reserve := memoryReserve(0, state.HostTotal)
		severe := state.HostAvailable < reserve || state.HostStall >= 10
		step := int64(memoryShrinkStep)
		if severe {
			step = memoryPressureStep
		}
		var grown int64
		var fairSum int64
		for id, report := range result.report {
			if state.Instances[id].GuestSilent {
				continue
			}
			fairSum += report.Fair
		}
		if !result.overcommitted && fairSum > result.capacity {
			t.Fatalf("fair allocations exceed capacity: %d > %d (%+v)", fairSum, result.capacity, state)
		}
		for _, change := range result.changes {
			entry := state.Instances[change.id]
			if change.to%(1<<20) != 0 || change.to < MinimumMemory {
				t.Fatalf("invalid size %+v", change)
			}
			if change.to > entry.Limit && change.reason != "guest did not answer" {
				grown += change.to - entry.Limit
			}
			// Never from above the fair allocation to below it.
			if fair := result.report[change.id].Fair; change.to < entry.Limit && entry.Limit > fair && change.to < fair {
				t.Fatalf("shrank across the fair allocation %d: %+v", fair, change)
			}
			if change.to < entry.Limit {
				working := entry.GuestTotal - entry.GuestAvailable
				if entry.GuestSilent || entry.Status != "Running" {
					t.Fatalf("shrank a VM without a sample: %+v", change)
				}
				if change.to < working+memoryMinHeadroom || change.to < DefaultMemoryMin && entry.Limit >= DefaultMemoryMin {
					t.Fatalf("below working set or guarantee: %+v %+v", change, entry)
				}
				if entry.Limit-change.to > step {
					t.Fatalf("step too large: %+v", change)
				}
			}
		}
		// Growth stays within the host's free memory above what it keeps.
		if grown > max(0, state.HostAvailable-hostKeep(reserve)) {
			t.Fatalf("grew %d MiB with %d MiB available: %+v", grown>>20, state.HostAvailable>>20, result.changes)
		}
		if result.backlog > 0 {
			// Growth that waits for free memory or for pressure to end goes on
			// in the next rounds, as the inputs change.
			continue
		}
		// Stable: with the same input and the result applied, a second round
		// moves nothing that the first did not already decide.
		applied := MemoryState{HostTotal: state.HostTotal, HostAvailable: state.HostAvailable, HostStall: state.HostStall, Ceiling: state.Ceiling, Instances: map[string]InstanceMemory{}}
		for id, entry := range state.Instances {
			applied.Instances[id] = entry
		}
		for _, change := range result.changes {
			// The guest that got memory is no longer short.
			applyChange(&applied, change)
			entry := applied.Instances[change.id]
			entry.GuestStall = 0
			applied.Instances[change.id] = entry
		}
		again := allocate(applied, result.held, now)
		for _, change := range again.changes {
			if change.to > change.from && change.reason != "guest did not answer" {
				t.Fatalf("grew again on the same input: first %+v then %+v", result.changes, again.changes)
			}
		}
	}
}

func TestAllocationMonotoneInDemand(t *testing.T) {
	random := rand.New(rand.NewPCG(5, 6))
	now := time.Unix(1e9, 0)
	for range 3000 {
		state := randomState(random)
		var ids []string
		for id, entry := range state.Instances {
			if entry.Status == "Running" && !entry.GuestSilent {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		slices.Sort(ids)
		id := ids[random.IntN(len(ids))]
		before := allocate(state, nil, now).report[id].Fair
		entry := state.Instances[id]
		entry.GuestAvailable = max(0, entry.GuestAvailable-int64(1+random.IntN(2048))*testMiB)
		state.Instances[id] = entry
		if after := allocate(state, nil, now).report[id].Fair; after < before {
			t.Fatalf("more demand gave less: %d -> %d", before, after)
		}
	}
}

func TestAllocationStableUnderConstantDemand(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 8))
	for range 300 {
		state := randomState(random)
		state.HostStall = 0
		state.HostAvailable = state.HostTotal / 2
		for id, entry := range state.Instances {
			entry.GuestStall = 0
			state.Instances[id] = entry
		}
		now := time.Unix(1e9, 0)
		held := map[string]demandState{}
		var last []memoryChange
		quiet := 0
		for round := 0; round < 400 && quiet < 3; round++ {
			result := allocate(state, held, now)
			held, last = result.held, result.changes
			if len(last) == 0 {
				quiet++
			} else {
				quiet = 0
			}
			for _, change := range last {
				// Guests with constant working sets: MemTotal follows the
				// balloon, the working set stays.
				applyChange(&state, change)
			}
			now = now.Add(15 * time.Second)
		}
		if quiet < 3 {
			t.Fatalf("constant demand never settled, last changes %+v", last)
		}
	}
}

func TestAllocationNestedPressureIsAShortageUnderHostPressure(t *testing.T) {
	// The host is short of memory. A machine whose nested guest only lacks
	// headroom waits; one whose nested guest is under pressure grows.
	id := vmID(8)
	now := time.Unix(1e9, 0)
	entry := guestVM(4352*testMiB, 3*testGiB, 0)
	entry.NestedShort = 1536 * testMiB
	state := MemoryState{HostTotal: 16 * testGiB, HostAvailable: 2 * testGiB, Ceiling: 12 * testGiB, Instances: map[string]InstanceMemory{id: entry}}
	if result := allocate(state, nil, now); !result.pressure || len(result.changes) != 0 && result.changes[0].to > entry.Limit {
		t.Fatalf("missing headroom grew under host pressure: %+v", result.changes)
	}
	entry.NestedPressure = true
	state.Instances[id] = entry
	result := allocate(state, nil, now)
	if len(result.changes) != 1 || result.changes[0].to <= entry.Limit || result.changes[0].reason != "nested guest needs memory" {
		t.Fatalf("nested shortage under host pressure: %+v", result.changes)
	}
}
