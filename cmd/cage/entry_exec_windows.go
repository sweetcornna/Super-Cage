//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
)

// execSSH runs ssh as a child and returns its exit status; Ctrl+C belongs to
// ssh while it runs.
func execSSH(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Super Cage：无法启动 ssh："+err.Error())
		return 255
	}
	return 0
}
