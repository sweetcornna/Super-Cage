package cage

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Level B of resource-allocation.md: the balloon target of a Linux guest, and
// the cgroup limits the guest governor applies inside it (level C).

const (
	mib = uint64(1 << 20)
	// guestUnit rounds balloon targets.
	guestUnit = 256 * mib
	// guestFailureHold is how many failed samples in a row keep the target as
	// it is; each further failure grows it one guestFailureStep.
	guestFailureHold = 2
	guestFailureStep = gib
	// governorFresh is how far the governor's report may lag the probe.
	governorFresh = 10 * time.Second
)

func roundUpUnit(size uint64) uint64   { return (size + guestUnit - 1) / guestUnit * guestUnit }
func roundDownUnit(size uint64) uint64 { return size / guestUnit * guestUnit }

// GuestClass is one priority class inside the guest as the governor reports
// it: control (system services, SSH sessions without a terminal: probes and
// helpers), sessions (terminals, claude, codex) and browser.
type GuestClass struct {
	Name      string  `json:"name"`
	Current   uint64  `json:"current_bytes"`
	Stall     float64 `json:"stall_percent"`
	Protected uint64  `json:"protected_bytes,omitempty"`
	High      uint64  `json:"high_bytes,omitempty"`
	Max       uint64  `json:"max_bytes,omitempty"`
}

// guestInput is one turn of the host agent's loop.
type guestInput struct {
	now     time.Time
	sample  ResourceSample // valid when ok
	ok      bool
	swapIn  bool // pswpin advanced since the previous sample of this boot
	quiet   bool // the guest's CPUs are nearly idle
	machine MachineMemory
}

// guestPlan is the decision and its explanation.
type guestPlan struct {
	target, wanted, capacity, demand uint64
	pressure, severe, short          bool
	// shortBytes is what the guest wants and the machine cannot back now,
	// for an outer level (machine-memory.json).
	shortBytes uint64
	reason     string
}

// guestPressure reports a guest short of memory, and one about to run out.
// The stall that counts is that of the protected classes when the governor
// reports them: a browser held at its limit stalls only itself.
func guestPressure(s ResourceSample) (pressure, severe bool) {
	stall := s.Stall
	if s.Protected {
		stall = s.ProtectedStall
	}
	pressure = s.Available < max(512*mib, s.Total/8) || stall >= 1
	severe = s.Available < max(256*mib, s.Total/16) || stall >= 10
	return pressure, severe
}

// guestDemand is the guest's working set plus headroom and kernel memory
// outside MemTotal, at least the floor and at most the maximum. Swapped-out
// pages count only while they are being swapped in: cold swap is not demand.
func (p *memoryPolicy) guestDemand(s ResourceSample, swapIn bool) uint64 {
	used := s.Total - s.Available
	if swapIn {
		used += s.SwapTotal - s.SwapFree
	}
	reserve := max(gib, used/4)
	return min(p.maximum, max(min(2*gib, p.maximum), roundUpUnit(used+reserve+256*mib)))
}

// machineCapacity is the largest target the machine can back now: the guest's
// resident memory plus what the machine has available, less a margin that
// stays below the gateway's smallest headroom (1 GiB), so a machine parked at
// its working set plus headroom never pins the guest. Without an available
// figure (macOS, Windows) it is the maximum, or the old half share while the
// machine reports an early pressure warning.
func (p *memoryPolicy) machineCapacity(s ResourceSample, known bool, m MachineMemory) uint64 {
	capacity := p.maximum
	switch {
	case m.Measured && m.Total > 0:
		margin := min(max(m.Total/64, 256*mib), 512*mib)
		// Without a sample, what the guest held at the last one: the machine's
		// own figure already shows anything the guest took since.
		resident := p.target
		if p.resident > 0 {
			resident = p.resident
		}
		if known {
			resident = s.Total
			if s.Free > 0 && s.Free <= s.Total {
				resident = s.Total - s.Free
			}
		}
		if room := resident + m.Available; room > margin {
			capacity = min(capacity, roundDownUnit(room-margin))
		} else {
			capacity = 0
		}
	case m.Elevated && m.Boot > 0:
		capacity = min(capacity, max(2*gib, roundDownUnit(m.Boot/2)))
	}
	return capacity
}

