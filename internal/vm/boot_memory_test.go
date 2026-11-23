package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

func TestBootMemoryAppliesTheDecision(t *testing.T) {
	previous := diagnostics.Default()
	diagnostics.SetDefault(diagnostics.New(t.TempDir()))
	oldPrepare, oldOutput := bootPrepare, bootOutput
	t.Cleanup(func() { diagnostics.SetDefault(previous); bootPrepare, bootOutput = oldPrepare, oldOutput })
	var out strings.Builder
	bootOutput = &out
	t.Setenv(cage.JobEnv, "job-1")
	var asked cage.BootRequest
	bootPrepare = func(_ context.Context, req cage.BootRequest) (cage.BootDecision, error) {
		asked = req
		req.Progress("正在向服务器申请内存…")
		return cage.BootDecision{Maximum: 4 << 30, Notice: "服务器暂时分不出更多内存，环境按 4 GB 内存上限启动（设置为 7 GB），之后重启环境即可恢复。", Asked: true, Waited: 75 * time.Second}, nil
	}
	config, instance := filepath.Join(t.TempDir(), "config.json"), t.TempDir()
	memory, err := bootMemory(context.Background(), config, instance, 7)
	if err != nil || memory != 4 || asked.Maximum != 7<<30 || asked.Profile != filepath.Dir(config) {
		t.Fatalf("memory %d %v, request %+v", memory, err, asked)
	}
	// Both steps reach the job's message.
	if got := out.String(); got != cage.JobMessagePrefix+"正在向服务器申请内存…\n"+cage.JobMessagePrefix+"服务器暂时分不出更多内存，环境按 4 GB 内存上限启动（设置为 7 GB），之后重启环境即可恢复。\n" {
		t.Fatalf("progress %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(instance, cage.BootNoticeFile)); err != nil || !strings.Contains(string(data), "环境按 4 GB 内存上限启动") {
		t.Fatalf("notice for the status: %q %v", data, err)
	}
	refusal := &cage.BootMemoryError{Need: 2304 << 20, Have: 1738 << 20, Growable: true}
	bootPrepare = func(context.Context, cage.BootRequest) (cage.BootDecision, error) {
		return cage.BootDecision{Asked: true}, refusal
	}
	if _, err := bootMemory(context.Background(), config, instance, 7); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	// Nothing configured, nothing asked.
	bootPrepare = func(context.Context, cage.BootRequest) (cage.BootDecision, error) {
		t.Fatal("asked")
		return cage.BootDecision{}, nil
	}
	if memory, err := bootMemory(context.Background(), config, instance, 0); memory != 0 || err != nil {
		t.Fatal(memory, err)
	}
	// The next start that keeps its maximum clears the notice.
	bootPrepare = func(_ context.Context, req cage.BootRequest) (cage.BootDecision, error) {
		return cage.BootDecision{Maximum: req.Maximum}, nil
	}
	if memory, err := bootMemory(context.Background(), config, instance, 7); memory != 7 || err != nil {
		t.Fatal(memory, err)
	}
	if _, err := os.Stat(filepath.Join(instance, cage.BootNoticeFile)); !os.IsNotExist(err) {
		t.Fatalf("notice outlived its start: %v", err)
	}
}

func TestVMStartErrorNamesAMemoryFailure(t *testing.T) {
	previous := diagnostics.Default()
	diagnostics.SetDefault(diagnostics.New(t.TempDir()))
	t.Cleanup(func() { diagnostics.SetDefault(previous) })
	dir := t.TempDir()
	log := `{"msg":"[qemu[stderr]] qemu-system-x86_64: cannot set up guest memory 'pc.ram': Cannot allocate memory"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ha.stderr.log"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("host agent process has exited")
	err := vmStartError(cause, dir, time.Time{})
	if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), "QEMU 启动失败：内存不足") || !strings.Contains(err.Error(), "cannot set up guest memory") {
		t.Fatalf("memory failure: %v", err)
	}
	for _, message := range []string{"qemu-system-x86_64: cannot set up guest memory 'pc.ram': Cannot allocate memory", "memory-backend-ram: Cannot allocate memory"} {
		if !qemuMemoryFailure(message) {
			t.Fatalf("%q not read as a memory failure", message)
		}
	}
	if qemuMemoryFailure("failed to initialize kvm: Cannot allocate memory in kernel") {
		t.Fatal("an unrelated failure read as a memory failure")
	}
}
