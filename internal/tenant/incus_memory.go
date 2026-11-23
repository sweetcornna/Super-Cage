package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The host files MemoryState reads; tests point them elsewhere.
var (
	hostMeminfo        = "/proc/meminfo"
	hostMemoryPressure = "/proc/pressure/memory"
)

// ElasticMemory reports whether this runtime resizes running workspaces.
// Only virtual machines are resized: a container takes host memory only as it
// uses it, and a cgroup limit below its use would start its OOM killer.
func (r *IncusRuntime) ElasticMemory() bool { return r.instanceType() == InstanceVM }

// MemoryState reads this host's memory, the trusted profile ceiling and every
// gateway-owned instance's memory size. Running VMs also report their guest
// /proc/meminfo; a VM that does not answer within a few seconds is left
// without guest figures and is not resized.
func (r *IncusRuntime) MemoryState(ctx context.Context) (MemoryState, error) {
	var state MemoryState
	data, err := os.ReadFile(hostMeminfo)
	if err != nil {
		return state, errors.New("host memory is unreadable")
	}
	if state.HostTotal, state.HostAvailable, err = parseMeminfo(data); err != nil {
		return state, err
	}
	if data, err := os.ReadFile(hostMemoryPressure); err == nil {
		state.HostStall = parseMemoryStall(data)
	}
	profile, err := r.loadProfile(ctx)
	if err != nil {
		return state, err
	}
	state.Ceiling, _ = incusSizeBytes(profile.Config["limits.memory"])
	// recursion=1 returns configuration without state, so it never waits on
	// guest agents.
	body, err := captureIncus(r.localCommand(ctx, "query", "local:/1.0/instances?recursion=1&project="+r.cfg.Project), 64<<20)
	if err != nil {
		return state, err
	}
	var records []instance
	if json.Unmarshal(body, &records) != nil {
		return state, errors.New("invalid Incus instance inventory")
	}
	state.Instances = make(map[string]InstanceMemory, len(records))
	for _, record := range records {
		id := record.Config[instanceLabel]
		if record.Type != r.instanceType() || !vmIDPattern.MatchString(id) || record.Name != "cage-"+id {
			continue
		}
		limit, ok := incusSizeBytes(record.ExpandedConfig["limits.memory"])
		if !ok {
			continue
		}
		state.Instances[id] = InstanceMemory{Status: record.Status, Limit: limit, Hotplugged: record.Config["volatile.vm.hotplug.memory"] != ""}
	}
	var running []string
	for id, entry := range state.Instances {
		if entry.Status == "Running" {
			running = append(running, id)
		}
	}
	guests := make([]InstanceMemory, len(running))
	var probes sync.WaitGroup
	slots := make(chan struct{}, 4)
	for index, id := range running {
		probes.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			probe, cancel := context.WithTimeout(ctx, 5*time.Second)
			reading, err := r.guestMemory(probe, id, time.Now())
			cancel()
			reading.GuestSilent = err != nil
			guests[index] = reading
		})
	}
	probes.Wait()
	for index, id := range running {
		entry := state.Instances[id]
		reading := guests[index]
		entry.GuestTotal, entry.GuestAvailable, entry.GuestSilent = reading.GuestTotal, reading.GuestAvailable, reading.GuestSilent
		entry.GuestStall, entry.GuestSwapIns, entry.GuestSwapKnown = reading.GuestStall, reading.GuestSwapIns, reading.GuestSwapKnown
		// The pressure flag reaches the rule only for a start's request: for
		// a running guest the reading has always dropped it, and passing it
		// on (as resource-allocation.md 7.13 intends) changes allocation
		// under host pressure for every running environment; that is a
		// decision of its own.
		entry.NestedShort, entry.NestedStart = reading.NestedShort, reading.NestedStart
		entry.NestedPressure = reading.NestedStart && reading.NestedPressure
		state.Instances[id] = entry
	}
	return state, nil
}

// guestMemoryScript reads, in one exec, the guest's /proc/meminfo (required)
// and what is optional: its memory pressure, its swap-in counter and what a
// Cage guest nested in it wants (machine-memory.json next to the control
// file, written by that guest's host agent). A missing optional file leaves
// its section empty.
const guestMemoryScript = "head -c 16384 /proc/meminfo; echo '#cage-psi'; head -c 1024 /proc/pressure/memory 2>/dev/null; " +
	"echo '#cage-vmstat'; grep '^pswpin ' /proc/vmstat 2>/dev/null; echo '#cage-nested'; head -c 4096 \"$1\" 2>/dev/null; exit 0"

// machineMemoryFile is the file a host agent writes next to its profile's
// control.json for the gateway: what its guest wants and the machine cannot
// back now. The guest writes it, so it can at most claim its own ceiling.
const machineMemoryFile = "machine-memory.json"

// nestedFresh is how old a nested demand may be, either way, before it is
// ignored: it is written every few seconds while the guest is short.
const nestedFresh = 45 * time.Second

// guestMemory reads a running VM. The guest reports its figures, so a tenant
// can at most claim more need and get its own profile ceiling, or claim less
// and shrink only itself.
func (r *IncusRuntime) guestMemory(ctx context.Context, id string, now time.Time) (InstanceMemory, error) {
	body, err := r.capture(ctx, 24576, "exec", "local:cage-"+id, "--mode=non-interactive", "--",
		"/bin/sh", "-c", guestMemoryScript, "sh", path.Join(path.Dir(r.cfg.ControlPath), machineMemoryFile))
	if err != nil {
		return InstanceMemory{}, err
	}
	return parseGuestMemory(body, now)
}

