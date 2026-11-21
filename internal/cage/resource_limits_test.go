package cage

import (
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/host"
)

// The manual range follows the machine's role, which follows DISPLAY and
// WAYLAND_DISPLAY: a value a headless cage serve (dedicated) saved can be
// above what the desktop app (desktop) allows on the same machine. Starting
// clamps it with a notice; only choosing a value checks it.
func TestSavedAllocationOutsideTheRangeNeverRefusesAStart(t *testing.T) {
	oldMem, oldCPU := host.MemGiB, host.CPUs
	t.Cleanup(func() { host.MemGiB, host.CPUs = oldMem, oldCPU })
	host.MemGiB = func() int { return 16 }
	host.CPUs = func() int { return 10 }
	t.Setenv("AGENT_VM_HOST_SHARE", "2")
	t.Setenv("CAGE_MACHINE_ROLE", RoleDedicated)
	dedicatedMemory, dedicatedCPUs := ManualLimitRange()
	saved := ResourceAllocation{MemoryGiB: dedicatedMemory, CPUs: dedicatedCPUs, Chosen: OriginUser}
	if err := saved.Validate(); err != nil {
		t.Fatalf("chosen within the headless range: %v", err)
	}
	t.Setenv("CAGE_MACHINE_ROLE", RoleDesktop)
	desktopMemory, desktopCPUs := ManualLimitRange()
	if desktopMemory >= dedicatedMemory {
		t.Fatalf("ranges: dedicated %d, desktop %d GiB", dedicatedMemory, desktopMemory)
	}
	memory, cpus, err := saved.Limits()
	if err != nil || memory != desktopMemory || cpus != desktopCPUs {
		t.Fatalf("desktop start with %d GiB/%d saved: %d/%d %v", saved.MemoryGiB, saved.CPUs, memory, cpus, err)
	}
	if notice := saved.LimitsNotice(); !strings.Contains(notice, "超出这台机器当前允许的范围") {
		t.Fatalf("notice %q", notice)
	}
	if saved.Validate() == nil {
		t.Fatal("a new choice above the desktop range was accepted")
	}
	// Below the range too, and nothing to say when the saved value applies.
	if memory, cpus, err := (ResourceAllocation{MemoryGiB: 1, CPUs: 0}).Limits(); err != nil || memory != 2 || cpus != 1 {
		t.Fatalf("below the range: %d/%d %v", memory, cpus, err)
	}
	if notice := (ResourceAllocation{MemoryGiB: 4, CPUs: 2}).LimitsNotice(); notice != "" {
		t.Fatalf("notice for an allocation in range: %q", notice)
	}
	if notice := (ResourceAllocation{Automatic: true}).LimitsNotice(); notice != "" {
		t.Fatalf("notice for automatic: %q", notice)
	}
}
