package workbench

// The agent adapter uses the same authenticated local workload as the desktop.
// It never follows the desktop gateway's currently selected remote connection.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/agentcatalog"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

const AgentProtocol = 1

type AgentOperation struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Mutates      bool           `json:"mutates"`
	InputSchema  map[string]any `json:"input_schema"`
	method, path string
}

// Named operations are discoverable shortcuts for the same production handlers.
// AgentRequest also accepts UI endpoints so the shortcut list is not a permission boundary.
func AgentOperations() []AgentOperation {
	object := func(fields map[string]string) map[string]any {
		properties := map[string]any{}
		for name, kind := range fields {
			if kind == "array" {
				properties[name] = map[string]any{"type": kind, "items": map[string]string{"type": "string"}}
			} else {
				properties[name] = map[string]string{"type": kind}
			}
		}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	}
	from := func(value any) map[string]any { return agentSchema(reflect.TypeOf(value)) }

	var ops []AgentOperation
	add := func(name, description, method, path string, schema map[string]any) {
		ops = append(ops, AgentOperation{Name: name, Description: description, Mutates: method != "GET", InputSchema: schema, method: method, path: path})
	}
	get := func(name, description, path string) { add(name, description, "GET", path, object(nil)) }
	post := func(name, description, path string, schema map[string]any) {
		add(name, description, "POST", path, schema)
	}
	get("status", "Environment readiness, network checks and current job", "/api/status")
	get("account.status", "Account presence and lock state; no credentials", "/api/account")
	post("account.create", "Create an account only on an empty profile; user chooses the password. Returns a recovery_key once; deliver it privately to the user for secure storage", "/api/account/create", object(map[string]string{"username": "string", "password": "string", "confirm_password": "string", "avatar": "string"}))
	post("account.unlock", "Unlock using a user-provided password via private input", "/api/account/unlock", object(map[string]string{"password": "string"}))
	get("settings.get", "Read settings without proxy credentials", "/api/settings")
	post("settings.patch", "Merge specified fields under the operation lock; dry_run validates without writing", "/api/agent/settings", object(map[string]string{"patch": "object", "dry_run": "boolean"}))
	// Include the production Settings schema inside the patch field.
	ops[len(ops)-1].InputSchema["properties"].(map[string]any)["patch"] = from(Settings{})
	ops[len(ops)-1].InputSchema["properties"].(map[string]any)["patch"].(map[string]any)["properties"].(map[string]any)["exit_policy"] = object(map[string]string{"mode": "string", "scope": "string", "ip": "string", "country_code": "string", "region": "string", "city": "string"})
	post("settings.restore", "Restore the saved previous settings and restart the environment; inspect status and obtain permission to interrupt work first", "/api/restore", object(nil))
	get("proxy.status", "List source and node IDs without subscription URLs or node secrets", "/api/proxy")
	proxy := from(proxyInput{})
	for _, action := range []string{"import", "refresh", "edit", "delete", "select", "route", "restart", "stop"} {
		post("proxy."+action, "Proxy operation; import creates a source, refresh updates an existing source. Read state before retrying. select/route applies the route", "/api/proxy/"+action, proxy)
	}
	get("guest.options", "Available guest systems and resumable installation state", "/api/environment/guest-systems")
	get("guest.match", "Inspect a host-matching clean image plan; pass key as clone_key and plan.host.system as guest_system to environment.start", "/api/environment/guest-systems/clone")
	post("guest.activate", "Activate an installed guest; controller may retire, reconnect afterwards", "/api/environment/guest-systems/activate", object(nil))
	get("initialization.status", "Read initial setup state", "/api/environment/initialization")
	post("initialization.set", "action: begin, install or complete; complete requires verified readiness", "/api/environment/initialization", object(map[string]string{"action": "string"}))
	post("environment.start", "Start or resume installation. Omit guest_system for an existing environment; use guest.options for supported choices", "/api/start", from(guestInstallRequest{}))
	post("environment.stop", "Stop the environment and active terminals; preserves persistent data", "/api/stop", object(nil))
	get("environment.versions", "Read installed component and tool versions", "/api/environment/versions")
	post("environment.update", "Update guest components; may restart guest services", "/api/environment/update", object(nil))
	post("environment.tools", "Update tools to this build's versions", "/api/environment/tools", object(nil))
	get("resources.get", "Read platform resource limits and recommendations", "/api/environment/resources")
	post("resources.set", "Save allocation={automatic,memory_gib,cpus}; send {} separately to apply and restart", "/api/environment/resources", object(map[string]string{"allocation": "object"}))
	ops[len(ops)-1].InputSchema["properties"].(map[string]any)["allocation"] = object(map[string]string{"automatic": "boolean", "memory_gib": "integer", "cpus": "integer"})
	get("storage.get", "Read storage location and migration prerequisites", "/api/environment/storage")
	post("storage.set", "Migrate using the previous directory and has_environment value from storage.get; stops running tasks", "/api/environment/storage", object(map[string]string{"directory": "string", "previous": "string", "migrate": "boolean"}))
	post("network.check", "Run existing safety checks; may reopen networking when checks pass", "/api/safety/check", object(nil))
	post("network.pause", "Close external networking and persist pause intent", "/api/safety/close", object(nil))
	post("project.open", "Grant access only to a user-authorized directory; add=true remembers the grant", "/api/projects/open", object(map[string]string{"workspace": "string", "add": "boolean", "activate": "boolean"}))
	post("project.save", "Save a named project with user-authorized folders. For a new project, omit workspace; the first folder becomes its stable default directory. To edit, provide its existing workspace and keep that folder. Returns approval if macOS authorization is needed; retry the same request after approval", "/api/projects/save", from(projectGroup{}))
	post("project.remove", "Remove a project and revoke every bound folder share. Close its terminals, running sessions and editors first. Preserve files and history", "/api/projects/remove", object(map[string]string{"workspace": "string"}))
	post("project.refresh", "Linux guests only: forget cached names and attributes of project files so host-side changes show at once", "/api/projects/refresh", object(nil))

	for _, action := range []string{"profile", "password", "lock", "logout", "suspend"} {
		schema := object(nil)
		if action == "profile" {
			schema = object(map[string]string{"username": "string", "avatar": "string"})
		}
		if action == "password" {
			schema = object(map[string]string{"old_password": "string", "password": "string", "confirm_password": "string"})
		}
		post("account."+action, "Same account action and verification as the workbench", "/api/account/"+action, schema)
	}
	get("appearance.get", "Read complete appearance and saved themes", "/api/appearance")
	post("appearance.set", "Save complete appearance using the workbench validator", "/api/appearance", from(Appearance{}))
	get("appearance.codex", "Read the same local Codex appearance import as the UI", "/api/appearance/codex")
	post("settings.probe", "Probe actual exit location for a complete settings draft", "/api/settings/probe", from(Settings{}))
	get("proxy.connections", "Inspect live proxy connections", "/api/proxy/connections")
	get("proxy.logs", "Inspect proxy logs", "/api/proxy/logs")
	post("proxy.delay", "Measure one proxy node", "/api/proxy/delay", object(map[string]string{"node_id": "string"}))
	add("guest.reselect", "Clear only the same unstarted installation the UI can reselect", "DELETE", "/api/environment/guest-systems", object(map[string]string{"id": "string"}))
	get("claude-accounts.get", "Read account roster, quotas and scheduling", "/api/claude-accounts")
	post("claude-accounts.change", "Apply the same join, cancel, update, scheduling, refresh and removal actions as the UI", "/api/claude-accounts", from(claudeAccountInput{}))
	get("codex-accounts.get", "Read Codex account roster, quotas and scheduling (Linux guests)", "/api/codex-accounts")
	post("codex-accounts.change", "Apply the same update, scheduling, refresh and remove actions as the Codex account panel; log in through a codex-login terminal", "/api/codex-accounts", from(codexAccountInput{}))
	get("agent-clis.list", "List built-in and custom Agent CLIs, installation status, prerequisites and account summaries", "/api/agent-clis")
	post("agent-clis.install", "Install or update an Agent CLI through its official channel", "/api/agent-clis/install", object(map[string]string{"agent_id": "string", "channel": "string"}))
	post("agent-clis.uninstall", "Uninstall a non-core Agent CLI package while preserving accounts and project files", "/api/agent-clis/uninstall", object(map[string]string{"agent_id": "string", "channel": "string"}))
	post("agent-clis.config", "Update per-agent terminal launch command and ergonomic options", "/api/agent-clis/config", object(map[string]string{"agent_id": "string", "launch_command": "string", "extra_args": "string", "preferred_channel": "string", "image_paste": "boolean", "shift_enter_mode": "string", "hidden_in_menu": "boolean"}))
	post("agent-clis.custom.save", "Register or update a custom Agent CLI specification", "/api/agent-clis/custom", from(agentcatalog.AgentSpec{}))
	get("t3code.get", "Read optional T3 Code web workbench status, mode and Cage account bindings", "/api/t3code")
	post("t3code.config", "Configure optional T3 Code integration mode, port, open mode and account bindings", "/api/t3code/config", object(map[string]string{"enabled": "boolean", "mode": "string", "external_url": "string", "port": "integer", "open_mode": "string", "focus_chrome": "boolean", "account_bindings": "object"}))
	post("t3code.install", "Install official T3 Code CLI inside the Cage guest", "/api/t3code/install", object(map[string]string{"channel": "string"}))
	post("t3code.open", "Open T3 Code original web UI in the workbench or direct zero-VNC loopback window", "/api/t3code/open", object(map[string]string{"workspace": "string", "force_mode": "string"}))
	for _, action := range []string{"start", "stop", "restart"} {
		post("t3code."+action, "Control the managed T3 Code guest server", "/api/t3code/"+action, object(nil))
	}
	get("magpie.get", "Read optional Magpie model router and local gateway status", "/api/magpie")
	post("magpie.config", "Configure optional Magpie mode, port, gateway-only flag, open mode and focus chrome", "/api/magpie/config", object(map[string]string{"enabled": "boolean", "mode": "string", "external_url": "string", "port": "integer", "gateway_only": "boolean", "open_mode": "string", "focus_chrome": "boolean"}))
	post("magpie.install", "Install official Magpie inside the Cage guest", "/api/magpie/install", object(map[string]string{"channel": "string"}))
	post("magpie.uninstall", "Uninstall Magpie inside the Cage guest while preserving user data", "/api/magpie/uninstall", object(nil))
	post("magpie.open", "Open Magpie original web UI in the workbench or direct zero-VNC loopback window", "/api/magpie/open", object(map[string]string{"workspace": "string", "force_mode": "string"}))
	for _, action := range []string{"start", "stop", "restart"} {
		post("magpie."+action, "Control the managed Magpie guest server", "/api/magpie/"+action, object(nil))
	}
	get("herdr.get", "Read optional Herdr (herdr-core) status: installed, version, core running, terminal count", "/api/herdr")
	post("herdr.config", "Configure optional Herdr integration and whether workbench terminals are Herdr terminals", "/api/herdr/config", object(map[string]string{"enabled": "boolean", "takeover_terminals": "boolean"}))
	post("herdr.install", "Install official herdr-core inside the Cage guest, or keep a working one; update asks for a newer release", "/api/herdr/install", object(map[string]string{"channel": "string", "update": "boolean"}))
	post("herdr.uninstall", "Uninstall herdr-core inside the Cage guest; refuses while terminals run unless force", "/api/herdr/uninstall", object(map[string]string{"force": "boolean"}))
	for _, action := range []string{"start", "stop", "restart"} {
		post("herdr."+action, "Control the Herdr core in the guest; stop refuses while terminals run unless force, restart keeps them", "/api/herdr/"+action, object(map[string]string{"force": "boolean"}))
	}
	get("herdr.legacy", "Read whether the old Herdr TUI is still installed or running with panes", "/api/herdr/legacy")
	post("herdr.legacy_uninstall", "Remove the old Herdr TUI; refuses while it holds panes unless force", "/api/herdr/legacy/uninstall", object(map[string]string{"force": "boolean"}))
	get("vault.get", "List metadata; passwords remain hidden exactly as in the UI", "/api/vault")
	post("vault.change", "Save/delete/unblock; reveal requires the same account_password reauthentication as the UI", "/api/vault", object(map[string]string{"action": "string", "entry": "object", "id": "string", "origin": "string", "account_password": "string"}))
	ops[len(ops)-1].InputSchema["properties"].(map[string]any)["entry"] = from(vaultEntry{})
	post("vault.browser", "Use the browser vault's existing save/fill/block actions", "/api/vault/browser", from(vaultBrowserInput{}))
	get("mobile.get", "Inspect phone pairing", "/api/mobile-pair")
	post("mobile.pair", "Window only: a device is added in the desktop window, which shows the code; returns open_desktop", "/api/mobile-pair", object(map[string]string{"admin": "string", "relay": "string"}))
	add("mobile.remove", "Remove the same paired device", "DELETE", "/api/mobile-pair", object(nil))
	get("tailscale.get", "Read guest private-network state", "/api/tailscale")
	for _, action := range []string{"install", "connect", "login", "disconnect", "logout", "add-key", "remove-key"} {
		schema := object(nil)
		if action == "add-key" || action == "remove-key" {
			schema = object(map[string]string{"key": "string"})
		}
		post("tailscale."+action, "Run the same Tailscale action as the workbench", "/api/tailscale/"+action, schema)
	}
	get("browser.preferences.get", "Read the browser homepage", "/api/browser/preferences")
	post("browser.preferences.set", "Apply and persist the browser homepage with the same rollback as the UI", "/api/browser/preferences", object(map[string]string{"browser_home_url": "string"}))
	get("browser.tabs", "Read browser tabs and state", "/api/browser/tabs")
	for _, action := range []string{"settings", "cookies", "extensions", "internal-page", "info", "new", "activate", "close", "navigate", "back", "forward", "reload", "editor", "editor-restart", "split"} {
		post("browser."+action, "Use the browser's existing workbench action", "/api/browser/"+action, from(browserInput{}))
	}
	get("favorites.get", "Read bookmarks and folders", "/api/favorites")
	post("favorites.change", "Save, move, reorder or delete using the same collection owner", "/api/favorites", from(favoriteChange{}))
	get("terminals.get", "Read terminal sessions", "/api/terminals")
	post("terminals.create", "Create the same shell/Claude terminal as the UI", "/api/terminals", from(terminalRequest{}))
	get("client.context", "Read the desktop connection generation; use desktop scope", "/api/client/context")
	get("client.hosts", "Read saved SSH hosts; use desktop scope", "/api/client/hosts")
	post("client.hosts.create", "Save an SSH host through the desktop store", "/api/client/hosts", from(remotehost.HostProfile{}))
	get("client.local-status", "Inspect local workload from the desktop chooser", "/api/client/local-status")
	for _, action := range []string{"candidate", "commit", "cancel", "disconnect", "local-stop"} {
		post("client."+action, "Use the desktop chooser's same explicit connection/candidate protocol", "/api/client/"+action, from(switchRequest{}))
	}
	post("native.submit", "Queue an existing native bridge action or clientPreferences request for the desktop window; use desktop scope", "/api/agent/native", object(map[string]string{"request": "object", "connection_id": "string"}))

	get("diagnostics.status", "Read log collection status", "/api/diagnostics")
	return ops
}

