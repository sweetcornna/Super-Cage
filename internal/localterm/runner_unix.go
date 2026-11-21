//go:build !windows

package localterm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ExecRunner runs steps for real. Only the desktop's "在本机终端中打开" uses it.
type ExecRunner struct{}

// detachWait is how long a detached step must keep running to count as started.
var detachWait = 3 * time.Second

func (ExecRunner) Run(ctx context.Context, step Step) error {
	if len(step.Argv) == 0 {
		return errors.New("empty step")
	}
	if !step.Detach {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, step.Argv[0], step.Argv[1:]...)
		cmd.Env = LaunchEnvironment(os.Environ())
		output, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(firstLine(output, err))
		}
		return nil
	}
	cmd := exec.Command(step.Argv[0], step.Argv[1:]...)
	cmd.Env = LaunchEnvironment(os.Environ())
	// Its own session: the application outlives Super Cage.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		return err
	case <-time.After(detachWait):
		return nil
	case <-ctx.Done():
		return nil
	}
}
