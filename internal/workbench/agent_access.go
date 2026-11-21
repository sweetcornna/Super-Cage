package workbench

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// What `cage agent` may do. It is the person's own automation driving the
// workbench, so workbench work stays open to it. What it never does is act on
// this computer outside the workbench without the person: install or replace
// Super Cage, launch host applications, read or replace the host clipboard,
// change what the desktop window itself trusts, or use the window's own
// transports. Those routes and native actions are window only.
//
// Both lists are default deny. A route that changes state (anything but GET
// and HEAD) is refused unless it is listed in agentRoutes, and a native action
// unless agentNativeAccess lists it with a matching request. A new route or
// action therefore ships closed to agents until it is classified here, and the
// tests in agent_access_test.go fail for any registered route or shell action
// that is in neither list.

// agentWindowRoute is a route family the agent transport refuses for every
// method. A pattern is "[METHOD[,METHOD…]] /path", where {name} matches one
// path segment, {name...} the rest, and a trailing "/" the whole subtree.
type agentWindowRoute struct {
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
	next    string
}

const agentTransportReason = "Use native.submit with the same accountDevice or desktop action. Transport credentials are not exported."

var agentWindowRoutes = []agentWindowRoute{
	// The launcher's and the window's own transports.
	{"/api/session", agentTransportReason, "native.submit"},
	{"/api/launch", agentTransportReason, "native.submit"},
	{"/api/handoff", agentTransportReason, "native.submit"},
	{"/api/desktop-owner", agentTransportReason, "native.submit"},
	{"/api/desktop-local/", agentTransportReason, "native.submit"},
	{"POST /api/agent/native/window", agentTransportReason, "native.submit"},
	{"GET /api/agent/native", "Only the desktop window claims native requests; read your own request with GET /api/agent/native/ID.", "native.submit"},
	{"POST /api/agent/native/{id}/complete", "Only the desktop window that ran a native action reports its result.", "native.submit"},
	// Device keys unlock the account through the native secure store.
	{"POST /api/account/device-enroll", agentTransportReason, "native.submit"},
	{"POST /api/account/device-unlock", agentTransportReason, "native.submit"},
	{"POST /api/account/device-revoke", agentTransportReason, "native.submit"},
	{"POST /api/account/prepare-macos", "The login password shared with the macOS guest is set in the desktop window.", "open_desktop"},
	{"/api/client/virtual-machines", "Local virtual machines are managed only by the native desktop window; profile grants are not exported through the agent transport.", "open_desktop"},
	{"/api/client/virtual-machines/", "Local virtual machines are managed only by the native desktop window; profile grants are not exported through the agent transport.", "open_desktop"},
	{"/api/client/trusted-login", "Remembering the login on this computer changes what the desktop window trusts; the person sets it in the window.", "open_desktop"},
	// Application updates replace Super Cage itself. The mode decides whether
	// it may do so unattended; the confirmation list hands out the ids that
	// let an update interrupt work.
	{"/api/app-update/", "Application updates are configured and confirmed only in the desktop window.", "open_desktop"},
	// Opening a workbench file with a host application (claim and complete).
	{"POST /api/input/files/desktop/open", "Opening a file with an application on this computer is done by the desktop window.", "open_desktop"},
	// The terminal app on this computer: choosing it, listing installed
	// apps and opening windows in it (launches host applications and may
	// raise the macOS automation prompt). Reading the setting stays open.
	{"PUT,POST,PATCH,DELETE /api/client/local-terminal", "The terminal app on this computer is chosen and opened by the person in the desktop window.", "open_desktop"},
	{"/api/client/local-terminal/", "The terminal app on this computer is chosen and opened by the person in the desktop window.", "open_desktop"},
	// Claude Science host rendering runs a renderer on this computer and
	// hands the window a frame to trust.
	{"POST /api/claude-science/host-render", "Claude Science host rendering is started and released by the desktop window.", "open_desktop"},
	{"POST /api/claude-science/ticket", "Claude Science host rendering is started and released by the desktop window.", "open_desktop"},
	{"POST /api/claude-science/release", "Claude Science host rendering is started and released by the desktop window.", "open_desktop"},
	// Reserved for the SSH entry: turning it on, its device keys and its
	// connections change who can reach this computer.
	{"/api/entry", "The SSH entry and its device keys are managed by the person in the desktop window.", "open_desktop"},
	{"/api/entry/", "The SSH entry and its device keys are managed by the person in the desktop window.", "open_desktop"},
	{"/api/client/entry", "The SSH entry and its device keys are managed by the person in the desktop window.", "open_desktop"},
	{"/api/client/entry/", "The SSH entry and its device keys are managed by the person in the desktop window.", "open_desktop"},
	// Keeping a terminal open after its SSH-entry connection ends (branch
	// local-terminal-ssh) is decided by the person.
	{"POST /api/terminals/{id}/keep", "Keeping a terminal open for the SSH entry is decided by the person in the desktop window.", "open_desktop"},
	// A paired phone reports its own device.
	{"POST /api/mobile/device", "Only a paired phone reports its own device details.", "mobile.get"},
	// Opens Screen Sharing on this computer (open vnc://…).
	{"POST /api/environment/macos-screen-sharing", "Screen Sharing opens on this computer only from the desktop window.", "open_desktop"},
	// A recovery key resets the account password.
	{"POST /api/account/recovery-generate", "A recovery key unlocks the account; the person creates and keeps it in the desktop window.", "open_desktop"},
	{"POST /api/account/recovery-reset", "A recovery key unlocks the account; the person uses it in the desktop window.", "open_desktop"},
	// Giving a cloud host access to a folder on this computer (the same as
	// the window-only native shareLocalProject); withdrawing a share with
	// DELETE stays open.
	{"POST /api/client/project-shares", "Giving a cloud host access to a folder on this computer is done in the desktop window.", "open_desktop"},
	// The host browser's profiles and extension files.
	{"/api/client/browser-extensions", "The host browser's profiles and extension files are read only by the desktop window.", "open_desktop"},
}

