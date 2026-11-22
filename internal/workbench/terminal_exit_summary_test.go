package workbench

import (
	"encoding/json"
	"io"
	"testing"
)

// idlePTY is a terminal that never produces output; only its summary is read.
type idlePTY struct{}

func (idlePTY) Read([]byte) (int, error)       { return 0, io.EOF }
func (idlePTY) Write(p []byte) (int, error)    { return len(p), nil }
func (idlePTY) Close() error                   { return nil }
func (idlePTY) Resize(cols, rows uint16) error { return nil }
func (idlePTY) Wait() error                    { return nil }
func (idlePTY) Kill() error                    { return nil }

// The terminal list the workbench reads says how a session ended, so a Codex
// launch the guest refused (75) does not look like an ordinary close.
func TestTerminalSummaryCarriesTheExitCodeOfAnEndedSession(t *testing.T) {
	session := newTerminalSession(idlePTY{})
	session.id, session.kind = "t1", "codex"
	if _, ok := session.summary()["exit_code"]; ok {
		t.Fatalf("a running session has an exit code: %+v", session.summary())
	}
	session.mu.Lock()
	session.exitCode, session.ended = 75, true
	session.mu.Unlock()
	close(session.done)
	summary := session.summary()
	if summary["closed"] != true || summary["exit_code"] != 75 {
		t.Fatalf("ended session: %+v", summary)
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Closed   bool `json:"closed"`
		ExitCode *int `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || !wire.Closed || wire.ExitCode == nil || *wire.ExitCode != 75 {
		t.Fatalf("wire form %s: %v", data, err)
	}
	// A signal or an unknown status says nothing.
	killed := newTerminalSession(idlePTY{})
	close(killed.done)
	if _, ok := killed.summary()["exit_code"]; ok {
		t.Fatalf("an unknown status was reported: %+v", killed.summary())
	}
}
