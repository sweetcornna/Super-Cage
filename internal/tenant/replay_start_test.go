package tenant

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// TestReplayStoppedEnvironmentStart replays the incident end to end: the
// workspace VM boots at its ceiling (9728 MiB) with its environment stopped
// and idle; the gateway (A, every 15 s) returns the idle memory down to the
// guarantee; then someone starts the environment: the host agent's start
// (cage.PrepareBoot, B) publishes its need, the gateway reads those bytes
// through its own parser and grows the VM, the start sees the machine map the
// 7 GiB guest and launches it; the environment runs (the simulator's guest and
// B's balloon rule) and the VM goes back to the running guest's demand.
func TestReplayStoppedEnvironmentStart(t *testing.T) {
	id := vmID(1)
	profile := t.TempDir()
	const own = 538 * simMiB // the workspace's own services
	world := &simWorld{host: 32 * simGiB, hostOwn: 2 * simGiB, ceiling: 9728 << 20, now: time.Unix(1e9, 0), held: map[string]demandState{}, silent: map[string]int{}}
	limit := int64(9728 << 20)
	memTotal := func() uint64 { return uint64(limit) - 200*simMiB }
	stopped := func() InstanceMemory {
		return InstanceMemory{Status: "Running", Limit: limit, GuestTotal: int64(memTotal()), GuestAvailable: int64(memTotal() - own)}
	}
	var journal []string
	round := func() {
		entry := stopped()
		reading, err := parseGuestMemory(guestExec(entry, profile), world.now)
		if err != nil {
			t.Fatal(err)
		}
		entry.NestedShort, entry.NestedPressure, entry.NestedStart = reading.NestedShort, reading.NestedPressure, reading.NestedStart
		state := MemoryState{HostTotal: int64(world.host), HostAvailable: int64(world.host - world.hostOwn - min(uint64(limit), own+200*simMib())), Ceiling: world.ceiling,
			Instances: map[string]InstanceMemory{id: entry}}
		result := allocateMemory(allocationInput{now: world.now, state: state, minimum: DefaultMemoryMin}, world.held)
		world.held = result.held
		for _, change := range result.changes {
			journal = append(journal, (time.Duration(world.second)*time.Second).String()+" "+mib(change.from)+" -> "+mib(change.to)+" ("+change.reason+")")
			limit = change.to
		}
	}
	tick := func() {
		if world.second%15 == 0 {
			round()
		}
		world.second++
		world.now = world.now.Add(time.Second)
	}
	// Stopped and idle for 10 minutes.
	shrunkAt := -1
	for world.second < 600 {
		tick()
		if shrunkAt < 0 && limit == 2048<<20 {
			shrunkAt = world.second
		}
	}
	if limit != 2048<<20 || shrunkAt < 0 || shrunkAt > 300 {
		t.Fatalf("idle return: %d MiB, at the guarantee after %d s: %q", limit>>20, shrunkAt, journal)
	}
	// One second after a round: the start waits most of the next one.
	tick()
	requested := world.second
	var progress []string
	decision, err := cage.PrepareBoot(context.Background(), cage.BootRequest{Profile: profile, Maximum: 7 << 30,
		Machine: func() (cage.BootMachine, bool) {
			return cage.BootMachine{Total: memTotal(), Available: memTotal() - own, CommitLimit: memTotal() / 2, Committed: 900 << 20, Overcommit: -1}, true
		},
		Overcommit: func() int { return 0 }, Growable: func() bool { return true }, Progress: func(m string) { progress = append(progress, m) },
		Now: func() time.Time { return world.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			for range int(d / time.Second) {
				tick()
			}
			return nil
		}})
	launched := world.second
	if err != nil || decision.Maximum != 7<<30 || !decision.Asked || len(progress) != 1 {
		t.Fatalf("start: %+v %v %q", decision, err, progress)
	}
	if _, err := os.Stat(filepath.Join(profile, "machine-memory.json")); !os.IsNotExist(err) {
		t.Fatalf("the request outlived the start: %v", err)
	}
	if wait := launched - requested; wait > 20 || memTotal() < 7<<30+256*simMiB {
		t.Fatalf("QEMU launched %d s after the request, MemTotal %d MiB", wait, memTotal()>>20)
	}
	grown := limit
	// The environment runs: the guest boots and the person uses it for five
	// minutes, then leaves it idle. The machine keeps the held demand across
	// the gap between the start's request and the guest's own reports.
	m := &simMachine{id: id, limit: limit, own: own, active: true}
	m.guest = newSimGuest(7<<30, true, simClasses{control: 400 * simMiB, sessions: 300 * simMiB, browser: 150 * simMiB})
	world.machines = []*simMachine{m}
	minimum, last := m.limit, m.limit
	boot := func(second int, m *simMachine) {
		if m.limit != last {
			journal = append(journal, (time.Duration(second)*time.Second).String()+" "+mib(last)+" -> "+mib(m.limit)+" (running)")
			last = m.limit
		}
		ramp := min(uint64(second-launched), 120)
		m.guest.want = simClasses{control: 900 * simMiB, sessions: 300*simMiB + ramp*4*simMiB, browser: 150*simMiB + ramp*8*simMiB, cache: 600 * simMiB}
		m.active = second-launched < 300
		minimum = min(minimum, m.limit)
	}
	world.run(1200, boot)
	for _, change := range journal {
		t.Log(change)
	}
	t.Logf("start requested at %d s, QEMU launched at %d s (%d s), VM %d MiB at launch, %d MiB after 20 min running (lowest %d), machine out of memory %d s, probes silent %d s",
		requested, launched, launched-requested, grown>>20, m.limit>>20, minimum>>20, m.swaps, world.silent[id])
	if m.swaps != 0 || world.silent[id] != 0 {
		t.Fatalf("running: machine out of memory %d s, probes silent %d s", m.swaps, world.silent[id])
	}
	// Back to the running guest's demand: well below the start's grant, never
	// below what the machine uses.
	if m.limit >= grown || uint64(m.limit) < m.used()+512*simMiB {
		t.Fatalf("after the start: VM %d MiB, uses %d MiB, start grant %d MiB", m.limit>>20, m.used()>>20, grown>>20)
	}
}

func simMib() uint64 { return simMiB }
