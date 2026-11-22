package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cliconversation"
)

// The channel helper's task fields reach the session list and the phone's view
// unchanged; an older helper without them leaves both as before.
func TestTerminalTasksReachListAndView(t *testing.T) {
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false); err != nil {
		t.Fatal(err)
	}
	observation := filepath.Join(home, "observation.json")
	helper := filepath.Join(home, ".local/share/cage/cli-channel-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(cliChannelHelper+cliResumePrompt+claudeAccountResumePrompt))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf("import json,sys\njson.load(sys.stdin)\nprint(open(%q).read())\n", observation)
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	config := filepath.Join(home, ".claude")
	for _, phase := range []struct {
		running bool
		tasks   int
		want    int
	}{{true, 2, 2}, {false, 2, 0}, {true, 0, 0}} {
		if err := writePrivate(observation, []terminalConversation{{CLIID: record.CLIID, Workspace: guestPath(project), Config: config, Running: phase.running, RunningTasks: phase.tasks}}); err != nil {
			t.Fatal(err)
		}
		if err := s.discoverTerminalConversations(context.Background(), []string{project}); err != nil {
			t.Fatal(err)
		}
		list, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false)
		if err != nil || len(list) != 1 || list[0].RunningTasks != phase.want {
			t.Fatalf("phase %+v: summaries = %+v, error = %v", phase, list, err)
		}
		encoded, _ := json.Marshal(list[0])
		if strings.Contains(string(encoded), "running_tasks") != (phase.want > 0) {
			t.Fatalf("phase %+v: list row %s", phase, encoded)
		}
	}
	agent := cliconversation.ToolActivity{ID: "toolu_agent", Name: "Agent", Detail: "梳理目录", Status: "completed", Kind: "subagent", State: "running",
		AgentType: "Explore", StartedAt: 1795000000.25, Now: &cliconversation.ToolNow{Name: "Read", Detail: "src/app.py"}, Calls: 3}
	view := func(tools []cliconversation.ToolActivity, tasks *mobileTasks) mobileSession {
		t.Helper()
		current := terminalConversation{CLIID: record.CLIID, Workspace: guestPath(project), Config: config, HasHistory: true, Running: true, CanSend: true,
			Turns: []mobileTurn{{ID: uuid.NewString(), Prompt: "看一下目录", Status: "completed", Text: "已开始", Tools: tools}}, Tasks: tasks}
		current.Turns[0].ID = "c0ffee00-0000-4000-8000-000000000001"
		if err := writePrivate(observation, current); err != nil {
			t.Fatal(err)
		}
		got, err := s.terminalConversationView(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	running := &mobileTasks{Count: 1, Running: []cliconversation.ToolActivity{agent}}
	first := view([]cliconversation.ToolActivity{agent}, running)
	if first.Tasks == nil || first.Tasks.Count != 1 || first.Tasks.Running[0].Now.Detail != "src/app.py" || first.Turns[0].Tools[0].Kind != "subagent" {
		t.Fatalf("view lost the tasks: %+v", first)
	}
	if again := view([]cliconversation.ToolActivity{agent}, running); again.Revision != first.Revision {
		t.Fatalf("an unchanged view moved the revision: %d -> %d", first.Revision, again.Revision)
	}
	agent.Calls, agent.Now = 4, &cliconversation.ToolNow{Name: "Edit", Detail: "src/app.py"}
	progressed := view([]cliconversation.ToolActivity{agent}, &mobileTasks{Count: 1, Running: []cliconversation.ToolActivity{agent}})
	if progressed.Revision <= first.Revision {
		t.Fatal("a running subagent's progress did not move the revision")
	}
	agent.State, agent.Now, agent.EndedAt = "completed", nil, 1795000060.5
	ended := view([]cliconversation.ToolActivity{agent}, nil)
	if ended.Tasks != nil || ended.Revision <= progressed.Revision {
		t.Fatalf("ended tasks still listed: %+v", ended.Tasks)
	}
	encoded, _ := json.Marshal(ended)
	if strings.Contains(string(encoded), "\"tasks\"") || !strings.Contains(string(encoded), "\"ended_at\":1795000060.5") {
		t.Fatalf("ended view: %s", encoded)
	}
	// An older helper's view: no task fields anywhere.
	old := view([]cliconversation.ToolActivity{{ID: "a", Name: "Bash", Detail: "检查", Status: "completed"}}, nil)
	encoded, _ = json.Marshal(old.Turns)
	if want := `"tools":[{"id":"a","name":"Bash","detail":"检查","status":"completed"}]`; !strings.Contains(string(encoded), want) {
		t.Fatalf("old tools changed shape: %s", encoded)
	}
}

func TestRestartStopsTasksOfAnInterruptedRun(t *testing.T) {
	s := testServer(t)
	project, _ := filepath.EvalSymlinks(t.TempDir())
	id := uuid.NewString()
	if _, err := s.mobileCLI.create(s.profile, project, id); err != nil {
		t.Fatal(err)
	}
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	record.Turns = append(record.Turns, mobileTurn{ID: uuid.NewString(), Prompt: "x", Status: "running", Tools: []cliconversation.ToolActivity{
		{ID: "a", Name: "Agent", Status: "called", Kind: "subagent", State: "running", Now: &cliconversation.ToolNow{Name: "Read"}},
		{ID: "b", Name: "Read", Status: "completed"}}})
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	s.mobileCLI = mobileCLIStore{}
	record, err = s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	tools := record.Turns[0].Tools
	if record.Turns[0].Status != "interrupted" || tools[0].State != "stopped" || tools[0].Now != nil || tools[1].State != "" {
		t.Fatalf("interrupted run's tasks: %+v", tools)
	}
}
