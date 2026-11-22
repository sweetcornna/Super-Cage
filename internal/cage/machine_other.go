//go:build !linux && !darwin && !windows

package cage

import "github.com/pbnjay/memory"

func platformMachine() MachineMemory {
	total := memory.TotalMemory()
	return MachineMemory{Total: total, Boot: total}
}

func balloonBytes() uint64 { return 0 }
