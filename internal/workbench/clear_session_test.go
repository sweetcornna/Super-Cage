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
)

// clearFixture is a terminal record whose open terminal owns one channel
// connection, with a guest that answers discover from an observation file and
// records every request.
type clearFixture struct {
	s          *Server
	id         string
	project    string
	home       string
	connection string
	terminal   *terminalSession
	requests   string
	observe    func(...terminalConversation)
}

func newClearFixture(t *testing.T) *clearFixture {
	t.Helper()
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	f := &clearFixture{s: s, id: id, project: project, home: home, connection: uuid.NewString(), requests: filepath.Join(home, "requests.jsonl")}
	observation := filepath.Join(home, "observation.json")
	helper := filepath.Join(home, ".local/share/cage/cli-channel-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(cliChannelHelper+cliResumePrompt+claudeAccountResumePrompt))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf("import json,sys\nrequest=json.load(sys.stdin)\nopen(%q,'a').write(json.dumps([sys.argv[1],request])+'\\n')\nprint(open(%q).read() if sys.argv[1]=='discover' else json.dumps({'ok':True}))\n", f.requests, observation)
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	f.observe = func(found ...terminalConversation) {
		t.Helper()
		if found == nil {
			found = []terminalConversation{}
		}
		if err := writePrivate(observation, found); err != nil {
			t.Fatal(err)
		}
	}
	s.ready = true
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	record.Connection, record.Resume = f.connection, true
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	// The terminal shows the record's conversation and owns its connection.
	f.terminal = newTerminalSession(nil)
	f.terminal.id, f.terminal.sharedID, f.terminal.connectionID, f.terminal.workspace, f.terminal.kind = "live-terminal", id, f.connection, project, "claude"
	s.terminals.sessions[f.terminal.id] = f.terminal
	record.TerminalID = f.terminal.id
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *clearFixture) item(cliID, connection, title string, running bool) terminalConversation {
	return terminalConversation{CLIID: cliID, Workspace: guestPath(f.project), Config: filepath.Join(f.home, ".claude"), Connection: connection, Title: title, Running: running, UpdatedAt: 1795000000}
}

// list runs one discovery and returns the project's entries.
func (f *clearFixture) list(t *testing.T) []mobileSummary {
	t.Helper()
	// As a list request does: the catalog is loaded before discovery.
	if _, err := f.s.mobileCLI.summaries(f.s.profile, map[string]bool{f.project: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.discoverTerminalConversations(context.Background(), []string{f.project}); err != nil {
		t.Fatal(err)
	}
	list, err := f.s.mobileCLI.summaries(f.s.profile, map[string]bool{f.project: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func (f *clearFixture) record(t *testing.T) *mobileRecord {
	t.Helper()
	f.s.mobileCLI.mu.Lock()
	defer f.s.mobileCLI.mu.Unlock()
	record, err := f.s.mobileCLI.record(f.s.profile, f.id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestClearKeepsOneEntryThroughRestarts(t *testing.T) {
	f := newClearFixture(t)
	a, b, c := f.record(t).CLIID, uuid.NewString(), uuid.NewString()
	f.observe(f.item(a, f.connection, "整理发布流程", true))
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id || list[0].Title != "整理发布流程" || !list[0].Running {
		t.Fatalf("before /clear: %+v", list)
	}
	if _, err := f.s.updateSessionMetadata(f.id, sessionMetadataInput{Pinned: new(bool)}, nil); err != nil {
		t.Fatal(err)
	}
	pinned, named := true, "发布 v2"
	if _, err := f.s.updateSessionMetadata(f.id, sessionMetadataInput{Title: &named, Pinned: &pinned}, nil); err != nil {
		t.Fatal(err)
	}
	// The SessionStart hook moved the channel to the new id before the CLI
	// wrote its transcript: the guest knows it only from the channel state.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, f.connection, untitledTitle, true))
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id || !list[0].Running {
		t.Fatalf("a new id without transcript made an entry: %+v", list)
	}
	// Its transcript begins with the /clear command.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, f.connection, clearedTitle, true))
	list := f.list(t)
	if len(list) != 1 || list[0].ID != f.id || !list[0].Pinned || list[0].Title != named || !list[0].Running {
		t.Fatalf("after /clear: %+v", list)
	}
	record := f.record(t)
	if record.CLIID != b || !record.Resume || record.Title != "整理发布流程" || len(record.Cleared) != 1 || record.Cleared[0].CLIID != a || record.Cleared[0].Empty || record.TerminalID != f.terminal.id {
		t.Fatalf("record after /clear: %+v", record)
	}
	if f.terminal.sharedID != f.id {
		t.Fatalf("the terminal moved to %s", f.terminal.sharedID)
	}
	// Twice in a row, the second time before any prompt.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, "", clearedTitle, false), f.item(c, f.connection, clearedTitle, true))
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id {
		t.Fatalf("after a second /clear: %+v", list)
	}
	if record := f.record(t); record.CLIID != c || len(record.Cleared) != 2 || record.Cleared[1].CLIID != b || len(record.Executions) != 2 {
		t.Fatalf("record after a second /clear: %+v", record)
	}
	// /compact keeps the id: nothing changes.
	before := f.record(t).Revision
	f.list(t)
	if after := f.record(t); after.Revision != before || after.CLIID != c {
		t.Fatalf("an unchanged guest rewrote the record: %d -> %d", before, after.Revision)
	}
	// After an app restart (no terminal, fresh store) and with the old
	// transcripts still there, still one entry, on the latest id.
	f.s.terminals.sessions = map[string]*terminalSession{}
	f.s.mobileCLI = mobileCLIStore{}
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, "", clearedTitle, false), f.item(c, "", clearedTitle, false))
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id || list[0].Title != named {
		t.Fatalf("after a restart: %+v", list)
	}
	if record := f.record(t); record.CLIID != c || record.Title != "整理发布流程" {
		t.Fatalf("record after a restart: %+v", record)
	}
}

func TestClearBeforeTheFirstPromptLeavesAnEmptyPart(t *testing.T) {
	f := newClearFixture(t)
	a, b := f.record(t).CLIID, uuid.NewString()
	// The CLI writes no transcript before the first prompt.
	f.observe(f.item(b, f.connection, clearedTitle, true))
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id {
		t.Fatalf("after /clear at once: %+v", list)
	}
	if record := f.record(t); record.CLIID != b || len(record.Cleared) != 1 || record.Cleared[0].CLIID != a || !record.Cleared[0].Empty {
		t.Fatalf("record: %+v", record)
	}
}

func TestResumeOfAnotherConversationIsARealSwitch(t *testing.T) {
	f := newClearFixture(t)
	a, other := f.record(t).CLIID, uuid.NewString()
	f.observe(f.item(a, f.connection, "整理发布流程", true))
	f.list(t)
	// /resume in the terminal: the other conversation's own transcript, with
	// its own first prompt, now on the terminal's connection.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(other, f.connection, "另一段对话", true))
	list := f.list(t)
	if len(list) != 2 {
		t.Fatalf("lists after /resume: %+v", list)
	}
	record := f.record(t)
	if record.CLIID != a || len(record.Cleared) != 0 {
		t.Fatalf("the left conversation changed: %+v", record)
	}
	var joined *mobileSummary
	for i := range list {
		if list[i].ID != f.id {
			joined = &list[i]
		}
	}
	if joined == nil || joined.Title != "另一段对话" || !joined.Running || f.terminal.sharedID != joined.ID {
		t.Fatalf("the joined conversation: %+v, terminal on %s", joined, f.terminal.sharedID)
	}
	// Opening the entry it left never hands back the terminal that now shows
	// the other conversation.
	if bound := f.s.boundTerminal(record); bound != nil {
		t.Fatal("the left entry still names the moved terminal")
	}
	f.s.mobileCLI.mu.Lock()
	other2, err := f.s.mobileCLI.record(f.s.profile, joined.ID)
	f.s.mobileCLI.mu.Unlock()
	if err != nil || f.s.boundTerminal(other2) != f.terminal {
		t.Fatalf("the joined entry lost its terminal: %v", err)
	}
	if terminal, _ := f.s.createTerminalLocked(terminalRequest{Workspace: f.project, Kind: "claude", sharedID: f.id}); terminal == f.terminal {
		t.Fatal("opening the left entry returned the terminal of another conversation")
	}
	// A resume into a conversation that is already an entry (here an earlier
	// cleared part counts too) is never linked.
	f.observe(f.item(a, f.connection, "整理发布流程", true), f.item(other, "", "另一段对话", false))
	if list := f.list(t); len(list) != 2 {
		t.Fatalf("lists after resuming back: %+v", list)
	}
}

func TestUnknownCommandTitleKeepsTodaysBehaviour(t *testing.T) {
	f := newClearFixture(t)
	a, b := f.record(t).CLIID, uuid.NewString()
	// Any other first command (here /model) is not a /clear: a separate entry
	// as before, which is also what happens when the CLI's hook values change.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, f.connection, "/model", true))
	if list := f.list(t); len(list) != 2 {
		t.Fatalf("lists: %+v", list)
	}
	if record := f.record(t); record.CLIID != a || len(record.Cleared) != 0 {
		t.Fatalf("record: %+v", record)
	}
}

func TestSessionsSplitBeforeThisChangeStayAsTheyAre(t *testing.T) {
	f := newClearFixture(t)
	a, b := f.record(t).CLIID, uuid.NewString()
	// An older version made a second entry for the cleared conversation; the
	// discovery that blanked the first entry's connection left no link.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, "", clearedTitle, false))
	f.s.terminals.sessions = map[string]*terminalSession{}
	record := f.record(t)
	record.Connection, record.TerminalID = "", ""
	if err := writePrivate(mobileRecordPath(f.s.profile, f.id), record); err != nil {
		t.Fatal(err)
	}
	f.s.mobileCLI = mobileCLIStore{}
	if list := f.list(t); len(list) != 2 {
		t.Fatalf("old split sessions: %+v", list)
	}
	if record := f.record(t); record.CLIID != a || len(record.Cleared) != 0 {
		t.Fatalf("record: %+v", record)
	}
}

func TestPurgeTakesTheClearedPartsWithTheEntry(t *testing.T) {
	f := newClearFixture(t)
	a, b, c := f.record(t).CLIID, uuid.NewString(), uuid.NewString()
	f.observe(f.item(a, f.connection, "整理发布流程", true))
	f.list(t)
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, f.connection, clearedTitle, true))
	f.list(t)
	f.observe(f.item(b, "", clearedTitle, false), f.item(c, f.connection, clearedTitle, true))
	f.list(t)
	record := f.record(t)
	if len(record.Cleared) != 2 || record.Cleared[0].Empty || record.Cleared[1].Empty {
		t.Fatalf("cleared parts: %+v", record.Cleared)
	}
	// The first part's transcript vanished before the second /clear was seen
	// (present only in the first two discoveries): it was not empty then.
	f.terminal.closed = true
	close(f.terminal.done)
	if w := request(f.s, "POST", "/api/cli/sessions/"+f.id+"/archive", f.s.origin, `{}`, true); w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}
	// Archived: none of its parts is listed, even when their transcripts are.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, "", clearedTitle, false), f.item(c, "", clearedTitle, false))
	if list := f.list(t); len(list) != 0 {
		t.Fatalf("an archived entry's parts were listed: %+v", list)
	}
	if w := request(f.s, "POST", "/api/cli/sessions/"+f.id+"/purge", f.s.origin, `{"confirm":true}`, true); w.Code != 200 {
		t.Fatalf("purge: %d %s", w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(f.requests)
	purged := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry []json.RawMessage
		var input struct {
			CLIID  string `json:"cli_id"`
			Expect bool   `json:"expect_history"`
		}
		var mode string
		if json.Unmarshal([]byte(line), &entry) != nil || len(entry) != 2 || json.Unmarshal(entry[0], &mode) != nil || mode != "purge" || json.Unmarshal(entry[1], &input) != nil {
			continue
		}
		purged[input.CLIID] = input.Expect
	}
	if len(purged) != 3 || !purged[a] || !purged[b] || !purged[c] {
		t.Fatalf("purge requests: %v", purged)
	}
	// After the purge the transcripts still listed by a slow guest stay hidden.
	if list := f.list(t); len(list) != 0 {
		t.Fatalf("purged parts came back: %+v", list)
	}
}
