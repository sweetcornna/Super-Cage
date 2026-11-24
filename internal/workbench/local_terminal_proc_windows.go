//go:build windows

package workbench

import (
	"os/exec"
	"time"
)

func configureLocalTermProcGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 3 * time.Second
}
