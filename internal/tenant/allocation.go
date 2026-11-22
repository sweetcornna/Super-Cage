package tenant

import (
	"slices"
	"strings"
	"time"
)

// Automatic memory allocation across the gateway's VMs (level A of the Super
// Cage resource design, internal/cage/resource-allocation.md).
//
// Every round each running VM gets a demand: its guest working set (MemTotal -
// MemAvailable, so page cache never counts) plus headroom, raised when the
// guest is short (memory stall, swap-ins, almost nothing available) and by
// what a nested Cage guest wants and its machine cannot back. The demand is
// held for memoryDemandHold before it may fall, one step per round.
//
// The host's capacity (total - reserve - frozen and pinned VMs) is shared by
// max-min fair water-filling over the held demands: every VM gets at least its
// guarantee, the smallest unmet demand is met first, and nothing stays
// unallocated while a VM is short. A VM below its fair allocation grows to it
// at once. Memory above a VM's allocation is taken back only
//   - when others grow into it (capacity needed), idle VMs first, then in-use
//     VMs, in proportion to their slack;
//   - from an idle VM whose held demand is a step below its size, one step per
//     round;
//   - while the host is short, by tiers: idle VMs above their demand, in-use
//     VMs above their demand, then the headroom of idle and then in-use VMs.
//
// Nothing ever goes below the working set plus memoryMinHeadroom, the
// guarantee or (from above it) its fair allocation, a VM whose guest did not
// answer this round keeps its size, and a shrink is at most one step per round
// (memoryShrinkStep, memoryPressureStep when the host is below its reserve).
//
// Page cache of a guest fills its limit and is resident on the host, so a
// host whose limits add up to its capacity has about its reserve (less its
// own use) available: that is the plan, not a shortage. The host is in
// pressure when tasks wait for memory, when less than hostKeep is available,
// or when it is below its reserve while its limits leave more unallocated
// than it has (its own use is above the reserve). Growth happens at once,
// within the host's free memory above hostKeep.

const (
	// memoryDemandHold is how long a VM's demand is held before it may fall.
	memoryDemandHold = 5 * time.Minute
	// memoryMinHeadroom is the least a shrink leaves above the working set.
	memoryMinHeadroom = 512 << 20
	// memoryPressureStep is the shrink step while host memory is below the
	// reserve itself or tasks wait for memory a tenth of the time.
	memoryPressureStep = 1 << 30
	// memoryShortStall is the guest memory stall (PSI some avg10, percent)
	// that marks a guest as short.
	memoryShortStall = 10
)

// hostKeep is the least free memory the host keeps beyond the limits it
// grants in one round: well under its reserve, which also covers its own use.
func hostKeep(reserve int64) int64 { return max(int64(memoryShrinkStep), reserve/4) }

// demandState is the held demand of one VM.
type demandState struct {
	held int64
	at   time.Time
}

// allocationInput is one round of the allocation.
type allocationInput struct {
	now     time.Time
	state   MemoryState
	reserve int64 // resolved host reserve
	minimum int64 // the guarantee, -memory-min
	// fixed VMs keep their size but count against the host; holdShrink VMs
	// may only grow; silent VMs did not answer for several rounds; swapIn VMs
	// swapped pages in since the previous round.
	fixed, holdShrink, silent, swapIn map[string]bool
}

// vmAllocation explains one VM's result.
type vmAllocation struct {
	Working, Demand, Held, Fair, Limit, Target int64
	Reason                                     string
}

type allocationResult struct {
	changes  []memoryChange
	held     map[string]demandState
	report   map[string]vmAllocation
	capacity int64
	pressure bool
	// backlog is what the VMs below their fair allocations did not get this
	// round: growth that waits for pressure to end or for free memory.
	backlog int64
	// overcommitted is set when the guarantees alone exceed the capacity.
	overcommitted bool
}

// waterFill shares capacity among consumers with the given guarantees and
// demands, all in the same unit: a max-min fair allocation in which every
// consumer gets at least its guarantee and at most its demand (or exactly its
// guarantee when the guarantees exceed the capacity). Leftover units below one
// per consumer go to the lowest-numbered consumers at the water level.
func waterFill(capacity int64, guarantee, demand []int64) []int64 {
	n := len(guarantee)
	result := make([]int64, n)
	var floor, want, top int64
	for i := range n {
		d := max(demand[i], guarantee[i])
		floor += guarantee[i]
		want += d
		top = max(top, d)
	}
	level := func(lambda int64) int64 {
		var sum int64
		for i := range n {
			sum += max(guarantee[i], min(max(demand[i], guarantee[i]), lambda))
		}
		return sum
	}
	switch {
	case floor >= capacity:
		copy(result, guarantee)
		return result
	case want <= capacity:
		for i := range n {
			result[i] = max(demand[i], guarantee[i])
		}
		return result
	}
	low, high := int64(0), top
	for low < high {
		mid := low + (high-low+1)/2
		if level(mid) <= capacity {
			low = mid
		} else {
			high = mid - 1
		}
	}
	rest := capacity - level(low)
	for i := range n {
		d := max(demand[i], guarantee[i])
		result[i] = max(guarantee[i], min(d, low))
		if rest > 0 && guarantee[i] <= low && d > low {
			result[i]++
			rest--
		}
	}
	return result
}

