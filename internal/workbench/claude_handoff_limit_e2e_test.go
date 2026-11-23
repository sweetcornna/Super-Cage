//go:build !windows

package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
)

// limitHarness runs the shipped workbench routes and the official CLI with two
// synthetic subscription identities (a, b) against a scripted model that can
// answer account a with a real subscription limit (HTTP 429 and the unified
// rate-limit headers the CLI reads). No real account directory is read.
type limitHarness struct {
	t             *testing.T
	s             *Server
	home, project string
	list          claudeAccountList
	limited       atomic.Value // "" or the limited window: "five_hour", "seven_day"
	resetsAt      atomic.Int64
	mu            sync.Mutex
	bodies        map[string][]string // account letter -> request bodies
	usage         map[string]int      // account letter -> usage reports served
	script        func(account, body string) (stop string, block map[string]any)
}

func newLimitHarness(t *testing.T) *limitHarness {
	t.Helper()
	binary := os.Getenv("CAGE_TEST_CLAUDE_CLI")
	features := os.Getenv("CAGE_TEST_CHANNEL_FEATURES")
	if runtime.GOOS != "darwin" || !filepath.IsAbs(binary) || !filepath.IsAbs(features) {
		t.Skip("set CAGE_TEST_CLAUDE_CLI and CAGE_TEST_CHANNEL_FEATURES on macOS for the isolated official-CLI harness")
	}
	binary, _ = filepath.EvalSymlinks(binary)
	h := &limitHarness{t: t, s: testServer(t), bodies: map[string][]string{}, usage: map[string]int{}}
	h.limited.Store("")
	h.resetsAt.Store(time.Now().Add(80 * time.Hour).Unix())
	s := h.s
	h.home, _ = filepath.EvalSymlinks(t.TempDir())
	h.project, _ = filepath.EvalSymlinks(t.TempDir())
	home, project := h.home, h.project
	s.ready = true
	s.preferences.Workspace = project
	if err := writePrivate(filepath.Join(s.profile, "environment.json"), map[string]string{"directory": project}); err != nil {
		t.Fatal(err)
	}
	personal, _ := os.UserHomeDir()
	quote := func(value string) string { b, _ := json.Marshal(value); return string(b) }
	policy := "(version 1)(allow default)(deny mach-lookup)(deny file-read-data (subpath " + quote(personal) + ") (subpath \"/Library/Keychains\"))(allow file-read* (literal " + quote(binary) + "))(deny network-outbound)(allow network-outbound (remote ip \"localhost:*\"))"
	bin := filepath.Join(home, "bin")
	_ = os.Mkdir(bin, 0700)
	shim := "#!/bin/sh\nexec /usr/bin/sandbox-exec -p " + shellescape.Quote(policy) + " " + shellescape.Quote(binary) + " --debug-file \"$HOME/cli-$$.log\" --model sonnet --permission-mode bypassPermissions \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export PATH="+shellescape.Quote(bin)+":/usr/bin:/bin\n"), 0600)
	helper := filepath.Join(bin, "cage-claude-account")
	original, _ := os.ReadFile("../cage/claude-account.py")
	if err := os.WriteFile(helper, original, 0700); err != nil {
		t.Fatal(err)
	}
	runtimeHelper, _ := os.ReadFile("../cage/client_runtime.py")
	if err := os.WriteFile(filepath.Join(bin, "client_runtime.py"), runtimeHelper, 0600); err != nil {
		t.Fatal(err)
	}
	model := httptest.NewServer(http.HandlerFunc(h.answer))
	t.Cleanup(model.Close)
	proxy, ca := mobileOfficialTestProxy(t, model)
	flags := map[string]bool{}
	if data, err := os.ReadFile(features); err == nil {
		_ = json.Unmarshal(data, &flags)
	}
	if err := writePrivate(filepath.Join(home, ".claude.json"), map[string]any{"hasCompletedOnboarding": true, "cachedGrowthBookFeatures": flags, "cachedGrowthBookFeaturesAt": time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	h.list = claudeAccountList{Scheduling: claudeScheduling{Mode: "sequential", SwitchPolicy: "ask"}}
	for _, id := range []string{strings.Repeat("a", 24), strings.Repeat("b", 24)} {
		email := id[:1] + "@example.invalid"
		h.list.Accounts = append(h.list.Accounts, claudeAccount{ID: id, Label: strings.ToUpper(id[:1]), Email: email, Enabled: true, Status: "ready"})
		config := filepath.Join(home, ".local/share/cage/claude-accounts", id, "config")
		if err := writePrivate(filepath.Join(config, ".claude.json"), map[string]any{"hasCompletedOnboarding": true, "cachedGrowthBookFeatures": flags, "cachedGrowthBookFeaturesAt": time.Now().UnixMilli(), "oauthAccount": map[string]string{"accountUuid": uuid.NewString(), "emailAddress": email}, "projects": map[string]any{project: map[string]bool{"hasTrustDialogAccepted": true}}}); err != nil {
			t.Fatal(err)
		}
		if err := writePrivate(filepath.Join(config, ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat01-synthetic-" + id[:1], "refreshToken": "synthetic", "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max"}}); err != nil {
			t.Fatal(err)
		}
		if err := writePrivate(filepath.Join(config, "settings.json"), map[string]any{"skipDangerousModePermissionPrompt": true, "statusLine": map[string]string{"type": "command", "command": "printf test"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.saveClaudeAccounts(h.list); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "NO_PROXY=localhost,127.0.0.1", "NODE_EXTRA_CA_CERTS=" + ca, "SSL_CERT_FILE=" + ca, "DISABLE_AUTOUPDATER=1", "TERM=xterm-256color"}
	mobileTestSSHRewrite(t, s, home, env, func(command string) string {
		return strings.ReplaceAll(command, "/usr/local/bin/cage-claude-account", helper)
	})
	s.claudeAccounts.probe = func(ctx context.Context, id string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: id[:1] + "@example.invalid"}, nil
	}
	t.Cleanup(func() {
		if t.Failed() {
			h.dump()
		}
		output, _ := exec.Command("pgrep", "-f", regexp.QuoteMeta(home)).Output()
		for _, value := range strings.Fields(string(output)) {
			if pid, err := strconv.Atoi(value); err == nil && pid > 0 {
				p, _ := os.FindProcess(pid)
				_ = p.Kill()
				_ = p.Release()
			}
		}
	})
	t.Cleanup(s.terminals.closeAll)
	return h
}

// dump keeps the evidence of a failed run in the test's log directory.
func (h *limitHarness) dump() {
	dir := os.Getenv("CAGE_TEST_EVIDENCE")
	if dir == "" {
		return
	}
	name := strings.NewReplacer("/", "-").Replace(h.t.Name())
	states, _ := filepath.Glob(filepath.Join(h.home, ".local/share/cage/cli-channels/*/state.json"))
	for i, path := range states {
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-state-%d.json", name, i)), b, 0600)
	}
	logs, _ := filepath.Glob(filepath.Join(h.home, "cli-*.log"))
	for i, path := range logs {
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-cli-%d.log", name, i)), b, 0600)
	}
	for key, terminal := range h.s.terminals.sessions {
		terminal.mu.Lock()
		_ = os.WriteFile(filepath.Join(dir, name+"-term-"+key+".txt"), []byte(terminal.history), 0600)
		terminal.mu.Unlock()
	}
	h.mu.Lock()
	b, _ := json.MarshalIndent(h.bodies, "", " ")
	h.mu.Unlock()
	_ = os.WriteFile(filepath.Join(dir, name+"-requests.json"), b, 0600)
}

func (h *limitHarness) answer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/messages/count_tokens" {
		_, _ = io.WriteString(w, `{"input_tokens":7}`)
		return
	}
	account := "?"
	if match := regexp.MustCompile(`synthetic-([ab])`).FindStringSubmatch(r.Header.Get("Authorization")); match != nil {
		account = match[1]
	}
	if r.URL.Path == "/api/oauth/usage" {
		// The subscription usage report the official CLI reads for Cage.
		used := 12.0
		if window, _ := h.limited.Load().(string); window != "" && account == "a" {
			used = 100
		}
		h.mu.Lock()
		h.usage[account]++
		h.mu.Unlock()
		reset := time.Unix(h.resetsAt.Load(), 0).UTC().Format(time.RFC3339)
		_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 20, "resets_at": time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)}, "seven_day": map[string]any{"utilization": used, "resets_at": reset}})
		return
	}
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.bodies[account] = append(h.bodies[account], string(body))
	h.mu.Unlock()
	if window, _ := h.limited.Load().(string); window != "" && account == "a" {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(h.resetsAt.Load(), 10))
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", window)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &input)
	stop, block := "end_turn", map[string]any{"type": "text", "text": "ok"}
	if h.script != nil {
		stop, block = h.script(account, string(body))
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, fields map[string]any) {
		fields["type"] = kind
		b, _ := json.Marshal(fields)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
		w.(http.Flusher).Flush()
	}
	event("message_start", map[string]any{"message": map[string]any{"id": "msg_" + uuid.NewString(), "type": "message", "role": "assistant", "model": input.Model, "content": []string{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 8, "output_tokens": 1}}})
	if block["type"] == "tool_use" {
		arguments, _ := json.Marshal(block["input"])
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_" + uuid.NewString(), "name": block["name"], "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(arguments)}})
	} else {
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]string{"type": "text_delta", "text": block["text"].(string)}})
	}
	event("content_block_stop", map[string]any{"index": 0})
	event("message_delta", map[string]any{"delta": map[string]string{"stop_reason": stop}, "usage": map[string]int{"output_tokens": 5}})
	event("message_stop", map[string]any{})
}