// agentBodyRule refuses a request to an otherwise open route by a value in
// its JSON body: an action switch whose value returns a secret or acts on this
// computer. The CLI reads the body (at most agentBodyLimit bytes) and decodes
// Field the way the handler decodes it (encoding/json into a struct field:
// case-insensitive, the last matching key wins). A body it cannot decode that
// way is refused, so nothing it did not read is sent.
type agentBodyRule struct {
	Pattern string   `json:"pattern"`
	Field   string   `json:"field"`
	Refused []string `json:"refused"`
	Reason  string   `json:"reason"`
}

const agentBodyLimit = 1 << 20

const agentGuestPasswordReason = "The macOS guest's stored login password is shown only in the desktop window."

var agentBodyRules = []agentBodyRule{
	// The three macOS window routes share one handler (macOSLoginWindow),
	// where show-password returns the guest's stored login password.
	{"POST /api/macos/login", "action", []string{"show-password"}, agentGuestPasswordReason},
	{"POST /api/macos/first-setup", "action", []string{"show-password"}, agentGuestPasswordReason},
	{"POST /api/macos/maintenance", "action", []string{"show-password"}, agentGuestPasswordReason},
}

// agentBodyRulesFor returns the body rules of method path.
func agentBodyRulesFor(method, path string) []agentBodyRule {
	var rules []agentBodyRule
	for _, rule := range agentBodyRules {
		if agentPatternMatches(rule.Pattern, method, path) {
			rules = append(rules, rule)
		}
	}
	return rules
}

// agentBodyRefusal checks body against rules; an empty body decodes as {}.
func agentBodyRefusal(rules []agentBodyRule, body []byte) *AgentError {
	if len(body) > agentBodyLimit {
		return &AgentError{Code: "invalid_input", Message: "This route's request body is larger than the agent transport reads; nothing was sent.", NextAction: "capabilities"}
	}
	for _, rule := range rules {
		value := reflect.New(reflect.StructOf([]reflect.StructField{{Name: "Value", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`json:"` + rule.Field + `"`)}}))
		if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, value.Interface()) != nil {
			return &AgentError{Code: "invalid_input", Message: "This route's request body must be one JSON object whose " + rule.Field + " is a string; nothing was sent.", NextAction: "capabilities"}
		}
		if slices.Contains(rule.Refused, value.Elem().Field(0).String()) {
			return &AgentError{Code: "native_transport_required", Message: rule.Reason, NextAction: "open_desktop"}
		}
	}
	return nil
}

