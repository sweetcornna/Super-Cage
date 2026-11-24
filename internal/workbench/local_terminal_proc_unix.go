//go:build !windows

package workbench

import (
	"os/exec"
	"syscall"
	"time"
)

func configureLocalTermProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
