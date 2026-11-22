//go:build !windows

package workbench

import (
	"io"
	"os"
)

// entryOpenTTY opens the controlling terminal, where cage entry on asks.
func entryOpenTTY() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
