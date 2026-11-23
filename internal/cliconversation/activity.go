package cliconversation

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ToolActivity is a display-only summary. Command bodies, file contents and
// tool outputs stay in the CLI transcript. Approval has a separate protocol.
type ToolActivity struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Status string `json:"status"`
	// Subagents and background tasks only, as in the resident helper's entries
	// (cli_discovery.py view_tasks): what the call started, whether it still runs, when, and
	// for a running subagent its latest tool call and how many it made.
	Kind         string   `json:"kind,omitempty"`  // "subagent", "background_agent" or "background_command"
	State        string   `json:"state,omitempty"` // "running", "completed", "failed" or "stopped"
	AgentType    string   `json:"agent_type,omitempty"`
	StartedAt    float64  `json:"started_at,omitempty"` // seconds since the epoch
	EndedAt      float64  `json:"ended_at,omitempty"`
	Now          *ToolNow `json:"now,omitempty"`
	Calls        int      `json:"calls,omitempty"`
	CallsPartial bool     `json:"calls_partial,omitempty"`
	// task is the background task or agent id a launch was accepted with, or
	// the task a stop call names; launched is when a plain Bash call started,
	// should it move to the background. Both live for one run only.
	task     string
	launched float64
}

// ToolNow is a running subagent's latest tool call, by the same display rule.
type ToolNow struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

var (
	taskStates  = map[string]string{"completed": "completed", "failed": "failed", "killed": "stopped", "stopped": "stopped", "cancelled": "stopped"}
	taskID      = regexp.MustCompile("^[A-Za-z0-9_.:-]{1,128}$")
	launchedID  = regexp.MustCompile("running in background with ID: ([A-Za-z0-9_.:-]{1,128})")
	noticeField = func(text, name string) string {
		match := regexp.MustCompile("<" + name + `>\s*([^<]{1,256}?)\s*</` + name + ">").FindStringSubmatch(text)
		if match == nil {
			return ""
		}
		return match[1]
	}
	// clock is the time of launches and ends seen in a stream, which has no timestamps.
	clock = time.Now
)

type toolBlock struct {
	Type      string                     `json:"type"`
	ID        string                     `json:"id"`
	Name      string                     `json:"name"`
	Input     map[string]json.RawMessage `json:"input"`
	ToolUseID string                     `json:"tool_use_id"`
	IsError   bool                       `json:"is_error"`
	Text      string                     `json:"text"`
	Content   json.RawMessage            `json:"content"`
}

// toolOutcome is what the CLI reports about an accepted launch or a stop.
type toolOutcome struct {
	BackgroundTaskID string `json:"backgroundTaskId"`
	AgentID          string `json:"agentId"`
	Status           string `json:"status"`
	IsAsync          bool   `json:"isAsync"`
	TaskID           string `json:"task_id"`
}

func toolDetail(input map[string]json.RawMessage) string {
	for _, key := range []string{"description", "subject", "file_path", "pattern", "query", "skill", "subagent_type"} {
		var value string
		if json.Unmarshal(input[key], &value) == nil && strings.TrimSpace(value) != "" {
			runes := []rune(strings.TrimSpace(value))
			return string(runes[:min(len(runes), 160)])
		}
	}
	return ""
}

func stamp() float64 { return float64(clock().UnixMilli()) / 1000 }

