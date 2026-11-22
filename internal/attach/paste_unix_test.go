//go:build !windows

package attach

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Only a regular image file is read: not through a symbolic link, not a
// FIFO (which would block), not one over the size limit, and never for
// longer than the read deadline.
func TestImageFileChecks(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "a.png")
	writePNG(t, good)
	if _, err := attachImageFile(good); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := attachImageFile(link); err == nil {
		t.Fatal("followed a symbolic link")
	}
	fifo := filepath.Join(dir, "fifo.png")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := attachImageFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked on a FIFO")
	}
	big := filepath.Join(dir, "big.png")
	if err := os.WriteFile(big, make([]byte, MaxImageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachImageFile(big); err == nil {
		t.Fatal("read an oversized file")
	}
	// A read that hangs (a stalled network volume) gives up at the deadline.
	load, wait := loadImage, imageReadTimeout
	t.Cleanup(func() { loadImage, imageReadTimeout = load, wait })
	release := make(chan struct{})
	defer close(release)
	loadImage = func(string) ([]byte, error) { <-release; return nil, errors.New("released") }
	imageReadTimeout = 200 * time.Millisecond
	start := time.Now()
	if _, err := readImage(good); err == nil || time.Since(start) > time.Second {
		t.Fatal("no read deadline", err, time.Since(start))
	}
}
