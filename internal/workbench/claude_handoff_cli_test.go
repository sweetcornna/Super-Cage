//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Exercise the published CLI with two synthetic subscription identities. The
// sandbox denies personal credentials and Internet access. This proves local
// resume/tool semantics, not acceptance by Anthropic of real account switching.
func TestClaudeCrossAccountTranscriptResume(t *testing.T) {
	binary := os.Getenv("CAGE_TEST_CLAUDE_CLI")
	if !filepath.IsAbs(binary) || runtime.GOOS != "darwin" {
		t.Skip("requires an absolute CAGE_TEST_CLAUDE_CLI and macOS sandbox-exec")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	home, project := t.TempDir(), t.TempDir()
	realHome, _ := os.UserHomeDir()
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	policy := "(version 1)(allow default)(deny mach-lookup)(deny file-read* (subpath " + quote(realHome) + ") (subpath \"/Library/Keychains\"))(allow file-read* (literal " + quote(binary) + "))(deny network-outbound)(allow network-outbound (remote ip \"localhost:*\"))"
	var resumed atomic.Bool
	model := mobileTestModel(t, &resumed)
	defer model.Close()
	var aRequests, bRequests atomic.Int32
	var target atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			if target.Load() == 0 {
				aRequests.Add(1)
			} else {
				bRequests.Add(1)
			}
		}
		recorder := httptest.NewRecorder()
		model.Config.Handler.ServeHTTP(recorder, r)
		for k, values := range recorder.Header() {
			w.Header()[k] = values
		}
		w.WriteHeader(recorder.Code)
		body := strings.ReplaceAll(recorder.Body.String(), `printf approved \u003e mobile-approved.txt`, `printf performed \u003e\u003e handoff-tool-count.txt`)
		_, _ = w.Write([]byte(body))
	}))
	defer fixture.Close()
	proxy, ca := mobileOfficialTestProxy(t, fixture)
	configs := []string{filepath.Join(home, "account-a"), filepath.Join(home, "account-b")}
	save := func(path string, value any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(value)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, config := range configs {
		save(filepath.Join(config, ".claude.json"), map[string]any{"hasCompletedOnboarding": true, "oauthAccount": map[string]string{"accountUuid": uuid.NewString(), "emailAddress": fmt.Sprintf("account-%d@example.invalid", i), "organizationUuid": uuid.NewString()}})
		save(filepath.Join(config, ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{"accessToken": fmt.Sprintf("sk-ant-oat01-synthetic-%d", i), "refreshToken": "synthetic", "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "pro"}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	runIn := func(t *testing.T, dir string, account int, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-p", policy, binary}, args...)...)
		cmd.Dir = dir
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + configs[account], "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "NO_PROXY=localhost,127.0.0.1", "NODE_EXTRA_CA_CERTS=" + ca, "SSL_CERT_FILE=" + ca, "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated account %d: %v\n%s", account, err, out)
		}
		return out
	}
	run := func(account int, args ...string) []byte { return runIn(t, project, account, args...) }
	for i := range configs {
		out := run(i, "auth", "status")
		if !strings.Contains(string(out), fmt.Sprintf("account-%d@example.invalid", i)) || !strings.Contains(string(out), "claude.ai") {
			t.Fatal("synthetic subscription identity was not selected", string(out))
		}
	}
	flags := []string{"--model", "sonnet", "-p", "--output-format", "stream-json", "--verbose", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--dangerously-skip-permissions", "--max-turns", "4"}
	sourceID, targetID := uuid.NewString(), uuid.NewString()
	firstOutput := run(0, append(append([]string{}, flags...), "--session-id", sourceID, "initial mobile prompt")...)
	marker := filepath.Join(project, "handoff-tool-count.txt")
	before, err := os.ReadFile(marker)
	if err != nil || len(before) == 0 {
		t.Fatalf("the real CLI did not execute the tool: %v\n%s", err, firstOutput)
	}
	paths, _ := filepath.Glob(filepath.Join(configs[0], "projects", "*", sourceID+".jsonl"))
	if len(paths) != 1 {
		t.Fatal("source transcript missing", paths)
	}
	sourceBefore, _ := os.ReadFile(paths[0])
	if !strings.Contains(string(sourceBefore), "tool_result") {
		t.Fatal("completed tool result absent")
	}
	target.Store(1)
	out := run(1, append(append([]string{}, flags...), "--resume", paths[0], "--fork-session", "--session-id", targetID, "follow-up")...)
	after, _ := os.ReadFile(marker)
	sourceAfter, _ := os.ReadFile(paths[0])
	if string(before) != string(after) {
		t.Fatal("completed tool ran again")
	}
	if string(sourceBefore) != string(sourceAfter) {
		t.Fatal("source account history changed")
	}
	if !resumed.Load() || aRequests.Load() < 2 || bRequests.Load() != 1 {
		t.Fatalf("history was not reused: resumed=%t requests=%d/%d", resumed.Load(), aRequests.Load(), bRequests.Load())
	}
	if !strings.Contains(string(out), targetID) {
		t.Fatal("target CLI session identity missing")
	}
	paths, _ = filepath.Glob(filepath.Join(configs[1], "projects", "*", targetID+".jsonl"))
	if len(paths) != 1 {
		t.Fatal("resumed history not persisted in target account")
	}
	t.Logf("official CLI: two isolated subscription identities; source requests=%d target requests=%d; completed tool count unchanged; source transcript unchanged; target history persisted", aRequests.Load(), bRequests.Load())
	t.Run("original account by ID from another checkout", func(t *testing.T) {
		resumed.Store(false)
		out := runIn(t, t.TempDir(), 0, append(append([]string{}, flags...), "--resume", sourceID, "follow-up")...)
		if !resumed.Load() || !strings.Contains(string(out), sourceID) {
			t.Fatalf("original history did not resume by ID: %s", out)
		}
	})
}

