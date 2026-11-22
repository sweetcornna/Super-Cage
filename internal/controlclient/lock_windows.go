package controlclient

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Lock takes the controller lock without waiting.
func Lock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
}

// Unlock releases the controller lock.
func Unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// LockContended reports whether Lock failed because another process holds it.
func LockContended(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
