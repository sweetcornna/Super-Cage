//go:build !windows

package localterm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// ExecRunner runs steps for real. Only the desktop's "在本机终端中打开" uses it.
type ExecRunner struct{}

// stepWait bounds a step; promptWait an AppleScript step, which waits while
// the owner reads macOS's Automation prompt.
var stepWait, promptWait = 20 * time.Second, 2 * time.Minute

func (ExecRunner) Run(ctx context.Context, step Step) error {
	if len(step.Argv) == 0 {
		return errors.New("empty step")
	}
	wait := stepWait
	if step.Prompt {
		wait = promptWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, step.Argv[0], step.Argv[1:]...)
	cmd.Env = LaunchEnvironment(os.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		if step.Prompt {
			return appleScriptError(output, ctx.Err() != nil, err)
		}
		return errors.New(firstLine(output, err))
	}
	return nil
}
