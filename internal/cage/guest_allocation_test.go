package cage

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func linuxMachine(total, available uint64) MachineMemory {
	return MachineMemory{Total: total, Boot: total, Available: available, Measured: true, Role: RoleDedicated}
}

// guestSample is a guest of total bytes that uses used of it, with free bytes
// never touched.
func guestSample(total, used, free uint64) ResourceSample {
	return ResourceSample{Total: total, Available: total - used, Free: free, Boot: strings.Repeat("b", 36)}
}

func TestGuestPlanFollowsTheMachine(t *testing.T) {
	now := time.Unix(1e9, 0)
	p := memoryPolicy{maximum: 6 * gib, target: 3 * gib}
	// An 8 GiB machine with 3 GiB available; the guest holds 2 GiB of its 3.
	plan := p.plan(guestInput{now: now, ok: true, sample: guestSample(3*gib, 2*gib+512*mib, gib), machine: linuxMachine(8*gib, 3*gib)})
	if plan.capacity != 4864*mib || plan.reason != "guest_needs" || plan.target != 3840*mib {
		t.Fatalf("capacity plan %+v", plan)
	}
	// Under pressure it grows into what the machine can back, not beyond.
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(3*gib, 2900*mib, 64*mib), machine: linuxMachine(8*gib, 3*gib)})
	if plan.reason != "guest_severe" || plan.target != plan.capacity || plan.capacity >= 6*gib || plan.shortBytes != 0 {
		t.Fatalf("pressure within capacity %+v", plan)
	}
	// The gateway shrinks the machine: 400 MiB left. The guest gives back
	// what it does not need at once, never below its demand.
	// The gateway shrinks the machine until nothing is available: the guest
	// (3 GiB used, 2 GiB of cache) gives back what the machine cannot back,
	// at once, and never goes below its demand (4.25 GiB).
	p = memoryPolicy{maximum: 6 * gib, target: 5 * gib}
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(5*gib, 3*gib, 0), machine: linuxMachine(6*gib, 0)})
	if plan.reason != "machine_smaller" || plan.target != 4864*mib {
		t.Fatalf("machine shrink plan %+v", plan)
	}
	// A machine that cannot back the demand: the guest goes down to what the
	// machine backs, never below its working set plus a cushion (3.25 GiB),
	// and the outer level is told what is missing.
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(5*gib, 3*gib, 1536*mib), machine: linuxMachine(6*gib, 0)})
	if plan.reason != "machine_smaller" || plan.target != 3328*mib || plan.demand != 4352*mib || plan.shortBytes != plan.demand-plan.capacity {
		t.Fatalf("machine shrink stops at the working set %+v", plan)
	}
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(5*gib, 3*gib, 4*gib), machine: linuxMachine(6*gib, 0)})
	if plan.reason != "machine_smaller" || plan.target != 3328*mib || plan.capacity >= plan.target {
		t.Fatalf("machine shrink below the working set %+v", plan)
	}
	// A machine that cannot back the demand: the guest keeps what it has,
	// does not grow into the machine's swap, and the gateway is told.
	p = memoryPolicy{maximum: 6 * gib, target: 4 * gib}
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(4*gib, 3*gib, 0), machine: linuxMachine(6*gib, 0)})
	if plan.target != 4*gib || plan.shortBytes != plan.demand-plan.capacity || plan.reason != "guest_needs" {
		t.Fatalf("starved machine plan %+v", plan)
	}
	// About to run out of memory on a measured machine: still no growth past
	// what the machine backs (its OOM killer would take the whole guest);
	// the shortfall is published for the outer level.
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(4*gib, 4*gib-100*mib, 0), machine: linuxMachine(6*gib, 0)})
	if plan.reason != "guest_severe" || plan.target != 4*gib || plan.shortBytes == 0 || !plan.short {
		t.Fatalf("severe on a starved machine %+v", plan)
	}
	// Where the machine's memory is not measured, a guest in trouble gets
	// its demand.
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(4*gib, 4*gib-100*mib, 0), machine: MachineMemory{Total: 16 * gib, Boot: 16 * gib}})
	if plan.reason != "guest_severe" || plan.target < plan.demand {
		t.Fatalf("severe on an unmeasured machine %+v", plan)
	}
	// macOS or Windows under an early warning: growth stops at the old half
	// share, and a guest above it returns its excess.
	p = memoryPolicy{maximum: 12 * gib, target: 12 * gib}
	plan = p.plan(guestInput{now: now, ok: true, sample: guestSample(12*gib, 3*gib, 0), machine: MachineMemory{Total: 16 * gib, Boot: 16 * gib, Elevated: true}})
	if plan.capacity != 8*gib || plan.target != 8*gib || plan.reason != "machine_smaller" {
		t.Fatalf("elevated desktop plan %+v", plan)
	}
}