// plan is the target rule. It grows at once when the guest needs memory,
// within what the machine can back, and tells the outer level what the
// machine lacks (shortBytes); follows the machine down when it shrinks,
// never below the guest's working set; returns idle memory after sustained
// headroom; and holds on a failed sample, growing one step per further
// failure while the machine has room.
func (p *memoryPolicy) plan(in guestInput) guestPlan {
	if !in.ok {
		p.idleSince = time.Time{}
		p.failures++
		capacity := p.machineCapacity(ResourceSample{}, false, in.machine)
		// Hold everything, including what the outer level was told the
		// machine is short of: a failed sample is no evidence that it ended.
		result := guestPlan{target: p.target, wanted: p.target, capacity: capacity, shortBytes: p.short, reason: "sample_failed_hold"}
		if p.failures > guestFailureHold && p.target < p.maximum {
			// A guest that stops answering may be out of memory: one step more
			// per failure, within what the machine backs, and the outer level
			// is asked for the step the machine lacks (the gateway's own rule
			// for a guest that does not answer).
			want := min(p.maximum, roundUpUnit(p.target+guestFailureStep))
			if !in.machine.Pressure && p.target < capacity {
				result.target, result.reason = min(capacity, want), "sample_failed_grow"
			}
			result.wanted = want
			if want > capacity {
				result.shortBytes = max(p.short, want-capacity)
			}
		}
		p.short = result.shortBytes
		return result
	}
	p.failures = 0
	s := in.sample
	p.resident = s.Total
	if s.Free > 0 && s.Free <= s.Total {
		p.resident = s.Total - s.Free
	}
	demand := p.guestDemand(s, in.swapIn)
	pressure, severe := guestPressure(s)
	capacity := p.machineCapacity(s, true, in.machine)
	// Growth stays within what the machine can back. Where the machine's
	// available memory is measured (Linux), never beyond it, even for a guest
	// about to run out: the machine's OOM killer would take QEMU, the whole
	// guest, while the guest's own OOM order takes one browser process. The
	// shortfall goes to the outer level instead (shortBytes). Where it is not
	// measured (macOS, Windows), capacity is only an estimate, and a guest in
	// trouble gets its demand as before.
	backed := min(p.maximum, max(capacity, p.target))
	ceiling := min(p.maximum, max(capacity, demand))
	if in.machine.Measured {
		ceiling = backed
	}
	result := guestPlan{target: p.target, wanted: p.target, capacity: capacity, demand: demand, pressure: pressure, severe: severe, reason: "steady"}
	if pressure || demand > p.target {
		p.idleSince = time.Time{}
		switch {
		case in.machine.Pressure && pressure:
			// The machine is short as well: a guest under pressure gets what
			// keeps it out of that pressure, never the whole budget.
			result.target, result.wanted, result.reason = max(p.target, min(demand, ceiling)), max(p.target, demand), "machine_short_guest_short"
		case in.machine.Pressure:
			result.wanted, result.reason = demand, "machine_short_waiting"
		case severe:
			// Only an imminent out-of-memory condition justifies the whole
			// budget at once (where the machine's memory is not measured).
			grown := roundUpUnit(p.target + max(2*gib, p.target/2))
			result.target, result.wanted, result.reason = max(p.target, ceiling), min(p.maximum, max(demand, grown)), "guest_severe"
		case pressure:
			// Grow geometrically instead of jumping to the maximum: a short
			// burst stops at a modest target, a sustained one still reaches the
			// maximum within a few two-second probes.
			grown := roundUpUnit(p.target + max(2*gib, p.target/2))
			result.target = max(p.target, min(backed, max(demand, grown)))
			result.wanted, result.reason = min(p.maximum, max(demand, grown)), "guest_pressure"
		default:
			result.target, result.wanted, result.reason = max(p.target, min(demand, backed)), demand, "guest_needs"
		}
		if result.wanted > capacity {
			result.shortBytes = result.wanted - capacity
		}
		result.short = pressure && result.target >= backed && (result.target >= p.maximum || capacity < p.maximum)
		p.short = result.shortBytes
		return result
	}
	if p.target > capacity {
		// The machine got smaller (the gateway shrank it, or its own use
		// grew): give back what the machine cannot back, at once, before it
		// swaps. Where the machine is measured, down to the guest's working
		// set plus a cushion, never below it, telling the outer level the
		// rest of the demand; elsewhere down to the demand.
		p.idleSince = time.Time{}
		floor := demand
		if in.machine.Measured {
			floor = min(demand, max(min(2*gib, p.maximum), roundUpUnit(s.Total-min(s.Total, s.Available)+guestUnit)))
		}
		result.target, result.wanted, result.reason = min(p.target, max(floor, capacity)), demand, "machine_smaller"
		if demand > capacity {
			result.shortBytes = demand - capacity
		}
		p.short = result.shortBytes
		return result
	}
	result.wanted = demand
	p.short = 0
	if demand+512*mib > p.target {
		p.idleSince = time.Time{}
		return result
	}
	if p.idleSince.IsZero() {
		p.idleSince = in.now
	}
	// Reclaim after sustained headroom: at once while the machine is short of
	// memory, sooner when the guest's CPUs are quiet.
	wait := 30 * time.Second
	switch {
	case in.machine.Pressure:
		wait = 0
	case in.quiet:
		wait = 10 * time.Second
	}
	result.reason = "reclaim_wait"
	if in.now.Sub(p.idleSince) < wait {
		return result
	}
	// Never request another reclamation until the guest reflects the prior one.
	if s.Balloon < p.maximum-p.target {
		return result
	}
	// Reclaim a quarter of the excess per probe (at least 256 MiB), half of it
	// for a short machine or a quiet guest. Never below the demand.
	step := (p.target - demand) / 4
	if in.machine.Pressure || in.quiet {
		step = (p.target - demand) / 2
	}
	step = max(guestUnit, step/guestUnit*guestUnit)
	result.target, result.reason = max(demand, p.target-step), "reclaim_idle"
	return result
}

