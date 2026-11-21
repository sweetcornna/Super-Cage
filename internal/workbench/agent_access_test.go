package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

// The window's copy of agentNativeAccess (ui/src/agent-native.js) must equal
// the server's, and both must answer the shared cases the same way.
func TestAgentNativeAccessMatchesTheWindow(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "agent-native.js"))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)// BEGIN agentNativeAccess\nexport const agentNativeAccess = Object\.freeze\((.*?)\);\n// END agentNativeAccess`).FindSubmatch(source)
	if block == nil {
		t.Fatal("agentNativeAccess block not found in ui/src/agent-native.js")
	}
	var window map[string][]map[string]string
	if err := json.Unmarshal(block[1], &window); err != nil {
		t.Fatal("the window's table is not JSON:", err)
	}
	if !reflect.DeepEqual(window, agentNativeAccess) {
		t.Fatalf("the window and the server disagree on what agents may ask:\nwindow %v\nserver %v", window, agentNativeAccess)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "agent-native-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Request map[string]json.RawMessage `json:"request"`
		Allowed bool                       `json:"allowed"`
	}
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) < 40 {
		t.Fatal("shared cases unreadable", err, len(cases))
	}
	for _, c := range cases {
		if allowed := agentNativeRefusal(c.Request) == ""; allowed != c.Allowed {
			raw, _ := json.Marshal(c.Request)
			t.Errorf("%s: allowed %v, want %v", raw, allowed, c.Allowed)
		}
	}
}

// Every native action the three shells answer is classified, once.
func TestAgentNativeAccessCoversEveryShellAction(t *testing.T) {
	read := func(parts ...string) string {
		data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	region := func(text, from, to string) string {
		start := strings.Index(text, from)
		if start < 0 {
			t.Fatalf("%q not found", from)
		}
		end := strings.Index(text[start:], to)
		if end < 0 {
			t.Fatalf("%q not found after %q", to, from)
		}
		return text[start : start+end]
	}
	shells := map[string]*regexp.Regexp{
		region(read("desktop", "Cage.swift"), "func userContentController(", `default: replyHandler(nil,"未知操作")`): regexp.MustCompile(`(?m)^        case "([A-Za-z]+)":`),
		read("desktop", "windows", "MainWindow.cs"): regexp.MustCompile(`(?m)^                case "([A-Za-z]+)":`),
		region(read("desktop", "linux", "cage-desktop.c"), `const char *name = text_member(request, "action");`, "\n}\n"): regexp.MustCompile(`strcmp\(name, "([A-Za-z]+)"\)`),
	}
	seen := map[string]bool{"clientPreferences": true} // answered by the page itself
	for text, pattern := range shells {
		matches := pattern.FindAllStringSubmatch(text, -1)
		if len(matches) < 20 {
			t.Fatalf("found only %d native actions in a shell; the parser no longer matches it", len(matches))
		}
		for _, match := range matches {
			action := match[1]
			seen[action] = true
			_, open := agentNativeAccess[action]
			_, window := agentNativeWindowOnly[action]
			if open == window {
				t.Errorf("native action %s must be either open to agents or window only (open %v, window only %v)", action, open, window)
			}
		}
	}
	for action := range agentNativeAccess {
		if !seen[action] {
			t.Errorf("agentNativeAccess lists %s, which no shell answers", action)
		}
	}
	for action := range agentNativeWindowOnly {
		if !seen[action] && action != "scienceFrame" { // claude-science branch
			t.Errorf("agentNativeWindowOnly lists %s, which no shell answers", action)
		}
	}
}

// A refused native request reaches the agent as an error with a next action
// and is never queued for the window.
func TestAgentNativeSubmitRefusesWindowOnlyRequests(t *testing.T) {
	s := agentTestController(t)
	submit := func(request string) (json.RawMessage, error) {
		return AgentCall(context.Background(), s.configPath, "native.submit", json.RawMessage(`{"connection_id":"local","request":`+request+`}`))
	}
	for _, request := range []string{
		`{"action":"appUpdate","operation":"install","confirmed":[]}`,
		`{"action":"appUpdate","operation":"cancel"}`,
		`{"action":"appUpdate","operation":"download"}`,
		`{"action":"workbenchReady"}`,
		`{"action":"quitNow"}`,
		`{"action":"appReference","kind":"releases"}`,
		`{"action":"clipboardRead"}`,
		`{"action":"clipboardWrite","text":"x"}`,
		`{"action":"desktopFileAction","id":"00000000-0000-0000-0000-000000000000"}`,
		`{"action":"shareLocalProject","mode":"write","connection_id":"local"}`,
		`{"action":"accountDevice","operation":"enable","kind":"automatic"}`,
		`{"action":"noSuchAction"}`,
	} {
		_, err := submit(request)
		var ae *AgentError
		if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusForbidden || ae.NextAction == "" {
			t.Fatalf("%s was not refused with a next action: %v", request, err)
		}
	}
	s.agentNativeQueue.mu.Lock()
	queued := len(s.agentNativeQueue.items)
	s.agentNativeQueue.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d refused native requests were queued for the window", queued)
	}
	for _, request := range []string{`{"action":"appUpdate","operation":"check"}`, `{"action":"chooseFolder"}`, `{"action":"accountDevice","operation":"status"}`} {
		if _, err := submit(request); err != nil {
			t.Fatalf("%s refused: %v", request, err)
		}
	}
}

