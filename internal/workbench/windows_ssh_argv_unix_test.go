//go:build !windows

package workbench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestGuestScriptEnsureCacheRecoveryAndZeroSteadyStateBytes(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	s := testServer(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var sshExecs atomic.Int32
	var execCommands []string
	var execMu sync.Mutex
	mobileTestSSHRewrite(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"}, func(command string) string {
		sshExecs.Add(1)
		execMu.Lock()
		execCommands = append(execCommands, command)
		execMu.Unlock()
		return command
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 1. Cold call: installs agent-runtime.py into ~/.local/share/cage/agent-runtime/<hash>.py
	// with mode 0700 and executes probe-all cleanly.
	var snap agentProbeSnapshot
	if err := s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &snap); err != nil {
		t.Fatalf("cold runAgentRuntime failed: %v", err)
	}
	installedPath := filepath.Join(home, ".local/share/cage/agent-runtime", agentRuntimeHash+".py")
	info, err := os.Stat(installedPath)
	if err != nil {
		t.Fatalf("agent-runtime.py not installed at expected hash-pinned path: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("installed agent-runtime.py mode = %04o, want 0700", perm)
	}
	gotSource, err := os.ReadFile(installedPath)
	if err != nil || string(gotSource) != cage.AgentRuntimeHelper {
		t.Fatalf("installed agent-runtime.py content mismatch (len=%d want=%d err=%v)", len(gotSource), len(cage.AgentRuntimeHelper), err)
	}
	if got := sshExecs.Load(); got != 1 {
		t.Fatalf("cold call ssh execs = %d, want 1 (shared guestSession carries ensure + run)", got)
	}

	// 2. Steady-state call: cached in memory & multiplexed over shared guestSession -> 0 new SSH execs!
	sshExecs.Store(0)
	execMu.Lock()
	execCommands = nil
	execMu.Unlock()
	if err := s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &snap); err != nil {
		t.Fatalf("steady-state runAgentRuntime failed: %v", err)
	}
	if got := sshExecs.Load(); got != 0 {
		t.Fatalf("steady-state call ssh execs = %d, want 0 (reuses shared guestSession)", got)
	}

	// 3. Out-of-band file deletion on guest: exit 86 triggers automatic cache
	// invalidation, re-install, and retry over the same shared guestSession (0 new SSH execs).
	if err := os.Remove(installedPath); err != nil {
		t.Fatal(err)
	}
	sshExecs.Store(0)
	if err := s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &snap); err != nil {
		t.Fatalf("recovery after guest file deletion failed: %v", err)
	}
	if _, err := os.Stat(installedPath); err != nil {
		t.Fatalf("agent-runtime.py was not reinstalled after deletion: %v", err)
	}
	if got := sshExecs.Load(); got != 0 {
		t.Fatalf("recovery ssh execs = %d, want 0 (miss/86 + re-ensure + retry share open guestSession)", got)
	}

	// 4. Guest lifecycle reset closes shared session and clears cache; 4 concurrent calls
	// coalesce onto 1 shared guestSession SSH connection.
	s.closeGuestSessions()
	sshExecs.Store(0)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local agentProbeSnapshot
			errs[i] = s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &local)
		}()
	}
	wg.Wait()
	for i, callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent post-reset call %d failed: %v", i, callErr)
		}
	}
	if got := sshExecs.Load(); got != 1 {
		t.Fatalf("4 concurrent post-reset calls used %d ssh execs, want 1 (shared guestSession)", got)
	}

	// 5. Dedicated-SSH fallback path (when shared guestSession is backing off):
	// verifies cold=2, steady=1 (<512 B remote command), exit-86 recovery=3, and 4 concurrent=5.
	s.closeGuestSessions()
	s.controlSession.mu.Lock()
	s.controlSession.retryAt = time.Now().Add(time.Minute)
	s.controlSession.mu.Unlock()
	if err := os.Remove(installedPath); err != nil {
		t.Fatal(err)
	}
	sshExecs.Store(0)
	if err := s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &snap); err != nil {
		t.Fatalf("fallback cold runAgentRuntime failed: %v", err)
	}
	if got := sshExecs.Load(); got != 2 {
		t.Fatalf("fallback cold call ssh execs = %d, want 2 (1 ensure + 1 run)", got)
	}
	sshExecs.Store(0)
	execMu.Lock()
	execCommands = nil
	execMu.Unlock()
	if err := s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &snap); err != nil {
		t.Fatalf("fallback steady-state runAgentRuntime failed: %v", err)
	}
	if got := sshExecs.Load(); got != 1 {
		t.Fatalf("fallback steady-state call ssh execs = %d, want 1", got)
	}
	execMu.Lock()
	steadyCmd := execCommands[0]
	execMu.Unlock()
	if len(steadyCmd) >= 512 {
		t.Fatalf("fallback steady-state remote command unexpectedly large: %d bytes (%s)", len(steadyCmd), steadyCmd)
	}
	if err := os.Remove(installedPath); err != nil {
		t.Fatal(err)
	}
	sshExecs.Store(0)
	if err := s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &snap); err != nil {
		t.Fatalf("fallback recovery failed: %v", err)
	}
	if got := sshExecs.Load(); got != 3 {
		t.Fatalf("fallback recovery ssh execs = %d, want 3 (1 miss/86 + 1 re-ensure + 1 retry)", got)
	}
	s.guestScripts.reset()
	sshExecs.Store(0)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local agentProbeSnapshot
			errs[i] = s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}, "force": true}, &local)
		}()
	}
	wg.Wait()
	for i, callErr := range errs {
		if callErr != nil {
			t.Fatalf("fallback concurrent call %d failed: %v", i, callErr)
		}
	}
	if got := sshExecs.Load(); got != 5 {
		t.Fatalf("fallback 4 concurrent calls used %d ssh execs, want 5 (1 coalesced ensure + 4 runs)", got)
	}
}

