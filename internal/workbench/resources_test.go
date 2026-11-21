package workbench

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
)

func TestResourceAllocationSaveAndStartup(t *testing.T) {
	oldMem, oldCPU := host.MemGiB, host.CPUs
	t.Cleanup(func() { host.MemGiB, host.CPUs = oldMem, oldCPU })
	host.MemGiB = func() int { return 24 }
	host.CPUs = func() int { return 10 }
	t.Setenv("AGENT_VM_HOST_SHARE", "2")
	t.Setenv("CAGE_MACHINE_ROLE", "desktop")
	s := testServer(t)
	check := func(memory, cpus int) {
		t.Helper()
		m, c, err := cage.ConfiguredResourceLimits(s.configPath)
		if err != nil || m != memory || c != cpus {
			t.Fatalf("startup limits = %d/%d, %v", m, c, err)
		}
	}
	// Automatic on a 24 GiB, 10-thread desktop: all but a quarter of the
	// memory (at least 4 GiB stays) and all but a quarter of the threads.
	check(18, 8)
	for _, body := range []string{
		`{"allocation":{"automatic":false,"memory_gib":0,"cpus":2}}`,
		`{"allocation":{"automatic":false,"memory_gib":19,"cpus":2}}`,
		`{"allocation":{"automatic":false,"memory_gib":4,"cpus":9}}`,
		`{"allocation":{"automatic":false,"memory_gib":4.5,"cpus":2}}`,
	} {
		if w := request(s, "POST", "/api/environment/resources", s.origin, body, true); w.Code == 200 {
			t.Fatalf("accepted invalid allocation: %s", body)
		}
		check(18, 8)
	}
	before, _ := os.ReadFile(s.configPath)
	if w := request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":false,"memory_gib":4,"cpus":2}}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check(4, 2)
	after, _ := os.ReadFile(s.configPath)
	if string(before) != string(after) {
		t.Fatal("resource save changed network configuration")
	}
	w := request(s, "GET", "/api/environment/resources", "", "", true)
	var status struct {
		Automatic bool `json:"automatic"`
		Memory    int  `json:"memory_gib"`
		CPUs      int  `json:"cpus"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Automatic || status.Memory != 4 || status.CPUs != 2 {
		t.Fatal(w.Body.String(), err)
	}
	s.op.Lock()
	w = request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":true}}`, true)
	s.op.Unlock()
	if w.Code != 409 {
		t.Fatal("save must not race an environment operation", w.Code)
	}
	check(4, 2)
	if w = request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":true}}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	check(18, 8)
}

// A value chosen while the machine counted as dedicated (a headless cage
// serve) can be above the desktop app's range on the same machine: the
// desktop starts with the range's end and says so, and nothing rewrites the
// file the other role still reads.
func TestSavedAllocationAboveTheDesktopRangeStartsClamped(t *testing.T) {
	oldMem, oldCPU := host.MemGiB, host.CPUs
	t.Cleanup(func() { host.MemGiB, host.CPUs = oldMem, oldCPU })
	host.MemGiB = func() int { return 24 }
	host.CPUs = func() int { return 10 }
	t.Setenv("AGENT_VM_HOST_SHARE", "2")
	t.Setenv("CAGE_MACHINE_ROLE", "dedicated")
	s := testServer(t)
	if w := request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":false,"memory_gib":21,"cpus":9}}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, _ := os.ReadFile(cage.ResourceAllocationPath(s.configPath))
	t.Setenv("CAGE_MACHINE_ROLE", "desktop")
	if m, c, err := cage.ConfiguredResourceLimits(s.configPath); err != nil || m != 18 || c != 8 {
		t.Fatalf("desktop startup limits = %d/%d, %v", m, c, err)
	}
	w := request(s, "GET", "/api/environment/resources", "", "", true)
	var status struct {
		Memory int    `json:"memory_gib"`
		CPUs   int    `json:"cpus"`
		Error  string `json:"resource_error"`
		Notice string `json:"notice"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Memory != 18 || status.CPUs != 8 || status.Error != "" || !strings.Contains(status.Notice, "环境按内存 18 GB、CPU 8 核启动") {
		t.Fatal(w.Body.String(), err)
	}
	if after, _ := os.ReadFile(cage.ResourceAllocationPath(s.configPath)); string(after) != string(saved) {
		t.Fatalf("the saved allocation was rewritten: %s", after)
	}
	// Choosing the same value on the desktop is still refused.
	if w := request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":false,"memory_gib":21,"cpus":9}}`, true); w.Code == 200 {
		t.Fatal("accepted a new choice above the desktop range")
	}
}

