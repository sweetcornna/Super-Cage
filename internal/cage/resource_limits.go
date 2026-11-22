package cage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/host"
)

// ResourceAllocation is separate from network configuration: changing a VM's
// capacity does not change its network authorization fingerprint.
type ResourceAllocation struct {
	Automatic bool `json:"automatic"`
	MemoryGiB int  `json:"memory_gib"`
	CPUs      int  `json:"cpus"`
	// Chosen is "user" on an allocation saved through the settings API by
	// this version or later; older files lack it (older versions ignore it).
	Chosen string `json:"chosen,omitempty"`
}

// The origins of an allocation.
const (
	// OriginAutomatic: no saved allocation, or automatic saved.
	OriginAutomatic = "automatic"
	// OriginUser: an explicit allocation someone chose.
	OriginUser = "user"
	// OriginLegacyDefault: an explicit allocation, saved by an older version,
	// that equals the old automatic default on this machine: most likely
	// written by a settings form that was saved with its default values.
	OriginLegacyDefault = "legacy_default"
)

// LegacyDefaultAction is what an allocation of OriginLegacyDefault becomes:
// "offer" keeps it and offers automatic sizing in the status; "apply" treats
// it as automatic. Matching the old formula cannot prove nobody chose it, so
// the default keeps it.
var LegacyDefaultAction = "offer"

// Origin classifies the allocation. Pure apart from reading the machine.
func (a ResourceAllocation) Origin() string {
	if a.Automatic {
		return OriginAutomatic
	}
	if a.Chosen == OriginUser {
		return OriginUser
	}
	// The old default followed host.MemGiB, which inside a ballooned machine
	// moves between its current size and its boot size.
	for memory := max(host.MemGiB(), 1); memory <= max(machineBootGiB(), host.MemGiB()); memory++ {
		if legacyMemory, legacyCPUs := legacyAutomaticLimits(memory, host.CPUs()); legacyMemory == a.MemoryGiB && legacyCPUs == a.CPUs {
			return OriginLegacyDefault
		}
	}
	return OriginUser
}

// Effective is the allocation that applies: a legacy default becomes
// automatic only when LegacyDefaultAction says so.
func (a ResourceAllocation) Effective() ResourceAllocation {
	if LegacyDefaultAction == "apply" && a.Origin() == OriginLegacyDefault {
		return ResourceAllocation{Automatic: true}
	}
	return a
}

func ResourceAllocationPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "resource-limits.json")
}

// An automatically sized VM may grow to three eighths of host memory, at least
// AutomaticMemoryFloorGiB and at most AutomaticMemoryCapGiB, and never beyond
// the host share (half of host memory by default), which also bounds a
// manually chosen allocation. The balloon returns what the guest does not use,
// so a larger host lends more without starting every VM with half of it.
const (
	AutomaticMemoryFloorGiB = 8
	AutomaticMemoryCapGiB   = 32
	// AutomaticFixedMemoryGiB caps the automatic memory of a guest without a
	// memory balloon, such as macOS: it keeps everything it boots with.
	AutomaticFixedMemoryGiB = 8
)

// AutomaticMemoryGiB is the memory ceiling of an automatic allocation on a
// host whose share is shareGiB. Inside a tenant VM the host is that VM, so the
// share keeps half of it for the VM's own system.
func AutomaticMemoryGiB(shareGiB int) int {
	return min(shareGiB, max(AutomaticMemoryFloorGiB, host.MemGiB()*3/8), AutomaticMemoryCapGiB)
}

// Limits is the boot-time maximum and vCPUs of a ballooned Linux guest. An
// automatic allocation follows the machine (AutomaticLimits); an explicit one
// is taken as it is, within the larger of the host share and the automatic
// maximum. A saved value outside that range is brought into it, never
// refused (LimitsNotice says so): the range depends on the machine's role,
// so a value saved by a headless cage serve may exceed what the desktop app
// allows on the same machine, and the file is shared by both. A value
// someone is choosing now is checked with Validate instead.
func (a ResourceAllocation) Limits() (memory, cpus int, err error) {
	memory, cpus, _ = a.limits()
	return memory, cpus, nil
}

// LimitsNotice explains, in the product's words, a saved allocation that
// Limits brought into this machine's range; "" when it applies as saved.
func (a ResourceAllocation) LimitsNotice() string {
	memory, cpus, clamped := a.limits()
	if !clamped {
		return ""
	}
	maxMemory, maxCPUs := ManualLimitRange()
	return fmt.Sprintf("保存的资源设置（内存 %d GB、CPU %d 核）超出这台机器当前允许的范围（内存 2–%d GB、CPU 1–%d 核），环境按内存 %d GB、CPU %d 核启动。可在设置中重新选择。",
		a.MemoryGiB, a.CPUs, maxMemory, maxCPUs, memory, cpus)
}

// Validate checks an explicit allocation being chosen now against this
// machine's range.
func (a ResourceAllocation) Validate() error {
	if a.Effective().Automatic {
		return nil
	}
	memory, cpus := ManualLimitRange()
	if a.MemoryGiB < 2 || a.MemoryGiB > memory || a.CPUs < 1 || a.CPUs > cpus {
		return fmt.Errorf("内存上限须为 2–%d GiB，CPU 须为 1–%d 个逻辑核心", memory, cpus)
	}
	return nil
}

func (a ResourceAllocation) limits() (memory, cpus int, clamped bool) {
	a = a.Effective()
	if a.Automatic {
		memory, cpus = AutomaticLimits()
		return memory, cpus, false
	}
	maxMemory, maxCPUs := ManualLimitRange()
	memory, cpus = min(max(a.MemoryGiB, 2), max(maxMemory, 2)), min(max(a.CPUs, 1), max(maxCPUs, 1))
	return memory, cpus, memory != a.MemoryGiB || cpus != a.CPUs
}

// ManualLimitRange is the largest explicit allocation: the host share or the
// automatic maximum, whichever is larger.
func ManualLimitRange() (memory, cpus int) {
	memory, cpus = ResourceLimits()
	automaticMemory, automaticCPUs := AutomaticLimits()
	return max(memory, automaticMemory), max(cpus, automaticCPUs)
}

// LegacyLimits is the sizing before machine-following automatic sizing:
// guests without a balloon (macOS) keep it.
func (a ResourceAllocation) LegacyLimits() (memory, cpus int, err error) {
	memory, cpus = ResourceLimits()
	if a.Automatic {
		return AutomaticMemoryGiB(memory), cpus, nil
	}
	if a.MemoryGiB < 2 || a.MemoryGiB > memory || a.CPUs < 1 || a.CPUs > cpus {
		return 0, 0, fmt.Errorf("内存上限须为 2–%d GiB，CPU 须为 1–%d 个逻辑核心", memory, cpus)
	}
	return a.MemoryGiB, a.CPUs, nil
}

func LoadResourceAllocation(configPath string) (ResourceAllocation, error) {
	a := ResourceAllocation{Automatic: true}
	b, err := os.ReadFile(ResourceAllocationPath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal(b, &a); err != nil {
		return a, err
	}
	return a, nil
}

func ConfiguredResourceLimits(configPath string) (int, int, error) {
	a, err := LoadResourceAllocation(configPath)
	if err != nil {
		return 0, 0, err
	}
	return a.Limits()
}
