package cage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

// ResourceLimits keeps the existing host-share policy while replacing the fixed
// 4 GiB / 2 CPU defaults. Zero hardware observations keep those legacy defaults.
func ResourceLimits() (memoryGiB, cpus int) {
	memoryGiB, cpus = host.MemGiB(), host.CPUs()
	if memoryGiB <= 0 {
		memoryGiB = 4
	}
	if cpus <= 0 {
		cpus = 2
	}
	return host.Cap("memory", memoryGiB, io.Discard), host.Cap("cpus", cpus, io.Discard)
}

const resourceProbe = `LC_ALL=C; export LC_ALL
awk '/^(MemTotal|MemFree|MemAvailable|SwapTotal|SwapFree):/ {print $1,$2}' /proc/meminfo
awk '/^(oom_kill|balloon_inflate|balloon_deflate|pswpin) / {print $1,$2}' /proc/vmstat
printf 'page_size '; getconf PAGESIZE
cat /proc/pressure/memory
head -n 1 /proc/stat
printf 'boot '; cat /proc/sys/kernel/random/boot_id
printf 'uptime '; cat /proc/uptime /run/cage-resources/state 2>/dev/null
true`

type ResourceSample struct {
	Total     uint64  `json:"total_bytes"`
	Available uint64  `json:"available_bytes"`
	SwapTotal uint64  `json:"swap_total_bytes"`
	SwapFree  uint64  `json:"swap_free_bytes"`
	OOMKills  uint64  `json:"oom_kills"`
	Balloon   uint64  `json:"balloon_bytes"`
	Stall     float64 `json:"memory_stall_percent"`
	Boot      string  `json:"boot_id"`
	// Free is MemFree: with it the machine knows what the guest holds.
	Free uint64 `json:"free_bytes,omitempty"`
	// Protected is set when the guest governor reported its classes; then
	// ProtectedStall is the stall of the control plane and the sessions and
	// BrowserStall that of the browser.
	Protected         bool    `json:"classes_reported,omitempty"`
	ProtectedStall    float64 `json:"protected_stall_percent,omitempty"`
	BrowserStall      float64 `json:"browser_stall_percent,omitempty"`
	cpuTotal, cpuIdle uint64
	swapIns           uint64
	swapInsKnown      bool
	classes           map[string]GuestClass
}

func parseResources(data []byte) (ResourceSample, error) {
	var s ResourceSample
	var inflated, deflated, pageSize uint64
	var uptime float64
	var governor []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		key := strings.TrimSuffix(f[0], ":")
		if key == "governor" || key == "class" {
			governor = append(governor, line)
			continue
		}
		if key == "uptime" {
			uptime, _ = strconv.ParseFloat(f[1], 64)
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		switch key {
		case "balloon_inflate", "balloon_deflate", "page_size":
			if err != nil || n > 1<<40 {
				return s, errors.New("invalid balloon observation")
			}
			if key == "balloon_inflate" {
				inflated = n
			}
			if key == "balloon_deflate" {
				deflated = n
			}
			if key == "page_size" {
				pageSize = n
			}
		case "MemFree", "pswpin":
			if err != nil || n > 1<<40 {
				return s, errors.New("invalid guest resource observation")
			}
			if key == "MemFree" {
				s.Free = n << 10
			} else {
				s.swapIns, s.swapInsKnown = n, true
			}
		case "MemTotal", "MemAvailable", "SwapTotal", "SwapFree", "oom_kill":
			if err != nil || n > 1<<40 {
				return s, errors.New("invalid guest resource observation")
			}
			seen[key] = true
			switch key {
			case "MemTotal":
				s.Total = n << 10
			case "MemAvailable":
				s.Available = n << 10
			case "SwapTotal":
				s.SwapTotal = n << 10
			case "SwapFree":
				s.SwapFree = n << 10
			case "oom_kill":
				s.OOMKills = n
			}
		case "some":
			for _, value := range f[1:] {
				if strings.HasPrefix(value, "avg10=") {
					s.Stall, _ = strconv.ParseFloat(strings.TrimPrefix(value, "avg10="), 64)
				}
			}
		case "boot":
			s.Boot = f[1]
		case "cpu":
			// guest/guest_nice are already included in user/nice.
			for i := 1; i < len(f) && i <= 8; i++ {
				v, e := strconv.ParseUint(f[i], 10, 64)
				if e != nil {
					return s, e
				}
				s.cpuTotal += v
				if i == 4 || i == 5 {
					s.cpuIdle += v
				}
			}
		}
	}
	if !seen["MemAvailable"] || !seen["oom_kill"] || s.Total < 256<<20 || s.Available > s.Total || s.SwapFree > s.SwapTotal || len(s.Boot) != 36 || math.IsNaN(s.Stall) || s.Stall < 0 || s.Stall > 100 {
		return s, errors.New("incomplete guest resource observation")
	}
	if s.Free > s.Total {
		return s, errors.New("incomplete guest resource observation")
	}
	if classes := parseGovernor(governor, uptime); len(classes) > 0 {
		s.classes, s.Protected = classes, true
		s.ProtectedStall = max(classes["control"].Stall, classes["sessions"].Stall)
		s.BrowserStall = classes["browser"].Stall
	}
	if inflated != 0 || deflated != 0 {
		if inflated < deflated || pageSize < 4096 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
			return s, errors.New("invalid balloon counters")
		}
		s.Balloon = (inflated - deflated) * pageSize
	}
	return s, nil
}

