package tenant

import (
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// A replay simulator that drives the three levels of the resource design
// together (internal/cage/resource-allocation.md): the gateway's allocation
// (A, every 15 s) over workspace machines, each machine's host agent rule (B,
// every 2 s) over its nested guest, and inside the guest a model of the
// cgroup classes (C). One step is one second.
//
// The kernel is a model, not a kernel: anonymous memory of each class, page
// cache that fills free memory and counts as available, zram that holds a
// third of what it stores, memory.high pushing a class's excess into zram
// (and stalling that class), memory.max killing the class's largest process,
// memory.low protecting the control plane and the sessions from reclaim. A
// probe (the host agent's sample, the control plane's monitoring) answers
// when the protected classes do not stall and at least 128 MiB is
// available; with the guest out of memory it times out, as in the evidence.
// The machine's working set is its own use plus the nested guest's resident
// memory (QEMU memory is anonymous to the machine), with free page
// reporting returning the guest's free pages.

const simMiB = uint64(1 << 20)
const simGiB = uint64(1 << 30)

// simClasses is what each class of a guest wants at a second.
type simClasses struct{ control, sessions, browser, cache uint64 }

type simGuest struct {
	policy   *cage.GuestPolicy
	maximum  uint64
	target   uint64
	classesC bool // level C: priority classes in the guest
	limits   cage.ClassLimits
	zram     uint64 // disksize
	swapped  uint64 // bytes stored in zram
	cache    uint64
	want     simClasses
	// the browser's runaway renderer is killed at memory.max: what remains
	browserFloor uint64
	browserKill  int
	sessionKill  int
	// observations of the last second
	available, free              uint64
	protectedStall, browserStall float64
	globalStall, controlStall    float64
	answers                      bool
	forceSilent                  bool
	resident                     uint64
}

// step settles the guest's memory for one second.
func (g *simGuest) step() {
	const kernelFree = 64 * simMiB
	c, s, b := g.want.control, g.want.sessions, max(g.want.browser, g.browserFloor)
	if g.browserFloor > 0 && g.want.browser > g.browserFloor {
		b = g.browserFloor
	}
	// MemTotal is a little below the balloon target (the evidence guest:
	// 2.84 GiB of 3), and the kernel keeps some memory free.
	capacity := g.target - min(g.target, kernelFree+max(64*simMiB, g.target/20))
	var swapB, swapS uint64
	g.protectedStall, g.browserStall, g.globalStall, g.controlStall = 0, 0, 0, 0
	if g.classesC && g.limits.BrowserHigh > 0 {
		if b > g.limits.BrowserMax {
			// memory.max: the cgroup's OOM killer takes its largest process,
			// the runaway renderer; the rest of the browser stays.
			g.browserKill++
			g.browserFloor = 200 * simMiB
			b = g.browserFloor
		}
		if b > g.limits.BrowserHigh {
			swapB = b - g.limits.BrowserHigh
			g.browserStall = min(60, 10+50*float64(swapB)/float64(g.limits.BrowserHigh))
		}
	}
	resident := c + (s - swapS) + (b - swapB)
	used := resident + (swapB+swapS)/3
	if used > capacity {
		// Global reclaim: unprotected memory first (the browser, then the
		// sessions above their protection) with classes; everything alike
		// without.
		deficit := used - capacity
		// Moving x bytes into zram frees two thirds of x.
		take := func(from *uint64, limit uint64) {
			need := (deficit*3 + 1) / 2
			moved := min(need, limit)
			*from += moved
			if moved == need {
				deficit = 0
			} else {
				deficit -= min(deficit, moved*2/3)
			}
		}
		if g.classesC {
			take(&swapB, (b-swapB)*3/4)
			if deficit > 0 {
				take(&swapS, (s-swapS)-min(s-swapS, g.limits.SessionsLow))
				g.protectedStall = max(g.protectedStall, 15)
			}
			if deficit > 0 {
				// Nothing unprotected is left: the control plane waits too.
				g.protectedStall, g.controlStall = 50, 50
			}
			g.browserStall = max(g.browserStall, 20)
		} else {
			moved := min(deficit*3/2, (s+b)*3/4)
			swapS, swapB = moved*s/max(s+b, 1), moved*b/max(s+b, 1)
			deficit -= min(deficit, moved*2/3)
			g.globalStall = 3
			if deficit > 0 {
				g.globalStall = 30
			}
		}
		resident = c + (s - swapS) + (b - swapB)
		used = resident + (swapB+swapS)/3
	}
	g.swapped = swapB + swapS
	if g.swapped > g.zram {
		// zram is full: what does not fit stays resident and the guest is out
		// of memory. With classes the OOM order kills a browser process
		// first; without, the evidence shows a thrashing guest without kills.
		over := g.swapped - g.zram
		if g.classesC {
			g.browserKill++
			g.browserFloor = 200 * simMiB
		} else {
			used += over * 2 / 3
			g.globalStall, g.controlStall = 30, 30
		}
		g.swapped = g.zram
	}
	available := uint64(48 * simMiB)
	if capacity > used {
		available = max(available, capacity-used)
	}
	g.cache = min(g.want.cache, available)
	g.available = available
	g.free = available - g.cache
	if !g.classesC {
		g.protectedStall, g.controlStall = g.globalStall, g.globalStall
	}
	// The probes and the monitoring run in the control class. Without
	// classes a probe needs free memory: with zram full and 48 MB left it
	// times out (the evidence). With classes the control plane's pages are
	// protected and its new allocations reclaim from the unprotected
	// classes, so it answers unless nothing unprotected is left.
	if g.classesC {
		// The probes arrive as SSH sessions of the sessions' user: they share
		// the sessions' cgroup (user.slice) and its stall.
		g.answers = !g.forceSilent && g.controlStall < 10 && g.protectedStall < 10
	} else {
		g.answers = !g.forceSilent && g.controlStall < 10 && g.available >= 128*simMiB
	}
	g.resident = min(g.target, used+g.cache+kernelFree)
}

func (g *simGuest) sample() *cage.ResourceSample {
	if !g.answers {
		return nil
	}
	s := &cage.ResourceSample{Total: g.target, Available: g.available, Free: g.free, SwapTotal: g.zram, SwapFree: g.zram - g.swapped,
		Stall: max(g.protectedStall, g.browserStall, g.globalStall), Balloon: g.maximum - g.target, Boot: strings.Repeat("b", 36)}
	if g.classesC {
		s.Protected, s.ProtectedStall, s.BrowserStall = true, g.protectedStall, g.browserStall
	}
	return s
}

type simMachine struct {
	id             string
	limit          int64  // limits.memory set by the gateway
	own            uint64 // the machine's own processes (cage serve, host agent, QEMU)
	guest          *simGuest
	short          uint64 // published in machine-memory.json
	nestedPressure bool
	active         bool
	swaps          int // seconds the machine itself was out of memory
	// cache is the machine's own page cache (the nested guest's disk image,
	// logs): it fills free memory, counts as available inside the machine
	// and is resident on the gateway host, which cannot reclaim it.
	cache uint64
}

func (m *simMachine) memTotal() uint64 { return uint64(m.limit) - 200*simMiB }
func (m *simMachine) used() uint64     { return m.own + m.guest.resident }
func (m *simMachine) available() uint64 {
	if total, used := m.memTotal(), m.used(); total > used {
		return total - used
	}
	return 0
}

// settleCache grows the machine's page cache by rate toward its free memory
// and drops what no longer fits.
func (m *simMachine) settleCache(rate uint64) {
	m.cache = min(m.cache+rate, m.available())
}

type simWorld struct {
	host, hostOwn uint64
	ceiling       int64
	machines      []*simMachine
	held          map[string]demandState
	now           time.Time
	second        int
	// per machine: seconds the control plane did not answer, minimum
	// available, the last target
	silent map[string]int
	// cacheRate is how fast each machine's own page cache grows (bytes per
	// second); zero leaves machines without page cache.
	cacheRate uint64
	// pressured counts gateway rounds that saw host pressure.
	pressured int
}

func newSimGuest(maximum uint64, classes bool, want simClasses) *simGuest {
	zram := min(maximum/4, 2*simGiB)
	if classes {
		zram = min(maximum/2, 4*simGiB)
	}
	g := &simGuest{policy: cage.NewGuestPolicy(maximum), maximum: maximum, target: maximum, classesC: classes, zram: zram, want: want}
	g.step()
	return g
}

func (w *simWorld) run(seconds int, trace func(second int, m *simMachine)) {
	if w.held == nil {
		w.held, w.silent = map[string]demandState{}, map[string]int{}
	}
	for range seconds {
		for _, m := range w.machines {
			trace(w.second, m)
			m.guest.step()
			m.settleCache(w.cacheRate)
			if !m.guest.answers && w.second >= 30 {
				w.silent[m.id]++
			}
			if m.used() > m.memTotal() {
				m.swaps++
			}
			if w.second%2 == 0 {
				g := m.guest
				machine := cage.MachineMemory{Total: m.memTotal(), Boot: uint64(w.ceiling), Available: m.available(), Measured: true, Role: cage.RoleDedicated,
					Pressure: m.available() < m.memTotal()/10}
				decision := g.policy.Decide(w.now, g.sample(), false, false, machine)
				g.target = decision.Target
				g.policy.Applied(decision.Target)
				m.short, m.nestedPressure = decision.ShortBytes, decision.Pressure || g.sample() == nil
				if g.classesC && g.answers {
					g.limits = cage.PlanGuestClasses(min(g.maximum, max(decision.Capacity, decision.Demand)),
						map[string]cage.GuestClass{"control": {Current: g.want.control}, "sessions": {Current: min(g.want.sessions, max(g.limits.SessionsHigh, g.want.sessions))}})
				}
			}
		}
		if w.second%15 == 0 {
			w.allocate()
		}
		w.second++
		w.now = w.now.Add(time.Second)
	}
}

func (w *simWorld) allocate() {
	var resident uint64
	state := MemoryState{HostTotal: int64(w.host), Ceiling: w.ceiling, Instances: map[string]InstanceMemory{}}
	for _, m := range w.machines {
		resident += min(uint64(m.limit), m.used()+m.cache+200*simMiB)
		state.Instances[m.id] = InstanceMemory{Status: "Running", Limit: m.limit, GuestTotal: int64(m.memTotal()), GuestAvailable: int64(m.available()),
			NestedShort: int64(m.short), NestedPressure: m.nestedPressure, Active: m.active}
	}
	state.HostAvailable = int64(w.host - min(w.host, w.hostOwn+resident))
	result := allocateMemory(allocationInput{now: w.now, state: state, minimum: DefaultMemoryMin}, w.held)
	w.held = result.held
	if result.pressure {
		w.pressured++
	}
	for _, change := range result.changes {
		for _, m := range w.machines {
			if m.id == change.id {
				m.limit = change.to
				// A smaller balloon target makes the machine drop cache first.
				m.settleCache(0)
			}
		}
	}
}

// outage is the evidence: two claude sessions 513 MB, one tab growing to
// 1.7 GB (3.5 hours compressed into 30 minutes), and the rest of the 190
// processes, each under 40 MB (system, display, editor, browser agent, SSH
// sessions), about 900 MB: with zram full the 2.84 GiB guest had 48 MB left.
func outage(second int, m *simMachine) {
	m.guest.want = simClasses{control: 900 * simMiB, sessions: 513 * simMiB, browser: 150*simMiB + min(uint64(second), 1800)*1550*simMiB/1800, cache: 600 * simMiB}
}

func singleMachine(classes bool, maximum uint64, trace func(int, *simMachine)) (*simWorld, *simMachine) {
	m := &simMachine{id: vmID(1), limit: 8 << 30, own: simGiB, active: true}
	world := &simWorld{host: 64 * simGiB, hostOwn: 2 * simGiB, ceiling: 8 << 30, machines: []*simMachine{m}, now: time.Unix(1e9, 0)}
	m.guest = newSimGuest(maximum, classes, m.guest0())
	return world, m
}

func (m *simMachine) guest0() simClasses {
	return simClasses{control: 400 * simMiB, sessions: 300 * simMiB, browser: 150 * simMiB}
}

// newSizing is the automatic maximum of the evidence machine (8 GiB, which
// host.MemGiB reports as 7) after this change, and oldSizing before it.
const newSizing, oldSizing = 6 * simGiB, 3 * simGiB

func report(t *testing.T, name string, w *simWorld, m *simMachine) {
	g := m.guest
	t.Logf("%-28s maximum %4d MiB  target %4d MiB  machine %5d MiB (available %5d)  control silent %4d s  browser kills %d  session kills %d  machine out of memory %d s",
		name, g.maximum>>20, g.target>>20, m.limit>>20, m.available()>>20, w.silent[m.id], g.browserKill, g.sessionKill, m.swaps)
}

func TestReplayOutageBeforeAndAfter(t *testing.T) {
	before, bm := singleMachine(false, oldSizing, outage)
	before.run(2400, outage)
	report(t, "outage, old sizing", before, bm)
	after, am := singleMachine(true, newSizing, outage)
	after.run(2400, outage)
	report(t, "outage, new allocation", after, am)
	// The new sizing alone, on a guest without the governor (components not
	// yet updated): the tab fits in the larger guest.
	sizingOnly, sm := singleMachine(false, newSizing, outage)
	sizingOnly.run(2400, outage)
	report(t, "outage, new size, no classes", sizingOnly, sm)
	if sizingOnly.silent[sm.id] != 0 || sm.swaps != 0 {
		t.Fatalf("sizing alone: silent %d s, machine out of memory %d s", sizingOnly.silent[sm.id], sm.swaps)
	}
	// Priorities alone, with the old 3 GiB ceiling (an explicit manual
	// setting): the tab is reclaimed and throttled, the control plane answers.
	classesOnly, cm := singleMachine(true, oldSizing, outage)
	classesOnly.run(2400, outage)
	report(t, "outage, old size + classes", classesOnly, cm)
	if classesOnly.silent[cm.id] != 0 || cm.guest.sessionKill != 0 {
		t.Fatalf("classes alone: silent %d s, session kills %d", classesOnly.silent[cm.id], cm.guest.sessionKill)
	}
	// The model reproduces the outage: a 3 GiB guest without priorities stops
	// answering once the tab is large, although its machine has room.
	if before.silent[bm.id] < 60 || bm.available() < 2*simGiB {
		t.Fatalf("the old policy did not reproduce the outage: silent %d s, machine available %d MiB", before.silent[bm.id], bm.available()>>20)
	}
	// With the new allocation the control plane answers throughout, the
	// sessions are untouched, and the guest uses the machine's idle memory.
	if after.silent[am.id] != 0 || am.guest.sessionKill != 0 || am.guest.target <= oldSizing || am.swaps != 0 {
		t.Fatalf("new allocation: silent %d s, session kills %d, target %d MiB, machine out of memory %d s", after.silent[am.id], am.guest.sessionKill, am.guest.target>>20, am.swaps)
	}
}

func TestReplayRunawayTabIsContained(t *testing.T) {
	runaway := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 513 * simMiB, browser: 150*simMiB + uint64(second)*8*simMiB, cache: 400 * simMiB}
	}
	w, m := singleMachine(true, newSizing, runaway)
	w.run(1800, runaway)
	report(t, "runaway tab, new allocation", w, m)
	if m.guest.browserKill == 0 || m.guest.sessionKill != 0 || w.silent[m.id] != 0 || m.swaps != 0 {
		t.Fatalf("runaway tab: browser kills %d, session kills %d, silent %d s, machine out of memory %d s", m.guest.browserKill, m.guest.sessionKill, w.silent[m.id], m.swaps)
	}
}

