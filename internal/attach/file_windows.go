//go:build windows

package attach

import "os"

// openNoFollow opens a dropped file for reading. Windows has no O_NOFOLLOW;
// the caller's Lstat refuses links and reparse points before this, and
// compares the file opened with it.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