type ResourceStatus struct {
	Version    int             `json:"version"`
	PID        int             `json:"pid"`
	UpdatedAt  time.Time       `json:"updated_at"`
	ObservedAt time.Time       `json:"observed_at"`
	Maximum    uint64          `json:"maximum_bytes"`
	Target     uint64          `json:"target_bytes"`
	CPUs       int             `json:"cpus"`
	CPUPercent float64         `json:"cpu_percent"`
	Guest      *ResourceSample `json:"guest,omitempty"`
	// Pressure: the guest's protected classes (control plane, sessions) are
	// short of memory; the whole guest on guests without the governor.
	Pressure     bool   `json:"pressure"`
	HostPressure bool   `json:"host_pressure"`
	Error        string `json:"error,omitempty"`

	// The fields below explain the allocation (resource-allocation.md,
	// section 5). All are additive.
	// Capacity is what the machine can back now; Demand the guest's working
	// set plus headroom; Wanted the target the rule would choose if the
	// machine had room; Possible the largest maximum automatic sizing gives on
	// this machine (a restart applies it).
	Capacity uint64 `json:"capacity_bytes,omitempty"`
	Demand   uint64 `json:"demand_bytes,omitempty"`
	Wanted   uint64 `json:"wanted_bytes,omitempty"`
	Possible uint64 `json:"possible_bytes,omitempty"`
	// Short: the guest is short of memory and has all it can get now;
	// LimitedBy says what holds it: "maximum" (a human can raise it, see
	// Advice) or "machine" (the machine has no more to give now).
	Short           bool            `json:"short"`
	LimitedBy       string          `json:"limited_by,omitempty"`
	BrowserPressure bool            `json:"browser_pressure,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	NextAction      string          `json:"next_action,omitempty"`
	Advice          *ResourceAdvice `json:"advice,omitempty"`
	Machine         *MachineMemory  `json:"machine,omitempty"`
	Classes         []GuestClass    `json:"classes,omitempty"`
	Failures        int             `json:"failures,omitempty"`
}

// ResourceAdvice is a decision only a person can take, worded for display.
type ResourceAdvice struct {
	Message         string `json:"message"`
	MemoryBytes     uint64 `json:"memory_bytes,omitempty"`
	RestartRequired bool   `json:"restart_required"`
}

// memoryPolicy is the balloon target rule's state (guest_allocation.go). The
// requested target is not a measurement of the guest or host's resident RAM.
type memoryPolicy struct {
	maximum, target uint64
	idleSince       time.Time
	failures        int
	// resident is what the guest held at the last good sample, short what
	// the outer level was last told the machine lacks.
	resident, short uint64
}

func gb(size uint64) string {
	value := float64(size) / float64(gib)
	if value >= 10 || value == float64(int(value)) {
		return fmt.Sprintf("%.0f GB", value)
	}
	return fmt.Sprintf("%.1f GB", value)
}

// explain fills the status's explanation from a plan. manual is set for an
// explicitly chosen allocation, possible is the automatic maximum.
func explain(status *ResourceStatus, plan guestPlan, maximum, possible uint64, manual bool, failures int) {
	status.Capacity, status.Demand, status.Wanted, status.Reason, status.Failures = plan.capacity, plan.demand, plan.wanted, plan.reason, failures
	failed := plan.reason == "sample_failed_hold" || plan.reason == "sample_failed_grow"
	if !failed {
		// A failed sample keeps the last reading's shortage and advice: a
		// guest out of memory also fails to answer the probe.
		status.Short, status.LimitedBy, status.Advice = plan.short, "", nil
	}
	target := gb(plan.target)
	switch plan.reason {
	case "guest_needs":
		status.Summary, status.NextAction = "环境的内存用量上升，分配已增加到 "+target+"。", "无需操作。"
	case "guest_pressure":
		status.Summary, status.NextAction = "环境内存紧张，分配正在增加到 "+target+"。", "无需操作，几秒内会继续调整。"
	case "guest_severe":
		status.Summary, status.NextAction = "环境内存即将用尽，分配已放宽到 "+target+"。", "无需操作。"
	case "machine_short_guest_short":
		status.Summary, status.NextAction = "主机和环境的内存都紧张，环境只增加到刚好够用的 "+target+"。", "关闭主机上暂时不用的程序，环境可以获得更多内存。"
	case "machine_short_waiting":
		status.Summary, status.NextAction = "主机内存紧张，环境暂不增加内存（目前 "+target+"）。", "无需操作，主机内存恢复后自动继续。"
	case "machine_smaller":
		status.Summary, status.NextAction = "主机可用内存变少，已收回环境的空闲内存（目前 "+target+"）。", "无需操作。"
	case "reclaim_wait":
		status.Summary, status.NextAction = "环境有空闲内存（分配 "+target+"，需要 "+gb(plan.demand)+"）。", "持续空闲后逐步归还给主机。"
	case "reclaim_idle":
		status.Summary, status.NextAction = "正在把环境的空闲内存归还给主机（目前 "+target+"）。", "无需操作。"
	case "sample_failed_hold":
		status.Summary, status.NextAction = fmt.Sprintf("暂时读不到环境的内存用量（连续 %d 次），保持当前分配 %s。", failures, target), "无需操作，恢复读取后自动继续调整。"
	case "sample_failed_grow":
		status.Summary, status.NextAction = fmt.Sprintf("持续读不到环境的内存用量（连续 %d 次），已逐步放宽到 %s。", failures, target), "无需操作，恢复读取后自动继续调整。"
	default:
		status.Summary, status.NextAction = "环境内存充足（分配 "+target+"，需要 "+gb(plan.demand)+"）。", "无需操作。"
	}
	if failed {
		if status.Advice != nil {
			status.NextAction = status.Advice.Message
		}
		return
	}
	if !plan.short {
		return
	}
	if plan.target >= maximum {
		status.LimitedBy = "maximum"
		switch {
		case possible > maximum && manual:
			status.Advice = &ResourceAdvice{Message: "环境内存已用满手动设置的 " + gb(maximum) + "。改为自动（推荐）后可以调到 " + gb(possible) + "，需要重启环境。", MemoryBytes: possible, RestartRequired: true}
		case possible > maximum:
			status.Advice = &ResourceAdvice{Message: "可以把内存调到 " + gb(possible) + "，需要重启环境。", MemoryBytes: possible, RestartRequired: true}
		default:
			status.Advice = &ResourceAdvice{Message: "环境已用到这台机器能给的 " + gb(maximum) + "。请关闭不用的浏览器标签页或会话，或换一台内存更大的机器。"}
		}
		status.NextAction = status.Advice.Message
		return
	}
	status.LimitedBy = "machine"
	status.NextAction = "主机暂时没有更多内存可给；主机有空闲内存后自动增加。"
}

// machineMemoryReport is what the host agent tells an outer gateway about
// its machine (machine-memory.json next to the profile's control.json).
type machineMemoryReport struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Short     uint64    `json:"short_bytes"`
	Wanted    uint64    `json:"wanted_bytes"`
	Target    uint64    `json:"target_bytes"`
	// Pressure: the guest is under memory pressure (or stopped answering
	// after being so), so the shortfall is a shortage, not missing headroom.
	Pressure bool `json:"pressure,omitempty"`
	// Reason is BootRequestReason while a start waits for the machine to
	// grow (PrepareBoot); empty from the running guest's host agent.
	Reason string `json:"reason,omitempty"`
}

// allocationContext is what the host agent learns once at start from the
// environment's settings: the automatic maximum and whether the user chose
// the allocation.
var allocationContext = func(profile string) (possible uint64, manual bool) {
	memory, _ := AutomaticLimits()
	possible = uint64(memory) << 30
	if profile != "" {
		if allocation, err := LoadResourceAllocation(filepath.Join(profile, "config.json")); err == nil {
			manual = !allocation.Effective().Automatic
		}
	}
	return possible, manual
}

// agentProfile is the profile directory of the environment whose instance
// directory is dir: from the environment variable the host agent inherits,
// or from the layout <profile>/state/lima/<instance>.
func agentProfile(dir string) string {
	if path := os.Getenv(ConfigEnv); path != "" {
		return filepath.Dir(path)
	}
	if lima := filepath.Dir(dir); filepath.Base(lima) == "lima" && filepath.Base(filepath.Dir(lima)) == "state" {
		return filepath.Dir(filepath.Dir(lima))
	}
	return ""
}

// StartResourceManager belongs to the VM hostagent, so closing a window cannot
// strand a small balloon target. stop joins the loop before the driver stops.
func StartResourceManager(parent context.Context, dir string, maximum uint64, cpus int, read func(context.Context, string) ([]byte, error), set func(context.Context, uint64) error) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	reportPath := ""
	if profile := agentProfile(dir); profile != "" {
		reportPath = filepath.Join(profile, "machine-memory.json")
	}
	go func() {
		defer close(done)
		p := memoryPolicy{maximum: maximum, target: maximum}
		status := ResourceStatus{Version: 1, PID: os.Getpid(), Maximum: maximum, Target: maximum, CPUs: cpus}
		possible, manual := allocationContext(agentProfile(dir))
		status.Possible = possible
		var previous ResourceSample
		var bootTotal uint64
		var lastError string
		limitsPending := ""
		reported := false
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			probeCtx, finish := context.WithTimeout(ctx, 3*time.Second)
			data, err := read(probeCtx, limitsPending+resourceProbe)
			finish()
			var sample ResourceSample
			if err == nil {
				sample, err = parseResources(data)
				if err == nil && sample.Total > maximum {
					err = errors.New("guest memory exceeds VM capacity")
				}
				if err == nil {
					if previous.Boot != sample.Boot {
						bootTotal = sample.Total
					}
					// Some Linux kernels keep MemTotal constant while balloon pages
					// count as used. Subtract them once, also accepting kernels that
					// reduce MemTotal. Balloon counters acknowledge each reclamation.
					if sample.Balloon >= bootTotal {
						err = errors.New("invalid balloon size")
					} else {
						sample.Total = min(sample.Total, bootTotal-sample.Balloon)
						if sample.Available > sample.Total {
							err = errors.New("inconsistent available memory")
						}
						sample.Free = min(sample.Free, sample.Total)
					}
				}
			}
			now := time.Now()
			detail := ""
			machine := readMachine()
			status.UpdatedAt, status.HostPressure, status.Machine = now, machine.Pressure, &machine
			var plan guestPlan
			if err != nil {
				cause := usererror.Reason(err)
				if cause == "" {
					cause = "环境未能返回完整有效的内存与 CPU 数据"
				}
				plan = p.plan(guestInput{now: now, machine: machine})
				status.Error = "读取资源用量失败：" + cause + "。暂时保持当前内存分配，恢复读取后自动继续；若持续出现，请在设置中导出诊断包。"
				detail = fmt.Sprintf("%s: %v", status.Error, err)
			} else {
				status.Error = ""
				status.ObservedAt, status.Guest = now, &sample
				quiet := false
				if previous.Boot == sample.Boot && sample.cpuTotal > previous.cpuTotal && sample.cpuIdle >= previous.cpuIdle {
					total, idle := sample.cpuTotal-previous.cpuTotal, sample.cpuIdle-previous.cpuIdle
					status.CPUPercent = 100 * float64(total-min(idle, total)) / float64(total)
					quiet = status.CPUPercent < 5
				}
				if previous.Boot == sample.Boot && sample.OOMKills > previous.OOMKills {
					diagnostics.Log("warn", "resources", fmt.Sprintf("guest OOM kills increased by %d; available=%d target=%d maximum=%d", sample.OOMKills-previous.OOMKills, sample.Available, p.target, maximum))
				}
				if previous.Boot != sample.Boot {
					p.idleSince = time.Time{}
				}
				swapIn := previous.Boot == sample.Boot && sample.swapInsKnown && previous.swapInsKnown && sample.swapIns > previous.swapIns
				plan = p.plan(guestInput{now: now, sample: sample, ok: true, swapIn: swapIn, quiet: quiet, machine: machine})
				status.Pressure = plan.pressure
				status.BrowserPressure = sample.BrowserStall >= 1
				status.Classes = nil
				for _, name := range []string{"control", "sessions", "browser"} {
					if class, ok := sample.classes[name]; ok {
						status.Classes = append(status.Classes, class)
					}
				}
				// The classes are sized for what the guest can have now. The
				// limits go to the governor with every probe: it falls back to
				// the guest's own size when they stop coming (an older host
				// agent, or failed probes).
				limitsPending = planGuestClasses(min(maximum, max(plan.capacity, plan.demand)), sample.classes).script()
				previous = sample
			}
			explain(&status, plan, maximum, possible, manual, p.failures)
			if ctx.Err() != nil {
				return
			}
			if plan.target != p.target {
				changeCtx, finish := context.WithTimeout(ctx, 3*time.Second)
				e := set(changeCtx, plan.target)
				finish()
				if e != nil {
					status.Error = "调整环境内存失败：虚拟机没有接受新的内存目标，当前分配保持不变。请在设置中导出诊断包。"
					detail = fmt.Sprintf("%s: %v", status.Error, e)
				} else {
					p.target, status.Target = plan.target, plan.target
					diagnostics.Log("info", "resources", fmt.Sprintf("memory target=%d maximum=%d reason=%s", plan.target, maximum, plan.reason))
				}
			}
			if reportPath != "" && (plan.shortBytes > 0 || reported) {
				// Tell an outer gateway what the machine cannot back; once more
				// with zero when that is over.
				report := machineMemoryReport{Version: 1, UpdatedAt: now.UTC(), Short: plan.shortBytes, Wanted: plan.wanted, Target: p.target,
					Pressure: status.Pressure || p.failures > guestFailureHold}
				if writeJSONAtomic(reportPath, report) == nil {
					reported = plan.shortBytes > 0
				}
			}
			if detail != lastError {
				if detail != "" {
					diagnostics.Log("warn", "resources", detail)
				}
				lastError = detail
			}
			if e := writeResourceStatus(dir, status); e != nil && lastError != "status write failed" {
				diagnostics.Log("warn", "resources", "resource status write failed")
				lastError = "status write failed"
			}
		}
	}()
	return func() { cancel(); <-done }
}

// writeJSONAtomic replaces path with value, readable by others: the gateway
// reads machine-memory.json as root from outside the VM.
func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".machine-memory-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func writeResourceStatus(dir string, status ResourceStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".resources-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = host.ProtectFile(f.Name()); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(data)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "cage-resources.json"))
}

func ReadResourceStatus(dir string) (ResourceStatus, error) {
	var status ResourceStatus
	f, err := os.Open(filepath.Join(dir, "cage-resources.json"))
	if err != nil {
		return status, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 16384)).Decode(&status)
	if err == nil && (status.Version != 1 || time.Since(status.UpdatedAt) > 10*time.Second || time.Until(status.UpdatedAt) > 5*time.Second) {
		err = errors.New("resource observation expired")
	}
	return status, err
}
