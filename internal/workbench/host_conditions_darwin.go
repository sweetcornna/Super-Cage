//go:build darwin

package workbench

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
)

// hostConditions reads this Mac's power and display state for an
// unattended decision: four short commands, only in silent mode, only when
// idle has already held for an hour.
func hostConditions(ctx context.Context) (*policy.HostState, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var outputs [4][]byte
	for i, command := range [][]string{
		{"/usr/bin/pmset", "-g", "systemstate"},
		{"/usr/bin/pmset", "-g", "batt"},
		{"/usr/bin/pmset", "-g", "assertions"},
		{"/usr/sbin/ioreg", "-r", "-n", "IOPMrootDomain", "-d", "1", "-k", "AppleClamshellState"},
	} {
		out, err := exec.CommandContext(ctx, command[0], command[1:]...).Output()
		if err != nil {
			return nil, err
		}
		outputs[i] = out
	}
	return parseHostConditions(outputs[0], outputs[1], outputs[2], outputs[3], map[int]bool{os.Getpid(): true})
}
