package controlclient

import "os"

// fileInode has no cheap equivalent on Windows; a running program cannot be
// replaced there, and the size and time still apply.
func fileInode(os.FileInfo) uint64 { return 0 }