// agentRoutes are the state-changing routes an agent may call: the
// workbench's own work (environment, terminals, sessions, projects, guest
// apps, accounts in the guest, network, vault, phone pairing) and the desktop
// chooser. Patterns are the registered ones; /api/client/ routes, which are
// dispatched outside the mux, are listed with their concrete paths.
var agentRoutes = []string{
	"POST /api/start", "POST /api/stop", "POST /api/restore", "POST /api/settings", "POST /api/settings/probe", "POST /api/agent/settings",
	"POST /api/agent/native", "POST /api/appearance", "POST /api/favorites", "POST /api/diagnostics/events",
	"DELETE /api/environment/guest-systems", "POST /api/environment/guest-systems/activate", "POST /api/environment/guest-profile/preview",
	"POST /api/environment/cache/clean", "POST /api/environment/cache/inventory", "POST /api/environment/cache/restore",
	"POST /api/environment/device-check", "POST /api/environment/initialization",
	"POST /api/environment/resources", "POST /api/environment/storage", "POST /api/environment/storage/recover",
	"POST /api/environment/tools", "POST /api/environment/update",
	"POST /api/terminals", "DELETE /api/terminals/{id}", "POST /api/terminals/{id}/login-link", "POST /api/terminals/{id}/metadata",
	"POST /api/cli/sessions/{id}/account", "POST /api/cli/sessions/{id}/archive", "POST /api/cli/sessions/{id}/metadata", "POST /api/cli/sessions/{id}/open",
	"POST /api/cli/sessions/{id}/purge", "POST /api/cli/sessions/{id}/restore", "POST /api/cli/sessions/{id}/stop",
	"POST /api/projects/open", "POST /api/projects/save", "POST /api/projects/remove", "POST /api/projects/refresh", "POST /api/projects/client-share",
	"POST /api/browser/{action}", "POST /api/browser/clipboard", "POST /api/browser/extensions/upload", "POST /api/browser/files/uploads/{id}", "POST /api/browser/preferences",
	"POST /api/claude-desktop/{action}", "POST /api/claude-desktop/resize",
	"POST /api/claude-science/install", "POST /api/claude-science/update", "POST /api/claude-science/start", "POST /api/claude-science/stop", "POST /api/claude-science/open",
	"POST /api/macos-browser/action", "POST /api/macos-browser/window", "POST /api/macos/claude/window", "POST /api/macos/exit-policy",
	"POST /api/macos/first-setup", "POST /api/macos/install-recovery", "POST /api/macos/login", "POST /api/macos/maintenance", "POST /api/macos/project-approval",
	"POST /api/input", "POST /api/input/text", "POST /api/input/image", "POST /api/input/paste/start", "POST /api/input/paste/cancel",
	"POST /api/input/files/{action}", "POST /api/input/files/{target}/{id}",
	"POST /api/clipboard/copy", "POST /api/clipboard/files", "DELETE /api/clipboard/files/{id}",
	"POST /api/claude-accounts", "POST /api/codex-accounts", "POST /api/vault", "POST /api/vault/browser",
	"POST /api/proxy/{action}", "POST /api/proxy/delay", "POST /api/safety/check", "POST /api/safety/close", "POST /api/tailscale/{action}",
	"POST /api/mobile-pair", "DELETE /api/mobile-pair",
	"POST /api/mobile/browser/open", "POST /api/mobile/browser/favorites", "POST /api/mobile/browser/{id}/action", "POST /api/mobile/browser/{id}/close",
	"POST /api/mobile/cli/sessions", "POST /api/mobile/cli/sessions/{id}/account", "POST /api/mobile/cli/sessions/{id}/approval",
	"POST /api/mobile/cli/sessions/{id}/archive", "POST /api/mobile/cli/sessions/{id}/delete", "POST /api/mobile/cli/sessions/{id}/media",
	"POST /api/mobile/cli/sessions/{id}/messages", "POST /api/mobile/cli/sessions/{id}/metadata", "POST /api/mobile/cli/sessions/{id}/open",
	"POST /api/mobile/cli/sessions/{id}/purge", "POST /api/mobile/cli/sessions/{id}/restore", "POST /api/mobile/cli/sessions/{id}/stop",
	"POST /api/mobile/desktop/open", "POST /api/mobile/desktop/{id}/action", "POST /api/mobile/desktop/{id}/close",
	"POST /api/mobile/environment/start", "POST /api/mobile/environment/stop", "POST /api/mobile/environment/restart",
	"POST /api/mobile/environment/proxy/delay", "POST /api/mobile/environment/proxy/select",
	"POST /api/mobile/files/list", "POST /api/mobile/files/mkdir", "POST /api/mobile/files/read",
	"POST /api/mobile/files/upload/start", "POST /api/mobile/files/upload/chunk", "POST /api/mobile/files/upload/finish", "POST /api/mobile/files/upload/abort",
	"POST /api/mobile/terminals", "POST /api/mobile/terminals/{id}/close", "POST /api/mobile/terminals/{id}/input", "POST /api/mobile/terminals/{id}/resize",
	// The workbench account: the same verification as the window (password,
	// recovery key or an unlocked session). Device keys and the macOS shared
	// password are window only (above).
	"POST /api/account/create", "POST /api/account/unlock", "POST /api/account/profile", "POST /api/account/password",
	"POST /api/account/lock", "POST /api/account/logout", "POST /api/account/suspend",
	// The desktop chooser, SSH hosts, cloud project shares and tenants.
	"POST /api/client/about",
	"POST /api/client/hosts", "PUT /api/client/hosts/{id}", "DELETE /api/client/hosts/{id}",
	"POST /api/client/connect", "POST /api/client/disconnect", "POST /api/client/local",
	"POST /api/client/candidate", "POST /api/client/commit", "POST /api/client/cancel", "POST /api/client/local-stop",
	"DELETE /api/client/project-shares/{id}",
	"POST /api/client/tenant/login", "POST /api/client/tenant/vms",
	// Routes of the browser-data branch, classified ahead of its merge:
	// the guest browser's browsing data.
	"POST /api/browser/data", "POST /api/browser/data-settings",
}