func (h *limitHarness) requests(account string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies[account]...)
}

// call sends one authenticated request to a session route and decodes it.
func (h *limitHarness) call(id, method, path string, body any, out any) int {
	h.t.Helper()
	raw := []byte(nil)
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	response := request(h.s, method, "/api/mobile/cli/sessions/"+id+path, h.s.origin, string(raw), true)
	if out != nil && response.Code == 200 {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			h.t.Fatal(err)
		}
	}
	if out == nil && response.Code != 200 {
		h.t.Logf("%s %s: %d %s", method, path, response.Code, response.Body.String())
	}
	return response.Code
}

func (h *limitHarness) start() string {
	h.t.Helper()
	id := uuid.NewString()
	response := request(h.s, "POST", "/api/mobile/cli/sessions", h.s.origin, `{"id":"`+id+`","workspace":"`+h.project+`"}`, true)
	if response.Code != 200 {
		h.t.Fatal(response.Code, response.Body.String())
	}
	h.wait(id, "a session that can send", func(v mobileSession) bool { return v.CanSend != nil && *v.CanSend })
	return id
}

func (h *limitHarness) view(id string) mobileSession {
	var view mobileSession
	if code := h.call(id, "GET", "", nil, &view); code != 200 {
		h.t.Fatalf("view: %d", code)
	}
	return view
}

