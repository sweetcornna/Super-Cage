package tenant

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// A stopped environment: the workspace VM runs only the gateway-side
// services, 538 MiB of working set, and its kernel keeps 310 MiB of
// limits.memory outside MemTotal (the incident: 2048 MiB, MemTotal 1738).
func stoppedVM(limit int64) InstanceMemory {
	total := limit - 310*testMiB
	return InstanceMemory{Status: "Running", Limit: limit, GuestTotal: total, GuestAvailable: total - 538*testMiB}
}

// bootMachineOf is what cage.PrepareBoot reads inside that VM.
func bootMachineOf(entry InstanceMemory) cage.BootMachine {
	return cage.BootMachine{Total: uint64(entry.GuestTotal), Available: uint64(entry.GuestAvailable), CommitLimit: uint64(entry.GuestTotal) / 2, Committed: 900 << 20, Overcommit: 0}
}

// guestExec is what the gateway's one exec returns for a VM whose profile is
// dir: its meminfo and machine-memory.json as the start wrote it.
func guestExec(entry InstanceMemory, dir string) []byte {
	body := "MemTotal: " + strconv.FormatInt(entry.GuestTotal>>10, 10) + " kB\nMemAvailable: " + strconv.FormatInt(entry.GuestAvailable>>10, 10) + " kB\n#cage-psi\n#cage-vmstat\n#cage-nested\n"
	if data, err := os.ReadFile(filepath.Join(dir, "machine-memory.json")); err == nil {
		body += string(data)
	}
	return []byte(body)
}

// readThrough feeds the bytes the start wrote through the gateway's parser.
func readThrough(t *testing.T, entry InstanceMemory, dir string, now time.Time) InstanceMemory {
	t.Helper()
	reading, err := parseGuestMemory(guestExec(entry, dir), now)
	if err != nil {
		t.Fatal(err)
	}
	entry.GuestTotal, entry.GuestAvailable = reading.GuestTotal, reading.GuestAvailable
	entry.NestedShort, entry.NestedPressure, entry.NestedStart = reading.NestedShort, reading.NestedPressure, reading.NestedStart
	return entry
}

// publishStart lets cage.PrepareBoot write its request for a machine of entry
// and returns at its first wait.
func publishStart(t *testing.T, entry InstanceMemory, maximum uint64, now time.Time) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	go func() {
		defer close(stop)
		_, _ = cage.PrepareBoot(ctx, cage.BootRequest{Profile: dir, Maximum: maximum,
			Machine: func() (cage.BootMachine, bool) { return bootMachineOf(entry), true }, Growable: func() bool { return true },
			Now: func() time.Time { return now }, Sleep: func(ctx context.Context, d time.Duration) error { <-ctx.Done(); return ctx.Err() }})
	}()
	path := filepath.Join(dir, "machine-memory.json")
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			saved := filepath.Join(t.TempDir(), "machine-memory.json")
			if err := os.WriteFile(saved, data, 0o600); err != nil {
				t.Fatal(err)
			}
			cancel()
			<-stop
			return filepath.Dir(saved)
		}
	}
	t.Fatal("the start published nothing")
	return ""
}

func TestAllocationGrowsAStoppedVMForItsStart(t *testing.T) {
	// The incident: the VM at the guarantee, its environment stopped and idle
	// (nobody marks it active), a 7 GiB guest to start. The bytes the start
	// writes go through the gateway's parser: one round grows the VM so that
	// the kernel maps the guest (MemTotal >= 7 GiB + 256 MiB).
	id := vmID(1)
	now := time.Unix(1e9, 0)
	entry := stoppedVM(2048 * testMiB)
	dir := publishStart(t, entry, 7<<30, now)
	entry = readThrough(t, entry, dir, now.Add(5*time.Second))
	if entry.NestedShort != 6912*testMiB || !entry.NestedPressure || !entry.NestedStart {
		t.Fatalf("reading %+v", entry)
	}
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 24 * testGiB, Ceiling: 9728 * testMiB, Instances: map[string]InstanceMemory{id: entry}}
	result := allocate(state, map[string]demandState{}, now.Add(5*time.Second))
	if len(result.changes) != 1 || result.changes[0].reason != "environment start needs memory" {
		t.Fatalf("changes %+v", result.changes)
	}
	grown := result.changes[0].to
	if memTotal := grown - 310*testMiB; memTotal < 7*testGiB+256*testMiB || grown > 9728*testMiB {
		t.Fatalf("grown to %d MiB: MemTotal %d MiB does not map the guest", grown>>20, memTotal>>20)
	}
	t.Logf("one round: 2048 -> %d MiB (MemTotal %d MiB for a 7168 MiB guest)", grown>>20, (grown-310*testMiB)>>20)

	// A request older than 45 s (a start that crashed) is not read at all.
	if stale := readThrough(t, stoppedVM(2048*testMiB), dir, now.Add(50*time.Second)); stale.NestedShort != 0 || stale.NestedStart {
		t.Fatalf("stale request read: %+v", stale)
	}
}

