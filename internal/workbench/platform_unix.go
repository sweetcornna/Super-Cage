//go:build !windows

package workbench

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

func lockController(f *os.File) error        { return controlclient.Lock(f) }
func unlockController(f *os.File) error      { return controlclient.Unlock(f) }
func controllerLockContended(err error) bool { return controlclient.LockContended(err) }
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err != syscall.ESRCH
}
func terminateLegacy(pid int) error  { return syscall.Kill(pid, syscall.SIGTERM) }
func detachController(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func sshExecutable() string          { return sshProgram }

// sshProgram is the system ssh; the tests put a shim in its place
// (ssh_shim_test.go).
var sshProgram = "/usr/bin/ssh"

func sshEnvironment() []string {
	return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C", "LC_ALL=C", "TERM=xterm-256color"}
}
func defaultKeyboard() string {
	if runtime.GOOS == "darwin" {
		return "mac"
	}
	return "native"
}
