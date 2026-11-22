//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// execSSH replaces this process with ssh, so the terminal's signals and
// exit status are ssh's own.
func execSSH(argv []string) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Super Cage：找不到 ssh")
		return 255
	}
	err = syscall.Exec(path, argv, os.Environ())
	fmt.Fprintln(os.Stderr, "Super Cage：无法启动 ssh："+err.Error())
	return 255
}
