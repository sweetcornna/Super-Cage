package cage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Before a nested guest starts, its machine must be able to map the guest's
// boot maximum: QEMU allocates the guest's memory as one anonymous mapping,
// and the kernel refuses one larger than MemTotal + swap
// (vm.overcommit_memory=0) or than CommitLimit - Committed_AS (2). Inside a
// workspace VM that the gateway shrank to its guarantee while the environment
// was stopped (level A, "idle memory returned"), a boot maximum chosen from
// the machine's boot size does not fit, and QEMU exits at once with "cannot
// set up guest memory". PrepareBoot asks the outer level for the memory
// through the channel it already reads (machine-memory.json, short_bytes),
// waits a bounded time for the machine to grow, and otherwise starts with
// the largest maximum the machine maps now or fails with a sentence about
// memory. See resource-allocation.md, section 8.

// The files the boot check reads; tests point them elsewhere.
var (
	bootMeminfo    = "/proc/meminfo"
	bootOvercommit = "/proc/sys/vm/overcommit_memory"
)

const (
	// bootOverhead is what QEMU maps beside the guest's memory, and slack for
	// the kernel's check and for MemTotal moving a little with the balloon.
	bootOverhead = 256 * mib
	// bootFloor is the smallest boot maximum a start falls back to: the
	// floor of the balloon rule (min(2 GiB, maximum)) and the smallest manual
	// allocation.
	bootFloor = 2 * gib
	// bootWait is how long a start waits for the outer level: five rounds of
	// the gateway (15 s each) and the balloon's own time.
	bootWait = 75 * time.Second
	// bootPoll is how often the machine is read while waiting, bootRefresh
	// how often the request is rewritten: the gateway ignores it 45 s after
	// it was written, so a crashed start leaves nothing that sticks.
	bootPoll    = time.Second
	bootRefresh = 10 * time.Second
	// BootRequestReason marks a start's request in machine-memory.json.
	BootRequestReason = "start"
	// BootMemoryMarker begins every sentence of a start refused for memory.
	BootMemoryMarker = "内存不足以启动环境"
)

// BootMachine is the machine as the kernel's overcommit check sees it.
type BootMachine struct {
	Total, Available, SwapTotal, CommitLimit, Committed uint64
	// Overcommit is vm.overcommit_memory: 0 heuristic, 1 always, 2 never;
	// -1 when not read (yet).
	Overcommit int
}

// readBootMachine reads /proc/meminfo; ok is false off Linux or when it is
// incomplete. The overcommit mode is read only when it decides (bootFits).
func readBootMachine() (BootMachine, bool) {
	m := BootMachine{Overcommit: -1}
	if runtime.GOOS != "linux" {
		return m, false
	}
	return parseBootMeminfo(bootMeminfo)
}

func parseBootMeminfo(path string) (BootMachine, bool) {
	m := BootMachine{Overcommit: -1}
	data, err := os.ReadFile(path)
	if err != nil {
		return m, false
	}
	seen := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		var into *uint64
		switch fields[0] {
		case "MemTotal:":
			into = &m.Total
		case "MemAvailable:":
			into = &m.Available
		case "SwapTotal:":
			into = &m.SwapTotal
		case "CommitLimit:":
			into = &m.CommitLimit
		case "Committed_AS:":
			into = &m.Committed
		default:
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || value > 1<<40 {
			return m, false
		}
		*into = value << 10
		seen++
	}
	return m, seen == 5 && m.Total > 0 && m.Available <= m.Total
}

// readOvercommit is vm.overcommit_memory, 0 (the default) when unreadable.
func readOvercommit() int {
	data, err := os.ReadFile(bootOvercommit)
	if err != nil {
		return 0
	}
	mode, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || mode < 0 || mode > 2 {
		return 0
	}
	return mode
}

// maps reports whether the kernel maps size bytes of guest memory now.
func (m BootMachine) maps(size uint64) bool {
	switch m.Overcommit {
	case 1:
		return true
	case 2:
		return m.Committed+size+bootOverhead <= m.CommitLimit
	}
	return size+bootOverhead <= m.Total+m.SwapTotal
}

