package tenant

import (
	"context"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Elastic memory sizes running VMs to this host. Each VM boots at the trusted
// profile's limits.memory, its ceiling, and the balancer then moves its
// limits.memory through the virtio balloon. The rule is allocateMemory
// (allocation.go): max-min fair water-filling over held demands. planMemory
// below is the previous policy, still selectable with
// CAGE_TENANT_MEMORY_POLICY=legacy:
//
//   - A guest that runs short (less than its headroom available) grows at
//     once, also while the host is short of memory: a guest OOM kill is worse.
//   - While the host is short of memory, every VM gives back what it holds
//     beyond its working set plus headroom: mostly page cache.
//   - A VM above its fair share of the host shrinks toward it, for example
//     when another VM starts, unless it was used within memoryActiveWindow: the
//     idle VMs give back first, and only a shortage of host memory (the bullet
//     above) shrinks one that is in use.
//   - Otherwise sizes stay as they are. Nothing grows only because memory is
//     free, so a shrink is never undone by itself.
//
// Shrinking moves at most memoryShrinkStep per sweep and never below the
// guest's working set (MemTotal - MemAvailable) plus headroom. A guest that
// stops answering for memorySilentSweeps sweeps in a row may be thrashing
// after a shrink, so it gets its memory back: at once, or one step per sweep
// while the host is short.

// MemoryBalancer is implemented by runtimes that can resize a running
// workspace's memory without a restart.
type MemoryBalancer interface {
	// ElasticMemory reports whether this runtime resizes workspaces at all.
	ElasticMemory() bool
	MemoryState(context.Context) (MemoryState, error)
	SetMemory(ctx context.Context, id string, bytes int64) error
}

// MemoryState is one reading of the host and its gateway-owned instances.
type MemoryState struct {
	HostTotal, HostAvailable int64
	// HostStall is the share of the last ten seconds, in percent, in which a
	// host task waited for memory (PSI "some avg10"); zero when unknown.
	HostStall float64
	// Ceiling is the trusted profile's limits.memory.
	Ceiling   int64
	Instances map[string]InstanceMemory
}

// InstanceMemory is one instance's memory as the runtime sees it.
type InstanceMemory struct {
	Status string
	// Limit is the effective limits.memory in bytes.
	Limit int64
	// Hotplugged is set when Incus added memory to the VM since its boot. Such
	// a VM is only grown until its next boot: Incus cannot shrink it back
	// reliably.
	Hotplugged bool
	// GuestTotal and GuestAvailable come from the guest's /proc/meminfo. Zero
	// when the guest did not answer, and then GuestSilent is set.
	GuestTotal, GuestAvailable int64
	GuestSilent                bool
	// GuestStall is the guest's memory stall (PSI some avg10, percent) and
	// GuestSwapIns its pswpin counter (GuestSwapKnown when it was read); both
	// are optional readings. NestedShort is what a Cage guest nested in this
	// VM wants and the VM cannot back now, from the host agent inside.
	GuestStall     float64
	GuestSwapIns   uint64
	GuestSwapKnown bool
	NestedShort    int64
	// NestedPressure: the nested guest is under memory pressure itself, so its
	// shortfall is a shortage, not missing headroom.
	NestedPressure bool
	// NestedStart: NestedShort is what a nested guest that is about to start
	// needs to map its boot maximum (cage.PrepareBoot): the environment is
	// stopped and its start waits for this VM to grow.
	NestedStart bool
	// Active is set by the balancer, not by the runtime: the workspace was
	// used (browser traffic, a portal open, guest CPU) within memoryActiveWindow.
	Active bool
}

const (
	// DefaultMemoryMin is the default smallest size the balancer gives a VM.
	DefaultMemoryMin = 2 << 30
	memoryUnit       = 256 << 20
	memoryShrinkStep = 512 << 20
	memoryHeadroom   = 1 << 30
	// memoryShrinkPause is how long a failed shrink of one VM is not retried.
	memoryShrinkPause = time.Minute
	// memorySilentSweeps is how many sweeps in a row a running guest may fail
	// to report its memory before it grows back to its ceiling.
	memorySilentSweeps = 2
	// memoryActiveWindow is how long after its last use a workspace counts as
	// active: it is spared the fair-share shrink while the host is not short.
	memoryActiveWindow = time.Minute
)

// memoryBalancing is the elastic memory state of a server.
type memoryBalancing struct {
	runtime MemoryBalancer // nil when elastic memory is off
	// changes holds the workspaces being resized. Guarded by the server lock.
	changes map[string]bool
	// shrinkHold and shrinkHoldAll pause shrinking (while still allowing growth)
	// during and immediately after a self-update. Guarded by the server lock.
	shrinkHold    map[string]time.Time
	shrinkHoldAll time.Time
	// The rest belongs to the balancer goroutine.
	pause    map[string]time.Time
	failures map[string]string
	failure  string
	// silent counts the sweeps in a row each running guest did not answer.
	silent map[string]int
	// held is each VM's held demand and swapIns its last pswpin counter.
	held    map[string]demandState
	swapIns map[string]uint64
	// starts is when each VM's start request was first and last seen
	// (boundStarts).
	starts map[string]startSight
	// legacy selects the previous fair-share policy
	// (CAGE_TENANT_MEMORY_POLICY=legacy in the gateway's environment).
	legacy bool
	// report explains the last round, for the status.
	report allocationResult
}

type memoryChange struct {
	id       string
	from, to int64
	reason   string
}

func (s *Server) startMemoryBalancer() {
	runtime, ok := s.cfg.Runtime.(MemoryBalancer)
	if s.cfg.MemoryInterval == 0 || !ok || !runtime.ElasticMemory() {
		return
	}
	s.memory = memoryBalancing{runtime: runtime, changes: make(map[string]bool), shrinkHold: make(map[string]time.Time),
		shrinkHoldAll: readPostUpdateGrace(s.cfg.StateDir, s.now()),
		pause: make(map[string]time.Time), failures: make(map[string]string), silent: make(map[string]int),
		held: make(map[string]demandState), swapIns: make(map[string]uint64), starts: make(map[string]startSight), legacy: os.Getenv("CAGE_TENANT_MEMORY_POLICY") == "legacy"}
	if !s.memory.shrinkHoldAll.IsZero() {
		log.Printf("cage-tenant: holding elastic VM memory shrink until %s after update", s.memory.shrinkHoldAll.UTC().Format(time.RFC3339))
	}
	reserve := "the larger of 2 GiB and 15% of host memory"
	if s.cfg.MemoryReserve > 0 {
		reserve = mib(s.cfg.MemoryReserve)
	}
	policy := "max-min fair over held demand"
	if s.memory.legacy {
		policy = "legacy fair share"
	}
	log.Printf("cage-tenant: elastic VM memory every %s (%s); host reserve %s, VM minimum %s", s.cfg.MemoryInterval, policy, reserve, mib(s.memoryMin()))
	s.workers.Go(func() {
		ticker := time.NewTicker(s.cfg.MemoryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.context.Done():
				return
			case <-ticker.C:
				s.balanceOnce()
			}
		}
	})
}