func TestAllocationStartOnAHostShortOfMemory(t *testing.T) {
	// 16 GiB host, reserve 2.4 GiB, 1.5 GiB free: the start's VM grows by
	// what is free at once and the rest follows as an idle neighbour above
	// its fair share gives back, one step per round.
	start, idle := vmID(1), vmID(2)
	now := time.Unix(1e9, 0)
	entry := stoppedVM(2048 * testMiB)
	entry.NestedShort, entry.NestedPressure, entry.NestedStart = 6912*testMiB, true, true
	state := MemoryState{HostTotal: 16 * testGiB, HostAvailable: 1536 * testMiB, Ceiling: 9728 * testMiB, Instances: map[string]InstanceMemory{
		start: entry, idle: guestVM(10*testGiB, testGiB, 0)}}
	held := map[string]demandState{}
	var first int64
	rounds := 0
	for ; rounds < 40 && state.Instances[start].Limit < 8960*testMiB; rounds++ {
		result := allocate(state, held, now)
		held = result.held
		for _, change := range result.changes {
			applyChange(&state, change)
			if change.id == idle {
				// What the neighbour gives back is free on the host.
				state.HostAvailable += change.from - change.to
			} else {
				state.HostAvailable -= change.to - change.from
				if first == 0 {
					first = change.to
				}
			}
		}
		now = now.Add(15 * time.Second)
	}
	if first <= 2048*testMiB || first >= 4*testGiB || state.Instances[start].Limit < 8960*testMiB {
		t.Fatalf("first grant %d MiB, after %d rounds %d MiB", first>>20, rounds, state.Instances[start].Limit>>20)
	}
	t.Logf("short host: first round 2048 -> %d MiB, %d rounds (%s) to %d MiB", first>>20, rounds, time.Duration(rounds)*15*time.Second, state.Instances[start].Limit>>20)
}

func TestAllocationStartUnderHostPressure(t *testing.T) {
	// The host is in pressure (tasks wait for memory): only a VM read as short
	// grows. The start sets pressure, so it does; without the flag it would
	// wait (the 0.1.16 gateway dropped the flag between reading and the rule).
	for _, pressure := range []bool{true, false} {
		id := vmID(1)
		entry := stoppedVM(2048 * testMiB)
		entry.NestedShort, entry.NestedPressure, entry.NestedStart = 6912*testMiB, pressure, true
		state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, HostStall: 15, Ceiling: 9728 * testMiB, Instances: map[string]InstanceMemory{id: entry}}
		result := allocate(state, map[string]demandState{}, time.Unix(1e9, 0))
		if grew := len(result.changes) == 1 && result.changes[0].to > entry.Limit; grew != pressure || !result.pressure {
			t.Fatalf("pressure flag %v: changes %+v (host pressure %v)", pressure, result.changes, result.pressure)
		}
	}
}

func TestAllocationStartRequestExpires(t *testing.T) {
	// The start was abandoned: the request is gone, the environment stayed
	// stopped and nobody uses the workspace. The held demand keeps the VM for
	// memoryDemandHold, then it returns to the guarantee one step per round.
	id := vmID(1)
	now := time.Unix(1e9, 0)
	entry := stoppedVM(2048 * testMiB)
	entry.NestedShort, entry.NestedPressure, entry.NestedStart = 6912*testMiB, true, true
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 24 * testGiB, Ceiling: 9728 * testMiB, Instances: map[string]InstanceMemory{id: entry}}
	result := allocate(state, map[string]demandState{}, now)
	held := result.held
	applyChange(&state, result.changes[0])
	grown := state.Instances[id].Limit
	state.Instances[id] = stoppedVM(grown)
	var heldFor, back time.Duration
	for elapsed := 15 * time.Second; elapsed <= 30*time.Minute; elapsed += 15 * time.Second {
		result := allocate(state, held, now.Add(elapsed))
		held = result.held
		for _, change := range result.changes {
			if change.to > change.from || change.reason != "idle memory returned" || change.from-change.to > memoryShrinkStep {
				t.Fatalf("at %s: %+v", elapsed, change)
			}
			if heldFor == 0 {
				heldFor = elapsed
			}
			state.Instances[id] = stoppedVM(change.to)
		}
		// Idle return stops within one step of the guarantee (hysteresis).
		if state.Instances[id].Limit < 2048*testMiB+memoryShrinkStep {
			back = elapsed
			break
		}
	}
	if heldFor < memoryDemandHold || back == 0 || back > memoryDemandHold+20*15*time.Second {
		t.Fatalf("held %s, back at the guarantee after %s", heldFor, back)
	}
	t.Logf("abandoned start: %d MiB held %s, back to %d MiB after %s", grown>>20, heldFor, state.Instances[id].Limit>>20, back)
}

