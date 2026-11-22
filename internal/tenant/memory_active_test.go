package tenant

import (
	"strings"
	"testing"
	"time"
)

// Two VMs at their 10 GiB ceiling on a 16 GiB host that is not short of
// memory (8 GiB available, 2 GiB reserve): the fair share is 7 GiB each, so
// both are above it. The guests use 1 GiB.
func twoVMsAboveTheirShare() (MemoryState, string, string) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	return MemoryState{HostTotal: 16 * testGiB, HostAvailable: 8 * testGiB, Ceiling: 10 * testGiB, Instances: map[string]InstanceMemory{
		a: runningVM(10*testGiB, 9890*testMiB, 8890*testMiB),
		b: runningVM(10*testGiB, 9890*testMiB, 8890*testMiB),
	}}, a, b
}

func markActive(state MemoryState, ids ...string) {
	for _, id := range ids {
		entry := state.Instances[id]
		entry.Active = true
		state.Instances[id] = entry
	}
}

func changedIDs(changes []memoryChange) map[string]memoryChange {
	result := make(map[string]memoryChange)
	for _, change := range changes {
		result[change.id] = change
	}
	return result
}

func TestMemoryPlanFairShareSparesTheVMInUse(t *testing.T) {
	state, a, b := twoVMsAboveTheirShare()
	if changes := changedIDs(planMemory(state, 2*testGiB, DefaultMemoryMin, nil, nil, nil)); len(changes) != 2 {
		t.Fatalf("with both idle the fair share must shrink both: %+v", changes)
	}
	markActive(state, a)
	changes := changedIDs(planMemory(state, 2*testGiB, DefaultMemoryMin, nil, nil, nil))
	if _, shrunk := changes[a]; shrunk || len(changes) != 1 || changes[b].reason != "fair share of the host" {
		t.Fatalf("the VM in use must keep its memory while the idle one gives back: %+v", changes)
	}
	markActive(state, b)
	if changes := planMemory(state, 2*testGiB, DefaultMemoryMin, nil, nil, nil); len(changes) != 0 {
		t.Fatalf("no VM is idle and the host is not short: nothing to shrink, got %+v", changes)
	}
}

func TestMemoryPlanShrinksTheVMInUseWhenTheHostIsShort(t *testing.T) {
	state, a, b := twoVMsAboveTheirShare()
	markActive(state, a, b)
	state.HostAvailable = 3 * testGiB // below twice the reserve
	changes := changedIDs(planMemory(state, 2*testGiB, DefaultMemoryMin, nil, nil, nil))
	if len(changes) != 2 || changes[a].reason != "host memory is low" || changes[b].reason != "host memory is low" {
		t.Fatalf("under pressure every VM gives back its cache, in use or not: %+v", changes)
	}
	state.HostAvailable, state.HostStall = 8*testGiB, 12
	if changes := planMemory(state, 2*testGiB, DefaultMemoryMin, nil, nil, nil); len(changes) != 2 {
		t.Fatalf("memory stall is pressure too: %+v", changes)
	}
}

func TestMemoryPlanStillGrowsTheVMInUse(t *testing.T) {
	state, a, _ := twoVMsAboveTheirShare()
	entry := state.Instances[a]
	entry.Limit, entry.GuestTotal, entry.GuestAvailable, entry.Active = 3*testGiB, 2900*testMiB, 100*testMiB, true
	state.Instances[a] = entry
	changes := changedIDs(planMemory(state, 2*testGiB, DefaultMemoryMin, nil, nil, nil))
	if changes[a].reason != "guest needs memory" || changes[a].to <= 3*testGiB {
		t.Fatalf("a short VM in use must grow: %+v", changes)
	}
}

func TestTenantMemoryBalancerSparesAWorkspaceInUse(t *testing.T) {
	base := newFixture(t, false)
	base.server.Close()
	runtime := &memoryRuntime{fakeRuntime: base.runtime, elastic: true}
	base.config.Runtime, base.config.MemoryInterval = runtime, 10*time.Minute
	base.openServer()
	login, _ := base.login("register", "alice")
	record := base.create(login, "first")
	base.connect(login, record.ID)
	// An 8 GiB host with 5 GiB available: not short (twice the 2 GiB reserve is
	// 4 GiB), but the one VM at its 10 GiB ceiling is above the 6 GiB it may have.
	runtime.state = MemoryState{HostTotal: 8 * testGiB, HostAvailable: 5 * testGiB, Ceiling: 10 * testGiB, Instances: map[string]InstanceMemory{
		record.ID: runningVM(10*testGiB, 9890*testMiB, 8890*testMiB),
	}}
	// The workspace was just opened and used: it keeps its memory although it
	// is above its fair share.
	base.server.balanceOnce()
	if changes := runtime.changes(); len(changes) != 0 {
		t.Fatalf("a workspace in use was shrunk for its fair share: %+v", changes)
	}
	// A minute and more later without use it gives back.
	base.server.mu.Lock()
	base.server.now = func() time.Time { return time.Now().Add(memoryActiveWindow + time.Second) }
	base.server.mu.Unlock()
	base.server.balanceOnce()
	if changes := runtime.changes(); len(changes) != 1 || changes[0].to >= 10*testGiB {
		t.Fatalf("an idle workspace above its share must shrink: %+v", changes)
	}
	// Host memory running short shrinks even a workspace in use.
	base.server.mu.Lock()
	base.server.now = time.Now
	base.server.mu.Unlock()
	runtime.mu.Lock()
	runtime.state.HostAvailable = 2 * testGiB
	runtime.mu.Unlock()
	base.server.balanceOnce()
	if changes := runtime.changes(); len(changes) != 2 {
		t.Fatalf("pressure must shrink a workspace in use: %+v", changes)
	}
}
