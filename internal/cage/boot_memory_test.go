package cage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bootClock is a fake clock whose Sleep advances it and lets the machine
// change.
type bootClock struct {
	now    time.Time
	slept  time.Duration
	sleeps int
	onTick func(elapsed time.Duration)
}

func (c *bootClock) Now() time.Time { return c.now }
func (c *bootClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now, c.slept, c.sleeps = c.now.Add(d), c.slept+d, c.sleeps+1
	if c.onTick != nil {
		c.onTick(c.slept)
	}
	return nil
}

// The incident's workspace VM at the guarantee: limits.memory 2048 MiB,
// MemTotal 1738 MiB, no swap, overcommit heuristic; the nested guest's boot
// maximum is 7 GiB.
func incidentMachine() BootMachine {
	return BootMachine{Total: 1738 * mib, Available: 1200 * mib, CommitLimit: 869 * mib, Committed: 900 * mib, Overcommit: -1}
}

func readReport(t *testing.T, path string) (machineMemoryReport, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return machineMemoryReport{}, false
	}
	var report machineMemoryReport
	if err != nil || json.Unmarshal(data, &report) != nil {
		t.Fatalf("machine-memory.json %q: %v", data, err)
	}
	return report, true
}

func TestBootFastPathWritesAndWaitsForNothing(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	// The workspace VM at its boot size: 9.4 GiB, the 7 GiB guest maps.
	if err := os.WriteFile(meminfo, []byte("MemTotal:        9856000 kB\nMemFree:         9000000 kB\nMemAvailable:    9100000 kB\nSwapTotal:             0 kB\nCommitLimit:     4928000 kB\nCommitted_AS:     900000 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		mode      int
		modeReads int
	}{{"heuristic", 0, 1}, {"always", 1, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			modeReads, other := 0, 0
			clock := &bootClock{now: time.Unix(1e9, 0)}
			decision, err := PrepareBoot(context.Background(), BootRequest{Profile: dir, Maximum: 7 * gib,
				Machine:    func() (BootMachine, bool) { return parseBootMeminfo(meminfo) },
				Overcommit: func() int { modeReads++; return tc.mode },
				Growable:   func() bool { other++; return true },
				Progress:   func(string) { other++ },
				Now:        clock.Now, Sleep: clock.Sleep})
			if err != nil || decision.Maximum != 7*gib || decision.Asked || decision.Notice != "" || clock.sleeps != 0 || other != 0 || modeReads != tc.modeReads {
				t.Fatalf("fast path: %+v %v, sleeps %d, mode reads %d, other calls %d", decision, err, clock.sleeps, modeReads, other)
			}
			if _, written := readReport(t, filepath.Join(dir, "machine-memory.json")); written {
				t.Fatal("the fast path published a request")
			}
		})
	}
	// Both rules pass: the mode is not even read.
	big := BootMachine{Total: 32 * gib, Available: 30 * gib, CommitLimit: 40 * gib, Committed: gib, Overcommit: -1}
	decision, err := PrepareBoot(context.Background(), BootRequest{Profile: dir, Maximum: 7 * gib,
		Machine: func() (BootMachine, bool) { return big, true }, Overcommit: func() int { t.Fatal("mode read"); return 0 }})
	if err != nil || decision.Maximum != 7*gib {
		t.Fatal(decision, err)
	}
	// Not measured (macOS, Windows): as before.
	decision, err = PrepareBoot(context.Background(), BootRequest{Maximum: 7 * gib, Machine: func() (BootMachine, bool) { return BootMachine{}, false }})
	if err != nil || decision.Maximum != 7*gib {
		t.Fatal(decision, err)
	}
}

