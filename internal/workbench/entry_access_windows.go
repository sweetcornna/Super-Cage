package workbench

import "os"

// entryFileRefusal says why Super Cage must not replace the file at path:
// Windows has the read-only attribute; the file's ACL decides the rest,
// and a write it refuses fails without changing the file.
func entryFileRefusal(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o200 == 0 {
		return errEntryConfigReadOnly
	}
	return nil
}
