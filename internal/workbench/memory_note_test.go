package workbench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

const testGiB = uint64(1) << 30

// writeBrowserHeld publishes a resource status in which the governor holds
// the browser at its limit: its class stalls (browser_pressure) while the
// protected classes do not (pressure false), and automatic sizing could give
// the environment more than it has now.
func writeBrowserHeld(t *testing.T, s *Server, possible uint64) {
	t.Helper()
	now := time.Now()
	status := cage.ResourceStatus{Version: 1, UpdatedAt: now, ObservedAt: now, Maximum: 4 * testGiB, Target: 4 * testGiB, CPUs: 3,
		Pressure: false, BrowserPressure: true, Possible: possible, Classes: heldBrowserClasses(),
		Guest: &cage.ResourceSample{Total: 4 * testGiB, Available: 900 << 20, Stall: 0.4, Protected: true, ProtectedStall: 0.2, BrowserStall: 31}}
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-resources.json"), status); err != nil {
		t.Fatal(err)
	}
}

// heldBrowserClasses are the governor's classes with the browser at its
// soft limit (memory.high), where it is throttled.
func heldBrowserClasses() []cage.GuestClass {
	return []cage.GuestClass{{Name: "control", Current: 400 << 20}, {Name: "sessions", Current: 900 << 20},
		{Name: "browser", Current: 2*testGiB - 20<<20, Stall: 31, High: 2 * testGiB, Max: 2*testGiB + 512<<20}}
}

func joinedAdvice(advice []string) string { return strings.Join(advice, "\n") }

func TestRecoveryWaitsWhileTheBrowserIsHeldAtItsLimit(t *testing.T) {
	s, gate := failedMonitoringServer(t)
	t.Cleanup(s.jobs.close)
	checks := 0
	replaceRecoveryCheck(t, func(s *Server) error {
		checks++
		return grantFreshCheck(s, gate)
	})
	s.recovery.Next = time.Now().Add(-time.Second)
	round := func() bool {
		writeBrowserHeld(t, s, 6*testGiB)
		s.op.Lock()
		s.recoverNetwork()
		job := s.jobs.snapshot()
		s.op.Unlock()
		admitted := job.Running && job.Kind == "network-recovery"
		for deadline := time.Now().Add(10 * time.Second); s.jobs.snapshot().Running && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		return admitted
	}
	if round() || checks != 0 || s.recovery.pressureSince.IsZero() {
		t.Fatalf("a browser held at its limit did not postpone the recheck: checks=%d recovery=%+v", checks, s.recovery)
	}
	e := s.safety.Explanation
	if e == nil || e.State != networkWaitingMemory || !e.MemoryShort || e.RecheckAt == nil ||
		!strings.Contains(e.Reason, "隔离浏览器占用的内存超过了环境能分给它的上限") {
		t.Fatalf("explanation while waiting: %+v", e)
	}
	if advice := joinedAdvice(e.Advice); !strings.Contains(advice, "关闭或刷新占用内存较多的标签页") || !strings.Contains(advice, "可以把内存调到 6 GB") {
		t.Fatalf("advice: %q", advice)
	}
	if e.MemoryCause != memoryCauseBrowser {
		t.Fatalf("memory_cause: %q", e.MemoryCause)
	}
	if label := s.networkStateLabel("paused", "外网未放行"); label != "外网未放行：浏览器内存不足，等待自动重检" {
		t.Fatalf("proxy label: %s", label)
	}
	// The wait is bounded: past it the full check runs, and passes.
	s.recovery.pressureSince = time.Now().Add(-recoveryPressureWait - time.Second)
	if !round() || checks != 1 || gate.Status().Mode != cage.GateOpen {
		t.Fatalf("the recheck did not run after the bound: checks=%d gate=%+v", checks, gate.Status())
	}
}

