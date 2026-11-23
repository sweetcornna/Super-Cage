package workbench

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/go-units"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"go.yaml.in/yaml/v3"
)

func (s *Server) instanceDir() string {
	if s.windowsGuest != nil {
		// The profile owns storage paths, while the server retains the bound
		// VM/user identity across a migration. Relay calls load this file too.
		if current, err := guestos.LoadWindowsProfile(s.profile); err == nil && current != nil && current.VMID == s.windowsGuest.VMID && current.GUIUserSID == s.windowsGuest.GUIUserSID {
			return current.InstanceDirectory
		}
		return s.windowsGuest.InstanceDirectory
	}
	if s.macosGuest != nil {
		return filepath.Join(s.profile, "state", "lima", s.macosGuest.Instance)
	}
	if containerenv.Configured(s.profile) {
		return filepath.Join(s.profile, "state", "container", "environment")
	}
	return filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()))
}

func (s *Server) resourceStatus(w http.ResponseWriter, r *http.Request) {
	if s.windowsGuest != nil {
		s.windowsResourceStatus(w, r)
		return
	}
	if s.macosGuest != nil {
		s.macOSResourceStatus(w, r)
		return
	}
	if containerenv.Configured(s.profile) {
		environment, err := containerenv.Load(s.profile)
		if err != nil {
			requestError(w, err)
			return
		}
		state, err := environment.State(r.Context())
		if err != nil {
			requestError(w, err)
			return
		}
		config := environment.Config()
		jsonReply(w, map[string]any{"running": state.Running, "automatic": false, "runtime": "container",
			"recommended_memory_bytes": config.MemoryBytes, "recommended_cpus": config.CPUs,
			"configured_memory_bytes": state.MemoryBytes, "configured_cpus": state.CPUs,
			"resources_verified": state.ResourcesVerified, "resource_error": state.ResourceError, "pids_limit": state.PIDs,
			"restart_required": false, "observation": nil})
		return
	}
	a, err := cage.LoadResourceAllocation(s.configPath)
	if err != nil {
		requestError(w, err)
		return
	}
	memory, cpus, err := a.Limits()
	allocationError := ""
	if err != nil {
		allocationError = err.Error()
		memory, cpus = a.MemoryGiB, a.CPUs
	}
	// A saved value outside this machine's range starts clamped (the range
	// follows the machine's role); the notice says what applies.
	clampNotice := a.LimitsNotice()
	// A Linux guest follows the machine (resource-allocation.md, level B);
	// macOS guests answer above with the previous sizing.
	maxMemory, maxCPUs := cage.ManualLimitRange()
	automaticMemory, automaticCPUs := cage.AutomaticLimits()
	origin, automatic := a.Origin(), a.Effective().Automatic
	running := s.vmRunning()
	var configured struct {
		Memory string `yaml:"memory"`
		CPUs   int    `yaml:"cpus"`
	}
	if b, err := os.ReadFile(filepath.Join(s.instanceDir(), "lima.yaml")); err == nil {
		_ = yaml.Unmarshal(b, &configured)
	}
	maximum, _ := units.RAMInBytes(configured.Memory)
	var observation *cage.ResourceStatus
	status, err := cage.ReadResourceStatus(s.instanceDir())
	if running && err == nil {
		observation = &status
	}
	restart := running && (maximum != int64(memory)<<30 || configured.CPUs != cpus || observation == nil)
	reply := map[string]any{
		"running": running, "automatic": automatic,
		"memory_gib": memory, "cpus": cpus,
		"max_memory_gib": maxMemory, "max_cpus": maxCPUs,
		"resource_error":           allocationError,
		"recommended_memory_bytes": uint64(automaticMemory) << 30, "recommended_cpus": automaticCPUs,
		"configured_memory_bytes": maximum, "configured_cpus": configured.CPUs,
		"restart_required": restart,
		"observation":      observation,
		// Additive: where the allocation comes from and what automatic
		// sizing gives on this machine.
		"allocation_origin": origin, "automatic_memory_gib": automaticMemory, "automatic_cpus": automaticCPUs,
		"possible_memory_bytes": uint64(max(automaticMemory, memory)) << 30,
	}
	// A start that fell back to a smaller boot maximum (cage.PrepareBoot)
	// said why; the guest keeps that ceiling until the next start.
	bootNotice := ""
	if running && maximum > 0 && maximum < int64(memory)<<30 {
		if data, err := os.ReadFile(filepath.Join(s.instanceDir(), cage.BootNoticeFile)); err == nil && len(data) <= 1024 {
			bootNotice = strings.TrimSpace(string(data))
		}
	}
	switch {
	case bootNotice != "":
		reply["notice"] = bootNotice
	case origin == cage.OriginLegacyDefault && !automatic && (automaticMemory > memory || automaticCPUs > cpus):
		// Saved by an older version with its default values: offered, never
		// imposed, since matching the old rule cannot prove nobody chose it.
		reply["legacy_allocation"] = true
		reply["notice"] = fmt.Sprintf("内存 %d GB、CPU %d 核与旧版本的默认设置相同。改为自动（推荐）后，这台机器可以给到 %d GB、%d 核，需要重启环境；如果这是你特意设的，可以保留。", memory, cpus, automaticMemory, automaticCPUs)
		reply["offer"] = map[string]any{"automatic": true, "memory_gib": automaticMemory, "cpus": automaticCPUs, "restart_required": running}
	case automatic && restart && observation != nil && maximum != int64(memory)<<30:
		reply["notice"] = fmt.Sprintf("自动分配已按这台机器重新计算为内存 %d GB、CPU %d 核，重启环境后生效。", memory, cpus)
	case clampNotice != "":
		reply["notice"] = clampNotice
	}
	if observation != nil && observation.Advice != nil {
		reply["advice"] = observation.Advice
	}
	jsonReply(w, reply)
}

