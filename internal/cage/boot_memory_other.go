//go:build !linux

package cage

// onlineMemory is unknown off Linux; PrepareBoot measures only Linux.
func onlineMemory() uint64 { return 0 }