func TestReplaySessionRunawayAtTheMaximum(t *testing.T) {
	// A session grows to 5.9 GiB in a guest whose maximum is 6 GiB. The host
	// agent gives the guest its maximum and the machine stays safe; the
	// browser is reclaimed first. The probes are SSH sessions of the same
	// user as the sessions and share their cgroup, so once the guest is out
	// of memory they stop answering too: a known limit (resource-allocation.md
	// section 7), reported, not asserted away.
	build := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 500*simMiB + min(uint64(second), 900)*6*simMiB, browser: 900 * simMiB, cache: 400 * simMiB}
	}
	w, m := singleMachine(true, newSizing, build)
	firstSilent := -1
	for range 1500 {
		w.run(1, build)
		if firstSilent < 0 && w.silent[m.id] > 0 {
			firstSilent = w.second
		}
	}
	report(t, "session runaway, new", w, m)
	t.Logf("probes first silent at %d s, when the sessions wanted %d MiB in a %d MiB guest", firstSilent, (500*simMiB+min(uint64(firstSilent), 900)*6*simMiB)>>20, m.guest.maximum>>20)
	if m.swaps != 0 || m.guest.target != m.guest.maximum || m.guest.sessionKill != 0 || firstSilent >= 0 && 500*simMiB+uint64(firstSilent)*6*simMiB < m.guest.maximum-1536*simMiB {
		t.Fatalf("session runaway: machine out of memory %d s, target %d MiB, first silent at %d s", m.swaps, m.guest.target>>20, firstSilent)
	}
}

