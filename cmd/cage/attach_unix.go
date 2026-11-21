//go:build !windows

package main

import (
	"os"
	"syscall"
)

// execAttach replaces this process with cage-attach: the window keeps one
// small process, and its exit code is cage-attach's. It returns only when
// the program could not be started.
func execAttach(path string, args []string) (int, bool) {
	_ = syscall.Exec(path, append([]string{path}, args...), os.Environ())
	return 0, false
}