func (h *limitHarness) wait(id, what string, predicate func(mobileSession) bool) mobileSession {
	h.t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	var view mobileSession
	for time.Now().Before(deadline) {
		view = h.view(id)
		if predicate(view) {
			return view
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, _ := json.Marshal(view)
	h.t.Fatalf("waiting for %s: %s", what, raw)
	return view
}

func (h *limitHarness) send(id, text string) string {
	h.t.Helper()
	requestID := uuid.NewString()
	if code := h.call(id, "POST", "/messages", map[string]string{"request_id": requestID, "text": text}, nil); code != 200 {
		h.t.Fatalf("send %q: %d", text, code)
	}
	return requestID
}

func (h *limitHarness) state(id string) map[string]any {
	m := &h.s.mobileCLI
	m.mu.Lock()
	record, _ := m.record(h.s.profile, id)
	m.mu.Unlock()
	data, _ := os.ReadFile(filepath.Join(h.home, ".local/share/cage/cli-channels", record.Connection, "state.json"))
	var state map[string]any
	_ = json.Unmarshal(data, &state)
	return state
}

// script answers the main conversation of each scenario; anything else (a
// title or a side query) gets a plain answer.
func (h *limitHarness) messages(body string) string {
	var input struct {
		Messages json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &input)
	return string(input.Messages)
}

func (h *limitHarness) record(id string) *mobileRecord {
	m := &h.s.mobileCLI
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.record(h.s.profile, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return record
}

func (h *limitHarness) running(pattern string) []string {
	output, _ := exec.Command("pgrep", "-f", pattern).Output()
	return strings.Fields(string(output))
}

func (h *limitHarness) lastBody(account, marker string) string {
	bodies := h.requests(account)
	for i := len(bodies) - 1; i >= 0; i-- {
		if strings.Contains(h.messages(bodies[i]), marker) {
			return h.messages(bodies[i])
		}
	}
	return ""
}

// At a real subscription limit (HTTP 429 with the unified headers) under the
// automatic rule the session continues on B by itself: the background
// command it started is stopped, the messages A never took arrive once, the
// resume prompt says what was interrupted, and another session of A is not
// touched.
func TestClaudeLimitHandoffEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.list.Scheduling = claudeScheduling{Mode: "sequential", SwitchPolicy: "allow", SwitchAnswered: true, UngroupedHandoff: true}
	if err := h.s.saveClaudeAccounts(h.list); err != nil {
		t.Fatal(err)
	}
	marker := "sleep 6" + strconv.Itoa(100+int(time.Now().UnixNano()%800))
	h.script = func(account, body string) (string, map[string]any) {
		history := h.messages(body)
		if account == "a" && strings.Contains(history, "start the long job") && strings.Contains(body, `"name":"Bash"`) {
			switch strings.Count(history, `"tool_result"`) {
			case 0:
				return "tool_use", map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": marker, "run_in_background": true, "description": "Long build"}}
			case 1:
				return "tool_use", map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "sleep 4", "description": "Wait for the build"}}
			}
		}
		return "end_turn", map[string]any{"type": "text", "text": "ok from " + account}
	}
	other := h.start()
	h.send(other, "hello from the other session")
	h.wait(other, "the other session's answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	otherBefore := h.record(other)
	id := h.start()
	if h.record(id).Account.ID != h.list.Accounts[0].ID || otherBefore.Account.ID != h.list.Accounts[0].ID {
		t.Fatal("both sessions must start on A")
	}
	h.send(id, "start the long job")
	h.wait(id, "the foreground step", func(v mobileSession) bool {
		return len(v.Turns) == 1 && len(v.Turns[0].Tools) == 2 && v.Turns[0].Tools[1].Status == "called"
	})
	if len(h.running(marker)) == 0 {
		t.Fatal("the background command is not running")
	}
	h.registryRowsDescendFrom(h.record(id).CLIID)
	// The channel admits a message only between turns; the one sent at the
	// limit is the message the CLI's limit menu leaves unsubmitted.
	h.limited.Store("seven_day")
	limitedAt := time.Now()
	h.wait(id, "the limit", func(v mobileSession) bool { return v.AccountFailure == "rate_limit" })
	observed := time.Since(limitedAt)
	h.send(id, "sent after the limit")
	view := h.wait(id, "the automatic handoff", func(v mobileSession) bool {
		return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.CanSend != nil && *v.CanSend && !v.Active
	})
	handoff := time.Since(limitedAt)
	sw := h.record(id).Switch
	if view.AccountID != h.list.Accounts[1].ID || !sw.Automatic || sw.Reason != "seven_day" || sw.SourceLabel != "A" || sw.ResetsAt != h.resetsAt.Load() {
		t.Fatalf("handoff: %s %+v", view.AccountID, sw)
	}
	h.wait(id, "carried messages answered", func(v mobileSession) bool {
		r := h.record(id)
		for _, message := range r.Switch.Carry {
			if !message.Delivered {
				return false
			}
		}
		return strings.Contains(h.lastBody("b", "sent after the limit"), "ok from b") && !v.Active
	})
	final := h.lastBody("b", "sent after the limit")
	for _, text := range []string{"start the long job", "sent after the limit"} {
		if n := strings.Count(final, text); n != 1 {
			t.Fatalf("%q reached B %d times: %s", text, n, final)
		}
	}
	for _, body := range h.requests("a") {
		if strings.Contains(h.messages(body), "sent after the limit") {
			t.Fatal("a message A never took was sent on A")
		}
	}
	resumed := h.lastBody("b", "stopped for this account change")
	// It says what was stopped, and no more: a detached command may live on.
	if resumed == "" || !strings.Contains(resumed, "with the subagents and background commands it was running as its own processes") || !strings.Contains(resumed, "A command that detached itself may still be running") {
		t.Fatal("the resume prompt does not say what was interrupted")
	}
	if left := h.running(marker); len(left) != 0 {
		t.Fatal("the background command outlived its session:", left)
	}
	otherAfter := h.record(other)
	if otherAfter.Account.ID != otherBefore.Account.ID || otherAfter.CLIID != otherBefore.CLIID || otherAfter.Switch != nil {
		t.Fatal("the other session of A was touched")
	}
	if state := h.state(other); state["closed"] == true {
		t.Fatal("the other session's channel was closed")
	}
	h.send(other, "the other session still works")
	h.wait(other, "the other session after the handoff", func(v mobileSession) bool {
		return len(v.Turns) == 2 && v.Turns[1].Status != "queued" && v.AccountFailure == "rate_limit" || len(v.Turns) == 2 && v.Turns[1].Status == "completed"
	})
	began := time.Unix(sw.At, 0).Sub(limitedAt)
	t.Logf("limit observed after %s; handoff began %s after the limit (the terminal's ten-second check) and was ready %s after it (%d usage reports for A); carried %d messages", observed.Round(time.Millisecond), began.Round(time.Second), handoff.Round(time.Millisecond), h.usage["a"], len(sw.Carry))
}

// A manual switch while a subagent runs: one confirm, then the target
// resumes with the unfinished subagent named.
func TestClaudeManualSwitchDuringWorkEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	h.script = func(account, body string) (string, map[string]any) {
		history := h.messages(body)
		if account == "a" && strings.Contains(history, "review the patch") && strings.Contains(body, `"name":"Agent"`) && !strings.Contains(history, `"tool_result"`) {
			return "tool_use", map[string]any{"type": "tool_use", "name": "Agent", "input": map[string]any{"description": "Review the patch", "prompt": "Look at the patch in this project", "subagent_type": "general-purpose"}}
		}
		if account == "a" && strings.Contains(history, "Look at the patch in this project") && !strings.Contains(history, "review the patch") {
			select { // the subagent is still working when the person switches
			case <-release:
			case <-time.After(60 * time.Second):
			}
		}
		return "end_turn", map[string]any{"type": "text", "text": "ok from " + account}
	}
	id := h.start()
	h.send(id, "review the patch")
	h.wait(id, "the running subagent", func(v mobileSession) bool {
		return len(v.Turns) == 1 && len(v.Turns[0].Tools) == 1 && v.Turns[0].Tools[0].Name == "Agent" && v.Turns[0].Tools[0].Status == "called"
	})
	h.registryRowsDescendFrom(h.record(id).CLIID)
	account := func(body map[string]any) (int, string) {
		raw, _ := json.Marshal(body)
		response := request(h.s, "POST", "/api/mobile/cli/sessions/"+id+"/account", h.s.origin, string(raw), true)
		return response.Code, response.Body.String()
	}
	code, reply := account(map[string]any{"action": "inspect"})
	var inspect struct {
		Busy    bool                 `json:"source_busy"`
		Targets []claudeSwitchTarget `json:"targets"`
		Source  string               `json:"source_id"`
	}
	_ = json.Unmarshal([]byte(reply), &inspect)
	if code != 200 || !inspect.Busy || len(inspect.Targets) != 1 || inspect.Targets[0].Label != "B" {
		t.Fatal(code, reply)
	}
	switchInput := map[string]any{"action": "switch", "target": inspect.Targets[0].ID, "request_id": uuid.NewString(), "source_id": inspect.Source, "confirm": true}
	if code, reply := account(switchInput); code != 409 || !strings.Contains(reply, "停止当前任务") {
		t.Fatal("a working source needs the one confirm:", code, reply)
	}
	if h.record(id).Switch != nil || h.record(id).Account.ID != h.list.Accounts[0].ID {
		t.Fatal("the unconfirmed switch changed the session")
	}
	switchInput["force"], switchInput["request_id"] = true, uuid.NewString()
	started := time.Now()
	if code, reply := account(switchInput); code != 200 {
		t.Fatal(code, reply)
	}
	view := h.wait(id, "the manual switch", func(v mobileSession) bool {
		return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.CanSend != nil && *v.CanSend && !v.Active
	})
	took := time.Since(started)
	once.Do(func() { close(release) })
	if view.AccountID != h.list.Accounts[1].ID {
		t.Fatal("not on B", view.AccountID)
	}
	resumed := h.lastBody("b", "stopped for this account change")
	if !strings.Contains(resumed, "Review the patch (Agent)") || !strings.Contains(resumed, "review the patch") {
		t.Fatal("the resume prompt does not name the subagent:", resumed)
	}
	t.Logf("manual switch during work: confirm asked once, %s from the confirmed request to B ready", took.Round(time.Millisecond))
}

