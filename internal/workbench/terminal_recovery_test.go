//go:build !windows

package workbench

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlwire"
)

func TestWorkbenchRecoveryRequiresNativeHistoryAndKeepsTerminalID(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	row := listedCodexSession(t, s, guest, project, "重启前的 Codex")
	const terminalID = "a9af2d5fc3508ea1"
	m := &s.mobileCLI
	m.mu.Lock()
	record, err := m.record(s.profile, row.ID)
	if err == nil {
		record.TerminalID, record.TerminalRunning = terminalID, true
		err = writePrivate(mobileRecordPath(s.profile, row.ID), record)
	}
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	view := map[string]any{
		"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "turns": []any{}, "can_send": true,
	}
	guest.reply("view", view)
	if err := s.restoreWorkbenchSessions(context.Background()); err != nil || s.terminals.get(terminalID) != nil {
		t.Fatalf("opened a placeholder without verified history: %v", err)
	}
	view["history_found"] = true
	guest.reply("view", view)
	if err := s.restoreWorkbenchSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	terminal := s.terminals.get(terminalID)
	if terminal == nil || terminal.summary()["cli_session_id"] != row.ID || !terminal.outputAfter(9, 1024).Reset {
		t.Fatal("recovery did not publish the original terminal ID with a new output stream")
	}
	launch := waitCodexCalls(t, guest, "tui-launch", 1)[0]
	if launch["thread"] != record.CLIID || launch["cwd"] != guestPath(project) || launch["account"] != "default" {
		t.Fatal("recovery changed the conversation binding", launch)
	}
	for _, key := range []string{"text", "prompt", "input", "command"} {
		if _, exists := launch[key]; exists {
			t.Fatal("recovery sent input", key)
		}
	}
	if err := s.restoreWorkbenchSessions(context.Background()); err != nil || s.terminals.get(terminalID) != terminal {
		t.Fatalf("recovery started a second writer: %v", err)
	}
	s.terminals.remove(terminalID)
	if err := s.closeTerminalID(terminalID); err != nil {
		t.Fatal(err)
	}
	w := request(s, "POST", "/api/cli/sessions/"+row.ID+"/open", s.origin, `{"recover_terminal_id":"`+terminalID+`"}`, true)
	if w.Code != http.StatusConflict || len(guest.calls("tui-launch")) != 1 {
		t.Fatal("a dismissed terminal was reopened", w.Code, w.Body.String())
	}
}

func TestEntryTerminalRejoinsAfterTransportLoss(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	var abort func() error
	client, err := f.dialThrough(f.device, func(ws *controlwire.Conn, conn net.Conn) net.Conn {
		abort = ws.Close
		return conn
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	shell := entryOpenShell(t, client, 100, 30, "")
	shell.send("existing_value=kept; printf 'READY=%s\\n' \"$$\"\n")
	pid := shell.expect("READY=")
	if err := abort(); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 5*time.Second, "the detached shell to be offered for rejoin", func() bool {
		return len(f.s.entryWaitingTerminals(f.computer.ID)) == 1
	})
	rejoined := entryOpenShell(t, f.client(), 100, 30, "")
	rejoined.prompt("输入编号重新接入")
	rejoined.send("1\n")
	rejoined.send("printf 'AFTER=%s:%s\\n' \"$$\" \"$existing_value\"\n")
	if got := rejoined.expect("AFTER="); strings.TrimSpace(got) != strings.TrimSpace(pid)+":kept" {
		t.Fatalf("rejoin did not keep the original process: %q (original %q)", got, pid)
	}
	rejoined.send("exit\n")
}

func TestClaudeRecoveryDoesNotReplayPendingHandoffs(t *testing.T) {
	record := &mobileRecord{Switch: &claudeAccountSwitch{Phase: "completed", Carry: []claudeCarried{{Text: "queued fixture"}}}}
	if record.canResumeClaudeWithoutInput() {
		t.Fatal("an undelivered message could be sent on recovery")
	}
	record.Switch.Carry[0].Delivered = true
	if !record.canResumeClaudeWithoutInput() {
		t.Fatal("a completed handoff does not prevent native resume")
	}
	record.ResumeTranscript = "/frozen/history.jsonl"
	if record.canResumeClaudeWithoutInput() {
		t.Fatal("a frozen transcript could enter a new handoff during recovery")
	}
}