type AgentError = controlclient.AgentError

func agentHTTPError(code int) *AgentError { return controlclient.HTTPError(code) }

// AgentStartService starts only an absent service. It never hands off, upgrades
// or stops an existing controller as a side effect of an agent call.
func AgentStartService(ctx context.Context, configPath string) error {
	profile := filepath.Dir(configPath)
	c, err := connectProvenController(profile)
	if err == nil {
		defer c.close()
		return agentCheckController(c, configPath)
	}
	if controllerIdentityError(err) {
		return &AgentError{Code: "controller_mismatch", Message: err.Error(), NextAction: "open_desktop"}
	}
	if !errors.Is(err, errControllerUnavailable) || !controllerLockFree(profile) {
		return &AgentError{Code: "service_unavailable", Message: "The controller cannot be authenticated. Preserve its data and inspect diagnostics.", NextAction: "doctor"}
	}
	if err := startController(configPath); err != nil {
		return &AgentError{Code: "service_start_failed", Message: "Super Cage could not start its controller.", NextAction: "doctor"}
	}
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return &AgentError{Code: "service_start_timeout", Message: "The controller has not become ready. It may still be starting.", NextAction: "doctor", Retryable: true}
		case <-timer.C:
			c, err := connectProvenController(profile)
			if err == nil {
				defer c.close()
				return agentCheckController(c, configPath)
			}
			if controllerIdentityError(err) {
				return &AgentError{Code: "controller_mismatch", Message: err.Error(), NextAction: "open_desktop"}
			}
		}
	}
}