// registryRowsDescendFrom checks the assumption behind stopping a source with
// its children: every live registry row of the session (claude agents
// --json, what the guest reads) is the source CLI or one of its descendants.
func (h *limitHarness) registryRowsDescendFrom(session string) {
	h.t.Helper()
	config := filepath.Join(h.home, ".local/share/cage/claude-accounts", h.list.Accounts[0].ID, "config")
	listing := exec.Command(filepath.Join(h.home, "bin", "claude"), "agents", "--json")
	listing.Dir, listing.Env = h.project, []string{"HOME=" + h.home, "PATH=" + filepath.Join(h.home, "bin") + ":/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + config}
	raw, err := listing.Output()
	var rows []map[string]any
	if err != nil || json.Unmarshal(raw, &rows) != nil {
		h.t.Fatalf("agents --json: %v %s", err, raw)
	}
	table, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	parent := map[string]string{}
	for _, line := range strings.Split(string(table), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			parent[fields[0]] = fields[1]
		}
	}
	var source string
	mine := []map[string]any{}
	for _, row := range rows {
		if row["sessionId"] == session {
			mine = append(mine, row)
			if row["kind"] == "interactive" {
				source = fmt.Sprint(row["pid"])
			}
		}
	}
	if source == "" {
		h.t.Fatalf("no interactive row for the session: %s", raw)
	}
	for _, row := range mine {
		pid, chain := fmt.Sprint(row["pid"]), []string{}
		for at := pid; at != "" && at != "1" && at != source && len(chain) < 12; at = parent[at] {
			chain = append(chain, at)
		}
		if pid != source && (len(chain) == 12 || parent[chain[len(chain)-1]] != source) {
			h.t.Fatalf("registry row %v is not a descendant of the source %s: %v", row, source, chain)
		}
	}
	h.t.Logf("registry rows of the session: %d (kinds %v), all the source CLI or its descendants", len(mine), func() []any {
		kinds := []any{}
		for _, row := range mine {
			kinds = append(kinds, row["kind"])
		}
		return kinds
	}())
}