func (s *Server) memoryMin() int64 {
	if s.cfg.MemoryMin == 0 {
		return DefaultMemoryMin
	}
	return s.cfg.MemoryMin
}

func mib(size int64) string { return strconv.FormatInt(size>>20, 10) + " MiB" }

// balanceOnce reads the host once and applies the resulting changes one at a
// time. A failed change is logged and retried later; it never marks the
// workspace unready.
func (s *Server) balanceOnce() {
	ctx, cancel := context.WithTimeout(s.context, time.Minute)
	state, err := s.memory.runtime.MemoryState(ctx)
	cancel()
	if err != nil {
		if s.context.Err() == nil && err.Error() != s.memory.failure {
			log.Printf("cage-tenant: elastic memory could not read the host: %v", err)
			s.memory.failure = err.Error()
		}
		return
	}
	s.memory.failure = ""
	now := s.now()
	fixed := make(map[string]bool)
	holdShrink := make(map[string]bool)
	s.mu.Lock()
	holdAll := !s.memory.shrinkHoldAll.IsZero() && now.Before(s.memory.shrinkHoldAll)
	if !holdAll && !s.memory.shrinkHoldAll.IsZero() {
		s.memory.shrinkHoldAll = time.Time{}
		clearPostUpdateGrace(s.cfg.StateDir)
	}
	for id, until := range s.memory.shrinkHold {
		if now.Before(until) {
			holdShrink[id] = true
		} else {
			delete(s.memory.shrinkHold, id)
		}
	}
	for id := range state.Instances {
		if _, owned := s.store.VMs[id]; !owned || s.starting[id] || s.cooling.changes[id] != nil {
			fixed[id] = true
		}
		if holdAll {
			holdShrink[id] = true
		}
		if s.activeLocked(id, now) {
			entry := state.Instances[id]
			entry.Active = true
			state.Instances[id] = entry
		}
	}
	s.mu.Unlock()
	for id, until := range s.memory.pause {
		if now.Before(until) {
			holdShrink[id] = true
		} else {
			delete(s.memory.pause, id)
		}
	}
	silent := make(map[string]bool)
	for id := range s.memory.silent {
		if entry := state.Instances[id]; entry.Status != "Running" || !entry.GuestSilent {
			delete(s.memory.silent, id)
		}
	}
	for id, entry := range state.Instances {
		if entry.Status == "Running" && entry.GuestSilent {
			s.memory.silent[id]++
			silent[id] = s.memory.silent[id] >= memorySilentSweeps
		}
	}
	swapIn := make(map[string]bool)
	for id, entry := range state.Instances {
		if !entry.GuestSwapKnown {
			continue
		}
		if previous, seen := s.memory.swapIns[id]; seen && entry.GuestSwapIns > previous {
			swapIn[id] = true
		}
		s.memory.swapIns[id] = entry.GuestSwapIns
	}
	for id := range s.memory.swapIns {
		if entry, ok := state.Instances[id]; !ok || entry.Status != "Running" {
			delete(s.memory.swapIns, id)
		}
	}
	var plan []memoryChange
	if s.memory.legacy {
		plan = planMemory(state, s.cfg.MemoryReserve, s.memoryMin(), fixed, holdShrink, silent)
	} else {
		var expired []string
		s.memory.starts, expired = boundStarts(now, state, s.memory.starts, s.memory.held)
		for _, id := range expired {
			log.Printf("cage-tenant: workspace %s: a start request still there %s after it was first seen is ignored until it has been gone for %s", id, startWindow, startRearm)
		}
		result := allocateMemory(allocationInput{now: now, state: state, reserve: s.cfg.MemoryReserve, minimum: s.memoryMin(),
			fixed: fixed, holdShrink: holdShrink, silent: silent, swapIn: swapIn}, s.memory.held)
		s.memory.held, s.memory.report, plan = result.held, result, result.changes
	}
	for _, change := range plan {
		if s.context.Err() != nil {
			return
		}
		s.mu.Lock()
		busy := s.starting[change.id] || s.cooling.changes[change.id] != nil
		if !busy {
			s.memory.changes[change.id] = true
		}
		s.mu.Unlock()
		if busy {
			continue
		}
		ctx, cancel := context.WithTimeout(s.context, 30*time.Second)
		err := s.memory.runtime.SetMemory(ctx, change.id, change.to)
		cancel()
		s.mu.Lock()
		delete(s.memory.changes, change.id)
		s.mu.Unlock()
		if err != nil {
			if change.to < change.from {
				s.memory.pause[change.id] = now.Add(memoryShrinkPause)
			}
			if s.context.Err() == nil && s.memory.failures[change.id] != err.Error() {
				log.Printf("cage-tenant: workspace %s memory %s -> %s (%s) failed: %v", change.id, mib(change.from), mib(change.to), change.reason, err)
				s.memory.failures[change.id] = err.Error()
			}
			continue
		}
		delete(s.memory.failures, change.id)
		log.Printf("cage-tenant: workspace %s memory %s -> %s (%s)", change.id, mib(change.from), mib(change.to), change.reason)
	}
}

