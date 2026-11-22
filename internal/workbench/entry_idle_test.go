//go:build !windows

package workbench

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"syscall"
	"testing"
	"time"
)

// entryCode marks a goroutine stack in the entry's code: the ssh endpoint,
// kept terminals, the Herdr filter and the terminal filter. The harness's
// stand-in for the environment's sshd (entryGuest) is not the entry's.
var (
	entryCode    = regexp.MustCompile(`workbench\.(\(\*Server\)\.entry|entry|\(\*entry)|herdrlink\.|termfilter\.`)
	entryHarness = regexp.MustCompile(`workbench\.\(\*entryGuest\)`)
)

// entryCodeStacks is the goroutine stacks in the entry's code.
func entryCodeStacks(dump []byte) []byte {
	var out [][]byte
	for _, stack := range bytes.Split(dump, []byte("\n\n")) {
		if entryCode.Match(stack) && !entryHarness.Match(stack) {
			out = append(out, stack)
		}
	}
	return bytes.Join(out, []byte("\n\n"))
}

// entryCPU is the user and system time this process has used.
func entryCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// Design 5.13: with the entry on and nothing connected (no ssh, no Herdr),
// nothing of it runs: no goroutine is in the entry's code, the kept
// terminals' idle check is not armed, nothing reached the environment, and
// the process spends next to no CPU while it waits. The figures are logged
// (CAGE_ENTRY_IDLE_OUT keeps them as JSON).
func TestEntryOnAndIdleRunsNothing(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	var stacks bytes.Buffer
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		stacks.Reset()
		_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
		if len(entryCodeStacks(stacks.Bytes())) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry code runs with nothing connected:\n%s", entryCodeStacks(stacks.Bytes()))
		}
	}
	before, goroutines := entryCPU(t), runtime.NumGoroutine()
	const idle = 5 * time.Second
	time.Sleep(idle)
	cpu := entryCPU(t) - before
	stacks.Reset()
	_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
	if len(entryCodeStacks(stacks.Bytes())) != 0 {
		t.Fatalf("entry code ran while idle:\n%s", entryCodeStacks(stacks.Bytes()))
	}
	if f.s.entryKeptTimerRunning() || f.guest.accepted.Load() != 0 || f.s.entry.registry.counts().Total != 0 {
		t.Fatalf("idle: kept timer %v, %d guest connections, %+v", f.s.entryKeptTimerRunning(), f.guest.accepted.Load(), f.s.entry.registry.counts())
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Fatalf("goroutines went from %d to %d while idle", goroutines, after)
	}
	results := map[string]any{"idle_s": idle.Seconds(), "process_cpu_ms": float64(cpu.Microseconds()) / 1000, "goroutines": goroutines}
	t.Logf("entry on, nothing connected, %v: %+v", idle, results)
	if out := os.Getenv("CAGE_ENTRY_IDLE_OUT"); out != "" {
		data, _ := json.MarshalIndent(results, "", "  ")
		_ = os.WriteFile(out, data, 0o600)
	}
}