// next is the target rule without machine figures, kept for callers and
// tests that only know whether the host is short of memory.
func (p *memoryPolicy) next(now time.Time, s ResourceSample, hostPressure, quiet bool) uint64 {
	return p.plan(guestInput{now: now, sample: s, ok: true, quiet: quiet, machine: MachineMemory{Pressure: hostPressure}}).target
}

// ClassLimits are the cgroup limits of the guest's classes, in bytes.
type ClassLimits struct {
	ControlLow, SessionsLow, SessionsHigh, BrowserHigh, BrowserMax uint64
}

func roundUp64(size uint64) uint64 { return (size + 64*mib - 1) / (64 * mib) * (64 * mib) }

// planGuestClasses sizes the classes for a guest that can have size bytes now
// (its attainable size: the maximum, or less when the machine cannot back
// it). The control plane and the sessions are protected at their current use
// plus a margin; the browser gets what is left as a soft limit (reclaimed
// into zram and throttled above it) and a hard limit a little higher (one
// browser process killed, alone, at it); the sessions a soft limit that
// always leaves the control plane its protection.
func planGuestClasses(size uint64, classes map[string]GuestClass) ClassLimits {
	control, sessions := classes["control"].Current, classes["sessions"].Current
	var limits ClassLimits
	limits.ControlLow = min(roundUp64(control+128*mib), size/4/(64*mib)*(64*mib))
	limits.SessionsLow = min(roundUp64(sessions+256*mib), size/2/(64*mib)*(64*mib))
	protected := control + sessions + 512*mib
	high := uint64(768 * mib)
	if size > protected {
		high = max(high, size-protected)
	}
	high = min(high, max(768*mib, size*3/4))
	limits.BrowserHigh = high / (64 * mib) * (64 * mib)
	limits.BrowserMax = roundUp64(limits.BrowserHigh + max(256*mib, size/16))
	limits.SessionsHigh = max(gib, (size-min(size, limits.ControlLow+256*mib))/(64*mib)*(64*mib))
	return limits
}