// agentRoutesAhead are open routes of branches not merged yet, by branch. The
// route test fails once one of them is registered here, so the entry is
// dropped from this list when its branch merges.
var agentRoutesAhead = map[string]string{
	"POST /api/browser/data":          "browser-data",
	"POST /api/browser/data-settings": "browser-data",
}

// agentRouteFamilies are registered {action} routes whose values differ:
// only the values listed in agentRoutes are open, the rest are refused.
var agentRouteFamilies = []string{"POST /api/account/{action}", "POST /api/claude-science/{action}"}

// agentRouteRefusal returns why an agent may not call method path, or nil.
func agentRouteRefusal(method, path string) *AgentError {
	for _, rule := range agentWindowRoutes {
		if agentPatternMatches(rule.Pattern, method, path) {
			return &AgentError{Code: "native_transport_required", Message: rule.Reason, NextAction: rule.next}
		}
	}
	if method == http.MethodGet || method == http.MethodHead {
		return nil
	}
	for _, pattern := range agentRoutes {
		if agentPatternMatches(pattern, method, path) {
			return nil
		}
	}
	return &AgentError{Code: "route_not_open", Message: "This route changes state and is not open to agents; nothing was sent. Read capabilities for the routes agents may call, or use the desktop window.", NextAction: "capabilities"}
}

// agentPlainPath reports whether a decoded path is already in the form the
// server matches: no "." or ".." segment and no empty segment except a
// trailing slash. Callers also refuse a path whose percent-encoding changes
// its text (url.URL.RawPath), so what is classified is what the server routes.
func agentPlainPath(path string) bool {
	segments := strings.Split(path, "/")
	for i, segment := range segments[1:] {
		if segment == "." || segment == ".." || (segment == "" && i != len(segments)-2) {
			return false
		}
	}
	return true
}