// With the default rule and no quota reading stored, a real limit brings the
// first-time question: the session's own check reads the usage, nothing moves
// before the answer, the answer from the phone route moves it, the message
// sent at the limit arrives once on B, and the desktop's late answer changes
// nothing.
func TestClaudeLimitQuestionEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.script = func(account, _ string) (string, map[string]any) {
		return "end_turn", map[string]any{"type": "text", "text": "ok from " + account}
	}
	id := h.start()
	h.send(id, "hello before the limit")
	h.wait(id, "first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	h.limited.Store("seven_day")
	limitedAt := time.Now()
	h.send(id, "this one hits the limit")
	h.wait(id, "the limit", func(v mobileSession) bool { return v.AccountFailure == "rate_limit" })
	h.send(id, "sent while the question is open")
	type inspect struct {
		Source  string            `json:"source_id"`
		Handoff claudeHandoffView `json:"handoff"`
	}
	ask := func(route string, body map[string]any) (int, string) {
		raw, _ := json.Marshal(body)
		response := request(h.s, "POST", route+id+"/account", h.s.origin, string(raw), true)
		return response.Code, response.Body.String()
	}
	var view inspect
	deadline := time.Now().Add(40 * time.Second)
	for view.Handoff.Question == nil && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		_, reply := ask("/api/mobile/cli/sessions/", map[string]any{"action": "inspect"})
		_ = json.Unmarshal([]byte(reply), &view)
	}
	asked := time.Since(limitedAt)
	if q := view.Handoff.Question; q == nil || q.Label != "B" || view.Handoff.Window != "seven_day" || view.Handoff.ResetsAt != h.resetsAt.Load() {
		t.Fatalf("no first-time question: %+v", view.Handoff)
	}
	// Nothing moves before the answer, across another check.
	time.Sleep(11 * time.Second)
	if r := h.record(id); r.Account.ID != h.list.Accounts[0].ID || r.Switch != nil || len(h.requests("b")) != 0 {
		t.Fatal("the session moved before an answer")
	}
	code, reply := ask("/api/mobile/cli/sessions/", map[string]any{"action": "answer", "answer": "once", "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true})
	if code != 200 {
		t.Fatal(code, reply)
	}
	if code, reply := ask("/api/cli/sessions/", map[string]any{"action": "answer", "answer": "wait_once", "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true}); code != 409 || !strings.Contains(reply, "另一台设备") {
		t.Fatal("the late answer from the desktop:", code, reply)
	}
	h.wait(id, "the answered handoff", func(v mobileSession) bool {
		return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.AccountID == h.list.Accounts[1].ID && !v.Active
	})
	h.wait(id, "the message sent at the limit answered on B", func(v mobileSession) bool {
		return strings.Contains(h.lastBody("b", "sent while the question is open"), "ok from b") && !v.Active
	})
	final := h.lastBody("b", "sent while the question is open")
	for _, text := range []string{"this one hits the limit", "sent while the question is open"} {
		if n := strings.Count(final, text); n != 1 {
			t.Fatalf("%q reached B %d times", text, n)
		}
	}
	if list, _ := h.s.loadClaudeAccounts(); list.Scheduling.SwitchPolicy != "ask" || list.Scheduling.UngroupedHandoff {
		t.Fatal("a one-time answer changed the rule", list.Scheduling)
	}
	t.Logf("question shown %s after the limit (%d usage reports for A); answered once from the phone route", asked.Round(time.Millisecond), h.usage["a"])
}