func messageBlocks(line []byte) ([]toolBlock, string, toolOutcome, bool) {
	var msg struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Snake json.RawMessage `json:"tool_use_result"`
		Camel json.RawMessage `json:"toolUseResult"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return nil, "", toolOutcome{}, false
	}
	var outcome toolOutcome
	if json.Unmarshal(msg.Camel, &outcome) != nil {
		_ = json.Unmarshal(msg.Snake, &outcome)
	}
	var blocks []toolBlock
	if json.Unmarshal(msg.Message.Content, &blocks) == nil {
		var text []string
		for _, b := range blocks {
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		return blocks, strings.Join(text, "\n"), outcome, true
	}
	var text string
	if json.Unmarshal(msg.Message.Content, &text) == nil {
		return nil, text, outcome, true
	}
	return nil, "", toolOutcome{}, false
}

func updateTools(tools []ToolActivity, role string, line []byte) ([]ToolActivity, bool) {
	blocks, text, outcome, ok := messageBlocks(line)
	if !ok {
		return tools, false
	}
	changed := false
	if role == "user" && strings.HasPrefix(strings.TrimSpace(text), "<task-notification>") {
		tool, task, state := noticeField(text, "tool-use-id"), noticeField(text, "task-id"), taskStates[noticeField(text, "status")]
		for i := range tools {
			if state != "" && tools[i].Kind != "" && (tools[i].ID == tool || task != "" && tools[i].task == task) {
				// An agent notifies each time it stops; the latest notice holds.
				tools[i].State, tools[i].EndedAt = state, stamp()
				changed = true
				break
			}
		}
	}
	results := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			results++
		}
	}
	if results != 1 {
		outcome = toolOutcome{}
	}
	for _, b := range blocks {
		if role == "user" && b.Type == "tool_result" {
			for i := range tools {
				if tools[i].ID == b.ToolUseID && tools[i].Status == "called" {
					tools[i].Status = "completed"
					if b.IsError {
						tools[i].Status = "failed"
					}
					settleLaunch(tools, i, b, outcome)
					changed = true
					break
				}
			}
		}
		if role != "assistant" || (b.Type != "tool_use" && b.Type != "server_tool_use") || !toolNamePattern.MatchString(b.Name) || b.ID == "" || len(b.ID) > 128 || len(tools) >= 64 {
			continue
		}
		duplicate := false
		for _, item := range tools {
			if item.ID == b.ID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		tools = append(tools, launchedTool(b))
		changed = true
	}
	return tools, changed
}

func launchedTool(b toolBlock) ToolActivity {
	tool := ToolActivity{ID: b.ID, Name: b.Name, Detail: toolDetail(b.Input), Status: "called"}
	var background bool
	_ = json.Unmarshal(b.Input["run_in_background"], &background)
	switch {
	case b.Name == "Agent" || b.Name == "Task":
		tool.Kind, tool.State, tool.StartedAt = "subagent", "running", stamp()
		if background {
			tool.Kind = "background_agent"
		}
		var kind string
		if json.Unmarshal(b.Input["subagent_type"], &kind) == nil && strings.TrimSpace(kind) != "" {
			runes := []rune(strings.TrimSpace(kind))
			tool.AgentType = string(runes[:min(len(runes), 64)])
		}
	case b.Name == "Bash" && background:
		tool.Kind, tool.State, tool.StartedAt = "background_command", "running", stamp()
	case b.Name == "Bash":
		tool.launched = stamp()
	case b.Name == "TaskStop" || b.Name == "KillShell" || b.Name == "KillBash":
		for _, key := range []string{"task_id", "shell_id", "bash_id"} {
			if json.Unmarshal(b.Input[key], &tool.task) == nil && tool.task != "" {
				break
			}
		}
	}
	return tool
}

// settleLaunch applies the first result of tools[i] to a subagent or
// background task, or to the task a stop call ended.
func settleLaunch(tools []ToolActivity, i int, b toolBlock, outcome toolOutcome) {
	tool := &tools[i]
	switch {
	case tool.Name == "TaskStop" || tool.Name == "KillShell" || tool.Name == "KillBash":
		target := tool.task
		if outcome.TaskID != "" {
			target = outcome.TaskID
		}
		for j := range tools {
			if !b.IsError && target != "" && tools[j].task == target && tools[j].State == "running" {
				tools[j].State, tools[j].EndedAt = "stopped", stamp()
			}
		}
		return
	case tool.Kind == "" && tool.Name == "Bash":
		if !b.IsError && taskID.MatchString(outcome.BackgroundTaskID) {
			tool.Kind, tool.State, tool.task, tool.StartedAt = "background_command", "running", outcome.BackgroundTaskID, tool.launched
		}
		return
	case tool.Kind == "background_command":
		if b.IsError {
			tool.State, tool.EndedAt = "failed", stamp()
			return
		}
		tool.task = outcome.BackgroundTaskID
		if tool.task == "" {
			var text string
			_ = json.Unmarshal(b.Content, &text)
			if match := launchedID.FindStringSubmatch(text); match != nil {
				tool.task = match[1]
			}
		}
		if !taskID.MatchString(tool.task) {
			tool.task = ""
		}
	case tool.Kind != "":
		if taskID.MatchString(outcome.AgentID) {
			tool.task = outcome.AgentID
		}
		if (outcome.Status == "async_launched" || outcome.IsAsync) && !b.IsError {
			return
		}
		tool.State, tool.EndedAt = "completed", stamp()
		if b.IsError {
			tool.State = "failed"
		}
	}
}

// updateChild counts a running subagent's own tool calls, which the CLI
// reports with the launching call's id as parent_tool_use_id.
func updateChild(tools []ToolActivity, parent string, line []byte) ([]ToolActivity, bool) {
	i := slices.IndexFunc(tools, func(tool ToolActivity) bool {
		return tool.ID == parent && (tool.Kind == "subagent" || tool.Kind == "background_agent") && tool.State == "running"
	})
	blocks, _, _, ok := messageBlocks(line)
	if i < 0 || !ok {
		return tools, false
	}
	changed := false
	for _, b := range blocks {
		if b.Type != "tool_use" || !toolNamePattern.MatchString(b.Name) {
			continue
		}
		tools[i].Calls++
		// A REPL's wait shows the call it waits for.
		if b.Name != "Poll" || tools[i].Now == nil {
			tools[i].Now = &ToolNow{Name: b.Name, Detail: toolDetail(b.Input)}
		}
		changed = true
	}
	return tools, changed
}

// CloseTasks reports the tasks of an ended CLI process that had no end of
// their own as stopped: they do not outlive the process that started them.
func CloseTasks(tools []ToolActivity) []ToolActivity {
	for i := range tools {
		if tools[i].State == "running" {
			tools[i].State, tools[i].Now = "stopped", nil
		}
	}
	return tools
}
