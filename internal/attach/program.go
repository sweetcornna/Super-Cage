package attach

import (
	"os"
	"path/filepath"
	"runtime"
)

// ProgramName is the file name of the slim cage-attach program.
func ProgramName() string {
	if runtime.GOOS == "windows" {
		return "cage-attach.exe"
	}
	return "cage-attach"
}

// Beside returns the cage-attach installed in the same directory as the
// executable exe, after resolving symbolic links, or "" when there is none.
// Packages install it next to cage (see cmd/cage-attach).
func Beside(exe string) string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	path := filepath.Join(filepath.Dir(exe), ProgramName())
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}