// Window-only routes and unclassified state changes are refused before the
// CLI connects to anything (the configuration below has no controller).
func TestAgentTransportRefusesWindowOnlyRoutes(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	refused := func(method, path, code string) {
		t.Helper()
		for _, target := range []AgentTarget{{}, {Scope: "desktop"}, {Scope: "selected", Connection: "c1"}} {
			_, err := AgentRequest(context.Background(), config, target, method, path, strings.NewReader(`{}`), "application/json", "")
			var ae *AgentError
			if !errors.As(err, &ae) || ae.Code != code || ae.NextAction == "" {
				t.Errorf("%s %s (%s scope): want %s with a next action, got %v", method, path, target.Scope, code, err)
			}
		}
	}
	for _, route := range [][2]string{
		{"PUT", "/api/client/local-terminal"}, {"POST", "/api/client/local-terminal/open"}, {"GET", "/api/client/local-terminal/apps"},
		{"POST", "/api/claude-science/host-render"}, {"POST", "/api/claude-science/ticket"}, {"POST", "/api/claude-science/release"},
		{"POST", "/api/app-update/policy"}, {"GET", "/api/app-update/interruptions"}, {"POST", "/api/desktop-local/app-update-handoff"},
		{"GET", "/api/entry"}, {"POST", "/api/entry/on"}, {"GET", "/api/entry/include"}, {"POST", "/api/entry/devices/x/revoke"}, {"POST", "/api/client/entry/keys"},
		{"POST", "/api/client/trusted-login"}, {"POST", "/api/account/prepare-macos"}, {"POST", "/api/account/device-enroll"},
		{"POST", "/api/input/files/desktop/open"}, {"POST", "/api/agent/native/x/complete"}, {"GET", "/api/agent/native?consumer=window"},
		{"GET", "/api/client/virtual-machines"},
	} {
		refused(route[0], route[1], "native_transport_required")
	}
	for _, route := range [][2]string{
		{"POST", "/api/not-a-route"}, {"PUT", "/api/terminals"}, {"DELETE", "/api/settings"},
		{"POST", "/api/account/some-new-action"}, {"POST", "/api/claude-science/some-new-action"}, {"POST", "/api/client/some-new-route"},
	} {
		refused(route[0], route[1], "route_not_open")
	}
	// Another spelling of a window-only path is refused too, never classified
	// as something else and then routed by the server to the same handler.
	beforeConnecting := map[string]bool{"invalid_input": true, "native_transport_required": true}
	for _, path := range []string{
		"/api/%65ntry/ssh", "/api/./app-update/interruptions", "/api/app-update/x/../interruptions", "/api/terminals/..",
		"/api/client%2Flocal-terminal/apps", "/api/client/local-terminal/apps/", "/api/entry/", "/api/app-update/interruptions/",
	} {
		for _, method := range []string{"GET", "POST"} {
			_, err := AgentRequest(context.Background(), config, AgentTarget{}, method, path, nil, "", "")
			var ae *AgentError
			if !errors.As(err, &ae) || !beforeConnecting[ae.Code] {
				t.Errorf("%s %s was not refused before connecting: %v", method, path, err)
			}
		}
	}
	for _, path := range []string{"/api/entry/ssh?device=x", "/api/%65ntry/ssh", "/api/./entry/ssh", "/api/entry/ssh/..", "/api/entry/ssh/", "/api/terminals/x/../../entry/ssh"} {
		err := AgentSocket(context.Background(), config, AgentTarget{}, path, io.NopCloser(strings.NewReader("")), io.Discard)
		var ae *AgentError
		if !errors.As(err, &ae) || !beforeConnecting[ae.Code] {
			t.Errorf("socket to %s: %v", path, err)
		}
	}
	var ae *AgentError
	// Reading the terminal-app setting stays open: this request goes on to
	// look for the (absent) desktop service.
	_, err := AgentRequest(context.Background(), config, AgentTarget{Scope: "desktop"}, "GET", "/api/client/local-terminal", nil, "", "")
	if !errors.As(err, &ae) || ae.Code == "native_transport_required" || ae.Code == "route_not_open" {
		t.Errorf("reading the terminal-app setting was refused: %v", err)
	}
}