func TestReplaySmallMachine(t *testing.T) {
	// A 4 GiB machine reported as 3 GiB: the automatic maximum is 2 GiB.
	small := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 350 * simMiB, sessions: 400 * simMiB, browser: 150*simMiB + min(uint64(second), 600)*2*simMiB, cache: 300 * simMiB}
	}
	m := &simMachine{id: vmID(2), limit: 4 << 30, own: 768 * simMiB, active: true}
	w := &simWorld{host: 16 * simGiB, hostOwn: 2 * simGiB, ceiling: 4 << 30, machines: []*simMachine{m}, now: time.Unix(1e9, 0)}
	m.guest = newSimGuest(2*simGiB, true, simClasses{control: 350 * simMiB, sessions: 400 * simMiB, browser: 150 * simMiB})
	w.run(1200, small)
	report(t, "4 GiB machine, new", w, m)
	if w.silent[m.id] != 0 || m.swaps != 0 || m.guest.sessionKill != 0 {
		t.Fatalf("small machine: silent %d s, machine out of memory %d s", w.silent[m.id], m.swaps)
	}
}

func TestReplayTwoMachinesBothGrowing(t *testing.T) {
	grow := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 300*simMiB + min(uint64(second), 900)*3*simMiB, browser: 500*simMiB + min(uint64(second), 900)*2*simMiB, cache: 500 * simMiB}
	}
	a := &simMachine{id: vmID(3), limit: 12 << 30, own: simGiB, active: true}
	b := &simMachine{id: vmID(4), limit: 12 << 30, own: simGiB, active: true}
	w := &simWorld{host: 20 * simGiB, hostOwn: 2 * simGiB, ceiling: 12 << 30, machines: []*simMachine{a, b}, now: time.Unix(1e9, 0)}
	for _, m := range w.machines {
		m.guest = newSimGuest(10*simGiB, true, m.guest0())
	}
	w.run(1800, grow)
	report(t, "two growing machines, A", w, a)
	report(t, "two growing machines, B", w, b)
	diff := a.limit - b.limit
	if diff < 0 {
		diff = -diff
	}
	capacity := int64(20*simGiB - uint64(memoryReserve(0, int64(20*simGiB))))
	if w.silent[a.id]+w.silent[b.id] != 0 || a.swaps+b.swaps != 0 || diff > 512<<20 || a.limit+b.limit > capacity+512<<20 {
		t.Fatalf("two machines: limits %d/%d MiB (capacity %d), silent %d/%d, out of memory %d/%d", a.limit>>20, b.limit>>20, capacity>>20, w.silent[a.id], w.silent[b.id], a.swaps, b.swaps)
	}
}

