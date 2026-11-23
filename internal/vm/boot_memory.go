package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// bootPrepare is cage.PrepareBoot and bootOutput where its progress goes;
// tests replace them.
var (
	bootPrepare           = cage.PrepareBoot
	bootOutput  io.Writer = os.Stderr
)

// bootMemory makes sure the machine can back a Linux guest's boot maximum of
// memoryGiB before Lima starts it (cage.PrepareBoot): it may ask the outer
// level and wait, lower the maximum to what the machine maps now, or fail
// with a sentence about memory. Every QEMU start of a Cage environment comes
// through here: cage up, the base build of cage setup, and the restart after
// a share change.
//
// A smaller maximum is the guest's ceiling until the next start: its notice
// stays in instanceDir (cage.BootNoticeFile) for the resources status until
// then.
func bootMemory(ctx context.Context, configPath, instanceDir string, memoryGiB int) (int, error) {
	if memoryGiB <= 0 {
		return memoryGiB, nil
	}
	notice := filepath.Join(instanceDir, cage.BootNoticeFile)
	_ = os.Remove(notice)
	progress := func(message string) { cage.JobMessage(bootOutput, message) }
	decision, err := bootPrepare(ctx, cage.BootRequest{Profile: filepath.Dir(configPath), Maximum: uint64(memoryGiB) << 30, Progress: progress})
	if decision.Asked {
		diagnostics.Log("info", "vm/start", fmt.Sprintf("boot memory: asked the outer level for a %d GiB guest, waited %s, maximum %d MiB, error=%v", memoryGiB, decision.Waited.Round(100_000_000), decision.Maximum>>20, err))
	}
	if err != nil {
		return 0, err
	}
	if decision.Notice != "" {
		progress(decision.Notice)
		diagnostics.Log("warn", "vm/start", decision.Notice)
		_ = os.WriteFile(notice, []byte(decision.Notice), 0o600)
	}
	return int(min(decision.Maximum, uint64(memoryGiB)<<30) >> 30), nil
}