func TestUnconfirmedBrowserReadExplainsTheHeldBrowser(t *testing.T) {
	for _, tc := range []struct {
		name      string
		manual    bool
		possible  uint64
		advice    string
		notAdvice string
	}{
		{"manual allocation below automatic", true, 6 * testGiB, "改为自动（推荐）后可以调到 6 GB", "可以把内存调到"},
		{"automatic, more after a restart", false, 6 * testGiB, "可以把内存调到 6 GB", "改为自动"},
		{"already at the most this machine gives", false, 4 * testGiB, "关闭或刷新占用内存较多的标签页", "调到"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			if tc.manual {
				if err := writePrivate(cage.ResourceAllocationPath(s.configPath), cage.ResourceAllocation{MemoryGiB: 4, CPUs: 2, Chosen: cage.OriginUser}); err != nil {
					t.Fatal(err)
				}
			}
			writeBrowserHeld(t, s, tc.possible)
			fakeMonitorReads(t, func(context.Context) (guestObservation, error) { return matchingGuestSettings(cfg), nil },
				func(ctx context.Context) (browserObservation, error) {
					return browserObservation{}, noAnswer(context.DeadlineExceeded)
				})
			s.monitorSafetyWithProbe(exitProbeOK)
			if state := gate.Status(); state.Mode != cage.GateOpen {
				t.Fatalf("gate: %+v", state)
			}
			e := s.safety.Explanation
			if e == nil || e.State != networkUnconfirmed || !e.MemoryShort ||
				!strings.HasPrefix(e.Reason, "隔离浏览器占用的内存超过了环境能分给它的上限") || !strings.Contains(e.Reason, "无法读取浏览器时区和语言") {
				t.Fatalf("explanation: %+v", e)
			}
			if advice := joinedAdvice(e.Advice); !strings.Contains(advice, tc.advice) || strings.Contains(advice, tc.notAdvice) {
				t.Fatalf("advice %q, want %q and not %q", advice, tc.advice, tc.notAdvice)
			}
		})
	}
}

