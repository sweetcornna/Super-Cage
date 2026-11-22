//go:build darwin

package workbench

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var hidIdlePattern = regexp.MustCompile(`"HIDIdleTime" = ([0-9]+)`)

// hostInputIdle is the time since the last keyboard or mouse input on this
// Mac, read once per update decision from the HID system.
func hostInputIdle(ctx context.Context) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem", "-d", "4", "-r", "-k", "HIDIdleTime").Output()
	if err != nil {
		return 0, err
	}
	return parseHIDIdle(out)
}

func parseHIDIdle(out []byte) (time.Duration, error) {
	match := hidIdlePattern.FindSubmatch(out)
	if match == nil {
		return 0, errors.New("HIDIdleTime missing")
	}
	ns, err := strconv.ParseUint(string(match[1]), 10, 63)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}