func TestReplayPageCacheHogGainsNothing(t *testing.T) {
	hog := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 300 * simMiB, browser: 300 * simMiB, cache: 20 * simGiB}
	}
	quiet := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 300 * simMiB, browser: 300 * simMiB, cache: 200 * simMiB}
	}
	a := &simMachine{id: vmID(5), limit: 12 << 30, own: simGiB}
	b := &simMachine{id: vmID(6), limit: 12 << 30, own: simGiB}
	w := &simWorld{host: 20 * simGiB, hostOwn: 2 * simGiB, ceiling: 12 << 30, machines: []*simMachine{a, b}, now: time.Unix(1e9, 0)}
	a.guest, b.guest = newSimGuest(10*simGiB, true, a.guest0()), newSimGuest(10*simGiB, true, b.guest0())
	w.run(2400, func(second int, m *simMachine) {
		if m == a {
			hog(second, m)
		} else {
			quiet(second, m)
		}
	})
	report(t, "page cache hog", w, a)
	report(t, "its quiet neighbour", w, b)
	// The hog's guest holds its cache only within its balloon, which follows
	// its working set; its machine ends no larger than the neighbour's by
	// more than the cache headroom the host agent leaves.
	if a.limit > b.limit+2<<30 || a.guest.target > 4*simGiB {
		t.Fatalf("page cache bought memory: machine %d vs %d MiB, guest target %d MiB", a.limit>>20, b.limit>>20, a.guest.target>>20)
	}
}

