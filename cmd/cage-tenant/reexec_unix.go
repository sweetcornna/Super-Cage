//go:build unix

package main

import "syscall"

func reexecSelf(binary string, argv, envv []string) error {
	return syscall.Exec(binary, argv, envv)
}
