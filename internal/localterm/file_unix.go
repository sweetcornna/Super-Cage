//go:build !windows

package localterm

import (
	"os"
	"syscall"
)

// openNoFollow refuses a link as the file opened.
const openNoFollow = syscall.O_NOFOLLOW

// ownedByMe reports whether this user owns a file.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