func TestReplayMachineShrinksUnderAGuest(t *testing.T) {
	// m1's guest is idle with 4 GiB of cache; m2's guest grows. The gateway
	// takes m1's idle memory; m1's host agent gives back its guest's cache
	// before m1 runs out.
	idle := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 300 * simMiB, browser: 300 * simMiB, cache: 4 * simGiB}
	}
	busy := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 500*simMiB + min(uint64(second), 600)*8*simMiB, browser: 600 * simMiB, cache: 300 * simMiB}
	}
	m1 := &simMachine{id: vmID(7), limit: 10 << 30, own: simGiB}
	m2 := &simMachine{id: vmID(8), limit: 4 << 30, own: simGiB, active: true}
	w := &simWorld{host: 16 * simGiB, hostOwn: 2 * simGiB, ceiling: 10 << 30, machines: []*simMachine{m1, m2}, now: time.Unix(1e9, 0)}
	m1.guest = newSimGuest(8*simGiB, true, m1.guest0())
	m2.guest = newSimGuest(8*simGiB, true, m2.guest0())
	m2.guest.target = 3 * simGiB
	m2.guest.policy.Applied(3 * simGiB)
	w.run(1800, func(second int, m *simMachine) {
		if m == m1 {
			idle(second, m)
		} else {
			busy(second, m)
		}
	})
	report(t, "shrinking machine", w, m1)
	report(t, "growing neighbour", w, m2)
	if m1.swaps != 0 || m2.swaps != 0 || w.silent[m1.id]+w.silent[m2.id] != 0 || m1.limit >= 10<<30 || m2.guest.target < 5*simGiB {
		t.Fatalf("machine shrink: out of memory %d/%d s, silent %d/%d, m1 %d MiB, m2 guest %d MiB", m1.swaps, m2.swaps, w.silent[m1.id], w.silent[m2.id], m1.limit>>20, m2.guest.target>>20)
	}
}

