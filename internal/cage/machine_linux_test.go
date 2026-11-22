package cage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxMachineReadingCountsTheOuterBalloon(t *testing.T) {
	dir := t.TempDir()
	oldMeminfo, oldVmstat, oldBlocks, oldBalloon := machineMeminfo, machineVmstat, machineBlocks, machineBalloon
	t.Cleanup(func() {
		machineMeminfo, machineVmstat, machineBlocks, machineBalloon = oldMeminfo, oldVmstat, oldBlocks, oldBalloon
	})
	machineMeminfo, machineVmstat, machineBlocks, machineBalloon = filepath.Join(dir, "meminfo"), filepath.Join(dir, "vmstat"), filepath.Join(dir, "blocks"), filepath.Join(dir, "balloon")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(machineMeminfo, "MemTotal:        4194304 kB\nMemFree: 100 kB\nMemAvailable:    1048576 kB\n")
	pages := uint64(4<<30) / uint64(os.Getpagesize())
	write(machineVmstat, "nr_free_pages 1\nballoon_inflate "+itoa(pages+10)+"\nballoon_deflate 10\n")
	reading := platformMachine()
	if !reading.Measured || reading.Total != 4<<30 || reading.Available != 1<<30 || reading.Boot != 8<<30 {
		t.Fatalf("ballooned machine %+v", reading)
	}
	// A kernel that keeps MemTotal under the balloon: the 8 GiB online in
	// memory blocks bound the boot size.
	write(filepath.Join(machineBlocks, "block_size_bytes"), "200000000\n")
	write(filepath.Join(machineBlocks, "memory0", "online"), "1\n")
	write(filepath.Join(machineBlocks, "memory1", "online"), "0\n")
	write(machineMeminfo, "MemTotal: 8000000 kB\nMemAvailable: 1048576 kB\n")
	if reading := platformMachine(); reading.Boot != 8000000<<10 {
		t.Fatalf("constant MemTotal under a balloon %+v", reading)
	}
	// The balloon driver's features decide when it reports them: bit 2 is
	// deflate-on-oom, with which MemTotal keeps the ballooned pages.
	write(filepath.Join(machineBalloon, "virtio3", "features"), "0010000000000000000000000000000011\n")
	write(machineMeminfo, "MemTotal: 4194304 kB\nMemAvailable: 1048576 kB\n")
	if reading := platformMachine(); reading.Boot != 4<<30 {
		t.Fatalf("deflate-on-oom balloon %+v", reading)
	}
	write(filepath.Join(machineBalloon, "virtio3", "features"), "0000000000000000000000000000000011\n")
	if reading := platformMachine(); reading.Boot != 8<<30 {
		t.Fatalf("balloon without deflate-on-oom %+v", reading)
	}
	// Nothing available is a reading, not an unknown.
	write(machineMeminfo, "MemTotal: 4194304 kB\nMemAvailable: 0 kB\n")
	if reading := platformMachine(); !reading.Measured || reading.Available != 0 {
		t.Fatalf("empty machine %+v", reading)
	}
	write(machineMeminfo, "MemTotal: 4194304 kB\n")
	if reading := platformMachine(); reading.Measured {
		t.Fatalf("incomplete reading taken as measured %+v", reading)
	}
}

func itoa(n uint64) string {
	digits := []byte{}
	for {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
		if n == 0 {
			return string(digits)
		}
	}
}
