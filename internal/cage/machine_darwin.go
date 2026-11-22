package cage

import (
	"github.com/pbnjay/memory"
	"golang.org/x/sys/unix"
)

// macOS has no reliable "available" figure; the kernel's pressure level is
// the signal: 2 is warn, 4 critical (host.MemoryPressure).
func platformMachine() MachineMemory {
	total := memory.TotalMemory()
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	return MachineMemory{Total: total, Boot: total, Elevated: err == nil && level >= 2}
}

func balloonBytes() uint64 { return 0 }
