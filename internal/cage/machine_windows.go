package cage

import "github.com/pbnjay/memory"

// Windows reports free memory only; a fifth free is the early warning,
// a tenth free host.MemoryPressure.
func platformMachine() MachineMemory {
	total := memory.TotalMemory()
	return MachineMemory{Total: total, Boot: total, Elevated: total > 0 && memory.FreeMemory() < total/5}
}

func balloonBytes() uint64 { return 0 }
