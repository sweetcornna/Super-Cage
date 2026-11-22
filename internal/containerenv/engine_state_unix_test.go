//go:build !windows

package containerenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingEngine answers ls, inspect and info for one stopped container and
// logs each call. lsDelay slows the ls answer; failures makes the first
// ls calls fail.
func countingEngine(t *testing.T, lsDelay string, failures int) (*Runtime, func(prefix string) int) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	config := fixtureConfig()
	config.Engine, config.Socket, config.Executable = "podman", "", filepath.Join(dir, "podman")
	r := newRuntime(t.TempDir(), config)
	inspect := fmt.Sprintf(`[{"Id":"c1","Config":{"Labels":{%q:%q}},"HostConfig":{"NanoCpus":%d,"Memory":%d}}]`, profileLabel, r.identity, int64(config.CPUs)*1e9, config.MemoryBytes)
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\ncase \"$1 $2\" in\n" +
		"'container ls') sleep " + lsDelay + "; n=$(grep -c '^container ls' '" + calls + "'); if [ \"$n\" -le " + fmt.Sprint(failures) + " ]; then echo 'engine busy' >&2; exit 1; fi; echo c1 ;;\n" +
		"'container inspect') printf '%s' '" + inspect + "' ;;\n" +
		"'info --format') echo /var/lib/containers/storage ;;\nesac\n"
	if err := os.WriteFile(config.Executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	count := func(prefix string) int {
		data, _ := os.ReadFile(calls)
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, prefix) {
				n++
			}
		}
		return n
	}
	return r, count
}

func expireStateReuse(r *Runtime) {
	stateCache.Lock()
	defer stateCache.Unlock()
	if read := stateCache.reads[r.cacheKey()]; read != nil {
		read.finished = read.finished.Add(-stateReuse)
	}
}

func TestStateReusesARecentRead(t *testing.T) {
	r, count := countingEngine(t, "0", 0)
	ctx := context.Background()
	for range 3 {
		state, err := r.State(ctx)
		if err != nil || !state.Exists || state.ContainerID != "c1" || state.StoragePath != "/var/lib/containers/storage" {
			t.Fatalf("state %+v err %v", state, err)
		}
	}
	if n := count("container ls"); n != 1 {
		t.Fatalf("three status reads ran ls %d times", n)
	}
	expireStateReuse(r)
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if count("container ls") != 2 || count("info") != 1 {
		t.Fatalf("after expiry: ls=%d info=%d, want 2 and 1 (storage path kept longer)", count("container ls"), count("info"))
	}
}

func TestStateSharesOneReadBetweenConcurrentCallers(t *testing.T) {
	r, count := countingEngine(t, "0.3", 0)
	var group sync.WaitGroup
	for range 6 {
		group.Add(1)
		go func() {
			defer group.Done()
			if state, err := r.State(context.Background()); err != nil || state.ContainerID != "c1" {
				t.Errorf("state %+v err %v", state, err)
			}
		}()
	}
	group.Wait()
	if n := count("container ls"); n != 1 {
		t.Fatalf("six concurrent reads ran ls %d times", n)
	}
	// A caller whose own deadline passes stops waiting for the shared read.
	expireStateReuse(r)
	background := make(chan struct{})
	go func() { defer close(background); _, _ = r.State(context.Background()) }()
	defer func() { <-background }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := r.State(ctx); err == nil || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("short caller returned %v after %s", err, time.Since(started))
	}
}

func TestStateRereadsWhenStartOrStopChangesTheRecords(t *testing.T) {
	r, count := countingEngine(t, "0", 0)
	ctx := context.Background()
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// The network worker publishes this record when a start completes.
	if err := os.WriteFile(filepath.Join(r.InstanceDir(), "network.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	// Stop removes it again.
	if err := os.Remove(filepath.Join(r.InstanceDir(), "network.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count("container ls"); n != 3 {
		t.Fatalf("record changes were not seen: ls ran %d times, want 3", n)
	}
}

func TestStateRereadsAfterAnEngineChangeAndNeverReusesFailures(t *testing.T) {
	r, count := countingEngine(t, "0", 1)
	ctx := context.Background()
	if _, err := r.State(ctx); err == nil {
		t.Fatal("fixture engine did not fail first")
	}
	if _, err := r.State(ctx); err != nil {
		t.Fatalf("a failed read was reused: %v", err)
	}
	if _, err := r.output(ctx, "stop", "--time", "20", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count("container ls"); n != 3 {
		t.Fatalf("ls ran %d times, want 3 (failure, read, after stop)", n)
	}
}

func TestFreshStateAlwaysAsksTheEngine(t *testing.T) {
	r, count := countingEngine(t, "0", 0)
	ctx := context.Background()
	for range 2 {
		if _, err := r.FreshState(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count("container ls"); n != 2 {
		t.Fatalf("ls ran %d times, want 2: fresh reads always run and refresh the reused state", n)
	}
}