func TestMemoryNoteReadsTheAllocator(t *testing.T) {
	now := time.Now()
	guest := &cage.ResourceSample{Total: 4 * testGiB, Available: 300 << 20}
	held := heldBrowserClasses()
	linux := memorySizing{applies: true}
	for _, tc := range []struct {
		name          string
		status        cage.ResourceStatus
		sizing        memorySizing
		cause, reason string
		advice        []string
		notAdvice     string
	}{
		{"short at a manual maximum: the allocator's own advice",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, Pressure: true, Short: true, LimitedBy: "maximum", Maximum: 4 * testGiB, Possible: 6 * testGiB,
				Advice: &cage.ResourceAdvice{Message: "环境内存已用满手动设置的 4 GB。改为自动（推荐）后可以调到 6 GB，需要重启环境。", MemoryBytes: 6 * testGiB, RestartRequired: true}},
			memorySizing{applies: true, manual: true}, memoryCauseEnvironment, "环境内存不足（可用约 300 MB，共 4.0 GB）",
			[]string{"改为自动（推荐）后可以调到 6 GB"}, "调高内存上限"},
		{"short, held by the machine",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, Short: true, LimitedBy: "machine", Maximum: 6 * testGiB, Possible: 6 * testGiB},
			linux, memoryCauseEnvironment, "环境内存不足", []string{"这台机器暂时没有更多内存可给环境"}, "调到"},
		{"browser held at its limit, automatic gives more",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, BrowserPressure: true, Classes: held, Maximum: 4 * testGiB, Possible: 6 * testGiB},
			linux, memoryCauseBrowser, "隔离浏览器占用的内存超过了环境能分给它的上限",
			[]string{"关闭或刷新占用内存较多的标签页", "可以把内存调到 6 GB"}, "结束环境里不需要的程序"},
		{"browser held, manual allocation",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, BrowserPressure: true, Classes: held, Maximum: 4 * testGiB, Possible: 6 * testGiB},
			memorySizing{applies: true, manual: true}, memoryCauseBrowser, "隔离浏览器",
			[]string{"改为自动（推荐）后可以调到 6 GB"}, "可以把内存调到"},
		{"browser held, older host agent: this machine's automatic size",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, BrowserPressure: true, Classes: held, Maximum: 3 * testGiB},
			memorySizing{applies: true, automatic: 6 * testGiB}, memoryCauseBrowser, "隔离浏览器", []string{"可以把内存调到 6 GB"}, "改为自动"},
		{"browser held, nothing more to give",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, BrowserPressure: true, Classes: held, Maximum: 6 * testGiB, Possible: 6 * testGiB},
			linux, memoryCauseBrowser, "隔离浏览器", []string{"关闭或刷新占用内存较多的标签页"}, "调到"},
		{"browser stalls below its limit: short of memory, not held",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, BrowserPressure: true, Classes: []cage.GuestClass{{Name: "browser", Current: testGiB, Stall: 2, High: 2 * testGiB}}, Maximum: 4 * testGiB, Possible: 6 * testGiB},
			linux, memoryCauseBrowser, "隔离浏览器内存紧张", []string{"关闭或刷新占用内存较多的标签页"}, "上限"},
		{"browser pressure from a stale reading is not shown",
			cage.ResourceStatus{ObservedAt: now.Add(-time.Minute), Guest: guest, BrowserPressure: true, Error: "读取资源用量失败"},
			linux, "", "", nil, ""},
		{"protected classes stall: the environment, not the browser",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, Pressure: true, BrowserPressure: true, Maximum: 4 * testGiB, Possible: 6 * testGiB},
			linux, memoryCauseEnvironment, "环境内存不足", []string{"结束环境里不需要的程序", "可以把内存调到 6 GB"}, ""},
		{"a guest the host agent does not size keeps the general advice",
			cage.ResourceStatus{ObservedAt: now, Guest: guest, Pressure: true},
			memorySizing{}, memoryCauseEnvironment, "环境内存不足", []string{"需要时调高内存上限"}, "调到"},
		{"host short", cage.ResourceStatus{ObservedAt: now, Guest: guest, HostPressure: true}, linux, memoryCauseMachine, "这台机器内存不足", []string{"关闭这台机器上其他占用内存较多的程序"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := memoryNote(tc.status, nil, now, tc.sizing)
			if note.Cause != tc.cause || note.Short != (tc.cause != "") || !strings.Contains(note.Reason, tc.reason) {
				t.Fatalf("note: %+v", note)
			}
			if held := tc.status.Classes == nil || tc.status.Classes[len(tc.status.Classes)-1].Current+64<<20 >= tc.status.Classes[len(tc.status.Classes)-1].High; !held && strings.Contains(note.Reason, "上限") {
				t.Fatalf("a browser below its limit is said to be held: %+v", note)
			}
			advice := joinedAdvice(note.Advice)
			for _, want := range tc.advice {
				if !strings.Contains(advice, want) {
					t.Fatalf("advice %q lacks %q", advice, want)
				}
			}
			if tc.notAdvice != "" && strings.Contains(advice, tc.notAdvice) {
				t.Fatalf("advice %q has %q", advice, tc.notAdvice)
			}
		})
	}
}

func TestRecoveryDoesNotWaitForABrowserBelowItsLimit(t *testing.T) {
	s, gate := failedMonitoringServer(t)
	t.Cleanup(s.jobs.close)
	checks := 0
	replaceRecoveryCheck(t, func(s *Server) error {
		checks++
		return grantFreshCheck(s, gate)
	})
	s.recovery.Next = time.Now().Add(-time.Second)
	now := time.Now()
	status := cage.ResourceStatus{Version: 1, UpdatedAt: now, ObservedAt: now, Maximum: 4 * testGiB, Target: 4 * testGiB, BrowserPressure: true,
		Classes: []cage.GuestClass{{Name: "browser", Current: testGiB, Stall: 2, High: 2 * testGiB}},
		Guest:   &cage.ResourceSample{Total: 4 * testGiB, Available: 2 * testGiB, Protected: true, BrowserStall: 2}}
	if err := writePrivate(filepath.Join(s.instanceDir(), "cage-resources.json"), status); err != nil {
		t.Fatal(err)
	}
	s.op.Lock()
	s.recoverNetwork()
	s.op.Unlock()
	for deadline := time.Now().Add(10 * time.Second); s.jobs.snapshot().Running && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if checks != 1 || gate.Status().Mode != cage.GateOpen {
		t.Fatalf("a mild browser stall postponed the recheck: checks=%d recovery=%+v", checks, s.recovery)
	}
}
