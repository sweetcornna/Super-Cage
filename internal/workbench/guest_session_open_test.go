//go:build !windows

package workbench

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowGuestSession starts the real session server after a delay, like a
// guest whose SSH login is slow.
func slowGuestSession(t *testing.T, delay string) (*guestSession, context.Context, func(context.Context) *exec.Cmd, *atomic.Int32) {
	t.Helper()
	for _, name := range []string{"python3", "bash"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is unavailable")
		}
	}
	lifetime, cancel := context.WithCancel(context.Background())
	session, starts := &guestSession{}, &atomic.Int32{}
	t.Cleanup(func() { session.close(); cancel() })
	command := func(ctx context.Context) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "bash", "-c", "sleep "+delay+"; exec python3 -I -u -c \"$0\"", guestSessionServer)
	}
	return session, lifetime, command, starts
}

func TestGuestSessionSlowOpenHonoursEachCallersDeadline(t *testing.T) {
	session, lifetime, command, starts := slowGuestSession(t, "1")
	var group sync.WaitGroup
	defer group.Wait()
	for _, name := range []string{"a", "b"} {
		group.Add(1)
		go func() {
			defer group.Done()
			reply, sent, err := session.run(context.Background(), lifetime, command, "printf "+name)
			if err != nil || !sent || string(reply.stdout) != name {
				t.Errorf("%s: %+v sent=%v err=%v", name, reply, sent, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, sent, err := session.run(ctx, lifetime, command, "true")
	if elapsed := time.Since(started); elapsed > 600*time.Millisecond {
		t.Fatalf("a caller with a 100 ms deadline waited %s for another caller's open", elapsed)
	}
	if sent || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short caller: sent=%v err=%v; want unsent with its own deadline", sent, err)
	}
	group.Wait()
	if starts.Load() != 1 {
		t.Fatalf("concurrent callers opened %d connections", starts.Load())
	}
}

func TestGuestSessionCloseDuringOpenDiscardsIt(t *testing.T) {
	session, lifetime, command, starts := slowGuestSession(t, "0.5")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, sent, _ := session.run(ctx, lifetime, command, "true"); sent {
		t.Fatal("script was sent before the connection opened")
	}
	started := time.Now()
	session.close()
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("close waited %s for an open in progress", elapsed)
	}
	// Wait for the discarded open to finish, then check it was not kept.
	time.Sleep(time.Second)
	session.mu.Lock()
	kept := session.conn != nil
	session.mu.Unlock()
	if kept {
		t.Fatal("a connection opened before close was kept")
	}
	reply, sent, err := session.run(context.Background(), lifetime, command, "printf fresh")
	if err != nil || !sent || string(reply.stdout) != "fresh" || starts.Load() != 2 {
		t.Fatalf("after close: %+v sent=%v err=%v opens=%d", reply, sent, err, starts.Load())
	}
}

func TestGuestSessionRetryBacksOff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, delay := range want {
		if got := guestSessionBackoff(i + 1); got != delay {
			t.Fatalf("retry after %d failures = %s, want %s", i+1, got, delay)
		}
	}
	session := &guestSession{}
	missing := func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/cage-guest-session")
	}
	if _, sent, err := session.run(context.Background(), context.Background(), missing, "true"); sent || err == nil {
		t.Fatal("unavailable session reported success")
	}
	session.mu.Lock()
	retry := time.Until(session.retryAt)
	session.mu.Unlock()
	if retry <= 0 || retry > guestSessionRetry {
		t.Fatalf("first retry in %s, want at most %s", retry, guestSessionRetry)
	}
}

func TestGuestSessionOpenFailingAfterCloseLeavesNoBackoff(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	session, starts := &guestSession{}, &atomic.Int32{}
	t.Cleanup(func() { session.close(); cancel() })
	// The guest is going down: the open fails, after close() was called.
	command := func(ctx context.Context) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "sh", "-c", "sleep 0.5; exit 255")
	}
	ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, sent, _ := session.run(ctx, lifetime, command, "true"); sent {
		t.Fatal("script was sent before the connection opened")
	}
	session.close()
	time.Sleep(time.Second)
	session.mu.Lock()
	failures, retryAt := session.failures, session.retryAt
	session.mu.Unlock()
	if failures != 0 || !retryAt.IsZero() {
		t.Fatalf("an open begun before close() set the new session's back-off: %d failures, retry at %v", failures, retryAt)
	}
	// The next caller opens at once instead of using a connection of its own.
	ctx, stop = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	session.run(ctx, lifetime, command, "true")
	if starts.Load() != 2 {
		t.Fatalf("%d opens: the session backed off after close()", starts.Load())
	}
}
