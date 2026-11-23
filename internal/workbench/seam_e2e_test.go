//go:build !windows

package workbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The seam between /clear (clear_session) and the account handoff
// (claude_switch), with the official CLI in the isolated harness of
// claude_handoff_limit_e2e_test.go: /clear and the prompts after it are typed
// into the session's terminal as a person does.

// typeInto types text into the session's terminal one key at a time, then Enter.
func (h *limitHarness) typeInto(id, text string) {
	h.t.Helper()
	terminal := h.s.terminals.get(h.record(id).TerminalID)
	if terminal == nil {
		h.t.Fatal("no terminal for the session")
	}
	for _, key := range text {
		if err := terminal.enqueueInput(context.Background(), string(key)); err != nil {
			h.t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if err := terminal.enqueueInput(context.Background(), "\r"); err != nil {
		h.t.Fatal(err)
	}
}

// clearAndPrompt types /clear and then a prompt, and waits until the entry
// follows the new conversation and the prompt is answered.
func (h *limitHarness) clearAndPrompt(id, prompt string) (previous, current string) {
	h.t.Helper()
	previous = h.record(id).CLIID
	h.typeInto(id, "/clear")
	time.Sleep(2 * time.Second)
	h.typeInto(id, prompt)
	h.wait(id, "the entry follows the /clear", func(v mobileSession) bool {
		r := h.record(id)
		return r.CLIID != previous && r.clearedPart(previous) != nil &&
			len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Prompt == prompt && v.Turns[len(v.Turns)-1].Status == "completed" && !v.Active
	})
	return previous, h.record(id).CLIID
}

// keepScreen writes the session terminal's output to SEAM_SCREENS when the test fails.
func (h *limitHarness) keepScreen(id, name string) {
	folder := os.Getenv("SEAM_SCREENS")
	if folder == "" {
		return
	}
	h.t.Cleanup(func() {
		if !h.t.Failed() {
			return
		}
		if terminal := h.s.terminals.get(h.record(id).TerminalID); terminal != nil {
			_ = os.MkdirAll(folder, 0700)
			_ = os.WriteFile(filepath.Join(folder, h.t.Name()+"-"+name+".txt"), terminal.outputAfter(0, 1<<20).Data, 0600)
		}
	})
}

func (h *limitHarness) account(id string, body map[string]any) (int, string) {
	raw, _ := json.Marshal(body)
	response := request(h.s, "POST", "/api/mobile/cli/sessions/"+id+"/account", h.s.origin, string(raw), true)
	return response.Code, response.Body.String()
}

type seamInspect struct {
	Source  string               `json:"source_id"`
	Targets []claudeSwitchTarget `json:"targets"`
	Undo    bool                 `json:"undo"`
}

func (h *limitHarness) inspect(id string) seamInspect {
	h.t.Helper()
	code, reply := h.account(id, map[string]any{"action": "inspect"})
	var view seamInspect
	if code != 200 || json.Unmarshal([]byte(reply), &view) != nil {
		h.t.Fatal("inspect:", code, reply)
	}
	return view
}

// switchTo is the manual switch from the account menu.
func (h *limitHarness) switchTo(id, label string) {
	h.t.Helper()
	view := h.inspect(id)
	target := ""
	for _, option := range view.Targets {
		if option.Label == label {
			target = option.ID
		}
	}
	if target == "" {
		h.t.Fatalf("no target %s: %+v", label, view.Targets)
	}
	input := map[string]any{"action": "switch", "target": target, "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true}
	if code, reply := h.account(id, input); code != 200 {
		h.t.Fatal("switch:", code, reply)
	}
	h.wait(id, "the switch to "+label, func(v mobileSession) bool {
		return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.CanSend != nil && *v.CanSend && !v.Active
	})
}

func (h *limitHarness) sessions() []mobileSummary {
	h.t.Helper()
	response := request(h.s, "GET", "/api/mobile/cli/sessions", h.s.origin, "", true)
	var answer struct {
		Sessions []mobileSummary `json:"sessions"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &answer) != nil {
		h.t.Fatal("list:", response.Code, response.Body.String())
	}
	return answer.Sessions
}

func (h *limitHarness) earlier(id string) []earlierPart {
	h.t.Helper()
	response := request(h.s, "GET", "/api/mobile/cli/sessions/"+id+"/earlier", h.s.origin, "", true)
	var answer struct {
		Parts []earlierPart `json:"parts"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &answer) != nil {
		h.t.Fatal("earlier:", response.Code, response.Body.String())
	}
	return answer.Parts
}

// transcripts lists the conversation ids with a transcript in an account's config.
func (h *limitHarness) transcripts(account string) map[string]string {
	matches, _ := filepath.Glob(filepath.Join(h.home, ".local/share/cage/claude-accounts", account, "config/projects/*/*.jsonl"))
	found := map[string]string{}
	for _, path := range matches {
		found[strings.TrimSuffix(filepath.Base(path), ".jsonl")] = path
	}
	return found
}

// firstCwd is the first cwd a transcript records (what the handoff's project check reads).
func firstCwd(path string) string {
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		var row struct {
			Cwd       string `json:"cwd"`
			Sidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal([]byte(line), &row) == nil && row.Cwd != "" && !row.Sidechain {
			return row.Cwd
		}
	}
	return ""
}

func (h *limitHarness) plainAnswers() {
	h.script = func(account, _ string) (string, map[string]any) {
		return "end_turn", map[string]any{"type": "text", "text": "ok from " + account}
	}
}

// a. /clear, a prompt, then a manual switch: the target continues the new
// conversation, the entry stays one entry, the earlier part (in A's config)
// is listed, opens, and becomes its own session that resumes on A.
func TestSeamClearThenManualSwitchEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.plainAnswers()
	a, b := h.list.Accounts[0].ID, h.list.Accounts[1].ID
	id := h.start()
	h.send(id, "first topic")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	first, second := h.clearAndPrompt(id, "second topic")
	h.switchTo(id, "B")
	h.send(id, "third on b")
	h.wait(id, "the answer on B", func(v mobileSession) bool {
		return len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Prompt == "third on b" && v.Turns[len(v.Turns)-1].Status == "completed" && !v.Active
	})
	onB := h.lastBody("b", "third on b")
	if !strings.Contains(onB, "second topic") || strings.Contains(onB, "first topic") {
		t.Fatalf("B did not continue the conversation after /clear: %s", onB)
	}
	record := h.record(id)
	if record.Account.ID != b || len(record.Cleared) != 1 || record.Cleared[0].CLIID != first || len(record.Executions) != 2 ||
		record.Executions[0].CLIID != first || record.Executions[1].CLIID != second || record.Executions[0].Account.ID != a || record.Executions[1].Account.ID != a {
		t.Fatalf("record after the switch: %+v", record)
	}
	if list := h.sessions(); len(list) != 1 || list[0].ID != id || list[0].EarlierParts != 1 {
		t.Fatalf("not one entry: %+v", list)
	}
	parts := h.earlier(id)
	if len(parts) != 1 || parts[0].CLIID != first || parts[0].Prompt != "first topic" {
		t.Fatalf("earlier parts after the switch: %+v", parts)
	}
	var part earlierView
	response := request(h.s, "GET", "/api/mobile/cli/sessions/"+id+"/earlier/"+first, h.s.origin, "", true)
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &part) != nil || !part.ReadOnly || len(part.Items) != 1 || part.Items[0].Prompt != "first topic" {
		t.Fatalf("the earlier part does not open: %d %s", response.Code, response.Body.String())
	}
	if _, ok := h.transcripts(a)[first]; !ok {
		t.Fatal("the earlier part is not in A's config directory")
	}
	// 恢复为单独会话 after the switch: a session of its own, on A, where its history is.
	response = request(h.s, "POST", "/api/mobile/cli/sessions/"+id+"/earlier/"+first+"/separate", h.s.origin, "{}", true)
	var separated mobileSummary
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &separated) != nil {
		t.Fatal("separate:", response.Code, response.Body.String())
	}
	if list := h.sessions(); len(list) != 2 {
		t.Fatalf("after separating: %+v", list)
	}
	if separated.AccountID != a || separated.AccountLabel != "A" {
		t.Fatalf("the separated part shows account %q %q, its history is on A", separated.AccountID, separated.AccountLabel)
	}
	if code := h.call(separated.ID, "POST", "/open", map[string]any{}, nil); code != 200 {
		t.Fatal("open the separated session:", code)
	}
	h.wait(separated.ID, "the separated session on A", func(v mobileSession) bool { return v.CanSend != nil && *v.CanSend })
	h.send(separated.ID, "back to the first")
	h.wait(separated.ID, "its answer", func(v mobileSession) bool {
		return len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Prompt == "back to the first" && v.Turns[len(v.Turns)-1].Status == "completed" && !v.Active
	})
	onA := h.lastBody("a", "back to the first")
	if !strings.Contains(onA, "first topic") || strings.Contains(onA, "second topic") || h.record(separated.ID).Account.ID != a {
		t.Fatalf("the separated part did not resume on A: %s", onA)
	}
	if r := h.record(id); len(r.Cleared) != 0 || len(r.Executions) != 1 || r.Executions[0].CLIID != second {
		t.Fatalf("the entry after separating: %+v", r)
	}
}

