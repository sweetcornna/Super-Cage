package workbench

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
)

// An installed app has cage-attach beside cage. The controller's launch
// answer then names that companion and its status does not, so the two
// identities differ in that one field: the desktop must still take them for
// the same controller and open the local workbench.
func TestDesktopLocalConnectsWithACompanionBesideTheController(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	installed := filepath.Join(dir, filepath.Base(exe))
	if err := os.Link(exe, installed); err != nil {
		copyTestFile(t, exe, installed)
	}
	if err := os.WriteFile(filepath.Join(dir, controlclient.AttachProgram(runtime.GOOS)), []byte("companion"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "local")
	config := filepath.Join(profile, "config.json")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, installed, "-test.run=^TestServeShutdownHandoff$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "CAGE_TEST_SHUTDOWN_CONFIG="+config)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	d := NewDesktopLocalController(config, installed)
	var st DesktopLocalStatus
	for {
		st, err = d.Status(ctx)
		if err == nil && st.Known && st.ServiceRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("controller did not start")
		case <-done:
			t.Fatal("controller exited", waitErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
	session, err := d.Connect(ctx)
	if err != nil {
		t.Fatalf("connect with a companion beside the controller: %v", err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	if err = d.Release(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	<-done
	if waitErr != nil {
		t.Fatal(waitErr)
	}
}

func copyTestFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
}

// The identity holds a pointer: two answers of one controller decode to
// different addresses, and one of them may leave the companion out.
func TestSameControllerIgnoresTheCompanion(t *testing.T) {
	a := ControllerIdentity{Version: "1", Build: "b", Protocol: controlProtocol, PID: 7, Stamp: "s", Attach: &controlclient.Companion{Path: "/x/cage-attach", SHA256: "aa"}}
	b := ControllerIdentity{Version: "1", Build: "b", Protocol: controlProtocol, PID: 7, Stamp: "s"}
	c := a
	c.Attach = &controlclient.Companion{Path: "/x/cage-attach", SHA256: "aa"}
	if !a.SameController(b) || !b.SameController(a) || !a.SameController(c) {
		t.Fatal("one controller taken for two")
	}
	for name, other := range map[string]ControllerIdentity{
		"pid":      {Version: "1", Build: "b", Protocol: controlProtocol, PID: 8, Stamp: "s"},
		"build":    {Version: "1", Build: "c", Protocol: controlProtocol, PID: 7, Stamp: "s"},
		"stamp":    {Version: "1", Build: "b", Protocol: controlProtocol, PID: 7, Stamp: "t"},
		"version":  {Version: "2", Build: "b", Protocol: controlProtocol, PID: 7, Stamp: "s"},
		"protocol": {Version: "1", Build: "b", Protocol: controlProtocol + 1, PID: 7, Stamp: "s"},
	} {
		if a.SameController(other) {
			t.Fatalf("a different %s taken for the same controller", name)
		}
	}
}