func TestBootAsksTheOuterLevelWaitsAndStarts(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "machine-memory.json")
	machine := incidentMachine()
	var progress []string
	var seen []machineMemoryReport
	clock := &bootClock{now: time.Unix(1e9, 0)}
	clock.onTick = func(elapsed time.Duration) {
		if r, ok := readReport(t, report); ok && (len(seen) == 0 || !r.UpdatedAt.Equal(seen[len(seen)-1].UpdatedAt)) {
			seen = append(seen, r)
		}
		if elapsed == 17*time.Second {
			// The gateway's round grew the VM to 8960 MiB.
			machine = BootMachine{Total: 8650 * mib, Available: 8100 * mib, CommitLimit: 4325 * mib, Committed: 900 * mib, Overcommit: -1}
		}
	}
	decision, err := PrepareBoot(context.Background(), BootRequest{Profile: dir, Maximum: 7 * gib,
		Machine: func() (BootMachine, bool) { return machine, true }, Overcommit: func() int { return 0 }, Growable: func() bool { return true },
		Progress: func(message string) { progress = append(progress, message) }, Now: clock.Now, Sleep: clock.Sleep})
	if err != nil || decision.Maximum != 7*gib || !decision.Asked || decision.Waited != 17*time.Second || decision.Notice != "" {
		t.Fatalf("decision %+v %v", decision, err)
	}
	if len(progress) != 2 || progress[0] != "正在向服务器申请内存…" || progress[1] != "已获得所需内存，继续启动环境…" {
		t.Fatalf("progress %q", progress)
	}
	// Published at once and rewritten every 10 s, each fresh for the gateway:
	// 7 GiB + 256 MiB beyond the 538 MiB working set, as a shortage.
	if len(seen) != 2 || seen[1].UpdatedAt.Sub(seen[0].UpdatedAt) != 10*time.Second {
		t.Fatalf("requests %+v", seen)
	}
	for _, r := range seen {
		if r.Version != 1 || r.Short != 6912*mib || r.Wanted != 7*gib || !r.Pressure || r.Reason != BootRequestReason {
			t.Fatalf("request %+v", r)
		}
	}
	if _, left := readReport(t, report); left {
		t.Fatal("the request outlived the wait")
	}
}

func TestBootFallsBackOrRefusesWhenNoMemoryComes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		machine  BootMachine
		growable bool
		maximum  uint64
		notice   string
		refusal  string
		waited   time.Duration
	}{
		// The gateway granted part of it (or ignores the request: 0.1.15).
		{"partial grant", BootMachine{Total: 5 * gib, Available: 4 * gib, Overcommit: 0}, true, 4 * gib, "环境按 4 GB 内存上限启动（设置为 7 GB）", "", bootWait},
		{"nothing came", incidentMachine(), true, 0, "", "工作区至少需要约 2.2 GB，现有 1.7 GB，服务器暂时分不出更多内存", bootWait},
		// No outer level (a desktop): nothing to ask, no wait.
		{"desktop", BootMachine{Total: 5 * gib, Available: 3 * gib, Overcommit: 0}, false, 4 * gib, "这台机器现在内存不够，环境按 4 GB 内存上限启动", "", 0},
		{"small desktop", BootMachine{Total: 2 * gib, Available: gib, Overcommit: 0}, false, 0, "", "至少需要约 2.2 GB，这台机器现有 2 GB；请关闭其他程序后重试", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := &bootClock{now: time.Unix(1e9, 0)}
			decision, err := PrepareBoot(context.Background(), BootRequest{Profile: dir, Maximum: 7 * gib,
				Machine: func() (BootMachine, bool) { return tc.machine, true }, Overcommit: func() int { return 0 }, Growable: func() bool { return tc.growable },
				Now: clock.Now, Sleep: clock.Sleep})
			if clock.slept != tc.waited || decision.Asked != tc.growable {
				t.Fatalf("waited %s, asked %v", clock.slept, decision.Asked)
			}
			if _, left := readReport(t, filepath.Join(dir, "machine-memory.json")); left {
				t.Fatal("request left behind")
			}
			var refused *BootMemoryError
			if tc.refusal != "" {
				if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), BootMemoryMarker+"：") || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("refusal %v", err)
				}
				return
			}
			if err != nil || decision.Maximum != tc.maximum || !strings.Contains(decision.Notice, tc.notice) {
				t.Fatalf("fallback %+v %v", decision, err)
			}
		})
	}
}