func agentCheckController(c *controllerClient, configPath string) error {
	return controlclient.CheckLocal(c.launch, configPath, agentSameBuild(c.launch.Controller))
}

// agentSameBuild accepts only a controller built from this executable.
func agentSameBuild(id ControllerIdentity) bool {
	want, err := currentController()
	return err == nil && want.Matches(id)
}

func agentConnect(ctx context.Context, configPath string) (*controllerClient, error) {
	c, err := controlclient.AgentConnect(ctx, configPath, agentSameBuild, connectProvenControllerConn)
	if err != nil {
		return nil, err
	}
	return c.(*controllerClient), nil
}

func AgentCall(ctx context.Context, configPath, operation string, input json.RawMessage) (json.RawMessage, error) {
	return AgentCallTarget(ctx, configPath, AgentTarget{}, operation, input)
}

func agentValidateInput(input json.RawMessage, schema map[string]any) error {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(input, &value) != nil || value == nil {
		return &AgentError{Code: "invalid_input", Message: "Expected one JSON object.", NextAction: "capabilities"}
	}
	properties := schema["properties"].(map[string]any)
	for key := range value {
		if _, ok := properties[key]; !ok {
			return &AgentError{Code: "invalid_input", Message: "Unknown input field; read the operation's input_schema.", NextAction: "capabilities"}
		}
	}
	return nil
}