// activeLocked reports whether a workspace was used within memoryActiveWindow.
// Unlike lastActiveLocked it never writes, so it is safe while idle cooling is
// off. Caller holds s.mu.
func (s *Server) activeLocked(id string, now time.Time) bool {
	last := s.cooling.activity[id]
	if workspace := s.workspaces[id]; workspace != nil {
		if io := workspace.lastActive(); io.After(last) {
			last = io
		}
	}
	return !last.IsZero() && now.Sub(last) < memoryActiveWindow
}

// holdMemoryShrink pauses elastic shrinking of id (or every owned VM when id is
// empty) until now + d while still allowing growth. Safe to call when elastic
// memory is disabled.
func (s *Server) holdMemoryShrink(id string, d time.Duration) {
	if d <= 0 {
		return
	}
	until := s.now().Add(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		if until.After(s.memory.shrinkHoldAll) {
			s.memory.shrinkHoldAll = until
		}
		return
	}
	if s.memory.shrinkHold == nil {
		s.memory.shrinkHold = make(map[string]time.Time)
	}
	if until.After(s.memory.shrinkHold[id]) {
		s.memory.shrinkHold[id] = until
	}
}

// memoryReserve is the host memory kept for the host itself.
func memoryReserve(configured, total int64) int64 {
	if configured > 0 {
		return configured
	}
	return max(2<<30, total/100*15)
}