func TestGuestPlanHoldsOnFailedSamples(t *testing.T) {
	now := time.Unix(1e9, 0)
	p := memoryPolicy{maximum: 6 * gib, target: 3 * gib, resident: 2 * gib}
	machine := linuxMachine(8*gib, 3*gib)
	var targets []uint64
	for range 6 {
		plan := p.plan(guestInput{now: now, machine: machine})
		if plan.target < p.target {
			t.Fatalf("a failed sample shrank: %+v", plan)
		}
		p.target = plan.target
		targets = append(targets, plan.target>>20)
	}
	// Two holds, then 1 GiB per failure while the machine has room: the guest
	// held 2 GiB at its last sample, so the machine backs 2 + 3 GiB - 256 MiB.
	want := []uint64{3072, 3072, 4096, 4864, 4864, 4864}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("targets after failures %v, want %v", targets, want)
		}
	}
	// A short machine keeps the target as it is.
	p = memoryPolicy{maximum: 6 * gib, target: 3 * gib, failures: 5}
	if plan := p.plan(guestInput{now: now, machine: MachineMemory{Pressure: true}}); plan.target != 3*gib || plan.reason != "sample_failed_hold" {
		t.Fatalf("failed sample on a short machine %+v", plan)
	}
	// A guest that keeps failing on a machine without room asks the outer
	// level for the step.
	p = memoryPolicy{maximum: 6 * gib, target: 3 * gib, resident: 3 * gib, failures: 5}
	if plan := p.plan(guestInput{now: now, machine: linuxMachine(4*gib, 0)}); plan.target != 3*gib || plan.shortBytes != 4*gib-(3*gib-256*mib) {
		t.Fatalf("failed sample on a full machine %+v", plan)
	}
	// The first good sample resumes: nothing stays paused.
	plan := p.plan(guestInput{now: now, ok: true, sample: guestSample(3*gib, gib, 0), machine: MachineMemory{}})
	if p.failures != 0 || plan.reason == "sample_failed_hold" {
		t.Fatalf("good sample after failures %+v", plan)
	}
}

func TestGuestDemandIgnoresColdSwapAndContainedBrowser(t *testing.T) {
	p := memoryPolicy{maximum: 6 * gib, target: 3 * gib}
	s := guestSample(3*gib, gib, 0)
	s.SwapTotal, s.SwapFree = gib, 300*mib
	if cold, hot := p.guestDemand(s, false), p.guestDemand(s, true); cold >= hot || cold != 2304*mib {
		t.Fatalf("cold swap counted: cold %d hot %d", cold>>20, hot>>20)
	}
	// The browser is throttled at its limit: the guest-wide stall is high,
	// the protected classes do not wait. Not guest pressure.
	s.Stall, s.Protected, s.ProtectedStall, s.BrowserStall = 35, true, 0.2, 35
	if pressure, _ := guestPressure(s); pressure {
		t.Fatal("a contained browser counted as guest pressure")
	}
	s.Protected = false
	if pressure, _ := guestPressure(s); !pressure {
		t.Fatal("without the governor the guest-wide stall must count")
	}
}