func TestBootRequestEndsWithTheStart(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "machine-memory.json")
	ctx, cancel := context.WithCancel(context.Background())
	clock := &bootClock{now: time.Unix(1e9, 0)}
	clock.onTick = func(elapsed time.Duration) {
		if elapsed == 3*time.Second {
			cancel()
		}
	}
	_, err := PrepareBoot(ctx, BootRequest{Profile: dir, Maximum: 7 * gib, Machine: func() (BootMachine, bool) { return incidentMachine(), true },
		Overcommit: func() int { return 0 }, Growable: func() bool { return true }, Now: clock.Now, Sleep: clock.Sleep})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, left := readReport(t, report); left {
		t.Fatal("a canceled start left its request")
	}
	// What the running guest's host agent wrote in the meantime stays.
	if err := writeJSONAtomic(report, machineMemoryReport{Version: 1, UpdatedAt: time.Now(), Short: gib, Pressure: true}); err != nil {
		t.Fatal(err)
	}
	withdrawBootRequest(report)
	if _, kept := readReport(t, report); !kept {
		t.Fatal("removed the host agent's report")
	}
}

func TestBootOvercommitModes(t *testing.T) {
	// Always (1): any mapping; the guest's floor still has to fit, and the
	// outer level is asked for that cushion only.
	always := BootMachine{Total: 1738 * mib, Available: 1200 * mib, Overcommit: 1}
	if !always.maps(32*gib) || always.largest(7*gib) != 7*gib || always.startShort(7*gib) != roundUpUnit(2*gib+256*mib) {
		t.Fatalf("always: %+v short %d", always, always.startShort(7*gib)>>20)
	}
	// Never (2): CommitLimit = swap + 50 % of MemTotal; 7 GiB + overhead on
	// top of 1 GiB committed needs a 16.5 GiB MemTotal.
	never := BootMachine{Total: 8 * gib, Available: 7 * gib, CommitLimit: 4 * gib, Committed: gib, Overcommit: 2}
	if never.maps(7*gib) || !never.maps(2*gib+512*mib) || never.largest(7*gib) != 2*gib {
		t.Fatalf("never: %+v", never)
	}
	if need := never.needTotal(7 * gib); need < 16*gib+512*mib || need > 16*gib+512*mib+mib {
		t.Fatalf("never: need %d MiB", need>>20)
	}
	// Heuristic (0) with swap: swap counts.
	swap := BootMachine{Total: 4 * gib, Available: 3 * gib, SwapTotal: 4 * gib, Overcommit: 0}
	if !swap.maps(7*gib) || swap.maps(8*gib) {
		t.Fatalf("heuristic with swap: %+v", swap)
	}
	// The incident: MemTotal must reach 7.25 GiB, 6886 MiB beyond the 538 MiB
	// working set, rounded to the 256 MiB unit.
	incident := incidentMachine()
	incident.Overcommit = 0
	if incident.maps(7*gib) || incident.largest(7*gib) != gib || incident.startShort(7*gib) != 6912*mib {
		t.Fatalf("incident: largest %d MiB short %d MiB", incident.largest(7*gib)>>20, incident.startShort(7*gib)>>20)
	}
}