func TestReplaySampleFailuresHold(t *testing.T) {
	failing := func(second int, m *simMachine) {
		outage(min(second, 600), m)
		m.guest.forceSilent = second >= 900 && second < 960
	}
	w, m := singleMachine(true, newSizing, failing)
	var during []uint64
	for range 1200 {
		w.run(1, failing)
		if w.second > 900 && w.second < 960 {
			during = append(during, m.guest.target)
		}
	}
	report(t, "sample failures", w, m)
	for i := 1; i < len(during); i++ {
		if during[i] < during[i-1] {
			t.Fatalf("target shrank during failed samples: %v", during)
		}
	}
	if m.swaps != 0 || during[len(during)-1] > m.guest.maximum {
		t.Fatalf("failures: machine out of memory %d s, targets %v", m.swaps, during)
	}
}

func TestReplayParkedMachineRegrowsQuickly(t *testing.T) {
	// The gateway parked an idle machine at its working set plus headroom.
	// Then its guest needs 3 GiB more: the host agent tells the gateway what
	// the machine cannot back, and the guest has it within two gateway rounds.
	// A parked machine absorbs about its headroom (1 GiB) at once; the rest
	// waits for gateway rounds (15 s each), so a burst faster than that leaves
	// the guest out of memory, and its probes silent, for up to two rounds
	// (logged; an open decision in resource-allocation.md section 7).
	idle := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 300 * simMiB, browser: 300 * simMiB, cache: 200 * simMiB}
	}
	for _, ramp := range []int{30, 1} {
		m := &simMachine{id: vmID(9), limit: 12 << 30, own: simGiB}
		w := &simWorld{host: 32 * simGiB, hostOwn: 2 * simGiB, ceiling: 12 << 30, machines: []*simMachine{m}, now: time.Unix(1e9, 0)}
		m.guest = newSimGuest(10*simGiB, true, m.guest0())
		w.run(1200, idle)
		parked := m.limit
		if parked > 6<<30 {
			t.Fatalf("idle machine not parked: %d MiB", parked>>20)
		}
		m.active = true
		// The worst case: the demand appears just after a gateway round.
		for w.second%15 != 1 {
			w.run(1, idle)
		}
		start := w.second
		busy := func(second int, m *simMachine) {
			grown := min(uint64(second-start+1), uint64(ramp)) * 3 * simGiB / uint64(ramp)
			m.guest.want = simClasses{control: 400 * simMiB, sessions: 300*simMiB + grown, browser: 300 * simMiB, cache: 200 * simMiB}
		}
		silent := w.silent[m.id]
		for w.second-start < 120 {
			w.run(1, busy)
			if w.second-start >= ramp && m.guest.target >= 3300*simMiB+400*simMiB+300*simMiB+simGiB && m.guest.answers {
				break
			}
		}
		elapsed := w.second - start
		t.Logf("3 GiB more over %2d s on a machine parked at %d MiB: the guest has it after %d s (machine %d MiB, guest %d MiB), control plane silent %d s, machine out of memory %d s",
			ramp, parked>>20, elapsed, m.limit>>20, m.guest.target>>20, w.silent[m.id]-silent, m.swaps)
		if elapsed > 40 || m.swaps != 0 || w.silent[m.id]-silent > 40 || ramp > 1 && w.silent[m.id]-silent > 10 {
			t.Fatalf("regrowth over %d s took %d s (machine out of memory %d s, silent %d s)", ramp, elapsed, m.swaps, w.silent[m.id]-silent)
		}
	}
}

