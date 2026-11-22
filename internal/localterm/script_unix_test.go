//go:build !windows

package localterm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The launch script runs cage attach as a child. A clean exit closes the
// window at once; any other exit resets the window, keeps the message on
// screen and waits for one key, and a signal says so.
func TestLaunchScriptWaitsOnlyAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "cage")
	// A stand-in for cage: --reset prints RESET; otherwise it prints its
	// message and exits as STUB_EXIT says (KILL: killed by a signal).
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nif [ \"$2\" = --reset ]; then printf RESET; exit 0; fi\nprintf 'attach message'\nif [ \"$STUB_EXIT\" = KILL ]; then kill -9 $$; fi\nexit \"$STUB_EXIT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		exit   string
		status int
		wait   bool
	}{{"0", 0, false}, {"3", 3, true}, {"KILL", 137, true}} {
		plan, err := PlanLaunch(PlanInput{App: installed(t, "terminal"), Command: Command{stub, "attach", "--terminal", "0123456789abcdef"}, Name: "0123456789abcdef", Scratch: dir})
		if err != nil {
			t.Fatal(err)
		}
		script := plan.Files[0]
		if err := os.WriteFile(script.Path, []byte(script.Content), script.Mode); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", script.Path)
		cmd.Env = append(os.Environ(), "STUB_EXIT="+row.exit)
		master, err := pty.Start(cmd)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var out bytes.Buffer
		go func() {
			buffer := make([]byte, 1024)
			for {
				n, err := master.Read(buffer)
				mu.Lock()
				out.Write(buffer[:n])
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		read := func() string { mu.Lock(); defer mu.Unlock(); return out.String() }
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		if row.wait {
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(read(), "按任意键关闭") {
				if time.Now().After(deadline) {
					t.Fatalf("exit %s: no prompt: %q", row.exit, read())
				}
				time.Sleep(20 * time.Millisecond)
			}
			select {
			case <-done:
				t.Fatalf("exit %s: closed without waiting for a key", row.exit)
			case <-time.After(300 * time.Millisecond):
			}
			_, _ = master.Write([]byte("x"))
		}
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("exit %s: still open: %q", row.exit, read())
		}
		time.Sleep(50 * time.Millisecond)
		got := read()
		master.Close()
		status := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			status = exitErr.ExitCode()
		}
		if status != row.status || !strings.Contains(got, "attach message") {
			t.Fatalf("exit %s: status %d, output %q", row.exit, status, got)
		}
		reset := strings.Index(got, "RESET")
		switch {
		case !row.wait && (reset >= 0 || strings.Contains(got, "按任意键关闭")):
			t.Fatalf("a clean exit waited or reset: %q", got)
		case row.wait && (reset < 0 || reset > strings.Index(got, "按任意键关闭")):
			t.Fatalf("exit %s: no reset before the prompt: %q", row.exit, got)
		case row.exit == "KILL" && !strings.Contains(got, "意外结束"):
			t.Fatalf("a killed attach was not explained: %q", got)
		}
		if _, err := os.Stat(script.Path); !os.IsNotExist(err) {
			t.Fatal("the script did not remove itself")
		}
	}
}