func TestGuestShortAdvice(t *testing.T) {
	now := time.Unix(1e9, 0)
	// The evidence: a 3 GiB guest with 48 MB available, at its maximum.
	p := memoryPolicy{maximum: 3 * gib, target: 3 * gib}
	s := ResourceSample{Total: 3049803776, Available: 48779264, Stall: 2.88, Boot: strings.Repeat("b", 36)}
	plan := p.plan(guestInput{now: now, ok: true, sample: s, machine: linuxMachine(8*gib, 4*gib)})
	var status ResourceStatus
	explain(&status, plan, 3*gib, 6*gib, false, 0)
	if !status.Short || status.LimitedBy != "maximum" || status.Advice == nil || status.Advice.Message != "可以把内存调到 6 GB，需要重启环境。" || !status.Advice.RestartRequired || status.Advice.MemoryBytes != 6*gib {
		t.Fatalf("advice %+v %+v", status, status.Advice)
	}
	// The guest then stops answering: the advice stays.
	failed := p
	explain(&status, failed.plan(guestInput{now: now, machine: linuxMachine(8*gib, 4*gib)}), 3*gib, 6*gib, false, 1)
	if !status.Short || status.Advice == nil || status.Advice.MemoryBytes != 6*gib || status.Reason != "sample_failed_hold" || status.NextAction != status.Advice.Message {
		t.Fatalf("advice lost on a failed sample %+v", status)
	}
	explain(&status, plan, 3*gib, 6*gib, true, 0)
	if status.Advice == nil || !strings.Contains(status.Advice.Message, "改为自动（推荐）") {
		t.Fatalf("manual advice %+v", status.Advice)
	}
	explain(&status, plan, 3*gib, 3*gib, false, 0)
	if status.Advice == nil || status.Advice.RestartRequired {
		t.Fatalf("advice at this machine's limit %+v", status.Advice)
	}
	// With room it is not short and wants more than the machine has now only
	// when the machine cannot back it: then the gateway is told.
	p = memoryPolicy{maximum: 6 * gib, target: 3 * gib}
	plan = p.plan(guestInput{now: now, ok: true, sample: s, machine: linuxMachine(8*gib, 512*mib)})
	if plan.shortBytes == 0 || plan.target != plan.capacity || plan.target >= plan.demand || !plan.short {
		t.Fatalf("machine-limited plan %+v", plan)
	}
}

func TestGuestClassLimits(t *testing.T) {
	// The evidence guest: 3 GiB, control plane 0.4 GB, two sessions 0.51 GB.
	classes := map[string]GuestClass{"control": {Current: 400 * mib}, "sessions": {Current: 513 * mib}, "browser": {Current: 1694 * mib}}
	limits := planGuestClasses(3*gib, classes)
	if limits.BrowserHigh != 1600*mib || limits.BrowserMax != 1856*mib || limits.ControlLow != 576*mib || limits.SessionsLow != 832*mib {
		t.Fatalf("limits for the evidence guest %+v", limits)
	}
	// The same guest sized by the machine: 6 GiB; the 1.7 GB tab fits.
	if limits := planGuestClasses(6*gib, classes); limits.BrowserHigh < 4*gib {
		t.Fatalf("limits at 6 GiB %+v", limits)
	}
	random := rand.New(rand.NewPCG(9, 10))
	for range 5000 {
		size := uint64(1+random.IntN(64)) * 256 * mib
		classes := map[string]GuestClass{"control": {Current: uint64(random.IntN(2048)) * mib}, "sessions": {Current: uint64(random.IntN(8192)) * mib}}
		l := planGuestClasses(size, classes)
		if l.BrowserMax <= l.BrowserHigh || l.BrowserHigh < 768*mib || l.ControlLow > size/4 || l.SessionsLow > size/2 || l.SessionsHigh < gib {
			t.Fatalf("limits out of bounds for %d MiB %v: %+v", size>>20, classes, l)
		}
		if size >= 2*gib && l.BrowserHigh > max(768*mib, size*3/4) {
			t.Fatalf("browser above three quarters: %+v", l)
		}
	}
}

