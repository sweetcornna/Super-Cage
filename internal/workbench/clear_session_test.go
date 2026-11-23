package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	views      map[string]terminalConversation
	viewsPath  string
}

// view sets what the guest answers to a view of that id.
func (f *clearFixture) view(t *testing.T, cliID string, view terminalConversation) {
	t.Helper()
	f.views[cliID] = view
	if err := writePrivate(f.viewsPath, f.views); err != nil {
		t.Fatal(err)
	}
}

func newClearFixture(t *testing.T) *clearFixture {
	t.Helper()
	s, id, project := metadataFixture(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	f := &clearFixture{s: s, id: id, project: project, home: home, connection: uuid.NewString(), requests: filepath.Join(home, "requests.jsonl"),
		views: map[string]terminalConversation{}, viewsPath: filepath.Join(home, "views.json")}
	if err := writePrivate(f.viewsPath, f.views); err != nil {
		t.Fatal(err)
	}
	observation := filepath.Join(home, "observation.json")
	helper := filepath.Join(home, ".local/share/cage/cli-channel-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(cliChannelHelper+cliResumePrompt+claudeAccountResumePrompt))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf("import json,sys\nrequest=json.load(sys.stdin)\nmode=sys.argv[1]\nopen(%q,'a').write(json.dumps([mode,request])+'\\n')\nif mode=='discover': print(open(%q).read())\nelif mode=='view': print(json.dumps(json.load(open(%q)).get(request.get('cli_id'),{})))\nelse: print(json.dumps({'ok':True}))\n", f.requests, observation, f.viewsPath)
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
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id || list[0].EarlierParts != 2 {
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

func TestEarlierPartsAreListedReadAndSeparated(t *testing.T) {
	f := newClearFixture(t)
	a, b, c := f.record(t).CLIID, uuid.NewString(), uuid.NewString()
	turn := func(prompt, text string) mobileTurn {
		return mobileTurn{ID: uuid.NewString(), Prompt: prompt, Text: text, Status: "completed"}
	}
	clearRow := turn("<command-name>/clear</command-name>\n<command-message>clear</command-message>", "")
	f.view(t, a, terminalConversation{CLIID: a, Workspace: guestPath(f.project), HasHistory: true, Turns: []mobileTurn{turn("整理发布流程", "好的"), turn("再检查一次", "检查完了")}})
	f.view(t, b, terminalConversation{CLIID: b, Workspace: guestPath(f.project), HasHistory: true, Turns: []mobileTurn{clearRow}})
	f.view(t, c, terminalConversation{CLIID: c, Workspace: guestPath(f.project), HasHistory: true, Turns: []mobileTurn{clearRow, turn("第二个问题", "回答")}})
	f.observe(f.item(a, f.connection, "整理发布流程", true))
	f.list(t)
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, f.connection, clearedTitle, true))
	f.list(t)
	// /clear again at once: b holds only the /clear command.
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, "", clearedTitle, false), f.item(c, f.connection, clearedTitle, true))
	f.list(t)
	for _, route := range []string{"/api/mobile/cli/sessions/", "/api/cli/sessions/"} {
		w := request(f.s, "GET", route+f.id+"/earlier", f.s.origin, "", true)
		var answer struct {
			Parts []earlierPart `json:"parts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || len(answer.Parts) != 1 || answer.Parts[0].CLIID != a || answer.Parts[0].Prompt != "整理发布流程" || answer.Parts[0].Turns != 2 {
			t.Fatalf("%s earlier: %d %s", route, w.Code, w.Body.String())
		}
	}
	w := request(f.s, "GET", "/api/mobile/cli/sessions/"+f.id+"/earlier/"+a, f.s.origin, "", true)
	var part earlierView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &part) != nil || !part.ReadOnly || len(part.Items) != 2 || part.Items[1].Text != "检查完了" {
		t.Fatalf("part: %d %s", w.Code, w.Body.String())
	}
	if w := request(f.s, "GET", "/api/mobile/cli/sessions/"+f.id+"/earlier/"+uuid.NewString(), f.s.origin, "", true); w.Code != 404 {
		t.Fatalf("an id that is not a part: %d", w.Code)
	}
	// 恢复为单独会话: an ordinary session again, the entry keeps the rest.
	w = request(f.s, "POST", "/api/mobile/cli/sessions/"+f.id+"/earlier/"+a+"/separate", f.s.origin, "{}", true)
	var separate mobileSummary
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &separate) != nil || separate.ID == f.id || separate.Title != "整理发布流程" {
		t.Fatalf("separate: %d %s", w.Code, w.Body.String())
	}
	record := f.record(t)
	if record.CLIID != c || len(record.Cleared) != 1 || record.Cleared[0].CLIID != b || slices.ContainsFunc(record.Executions, func(e claudeExecution) bool { return e.CLIID == a }) {
		t.Fatalf("entry after separating: %+v", record)
	}
	list := f.list(t)
	if len(list) != 2 {
		t.Fatalf("lists after separating: %+v", list)
	}
	f.s.mobileCLI.mu.Lock()
	separated, err := f.s.mobileCLI.record(f.s.profile, separate.ID)
	f.s.mobileCLI.mu.Unlock()
	if err != nil || separated.CLIID != a || !separated.Resume || !separated.Terminal || separated.Title != "整理发布流程" {
		t.Fatalf("separated record: %+v %v", separated, err)
	}
}

// openRequests posts the open of the fixture's entry and returns the response
// and the guest requests it made.
func (f *clearFixture) open(t *testing.T) (code int, body, recovery string, modes []string) {
	t.Helper()
	before := f.modes(t)
	w := request(f.s, "POST", "/api/cli/sessions/"+f.id+"/open", f.s.origin, "{}", true)
	return w.Code, w.Body.String(), w.Header().Get("X-Cage-Recovery"), f.modes(t)[len(before):]
}

func (f *clearFixture) modes(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.requests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var modes []string
	for line := range strings.Lines(string(data)) {
		var entry []json.RawMessage
		var mode string
		if json.Unmarshal([]byte(line), &entry) == nil && len(entry) == 2 && json.Unmarshal(entry[0], &mode) == nil {
			modes = append(modes, mode)
		}
	}
	return modes
}

// The terminal is gone (an app or environment restart, or the person closed it).
func (f *clearFixture) closeTerminal(t *testing.T) {
	t.Helper()
	f.s.terminals.mu.Lock()
	delete(f.s.terminals.sessions, f.terminal.id)
	f.s.terminals.mu.Unlock()
}

func TestReopenOfATerminalConversationResumesItsHistory(t *testing.T) {
	f := newClearFixture(t)
	f.closeTerminal(t)
	record := f.record(t)
	cli := record.CLIID
	// Started fresh in a desktop terminal (--session-id), prompted there, never
	// read on the phone: the record still says new.
	record.Resume = false
	if err := writePrivate(mobileRecordPath(f.s.profile, f.id), record); err != nil {
		t.Fatal(err)
	}
	f.view(t, cli, terminalConversation{CLIID: cli, Config: filepath.Join(f.home, ".claude"), Workspace: guestPath(f.project), Title: "整理发布流程", HasHistory: true, NativeHistory: true,
		Turns: []mobileTurn{{ID: "t1", Prompt: "整理发布流程", Text: "好的", Status: "completed"}}})
	_, _, _, modes := f.open(t)
	if !slices.Contains(modes, "view") || !f.record(t).Resume {
		t.Fatalf("the open did not learn the history: modes %v, resume %v", modes, f.record(t).Resume)
	}
	script, _, err := f.s.prepareTerminalConversation(f.project, claudeSelection{}, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `\"resume\": \"`+cli+`\"`) && !strings.Contains(script, `"resume":"`+cli+`"`) {
		t.Fatalf("the launch does not resume %s: %s", cli, script)
	}
	// A launch that fails (exit 1: "No conversation found", "already in use",
	// a refused login) leaves a shell that says so; leaving Claude Code exits 0.
	if !strings.Contains(script, "|| printf") || !strings.Contains(script, "未能启动或接续") {
		t.Fatalf("no note after a failed launch: %s", script)
	}
}

func TestReopenOfATerminalNeverPromptedStaysNew(t *testing.T) {
	f := newClearFixture(t)
	f.closeTerminal(t)
	record := f.record(t)
	record.Resume = false
	if err := writePrivate(mobileRecordPath(f.s.profile, f.id), record); err != nil {
		t.Fatal(err)
	}
	f.view(t, record.CLIID, terminalConversation{CLIID: record.CLIID, Config: filepath.Join(f.home, ".claude"), Workspace: guestPath(f.project), Title: "Claude Code"})
	if _, _, _, modes := f.open(t); !slices.Contains(modes, "view") || f.record(t).Resume {
		t.Fatalf("modes %v, resume %v", modes, f.record(t).Resume)
	}
	script, _, err := f.s.prepareTerminalConversation(f.project, claudeSelection{}, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "resume") && strings.Contains(script, record.CLIID+`"`) && strings.Contains(script, `"resume":"`) {
		t.Fatalf("a conversation without history was resumed: %s", script)
	}
}

func TestReopenUnderAnUnusableAccountSaysWhichAndWhy(t *testing.T) {
	f := newClearFixture(t)
	f.closeTerminal(t)
	f.observe()
	record := f.record(t)
	subject := claudeIdentityHash("a@example.test", "")
	accountID := "a1b2c3d4e5f6a7b8c9d0e1f2"
	record.Account = claudeSelection{ID: accountID, GuestID: accountID, Subject: subject}
	record.AccountLabel = "工作账号"
	if err := writePrivate(mobileRecordPath(f.s.profile, f.id), record); err != nil {
		t.Fatal(err)
	}
	ready := claudeAccount{ID: accountID, Label: "工作账号", Enabled: true, Status: "ready", Email: "a@example.test"}
	for _, step := range []struct {
		name     string
		accounts []claudeAccount
		says     string
	}{
		{"removed", []claudeAccount{}, "已从 Super Cage 移除"},
		{"disabled", []claudeAccount{func(a claudeAccount) claudeAccount { a.Enabled = false; return a }(ready)}, "已停用"},
		{"signed out", []claudeAccount{func(a claudeAccount) claudeAccount { a.Status = "signed_out"; return a }(ready)}, "已退出登录"},
		{"another login", []claudeAccount{func(a claudeAccount) claudeAccount { a.Email = "b@example.test"; return a }(ready)}, "登录的身份已变化"},
	} {
		if err := f.s.saveClaudeAccounts(claudeAccountList{Accounts: step.accounts}); err != nil {
			t.Fatal(err)
		}
		code, body, recovery, modes := f.open(t)
		if code != 409 || !strings.Contains(body, "「工作账号」") || !strings.Contains(body, step.says) || !strings.Contains(body, "Claude 账号管理") || recovery != "claude-account" {
			t.Fatalf("%s: %d %q recovery %q", step.name, code, body, recovery)
		}
		if slices.Contains(modes, "active") || slices.Contains(modes, "launch") {
			t.Fatalf("%s: the guest was asked %v before the account was usable", step.name, modes)
		}
		if list := f.list(t); len(list) != 1 || list[0].ID != f.id {
			t.Fatalf("%s: the entry left the list: %+v", step.name, list)
		}
	}
	if err := f.s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{ready}}); err != nil {
		t.Fatal(err)
	}
	if code, body, recovery, _ := f.open(t); recovery != "" || strings.Contains(body, "无法接续") {
		t.Fatalf("a usable account was refused: %d %q", code, body)
	}
}

