package controlclient

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/host"
)

// OpenLock opens a profile's controller lock file, refusing anything but a
// regular file that stays the same file while it is opened.
func OpenLock(profile string, flags int) (*os.File, error) {
	path := filepath.Join(profile, "control.lock")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("控制锁必须是普通文件")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	opened, statErr := f.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		f.Close()
		return nil, errors.New("控制锁路径已变化，请重试")
	}
	return f, nil
}

// LockStatus reports whether no controller holds profile's lock.
func LockStatus(profile string) (free bool, err error) {
	f, err := OpenLock(profile, os.O_RDWR)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	defer f.Close()
	if err := Lock(f); err != nil {
		if LockContended(err) {
			return false, nil
		}
		return false, err
	}
	defer Unlock(f)
	// Repair legacy ownership only while holding the same lock that protects
	// record publication. Never unlink a live lock or relax its permissions.
	if err := host.ProtectFile(f.Name()); err != nil {
		return false, err
	}
	if err := host.CheckPrivateFile(f.Name()); err != nil {
		return false, err
	}
	return true, nil
}

// LockFree is LockStatus for callers that treat errors as "held".
func LockFree(profile string) bool {
	free, err := LockStatus(profile)
	return free && err == nil
}