func TestAllocationRunningGuestKeepsWhatItNeeds(t *testing.T) {
	// The environment runs: the VM's working set holds the guest's resident
	// memory (QEMU's anonymous memory) and its host agent keeps the guest at
	// its balloon target. An idle VM gives back what lies above its working
	// set plus 512 MiB, never more, whatever the guest's boot maximum: the
	// boot maximum was mapped at start and the balloon follows the machine.
	id := vmID(1)
	now := time.Unix(1e9, 0)
	working := int64(538+3200) * testMiB // own use + a guest holding 3.2 GiB
	state := MemoryState{HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, Ceiling: 9728 * testMiB, Instances: map[string]InstanceMemory{id: guestVM(8960*testMiB, working, 0)}}
	held := map[string]demandState{}
	for range 120 {
		result := allocate(state, held, now)
		held = result.held
		for _, change := range result.changes {
			applyChange(&state, change)
			entry := state.Instances[id]
			entry.GuestTotal = entry.Limit - 200*testMiB
			entry.GuestAvailable = entry.GuestTotal - working
			state.Instances[id] = entry
		}
		now = now.Add(15 * time.Second)
	}
	limit := state.Instances[id].Limit
	// The floor: working set + kernel memory outside MemTotal + 512 MiB.
	if floor := working + 200*testMiB + memoryMinHeadroom; limit < floor || limit > roundUpMemory(working+200*testMiB+max(memoryHeadroom, (working+200*testMiB)/4))+memoryShrinkStep {
		t.Fatalf("running guest's VM settled at %d MiB (working %d MiB)", limit>>20, working>>20)
	}
	// The guest wants 2 GiB more than the machine backs: it grows at once.
	entry := state.Instances[id]
	entry.NestedShort, entry.NestedPressure = 2*testGiB, true
	state.Instances[id] = entry
	if result := allocate(state, held, now); len(result.changes) != 1 || result.changes[0].to < limit+2*testGiB || result.changes[0].reason != "nested guest needs memory" {
		t.Fatalf("nested shortage: %+v", result.changes)
	}
}

func TestIncusMemoryStatePassesTheNestedRequest(t *testing.T) {
	// The reading's nested fields reach the allocation, the pressure flag
	// included.
	dir := t.TempDir()
	running := strings.Repeat("1", 32)
	oldMeminfo, oldPressure := hostMeminfo, hostMemoryPressure
	t.Cleanup(func() { hostMeminfo, hostMemoryPressure = oldMeminfo, oldPressure })
	hostMeminfo = writeTestFile(t, filepath.Join(dir, "meminfo"), "MemTotal: 33554432 kB\nMemAvailable: 25165824 kB\n", 0600)
	hostMemoryPressure = filepath.Join(dir, "none")
	nested := `{"version":1,"updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `","short_bytes":7247757312,"wanted_bytes":7516192768,"target_bytes":0,"pressure":true,"reason":"start"}`
	writeTestFile(t, filepath.Join(dir, "guest"), "MemTotal: 1779712 kB\nMemAvailable: 1228800 kB\n#cage-psi\n#cage-vmstat\n#cage-nested\n"+nested+"\n", 0600)
	writeTestFile(t, filepath.Join(dir, "inventory"), `[{"name":"cage-`+running+`","type":"virtual-machine","status":"Running","config":{"user.cage-tenant.id":"`+running+`"},"expanded_config":{"limits.memory":"2048MiB"}}]`, 0600)
	runtime, _ := fakeIncus(t, `"--force-local query local:/1.0/projects/cage-tenant") echo '{"name":"cage-tenant","config":{"features.profiles":"true"}}' ;;
"--force-local query local:/1.0/profiles/default?project=cage-tenant") echo '{"name":"default","config":{"limits.cpu":"4","limits.memory":"9728MiB"},"devices":{"root":{"type":"disk","path":"/","pool":"tenant-pool","size":"10GiB"}}}' ;;
"--force-local query local:/1.0/instances?recursion=1&project=cage-tenant") cat '`+filepath.Join(dir, "inventory")+`' ;;
"--force-local --project cage-tenant exec local:cage-`+running+` --mode=non-interactive -- /bin/sh -c "*" sh /home/cage/.claude-cage/machine-memory.json") cat '`+filepath.Join(dir, "guest")+`' ;;`)
	state, err := runtime.MemoryState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if entry := state.Instances[running]; entry.NestedShort != 7247757312 || !entry.NestedPressure || !entry.NestedStart {
		t.Fatalf("nested request lost: %+v", entry)
	}
}