func TestReopenOfAConversationWhoseRecordIsGoneSaysSo(t *testing.T) {
	f := newClearFixture(t)
	f.closeTerminal(t)
	f.observe()
	record := f.record(t)
	config := filepath.Join(f.home, ".claude")
	// Claude Code removed the transcript (its retention period) or someone deleted it.
	f.view(t, record.CLIID, terminalConversation{CLIID: record.CLIID, Config: config, Workspace: guestPath(f.project), Title: "Claude Code"})
	code, body, recovery, modes := f.open(t)
	if code != 409 || recovery != "conversation-missing" || !strings.Contains(body, "找不到这段会话") || !strings.Contains(body, config) {
		t.Fatalf("%d %q %q", code, body, recovery)
	}
	if slices.Contains(modes, "active") {
		t.Fatalf("the guest was asked %v", modes)
	}
	if list := f.list(t); len(list) != 1 || list[0].ID != f.id {
		t.Fatalf("the entry left the list: %+v", list)
	}
	// A transcript without readable turns is still there: the CLI resumes it.
	f.view(t, record.CLIID, terminalConversation{CLIID: record.CLIID, Config: config, Workspace: guestPath(f.project), Title: "Claude Code", NativeHistory: true})
	if _, body, recovery, _ := f.open(t); recovery == "conversation-missing" {
		t.Fatalf("a present transcript was called missing: %q", body)
	}
}