// script writes the limits for the guest governor, atomically, when the
// runtime directory the start script made is there.
func (l ClassLimits) script() string {
	body := fmt.Sprintf("version 1\\ncontrol_low %d\\nsessions_low %d\\nsessions_high %d\\nbrowser_high %d\\nbrowser_max %d\\n",
		l.ControlLow, l.SessionsLow, l.SessionsHigh, l.BrowserHigh, l.BrowserMax)
	return "if [ -d /run/cage-resources/in ] && [ -w /run/cage-resources/in ]; then printf '" + body +
		"' > /run/cage-resources/in/.limits.$ && mv -f /run/cage-resources/in/.limits.$ /run/cage-resources/in/limits; fi\n"
}

// parseGovernor reads the governor's report (the lines after "uptime" in a
// probe): "governor 1 <uptime>" and "class <name> <current> <stall> <protected>
// <high> <max>". It returns the classes when the report is fresh.
func parseGovernor(lines []string, uptime float64) map[string]GuestClass {
	classes := map[string]GuestClass{}
	fresh := false
	for _, line := range lines {
		f := strings.Fields(line)
		switch {
		case len(f) == 3 && f[0] == "governor" && f[1] == "1":
			at, err := strconv.ParseFloat(f[2], 64)
			fresh = err == nil && uptime > 0 && at <= uptime+1 && uptime-at <= governorFresh.Seconds()
		case len(f) == 7 && f[0] == "class" && (f[1] == "control" || f[1] == "sessions" || f[1] == "browser"):
			var values [5]float64
			valid := true
			for i := range values {
				v, err := strconv.ParseFloat(f[2+i], 64)
				valid = valid && err == nil && v >= 0 && v <= 1<<44
				values[i] = v
			}
			if valid && values[1] <= 100 {
				classes[f[1]] = GuestClass{Name: f[1], Current: uint64(values[0]), Stall: values[1], Protected: uint64(values[2]), High: uint64(values[3]), Max: uint64(values[4])}
			}
		}
	}
	if !fresh {
		return nil
	}
	return classes
}

// GuestPolicy is the balloon target rule of one guest, for simulations and
// tools outside the host agent.
type GuestPolicy struct{ policy memoryPolicy }

// GuestDecision is one GuestPolicy decision.
type GuestDecision struct {
	Target, Wanted, Capacity, Demand, ShortBytes uint64
	Pressure, Short                              bool
	Reason                                       string
}

// NewGuestPolicy starts a guest at its boot maximum, as the host agent does.
func NewGuestPolicy(maximum uint64) *GuestPolicy {
	return &GuestPolicy{policy: memoryPolicy{maximum: maximum, target: maximum}}
}

// Decide plans one sample; a nil sample is a failed one. The policy assumes
// the target is applied: call Applied with what the balloon accepted.
func (g *GuestPolicy) Decide(now time.Time, sample *ResourceSample, swapIn, quiet bool, machine MachineMemory) GuestDecision {
	in := guestInput{now: now, swapIn: swapIn, quiet: quiet, machine: machine}
	if sample != nil {
		in.sample, in.ok = *sample, true
	}
	plan := g.policy.plan(in)
	return GuestDecision{Target: plan.target, Wanted: plan.wanted, Capacity: plan.capacity, Demand: plan.demand, ShortBytes: plan.shortBytes,
		Pressure: plan.pressure, Short: plan.short, Reason: plan.reason}
}

// Applied records the target the balloon now has.
func (g *GuestPolicy) Applied(target uint64) { g.policy.target = target }

// PlanGuestClasses is planGuestClasses for simulations and tools.
func PlanGuestClasses(size uint64, classes map[string]GuestClass) ClassLimits {
	return planGuestClasses(size, classes)
}