// agentSocketRefusal is the gate for `cage agent socket`: the same path shape
// as a request, then the same registry for the decoded path (a WebSocket is
// a GET upgrade).
func agentSocketRefusal(path string) *AgentError {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/api/") || strings.ContainsAny(path, "\r\n\\#") || u.RawPath != "" || !agentPlainPath(u.Path) {
		return &AgentError{Code: "invalid_input", Message: "Use a relative /api/ WebSocket path without traversal or unusual escaping", NextAction: "routes"}
	}
	return agentRouteRefusal(http.MethodGet, u.Path)
}

func agentPatternMatches(pattern, method, path string) bool {
	methods, route, ok := strings.Cut(pattern, " ")
	if !ok {
		methods, route = "", pattern
	}
	if methods != "" {
		matched := false
		for _, m := range strings.Split(methods, ",") {
			matched = matched || m == method || (m == http.MethodGet && method == http.MethodHead)
		}
		if !matched {
			return false
		}
	}
	want, got := strings.Split(route, "/"), strings.Split(path, "/")
	subtree := strings.HasSuffix(route, "/")
	if subtree {
		want = want[:len(want)-1]
	}
	for i, segment := range want {
		if strings.HasSuffix(segment, "...}") {
			return i < len(got)
		}
		if i >= len(got) || (segment != got[i] && !(strings.HasPrefix(segment, "{") && got[i] != "")) {
			return false
		}
	}
	if subtree {
		return len(got) > len(want)
	}
	return len(got) == len(want)
}

// agentNativeAccess lists, per native action, the requests an agent may
// queue for the desktop window: each entry names fields that must be equal,
// and an empty entry accepts any request. Everything else is refused before
// it is queued, and the window checks the same table again
// (agentNativeAccess in ui/src/agent-native.js, kept equal by
// TestAgentNativeAccessMatchesTheWindow).
var agentNativeAccess = map[string][]map[string]string{
	// The window's own layout, zoom, colours and connection.
	"clientPreferences": {{}},
	"appearance":        {{}},
	"refreshSession":    {{}},
	"reconnect":         {{}},
	// System pickers the person answers; nothing is read or written unless
	// the person chooses a file or folder.
	"chooseFolder":        {{}},
	"chooseGuestMedia":    {{}},
	"chooseUploadFiles":   {{}},
	"uploadHostFiles":     {{}},
	"importTheme":         {{}},
	"exportTheme":         {{}},
	"exportDiagnostics":   {{}},
	"saveBrowserDownload": {{}},
	// Reading the update state and looking for a newer version; never a
	// download, an install or a cancel.
	"appUpdate": {{"operation": "status"}, {"operation": "check"}},
	// Quick login: its state, turning it off, and Touch ID, which the person
	// answers. A key that unlocks without the person is window only.
	"accountDevice": {{"operation": "status"}, {"operation": "disable"}, {"operation": "enable", "kind": "biometric"}, {"operation": "unlock", "kind": "biometric"}},
}

// agentNativeWindowOnly names every other native action of the three shells
// and why only the window performs it.
var agentNativeWindowOnly = map[string]string{
	"workbenchReady":        "confirms to the installer that a new version launched",
	"quitNow":               "quits Super Cage without the quit confirmation",
	"appReference":          "opens the default browser on this computer",
	"guestInstallReference": "opens the default browser on this computer",
	"desktopFileAction":     "opens a file with an application on this computer",
	"browserDownloadAction": "opens or reveals a download with an application on this computer",
	"shareLocalProject":     "gives a cloud host access to a folder on this computer",
	"saveRecoveryKey":       "writes the account recovery key the window showed the person",
	"remoteInput":           "drives this computer's keyboard focus and input method",
	"clipboardRead":         "reads this computer's clipboard",
	"clipboardReadContent":  "reads this computer's clipboard",
	"clipboardRevision":     "watches this computer's clipboard",
	"clipboardSyncRead":     "reads this computer's clipboard",
	"clipboardSyncWrite":    "replaces this computer's clipboard",
	"clipboardSyncRelease":  "part of the window's clipboard sync",
	"clipboardSyncCancel":   "part of the window's clipboard sync",
	"clipboardWrite":        "replaces this computer's clipboard",
	"scienceFrame":          "changes the frame the desktop window loads and trusts",
}