func TestResumeFromAFolderThatIsGoneStartsInTheProject(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, ".claude/worktrees/done")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(directory string) string {
		t.Helper()
		out, err := exec.Command("/bin/bash", "-c", terminalDirectory(directory, root)+" && pwd -P").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	real := func(path string) string { path, _ = filepath.EvalSymlinks(path); return path }
	if got := run(sub); got != real(sub) {
		t.Fatalf("an existing folder: %s", got)
	}
	if got := run(root); got != real(root) {
		t.Fatalf("the project: %s", got)
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	got := run(sub)
	if lines := strings.Split(got, "\n"); len(lines) != 2 || !strings.Contains(lines[0], "已不存在") || lines[1] != real(root) {
		t.Fatalf("a removed worktree: %q", got)
	}
}

func TestPhoneInsideTheConversationFollowsTheClear(t *testing.T) {
	f := newClearFixture(t)
	a, b := f.record(t).CLIID, uuid.NewString()
	f.observe(f.item(a, f.connection, "整理发布流程", true))
	f.list(t)
	// The phone polls only the view: the old id no longer has the connection.
	f.view(t, a, terminalConversation{CLIID: a, Config: filepath.Join(f.home, ".claude"), Workspace: guestPath(f.project), HasHistory: true, NativeHistory: true,
		Turns: []mobileTurn{{ID: uuid.NewString(), Prompt: "整理发布流程", Status: "completed"}}})
	f.observe(f.item(a, "", "整理发布流程", false), f.item(b, f.connection, clearedTitle, true))
	if _, err := f.s.terminalConversationView(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.record(t).CLIID != b {
		if time.Now().After(deadline) {
			t.Fatalf("the view did not follow the /clear: %+v", f.record(t))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if list, _ := f.s.mobileCLI.summaries(f.s.profile, map[string]bool{f.project: true}, false); len(list) != 1 {
		t.Fatalf("lists: %+v", list)
	}
}
