package workbench

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cliconversation"
)

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
