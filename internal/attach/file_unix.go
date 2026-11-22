//go:build !windows

package attach

import (
	"os"
	"syscall"
)

// openNoFollow opens a dropped file for reading: not through a symbolic
// link, and without blocking on a FIFO or a device.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
