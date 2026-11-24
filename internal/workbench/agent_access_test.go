package workbench

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// Every classified action exists in a shell, except those of branches
	// not merged yet; once a shell answers one of those, it leaves the list.
	for _, table := range []map[string]string{agentNativeWindowOnly, func() map[string]string {
		names := map[string]string{}
		for action := range agentNativeAccess {
			names[action] = ""
		}
		return names
	}()} {
		for action := range table {
			branch, ahead := agentNativeAhead[action]
			if ahead && seen[action] {
				t.Errorf("a shell answers %s now: drop it from agentNativeAhead (branch %s merged)", action, branch)
			}
			if !ahead && !seen[action] {
				t.Errorf("%s is classified but no shell answers it", action)
			}
		}
	}
	for action := range agentNativeAhead {
		_, open := agentNativeAccess[action]
		_, window := agentNativeWindowOnly[action]
		if !open && !window {
			t.Errorf("agentNativeAhead lists %s without classifying it", action)
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
		{"PUT", "/api/client/local-terminal"}, {"POST", "/api/client/local-terminal/open"}, {"POST", "/api/client/local-terminal/install-herdr"}, {"GET", "/api/client/local-terminal/app"},
		{"POST", "/api/claude-science/host-render"}, {"POST", "/api/claude-science/ticket"}, {"POST", "/api/claude-science/release"},
		{"POST", "/api/app-update/policy"}, {"GET", "/api/app-update/interruptions"}, {"POST", "/api/desktop-local/app-update-handoff"},
		{"GET", "/api/entry"}, {"POST", "/api/entry/on"}, {"GET", "/api/entry/include"}, {"POST", "/api/entry/devices/x/revoke"}, {"POST", "/api/client/entry/keys"},
		{"POST", "/api/client/trusted-login"}, {"POST", "/api/account/prepare-macos"}, {"POST", "/api/account/device-enroll"},
		{"POST", "/api/input/files/desktop/open"}, {"POST", "/api/agent/native/x/complete"}, {"GET", "/api/agent/native?consumer=window"},
		{"POST", "/api/terminals/t1/keep"}, {"GET", "/api/entry/ssh"}, {"POST", "/api/entry/include"}, {"POST", "/api/entry/devices/x/rename"}, {"POST", "/api/entry/off"},
		{"GET", "/api/client/virtual-machines"},
		{"POST", "/api/client/devices/join"}, {"POST", "/api/client/devices/0123456789abcdef0123456789abcdef/open"}, {"GET", "/api/client/devices"},
		{"POST", "/api/agents/codex/install"}, {"POST", "/api/agents/codex/update"}, {"POST", "/api/agents/codex/remove"}, {"POST", "/api/agents/cancel"},
		{"POST", "/api/devices/invite"}, {"POST", "/api/mobile-pair"},
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
		"/api/client%2Flocal-terminal/app", "/api/client/local-terminal/app/", "/api/entry/", "/api/app-update/interruptions/",
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
				access, _ := agentRouteClass(route)
				if access == "not_open" {
					t.Errorf("%s (%s listener, %s guest) changes state and is neither open to agents nor window only: classify it in agent_access.go", route, listener, guest)
				}
				// Every GET route may be a WebSocket: cage agent socket must
				// answer it exactly as cage agent request does.
				if method, path, _ := strings.Cut(route, " "); method == "GET" && strings.HasPrefix(path, "/api/") {
					sample := regexp.MustCompile(`\{[^/]*\}`).ReplaceAllString(path, "x")
					socket := agentSocketRefusal(sample)
					if (socket != nil) != (access == "window_only") || (socket != nil && !reflect.DeepEqual(socket, agentRouteRefusal("GET", sample))) {
						t.Errorf("socket gate and request gate disagree on %s: %v", route, socket)
					}
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
	// HandleFunc call of this package. A literal pattern is classified here;
	// one built at run time (mobile_browser.go) must match a route the walk
	// above registered, or it has to be classified by hand.
	walked := maps.Clone(registered)
	fset := token.NewFileSet()
	sources, _ := filepath.Glob("*.go")
	calls := 0
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(source, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "HandleFunc" {
				return false // the recording mux itself
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "HandleFunc" {
				return true
			}
			calls++
			where := fset.Position(call.Pos()).String()
			if pattern, ok := astString(call.Args[0]); ok {
				registered[pattern] = true
				if access, _ := agentRouteClass(pattern); access == "not_open" {
					t.Errorf("%s (%s) changes state and is neither open to agents nor window only: classify it in agent_access.go", pattern, where)
				}
				return true
			}
			shape := regexp.MustCompile("^" + astShape(call.Args[0]) + "$")
			seen := 0
			for route := range walked {
				if shape.MatchString(route) {
					seen++
				}
			}
			if seen == 0 {
				t.Errorf("%s builds a route at run time that the route walk never registered: classify it by hand", where)
			}
			return true
		})
	}
	if calls < 150 {
		t.Fatalf("found only %d HandleFunc calls; the scan no longer reads the package", calls)
	}
	for route, branch := range agentRoutesAhead {
		if registered[route] {
			t.Errorf("%s is registered now: drop it from agentRoutesAhead (branch %s merged)", route, branch)
		}
		if !slices.Contains(agentRoutes, route) {
			t.Errorf("agentRoutesAhead lists %s, which agentRoutes does not open", route)
		}
	}
	// No stale entries: an open pattern is registered, or a value of a family,
	// or a /api/client/ route dispatched outside the mux.
	for _, open := range agentRoutes {
		family := false
		for _, f := range agentRouteFamilies {
			family = family || strings.HasPrefix(open, strings.TrimSuffix(f, "{action}"))
		}
		_, path, _ := strings.Cut(open, " ")
		if !registered[open] && !family && agentRoutesAhead[open] == "" && !strings.HasPrefix(path, "/api/client/") {
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
// routes the guide documents, with the bodies they are used with; what is
// refused on purpose is checked to be refused.
func TestAgentDocumentedOperationsStayOpen(t *testing.T) {
	refusal := func(method, path, body string) *AgentError {
		if refusal := agentRouteRefusal(method, path); refusal != nil {
			return refusal
		}
		return agentBodyRefusal(agentBodyRulesFor(method, path), []byte(body))
	}
	// Kept as shortcuts that send the agent to the desktop window.
	windowOnly := map[string]bool{"mobile.pair": true}
	var names []string
	for _, op := range AgentOperations() {
		names = append(names, op.Name)
		refused := refusal(op.method, op.path, "{}")
		if windowOnly[op.Name] {
			if refused == nil || refused.Code != "native_transport_required" || refused.NextAction != "open_desktop" {
				t.Errorf("%s (%s %s) must send the agent to the desktop window: %v", op.Name, op.method, op.path, refused)
			}
			continue
		}
		if refused != nil {
			t.Errorf("%s (%s %s): refused %v", op.Name, op.method, op.path, refused)
		}
	}
	sort.Strings(names)
	for _, route := range [][3]string{
		{"PUT", "/api/client/hosts/host-1", "{}"}, {"DELETE", "/api/client/hosts/host-1", ""}, {"POST", "/api/client/connect", "{}"},
		{"POST", "/api/cli/sessions/s1/metadata", "{}"}, {"POST", "/api/cli/sessions/s1/account", "{}"}, {"POST", "/api/cli/sessions/s1/archive", "{}"},
		{"POST", "/api/cli/sessions/s1/restore", "{}"}, {"POST", "/api/cli/sessions/s1/purge", "{}"}, {"POST", "/api/cli/sessions/s1/open", "{}"},
		{"POST", "/api/macos/first-setup", `{"action":"permissions"}`}, {"POST", "/api/macos/login", `{"action":"open"}`},
		{"POST", "/api/macos/login", `{"action":"click","x":1,"y":2}`}, {"POST", "/api/macos/maintenance", `{"action":"fill-password","maintenance":"projects"}`},
		{"POST", "/api/macos/project-approval", `{"action":"cancel"}`}, {"POST", "/api/browser/extensions/upload", "ZIP"},
		{"GET", "/api/diagnostics/export", ""}, {"GET", "/api/terminals/t1/stream", ""}, {"GET", "/api/agent/native/abc", ""}, {"POST", "/api/agent/native", "{}"},
		{"GET", "/api/client/local-terminal", ""}, {"GET", "/api/app-update", ""}, {"POST", "/api/claude-science/start", "{}"}, {"HEAD", "/api/status", ""},
		{"DELETE", "/api/client/project-shares/share-1", ""}, {"GET", "/api/client/project-shares", ""},
		{"GET", "/api/devices", ""}, {"GET", "/api/devices/invite", ""}, {"DELETE", "/api/devices/invite", ""},
		{"DELETE", "/api/devices/0123456789abcdef0123456789abcdef", ""}, {"GET", "/api/mobile-pair", ""}, {"DELETE", "/api/mobile-pair", ""},
	} {
		if refused := refusal(route[0], route[1], route[2]); refused != nil {
			t.Errorf("%s %s %s refused: %v", route[0], route[1], route[2], refused)
		}
	}
	// Refused on purpose: each returns a secret that unlocks the account or
	// the guest login, or acts on this computer.
	for _, route := range [][3]string{
		{"POST", "/api/client/trusted-login", "{}"}, {"POST", "/api/account/recovery-generate", `{"password":"x"}`}, {"POST", "/api/account/recovery-reset", "{}"},
		{"POST", "/api/macos/login", `{"action":"show-password"}`}, {"POST", "/api/macos/first-setup", `{"action":"show-password"}`},
		{"POST", "/api/macos/maintenance", `{"action":"show-password"}`}, {"POST", "/api/environment/macos-screen-sharing", "{}"},
		{"POST", "/api/client/project-shares", `{"path":"/Users/x"}`}, {"POST", "/api/client/browser-extensions", "{}"}, {"GET", "/api/client/browser-extensions", ""},
		{"POST", "/api/devices/invite", "{}"}, {"POST", "/api/mobile-pair", "{}"},
	} {
		if refused := refusal(route[0], route[1], route[2]); refused == nil || refused.NextAction == "" {
			t.Errorf("%s %s %s is open to agents", route[0], route[1], route[2])
		}
	}
	if len(names) < 60 {
		t.Fatal("named operations missing", len(names))
	}
}

func astString(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

// astShape turns "POST "+route+"/open" into a pattern its routes match.
func astShape(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return astShape(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return astShape(x.X) + astShape(x.Y)
		}
	}
	if value, ok := astString(e); ok {
		return regexp.QuoteMeta(value)
	}
	return ".*"
}

type clientDispatch struct{ method, path string }

// clientDispatchRoutes reads the /api/client/ dispatcher's own conditions:
// every path (and method, where the condition names one) compared in
// clientRequest, desktopGateway.handle, tenantClientRequest and
// clientProjectShareRequest.
func clientDispatchRoutes(t *testing.T) []clientDispatch {
	t.Helper()
	dispatchers := map[string]string{"remote_gateway.go": "clientRequest", "desktop_gateway.go": "handle", "tenant_client.go": "tenantClientRequest", "cloud_client_share.go": "clientProjectShareRequest"}
	var routes []clientDispatch
	var condition func(ast.Expr) []clientDispatch
	method := func(e ast.Expr) string {
		if value, ok := astString(e); ok {
			return value
		}
		if sel, ok := e.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Method") {
			return strings.ToUpper(strings.TrimPrefix(sel.Sel.Name, "Method"))
		}
		return ""
	}
	isIdent := func(e ast.Expr, name string) bool { id, ok := e.(*ast.Ident); return ok && id.Name == name }
	isMethod := func(e ast.Expr) bool { sel, ok := e.(*ast.SelectorExpr); return ok && sel.Sel.Name == "Method" }
	condition = func(e ast.Expr) []clientDispatch {
		switch x := e.(type) {
		case *ast.ParenExpr:
			return condition(x.X)
		case *ast.BinaryExpr:
			switch x.Op {
			case token.LOR:
				return append(condition(x.X), condition(x.Y)...)
			case token.LAND:
				var out []clientDispatch
				for _, a := range condition(x.X) {
					for _, b := range condition(x.Y) {
						out = append(out, clientDispatch{cmp.Or(a.method, b.method), cmp.Or(a.path, b.path)})
					}
				}
				return out
			case token.EQL:
				if value, ok := astString(x.Y); ok && isIdent(x.X, "path") {
					return []clientDispatch{{path: value}}
				}
				if isMethod(x.X) {
					return []clientDispatch{{method: method(x.Y)}}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "HasPrefix" && len(x.Args) == 2 && isIdent(x.Args[0], "path") {
				if value, ok := astString(x.Args[1]); ok {
					return []clientDispatch{{path: value + "x"}}
				}
			}
		}
		return []clientDispatch{{}}
	}
	fset := token.NewFileSet()
	for name, function := range dispatchers {
		source, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, decl := range source.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != function {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.IfStmt:
					routes = append(routes, condition(x.Cond)...)
				case *ast.SwitchStmt:
					for _, clause := range x.Body.List {
						for _, e := range clause.(*ast.CaseClause).List {
							if isIdent(x.Tag, "path") {
								if value, ok := astString(e); ok {
									routes = append(routes, clientDispatch{path: value})
								}
							} else if x.Tag == nil {
								routes = append(routes, condition(e)...)
							}
						}
					}
				}
				return true
			})
		}
		if !found {
			t.Fatalf("dispatcher %s not found in %s", function, name)
		}
	}
	var out []clientDispatch
	for _, route := range routes {
		if route.path != "" && !slices.Contains(out, route) {
			out = append(out, route)
		}
	}
	return out
}

// Every /api/client/ route the dispatcher answers (outside the mux) is open,
// window only, or read-only; read from the dispatcher, not listed by hand.
func TestAgentRegistryClassifiesClientRoutes(t *testing.T) {
	routes := clientDispatchRoutes(t)
	var paths []string
	for _, route := range routes {
		paths = append(paths, route.path)
	}
	for _, want := range []string{"hosts", "hosts/x", "trusted-login", "tenant/login", "project-shares", "candidate", "browser-extensions", "virtual-machines/x"} {
		if !slices.Contains(paths, want) {
			t.Fatalf("the dispatcher scan missed %s; it no longer reads the dispatcher: %v", want, paths)
		}
	}
	for _, route := range routes {
		// A prefix that only hands over to a sub-dispatcher whose paths are
		// read too (tenant/) is checked through those paths.
		if prefix, ok := strings.CutSuffix(route.path, "/x"); ok && slices.ContainsFunc(paths, func(p string) bool { return p != route.path && strings.HasPrefix(p, prefix+"/") }) {
			continue
		}
		// A path compared without a method only to hand over to a
		// sub-dispatcher that names the methods (project-shares/).
		if route.method == "" && slices.ContainsFunc(routes, func(o clientDispatch) bool { return o.method != "" && o.path == route.path }) {
			continue
		}
		method := cmp.Or(route.method, "POST")
		if method == "GET" || method == "HEAD" {
			continue
		}
		path := "/api/client/" + route.path
		if refusal := agentRouteRefusal(method, path); refusal != nil && refusal.Code == "route_not_open" {
			t.Errorf("%s %s is dispatched for /api/client/ and changes state, but is neither open to agents nor window only", method, path)
		}
	}
}

// show-password is refused however the body spells it, as the handler would
// decode it; a body the CLI cannot decode that way is not sent at all.
func TestAgentBodyRulesReadTheBodyAsTheHandlerDoes(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	send := func(path, body string) *AgentError {
		_, err := AgentRequest(context.Background(), config, AgentTarget{Scope: "selected", Connection: "c1"}, "POST", path, strings.NewReader(body), "application/json", "")
		var ae *AgentError
		if !errors.As(err, &ae) {
			t.Fatalf("%s %s: %v", path, body, err)
		}
		return ae
	}
	for _, path := range []string{"/api/macos/login", "/api/macos/first-setup", "/api/macos/maintenance"} {
		for _, body := range []string{
			`{"action":"show-password"}`, `{"Action":"show-password"}`, `{"ACTION":"show-password"}`,
			`{"action":"open","action":"show-password"}`, `{"action":"open","Action":"show-password"}`,
			` {"action":"show\u002dpassword"} `,
		} {
			if ae := send(path, body); ae.Code != "native_transport_required" || ae.NextAction != "open_desktop" {
				t.Errorf("%s %s: %+v", path, body, ae)
			}
		}
		for _, body := range []string{`{"action":"open"} {"action":"show-password"}`, `[{"action":"show-password"}]`, `{"action":["show-password"]}`, `{"action":7}`, "not json", strings.Repeat(" ", agentBodyLimit+1)} {
			if ae := send(path, body); ae.Code != "invalid_input" {
				t.Errorf("%s %.40q was not refused as unreadable: %+v", path, body, ae)
			}
		}
		// An ordinary action passes the gate and goes on to the (absent)
		// desktop service.
		if ae := send(path, `{"action":"open","x":1}`); ae.Code == "native_transport_required" || ae.Code == "invalid_input" {
			t.Errorf("%s open refused: %+v", path, ae)
		}
	}
}

// A real window-only route, end to end: the window's session gets a recovery
// key from it; the same request through the agent transport is refused before
// it reaches the handler, and the key in force is unchanged.
func TestAgentCannotTakeARecoveryKeyFromALiveController(t *testing.T) {
	s := agentTestController(t)
	a, err := loadLocalAccount(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.lock)
	s.account = a
	w := request(s, "POST", "/api/account/create", s.origin, `{"username":"agent_qa","password":"original-test-password","confirm_password":"original-test-password"}`, true)
	var created struct {
		RecoveryKey string `json:"recovery_key"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.RecoveryKey == "" {
		t.Fatal("account", w.Code, w.Body.String())
	}
	body := `{"password":"original-test-password"}`
	_, err = AgentRequest(context.Background(), s.configPath, AgentTarget{}, "POST", "/api/account/recovery-generate", strings.NewReader(body), "application/json", "")
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != "native_transport_required" || ae.NextAction != "open_desktop" {
		t.Fatalf("agent took a recovery key: %v", err)
	}
	// The original key still resets the password: nothing was rotated.
	a.lock()
	if _, err := a.resetPassword(created.RecoveryKey, "recovered-test-password"); err != nil {
		t.Fatal("the agent's refused request changed the recovery key", err)
	}
	// The route is live: the window's own session is answered.
	if err := a.unlock("recovered-test-password"); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "POST", "/api/account/recovery-generate", s.origin, `{"password":"recovered-test-password"}`, true); w.Code != 200 || !strings.Contains(w.Body.String(), "recovery_key") {
		t.Fatal("the route under test is not live", w.Code, w.Body.String())
	}
	// On the same controller show-password never leaves the CLI, while an
	// ordinary action reaches the controller (which answers it without a
	// macOS guest).
	_, err = AgentRequest(context.Background(), s.configPath, AgentTarget{}, "POST", "/api/macos/login", strings.NewReader(`{"action":"show-password"}`), "application/json", "")
	if !errors.As(err, &ae) || ae.Code != "native_transport_required" {
		t.Fatalf("show-password reached the controller: %v", err)
	}
	_, err = AgentRequest(context.Background(), s.configPath, AgentTarget{}, "POST", "/api/macos/login", strings.NewReader(`{"action":"open"}`), "application/json", "")
	if !errors.As(err, &ae) || ae.HTTPStatus == 0 {
		t.Fatalf("an ordinary macOS window action did not reach the controller: %v", err)
	}
}
