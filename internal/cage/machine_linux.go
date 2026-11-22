package cage

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The machine files platformMachine reads; tests point them elsewhere.
var (
	machineMeminfo = "/proc/meminfo"
	machineVmstat  = "/proc/vmstat"
	machineBlocks  = "/sys/devices/system/memory"
	machineBalloon = "/sys/bus/virtio/drivers/virtio_balloon"
)

func platformMachine() MachineMemory {
	var reading MachineMemory
	seen := 0
	if data, err := os.ReadFile(machineMeminfo); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != "kB" {
				continue
			}
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || value > 1<<40 {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				reading.Total = value << 10
				seen++
			case "MemAvailable:":
				reading.Available = value << 10
				seen++
			}
		}
	}
	reading.Measured = seen == 2 && reading.Total > 0 && reading.Available <= reading.Total
	if !reading.Measured {
		reading.Available = 0
	}
	keeps, known := balloonKeepsMemTotal()
	reading.Boot = bootBytes(reading.Total, balloonBytes(), memoryBlocks(), keeps, known)
	return reading
}

// balloonKeepsMemTotal reports whether the virtio balloon negotiated
// VIRTIO_BALLOON_F_DEFLATE_ON_OOM (feature bit 2), with which the kernel
// keeps ballooned pages in MemTotal; known is false without a balloon device.
func balloonKeepsMemTotal() (keeps, known bool) {
	matches, _ := filepath.Glob(filepath.Join(machineBalloon, "virtio*", "features"))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if features := strings.TrimSpace(string(data)); err == nil && len(features) > 2 && strings.Trim(features, "01") == "" {
			return features[2] == '1', true
		}
	}
	return false, false
}

// memoryBlocks is the memory the kernel has online in hotplug blocks, which a
// balloon does not change; zero when the kernel does not report them.
func memoryBlocks() uint64 {
	data, err := os.ReadFile(filepath.Join(machineBlocks, "block_size_bytes"))
	if err != nil {
		return 0
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(data)), 16, 64)
	if err != nil || size == 0 || size > 1<<40 {
		return 0
	}
	entries, err := os.ReadDir(machineBlocks)
	if err != nil {
		return 0
	}
	var online uint64
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "memory") {
			continue
		}
		state, err := os.ReadFile(filepath.Join(machineBlocks, entry.Name(), "online"))
		if err == nil && strings.TrimSpace(string(state)) == "1" {
			online++
		}
	}
	return online * size
}

// balloonBytes is what a virtio balloon of an outer level holds in this
// machine now: such pages leave MemTotal, but the outer level may give them
// back.
func balloonBytes() uint64 {
	data, err := os.ReadFile(machineVmstat)
	if err != nil {
		return 0
	}
	var inflated, deflated uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || value > 1<<40 {
			continue
		}
		switch fields[0] {
		case "balloon_inflate":
			inflated = value
		case "balloon_deflate":
			deflated = value
		}
	}
	if deflated >= inflated {
		return 0
	}
	return (inflated - deflated) * uint64(os.Getpagesize())
}
