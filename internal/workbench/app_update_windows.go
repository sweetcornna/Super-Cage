//go:build windows

package workbench

import "os"

// dirWritable creates and removes one file: Windows ACLs are not visible to
// a permission-bit check.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".cage-update-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	return os.Remove(name) == nil
}