func roundDownMemory(size int64) int64 { return max(size, 0) / memoryUnit * memoryUnit }
func roundUpMemory(size int64) int64   { return roundDownMemory(size + memoryUnit - 1) }

// planMemory decides the next size of every running VM it may resize, grows
// first. fixed VMs keep their size but count against the host; holdShrink VMs
// may only grow; silent VMs have not reported their memory for several sweeps
// and grow back to the ceiling.
func planMemory(state MemoryState, reserve, minimum int64, fixed, holdShrink, silent map[string]bool) []memoryChange {
	if state.HostTotal <= 0 || state.Ceiling <= 0 {
		return nil
	}
	reserve = memoryReserve(reserve, state.HostTotal)
	floor := roundDownMemory(min(state.Ceiling, max(minimum, MinimumMemory)))
	if floor < MinimumMemory {
		return nil
	}
	hardCap := max(floor, min(roundDownMemory(state.Ceiling), roundDownMemory(state.HostTotal-reserve)))
	// Below twice the reserve the host has started to eat into what it keeps
	// for itself, or tasks already wait for memory: reclaim guest caches.
	pressure := state.HostAvailable < 2*reserve || state.HostStall >= 10
	var adjustable []string
	var pinned int64
	var grow, shrink []memoryChange
	for id, entry := range state.Instances {
		switch {
		case entry.Status == "Running" && !fixed[id] && entry.GuestTotal > 0:
			adjustable = append(adjustable, id)
		case entry.Status == "Running" || entry.Status == "Frozen":
			// A frozen VM keeps its memory until the kernel swaps it out.
			pinned += entry.Limit
			if entry.Status == "Running" && !fixed[id] && silent[id] && entry.Limit < hardCap {
				to := hardCap
				if pressure {
					to = min(hardCap, roundUpMemory(entry.Limit+memoryShrinkStep))
				}
				grow = append(grow, memoryChange{id: id, from: entry.Limit, to: to, reason: "guest did not answer"})
			}
		}
	}
	slices.SortFunc(grow, func(a, b memoryChange) int { return strings.Compare(a.id, b.id) })
	if len(adjustable) == 0 {
		return grow
	}
	slices.Sort(adjustable)
	share := min(hardCap, max(floor, roundDownMemory((state.HostTotal-reserve-pinned)/int64(len(adjustable)))))
	for _, id := range adjustable {
		entry := state.Instances[id]
		used := entry.GuestTotal - entry.GuestAvailable
		// Kernel memory that MemTotal leaves out still comes from limits.memory.
		kernel := min(max(entry.Limit-entry.GuestTotal, 0), 1<<30)
		need := min(hardCap, max(floor, roundUpMemory(used+max(memoryHeadroom, used/4)+kernel)))
		if need > entry.Limit {
			grow = append(grow, memoryChange{id: id, from: entry.Limit, to: need, reason: "guest needs memory"})
			continue
		}
		if entry.Hotplugged || holdShrink[id] {
			continue
		}
		target, reason := max(need, share), "fair share of the host"
		if pressure {
			target, reason = need, "host memory is low"
		} else if entry.Active {
			// The fair share is a preparation for other VMs, not a response to a
			// shortage, and what it takes is page cache the workspace in use is
			// working with. Idle VMs give back first; a shortage still shrinks all.
			continue
		}
		if entry.Limit-target >= memoryShrinkStep {
			shrink = append(shrink, memoryChange{id: id, from: entry.Limit, to: max(target, roundUpMemory(entry.Limit-memoryShrinkStep)), reason: reason})
		}
	}
	return append(grow, shrink...)
}
