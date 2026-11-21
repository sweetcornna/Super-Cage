package main

import (
	"os"

	"github.com/sylvinus/agent-vm/internal/workbench"
)

// runAttach shows a Cage terminal in the terminal this command runs in.
func runAttach(args []string) int {
	return workbench.AttachMain(args, os.Stdin, os.Stdout, os.Stderr)
}
