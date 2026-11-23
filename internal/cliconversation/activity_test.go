package cliconversation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func feed(t *testing.T, tools []ToolActivity, role string, lines ...string) []ToolActivity {
	t.Helper()
	for _, line := range lines {
		tools, _ = updateTools(tools, role, []byte(line))
	}
	return tools
}

func TestOrdinaryToolCallsKeepTheirShape(t *testing.T) {
	tools := feed(t, nil, "assistant", `{"message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"private-command","description":"检查结果"}},{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"result.md"}}]}}`)
	running, _ := json.Marshal(tools)
	if string(running) != `[{"id":"a","name":"Bash","detail":"检查结果","status":"called"},{"id":"b","name":"Read","detail":"result.md","status":"called"}]` {
		t.Fatalf("running calls changed shape: %s", running)
	}
	tools = feed(t, tools, "user", `{"message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"private-output"},{"type":"tool_result","tool_use_id":"a","is_error":true}]},"tool_use_result":{"stdout":"private-output"}}`,
		`{"message":{"content":"<task-notification><summary>完成</summary></task-notification>"}}`)
	got, _ := json.Marshal(tools)
	if string(got) != `[{"id":"a","name":"Bash","detail":"检查结果","status":"failed"},{"id":"b","name":"Read","detail":"result.md","status":"completed"}]` {
		t.Fatalf("ordinary calls changed shape: %s", got)
	}
}

func TestToolActivityFollowsSubagentsAndBackgroundTasks(t *testing.T) {
	now := time.Unix(1795000000, 0)
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })
	tools := feed(t, nil, "assistant", `{"message":{"content":[
		{"type":"tool_use","id":"bg-agent","name":"Agent","input":{"description":"梳理目录","prompt":"private-prompt","run_in_background":true}},
		{"type":"tool_use","id":"fg-agent","name":"Agent","input":{"description":"前台检查","prompt":"private-prompt","subagent_type":"Explore"}},
		{"type":"tool_use","id":"bg-shell","name":"Bash","input":{"command":"private-command","description":"构建","run_in_background":true}},
		{"type":"tool_use","id":"repl-shell","name":"Bash","input":{"command":"private-command","timeout":1000}},
		{"type":"tool_use","id":"refused","name":"Bash","input":{"command":"private-command","run_in_background":true}}]}}`)
	now = now.Add(2 * time.Second)
	tools = feed(t, tools, "user",
		`{"message":{"content":[{"type":"tool_result","tool_use_id":"bg-agent","content":""}]},"tool_use_result":{"status":"async_launched","isAsync":true,"agentId":"a0123456789abcdef"}}`,
		`{"message":{"content":[{"type":"tool_result","tool_use_id":"bg-shell","content":"Command running in background with ID: bk1"}]}}`,
		`{"message":{"content":[{"type":"tool_result","tool_use_id":"repl-shell","content":""}]},"toolUseResult":{"backgroundTaskId":"bz9"}}`,
		`{"message":{"content":[{"type":"tool_result","tool_use_id":"refused","is_error":true,"content":"not available"}]}}`)
	state := func(id string) ToolActivity {
		for _, tool := range tools {
			if tool.ID == id {
				return tool
			}
		}
		t.Fatalf("no tool %s", id)
		return ToolActivity{}
	}
	if got := state("bg-agent"); got.Kind != "background_agent" || got.State != "running" || got.StartedAt != 1795000000 || got.Status != "completed" {
		t.Fatalf("background agent: %+v", got)
	}
	if got := state("fg-agent"); got.Kind != "subagent" || got.AgentType != "Explore" || got.State != "running" || got.Status != "called" {
		t.Fatalf("foreground agent: %+v", got)
	}
	if got := state("repl-shell"); got.Kind != "background_command" || got.State != "running" || got.StartedAt != 1795000000 {
		t.Fatalf("a call that moved to the background: %+v", got)
	}
	if got := state("refused"); got.State != "failed" || got.EndedAt != 1795000002 {
		t.Fatalf("refused launch: %+v", got)
	}
	tools, _ = updateChild(tools, "fg-agent", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"src/app.py"}}]}}`))
	tools, _ = updateChild(tools, "fg-agent", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c2","name":"Poll","input":{}}]}}`))
	if got := state("fg-agent"); got.Calls != 2 || got.Now == nil || *got.Now != (ToolNow{Name: "Read", Detail: "src/app.py"}) {
		t.Fatalf("subagent progress: %+v", got)
	}
	now = now.Add(10 * time.Second)
	tools = feed(t, tools, "user",
		`{"message":{"content":[{"type":"tool_result","tool_use_id":"fg-agent","content":"private-report"}]}}`,
		`{"message":{"content":"<task-notification>\n<task-id>bk1</task-id>\n<tool-use-id>bg-shell</tool-use-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"}}`,
		`{"message":{"content":"<task-notification>\n<task-id>bz9</task-id>\n<status>failed</status>\n</task-notification>"}}`)
	tools = feed(t, tools, "assistant", `{"message":{"content":[{"type":"tool_use","id":"stop","name":"TaskStop","input":{"task_id":"a0123456789abcdef"}}]}}`)
	tools = feed(t, tools, "user", `{"message":{"content":[{"type":"tool_result","tool_use_id":"stop","content":""}]},"tool_use_result":{"task_id":"a0123456789abcdef","task_type":"local_agent"}}`)
	for id, want := range map[string]string{"fg-agent": "completed", "bg-shell": "completed", "repl-shell": "failed", "bg-agent": "stopped"} {
		if got := state(id); got.State != want || got.EndedAt != 1795000012 {
			t.Fatalf("%s ended as %+v, want %s", id, got, want)
		}
	}
	if got := state("stop"); got.Kind != "" || got.State != "" {
		t.Fatalf("a stop call is an ordinary call: %+v", got)
	}
	encoded, _ := json.Marshal(tools)
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("tool activity leaked content: %s", encoded)
	}
}

func TestCloseTasksStopsWhatOutlivedTheRun(t *testing.T) {
	tools := CloseTasks([]ToolActivity{{ID: "a", Kind: "subagent", State: "running", Now: &ToolNow{Name: "Read"}}, {ID: "b", Kind: "background_command", State: "completed"}, {ID: "c", Status: "called"}})
	if tools[0].State != "stopped" || tools[0].Now != nil || tools[1].State != "completed" || tools[2].State != "" {
		t.Fatalf("closed tasks: %+v", tools)
	}
}

func TestReadResultCountsSubagentCalls(t *testing.T) {
	id := testSessionID
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"` + id + `"}`,
		`{"type":"assistant","session_id":"` + id + `","message":{"content":[{"type":"tool_use","id":"agent","name":"Agent","input":{"description":"梳理目录","prompt":"p"}}]}}`,
		`{"type":"assistant","session_id":"` + id + `","parent_tool_use_id":"agent","message":{"content":[{"type":"tool_use","id":"c1","name":"Grep","input":{"pattern":"TODO"}}]}}`,
		`{"type":"user","session_id":"` + id + `","parent_tool_use_id":"agent","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"private-output"}]}}`,
		`{"type":"result","subtype":"success","session_id":"` + id + `","result":"done"}`,
	}, "\n")
	var last []ToolActivity
	if _, err := readResult(strings.NewReader(stream), id, nil, nil, func(tools []ToolActivity) { last = tools }); err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].Calls != 1 || last[0].Now == nil || last[0].Now.Name != "Grep" || last[0].State != "running" {
		t.Fatalf("subagent calls: %+v", last)
	}
}
