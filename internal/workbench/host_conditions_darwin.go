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
	return readHostConditions(func(command []string) ([]byte, error) {
		return exec.CommandContext(ctx, command[0], command[1:]...).Output()
	}, map[int]bool{os.Getpid(): true})
}
