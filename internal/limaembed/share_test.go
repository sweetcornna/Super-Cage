package limaembed

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"testing/fstest"
)

func fakeLinuxELF(t *testing.T, machine uint16) []byte {
	t.Helper()
	var hdr [64]byte
	copy(hdr[:4], []byte{0x7f, 'E', 'L', 'F'})
	hdr[4] = 2 // ELFCLASS64
	hdr[5] = 1 // ELFDATA2LSB
	binary.LittleEndian.PutUint16(hdr[18:20], machine)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVerifyEmbeddedRejectsMissingOrWrongArchGuestAgent(t *testing.T) {
	amd64ELF := fakeLinuxELF(t, 62)
	arm64ELF := fakeLinuxELF(t, 183)

	if err := verifyEmbedded(fstest.MapFS{
		"assets/lima-guestagent.Linux-x86_64.gz": {Data: amd64ELF},
	}, "linux", "amd64"); err != nil {
		t.Fatalf("valid amd64 rejected: %v", err)
	}
	if err := verifyEmbedded(fstest.MapFS{
		"assets/lima-guestagent.Linux-aarch64.gz": {Data: arm64ELF},
	}, "linux", "arm64"); err != nil {
		t.Fatalf("valid arm64 rejected: %v", err)
	}
	// Classic cross-compile bug: host arm64 asset left in tree when building linux/amd64.
	if err := verifyEmbedded(fstest.MapFS{
		"assets/lima-guestagent.Linux-aarch64.gz": {Data: arm64ELF},
	}, "linux", "amd64"); !errors.Is(err, ErrNoAssets) {
		t.Fatalf("expected ErrNoAssets when x86_64 missing, got %v", err)
	}
	// Wrong-arch ELF renamed to x86_64.gz must also fail.
	if err := verifyEmbedded(fstest.MapFS{
		"assets/lima-guestagent.Linux-x86_64.gz": {Data: arm64ELF},
	}, "linux", "amd64"); !errors.Is(err, ErrNoAssets) {
		t.Fatalf("expected ErrNoAssets for arm64 ELF in x86_64 slot, got %v", err)
	}
	// Corrupt gzip fails.
	if err := verifyEmbedded(fstest.MapFS{
		"assets/lima-guestagent.Linux-x86_64.gz": {Data: []byte("not-gzip")},
	}, "linux", "amd64"); !errors.Is(err, ErrNoAssets) {
		t.Fatalf("expected ErrNoAssets for corrupt gzip, got %v", err)
	}
}

func TestGuestAgentMatchesGuestAndCurrentBuild(t *testing.T) {
	linuxArch := guestArch()
	const darwinFile = "assets/Darwin/lima-guestagent.Darwin-aarch64.gz"
	files := fstest.MapFS{
		"assets/lima-guestagent.Linux-" + linuxArch + ".gz": {Data: []byte("linux")},
		darwinFile: {Data: []byte("darwin")},
	}
	dir := t.TempDir()
	share, err := writeShare(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, guest := range []struct{ os, arch, data string }{
		{"Linux", linuxArch, "linux"},
		{"Darwin", "aarch64", "darwin"},
	} {
		path, err := share.GuestAgent(guest.os, guest.arch)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != guest.data {
			t.Fatalf("%s/%s: read %q, %v", guest.os, guest.arch, data, err)
		}
	}
	if path, err := share.GuestAgent("Darwin", "x86_64"); err == nil {
		t.Fatalf("used %s for an unbundled architecture", path)
	}

	// A different build can reuse the same profile with the old file on disk.
	delete(files, darwinFile)
	share, err = writeShare(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := share.GuestAgent("Darwin", "aarch64"); err == nil {
		t.Fatalf("used %s after the build stopped bundling it", path)
	}
}