func AgentJob(ctx context.Context, configPath, id string) (json.RawMessage, error) {
	if len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, &AgentError{Code: "invalid_input", Message: "Expected a 12-character job id.", NextAction: "status"}
	}
	return agentRequest(ctx, configPath, "GET", "/api/agent/jobs/"+id, nil)
}

func agentRequest(ctx context.Context, configPath, method, path string, input json.RawMessage) (json.RawMessage, error) {
	response, err := AgentRequest(ctx, configPath, AgentTarget{}, method, path, strings.NewReader(string(input)), "application/json", "")
	return response.Data, err
}

func (s *Server) agentRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/agent/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job := s.jobs.snapshot()
		if job.ID == "" || job.ID != r.PathValue("id") {
			http.Error(w, "Original job no longer available", http.StatusGone)
			return
		}
		jsonReply(w, job)
	})
	mux.HandleFunc("POST /api/agent/settings", s.agentSettings)
}

func agentRedact(data string) string {
	var value any
	if json.Unmarshal([]byte(data), &value) != nil {
		return diagnostics.Redact(data)
	}
	var clean func(any)
	clean = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if text, ok := item.(string); ok && (key == "error" || key == "log" || key == "message" || key == "reason" || strings.HasSuffix(key, "_error")) {
					v[key] = diagnostics.Redact(text)
				} else {
					clean(item)
				}
			}
		case []any:
			for _, item := range v {
				clean(item)
			}
		}
	}
	clean(value)
	body, _ := json.Marshal(value)
	return string(body)
}

