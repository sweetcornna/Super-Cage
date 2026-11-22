//go:build !windows

package workbench

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// entryFileRefusal says why Super Cage must not replace the file at path
// (N5 of the second review): another user owns it, or this user cannot
// write it, as access(2) answers, which counts the mode, an immutable flag
// (chflags uchg, chattr +i) and a read-only file system.
func entryFileRefusal(path string, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != entryCurrentUID() {
		return errEntryConfigNotOwned
	}
	if unix.Access(path, unix.W_OK) != nil {
		return errEntryConfigReadOnly
	}
	return nil
}