func TestReplayTwoMachinesGrowingWithPageCache(t *testing.T) {
	// TestReplayTwoMachinesBothGrowing on machines whose own page cache
	// fills their free memory (review 2, R2-5): the host's available memory
	// then stays near its reserve, which is the plan, not a shortage. Both
	// guests still get what they need, at once, and the gateway does not sit
	// in pressure.
	grow := func(second int, m *simMachine) {
		m.guest.want = simClasses{control: 400 * simMiB, sessions: 300*simMiB + min(uint64(second), 900)*3*simMiB, browser: 500*simMiB + min(uint64(second), 900)*2*simMiB, cache: 500 * simMiB}
	}
	a := &simMachine{id: vmID(3), limit: 12 << 30, own: simGiB, active: true}
	b := &simMachine{id: vmID(4), limit: 12 << 30, own: simGiB, active: true}
	w := &simWorld{host: 20 * simGiB, hostOwn: 2 * simGiB, ceiling: 12 << 30, machines: []*simMachine{a, b}, now: time.Unix(1e9, 0), cacheRate: 64 * simMiB}
	for _, m := range w.machines {
		m.guest = newSimGuest(10*simGiB, true, m.guest0())
	}
	w.run(1800, grow)
	report(t, "two growing machines with cache, A", w, a)
	report(t, "two growing machines with cache, B", w, b)
	t.Logf("rounds under host pressure: %d of %d", w.pressured, 1800/15)
	if w.silent[a.id]+w.silent[b.id] != 0 || a.swaps+b.swaps != 0 || a.guest.target < 3*simGiB || b.guest.target < 3*simGiB || w.pressured > 1800/15/10 {
		t.Fatalf("two machines with cache: limits %d/%d MiB, targets %d/%d MiB, silent %d/%d, out of memory %d/%d, pressure in %d rounds",
			a.limit>>20, b.limit>>20, a.guest.target>>20, b.guest.target>>20, w.silent[a.id], w.silent[b.id], a.swaps, b.swaps, w.pressured)
	}
}

// cacheHost is the reviewer's model of a gateway host whose VMs run no nested
// guest (review 2, R2-5): a guest's working set is anonymous memory, its page
// cache grows toward the balloon limit (Linux keeps file pages while memory is
// free) and counts as available inside the guest, and everything the guest
// touched stays resident on the host, which cannot reclaim guest memory: only
// a smaller balloon limit makes the guest drop cache.
type cacheHost struct {
	total, own int64
	ceiling    int64
	vms        []*cacheVM
	held       map[string]demandState
	now        time.Time
}

type cacheVM struct {
	id                    string
	limit, working, cache int64
}

const cacheKernel = 200 * testMiB

func (h *cacheHost) state() MemoryState {
	state := MemoryState{HostTotal: h.total, Ceiling: h.ceiling, Instances: map[string]InstanceMemory{}}
	resident := h.own
	for _, vm := range h.vms {
		total := vm.limit - cacheKernel
		vm.cache = max(0, min(vm.cache, total-vm.working))
		resident += vm.working + vm.cache + cacheKernel
		state.Instances[vm.id] = InstanceMemory{Status: "Running", Limit: vm.limit, GuestTotal: total, GuestAvailable: max(0, total-vm.working), Active: true}
	}
	state.HostAvailable = max(0, h.total-resident)
	return state
}

// round is one gateway round followed by 15 s of page cache growth.
func (h *cacheHost) round(t *testing.T, cacheGrowth int64) (MemoryState, allocationResult) {
	t.Helper()
	if h.held == nil {
		h.held = map[string]demandState{}
	}
	state := h.state()
	result := allocateMemory(allocationInput{now: h.now, state: state, minimum: DefaultMemoryMin}, h.held)
	h.held = result.held
	for _, change := range result.changes {
		fair := result.report[change.id].Fair
		if change.to < change.from && change.from > fair && change.to < fair {
			t.Fatalf("%s shrank from above its fair share %d MiB to below it: %+v", change.id, fair>>20, change)
		}
		for _, vm := range h.vms {
			if vm.id == change.id {
				vm.limit = change.to
			}
		}
	}
	for _, vm := range h.vms {
		vm.cache += cacheGrowth
	}
	h.now = h.now.Add(15 * time.Second)
	return state, result
}

// demandOf is the allocator's demand for a guest that is not short.
func demandOf(working int64) int64 {
	w := working + cacheKernel
	return roundUpMemory(w + max(memoryHeadroom, w/4))
}