// takeProportional takes up to need from the slack of the given VMs, in
// proportion to their slack, at most limit[id] from each. Amounts are in
// memory units. It returns what was taken from each and the rest of need.
func takeProportional(need int64, ids []string, slack, limit map[string]int64) (map[string]int64, int64) {
	taken := make(map[string]int64)
	for need > 0 {
		var total int64
		var open []string
		for _, id := range ids {
			if room := min(slack[id], limit[id]) - taken[id]; room > 0 {
				total += room
				open = append(open, id)
			}
		}
		if total == 0 {
			break
		}
		progress := int64(0)
		for _, id := range open {
			room := min(slack[id], limit[id]) - taken[id]
			share := min(room, max(1, (need*room+total-1)/total), need-progress)
			taken[id] += share
			progress += share
			if progress == need {
				break
			}
		}
		need -= progress
	}
	return taken, need
}

func units(size int64) int64    { return max(size, 0) / memoryUnit }
func unitsUp(size int64) int64  { return (max(size, 0) + memoryUnit - 1) / memoryUnit }
func bytesOf(count int64) int64 { return count * memoryUnit }

// allocateMemory is the pure allocation rule. held carries each VM's held
// demand from the previous round; the result carries it to the next.
func allocateMemory(in allocationInput, held map[string]demandState) allocationResult {
	state := in.state
	result := allocationResult{held: make(map[string]demandState), report: make(map[string]vmAllocation)}
	if state.HostTotal <= 0 || state.Ceiling <= 0 {
		return result
	}
	reserve := memoryReserve(in.reserve, state.HostTotal)
	guarantee := roundDownMemory(min(state.Ceiling, max(in.minimum, MinimumMemory)))
	if guarantee < MinimumMemory {
		return result
	}
	ceiling := max(guarantee, min(roundDownMemory(state.Ceiling), roundDownMemory(state.HostTotal-reserve)))

	var adjustable, silent []string
	var pinned, sumLimits int64
	var grow, shrink []memoryChange
	for id, entry := range state.Instances {
		switch {
		case entry.Status == "Running" && !in.fixed[id] && entry.GuestTotal > 0 && !entry.GuestSilent:
			adjustable = append(adjustable, id)
			sumLimits += entry.Limit
		case entry.Status == "Running" || entry.Status == "Frozen":
			// A frozen VM keeps its memory until the kernel swaps it out; a VM
			// that did not answer keeps its size: a failed sample means hold.
			pinned += entry.Limit
			if previous, seen := held[id]; seen && entry.Status == "Running" {
				result.held[id] = previous
			}
			if entry.Status == "Running" && !in.fixed[id] && in.silent[id] && entry.Limit < ceiling {
				silent = append(silent, id)
			}
		}
	}
	capacity := max(0, state.HostTotal-reserve-pinned)
	result.capacity = capacity
	// Guest page cache fills the limits, so a host whose limits fill its
	// capacity has about its reserve less its own use available: the plan,
	// not pressure. Pressure is tasks waiting for memory, less than hostKeep
	// available (limits above the capacity being filled), or less than the
	// reserve while the limits leave more unallocated than is available (the
	// host's own use is above its reserve). Below twice the reserve the host
	// takes back memory above the demands early (reclaim), which never holds
	// growth back.
	keep := hostKeep(reserve)
	pressure := state.HostStall >= 10 || state.HostAvailable < keep || state.HostAvailable < reserve && state.HostAvailable < capacity-sumLimits
	reclaim := pressure || state.HostAvailable < 2*reserve
	// Below the reserve, memory above the demands comes back a larger step
	// per round; it is never below a fair allocation, so this cannot undo
	// growth.
	severe := state.HostAvailable < reserve || state.HostStall >= 10
	result.pressure = pressure
	step := int64(memoryShrinkStep)
	if severe {
		step = memoryPressureStep
	}
	for _, id := range silent {
		entry := state.Instances[id]
		to := ceiling
		if pressure {
			to = min(ceiling, roundUpMemory(entry.Limit+memoryShrinkStep))
		}
		grow = append(grow, memoryChange{id: id, from: entry.Limit, to: to, reason: "guest did not answer"})
		result.report[id] = vmAllocation{Limit: entry.Limit, Target: to, Reason: "guest did not answer"}
	}
	slices.SortFunc(grow, func(a, b memoryChange) int { return strings.Compare(a.id, b.id) })
	slices.Sort(adjustable)
	if len(adjustable) == 0 {
		result.changes = grow
		return result
	}

	working := make(map[string]int64, len(adjustable))
	short := make(map[string]bool, len(adjustable))
	demand := make(map[string]int64, len(adjustable))
	heldDemand := make(map[string]int64, len(adjustable))
	gUnits, dUnits := make([]int64, len(adjustable)), make([]int64, len(adjustable))
	for i, id := range adjustable {
		entry := state.Instances[id]
		// Kernel memory that MemTotal leaves out still comes from limits.memory.
		kernel := min(max(entry.Limit-entry.GuestTotal, 0), 1<<30)
		w := entry.GuestTotal - entry.GuestAvailable + kernel
		headroom := max(int64(memoryHeadroom), w/4)
		d := w + headroom
		// Short: tasks wait for memory, pages come back from swap, or less is
		// available than the least a shrink leaves (so a shrink into the
		// headroom under host pressure never reads as a shortage).
		// A nested guest under pressure that its machine cannot back is a
		// shortage too; a nested guest that only lacks headroom is not.
		if entry.GuestStall >= memoryShortStall || in.swapIn[id] || entry.GuestAvailable < memoryMinHeadroom/2 || entry.NestedPressure && entry.NestedShort > 0 {
			short[id] = true
			d = max(d, entry.Limit+max(int64(memoryShrinkStep), entry.Limit/4))
		}
		d += max(entry.NestedShort, 0)
		d = min(ceiling, max(guarantee, roundUpMemory(d)))
		previous, seen := held[id]
		next := demandState{held: d, at: in.now}
		switch {
		case !seen || d >= previous.held:
		case in.now.Sub(previous.at) < memoryDemandHold:
			next = previous
		default:
			// Past the hold the demand falls one step per round.
			next = demandState{held: max(d, previous.held-memoryShrinkStep), at: previous.at}
		}
		next.held = min(ceiling, max(guarantee, next.held))
		result.held[id] = next
		working[id], demand[id], heldDemand[id] = w, d, next.held
		gUnits[i], dUnits[i] = unitsUp(guarantee), unitsUp(next.held)
	}
	fairUnits := waterFill(units(capacity), gUnits, dUnits)
	result.overcommitted = int64(len(adjustable))*unitsUp(guarantee) > units(capacity)
	fair := make(map[string]int64, len(adjustable))
	limits := make(map[string]int64, len(adjustable))
	gap := make(map[string]int64, len(adjustable))
	var growth int64
	anyShort := false
	for i, id := range adjustable {
		entry := state.Instances[id]
		fair[id] = bytesOf(fairUnits[i])
		limits[id] = entry.Limit
		if fair[id] < heldDemand[id] {
			anyShort = true
		}
		// While the host is in pressure only a guest that is short itself
		// grows; one that merely lacks its headroom waits.
		if entry.Limit < fair[id] && (!pressure || short[id]) {
			gap[id] = fair[id] - entry.Limit
			growth += gap[id]
		}
		result.report[id] = vmAllocation{Working: working[id], Demand: demand[id], Held: heldDemand[id], Fair: fair[id], Limit: entry.Limit, Target: entry.Limit}
		if gap[id] == 0 && entry.Limit < fair[id] {
			result.backlog += fair[id] - entry.Limit
		}
	}
	// Growth is at once within what the host has free above hostKeep, short
	// guests first; the rest of a guest's way to its fair allocation follows
	// as memory taken back from others is freed, in the next rounds.
	free := units(max(0, state.HostAvailable-keep))
	for _, shortTier := range []bool{true, false} {
		var ids []string
		var gaps []int64
		for _, id := range adjustable {
			if gap[id] > 0 && short[id] == shortTier {
				ids = append(ids, id)
				gaps = append(gaps, unitsUp(gap[id]))
			}
		}
		if len(ids) == 0 {
			continue
		}
		var wanted int64
		for _, n := range gaps {
			wanted += n
		}
		granted := waterFill(min(free, wanted), make([]int64, len(ids)), gaps)
		for i, id := range ids {
			free -= granted[i]
			entry := state.Instances[id]
			to := min(fair[id], entry.Limit+bytesOf(granted[i]))
			result.backlog += fair[id] - to
			if to <= entry.Limit {
				continue
			}
			reason := "guest needs memory"
			if entry.NestedShort > 0 {
				reason = "nested guest needs memory"
			}
			if fair[id] < heldDemand[id] {
				reason = "fair share of a short host"
			}
			grow = append(grow, memoryChange{id: id, from: entry.Limit, to: to, reason: reason})
			report := result.report[id]
			report.Target, report.Reason = to, reason
			result.report[id] = report
		}
	}

	// What each shrinkable VM may give, by reason, in units.
	var idle, inUse []string
	for _, id := range adjustable {
		entry := state.Instances[id]
		if entry.Hotplugged || in.holdShrink[id] || entry.Limit <= fair[id] {
			continue
		}
		if entry.Active {
			inUse = append(inUse, id)
		} else {
			idle = append(idle, id)
		}
	}
	stepLimit := make(map[string]int64)
	for _, id := range append(slices.Clone(idle), inUse...) {
		stepLimit[id] = units(step)
	}
	amount := make(map[string]int64)
	reason := make(map[string]string)
	note := func(taken map[string]int64, why string) {
		for id, n := range taken {
			if n > amount[id] {
				amount[id], reason[id] = n, why
			}
		}
	}
	floorOf := func(id string, base int64) int64 {
		return max(base, guarantee, roundUpMemory(working[id]+memoryMinHeadroom))
	}
	slackAbove := func(ids []string, base func(string) int64) map[string]int64 {
		slack := make(map[string]int64)
		for _, id := range ids {
			slack[id] = units(limits[id] - floorOf(id, base(id)))
		}
		return slack
	}
	// takeByPriority takes need from the tiers in order. A later tier gives
	// only what the earlier tiers cannot cover with all their slack, so the
	// per-round step limit never moves a shrink onto a VM of lower priority.
	takeByPriority := func(need int64, tiers [][]string, floor []func(string) int64, why string) {
		used := make(map[string]int64)
		for index, tier := range tiers {
			if need <= 0 {
				break
			}
			slack := slackAbove(tier, floor[index])
			var total int64
			limit := make(map[string]int64)
			for _, id := range tier {
				slack[id] = max(0, slack[id]-used[id])
				limit[id] = stepLimit[id] - used[id]
				total += slack[id]
			}
			taken, _ := takeProportional(need, tier, slack, limit)
			for id, n := range taken {
				used[id] += n
			}
			need -= total
		}
		note(used, why)
	}
	// Every floor is at least the fair allocation: a VM above it is never
	// taken below it, so nothing taken is grown back in the next round.
	aboveDemand := func(id string) int64 { return max(demand[id], fair[id]) }
	aboveFair := func(id string) int64 { return fair[id] }
	// Host memory first, so that its reason wins a tie: early, only what lies
	// above the demands; in pressure also the headroom above the working sets.
	if reclaim {
		need := max(2*reserve-state.HostAvailable, step)
		tiers, floors := [][]string{idle, inUse}, []func(string) int64{aboveDemand, aboveDemand}
		if pressure {
			need = max(need, max(capacity-sumLimits, keep)-state.HostAvailable)
			tiers, floors = append(tiers, idle, inUse), append(floors, aboveFair, aboveFair)
		}
		takeByPriority(unitsUp(need), tiers, floors, "host memory is low")
	}
	// Capacity needed: others grow, or are held below their demand, while the
	// limits exceed what the host can give. Without either nothing is taken:
	// a limit above the capacity is not use.
	if need := sumLimits + growth - capacity; need > 0 && (growth > 0 || anyShort) {
		takeByPriority(unitsUp(need), [][]string{idle, inUse}, []func(string) int64{aboveFair, aboveFair}, "others need memory")
	}
	// Idle return: an idle VM gives back what its held demand no longer needs.
	for _, id := range idle {
		if excess := limits[id] - max(heldDemand[id], fair[id], guarantee); excess >= memoryShrinkStep {
			note(map[string]int64{id: min(units(excess), units(step))}, "idle memory returned")
		}
	}
	for _, id := range append(idle, inUse...) {
		n := min(amount[id], stepLimit[id])
		if n <= 0 {
			continue
		}
		to := limits[id] - bytesOf(n)
		to = max(to, floorOf(id, fair[id]))
		if to >= limits[id] {
			continue
		}
		shrink = append(shrink, memoryChange{id: id, from: limits[id], to: to, reason: reason[id]})
		report := result.report[id]
		report.Target, report.Reason = to, reason[id]
		result.report[id] = report
	}
	slices.SortFunc(shrink, func(a, b memoryChange) int { return strings.Compare(a.id, b.id) })
	slices.SortStableFunc(grow, func(a, b memoryChange) int { return strings.Compare(a.id, b.id) })
	result.changes = append(grow, shrink...)
	return result
}
