package workbench

import (
	"os"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// The lock contains no credentials. Old Windows versions created it with the
// Administrators group as owner. Test the OS lock before repairing its ACL;
// credential-file ownership must not be used as a process-liveness test.
func openControllerLock(profile string, flags int) (*os.File, error) {
	return controlclient.OpenLock(profile, flags)
}

// A status probe can briefly own the lock while it checks or repairs its ACL.
// Let that probe finish before deciding that another controller owns the profile.
func lockControllerForStart(f *os.File) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := lockController(f)
		if err == nil || !controllerLockContended(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func controllerLockStatus(profile string) (free bool, err error) {
	return controlclient.LockStatus(profile)
}

func controllerLockFree(profile string) bool { return controlclient.LockFree(profile) }
