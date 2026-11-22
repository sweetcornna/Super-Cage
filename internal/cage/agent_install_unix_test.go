//go:build unix

package cage

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/agents"
)

// Stopping the environment or 取消 signals the job's whole process group, as
// KillMode=control-group does: the stage goes and nothing is committed.
func TestAgentInstallCanceledLeavesNoStage(t *testing.T) {
	r := newAgentRig(t)
	started := filepath.Join(r.root, "verify-started")
	archive := agentTarGz(t, tarMember{name: agentEntryName, body: "#!/bin/sh\n: >'" + started + "'\nexec /bin/sleep 30\n", mode: 0o755})
	e := r.entry("0.171.0", archive)
	r.server.publish(e, archive)
	cmd := exec.Command(r.bash, "-se")
	cmd.Stdin = strings.NewReader(r.compose(r.input(e, agents.Install)))
	cmd.Dir = r.root
	cmd.Env = r.env()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	group := -cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Minute)
	for !r.exists(started) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(group, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatalf("the verify step never started\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(r.stages()) != 1 {
		_ = syscall.Kill(group, syscall.SIGKILL)
		t.Fatalf("stages while verifying: %v", r.stages())
	}
	if err := syscall.Kill(group, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for deadline = time.Now().Add(10 * time.Second); len(r.stages()) != 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	result := r.result(out.String(), nil)
	if result.failed != "canceled" || !strings.Contains(result.out, "已取消，已下载的部分会在下次继续使用。") {
		t.Fatalf("failed=%q\n%s", result.failed, result.out)
	}
	r.absent(result)
}
