package cage

import (
	"os"
	"runtime"

	"github.com/sylvinus/agent-vm/internal/host"
)

// The machine is what runs this host agent and the guest's VM: the user's
// computer for the desktop edition, the workspace VM for the tenant edition.
// See resource-allocation.md, level B.

// MachineMemory is one reading of the machine's own memory.
type MachineMemory struct {
	// Total is the memory the machine has now. Inside a ballooned VM it is
	// smaller than Boot.
	Total uint64 `json:"total_bytes"`
	// Boot is what the machine was given at boot (installed RAM on a
	// computer): what it can become again.
	Boot uint64 `json:"boot_bytes"`
	// Available is measured on Linux (Measured); macOS and Windows have no
	// reliable figure, and their pressure signals are used instead.
	Available uint64 `json:"available_bytes,omitempty"`
	Measured  bool   `json:"available_measured,omitempty"`
	// Reserve is what automatic sizing leaves the machine for itself.
	Reserve uint64 `json:"reserve_bytes"`
	// Pressure is the platform's memory pressure (macOS critical, Linux and
	// Windows below a tenth available); Elevated an earlier warning (macOS
	// warn, Windows below a fifth).
	Pressure bool   `json:"pressure"`
	Elevated bool   `json:"elevated,omitempty"`
	Role     string `json:"role"`
}

// readMachine reads the machine; tests replace it.
var readMachine = func() MachineMemory {
	reading := platformMachine()
	reading.Pressure = host.MemoryPressure()
	reading.Role = machineRole()
	reading.Reserve = machineReserve(reading.Role, reading.Boot)
	return reading
}

// The two machine roles. A dedicated machine runs little besides Cage: the
// tenant workspace VM, a server. A desktop is someone's computer.
const (
	RoleDedicated = "dedicated"
	RoleDesktop   = "desktop"
)

// machineRole is CAGE_MACHINE_ROLE when set, desktop on macOS and Windows and
// on Linux with a graphical session, and dedicated otherwise.
func machineRole() string {
	switch os.Getenv("CAGE_MACHINE_ROLE") {
	case RoleDedicated:
		return RoleDedicated
	case RoleDesktop:
		return RoleDesktop
	}
	if runtime.GOOS != "linux" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return RoleDesktop
	}
	return RoleDedicated
}

const gib = uint64(1 << 30)

// machineReserve is what the machine keeps for itself: its own processes,
// the VM's overhead and some page cache (dedicated), or the user's own
// applications too (desktop).
func machineReserve(role string, boot uint64) uint64 {
	if role == RoleDedicated {
		return max(gib, boot/8)
	}
	return max(4*gib, boot/4)
}

// machineBootGiB is the machine's boot memory in whole GiB: host.MemGiB, which
// tests replace, without an outer balloon; with one, the boot size of the
// Linux reading, rounded down once, so that the balloon moving memory
// between MemTotal and itself never moves the result.
func machineBootGiB() int {
	if balloonBytes() == 0 {
		return host.MemGiB()
	}
	return int(platformMachine().Boot >> 30)
}

// bootBytes is a ballooned machine's boot memory: MemTotal plus the balloon
// on kernels that take ballooned pages out of MemTotal, MemTotal alone on
// kernels that keep it and count ballooned pages as used (the balloon
// negotiated deflate-on-oom). keeps is that feature as the balloon driver
// reports it; without it (known false) the memory blocks the kernel has
// online tell (zero when unknown): MemTotal of an unballooned kernel is below
// them, so the sum reaches them only when MemTotal kept the ballooned pages.
func bootBytes(total, balloon, blocks uint64, keeps, known bool) uint64 {
	switch {
	case known && keeps, !known && blocks > 0 && total+balloon >= blocks:
		return total
	}
	return total + balloon
}

// AutomaticLimits is the automatic allocation of a ballooned Linux guest: the
// machine's boot memory less its reserve, whole GiB, at least 2 and at most
// AutomaticMemoryCapGiB; vCPUs: all but one on a dedicated machine, all but a
// quarter (at least two) on a desktop. The balloon target, not this ceiling,
// decides what the guest holds.
func AutomaticLimits() (memoryGiB, cpus int) {
	return automaticLimits(machineRole(), uint64(max(machineBootGiB(), 0))<<30, host.CPUs())
}

// automaticLimits is AutomaticLimits for a machine of role with boot bytes
// of memory and n CPUs.
func automaticLimits(role string, boot uint64, n int) (memoryGiB, cpus int) {
	memoryGiB = 4
	if boot > 0 {
		memoryGiB = int((boot - min(boot, machineReserve(role, boot))) >> 30)
	}
	memoryGiB = min(max(memoryGiB, 2), AutomaticMemoryCapGiB)
	if n <= 0 {
		return memoryGiB, 2
	}
	if role == RoleDedicated {
		return memoryGiB, max(1, n-1)
	}
	return memoryGiB, min(n, max(2, n-max(2, n/4)))
}

// legacyAutomaticLimits is the automatic allocation before machine-following
// sizing on a machine of memoryGiB and cpus as host.MemGiB and host.CPUs report
// them: three eighths of the memory within the host share (half by default),
// and the CPU share. macOS guests keep it, and it identifies a saved old
// default.
func legacyAutomaticLimits(memoryGiB, cpus int) (int, int) {
	div, _, _ := host.Divisor()
	share, shareCPUs := 4, 2
	if memoryGiB > 0 {
		share = min(memoryGiB, max(memoryGiB/div, 2))
	}
	if cpus > 0 {
		shareCPUs = min(cpus, max(cpus/div, 1))
	}
	return min(share, max(AutomaticMemoryFloorGiB, memoryGiB*3/8), AutomaticMemoryCapGiB), shareCPUs
}
