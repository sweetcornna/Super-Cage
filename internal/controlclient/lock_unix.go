//go:build !windows

package controlclient

import (
	"errors"
	"os"
	"syscall"
)

// Lock takes the controller lock without waiting.
func Lock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }

// Unlock releases the controller lock.
func Unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// LockContended reports whether Lock failed because another process holds it.
func LockContended(err error) bool { return errors.Is(err, syscall.EWOULDBLOCK) }
