//go:build linux

package cage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootGrowableColdBootedIncusVM(t *testing.T) {
	dir := t.TempDir()
	oldVmstat, oldBlocks, oldMeminfo := machineVmstat, machineBlocks, bootMeminfo
	oldPort, oldVendor, oldName := bootIncusPort, bootBoardVendor, bootBoardName
	t.Cleanup(func() {
		machineVmstat, machineBlocks, bootMeminfo = oldVmstat, oldBlocks, oldMeminfo
		bootIncusPort, bootBoardVendor, bootBoardName = oldPort, oldVendor, oldName
	})

	// Cold-booted 2 GiB VM: balloon counters are zero and online blocks do not
	// exceed MemTotal + 1 GiB.
	machineVmstat = filepath.Join(dir, "vmstat")
	if err := os.WriteFile(machineVmstat, []byte("balloon_inflate 0\nballoon_deflate 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	machineBlocks = filepath.Join(dir, "no-blocks")
	bootMeminfo = filepath.Join(dir, "meminfo")
	if err := os.WriteFile(bootMeminfo, []byte("MemTotal:        1843200 kB\nMemFree:         1000000 kB\nMemAvailable:    1200000 kB\nSwapTotal:             0 kB\nCommitLimit:      921600 kB\nCommitted_AS:     600000 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bootIncusPort = filepath.Join(dir, "missing-port")
	bootBoardVendor = filepath.Join(dir, "board_vendor")
	bootBoardName = filepath.Join(dir, "board_name")

	if bootGrowable() {
		t.Fatal("plain small Linux machine reported growable")
	}

	if err := os.WriteFile(bootBoardVendor, []byte("LinuxContainers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootBoardName, []byte("Incus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bootGrowable() {
		t.Fatal("cold-booted Incus VM (DMI) not reported growable")
	}

	_ = os.Remove(bootBoardVendor)
	_ = os.Remove(bootBoardName)
	if err := os.WriteFile(bootIncusPort, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !bootGrowable() {
		t.Fatal("cold-booted Incus VM (virtio port) not reported growable")
	}
}
