package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// execAttach runs cage-attach in this console and returns its exit code.
// Windows cannot replace a process, so cage waits; Ctrl+C belongs to
// cage-attach, which detaches on it.
func execAttach(path string, args []string) (int, bool) {
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		return 0, false
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	if err != nil {
		return 1, true
	}
	return 0, true
}