// largest is the largest whole-GiB boot maximum, at most maximum, that the
// kernel maps now.
func (m BootMachine) largest(maximum uint64) uint64 {
	room := maximum
	switch m.Overcommit {
	case 1:
	case 2:
		room = m.CommitLimit - min(m.CommitLimit, m.Committed+bootOverhead)
	default:
		room = m.Total + m.SwapTotal - min(m.Total+m.SwapTotal, bootOverhead)
	}
	return min(maximum, room/gib*gib)
}

// cushion is what the guest needs available to boot: its floor plus the
// margin the balloon rule keeps on the machine (machineCapacity).
func (m BootMachine) cushion(maximum uint64) uint64 {
	return min(bootFloor, maximum) + min(max(m.Total/64, 256*mib), 512*mib)
}

func (m BootMachine) working() uint64 { return m.Total - min(m.Total, m.Available) }

// needTotal is the MemTotal at which the kernel maps maximum and the guest's
// cushion fits beside the machine's working set.
func (m BootMachine) needTotal(maximum uint64) uint64 {
	need := m.working() + m.cushion(maximum)
	switch m.Overcommit {
	case 1:
	case 2:
		// CommitLimit grows by overcommit_ratio of what MemTotal grows.
		if per := m.CommitLimit - min(m.CommitLimit, m.SwapTotal); per > 0 {
			if want := m.Committed + maximum + bootOverhead; want > m.CommitLimit {
				need = max(need, m.Total+uint64(float64(want-m.CommitLimit)*float64(m.Total)/float64(per))+1)
			}
		}
	default:
		need = max(need, maximum+bootOverhead-min(maximum+bootOverhead, m.SwapTotal))
	}
	return need
}

// startShort is what a start asks of the outer level: the memory it needs
// beyond the machine's working set. The gateway adds it to that working set
// plus its own headroom (allocateMemory: d = W + headroom + short_bytes), so
// one grant takes MemTotal to at least needTotal; a shortfall against the
// current MemTotal would come short whenever the machine's working set plus
// headroom is below its limit, which it is at the guarantee.
func (m BootMachine) startShort(maximum uint64) uint64 {
	need := m.needTotal(maximum)
	return roundUpUnit(need - min(need, m.working()))
}

// BootRequest is a start of a nested guest with a boot maximum of Maximum
// bytes. Profile is the directory of the profile's config.json, where
// machine-memory.json goes. The hooks default to the real machine and clock.
type BootRequest struct {
	Profile  string
	Maximum  uint64
	Progress func(string)

	Machine    func() (BootMachine, bool)
	Overcommit func() int
	// Growable reports whether an outer level holds memory of this machine
	// (a balloon) that it can give back.
	Growable func() bool
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
}

// BootDecision is the boot maximum to start with. Asked is set when the
// outer level was asked for memory; Notice explains a smaller maximum.
type BootDecision struct {
	Maximum uint64
	Notice  string
	Asked   bool
	Waited  time.Duration
}

// BootMemoryError refuses a start: the machine cannot map even the smallest
// boot maximum. Need and Have are what the kernel's check compares.
type BootMemoryError struct {
	Need, Have uint64
	Growable   bool
}

func (e *BootMemoryError) Error() string {
	if e.Growable {
		return fmt.Sprintf("%s：工作区至少需要约 %s，现有 %s，服务器暂时分不出更多内存；请稍后重试，或请管理员为这个工作区增加内存。", BootMemoryMarker, gb(e.Need), gb(e.Have))
	}
	return fmt.Sprintf("%s：至少需要约 %s，这台机器现有 %s；请关闭其他程序后重试。", BootMemoryMarker, gb(e.Need), gb(e.Have))
}

