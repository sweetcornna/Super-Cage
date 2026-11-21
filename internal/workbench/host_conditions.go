package workbench

import (
	"bytes"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
)

// The host's power and display state, read once for an unattended decision
// from the same tools a person would use (pmset, ioreg). Parsing is closed:
// anything but the expected shape is an error, and an error is "not now".

var (
	powerSourcePattern = regexp.MustCompile(`Now drawing from '([^']+)'`)
	batteryPattern     = regexp.MustCompile(`(\d{1,3})%`)
	assertionPattern   = regexp.MustCompile(`^\s*pid (\d+)\(([^)]*)\): \[0x[0-9a-fA-F]+\] \S+ (\S+) named: `)
	clamshellPattern   = regexp.MustCompile(`"AppleClamshellState" = (Yes|No)`)
	clamshellSleeps    = regexp.MustCompile(`"AppleClamshellCausesSleep" = (Yes|No)`)
)

// displayAssertions are the assertion types that keep the display awake.
var displayAssertions = map[string]bool{"PreventUserIdleDisplaySleep": true, "NoDisplaySleepAssertion": true}

// parseHostConditions reads the output of "pmset -g systemstate",
// "pmset -g batt", "pmset -g assertions" and
// "ioreg -r -n IOPMrootDomain -d 1 -k AppleClamshellState". own lists this
// process tree's ids, whose assertions do not count.
func parseHostConditions(systemState, battery, assertions, clamshell []byte, own map[int]bool) (*policy.HostState, error) {
	var state policy.HostState
	capabilities := ""
	for _, line := range strings.Split(string(systemState), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Current System Capabilities are:"); ok {
			capabilities = rest
		}
	}
	if strings.TrimSpace(capabilities) == "" {
		return nil, errors.New("system capabilities missing")
	}
	// A full wake has graphics; a dark wake (Power Nap, a timer) has none.
	state.DarkWake = !strings.Contains(" "+capabilities+" ", " Graphics ")

	source := powerSourcePattern.FindSubmatch(battery)
	if source == nil {
		return nil, errors.New("power source missing")
	}
	state.OnBattery = string(source[1]) != "AC Power"
	if state.OnBattery {
		level := batteryPattern.FindSubmatch(battery)
		if level == nil {
			return nil, errors.New("battery level missing")
		}
		state.Battery, _ = strconv.Atoi(string(level[1]))
	}

	if !bytes.Contains(assertions, []byte("Listed by owning process:")) {
		return nil, errors.New("assertion list missing")
	}
	held := map[string]bool{}
	for _, line := range strings.Split(string(assertions), "\n") {
		match := assertionPattern.FindStringSubmatch(line)
		if match == nil || !displayAssertions[match[3]] {
			continue
		}
		pid, _ := strconv.Atoi(match[1])
		if own[pid] || match[2] == "powerd" {
			continue
		}
		held[plainLabel(match[2], 32)] = true
	}
	for name := range held {
		state.DisplayHeld = append(state.DisplayHeld, name)
	}
	sort.Strings(state.DisplayHeld)

	if !bytes.Contains(clamshell, []byte("IOPMrootDomain")) {
		return nil, errors.New("power domain missing")
	}
	// A Mac without a lid has no clamshell state. A closed lid with an
	// external display (clamshell mode) does not cause sleep: that counts as
	// open.
	if lid := clamshellPattern.FindSubmatch(clamshell); lid != nil && string(lid[1]) == "Yes" {
		sleeps := clamshellSleeps.FindSubmatch(clamshell)
		state.LidClosed = sleeps == nil || string(sleeps[1]) != "No"
	}
	return &state, nil
}