func (s *Server) agentSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Patch  map[string]json.RawMessage `json:"patch"`
		DryRun bool                       `json:"dry_run"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if home, ok := in.Patch["browser_home_url"]; ok && !in.DryRun {
		if len(in.Patch) != 1 {
			http.Error(w, "Change browser_home_url through browser.preferences.set separately from environment settings", 400)
			return
		}
		var value string
		if json.Unmarshal(home, &value) != nil {
			http.Error(w, "Invalid browser_home_url", 400)
			return
		}
		s.saveBrowserHome(w, r, value)
		return
	}
	if len(in.Patch) == 0 {
		http.Error(w, "patch must contain at least one setting", 400)
		return
	}
	propose := func() (savedProfile, error) {
		old, err := loadConfig(s.configPath)
		if err != nil {
			return savedProfile{}, err
		}
		s.mu.Lock()
		p := s.preferences
		s.mu.Unlock()
		data, _ := json.Marshal(settingsView(old, p))
		fields := map[string]json.RawMessage{}
		_ = json.Unmarshal(data, &fields)
		if err := mergeAgentSettings(fields, in.Patch); err != nil {
			return savedProfile{}, err
		}
		data, _ = json.Marshal(fields)
		var input Settings
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if d.Decode(&input) != nil {
			return savedProfile{}, errors.New("unknown setting or invalid value")
		}
		if _, err := validateBrowserHome(input.BrowserHomeURL); err != nil {
			return savedProfile{}, err
		}
		c, p, err := proposedSettings(input, old, s.profile)
		return savedProfile{Config: c, Preferences: p}, err
	}
	// Validate even when a different job is running; the real mutation reloads
	// current settings only after acquiring the shared operation lock.
	v, err := propose()
	if err != nil {
		requestError(w, err)
		return
	}
	if in.DryRun {
		jsonReply(w, settingsView(v.Config, v.Preferences))
		return
	}
	s.launch(w, "settings", func() error {
		v, err := propose()
		if err != nil {
			return err
		}
		return s.apply(v, true)
	})
}

// An exit_policy patch must retain its existing IP/region anchor when only one
// field changes. Null never clears a setting implicitly.
func mergeAgentSettings(current, patch map[string]json.RawMessage) error {
	for key, value := range patch {
		value = bytes.TrimSpace(value)
		if bytes.Equal(value, []byte("null")) {
			return errors.New("settings cannot be null")
		}
		var nested map[string]json.RawMessage
		if len(value) > 0 && value[0] == '{' && json.Unmarshal(value, &nested) == nil {
			existing := map[string]json.RawMessage{}
			_ = json.Unmarshal(current[key], &existing)
			if existing == nil {
				existing = map[string]json.RawMessage{}
			}
			if err := mergeAgentSettings(existing, nested); err != nil {
				return err
			}
			value, _ = json.Marshal(existing)
		}
		current[key] = value
	}
	return nil
}

func agentSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		properties := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			if f.Anonymous {
				for name, schema := range agentSchema(f.Type)["properties"].(map[string]any) {
					properties[name] = schema
				}
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				properties[name] = agentSchema(f.Type)
			}
		}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": agentSchema(t.Elem())}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": agentSchema(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}