// agentNativeAhead are classified native actions of branches not merged yet,
// by branch. The shell test fails once a shell answers one of them, so the
// entry is dropped from this list when its branch merges.
var agentNativeAhead = map[string]string{"scienceFrame": "claude-science-p2"}

// agentNativeRefusal returns why an agent may not queue request, or "".
func agentNativeRefusal(request map[string]json.RawMessage) string {
	var action string
	_ = json.Unmarshal(request["action"], &action)
	for _, shape := range agentNativeAccess[action] {
		if agentNativeShapeMatches(shape, request) {
			return ""
		}
	}
	if reason, ok := agentNativeWindowOnly[action]; ok {
		return "Native action " + action + " is window only: it " + reason + "."
	}
	if _, ok := agentNativeAccess[action]; ok {
		return "This " + action + " request is window only; agents may send " + agentNativeShapes(action) + "."
	}
	return "Unknown native action; agents may send " + strings.Join(agentNativeActionNames(), ", ") + "."
}

func agentNativeShapeMatches(shape map[string]string, request map[string]json.RawMessage) bool {
	for field, want := range shape {
		var got string
		if json.Unmarshal(request[field], &got) != nil || got != want {
			return false
		}
	}
	return true
}

func agentNativeShapes(action string) string {
	var shapes []string
	for _, shape := range agentNativeAccess[action] {
		data, _ := json.Marshal(shape)
		shapes = append(shapes, string(data))
	}
	return strings.Join(shapes, " or ")
}

func agentNativeActionNames() []string {
	names := make([]string, 0, len(agentNativeAccess))
	for name := range agentNativeAccess {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// agentRouteClass says what a registered route pattern allows an agent:
// open, window_only, partly_open (an {action} family with listed values) or
// not_open (state-changing and not classified, so refused).
func agentRouteClass(pattern string) (access, reason string) {
	method, route, ok := strings.Cut(pattern, " ")
	if !ok {
		method, route = http.MethodGet, pattern
	}
	segments := strings.Split(route, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") {
			segments[i] = "x"
		}
	}
	sample := strings.Join(segments, "/")
	for _, rule := range agentWindowRoutes {
		if agentPatternMatches(rule.Pattern, method, sample) {
			return "window_only", rule.Reason
		}
	}
	if method == http.MethodGet || method == http.MethodHead {
		return "open", ""
	}
	for _, family := range agentRouteFamilies {
		if family == pattern {
			prefix := strings.TrimSuffix(pattern, "{action}")
			var values []string
			for _, open := range agentRoutes {
				if strings.HasPrefix(open, prefix) {
					values = append(values, strings.TrimPrefix(open, prefix))
				}
			}
			return "partly_open", "Agents may use only: " + strings.Join(values, ", ") + "; the other values are window only."
		}
	}
	for _, open := range agentRoutes {
		if open == pattern {
			return "open", ""
		}
	}
	return "not_open", "Changes state and is not classified for agents; refused."
}

type agentRouteView struct {
	Route  string `json:"route"`
	Access string `json:"access"`
	Reason string `json:"reason,omitempty"`
}

func agentRoutesWithAccess(routes []string) map[string]any {
	views := make([]agentRouteView, 0, len(routes))
	for _, route := range routes {
		access, reason := agentRouteClass(route)
		views = append(views, agentRouteView{route, access, reason})
	}
	summary := AgentAccess()
	summary["listener_routes"] = views
	return summary
}

// AgentAccess is the agent-visible summary of the rules above, for
// capabilities and GET /api/agent/routes?access=1.
func AgentAccess() map[string]any {
	return map[string]any{
		"rule":               "Agents drive the workbench. Acting on this computer outside the workbench without the person (installing or replacing Super Cage, launching host applications, the host clipboard, what the desktop window trusts, the window's own transports) is window only. GET and HEAD are open unless listed as window only; other methods are open only on the listed routes.",
		"window_only_routes": agentWindowRoutes,
		"window_only_bodies": agentBodyRules,
		"routes":             agentRoutes,
		"native":             agentNativeAccess,
		"native_window_only": agentNativeWindowOnly,
	}
}
