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
	// The owner's name may contain parentheses ("Code Helper (Renderer)"):
	// it ends at the first "): [0x" followed by the rest of the shape. The
	// first, not the last: the assertion's own name, which any app chooses,
	// comes later on the same line.
	assertionPattern  = regexp.MustCompile(`^\s*pid (\d+)\((.+?)\): \[0x[0-9a-fA-F]+\] \S+ (\S+) named: `)
	rootDomainPattern = regexp.MustCompile(`(?m)^\+-o IOPMrootDomain\s+<class IOPMrootDomain[,>]`)
	clamshellPattern  = regexp.MustCompile(`"AppleClamshellState" = (Yes|No)`)
	clamshellSleeps   = regexp.MustCompile(`"AppleClamshellCausesSleep" = (Yes|No)`)
)

// displayAssertions are the assertion types that keep the display awake.
var displayAssertions = map[string]bool{"PreventUserIdleDisplaySleep": true, "NoDisplaySleepAssertion": true}

// hostConditionCommands are read in this order, with fixed arguments and no
// shell. The root power domain is read whole: with "-k AppleClamshellState"
// ioreg prints nothing on a Mac without a lid.
var hostConditionCommands = [4][]string{
	{"/usr/bin/pmset", "-g", "systemstate"},
	{"/usr/bin/pmset", "-g", "batt"},
	{"/usr/bin/pmset", "-g", "assertions"},
	{"/usr/sbin/ioreg", "-r", "-n", "IOPMrootDomain", "-d", "1"},
}

// readHostConditions runs hostConditionCommands through run and parses them.
func readHostConditions(run func(command []string) ([]byte, error), own map[int]bool) (*policy.HostState, error) {
	var outputs [len(hostConditionCommands)][]byte
	for i, command := range hostConditionCommands {
		out, err := run(command)
		if err != nil {
			return nil, err
		}
		outputs[i] = out
	}
	return parseHostConditions(outputs[0], outputs[1], outputs[2], outputs[3], own)
}

// parseHostConditions reads the output of hostConditionCommands. own lists
// this process tree's ids, whose assertions do not count.
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
		if match == nil {
			// A process's assertion in a shape this parser does not know
			// could be a display held: unknown, so not now.
			if strings.HasPrefix(strings.TrimSpace(line), "pid ") {
				return nil, errors.New("unknown assertion line")
			}
			continue
		}
		if !displayAssertions[match[3]] {
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

	if !rootDomainPattern.Match(clamshell) {
		return nil, errors.New("power domain missing")
	}
	// A Mac without a lid has no clamshell state: never "lid closed". A
	// closed lid with an external display (clamshell mode) does not cause
	// sleep: that counts as open.
	if lid := clamshellPattern.FindSubmatch(clamshell); lid != nil && string(lid[1]) == "Yes" {
		sleeps := clamshellSleeps.FindSubmatch(clamshell)
		state.LidClosed = sleeps == nil || string(sleeps[1]) != "No"
	}
	return &state, nil
}