func TestGovernorReportMustBeFresh(t *testing.T) {
	lines := []string{"governor 1 100.0", "class control 1000 0.5 2000 0 0", "class browser 5000 30 0 6000 7000", "class bogus 1 1 1 1 1"}
	classes := parseGovernor(lines, 105)
	if len(classes) != 2 || classes["browser"].High != 6000 || classes["control"].Stall != 0.5 {
		t.Fatalf("fresh report %+v", classes)
	}
	if parseGovernor(lines, 200) != nil || parseGovernor(lines[1:], 105) != nil {
		t.Fatal("a stale or unstamped report was used")
	}
	sample := "MemTotal: 4000000 kB\nMemFree: 100000 kB\nMemAvailable: 2000000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\noom_kill 0\npswpin 12\nsome avg10=30.00 avg60=0 total=1\ncpu 1 0 1 1 0 0 0 0\nboot " + strings.Repeat("b", 36) + "\nuptime 105.2 300.1\n" + strings.Join(lines, "\n") + "\n"
	s, err := parseResources([]byte(sample))
	if err != nil || !s.Protected || s.ProtectedStall != 0.5 || s.BrowserStall != 30 || s.Free != 100000<<10 || !s.swapInsKnown || s.swapIns != 12 {
		t.Fatalf("sample with governor %+v %v", s, err)
	}
}

func TestGuestPlanProperties(t *testing.T) {
	random := rand.New(rand.NewPCG(11, 12))
	now := time.Unix(1e9, 0)
	for range 20000 {
		maximum := uint64(2+random.IntN(30)) * gib
		p := memoryPolicy{maximum: maximum, target: min(maximum, uint64(1+random.IntN(int(maximum>>28)))*256*mib)}
		p.target = max(p.target, 2*gib)
		total := p.target - uint64(random.IntN(200))*mib
		used := uint64(random.IntN(int(total>>20))) * mib
		s := guestSample(total, used, uint64(random.IntN(int((total-used)>>20)+1))*mib)
		s.Stall = float64(random.IntN(4)) * 4
		s.Balloon = maximum - p.target
		machine := MachineMemory{}
		if random.IntN(2) == 0 {
			machine = linuxMachine(maximum+uint64(random.IntN(8))*gib, uint64(random.IntN(8192))*mib)
		}
		machine.Pressure = random.IntN(5) == 0
		ok := random.IntN(6) != 0
		before, before0short := p.target, p.short
		plan := p.plan(guestInput{now: now.Add(time.Duration(random.IntN(60)) * time.Second), ok: ok, sample: s, machine: machine, quiet: random.IntN(2) == 0})
		if plan.target > maximum || plan.target < min(2*gib, maximum) && plan.target < before {
			t.Fatalf("target out of bounds %+v", plan)
		}
		if !ok && plan.target < before {
			t.Fatalf("failed sample shrank %+v", plan)
		}
		if ok && plan.target < before && plan.target < plan.demand {
			// Only to follow a measured machine down, never below the working
			// set plus a cushion, and the rest is asked of the outer level.
			if !machine.Measured || plan.target < min(plan.demand, max(2*gib, roundUpUnit(s.Total-s.Available+guestUnit))) || plan.shortBytes != plan.demand-plan.capacity {
				t.Fatalf("shrank below the demand %+v", plan)
			}
		}
		if !ok && plan.shortBytes < before0short {
			t.Fatalf("a failed sample withdrew the outer request %+v", plan)
		}
		if plan.target > before && ok && !plan.pressure && plan.demand <= before {
			t.Fatalf("grew without need %+v", plan)
		}
		if ok && machine.Measured && plan.target > before && plan.target > plan.capacity {
			t.Fatalf("grew past what a measured machine backs %+v", plan)
		}
	}
}

func TestGuestPlanStableUnderConstantUse(t *testing.T) {
	now := time.Unix(1e9, 0)
	p := memoryPolicy{maximum: 12 * gib, target: 12 * gib}
	machine := linuxMachine(16*gib, 6*gib)
	var last uint64
	changes := 0
	for second := 0; second < 600; second += 2 {
		total := p.target
		s := guestSample(total, 3*gib, 0)
		s.Balloon = p.maximum - p.target
		plan := p.plan(guestInput{now: now.Add(time.Duration(second) * time.Second), ok: true, sample: s, machine: machine})
		if plan.target != p.target {
			changes++
			if second > 300 {
				t.Fatalf("still changing after 5 minutes of constant use: %d -> %d", p.target>>20, plan.target>>20)
			}
		}
		p.target, last = plan.target, plan.target
	}
	if last < 3*gib+gib || last > 3*gib+gib+768*mib || changes > 12 {
		t.Fatalf("settled at %d MiB after %d changes", last>>20, changes)
	}
}
