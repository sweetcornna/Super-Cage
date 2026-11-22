//go:build !windows

package workbench

import "golang.org/x/sys/unix"

func dirWritable(dir string) bool { return unix.Access(dir, unix.W_OK) == nil }
