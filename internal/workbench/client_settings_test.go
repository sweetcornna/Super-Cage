package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/sylvinus/agent-vm/internal/localorca"
)

func readClientSettings[T any](t *testing.T, s *Server, method, path, body string) T {
	t.Helper()
	w := request(s, method, path, s.origin, body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestOrcaSettingsPersistOfflineAndKeepAutomaticMode(t *testing.T) {
	s := testServer(t)
	s.orca.localDetect = func() localorca.Installation { return localorca.Installation{} }
	var guestCalls atomic.Int32
	s.agentCLIs.runtimeHook = func(context.Context, string, any, any) error {
		guestCalls.Add(1)
		return nil
	}
	type orcaView struct {
		orcaConfig
		TunnelPort int `json:"tunnel_port"`
	}
	post := func(body string) orcaView {
		t.Helper()
		return readClientSettings[orcaView](t, s, "POST", "/api/orca/config", body)
	}
	reopen := func() orcaView {
		t.Helper()
		s.orca.mu.Lock()
		s.orca.configLoaded = false
		s.orca.mu.Unlock()
		return readClientSettings[orcaView](t, s, "GET", "/api/orca", "")
	}
	post(`{"guest_port":17023,"host_tunnel_port":17426,"server_name":"Lab Orca","bind_mode":"tailscale","auto_sync_projects":false,"terminal_memory_max":" 512m ","account_bindings":{"claude":"default"}}`)
	fixed := reopen()
	if guestCalls.Load() != 0 {
		t.Fatal("editing a disabled integration contacted the guest")
	}
	if fixed.Enabled || fixed.GuestPort != 17023 || fixed.HostTunnelPort != 17426 || fixed.TunnelPort != 17426 ||
		fixed.ServerName != "Lab Orca" || fixed.BindMode != "tailscale" || fixed.AutoSyncProjects ||
		fixed.TerminalMemoryMax != "512M" || fixed.AccountBindings["claude"] != "default" {
		t.Fatalf("offline config did not persist: %+v", fixed)
	}
	automatic := post(`{"enabled":true,"host_tunnel_port":0}`)
	if !automatic.Enabled || automatic.HostTunnelPort != 0 || automatic.TunnelPort < 1024 {
		t.Fatalf("failed to allocate a port while keeping automatic mode: %+v", automatic)
	}
	post(`{"server_name":"Renamed Orca"}`)
	reopened := reopen()
	if reopened.HostTunnelPort != 0 || reopened.TunnelPort != automatic.TunnelPort ||
		reopened.ServerName != "Renamed Orca" || reopened.AccountBindings["claude"] != "default" {
		t.Fatalf("automatic setting or effective tunnel port did not survive reopening: %+v", reopened)
	}
}

func TestT3CodeSettingsPersistBeforeEnablement(t *testing.T) {
	s := testServer(t)
	readClientSettings[t3CodeConfig](t, s, "POST", "/api/t3code/config", `{
		"mode":"external","external_url":"https://t3.example.test/agent#ignore","port":17453,
		"open_mode":"direct","focus_chrome":false,"account_bindings":{"claude":"default"}
	}`)
	s.t3code.mu.Lock()
	s.t3code.configLoaded = false
	s.t3code.mu.Unlock()
	stored := readClientSettings[t3CodeConfig](t, s, "GET", "/api/t3code", "")
	if stored.Enabled || stored.Mode != "external" || stored.ExternalURL != "https://t3.example.test/agent/" ||
		stored.Port != 17453 || stored.OpenMode != "direct" || stored.FocusChrome || stored.AccountBindings["claude"] != "default" {
		t.Fatalf("T3 Code settings did not persist: %+v", stored)
	}
}

func TestAgentConfigPatchPreservesOmittedLaunchSettings(t *testing.T) {
	s := testServer(t)
	type catalogView struct {
		Agents []agentCatalogEntry `json:"agents"`
	}
	post := func(body string) {
		t.Helper()
		readClientSettings[catalogView](t, s, "POST", "/api/agent-clis/config", body)
	}
	post(`{"agent_id":"claude","launch_command":"claude --continue"}`)
	post(`{"agent_id":"claude","shift_enter_mode":"plain","image_paste":false}`)
	post(`{"agent_id":"codex","launch_command":"codex --search","extra_args":"--model fixture"}`)
	post(`{"agent_id":"codex","preferred_channel":"npm-official","hidden_in_menu":true}`)
	list := readClientSettings[catalogView](t, s, "GET", "/api/agent-clis", "")
	configs := map[string]agentConfig{}
	for _, entry := range list.Agents {
		configs[entry.Spec.ID] = entry.Config
	}
	if claude := configs["claude"]; claude.LaunchCommand != "claude --continue" || claude.ShiftEnterMode != "plain" || claude.ImagePaste == nil || *claude.ImagePaste {
		t.Fatalf("Claude settings did not survive a partial edit: %+v", claude)
	}
	if codex := configs["codex"]; codex.LaunchCommand != "codex --search" || codex.ExtraArgs != "--model fixture" ||
		codex.PreferredChannel != "npm-official" || !codex.HiddenInMenu {
		t.Fatalf("Codex settings did not survive a partial edit: %+v", codex)
	}
}