func TestReplayPageCacheDoesNotHoldGrowthBack(t *testing.T) {
	// The reviewer's scenario: 17 GiB host, three VMs with a 3 GiB working
	// set whose page cache fills their limits. Then vm0's working set grows
	// by 128 MiB a round. While the host can back it, vm0 grows in the round
	// its demand rises, never runs short, and the host is not in pressure.
	h := &cacheHost{total: 17 * testGiB, own: 512 * testMiB, ceiling: 8 * testGiB, now: time.Unix(1e9, 0)}
	for i := range 3 {
		h.vms = append(h.vms, &cacheVM{id: vmID(20 + i), limit: 8 * testGiB, working: 3 * testGiB})
	}
	vm0 := h.vms[0]
	capacity := h.total - memoryReserve(0, h.total)
	for round := range 60 {
		if round >= 14 && vm0.working < 4608*testMiB {
			vm0.working += 128 * testMiB
		}
		state, result := h.round(t, 256*testMiB)
		if round < 10 {
			continue
		}
		var sum, demands int64
		for _, vm := range h.vms {
			sum += vm.limit
			demands += demandOf(vm.working)
		}
		entry := state.Instances[vm0.id]
		if entry.GuestAvailable < memoryMinHeadroom/2 {
			t.Fatalf("round %d: vm0 ran short (%d MiB available, limit %d MiB, host available %d MiB, pressure %v)", round, entry.GuestAvailable>>20, entry.Limit>>20, state.HostAvailable>>20, result.pressure)
		}
		if demands <= capacity {
			for _, vm := range h.vms {
				if vm.limit < demandOf(vm.working) {
					t.Fatalf("round %d: %s has %d MiB for a demand of %d MiB while the host backs every demand (host available %d MiB, pressure %v)",
						round, vm.id, vm.limit>>20, demandOf(vm.working)>>20, state.HostAvailable>>20, result.pressure)
				}
			}
		}
		if result.pressure {
			t.Fatalf("round %d: host pressure with %d MiB available and limits %d of %d MiB", round, state.HostAvailable>>20, sum>>20, capacity>>20)
		}
		if sum > capacity {
			t.Fatalf("round %d: limits %d MiB exceed the capacity %d MiB", round, sum>>20, capacity>>20)
		}
	}
}

func TestReplayLongSteadyStateWithPageCache(t *testing.T) {
	// Eight hours of constant working sets with page cache filling every
	// limit, in the middle band (every demand met, limits below the
	// capacity) and saturated (one guest wants more than its fair share, so
	// the limits fill the capacity). After it settles nothing moves, the
	// host is never in pressure, and every guest has its demand or its fair
	// share.
	for _, test := range []struct {
		name    string
		working []int64
	}{
		{"middle band", []int64{3 * testGiB, 3 * testGiB, 3 * testGiB}},
		{"saturated", []int64{3 * testGiB, 3 * testGiB, 5 * testGiB}},
	} {
		h := &cacheHost{total: 17 * testGiB, own: 512 * testMiB, ceiling: 8 * testGiB, now: time.Unix(1e9, 0)}
		for i, working := range test.working {
			h.vms = append(h.vms, &cacheVM{id: vmID(30 + i), limit: 8 * testGiB, working: working})
		}
		capacity := h.total - memoryReserve(0, h.total)
		moves, pressured := 0, 0
		for round := range 8 * 240 {
			state, result := h.round(t, 256*testMiB)
			if round < 60 {
				continue
			}
			moves += len(result.changes)
			if result.pressure {
				pressured++
			}
			var sum int64
			for _, vm := range h.vms {
				sum += vm.limit
				if entry := state.Instances[vm.id]; entry.GuestAvailable < memoryMinHeadroom/2 && demandOf(vm.working) <= result.report[vm.id].Fair {
					t.Fatalf("%s round %d: %s short below its fair share: %+v", test.name, round, vm.id, result.report[vm.id])
				}
			}
			if sum > capacity || state.HostAvailable <= 0 {
				t.Fatalf("%s round %d: limits %d MiB of %d, host available %d MiB", test.name, round, sum>>20, capacity>>20, state.HostAvailable>>20)
			}
		}
		var limits []int64
		for _, vm := range h.vms {
			limits = append(limits, vm.limit>>20)
		}
		state := h.state()
		t.Logf("%-12s limits %v MiB, host available %d MiB, moves after settling %d, rounds in pressure %d", test.name, limits, state.HostAvailable>>20, moves, pressured)
		if moves != 0 || pressured != 0 {
			t.Fatalf("%s: %d moves and %d rounds in pressure after settling (limits %v MiB)", test.name, moves, pressured, limits)
		}
	}
}
