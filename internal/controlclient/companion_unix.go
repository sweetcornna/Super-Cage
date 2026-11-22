//go:build !windows

package controlclient

import (
	"os"
	"syscall"
)

func fileInode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