// Every route registered on the workload, desktop and client listeners, for
// every guest, is open, window only, or a family with listed values. A new
// state-changing route fails here until it is classified.
func TestAgentRegistryClassifiesEveryRoute(t *testing.T) {
	registered := map[string]bool{}
	for _, listener := range []string{"workload", "desktop", "client"} {
		for _, guest := range []string{"linux", "macos", "windows"} {
			s := testServer(t)
			s.desktopEntry, s.clientOnly = listener == "desktop", listener == "client"
			switch guest {
			case "macos":
				s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"}
			case "windows":
				s.windowsGuest = &guestos.WindowsProfile{}
			}
			w := request(s, "GET", "/api/agent/routes", "", "", true)
			var routes []string
			if err := json.Unmarshal(w.Body.Bytes(), &routes); err != nil || len(routes) < 50 {
				t.Fatal(listener, guest, w.Code, err)
			}
			for _, route := range routes {
				registered[route] = true
				if access, _ := agentRouteClass(route); access == "not_open" {
					t.Errorf("%s (%s listener, %s guest) changes state and is neither open to agents nor window only: classify it in agent_access.go", route, listener, guest)
				}
			}
			w = request(s, "GET", "/api/agent/routes?access=1", "", "", true)
			var view struct {
				Routes []agentRouteView   `json:"listener_routes"`
				Window []agentWindowRoute `json:"window_only_routes"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Routes) != len(routes) || len(view.Window) != len(agentWindowRoutes) {
				t.Fatal("routes with access", listener, guest, err)
			}
		}
	}
	// Registrations that only some platforms or build tags reach: every
	// literal "METHOD /path" handed to HandleFunc in this package.
	sources, _ := filepath.Glob("*.go")
	literal := regexp.MustCompile(`HandleFunc\("([A-Z]+ /[^"]*)"`)
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range literal.FindAllStringSubmatch(string(data), -1) {
			registered[match[1]] = true
			if access, _ := agentRouteClass(match[1]); access == "not_open" {
				t.Errorf("%s (%s) changes state and is neither open to agents nor window only: classify it in agent_access.go", match[1], name)
			}
		}
	}
	// Classified ahead of their branches' merge; drop from here once merged.
	pending := map[string]bool{"POST /api/browser/data": true, "POST /api/browser/data-settings": true, "POST /api/terminals/{id}/keep": true}
	// No stale entries: an open pattern is registered, or a value of a family,
	// or a /api/client/ route dispatched outside the mux.
	for _, open := range agentRoutes {
		family := false
		for _, f := range agentRouteFamilies {
			family = family || strings.HasPrefix(open, strings.TrimSuffix(f, "{action}"))
		}
		_, path, _ := strings.Cut(open, " ")
		if !registered[open] && !family && !pending[open] && !strings.HasPrefix(path, "/api/client/") {
			t.Errorf("agentRoutes lists %s, which no listener registers", open)
		}
		if access, _ := agentRouteClass(open); access == "window_only" {
			t.Errorf("%s is both open and window only", open)
		}
	}
	for _, f := range agentRouteFamilies {
		if !registered[f] {
			t.Errorf("family %s is not registered", f)
		}
	}
}

// What agents used before still works: every named operation and the raw
// routes the guide documents, except the ones refused on purpose.
func TestAgentDocumentedOperationsStayOpen(t *testing.T) {
	var names []string
	for _, op := range AgentOperations() {
		names = append(names, op.Name)
		if refusal := agentRouteRefusal(op.method, op.path); refusal != nil {
			t.Errorf("%s (%s %s): refused %v", op.Name, op.method, op.path, refusal)
		}
	}
	// Remembering the login changes what the window trusts: refused on
	// purpose, and no longer offered as a named operation.
	if agentRouteRefusal("POST", "/api/client/trusted-login") == nil {
		t.Error("trusted login is open to agents")
	}
	sort.Strings(names)
	for _, route := range [][2]string{
		{"PUT", "/api/client/hosts/host-1"}, {"DELETE", "/api/client/hosts/host-1"}, {"POST", "/api/client/connect"},
		{"POST", "/api/cli/sessions/s1/metadata"}, {"POST", "/api/cli/sessions/s1/account"}, {"POST", "/api/cli/sessions/s1/archive"},
		{"POST", "/api/cli/sessions/s1/restore"}, {"POST", "/api/cli/sessions/s1/purge"}, {"POST", "/api/cli/sessions/s1/open"},
		{"POST", "/api/macos/first-setup"}, {"POST", "/api/macos/login"}, {"POST", "/api/macos/maintenance"}, {"POST", "/api/macos/project-approval"},
		{"POST", "/api/browser/extensions/upload"}, {"GET", "/api/diagnostics/export"}, {"GET", "/api/terminals/t1/stream"},
		{"GET", "/api/agent/native/abc"}, {"POST", "/api/agent/native"}, {"GET", "/api/client/local-terminal"}, {"GET", "/api/app-update"},
		{"POST", "/api/account/recovery-generate"}, {"POST", "/api/claude-science/start"}, {"HEAD", "/api/status"},
	} {
		if refusal := agentRouteRefusal(route[0], route[1]); refusal != nil {
			t.Errorf("%s %s refused: %v", route[0], route[1], refusal)
		}
	}
	if len(names) < 60 {
		t.Fatal("named operations missing", len(names))
	}
}