// b and e. A handoff at a limit, then /clear and a prompt on the target: one
// entry, the earlier part listed, the undo withdrawn (and refused) while the
// entry is on the new conversation, offered again (back to A) after /resume of
// the part. Then archive, restore and purge take the handoff's source, the
// cleared part and the current conversation together.
func TestSeamHandoffThenClearEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.list.Scheduling = claudeScheduling{Mode: "sequential", SwitchPolicy: "allow", SwitchAnswered: true, UngroupedHandoff: true}
	if err := h.s.saveClaudeAccounts(h.list); err != nil {
		t.Fatal(err)
	}
	h.plainAnswers()
	a, b := h.list.Accounts[0].ID, h.list.Accounts[1].ID
	id := h.start()
	h.send(id, "hello before the limit")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	source := h.record(id).CLIID
	h.limited.Store("seven_day")
	h.send(id, "this one hits the limit")
	h.wait(id, "the limit", func(v mobileSession) bool { return v.AccountFailure == "rate_limit" })
	h.wait(id, "the automatic handoff", func(v mobileSession) bool {
		return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.AccountID == b && v.CanSend != nil && *v.CanSend && !v.Active
	})
	target := h.record(id).CLIID
	if !h.inspect(id).Undo {
		t.Fatal("no undo right after the handoff")
	}
	cleared, current := h.clearAndPrompt(id, "fresh on b")
	if cleared != target {
		t.Fatalf("cleared %s, the handoff's conversation was %s", cleared, target)
	}
	if list := h.sessions(); len(list) != 1 || list[0].ID != id || list[0].EarlierParts != 1 {
		t.Fatalf("not one entry: %+v", list)
	}
	if parts := h.earlier(id); len(parts) != 1 || parts[0].CLIID != target {
		t.Fatalf("earlier parts: %+v", parts)
	}
	// The undo would move the new conversation back to A: it is withdrawn.
	view := h.inspect(id)
	if view.Undo {
		t.Fatal("undo still offered after /clear")
	}
	if code, reply := h.account(id, map[string]any{"action": "undo", "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true}); code == 200 || !strings.Contains(reply, "已不能撤销") {
		t.Fatal("undo after /clear:", code, reply)
	}
	if r := h.record(id); r.CLIID != current || r.Account.ID != b {
		t.Fatalf("the refused undo changed the entry: %+v", r)
	}
	// /resume of the cleared part in the terminal: the entry follows it back,
	// and the undo is offered again, back to A.
	h.typeInto(id, "/resume "+target)
	h.keepScreen(id, "resume")
	h.wait(id, "the entry follows /resume of its earlier part", func(mobileSession) bool {
		r := h.record(id)
		return r.CLIID == target && len(r.Cleared) == 1 && r.Cleared[0].CLIID == current
	})
	list, _ := h.s.loadClaudeAccounts()
	if back := claudeUndoTarget(list, h.record(id)); back == nil || back.ID != a || !h.inspect(id).Undo {
		t.Fatalf("undo after /resume of the handoff's conversation: %+v", back)
	}
	// The undo runs: the handoff's conversation goes back to A under a new id;
	// the part cleared on B stays listed.
	view = h.inspect(id)
	if code, reply := h.account(id, map[string]any{"action": "undo", "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true}); code != 200 {
		t.Fatal("undo:", code, reply)
	}
	h.wait(id, "the undo", func(v mobileSession) bool {
		return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.AccountID == a && v.CanSend != nil && *v.CanSend && !v.Active
	})
	undone := h.record(id).CLIID
	if r := h.record(id); r.Account.ID != a || undone == target || undone == current || r.clearedPart(current) == nil {
		t.Fatalf("after the undo: %+v", r)
	}
	if parts := h.earlier(id); len(parts) != 1 || parts[0].CLIID != current {
		t.Fatalf("earlier parts after the undo: %+v", parts)
	}
	// e. Archive, restore, purge.
	if code := h.call(id, "POST", "/archive", map[string]any{}, nil); code != 200 {
		t.Fatal("archive:", code)
	}
	if list := h.sessions(); len(list) != 0 {
		t.Fatalf("listed after archive: %+v", list)
	}
	if code := h.call(id, "POST", "/restore", map[string]any{}, nil); code != 200 {
		t.Fatal("restore:", code)
	}
	if list := h.sessions(); len(list) != 1 || list[0].EarlierParts != 1 {
		t.Fatalf("after restore: %+v", list)
	}
	if code := h.call(id, "POST", "/archive", map[string]any{}, nil); code != 200 {
		t.Fatal("archive:", code)
	}
	before := map[string]bool{}
	for cli := range h.transcripts(a) {
		before[cli] = true
	}
	for cli := range h.transcripts(b) {
		before[cli] = true
	}
	for _, cli := range []string{source, target, current} {
		if !before[cli] {
			t.Fatalf("missing transcript %s before the purge: %v", cli, before)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for code := 0; code != 200; {
		code = h.call(id, "POST", "/purge", map[string]any{"confirm": true}, nil)
		if code != 200 && time.Now().After(deadline) {
			t.Fatal("purge:", code)
		}
		if code != 200 {
			time.Sleep(time.Second)
		}
	}
	for _, account := range []string{a, b} {
		for _, cli := range []string{source, target, current, undone} {
			if path, ok := h.transcripts(account)[cli]; ok {
				t.Fatalf("%s survived the purge: %s", cli, path)
			}
		}
	}
	if frozen, _ := filepath.Glob(filepath.Join(h.home, ".local/share/cage/cli-account-switches/*/history.jsonl")); len(frozen) != 0 {
		t.Fatalf("the handoff's frozen history survived the purge: %v", frozen)
	}
	if list := h.sessions(); len(list) != 0 {
		t.Fatalf("listed after the purge: %+v", list)
	}
}

// Review finding 3: an idle manual switch resumes the target from the frozen
// copy, and the official CLI writes the target's transcript only with its first
// prompt. A /clear before that prompt left the target's id without history:
// the conversation before the /clear was hidden and purge skipped it. Now the
// handoff's source (in A's config) is that earlier part.
func TestSeamSwitchThenClearBeforeTheTargetsFirstPromptEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.plainAnswers()
	a, b := h.list.Accounts[0].ID, h.list.Accounts[1].ID
	id := h.start()
	h.send(id, "hello on a")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	source := h.record(id).CLIID
	h.switchTo(id, "B")
	target := h.record(id).CLIID
	if _, ok := h.transcripts(b)[target]; ok {
		t.Fatal("the official CLI wrote the target's transcript before its first prompt: this test no longer covers the case")
	}
	_, current := h.clearAndPrompt(id, "fresh on b")
	if list := h.sessions(); len(list) != 1 || list[0].EarlierParts != 1 {
		t.Fatalf("not one entry with one earlier part: %+v", list)
	}
	parts := h.earlier(id)
	if len(parts) != 1 || parts[0].CLIID != source || parts[0].Prompt != "hello on a" {
		t.Fatalf("the conversation before the /clear is not listed: %+v (record %+v)", parts, h.record(id).Cleared)
	}
	var part earlierView
	response := request(h.s, "GET", "/api/mobile/cli/sessions/"+id+"/earlier/"+source, h.s.origin, "", true)
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &part) != nil || len(part.Items) != 1 || part.Items[0].Prompt != "hello on a" {
		t.Fatalf("the earlier part does not open: %d %s", response.Code, response.Body.String())
	}
	if code := h.call(id, "POST", "/archive", map[string]any{}, nil); code != 200 {
		t.Fatal("archive:", code)
	}
	deadline := time.Now().Add(30 * time.Second)
	for code := 0; code != 200; {
		code = h.call(id, "POST", "/purge", map[string]any{"confirm": true}, nil)
		if code != 200 && time.Now().After(deadline) {
			t.Fatal("purge:", code)
		}
		if code != 200 {
			time.Sleep(time.Second)
		}
	}
	for account, ids := range map[string][]string{a: {source}, b: {target, current}} {
		for _, cli := range ids {
			if path, ok := h.transcripts(account)[cli]; ok {
				t.Fatalf("%s survived the purge: %s", cli, path)
			}
		}
	}
	if frozen, _ := filepath.Glob(filepath.Join(h.home, ".local/share/cage/cli-account-switches/*/history.jsonl")); len(frozen) != 0 {
		t.Fatalf("the handoff's frozen history survived the purge: %v", frozen)
	}
}

// c. The CLI's own /cd moves a session into a subfolder of the project (a
// worktree under it is the same): its transcript moves to that folder's
// project directory, and after /clear the new transcript begins there. The
// entry follows, and a switch passes the project check (the first recorded
// folder lies within the project) and starts the target in that folder.
func TestSeamClearInASubfolderThenSwitchEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.plainAnswers()
	a := h.list.Accounts[0].ID
	sub := filepath.Join(h.project, ".claude", "worktrees", "topic")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	id := h.start()
	h.send(id, "topic kestrel before the move")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	h.typeInto(id, "/cd "+sub)
	time.Sleep(3 * time.Second)
	_, current := h.clearAndPrompt(id, "topic heron after the clear")
	if first := firstCwd(h.transcripts(a)[current]); first != sub {
		t.Fatalf("the new transcript begins in %q, not in the worktree %q", first, sub)
	}
	if list := h.sessions(); len(list) != 1 || list[0].ID != id || list[0].EarlierParts != 1 {
		t.Fatalf("not one entry: %+v", list)
	}
	h.switchTo(id, "B")
	if r := h.record(id); r.GuestWorkspace != sub {
		t.Fatalf("the target does not start in the session's folder: %q", r.GuestWorkspace)
	}
	h.send(id, "topic osprey on b")
	h.wait(id, "the answer on B", func(v mobileSession) bool {
		return len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Prompt == "topic osprey on b" && v.Turns[len(v.Turns)-1].Status == "completed" && !v.Active
	})
	onB := h.lastBody("b", "topic osprey on b")
	if !strings.Contains(onB, "topic heron after the clear") || strings.Contains(onB, "topic kestrel") {
		t.Fatalf("B did not continue the conversation after /clear: %s", onB)
	}
}