// parseGuestMemory reads the sections of guestMemoryScript.
func parseGuestMemory(body []byte, now time.Time) (InstanceMemory, error) {
	var reading InstanceMemory
	sections := map[string][]byte{}
	name := "meminfo"
	for _, line := range bytes.SplitAfter(body, []byte("\n")) {
		if marker, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("#cage-")); ok && len(bytes.Fields(line)) == 1 {
			name = string(marker)
			continue
		}
		sections[name] = append(sections[name], line...)
	}
	var err error
	if reading.GuestTotal, reading.GuestAvailable, err = parseMeminfo(sections["meminfo"]); err != nil {
		return reading, err
	}
	reading.GuestStall = parseMemoryStall(sections["psi"])
	if fields := strings.Fields(string(sections["vmstat"])); len(fields) == 2 && fields[0] == "pswpin" {
		if count, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			reading.GuestSwapIns, reading.GuestSwapKnown = count, true
		}
	}
	var nested struct {
		Version   int       `json:"version"`
		UpdatedAt time.Time `json:"updated_at"`
		Short     int64     `json:"short_bytes"`
		Pressure  bool      `json:"pressure"`
		// Reason is "start" while a stopped environment's start waits for
		// this VM to grow (cage.PrepareBoot); the same short_bytes, so older
		// gateways honour it too.
		Reason string `json:"reason"`
	}
	if data := bytes.TrimSpace(sections["nested"]); len(data) > 0 && json.Unmarshal(data, &nested) == nil && nested.Version == 1 &&
		nested.Short > 0 && nested.Short <= 1<<40 && now.Sub(nested.UpdatedAt).Abs() <= nestedFresh {
		reading.NestedShort, reading.NestedPressure, reading.NestedStart = nested.Short, nested.Pressure, nested.Reason == "start"
	}
	return reading, nil
}

var errMemoryNotRunning = errors.New("Incus instance is not running")

// SetMemory changes a running VM's limits.memory. Incus applies it live
// through the virtio balloon and waits a few seconds for the guest; a guest
// that is slower makes this fail while QEMU keeps the new target.
func (r *IncusRuntime) SetMemory(ctx context.Context, id string, size int64) error {
	if r.instanceType() != InstanceVM || !vmIDPattern.MatchString(id) || size < MinimumMemory || size%(1<<20) != 0 {
		return errors.New("invalid memory size")
	}
	unlock := r.lockInstance(id)
	defer unlock()
	body, err := captureIncus(r.localCommand(ctx, "query", "local:/1.0/instances/cage-"+id+"?project="+r.cfg.Project), 1<<20)
	if err != nil {
		return err
	}
	var record instance
	if json.Unmarshal(body, &record) != nil || record.Name != "cage-"+id || record.Type != InstanceVM || record.Config[instanceLabel] != id {
		return errors.New("Incus instance ownership does not match the workspace")
	}
	if record.Status != "Running" {
		return errMemoryNotRunning
	}
	command := r.command(ctx, "config", "set", "local:cage-"+id, fmt.Sprintf("limits.memory=%dMiB", size>>20))
	reason := &firstBytes{limit: 4096}
	command.Stderr = reason
	if err := command.Run(); err != nil {
		return fmt.Errorf("Incus memory change failed: %s", incusReason(reason.String(), err))
	}
	return nil
}

// firstBytes keeps the start of a stream and discards the rest.
type firstBytes struct {
	bytes.Buffer
	limit int
}

func (b *firstBytes) Write(data []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(data[:min(len(data), room)])
	}
	return len(data), nil
}

// incusReason is the first line of an Incus error, for the gateway log.
func incusReason(stderr string, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	line = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return -1
		}
		return char
	}, strings.TrimPrefix(line, "Error: "))
	if len(line) > 200 {
		line = line[:200]
	}
	var exit *exec.ExitError
	switch {
	case line != "":
		return line
	case errors.As(err, &exit):
		return exit.String()
	default:
		return err.Error()
	}
}

// parseMeminfo returns MemTotal and MemAvailable of a /proc/meminfo in bytes.
func parseMeminfo(data []byte) (total, available int64, err error) {
	seen := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" || (fields[0] != "MemTotal:" && fields[0] != "MemAvailable:") {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || value < 0 || value > 1<<40 {
			return 0, 0, errors.New("invalid memory observation")
		}
		if fields[0] == "MemTotal:" {
			total = value << 10
		} else {
			available = value << 10
		}
		seen++
	}
	if seen != 2 || total <= 0 || available > total {
		return 0, 0, errors.New("incomplete memory observation")
	}
	return total, available, nil
}

// parseMemoryStall returns the "some avg10" share of /proc/pressure/memory:
// the percentage of the last ten seconds in which a task waited for memory.
func parseMemoryStall(data []byte) float64 {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "avg10="); ok {
				if stall, err := strconv.ParseFloat(value, 64); err == nil && stall >= 0 && stall <= 100 {
					return stall
				}
			}
		}
	}
	return 0
}
