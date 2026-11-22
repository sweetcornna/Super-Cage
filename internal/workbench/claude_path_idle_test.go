package workbench

import (
	"encoding/json"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Guarantee G (design 9.1): a Claude-only environment's /status and host work do
// not change because other clients can be added.

// statusKeysOfAClaudeOnlyEnvironment are the top-level fields /status answers
// today for an environment with no optional client (a Linux guest of the
// default system). clients_unknown may join only before the first reading of an
// environment (design 2.3); clients and every other new field may not.
var statusKeysOfAClaudeOnlyEnvironment = []string{
	"auto_timezone", "can_restore", "environment_directory", "environment_identity_directory", "environment_update_error",
	"environment_update_required", "job", "keyboard", "local_vm_storage", "location", "network_error", "project_error",
	"project_errors", "project_groups", "projects", "ready", "safety", "storage_recovery", "terminals_revision", "vm_running", "workspace",
}

func TestStatusOfAClaudeOnlyEnvironmentHasOnlyTodaysFields(t *testing.T) {
	s := testServer(t)
	response := httptest.NewRecorder()
	s.status(response, httptest.NewRequest("GET", "/api/status", nil))
	var view map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"clients_unknown": true}
	for _, key := range statusKeysOfAClaudeOnlyEnvironment {
		allowed[key] = true
	}
	var got []string
	for key := range view {
		got = append(got, key)
		if !allowed[key] {
			t.Errorf("/status of a Claude-only environment gained %q: nothing may be added for the optional clients (design 9.1)", key)
		}
	}
	sort.Strings(got)
	for _, key := range statusKeysOfAClaudeOnlyEnvironment {
		if _, ok := view[key]; !ok {
			t.Errorf("/status lost %q (pinned list: %s; got: %s)", key, strings.Join(statusKeysOfAClaudeOnlyEnvironment, ","), strings.Join(got, ","))
		}
	}
	if raw, ok := view["clients_unknown"]; ok && string(raw) != "true" {
		t.Errorf("clients_unknown is only ever true, got %s", raw)
	}
	for _, key := range []string{"clients", "agents", "agent_clients"} {
		if _, ok := view[key]; ok {
			t.Errorf("/status carries %q with nothing optional present", key)
		}
	}
}

// No goroutine of the feature exists while only the Claude path has run: the
// stacks of all goroutines mention nothing of internal/agents, and reading the
// status leaves the goroutine count as it was. The full path (start, attach,
// a Claude terminal) needs a fake guest; this is the part that needs none.
func TestClaudePathHasNoOtherClientsGoroutine(t *testing.T) {
	s := testServer(t)
	settled := func() int {
		runtime.GC()
		return runtime.NumGoroutine()
	}
	before := settled()
	for i := 0; i < 5; i++ {
		response := httptest.NewRecorder()
		s.status(response, httptest.NewRequest("GET", "/api/status", nil))
		if response.Code != 200 {
			t.Fatalf("status %d: %s", response.Code, response.Body.String())
		}
	}
	if after := settled(); after > before {
		t.Errorf("reading the status left %d goroutines behind", after-before)
	}
	buffer := make([]byte, 1<<20)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	for _, marker := range []string{"internal/agents.", "workbench.(*Server).agent", "workbench.agentJob", "workbench.runAgent"} {
		if strings.Contains(stacks, marker) {
			t.Errorf("a goroutine of the optional clients exists on the Claude path: found %q", marker)
		}
	}
}