func TestGuestScriptEnsureDoesNotOverwriteExistingFile(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	s := testServer(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})

	spec := newGuestScriptSpec(".local/share/cage/test-helper", 0o600, "print('real')\n")
	existing := filepath.Join(home, spec.entryRelPath())
	if err := os.MkdirAll(filepath.Dir(existing), 0o700); err != nil {
		t.Fatal(err)
	}
	stub := "print('preexisting-stub')\n"
	if err := os.WriteFile(existing, []byte(stub), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ensureGuestScript(ctx, spec, "python3"); err != nil {
		t.Fatalf("ensureGuestScript failed: %v", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil || string(got) != stub {
		t.Fatalf("ensureGuestScript overwrote existing file: got=%q err=%v", string(got), err)
	}
}

func TestGuestScriptEnsureBundleInstallsAllFiles(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	s := testServer(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ensureGuestScript(ctx, mobileCLIGuestScripts, "/usr/bin/python3"); err != nil {
		t.Fatalf("ensureGuestScript bundle failed: %v", err)
	}
	sup, err := os.ReadFile(filepath.Join(home, mobileCLIGuestScripts.relPath("-supervisor.py")))
	if err != nil || string(sup) != mobileSupervisor {
		t.Fatalf("supervisor file mismatch: err=%v len=%d", err, len(sup))
	}
	id, err := os.ReadFile(filepath.Join(home, mobileCLIGuestScripts.relPath("-identity.py")))
	if err != nil || string(id) != mobileCLIIdentity {
		t.Fatalf("identity file mismatch: err=%v len=%d", err, len(id))
	}
	// Verify every installer command stays tiny even for bundles.
	cmd := s.guestScriptInstallSSHCommand(ctx, mobileCLIGuestScripts, "/usr/bin/python3")
	assertShortSSHArgv(t, "mobile-cli/bundle-install", cmd.Args, 4096)
	if !strings.Contains(strings.Join(cmd.Args, " "), " -I ") {
		t.Fatalf("installer command missing -I flag: %v", cmd.Args)
	}
}