func TestResourceStatusTellsAnOldDefaultFromAChoice(t *testing.T) {
	oldMem, oldCPU := host.MemGiB, host.CPUs
	t.Cleanup(func() { host.MemGiB, host.CPUs = oldMem, oldCPU })
	// The production machine: 8 GiB, which host.MemGiB reports as 7, 6 vCPUs.
	host.MemGiB = func() int { return 7 }
	host.CPUs = func() int { return 6 }
	t.Setenv("CAGE_MACHINE_ROLE", "dedicated")
	s := testServer(t)
	type status struct {
		Automatic bool   `json:"automatic"`
		Memory    int    `json:"memory_gib"`
		CPUs      int    `json:"cpus"`
		MaxMemory int    `json:"max_memory_gib"`
		Origin    string `json:"allocation_origin"`
		Legacy    bool   `json:"legacy_allocation"`
		Notice    string `json:"notice"`
		Offer     *struct {
			Automatic bool `json:"automatic"`
			Memory    int  `json:"memory_gib"`
			CPUs      int  `json:"cpus"`
		} `json:"offer"`
		AutomaticMemory int    `json:"automatic_memory_gib"`
		Recommended     uint64 `json:"recommended_memory_bytes"`
	}
	read := func() status {
		t.Helper()
		var got status
		w := request(s, "GET", "/api/environment/resources", "", "", true)
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(w.Body.String(), err)
		}
		return got
	}
	// No settings file: automatic, 6 GiB and 5 vCPUs (the old rule gave 3/3).
	if got := read(); !got.Automatic || got.Memory != 6 || got.CPUs != 5 || got.Origin != cage.OriginAutomatic || got.Recommended != 6<<30 || got.MaxMemory != 6 || got.Offer != nil {
		t.Fatalf("automatic status %+v", got)
	}
	// An older version saved its default values explicitly: kept, and
	// automatic sizing is offered.
	if err := os.WriteFile(cage.ResourceAllocationPath(s.configPath), []byte(`{"automatic":false,"memory_gib":3,"cpus":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.Automatic || got.Memory != 3 || got.CPUs != 3 || got.Origin != cage.OriginLegacyDefault || !got.Legacy || got.Offer == nil || !got.Offer.Automatic || got.Offer.Memory != 6 || got.Offer.CPUs != 5 || !strings.Contains(got.Notice, "改为自动（推荐）") {
		t.Fatalf("legacy default status %+v", got)
	}
	// The same values chosen in settings are the user's: no offer.
	if w := request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":false,"memory_gib":3,"cpus":3}}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if got := read(); got.Origin != cage.OriginUser || got.Legacy || got.Offer != nil || got.Memory != 3 {
		t.Fatalf("chosen status %+v", got)
	}
	saved, _ := os.ReadFile(cage.ResourceAllocationPath(s.configPath))
	var savedAllocation cage.ResourceAllocation
	if json.Unmarshal(saved, &savedAllocation) != nil || savedAllocation.Chosen != cage.OriginUser {
		t.Fatalf("saved allocation %s", saved)
	}
	// The one step to automatic: saved, and with the environment stopped
	// nothing is restarted.
	if w := request(s, "POST", "/api/environment/resources", s.origin, `{"allocation":{"automatic":true},"apply":true}`, true); w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := read(); !got.Automatic || got.Memory != 6 || got.Origin != cage.OriginAutomatic {
		t.Fatalf("after moving to automatic %+v", got)
	}
}
