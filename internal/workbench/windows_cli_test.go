package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// The isolated relay child uses the real profile/operation argv and JSON stdin.
func init() {
	if os.Getenv("CAGE_WINDOWS_CLI_RELAY_TEST") != "1" {
		return
	}
	if len(os.Args) != 4 || os.Args[1] != "--relay-profile" {
		os.Exit(2)
	}
	var input map[string]json.RawMessage
	if json.NewDecoder(os.Stdin).Decode(&input) != nil {
		os.Exit(2)
	}
	var profile, action string
	_ = json.Unmarshal(input["profile"], &profile)
	_ = json.Unmarshal(input["action"], &action)
	operation := os.Args[3]
	if capture := os.Getenv("CAGE_WINDOWS_TERMINAL_RELAY_CAPTURE"); capture != "" && (action == "terminal" || action == "open") {
		valid := action == "terminal" && len(profile) == 16 && (operation == "cli" || operation == "headless-cli") ||
			action == "open" && (operation == "terminal" || operation == "headless-terminal")
		if !valid || input["command"] != nil {
			os.Exit(2)
		}
		encoded, _ := json.Marshal(map[string]any{"operation": operation, "request": input})
		if os.WriteFile(capture, encoded, 0600) != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"ready": true})
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if (operation != "cli" && operation != "headless-cli") || len(profile) != 16 || input["command"] != nil {
		os.Exit(2)
	}
	if stage := os.Getenv("CAGE_WINDOWS_CLI_RELAY_PROCESS_FAILURE"); stage != "" {
		fmt.Fprintf(os.Stderr, "Windows guest channel unavailable (%s:IOException); private-token-must-not-appear\n", stage)
		os.Exit(1)
	}
	if os.Getenv("CAGE_WINDOWS_CLI_RELAY_WARNING") == "1" {
		fmt.Fprintln(os.Stderr, strings.Repeat("warning", 4096))
	}
	if os.Getenv("CAGE_WINDOWS_CLI_RELAY_FAILURE") == "1" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": "requires_gui_login"})
		os.Exit(0)
	}
	switch action {
	case "account":
		if string(input["mode"]) == `"quotas"` {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": map[string]any{}})
			os.Exit(0)
		}
		if string(input["mode"]) != `"snapshot"` || string(input["account"]) != `"default"` {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"logged_in": false})
	case "channel":
		if os.Getenv("CAGE_WINDOWS_CLI_RELAY_MOBILE") == "1" {
			var mode string
			_ = json.Unmarshal(input["mode"], &mode)
			var query map[string]any
			if json.Unmarshal(input["input"], &query) != nil || query["workspace"] != os.Getenv("CAGE_WINDOWS_CLI_RELAY_WORKSPACE") || query["project_id"] != strings.Repeat("a", 16) || query["grant"] != strings.Repeat("b", 64) {
				os.Exit(2)
			}
			if capture := os.Getenv("CAGE_WINDOWS_CLI_RELAY_CAPTURE"); capture != "" {
				file, err := os.OpenFile(capture, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					os.Exit(2)
				}
				_ = json.NewEncoder(file).Encode(map[string]any{"mode": mode, "input": query})
				_ = file.Close()
			}
			if marker := os.Getenv("CAGE_WINDOWS_CLI_RELAY_BLOCK"); marker != "" {
				if os.WriteFile(marker, []byte("started"), 0600) != nil {
					os.Exit(2)
				}
				time.Sleep(time.Minute)
			}
			var result any = map[string]any{"ok": true}
			if mode == "view" {
				result = terminalConversation{CLIID: query["cli_id"].(string), Connection: query["connection"].(string), Config: query["config"].(string), Workspace: query["workspace"].(string), Running: true, CanSend: true, ChannelReady: true, Turns: []mobileTurn{}}
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": result})
			break
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": []any{}})
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
func windowsCLIRelayFixture(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	bindWindowsCLIRelayFixture(t, s, "hyperv")
	return s
}

func bindWindowsCLIRelayFixture(t *testing.T, s *Server, mode string) {
	t.Helper()
	binding := guestos.WindowsProfile{Version: 1, Mode: mode, VMID: uuid.New(), GUIUserSID: "S-1-5-21-123-456-789-1001", GUIUser: "Guest", InstanceDirectory: filepath.Join(s.profile, "vm"), PairingFile: filepath.Join(s.profile, "pair.dpapi"), RelayExecutable: os.Args[0]}
	if binding.Remote() {
		binding.MachineID = uuid.NewString()
	}
	if err := guestos.BindWindowsProfile(s.profile, binding); err != nil {
		t.Fatal(err)
	}
	s.windowsGuest = &binding
	t.Setenv("CAGE_WINDOWS_CLI_RELAY_TEST", "1")
}

func TestWindowsTerminalRoutesBindTheRosterAccount(t *testing.T) {
	for _, mode := range []string{"hyperv", "remote"} {
		t.Run(mode, func(t *testing.T) {
			s := agentTestController(t)
			bindWindowsCLIRelayFixture(t, s, mode)
			t.Cleanup(s.terminals.close)
			capture := filepath.Join(t.TempDir(), "relay.json")
			t.Setenv("CAGE_WINDOWS_TERMINAL_RELAY_CAPTURE", capture)
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			projectID, grant := strings.Repeat("a", 16), strings.Repeat("b", 64)
			s.ready, s.preferences.Workspace, s.windowsState.Home = true, project, project
			if err := s.rememberSharedProject(project); err != nil {
				t.Fatal(err)
			}
			s.projectShares = map[string]*projectShare{project: {
				windowsProject: &windowsProjectBinding{ID: projectID, Grant: grant, Path: `C:\Users\Guest\Cage\Projects\` + projectID},
				done:           make(chan struct{}),
			}}
			if mode == "remote" {
				previous := windowsGuestStatus
				t.Cleanup(func() { windowsGuestStatus = previous })
				windowsGuestStatus = func(*Server, context.Context) (windowsGuestState, error) { return s.windowsState, nil }
			}
			httpPost := func(payload any) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, s.origin+"/api/terminals", strings.NewReader(string(parityJSON(t, payload))))
				r.Header.Set("Origin", s.origin)
				r.Header.Set("Content-Type", "application/json")
				r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
				if mode == "remote" {
					r.AddCookie(&http.Cookie{Name: "cage_remote_view_" + r.URL.Port(), Value: remoteViewProof(s.token)})
				} else {
					r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
				}
				w := httptest.NewRecorder()
				s.handler().ServeHTTP(w, r)
				return w
			}
			for _, bad := range []terminalRequest{
				{Kind: "shell", AccountID: strings.Repeat("c", 24)},
				{Kind: "claude", AccountID: strings.Repeat("c", 24)},
				{Kind: "agent-login", AgentID: "claude", AccountID: strings.Repeat("c", 24), ClaudeAccountID: strings.Repeat("d", 24)},
				{Kind: "agent-login", AgentID: "claude", LoginMode: "device"},
				{Kind: "agent", AgentID: "codex"},
				{Kind: "codex"},
				{Kind: "herdr"},
				{Kind: "magpie-tui"},
			} {
				if w := httpPost(bad); w.Code != http.StatusBadRequest {
					t.Fatalf("unexpected admission for %#v: %d %s", bad, w.Code, w.Body.String())
				}
			}
			if _, err := os.Stat(capture); !os.IsNotExist(err) {
				t.Fatalf("invalid request started the relay: %v", err)
			}
			record := func() (string, map[string]any) {
				t.Helper()
				var result struct {
					Operation string         `json:"operation"`
					Request   map[string]any `json:"request"`
				}
				data, err := os.ReadFile(capture)
				if err != nil || json.Unmarshal(data, &result) != nil {
					t.Fatalf("unable to read relay launch: %v", err)
				}
				return result.Operation, result.Request
			}
			shellOperation, cliOperation := "terminal", "cli"
			if mode == "remote" {
				shellOperation, cliOperation = "headless-terminal", "headless-cli"
			}
			if w := httpPost(terminalRequest{Kind: "shell"}); w.Code != http.StatusOK {
				t.Fatalf("shell launch failed: %d %s", w.Code, w.Body.String())
			}
			if op, wire := record(); op != shellOperation || wire["shell"] != "powershell" || wire["account"] != nil {
				t.Fatalf("shell went to wrong relay: %s %#v", op, wire)
			}
			rosterID, guestID := strings.Repeat("c", 24), strings.Repeat("d", 24)
			account := claudeAccount{ID: rosterID, ProfileID: guestID, Source: "managed", Label: "Fixture", Email: "fixture@example.invalid", Enabled: true, Status: "ready"}
			if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{account}}); err != nil {
				t.Fatal(err)
			}
			// An external Magpie must leave the native relay's selection in place.
			if err := s.saveMagpieConfigLocked(magpieConfig{Enabled: true, Mode: "external", ExternalURL: "https://fixture.invalid"}); err != nil {
				t.Fatal(err)
			}
			w := httpPost(terminalRequest{Kind: "agent", AgentID: "claude", AccountID: "auto"})
			if w.Code != http.StatusOK {
				t.Fatalf("Claude launch failed: %d %s", w.Code, w.Body.String())
			}
			managed := parityDecode[map[string]any](t, w.Body.Bytes())
			if managed["kind"] != "claude" || managed["claude_account_id"] != rosterID {
				t.Fatalf("wrong managed Claude terminal: %#v", managed)
			}
			op, wire := record()
			if op != cliOperation || wire["kind"] != "claude" || wire["account"] != guestID || wire["subject"] != claudeIdentityHash(account.Email, account.Organization) {
				t.Fatalf("wrong managed Claude relay: %s %#v", op, wire)
			}
			if (mode == "remote" && wire["project_id"] != nil) || (mode == "hyperv" && (wire["project_id"] != projectID || wire["grant"] != grant)) {
				t.Fatalf("wrong project grant for %s: %#v", mode, wire)
			}
			s.terminals.remove(managed["id"].(string))
			s.mu.Lock()
			s.preferences.ClaudeTerminalCommand = "ccs fixture"
			s.mu.Unlock()
			w = httpPost(terminalRequest{Kind: "claude"})
			if mode == "remote" {
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "CCS") {
					t.Fatalf("headless CCS was not refused early: %d %s", w.Code, w.Body.String())
				}
			} else {
				if w.Code != http.StatusOK {
					t.Fatalf("local CCS launch failed: %d %s", w.Code, w.Body.String())
				}
				if op, wire = record(); op != "cli" || wire["ccs_profile"] != "fixture" || wire["account"] != "default" || wire["subject"] != "" {
					t.Fatalf("CCS inherited a Cage credential: %s %#v", op, wire)
				}
			}
			input := terminalRequest{Kind: "agent-login", AgentID: "claude", AccountID: rosterID, LoginMode: "oauth"}
			var body []byte
			if mode == "hyperv" {
				var err error
				body, err = AgentCall(context.Background(), s.configPath, "terminals.create", parityJSON(t, input))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				w := httpPost(input)
				if w.Code != http.StatusOK {
					t.Fatalf("login launch failed: %d %s", w.Code, w.Body.String())
				}
				body = w.Body.Bytes()
			}
			view := parityDecode[map[string]any](t, body)
			if view["kind"] != "claude-login" || view["claude_account_id"] != rosterID {
				t.Fatalf("wrong host terminal binding: %#v", view)
			}
			if op, wire := record(); op != cliOperation || wire["kind"] != "claude-login" || wire["account"] != guestID || wire["project_id"] != nil {
				t.Fatalf("wrong relay login binding: %s %#v", op, wire)
			}
		})
	}
}
func TestWindowsCLINativeReportAndArrayTransport(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	result, err := s.readWindowsClaudeAccount(context.Background(), "snapshot", "default", "")
	if err != nil || result.LoggedIn {
		t.Fatalf("native account report failed: %v", err)
	}
	var sessions []terminalConversation
	if err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "discover", "input": map[string]any{"accounts": []string{"default"}, "workspaces": map[string]string{}}}, &sessions); err != nil || sessions == nil {
		t.Fatalf("native array response failed: %v", err)
	}
}

func TestWindowsAgentCatalogUsesNativeComponentStatus(t *testing.T) {
	s := testServer(t)
	s.windowsGuest = &guestos.WindowsProfile{}
	s.windowsState.Components = map[string]windowsComponentState{"cli": {Installed: true}}
	if err := s.saveAgentStoreLocked(agentStoreData{LastProbe: map[string]agentRuntimeState{
		"codex": {Installed: true, BinaryPath: "/usr/local/bin/codex", Version: "0.1.0"},
	}}); err != nil {
		t.Fatal(err)
	}
	w := windowsBrowserRequestForTest(s, http.MethodGet, "/api/agent-clis", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("native catalog failed: %d %s", w.Code, w.Body.String())
	}
	catalog := parityDecode[struct {
		Agents []agentCatalogEntry `json:"agents"`
	}](t, w.Body.Bytes())
	observed := map[string]agentRuntimeState{}
	for _, entry := range catalog.Agents {
		observed[entry.Spec.ID] = entry.Runtime
	}
	codex, exists := observed["codex"]
	if !exists || codex != (agentRuntimeState{}) || !observed["claude"].Installed {
		t.Fatalf("catalog did not use the native CLI status: %#v", observed)
	}
}
func TestWindowsCLIRejectsRevokedAndSiblingWorkspace(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	path := `C:\Users\Guest\Cage\Projects\` + strings.Repeat("a", 16)
	share := &projectShare{windowsProject: &windowsProjectBinding{ID: strings.Repeat("a", 16), Grant: strings.Repeat("b", 64), Path: path}, done: make(chan struct{})}
	s.projectShares = map[string]*projectShare{"authorized": share}
	for _, bad := range []string{path + `-other`, path + `\..\private`} {
		if err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "handoff", "input": map[string]any{"workspace": bad}}, new(any)); err == nil {
			t.Fatal("unmapped guest workspace admitted")
		}
	}
	close(share.done)
	if err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "handoff", "input": map[string]any{"workspace": path}}, new(any)); err == nil {
		t.Fatal("revoked guest project admitted")
	}
	if !cliWorkspaceWithin(path+`\nested`, strings.ToLower(path), true) || cliWorkspaceWithin(path+`-other`, path, true) {
		t.Fatal("Windows workspace separator or case changed authorization")
	}
}

func TestWindowsCLIRPCFailureCannotSignAnAccountOut(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	t.Setenv("CAGE_WINDOWS_CLI_RELAY_FAILURE", "1")
	if _, err := s.readWindowsClaudeAccount(context.Background(), "snapshot", "default", ""); err == nil || !strings.Contains(err.Error(), "Windows 桌面尚未登录或已锁定") {
		t.Fatalf("GUI/RPC failure lost its login explanation: %v", err)
	}
}

func TestWindowsCLIProcessFailureExplainsStageWithoutLeakingStderr(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	for _, test := range []struct{ stage, message string }{
		{"profile", "无法读取 Windows 环境配置"},
		{"pairing", "安全配对校验未完成"},
		{"connect", "未能连接 Windows 会话服务"},
		{"transfer", "通信未完成"},
		{"unknown", "未能读取 Windows 会话服务的响应"},
	} {
		t.Run(test.stage, func(t *testing.T) {
			t.Setenv("CAGE_WINDOWS_CLI_RELAY_PROCESS_FAILURE", test.stage)
			err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "discover"}, new(any))
			if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "官方账号") {
				t.Fatalf("relay failure was misclassified or exposed stderr: %v", err)
			}
		})
	}
}

func TestWindowsCLIRequestTimeoutExplainsDeadline(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := s.windowsCLIRequest(ctx, map[string]any{"action": "channel", "mode": "discover"}, new(any))
	if err == nil || !strings.Contains(err.Error(), "响应超时") {
		t.Fatalf("request timeout lost its cause: %v", err)
	}
}

func TestWindowsCLILongStderrDoesNotFailSuccessfulRequest(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	t.Setenv("CAGE_WINDOWS_CLI_RELAY_WARNING", "1")
	if _, err := s.readWindowsClaudeAccount(context.Background(), "snapshot", "default", ""); err != nil {
		t.Fatalf("helper warning broke a successful response: %v", err)
	}
}
