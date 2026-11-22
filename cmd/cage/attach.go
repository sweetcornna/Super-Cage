package main

import (
	"os"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/version"
)

// runAttach shows a Cage terminal in the terminal this command runs in. The
// slim cage-attach installed beside cage does it, with the same flags and
// exit codes, so that an attached window does not keep all of cage
// resident; without it (a source build), cage attaches itself.
func runAttach(args []string) int {
	if exe, err := os.Executable(); err == nil {
		if slim := attach.Beside(exe); slim != "" {
			if code, ok := execAttach(slim, args); ok {
				return code
			}
		}
	}
	// In cage itself, the controller must be this very build, as for cage agent.
	build, _ := version.ExecutableIdentity()
	return attach.Main(args, os.Stdin, os.Stdout, os.Stderr, attach.Options{Check: controlclient.SameBuild(build)})
}