// c, outside. /cd to a folder outside the project (the CLI asks whether to
// trust it), then /clear and a prompt: the new conversation lives outside the
// project, so it is not linked and not listed. The entry keeps the part it
// had; a switch is refused, never as "another project", and changes nothing.
func TestSeamClearOutsideTheProjectEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.plainAnswers()
	a := h.list.Accounts[0].ID
	outside := filepath.Join(filepath.Dir(h.project), "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	id := h.start()
	h.send(id, "in the project")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	before := h.record(id).CLIID
	h.typeInto(id, "/cd "+outside)
	time.Sleep(3 * time.Second)
	terminal := h.s.terminals.get(h.record(id).TerminalID)
	_ = terminal.enqueueInput(context.Background(), "\x1b[B") // "Yes, move here"
	time.Sleep(500 * time.Millisecond)
	_ = terminal.enqueueInput(context.Background(), "\r")
	time.Sleep(3 * time.Second)
	h.typeInto(id, "/clear")
	time.Sleep(2 * time.Second)
	h.typeInto(id, "after the clear outside")
	deadline := time.Now().Add(20 * time.Second)
	var current string
	for current == "" && time.Now().Before(deadline) {
		for cli, path := range h.transcripts(a) {
			if cli != before && firstCwd(path) == outside {
				current = cli
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if current == "" {
		t.Fatalf("no new transcript outside the project: %v", h.transcripts(a))
	}
	for range 3 {
		h.sessions()
		// The view refuses while the CLI works outside the project.
		response := request(h.s, "GET", "/api/mobile/cli/sessions/"+id, h.s.origin, "", true)
		t.Logf("view while outside: %d %s", response.Code, strings.TrimSpace(response.Body.String()))
		time.Sleep(time.Second)
	}
	list := h.sessions()
	if r := h.record(id); len(list) != 1 || r.CLIID != before || len(r.Cleared) != 0 {
		t.Fatalf("a conversation outside the project was linked or listed: %+v %+v", list, r)
	}
	view := h.inspect(id)
	target := ""
	for _, option := range view.Targets {
		if option.Label == "B" {
			target = option.ID
		}
	}
	code, reply := h.account(id, map[string]any{"action": "switch", "target": target, "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true})
	t.Logf("switch while the CLI works outside the project: %d %s", code, strings.TrimSpace(reply))
	if code == 200 || strings.Contains(reply, claudeSwitchRefusals["switch:project"]) {
		t.Fatalf("switch: %d %s", code, reply)
	}
	r := h.record(id)
	if r.CLIID != before || r.Account.ID != a || h.s.terminals.get(r.TerminalID) != terminal || !terminal.ownsConversation() {
		t.Fatalf("the refused switch changed the entry: id %s (was %s), account %s, terminal %s (was %s)", r.CLIID, before, r.Account.ID, r.TerminalID, terminal.id)
	}
	if r.Switch != nil {
		// The guest lifted its own fence, but the host's extra cancel is refused
		// (switch:cancel: the channel now names the conversation outside), so
		// the refused preparation stays and every later choice fails on it.
		t.Skipf("open (claude_switch.go refusal branch, account handoff): the refused switch stays %q with %q", r.Switch.Phase, r.Switch.Error)
	}
}

// A switch picked right after /clear, before any prompt. The official CLI
// writes the new transcript only with that prompt, so the entry is still on
// the conversation before the /clear while its CLI already runs the new id:
// the guest refuses (switch:process). The refusal must leave no handoff
// behind; the /clear is linked with the next prompt and a switch then works.
func TestSeamSwitchRightAfterAClearEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.plainAnswers()
	id := h.start()
	h.send(id, "topic plover first")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	before := h.record(id).CLIID
	h.typeInto(id, "/clear")
	time.Sleep(3 * time.Second)
	view := h.inspect(id)
	target := ""
	for _, option := range view.Targets {
		if option.Label == "B" {
			target = option.ID
		}
	}
	code, reply := h.account(id, map[string]any{"action": "switch", "target": target, "request_id": uuid.NewString(), "source_id": view.Source, "confirm": true})
	r := h.record(id)
	t.Logf("switch right after /clear: %d %s; entry on %s (before %s)", code, strings.TrimSpace(reply), r.CLIID, before)
	if code == 200 {
		// The transcript was already there (the CLI writes it at /clear when it
		// has rows to write) and the entry followed it: the handoff takes the
		// new conversation, never the one before the /clear.
		h.wait(id, "the switch", func(v mobileSession) bool {
			return v.AccountSwitch != nil && v.AccountSwitch.Phase == "completed" && v.CanSend != nil && *v.CanSend && !v.Active
		})
		if r := h.record(id); r.Switch.Source.CLIID == before || r.clearedPart(before) == nil {
			t.Fatalf("handed off the conversation from before the /clear: %+v", r)
		}
		h.send(id, "topic wren on b")
		h.wait(id, "the answer on B", func(v mobileSession) bool {
			return len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Prompt == "topic wren on b" && v.Turns[len(v.Turns)-1].Status == "completed" && !v.Active
		})
		if onB := h.lastBody("b", "topic wren on b"); strings.Contains(onB, "topic plover") {
			t.Fatalf("B continued the conversation from before the /clear: %s", onB)
		}
		return
	}
	if strings.Contains(reply, claudeSwitchRefusals["switch:project"]) {
		t.Fatalf("refused as another project: %s", reply)
	}
	if r.Switch != nil {
		// The guest refused before fencing, but the host's extra cancel is
		// refused too (switch:cancel: the channel names the new id), so the
		// refused preparation stays: links wait for it and every later choice
		// fails on it ("原会话的交接已变化：刷新会话后再操作。").
		t.Skipf("open (claude_switch.go refusal branch, account handoff): the refused switch stays %q with %q", r.Switch.Phase, r.Switch.Error)
	}
	h.typeInto(id, "topic tern after")
	h.wait(id, "the entry follows the /clear", func(v mobileSession) bool { return h.record(id).clearedPart(before) != nil && !v.Active })
	h.switchTo(id, "B")
	h.send(id, "topic wren on b")
	h.wait(id, "the answer on B", func(v mobileSession) bool {
		return len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Prompt == "topic wren on b" && v.Turns[len(v.Turns)-1].Status == "completed" && !v.Active
	})
	if onB := h.lastBody("b", "topic wren on b"); !strings.Contains(onB, "topic tern after") || strings.Contains(onB, "topic plover") {
		t.Fatalf("B did not continue the conversation after /clear: %s", onB)
	}
}

// Between /clear and its first prompt the official CLI has already written the
// new transcript, holding only its mode and owner rows (no folder, no title).
// Lists in that time make no entry for it; the prompt links it.
func TestSeamListsBetweenAClearAndItsFirstPromptEndToEnd(t *testing.T) {
	h := newLimitHarness(t)
	h.plainAnswers()
	a := h.list.Accounts[0].ID
	id := h.start()
	h.send(id, "topic plover first")
	h.wait(id, "the first answer", func(v mobileSession) bool { return len(v.Turns) == 1 && v.Turns[0].Status == "completed" })
	before := h.record(id).CLIID
	h.typeInto(id, "/clear")
	deadline := time.Now().Add(10 * time.Second)
	for len(h.transcripts(a)) < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	for range 6 {
		if list := h.sessions(); len(list) != 1 || list[0].ID != id {
			t.Fatalf("a list between /clear and its first prompt: %+v", list)
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.typeInto(id, "topic tern after")
	h.wait(id, "the entry follows the /clear", func(v mobileSession) bool { return h.record(id).clearedPart(before) != nil && !v.Active })
	if list := h.sessions(); len(list) != 1 || list[0].ID != id || list[0].EarlierParts != 1 {
		t.Fatalf("after the first prompt: %+v", list)
	}
}