func (r *BootRequest) defaults() {
	if r.Machine == nil {
		r.Machine = readBootMachine
	}
	if r.Overcommit == nil {
		r.Overcommit = readOvercommit
	}
	if r.Growable == nil {
		r.Growable = func() bool { return balloonBytes() > 0 }
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Sleep == nil {
		r.Sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	if r.Progress == nil {
		r.Progress = func(string) {}
	}
}

// PrepareBoot makes sure the machine can back a nested guest's boot maximum
// before it starts. Fast path: /proc/meminfo (and the overcommit mode when it
// decides) shows that the kernel maps the maximum and the guest's floor fits:
// nothing is written or waited for. Otherwise, inside a machine whose outer
// level holds memory in a balloon, the need goes to that level
// (machine-memory.json: short_bytes, pressure, reason "start"), rewritten
// every bootRefresh, and the machine is read every bootPoll for up to
// bootWait. The request is removed when the wait ends, before the guest
// starts: the outer level holds a demand it saw for minutes, and the host
// agent of the running guest writes the same file. Then the maximum is
// returned when the kernel maps it, else the largest whole GiB it maps if
// that is at least bootFloor (with a notice), else a BootMemoryError.
func PrepareBoot(ctx context.Context, req BootRequest) (BootDecision, error) {
	req.defaults()
	decision := BootDecision{Maximum: req.Maximum}
	m, ok := req.Machine()
	if !ok || req.Maximum == 0 {
		return decision, nil
	}
	mode := m.Overcommit
	fits := func() bool {
		if m.Overcommit < 0 {
			// Both rules at once decide without reading the mode.
			if m.Available >= m.cushion(req.Maximum) && m.maps(req.Maximum) && m.Committed+req.Maximum+bootOverhead <= m.CommitLimit {
				return true
			}
			if mode < 0 {
				mode = req.Overcommit()
			}
			m.Overcommit = mode
		}
		return m.Available >= m.cushion(req.Maximum) && m.maps(req.Maximum)
	}
	if fits() {
		return decision, nil
	}
	growable := req.Growable()
	if growable && req.Profile != "" {
		decision.Asked = true
		report := filepath.Join(req.Profile, "machine-memory.json")
		defer withdrawBootRequest(report)
		started, published := req.Now(), time.Time{}
		req.Progress("正在向服务器申请内存…")
		for {
			now := req.Now()
			if now.Sub(started) >= bootWait {
				break
			}
			if published.IsZero() || now.Sub(published) >= bootRefresh {
				_ = writeJSONAtomic(report, machineMemoryReport{Version: 1, UpdatedAt: now.UTC(), Short: m.startShort(req.Maximum),
					Wanted: req.Maximum, Pressure: true, Reason: BootRequestReason})
				published = now
			}
			if err := req.Sleep(ctx, bootPoll); err != nil {
				return decision, err
			}
			next, ok := req.Machine()
			if !ok {
				break
			}
			if next.Overcommit < 0 {
				next.Overcommit = mode
			}
			if m = next; fits() {
				break
			}
		}
		decision.Waited = req.Now().Sub(started)
		if fits() {
			return decision, nil
		}
	}
	if m.maps(req.Maximum) {
		// The kernel maps it; what the machine lacks is room for the guest's
		// first pages, which the running guest's host agent asks for.
		return decision, nil
	}
	if largest := m.largest(req.Maximum); largest >= bootFloor {
		decision.Maximum = largest
		if growable {
			decision.Notice = fmt.Sprintf("服务器暂时分不出更多内存，环境按 %s 内存上限启动（设置为 %s），之后重启环境即可恢复。", gb(largest), gb(req.Maximum))
		} else {
			decision.Notice = fmt.Sprintf("这台机器现在内存不够，环境按 %s 内存上限启动（设置为 %s），关闭其他程序后重启环境即可恢复。", gb(largest), gb(req.Maximum))
		}
		return decision, nil
	}
	have := m.Total + m.SwapTotal
	if m.Overcommit == 2 {
		have = m.CommitLimit - min(m.CommitLimit, m.Committed)
	}
	return decision, &BootMemoryError{Need: min(req.Maximum, bootFloor) + bootOverhead, Have: have, Growable: growable}
}

// withdrawBootRequest removes a start's request, and nothing the running
// guest's host agent wrote in its place.
func withdrawBootRequest(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	var report machineMemoryReport
	err = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&report)
	f.Close()
	if err == nil && report.Reason == BootRequestReason {
		_ = os.Remove(path)
	}
}

// JobMessagePrefix starts a line a Super Cage job's child prints to set the
// job's message (the workbench hides the line from the job's log).
const JobMessagePrefix = "CAGE_JOB_MESSAGE "

// JobMessage shows a step of an operation: as the job's message when a
// Super Cage job runs it (JobEnv is set), as a plain line otherwise.
func JobMessage(w io.Writer, message string) {
	message = strings.ReplaceAll(message, "\n", " ")
	if os.Getenv(JobEnv) != "" {
		fmt.Fprintln(w, JobMessagePrefix+message)
		return
	}
	fmt.Fprintln(w, message)
}