// A turn that ended on a quota limit can leave a tool call without a
// result. The official CLI must resume such a history under another
// account without running the call again on its own.
func TestClaudeCrossAccountResumeOfUnfinishedTool(t *testing.T) {
	binary := os.Getenv("CAGE_TEST_CLAUDE_CLI")
	if !filepath.IsAbs(binary) || runtime.GOOS != "darwin" {
		t.Skip("requires an absolute CAGE_TEST_CLAUDE_CLI and macOS sandbox-exec")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	home, project := t.TempDir(), t.TempDir()
	realHome, _ := os.UserHomeDir()
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	policy := "(version 1)(allow default)(deny mach-lookup)(deny file-read* (subpath " + quote(realHome) + ") (subpath \"/Library/Keychains\"))(allow file-read* (literal " + quote(binary) + "))(deny network-outbound)(allow network-outbound (remote ip \"localhost:*\"))"
	var resumed atomic.Bool
	model := mobileTestModel(t, &resumed)
	defer model.Close()
	var seen atomic.Value
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if strings.Contains(string(body), "follow-up") {
				seen.Store(string(body))
			}
		}
		model.Config.Handler.ServeHTTP(w, r)
	}))
	defer fixture.Close()
	proxy, ca := mobileOfficialTestProxy(t, fixture)
	configs := []string{filepath.Join(home, "account-a"), filepath.Join(home, "account-b")}
	for i, config := range configs {
		for name, value := range map[string]any{
			".claude.json":      map[string]any{"hasCompletedOnboarding": true, "oauthAccount": map[string]string{"accountUuid": uuid.NewString(), "emailAddress": fmt.Sprintf("account-%d@example.invalid", i), "organizationUuid": uuid.NewString()}},
			".credentials.json": map[string]any{"claudeAiOauth": map[string]any{"accessToken": fmt.Sprintf("sk-ant-oat01-synthetic-%d", i), "refreshToken": "synthetic", "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "pro"}},
		} {
			if err := writePrivate(filepath.Join(config, name), value); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	run := func(account int, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-p", policy, binary}, args...)...)
		cmd.Dir = project
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + configs[account], "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "NO_PROXY=localhost,127.0.0.1", "NODE_EXTRA_CA_CERTS=" + ca, "SSL_CERT_FILE=" + ca, "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
		return cmd.CombinedOutput()
	}
	flags := []string{"--model", "sonnet", "-p", "--output-format", "stream-json", "--verbose", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--dangerously-skip-permissions", "--max-turns", "4"}
	sourceID := uuid.NewString()
	if out, err := run(0, append(append([]string{}, flags...), "--session-id", sourceID, "initial mobile prompt")...); err != nil {
		t.Fatalf("source run: %v\n%s", err, out)
	}
	marker := filepath.Join(project, "mobile-approved.txt")
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("the source did not run its tool", err)
	}
	paths, _ := filepath.Glob(filepath.Join(configs[0], "projects", "*", sourceID+".jsonl"))
	if len(paths) != 1 {
		t.Fatal("source transcript missing", paths)
	}
	// Cut the history right after the tool call, as a turn that died on the
	// limit leaves it: a tool_use and no tool_result.
	data, _ := os.ReadFile(paths[0])
	var kept []string
	toolID := ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		kept = append(kept, line)
		var row struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct{ Type, ID string } `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal([]byte(line), &row)
		for _, block := range row.Message.Content {
			if row.Type == "assistant" && block.Type == "tool_use" {
				toolID = block.ID
			}
		}
		if toolID != "" {
			break
		}
	}
	if toolID == "" {
		t.Fatal("no tool call in the source history")
	}
	frozen := filepath.Join(home, "frozen", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(frozen), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(frozen, []byte(strings.Join(kept, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.NewString()
	out, err := run(1, append(append([]string{}, flags...), "--resume", frozen, "--fork-session", "--session-id", targetID, "follow-up")...)
	if err != nil {
		t.Fatalf("the official CLI refused a history with an unfinished tool: %v\n%s", err, out)
	}
	after, _ := os.ReadFile(marker)
	if string(before) != string(after) {
		t.Fatal("the unfinished tool ran again without the model deciding to")
	}
	body, _ := seen.Load().(string)
	if !strings.Contains(body, toolID) {
		t.Fatal("the target did not receive the unfinished call in its history")
	}
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &request)
	shown := ""
	for _, message := range request.Messages {
		if strings.Contains(string(message.Content), `"tool_use_id":"`+toolID+`"`) {
			shown = string(message.Content)
		}
	}
	if len(shown) > 400 {
		shown = shown[:400]
	}
	t.Logf("official CLI resumed a history whose last tool call had no result; the model saw for that call: %s", shown)
}