func (s *Server) applyResources(w http.ResponseWriter, r *http.Request) {
	if s.windowsGuest != nil {
		s.applyWindowsResources(w, r)
		return
	}
	var input struct {
		Allocation *cage.ResourceAllocation `json:"allocation"`
		// Apply restarts a running environment right after saving, so moving
		// to automatic sizing is one step. Older clients omit it.
		Apply bool `json:"apply"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if containerenv.Configured(s.profile) {
		http.Error(w, "容器资源由服务器部署配置管理", http.StatusBadRequest)
		return
	}
	if input.Allocation != nil {
		allocation := *input.Allocation
		// Saved through settings: an explicit allocation is the user's
		// choice from now on, never mistaken for an old default.
		allocation.Chosen = ""
		if !allocation.Automatic {
			allocation.Chosen = cage.OriginUser
		}
		if err := s.validateResourceAllocation(allocation); err != nil {
			requestError(w, err)
			return
		}
		if !s.op.TryLock() {
			http.Error(w, "另一个操作仍在进行，请稍候", http.StatusConflict)
			return
		}
		err := writePrivate(cage.ResourceAllocationPath(s.configPath), allocation)
		s.op.Unlock()
		if err != nil {
			requestError(w, err)
			return
		}
		if !input.Apply || !s.vmRunning() {
			jsonReply(w, map[string]bool{"ok": true})
			return
		}
	}
	s.launch(w, "resources", func() error {
		allocation, err := cage.LoadResourceAllocation(s.configPath)
		if err != nil {
			return err
		}
		if _, _, err := s.resourceAllocationLimits(allocation); err != nil {
			return err
		}
		if s.vmRunning() {
			if err := s.stopEnvironment(); err != nil {
				return err
			}
		}
		if err := s.operationContext().Err(); err != nil {
			return err
		}
		return s.startEnvironment(false)
	})
}

type networkCheckError struct{ error }

func (e networkCheckError) Unwrap() error { return e.error }

func (s *Server) checkNetworkIfWanted() error {
	s.mu.Lock()
	paused := s.networkPaused
	s.mu.Unlock()
	if paused || s.macSetupPending() {
		return nil
	}
	var err error
	if s.macosGuest != nil {
		err = s.checkMacOSNetwork()
	} else {
		err = s.runSafetyCheck()
	}
	if err != nil {
		return networkCheckError{err}
	}
	return nil
}
