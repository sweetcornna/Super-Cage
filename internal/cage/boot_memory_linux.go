package cage

// onlineMemory is the memory the kernel has online in hotplug blocks
// (machine_linux.go), which a balloon does not change.
func onlineMemory() uint64 { return memoryBlocks() }