func TestBootMeminfoParsing(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"MemTotal: 1000 kB\nMemAvailable: 500 kB\nSwapTotal: 0 kB\nCommitLimit: 500 kB\nCommitted_AS: 10 kB\n", true},
		{"MemTotal: 1000 kB\nMemAvailable: 500 kB\n", false},
		{"MemTotal: 1000 kB\nMemAvailable: 5000 kB\nSwapTotal: 0 kB\nCommitLimit: 500 kB\nCommitted_AS: 10 kB\n", false},
		{"MemTotal: x kB\nMemAvailable: 500 kB\nSwapTotal: 0 kB\nCommitLimit: 500 kB\nCommitted_AS: 10 kB\n", false},
	} {
		path := filepath.Join(dir, "meminfo")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, ok := parseBootMeminfo(path); ok != tc.ok || ok && (m.Total != 1000<<10 || m.Committed != 10<<10 || m.Overcommit != -1) {
			t.Fatalf("%q: %+v %v", tc.body, m, ok)
		}
	}
	old := bootOvercommit
	t.Cleanup(func() { bootOvercommit = old })
	for body, want := range map[string]int{"0\n": 0, "1\n": 1, "2\n": 2, "7\n": 0, "": 0} {
		bootOvercommit = filepath.Join(dir, "overcommit")
		if err := os.WriteFile(bootOvercommit, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if mode := readOvercommit(); mode != want {
			t.Fatalf("overcommit %q = %d", body, mode)
		}
	}
}

func TestBootIncusVMDetection(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if isIncusVM(missing, missing, missing) {
		t.Fatal("missing paths detected as Incus VM")
	}
	port := filepath.Join(dir, "org.linuxcontainers.incus")
	if err := os.WriteFile(port, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !isIncusVM(port, missing, missing) {
		t.Fatal("Incus virtio-serial port not detected")
	}
	vendor := filepath.Join(dir, "board_vendor")
	name := filepath.Join(dir, "board_name")
	if err := os.WriteFile(vendor, []byte("LinuxContainers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("Incus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isIncusVM(missing, vendor, name) {
		t.Fatal("Incus DMI identity not detected")
	}
	for _, tc := range []struct {
		vendor, name string
	}{
		{"Dell Inc.\n", "Incus\n"},
		{"LinuxContainers\n", "LXD\n"},
		{"QEMU\n", "Standard PC\n"},
	} {
		if err := os.WriteFile(vendor, []byte(tc.vendor), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(tc.name), 0o600); err != nil {
			t.Fatal(err)
		}
		if isIncusVM(missing, vendor, name) {
			t.Fatalf("non-Incus DMI %q/%q detected as Incus VM", tc.vendor, tc.name)
		}
	}
}

func TestJobMessageLine(t *testing.T) {
	var out strings.Builder
	t.Setenv(JobEnv, "")
	JobMessage(&out, "正在向服务器申请内存…")
	t.Setenv(JobEnv, "job-1")
	JobMessage(&out, "a\nb")
	if out.String() != "正在向服务器申请内存…\n"+JobMessagePrefix+"a b\n" {
		t.Fatalf("%q", out.String())
	}
}

// BenchmarkBootFastPath is what a start of a machine that is big enough adds:
// one read of meminfo (and the overcommit mode when the two rules disagree).
func BenchmarkBootFastPath(b *testing.B) {
	dir := b.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	// A real 9.4 GiB guest's /proc/meminfo, 56 lines.
	body := "MemTotal:        9856000 kB\nMemFree:         8000000 kB\nMemAvailable:    9100000 kB\n" + strings.Repeat("Filler:          1234567 kB\n", 50) +
		"SwapTotal:             0 kB\nCommitLimit:     4928000 kB\nCommitted_AS:     900000 kB\n"
	if err := os.WriteFile(meminfo, []byte(body), 0o600); err != nil {
		b.Fatal(err)
	}
	old := bootOvercommit
	b.Cleanup(func() { bootOvercommit = old })
	bootOvercommit = filepath.Join(dir, "overcommit")
	if err := os.WriteFile(bootOvercommit, []byte("0\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	req := BootRequest{Profile: dir, Maximum: 7 * gib, Machine: func() (BootMachine, bool) { return parseBootMeminfo(meminfo) }}
	b.ResetTimer()
	for range b.N {
		if decision, err := PrepareBoot(context.Background(), req); err != nil || decision.Maximum != 7*gib {
			b.Fatal(decision, err)
		}
	}
}
