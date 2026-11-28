//go:build !windows

package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestCodexSessionsHelper(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cmd := exec.Command(python, "-I", "codex_sessions_test.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("codex session helper: %v\n%s", err, output)
	}
}

func TestCodexSessionsCommandFitsGuestArgument(t *testing.T) {
	// SSH passes the command to the guest shell as one argument; Linux limits
	// a single argument to 128 KiB.
	command := codexSessionsCommand("discover")
	t.Logf("embedded command: %d bytes", len(command))
	if len(command) > 96<<10 {
		t.Fatalf("Codex session command uses %d bytes", len(command))
	}
}

type codexGuestCall struct {
	Mode    string         `json:"mode"`
	Request map[string]any `json:"request"`
}

// codexFakeGuest serves the Codex helper's operations from canned replies
// through the real host SSH path. Only the guest operation boundary is fake.
type codexFakeGuest struct {
	t    *testing.T
	home string
}

func installCodexRouteFixture(t *testing.T, home string) {
	t.Helper()
	folder := filepath.Join(home, ".local/share/cage/agent-runtime")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "client_runtime.py"), []byte(cage.ClientRuntimeHelper), 0600); err != nil {
		t.Fatal(err)
	}
}

func newCodexFixture(t *testing.T) (*Server, string, *codexFakeGuest) {
	t.Helper()
	s := testServer(t)
	project, _ := filepath.EvalSymlinks(t.TempDir())
	s.preferences.Workspace = project
	home, _ := filepath.EvalSymlinks(t.TempDir())
	installCodexRouteFixture(t, home)
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin"})
	helper := filepath.Join(home, ".local/share/cage/codex-session-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(codexSessionsHelper))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	// A terminal's tui-launch gets its request as an argument and stays, as
	// Codex would, until its terminal goes.
	code := "import json,os,sys\nmode=sys.argv[1]\nrequest=json.load(sys.stdin) if sys.argv[2]=='-' else json.loads(sys.argv[2])\nhome=os.environ['HOME']\n" +
		"open(os.path.join(home,'calls.jsonl'),'a').write(json.dumps({'mode':mode,'request':request})+'\\n')\n" +
		"if mode=='tui-launch':\n print('fake codex tui',flush=True)\n sys.stdin.read()\n sys.exit(0)\n" +
		// delays.json holds seconds a mode (or a view asking for the probe,
		// "view-probe") takes, marked by started-<key> and finished-<key>; a
		// probe is slow once, then cached as in the guest.
		"d=os.path.join(home,'delays.json')\nif os.path.exists(d):\n import time\n key=mode+('-probe' if isinstance(request,dict) and request.get('probe') else '')\n" +
		" wait=json.load(open(d)).get(key,0)\n cached=os.path.join(home,'probe-cached')\n if wait and not ('probe' in key and os.path.exists(cached)):\n" +
		"  open(os.path.join(home,'started-'+key),'w').close()\n  time.sleep(wait)\n  open(os.path.join(home,'finished-'+key),'w').close()\n" +
		"  if 'probe' in key: open(cached,'w').close()\n" +
		"print(json.dumps(json.load(open(os.path.join(home,'replies.json'))).get(mode,{'error':'unexpected'})))\n"
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	s.ready = true
	return s, project, &codexFakeGuest{t: t, home: home}
}

func (g *codexFakeGuest) reply(mode string, value any) {
	g.t.Helper()
	replies := map[string]any{}
	if data, err := os.ReadFile(filepath.Join(g.home, "replies.json")); err == nil {
		_ = json.Unmarshal(data, &replies)
	}
	replies[mode] = value
	data, _ := json.Marshal(replies)
	if err := os.WriteFile(filepath.Join(g.home, "replies.json"), data, 0600); err != nil {
		g.t.Fatal(err)
	}
}

func (g *codexFakeGuest) calls(mode string) []map[string]any {
	g.t.Helper()
	data, _ := os.ReadFile(filepath.Join(g.home, "calls.jsonl"))
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call codexGuestCall
		if json.Unmarshal([]byte(line), &call) == nil && call.Mode == mode {
			found = append(found, call.Request)
		}
	}
	return found
}

func codexSessions(t *testing.T, s *Server) ([]mobileSummary, string) {
	t.Helper()
	w := request(s, "GET", "/api/mobile/cli/sessions", "", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var listing struct {
		Sessions []mobileSummary `json:"sessions"`
		Notice   string          `json:"notice"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	return listing.Sessions, listing.Notice
}

func TestCodexSessionsDiscoveryViewAndMessages(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	root := guestPath(project)
	thread := "0199a213-81c0-7800-8aa1-bbab2a035a53"
	guest.reply("discover", []map[string]any{
		{"thread_id": thread, "account": "default", "workspace": root + "/service", "title": "Codex 标题", "updated_at": 1763600000.5, "running": true, "owner": "external"},
		{"thread_id": uuid.NewString(), "account": "default", "workspace": "/elsewhere", "title": "outside"},
	})
	sessions, notice := codexSessions(t, s)
	if notice != "" || len(sessions) != 1 || sessions[0].Provider != codexProvider || sessions[0].Title != "Codex 标题" || !sessions[0].Running || sessions[0].Workspace != project {
		t.Fatalf("Codex discovery: %#v %q", sessions, notice)
	}
	if again, _ := codexSessions(t, s); len(again) != 1 || again[0].ID != sessions[0].ID {
		t.Fatal("rediscovery duplicated the session", again)
	}
	// The desktop sidebar cannot open Codex sessions, so it does not list them.
	if w := request(s, "GET", "/api/cli/sessions", "", "", true); w.Code != 200 || strings.Contains(w.Body.String(), sessions[0].ID) {
		t.Fatalf("desktop listing: %d %s", w.Code, w.Body.String())
	}
	if calls := guest.calls("discover"); len(calls) != 2 || fmt.Sprint(calls[0]["accounts"]) != "[default]" || fmt.Sprint(calls[0]["workspaces"]) != "["+root+"]" {
		t.Fatal("discovery request", calls)
	}
	id := sessions[0].ID
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil || record.CLIID != thread || !record.Resume || !record.Terminal || record.CodexAccount != "" || record.GuestWorkspace != root+"/service" || record.Connection != "" {
		t.Fatalf("discovered record: %#v %v", record, err)
	}

	turn := map[string]any{"id": uuid.NewString(), "prompt": "看看这个项目", "text": "好的", "status": "completed",
		"tools": []map[string]string{{"id": "call-1", "name": "exec_command", "detail": "ls", "status": "completed"}}}
	view := map[string]any{"thread_id": thread, "account": "default", "workspace": root + "/service", "title": "Codex 标题", "updated_at": 1763600001.0,
		"turns": []any{turn}, "running": false, "active": false, "can_send": true}
	guest.reply("view", view)
	path := "/api/mobile/cli/sessions/" + id
	w := request(s, "GET", path, "", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var session mobileSession
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Provider != codexProvider || len(session.Turns) != 1 || session.Turns[0].Text != "好的" || len(session.Turns[0].Tools) != 1 || session.Turns[0].Tools[0].Detail != "ls" ||
		session.CanSend == nil || !*session.CanSend || session.TerminalAvailable {
		t.Fatalf("Codex view: %s", w.Body.String())
	}
	if w := request(s, "GET", fmt.Sprintf("%s?after=%d&epoch=%s", path, session.Revision, session.Generation), "", "", true); w.Code != http.StatusNoContent {
		t.Fatal("unchanged view was resent", w.Code)
	}
	if calls := guest.calls("view"); calls[0]["thread"] != thread || calls[0]["account"] != "default" || calls[0]["workspace"] != root {
		t.Fatal("view request", calls[0])
	}

	guest.reply("send", map[string]any{"ok": true})
	requestID := uuid.NewString()
	body, _ := json.Marshal(map[string]string{"request_id": requestID, "text": "继续"})
	w = request(s, "POST", path+"/messages", s.origin, string(body), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt mobileSession
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	if receipt.AcknowledgedRequestID != requestID || receipt.Turns[len(receipt.Turns)-1].Status != "queued" || receipt.Turns[len(receipt.Turns)-1].Prompt != "继续" {
		t.Fatalf("send receipt: %s", w.Body.String())
	}
	record, _ = s.mobileCLI.record(s.profile, id)
	sent := guest.calls("send")
	if len(sent) != 1 || !mobileID(record.Connection) || sent[0]["connection"] != record.Connection || sent[0]["thread"] != thread || sent[0]["text"] != "继续" ||
		sent[0]["cwd"] != root+"/service" || sent[0]["account"] != "default" || sent[0]["subject"] != "" || fmt.Sprint(sent[0]["additional_directories"]) != "[]" {
		t.Fatalf("send request: %#v", sent)
	}
	busy := "这段会话正在电脑上的 Codex 终端中打开。请先退出该终端中的 Codex，再从手机继续。"
	guest.reply("send", map[string]any{"error": busy})
	body, _ = json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": "再来"})
	if w := request(s, "POST", path+"/messages", s.origin, string(body), true); w.Code != 409 || !strings.Contains(w.Body.String(), busy) {
		t.Fatal("interactive terminal did not refuse the message", w.Code, w.Body.String())
	}

	view["turns"] = []any{turn, map[string]any{"id": requestID, "prompt": "继续", "text": "", "status": "running"}}
	view["running"], view["active"], view["can_send"], view["owner"] = true, true, false, "phone"
	guest.reply("view", view)
	guest.reply("stop", map[string]any{"ok": true})
	body, _ = json.Marshal(map[string]string{"request_id": requestID})
	if w := request(s, "POST", path+"/stop", s.origin, string(body), true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if stops := guest.calls("stop"); len(stops) != 1 || stops[0]["request_id"] != requestID || stops[0]["connection"] != record.Connection {
		t.Fatal("stop request", stops)
	}
	for route, body := range map[string]string{"/approval": `{"request_id":"` + requestID + `","approval_id":"` + uuid.NewString() + `","allow":true}`, "/open": `{}`, "/account": `{"action":"switch","confirm":true}`} {
		if w := request(s, "POST", path+route, s.origin, body, true); w.Code != 409 {
			t.Fatal("Codex session accepted", route, w.Code, w.Body.String())
		}
	}
	w = request(s, "POST", path+"/account", s.origin, `{"action":"inspect"}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"supported":false`) {
		t.Fatal("account inspect", w.Code, w.Body.String())
	}

	view["workspace"] = "/elsewhere"
	guest.reply("view", view)
	if w := request(s, "GET", path, "", "", true); w.Code != 503 {
		t.Fatal("view outside the project accepted", w.Code)
	}
	guest.reply("purge", map[string]any{"ok": true})
	if w := request(s, "POST", path+"/archive", s.origin, `{}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(s, "POST", path+"/purge", s.origin, `{"confirm":true}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if purges := guest.calls("purge"); len(purges) != 1 || purges[0]["thread"] != thread || purges[0]["workspace"] != root || purges[0]["connection"] != record.Connection {
		t.Fatal("purge request", purges)
	}
	if sessions, _ := codexSessions(t, s); len(sessions) != 0 {
		t.Fatal("purged Codex session rediscovered", sessions)
	}
}

func TestCodexNewSessionUsesAllocatedAccountAndBindsItsThread(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	account := codexAccount{ID: strings.Repeat("ab", 12), Label: "work", Enabled: true, Status: "ready", Email: "codex@example.com"}
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{account}, Scheduling: (claudeScheduling{}).defaults()}); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	create := func(provider string) *httpResponse {
		body, _ := json.Marshal(map[string]string{"id": id, "workspace": project, "provider": provider})
		w := request(s, "POST", "/api/mobile/cli/sessions", s.origin, string(body), true)
		return &httpResponse{w.Code, w.Body.String()}
	}
	created := create(codexProvider)
	var session mobileSession
	if created.code != 200 || json.Unmarshal([]byte(created.body), &session) != nil || session.ID != id || session.Provider != codexProvider ||
		len(session.Turns) != 0 || session.CanSend == nil || !*session.CanSend || session.TerminalAvailable {
		t.Fatal("new Codex session", created)
	}
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil || record.CodexAccount != account.ID || record.Resume || !mobileID(record.Connection) || record.GuestWorkspace != guestPath(project) {
		t.Fatalf("new Codex record: %#v %v", record, err)
	}
	if retried := create(codexProvider); retried.code != 200 {
		t.Fatal("retried creation", retried)
	}
	if claude := create(""); claude.code != 409 {
		t.Fatal("Claude creation reused a Codex session", claude)
	}
	if other := create("other"); other.code != 400 {
		t.Fatal("unknown provider", other)
	}
	if len(s.terminals.snapshot()) != 0 {
		t.Fatal("a new Codex session started a terminal")
	}

	guest.reply("view", map[string]any{"thread_id": "", "turns": []any{}, "can_send": true})
	if w := request(s, "GET", "/api/mobile/cli/sessions/"+id, "", "", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	guest.reply("send", map[string]any{"ok": true})
	body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": "开始"})
	if w := request(s, "POST", "/api/mobile/cli/sessions/"+id+"/messages", s.origin, string(body), true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sent := guest.calls("send")
	if len(sent) != 1 || sent[0]["thread"] != "" || sent[0]["account"] != account.ID || sent[0]["subject"] != account.subject() || sent[0]["connection"] != record.Connection {
		t.Fatalf("first message: %#v", sent)
	}

	thread := "0199a213-81c0-7800-8aa1-bbab2a035a54"
	guest.reply("discover", []map[string]any{{"thread_id": thread, "account": account.ID, "connection": record.Connection, "workspace": guestPath(project), "title": "开始", "running": true, "active": true, "owner": "phone"}})
	sessions, _ := codexSessions(t, s)
	if len(sessions) != 1 || sessions[0].ID != id || !sessions[0].Running || !sessions[0].Active || sessions[0].Title != "开始" {
		t.Fatal("first turn did not bind the new session", sessions)
	}
	if calls := guest.calls("discover"); fmt.Sprint(calls[0]["accounts"]) != "[default "+account.ID+"]" {
		t.Fatal("account homes not searched", calls)
	}
	record, _ = s.mobileCLI.record(s.profile, id)
	if record.CLIID != thread || !record.Resume {
		t.Fatalf("thread not bound: %#v", record)
	}
	ctx := context.WithValue(context.Background(), accountEpochKey{}, mustEpoch(t, s))
	if err := s.discoverCodexConversations(ctx, []string{project}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.mobileCLI.summaries(s.profile, map[string]bool{project: true}, false); len(list) != 1 {
		t.Fatal("rediscovery duplicated the bound session", list)
	}
}

func TestCodexSessionBlocksProjectRemovalWhileRunning(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"tabs":[]}`))}, nil
	}))
	session := map[string]any{"thread_id": uuid.NewString(), "account": "default", "workspace": guestPath(project), "title": "busy", "running": true, "active": true, "owner": "phone"}
	guest.reply("discover", []any{session})
	if sessions, _ := codexSessions(t, s); len(sessions) != 1 || !sessions[0].Running {
		t.Fatal("running Codex session not listed", sessions)
	}
	removeProjectRequest(t, s, project, 409)
	session["running"], session["active"], session["owner"] = false, false, ""
	guest.reply("discover", []any{session})
	removeProjectRequest(t, s, project, 200)
}

func TestCodexSessionsBootstrapRunsShippedHelper(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	s := testServer(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin"})
	var found []codexConversation
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.runCodexSessions(ctx, "discover", map[string]any{"accounts": []string{"default"}, "workspaces": []string{"/project"}}, &found); err != nil || len(found) != 0 {
		t.Fatal("shipped helper discovery", found, err)
	}
	installed, err := os.ReadFile(filepath.Join(home, ".local/share/cage/codex-session-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(codexSessionsHelper)))))
	if err != nil || string(installed) != codexSessionsHelper {
		t.Fatal("helper was not installed verbatim", err)
	}
	var failure codexConversation
	if err := s.runCodexSessions(ctx, "view", map[string]any{"thread": "not-a-thread", "account": "default"}, &failure); err != nil || failure.Error == "" {
		t.Fatal("invalid request did not return a message", failure, err)
	}
}

// Every owner state keeps terminal_available false: shipped phones open a
// desktop terminal on their own whenever it is true and can_send is false.
func TestCodexOwnerStatesKeepTheTerminalUnavailable(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	root := guestPath(project)
	thread := uuid.NewString()
	item := map[string]any{"thread_id": thread, "account": "default", "workspace": root, "title": "owned", "updated_at": 1763600000.0}
	view := map[string]any{"thread_id": thread, "account": "default", "workspace": root, "turns": []any{}, "can_send": true}
	for _, tc := range []struct {
		guest string
		bound bool
		want  string
	}{{"", false, ""}, {"phone", false, "phone"}, {"external", false, "external"}, {"desktop", false, "external"}, {"", true, "desktop"}, {"phone", true, "desktop"}} {
		item["owner"], item["running"], item["active"] = tc.guest, tc.guest != "", tc.guest != ""
		view["owner"], view["can_send"], view["running"], view["active"] = tc.guest, tc.guest == "", tc.guest != "", tc.guest != ""
		view["connection_notice"] = map[string]string{"desktop": "这段会话正在电脑的 Codex 终端中打开，手机上可以查看和停止本轮。"}[tc.guest]
		guest.reply("discover", []any{item})
		guest.reply("view", view)
		var terminal *terminalSession
		if tc.bound {
			sessions, _ := codexSessions(t, s)
			var err error
			if terminal, err = s.terminals.startCodexSession(terminalChildCommand("echo"), project, "codex", codexSelection{}, nil); err != nil {
				t.Fatal(err)
			}
			terminal.mu.Lock()
			terminal.sharedID = sessions[0].ID
			terminal.mu.Unlock()
		}
		sessions, _ := codexSessions(t, s)
		if len(sessions) != 1 || sessions[0].Owner != tc.want || sessions[0].Running != (tc.want != "") || sessions[0].Active != (tc.guest != "" && tc.want != "") {
			t.Fatalf("%+v: listed %+v", tc, sessions)
		}
		w := request(s, "GET", "/api/mobile/cli/sessions/"+sessions[0].ID, "", "", true)
		var session mobileSession
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &session) != nil || strings.Contains(w.Body.String(), "terminal_available") || session.TerminalAvailable {
			t.Fatalf("%+v: view %d %s", tc, w.Code, w.Body.String())
		}
		if session.Owner != tc.want || session.CanSend == nil || *session.CanSend != (tc.want == "") || session.Running != (tc.want != "") {
			t.Fatalf("%+v: view %s", tc, w.Body.String())
		}
		if tc.want == "desktop" && session.ConnectionNotice != codexDesktopNotice {
			t.Fatalf("%+v: notice %q", tc, session.ConnectionNotice)
		}
		// A lease with no terminal here must not promise a desktop stop.
		if tc.guest == "desktop" && !tc.bound && session.ConnectionNotice != codexExternalNotice {
			t.Fatalf("%+v: notice %q", tc, session.ConnectionNotice)
		}
		if terminal != nil {
			if summary := terminal.summary(); summary["cli_session_provider"] != codexProvider || summary["cli_session_id"] != sessions[0].ID {
				t.Fatal("bound terminal summary", summary)
			}
			// The server's own terminal refuses the phone before the guest is asked.
			guest.reply("send", map[string]any{"ok": true})
			body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": "从手机"})
			if w := request(s, "POST", "/api/mobile/cli/sessions/"+sessions[0].ID+"/messages", s.origin, string(body), true); w.Code != 409 || !strings.Contains(w.Body.String(), "电脑的 Codex 终端") {
				t.Fatal("phone message while the desktop owns the thread", w.Code, w.Body.String())
			}
			if sent := guest.calls("send"); len(sent) != 0 {
				t.Fatal("the guest was asked to send", sent)
			}
			s.terminals.remove(terminal.id)
		}
	}
	plain, err := s.terminals.startCodexSession(terminalChildCommand("echo"), project, "codex", codexSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.summary()["cli_session_provider"]; ok {
		t.Fatal("a plain Codex terminal claims a session")
	}
}

func desktopCodexList(t *testing.T, s *Server, query string) (sessionListAnswer, int) {
	t.Helper()
	w := request(s, "GET", "/api/cli/sessions"+query, "", "", true)
	var answer sessionListAnswer
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
	}
	return answer, w.Code
}

// The desktop lists Codex rows only when it asks for them; without the
// parameter the answer and the guest work are what they were.
func TestCodexDesktopListingIsOptIn(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	root := guestPath(project)
	thread := uuid.NewString()
	guest.reply("discover", []map[string]any{{"thread_id": thread, "account": "default", "workspace": root, "title": "桌面可见", "updated_at": 1763600000.0, "owner": ""}})
	plain := request(s, "GET", "/api/cli/sessions", "", "", true)
	if plain.Code != 200 || strings.Contains(plain.Body.String(), "capabilities") || strings.Contains(plain.Body.String(), codexProvider) {
		t.Fatal("desktop listing without providers", plain.Code, plain.Body.String())
	}
	if calls := guest.calls("discover"); len(calls) != 0 {
		t.Fatal("a desktop listing without providers ran Codex discovery", calls)
	}
	if again := request(s, "GET", "/api/cli/sessions", "", "", true); again.Body.String() != plain.Body.String() {
		t.Fatal("the plain answer changed", again.Body.String())
	}
	for _, bad := range []string{"?providers=", "?providers=claude,gemini", "?providers=claude&providers=codex"} {
		if _, code := desktopCodexList(t, s, bad); code != 400 {
			t.Fatal("accepted", bad, code)
		}
	}
	answer, code := desktopCodexList(t, s, "?providers=claude,codex")
	if code != 200 || len(answer.Sessions) != 1 || answer.Sessions[0].Provider != codexProvider || answer.Sessions[0].Title != "桌面可见" ||
		answer.Capabilities == nil || !answer.Capabilities.CodexSessions || answer.Capabilities.CodexCLI != nil {
		t.Fatalf("opted-in listing: %d %+v", code, answer)
	}
	if calls := guest.calls("discover"); len(calls) != 1 {
		t.Fatal("one Codex discovery per listing", calls)
	}
	// The phone keeps listing both, without the desktop's capabilities.
	if w := request(s, "GET", "/api/mobile/cli/sessions", "", "", true); w.Code != 200 || strings.Contains(w.Body.String(), "capabilities") || !strings.Contains(w.Body.String(), thread[:0]+answer.Sessions[0].ID) {
		t.Fatal("phone listing", w.Code, w.Body.String())
	}
	// A custom Codex command keeps plain terminals.
	s.preferences.CodexTerminalCommand = "codex --search"
	if answer, _ = desktopCodexList(t, s, "?providers=claude,codex"); answer.Capabilities.CodexSessions {
		t.Fatal("a custom command offers sessions")
	}
	s.preferences.CodexTerminalCommand = ""

	// A desktop session whose Codex closed before making a thread is hidden
	// on the desktop until it has a terminal again.
	placeholder := uuid.NewString()
	if _, err := s.mobileCLI.createProvider(s.profile, project, placeholder, codexProvider, func(record *mobileRecord) {
		record.Terminal, record.DesktopMenu, record.Connection, record.GuestWorkspace = true, true, uuid.NewString(), root
	}); err != nil {
		t.Fatal(err)
	}
	listed := func(query string) bool {
		answer, _ := desktopCodexList(t, s, query)
		return slices.ContainsFunc(answer.Sessions, func(row mobileSummary) bool { return row.ID == placeholder })
	}
	if listed("?providers=claude,codex") {
		t.Fatal("placeholder listed on the desktop")
	}
	// The phone hides it too, but keeps a new session of its own.
	mine := uuid.NewString()
	if w := request(s, "POST", "/api/mobile/cli/sessions", s.origin, `{"id":"`+mine+`","workspace":"`+project+`","provider":"codex"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if sessions, _ := codexSessions(t, s); slices.ContainsFunc(sessions, func(row mobileSummary) bool { return row.ID == placeholder }) ||
		!slices.ContainsFunc(sessions, func(row mobileSummary) bool { return row.ID == mine }) {
		t.Fatal("phone placeholders", sessions)
	}
	terminal, err := s.terminals.startCodexSession(terminalChildCommand("echo"), project, "codex", codexSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal.mu.Lock()
	terminal.sharedID = placeholder
	terminal.mu.Unlock()
	if !listed("?providers=claude,codex") {
		t.Fatal("a session with a live terminal is hidden")
	}
	// A held list answers when the server's own terminal ends.
	answer, _ = desktopCodexList(t, s, "?providers=claude,codex")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- request(s, "GET", "/api/cli/sessions?providers=claude,codex&after="+answer.Revision+"&wait=10", "", "", true)
	}()
	time.Sleep(300 * time.Millisecond)
	started := time.Now()
	s.terminals.remove(terminal.id)
	select {
	case w := <-done:
		if w.Code != 200 || strings.Contains(w.Body.String(), placeholder) || time.Since(started) > 3*time.Second {
			t.Fatal("held list", w.Code, time.Since(started), w.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held list did not answer")
	}
}

func waitCodexCalls(t *testing.T, guest *codexFakeGuest, mode string, count int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		calls := guest.calls(mode)
		if len(calls) >= count {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d %s calls, want %d", len(calls), mode, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func listedCodexSession(t *testing.T, s *Server, guest *codexFakeGuest, project, title string) mobileSummary {
	t.Helper()
	thread := uuid.NewString()
	guest.reply("discover", []map[string]any{{"thread_id": thread, "account": "default", "workspace": guestPath(project), "title": title, "updated_at": 1763600000.0, "owner": ""}})
	answer, _ := desktopCodexList(t, s, "?providers=claude,codex")
	for _, row := range answer.Sessions {
		if row.Title == title {
			return row
		}
	}
	t.Fatal("session not listed", answer)
	return mobileSummary{}
}

// A Codex row opens on the desktop as a Codex terminal bound to the session.
func TestCodexDesktopOpenBindsATerminal(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	row := listedCodexSession(t, s, guest, project, "打开我")
	record, _ := s.mobileCLI.record(s.profile, row.ID)
	caps := map[string]any{"installed": true, "complete": true, "version": "0.171.0", "resume": true, "resume_no_daemon": true}
	guest.reply("view", map[string]any{"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "turns": []any{}, "can_send": true, "owner": "",
		"cli": caps})
	guest.reply("probe", caps)
	path := "/api/cli/sessions/" + row.ID + "/open"
	w := request(s, "POST", path, s.origin, "{}", true)
	var opened mobileSession
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &opened) != nil || opened.DesktopTerminalID == "" || opened.Owner != "desktop" || opened.TerminalAvailable {
		t.Fatal("open", w.Code, w.Body.String())
	}
	terminal := s.terminals.get(opened.DesktopTerminalID)
	if summary := terminal.summary(); summary["kind"] != "codex" || summary["cli_session_id"] != row.ID || summary["cli_session_provider"] != codexProvider {
		t.Fatal("terminal", summary)
	}
	if views := guest.calls("view"); len(views) != 1 || views[0]["probe"] != true || views[0]["thread"] != record.CLIID {
		t.Fatal("the guest was not asked who owns the thread", views)
	}
	record, _ = s.mobileCLI.record(s.profile, row.ID)
	launch := waitCodexCalls(t, guest, "tui-launch", 1)[0]
	if launch["connection"] != record.Connection || !mobileID(record.Connection) || launch["lease"] != record.CodexLease || launch["thread"] != record.CLIID ||
		launch["account"] != "default" || launch["cwd"] != guestPath(project) || record.TerminalID != terminal.id {
		t.Fatalf("launch %#v record %#v", launch, record)
	}
	if answer, _ := desktopCodexList(t, s, "?providers=claude,codex"); answer.Capabilities.CodexCLI == nil || answer.Capabilities.CodexCLI.Version != "0.171.0" {
		t.Fatal("probe not reported", answer.Capabilities)
	}
	// Opening again shows the same terminal.
	if again := request(s, "POST", path, s.origin, "{}", true); again.Code != 200 || !strings.Contains(again.Body.String(), terminal.id) || len(s.terminals.snapshot()) != 1 {
		t.Fatal("second open", again.Code, again.Body.String())
	}
	if views := guest.calls("view"); len(views) != 1 {
		t.Fatal("a second open asked the guest again", views)
	}
	// The phone route still refuses, and the phone cannot write meanwhile.
	if w := request(s, "POST", "/api/mobile/cli/sessions/"+row.ID+"/open", s.origin, "{}", true); w.Code != 409 {
		t.Fatal("phone open", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": "手机"})
	if w := request(s, "POST", "/api/mobile/cli/sessions/"+row.ID+"/messages", s.origin, string(body), true); w.Code != 409 || len(guest.calls("send")) != 0 {
		t.Fatal("phone message", w.Code, w.Body.String())
	}

	// Refusals answer the open instead of starting a terminal.
	for owner, want := range map[string]error{"phone": errCodexPhoneRunning, "external": errCodexOtherProcess, "desktop": errCodexOtherTerminal} {
		other := listedCodexSession(t, s, guest, project, "被占用 "+owner)
		guest.reply("view", map[string]any{"thread_id": "", "turns": []any{}, "owner": owner, "running": true})
		if w := request(s, "POST", "/api/cli/sessions/"+other.ID+"/open", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), want.Error()) {
			t.Fatal(owner, w.Code, w.Body.String())
		}
	}
	if len(s.terminals.snapshot()) != 1 {
		t.Fatal("a refused open started a terminal")
	}
	guest.reply("view", map[string]any{"thread_id": "", "turns": []any{}, "can_send": true})
	managed := listedCodexSession(t, s, guest, project, "账号")
	s.mobileCLI.mu.Lock()
	pinned, _ := s.mobileCLI.record(s.profile, managed.ID)
	pinned.CodexAccount = strings.Repeat("ab", 12)
	_ = writePrivate(mobileRecordPath(s.profile, pinned.ID), pinned)
	s.mobileCLI.mu.Unlock()
	if w := request(s, "POST", "/api/cli/sessions/"+managed.ID+"/open", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "已移除") {
		t.Fatal("removed account", w.Code, w.Body.String())
	}
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{{ID: pinned.CodexAccount, Enabled: true, Status: "signed_out"}}, Scheduling: (claudeScheduling{}).defaults()}); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "POST", "/api/cli/sessions/"+managed.ID+"/open", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "尚未登录") {
		t.Fatal("signed-out account", w.Code, w.Body.String())
	}
	s.preferences.CodexTerminalCommand = "codex --search"
	if w := request(s, "POST", "/api/cli/sessions/"+managed.ID+"/open", s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), errCodexCommand.Error()) {
		t.Fatal("custom command", w.Code, w.Body.String())
	}
	s.preferences.CodexTerminalCommand = ""
	if w := request(s, "POST", "/api/cli/sessions/"+managed.ID+"/archive", s.origin, "{}", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/cli/sessions/"+managed.ID+"/open", s.origin, "{}", true); w.Code != http.StatusGone {
		t.Fatal("archived", w.Code, w.Body.String())
	}
}

// History the helper cannot show keeps the phone read-only but still opens on
// the desktop, where Codex itself resumes it; missing history does not open.
func TestCodexOpenResumesCompressedHistory(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	row := listedCodexSession(t, s, guest, project, "压缩历史")
	record, _ := s.mobileCLI.record(s.profile, row.ID)
	cli := map[string]any{"installed": true, "complete": true, "version": "0.171.0", "resume": true, "resume_no_daemon": true}
	guest.reply("view", map[string]any{"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "turns": []any{}, "can_send": false, "owner": "",
		"connection_notice": "未找到这段 Codex 会话的历史，可能已在电脑上删除。", "cli": cli})
	path := "/api/cli/sessions/" + row.ID + "/open"
	if w := request(s, "POST", path, s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "未找到") || len(s.terminals.snapshot()) != 0 {
		t.Fatal("missing history", w.Code, w.Body.String())
	}
	guest.reply("view", map[string]any{"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "turns": []any{}, "can_send": false, "owner": "",
		"connection_notice": "这段会话的历史由较新的 Codex 压缩保存，暂时无法在这里显示，可在电脑终端中继续。", "history_compressed": true, "cli": cli})
	if w := request(s, "POST", path, s.origin, "{}", true); w.Code != 200 {
		t.Fatal("compressed history", w.Code, w.Body.String())
	}
	if launch := waitCodexCalls(t, guest, "tui-launch", 1)[0]; launch["thread"] != record.CLIID {
		t.Fatal("launch", launch)
	}
}

// The menu's Codex entry makes a session for a client that lists them.
func TestCodexMenuTerminalBecomesASession(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	body, _ := json.Marshal(map[string]string{"kind": "codex", "workspace": project})
	open := func(query string) map[string]any {
		w := request(s, "POST", "/api/terminals"+query, s.origin, string(body), true)
		var summary map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &summary) != nil {
			t.Fatal(query, w.Code, w.Body.String())
		}
		return summary
	}
	if plain := open(""); plain["cli_session_id"] != "" || plain["cli_session_provider"] != nil {
		t.Fatal("an older client got a session", plain)
	}
	session := open("?providers=claude,codex")
	id, _ := session["cli_session_id"].(string)
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil || session["cli_session_provider"] != codexProvider || !record.codex() || !record.Terminal || record.Resume || !mobileID(record.Connection) ||
		record.TerminalID != session["id"] || record.CodexLease == "" || record.GuestWorkspace != guestPath(project) {
		t.Fatalf("menu session %v %#v %v", session, record, err)
	}
	launch := waitCodexCalls(t, guest, "tui-launch", 1)[0]
	if launch["thread"] != "" || launch["connection"] != record.Connection || launch["lease"] != record.CodexLease {
		t.Fatal("launch", launch)
	}
	if views := guest.calls("view"); len(views) != 0 {
		t.Fatal("a new session asked the guest first", views)
	}
	if answer, _ := desktopCodexList(t, s, "?providers=claude,codex"); !slices.ContainsFunc(answer.Sessions, func(row mobileSummary) bool { return row.ID == id && row.Owner == "desktop" }) {
		t.Fatal("new session not listed as the desktop's", answer.Sessions)
	}
	s.preferences.CodexTerminalCommand = "codex --search"
	if custom := open("?providers=claude,codex"); custom["cli_session_id"] != "" {
		t.Fatal("a custom command made a session", custom)
	}
	if w := request(s, "POST", "/api/terminals?providers=gemini", s.origin, string(body), true); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

// The terminal script execs all the way into the helper, so the terminal's
// process is the helper's (later Codex's) and its status is theirs.
func TestCodexSessionScriptExecsIntoTheHelper(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(t.TempDir(), "project with ' quote")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(home, ".local/share/cage/codex-session-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(codexSessionsHelper))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	stand := "import json,os,sys\nopen(os.path.join(os.environ['HOME'],'argv'),'w').write(json.dumps({'argv':sys.argv[1:],'pid':os.getpid(),'cwd':os.getcwd()}))\nsys.exit(json.loads(sys.argv[2])['exit'])\n"
	if err := os.WriteFile(helper, []byte(stand), 0600); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{0, 75, 78} {
		payload := fmt.Sprintf("{\"exit\":%d,\"cwd\":\"it's\"}", code)
		script := codexSessionTerminalScript(work, payload)
		if len(script) > 96<<10 {
			t.Fatalf("terminal script uses %d bytes", len(script))
		}
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		cmd.Stdin = strings.NewReader("printf shell > shell-result\nexit\n")
		err := cmd.Run()
		var exit *exec.ExitError
		if code != 0 && (!errors.As(err, &exit) || exit.ExitCode() != code) || code == 0 && err != nil {
			t.Fatalf("status %d: %v", code, err)
		}
		var seen struct {
			Argv []string
			PID  int
			Cwd  string
		}
		data, _ := os.ReadFile(filepath.Join(home, "argv"))
		if json.Unmarshal(data, &seen) != nil || len(seen.Argv) != 2 || seen.Argv[0] != "tui-launch" || seen.Argv[1] != payload || seen.PID != cmd.Process.Pid {
			t.Fatalf("helper saw %s (shell %d)", data, cmd.Process.Pid)
		}
		if resolved, _ := filepath.EvalSymlinks(seen.Cwd); resolved != mustEval(t, work) {
			t.Fatal("cwd", seen.Cwd)
		}
		if _, err := os.Stat(filepath.Join(work, "shell-result")); !os.IsNotExist(err) {
			t.Fatal("a shell ran after the helper")
		}
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Stop reaches whoever runs the turn: Esc into the desktop's Codex, the
// helper's stop for a phone turn.
func TestCodexStopReachesTheTurnsOwner(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	row := listedCodexSession(t, s, guest, project, "停止")
	record, _ := s.mobileCLI.record(s.profile, row.ID)
	if err := s.codexConnection(record); err != nil {
		t.Fatal(err)
	}
	turn := uuid.NewString()
	running := map[string]any{"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "running": true, "active": true, "can_send": false,
		"turns": []any{map[string]any{"id": turn, "prompt": "长任务", "text": "", "status": "running"}}}
	stop := func(prefix, id string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"request_id": id})
		return request(s, "POST", prefix+row.ID+"/stop", s.origin, string(body), true)
	}
	// A phone turn: the helper stops it; the desktop route reaches it too.
	running["owner"] = "phone"
	guest.reply("view", running)
	guest.reply("stop", map[string]any{"ok": true})
	if w := stop("/api/cli/sessions/", turn); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if stops := guest.calls("stop"); len(stops) != 1 || stops[0]["request_id"] != turn {
		t.Fatal("helper stop", stops)
	}
	if w := stop("/api/cli/sessions/", uuid.NewString()); w.Code != 409 {
		t.Fatal("stale turn", w.Code, w.Body.String())
	}
	running["owner"] = "external"
	guest.reply("view", running)
	if w := stop("/api/mobile/cli/sessions/", turn); w.Code != 409 || !strings.Contains(w.Body.String(), "运行它的终端") || len(guest.calls("stop")) != 1 {
		t.Fatal("external turn", w.Code, w.Body.String())
	}
	// The desktop's Codex: Esc into its terminal.
	running["owner"] = "desktop"
	guest.reply("view", running)
	terminal, err := s.terminals.startCodexSession(terminalChildCommand("echo"), project, "codex", codexSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal.mu.Lock()
	terminal.sharedID = row.ID
	terminal.mu.Unlock()
	view := terminal.subscribe()
	defer view.Close()
	readTerminalUntil(t, view, "terminal-ready")
	if w := stop("/api/mobile/cli/sessions/", turn); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(guest.calls("stop")) != 1 {
		t.Fatal("the helper was asked to stop the desktop's turn")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		terminal.mu.Lock()
		history := string(terminal.history)
		terminal.mu.Unlock()
		if strings.Contains(history, "^[") || strings.Contains(history, "\x1b") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Esc did not reach the terminal: %q", history)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The phone's Stop on the desktop's Codex sends one Esc per running turn:
// repeated and concurrent Stops before the interrupt reaches the transcript
// (the turn still reads as running) send none, since a second Esc on an idle
// composer opens "edit previous message". A turn that is not running gets
// the existing answer and no Esc.
func TestCodexStopSendsOneEscPerRunningTurn(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	row := listedCodexSession(t, s, guest, project, "停止一次")
	record, _ := s.mobileCLI.record(s.profile, row.ID)
	if err := s.codexConnection(record); err != nil {
		t.Fatal(err)
	}
	turn := uuid.NewString()
	running := map[string]any{"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "running": true, "active": true,
		"can_send": false, "owner": "desktop", "turns": []any{map[string]any{"id": turn, "prompt": "长任务", "text": "", "status": "running"}}}
	guest.reply("view", running)
	terminal := boundCodexTerminal(t, s, project, row.ID)
	view := terminal.subscribe()
	defer view.Close()
	readTerminalUntil(t, view, "terminal-ready")
	stop := func(id string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"request_id": id})
		return request(s, "POST", "/api/mobile/cli/sessions/"+row.ID+"/stop", s.origin, string(body), true)
	}
	escapes := func(mark string) int { return terminalEscapes(t, terminal, view, mark) }
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- stop(turn).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatal("stop", code)
		}
	}
	if n := escapes("first"); n != 1 {
		t.Fatalf("%d Esc for one running turn", n)
	}
	// After the repeat window the turn still reads as running: the first Esc
	// did not stop it, so Stop sends one again.
	old := terminalStopRepeat
	terminalStopRepeat = 0
	defer func() { terminalStopRepeat = old }()
	if w := stop(turn); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if n := escapes("second"); n != 1 {
		t.Fatalf("%d Esc after the repeat window", n)
	}
	// A finished turn, or another one: the existing answer and no Esc.
	running["turns"] = []any{map[string]any{"id": turn, "prompt": "长任务", "text": "好了", "status": "completed"}}
	guest.reply("view", running)
	for _, id := range []string{turn, uuid.NewString()} {
		if w := stop(id); w.Code != 409 || !strings.Contains(w.Body.String(), "本轮运行状态已变化") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if n := escapes("third"); n != 0 {
		t.Fatalf("%d Esc for a turn that is not running", n)
	}
}

// terminalEscapes counts the Esc keys typed into an echo test terminal since
// the last line, by ending the line with mark.
func terminalEscapes(t *testing.T, terminal *terminalSession, view *terminalSubscription, mark string) int {
	t.Helper()
	if err := terminal.enqueueInput(context.Background(), terminalInputLine(mark)); err != nil {
		t.Fatal(err)
	}
	echoed := regexp.MustCompile("child:([^\r\n]*)" + mark + "\r?\n")
	text := ""
	for !echoed.MatchString(text) {
		text += readTerminalUntil(t, view, "\n")
	}
	return strings.Count(echoed.FindStringSubmatch(text)[1], "\x1b")
}

// The phone's Stop on a desktop Claude Code terminal follows the same rule:
// one Esc per running turn however many Stops arrive, and each still asks
// the guest to stop the turn.
func TestClaudeTerminalStopSendsOneEscPerRunningTurn(t *testing.T) {
	s, id, project := metadataFixture(t)
	home := t.TempDir()
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	helper := filepath.Join(home, ".local/share/cage/cli-channel-code", fmt.Sprintf("%x.py", sha256.Sum256([]byte(cliChannelHelper+cliResumePrompt+claudeAccountResumePrompt))))
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	code := "import json,os,sys\nrequest=json.load(sys.stdin)\nopen(os.path.join(os.environ['HOME'],'calls.jsonl'),'a').write(json.dumps({'mode':sys.argv[1],'request':request})+'\\n')\n" +
		"print(json.dumps({'ok':True} if sys.argv[1]=='rpc' else {}))\n"
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.terminals.close)
	terminal, err := s.terminals.startSession(terminalChildCommand("echo"), project, "claude", claudeSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mobileCLI.mu.Lock()
	record, err := s.mobileCLI.record(s.profile, id)
	if err == nil {
		record.Terminal, record.Connection, record.GuestWorkspace, record.TerminalID = true, uuid.NewString(), guestPath(project), terminal.id
		err = writePrivate(mobileRecordPath(s.profile, id), record)
	}
	s.mobileCLI.mu.Unlock()
	if err != nil || record.codex() {
		t.Fatal(err, record.codex())
	}
	view := terminal.subscribe()
	defer view.Close()
	readTerminalUntil(t, view, "terminal-ready")
	epoch, ok := s.account.access()
	if !ok {
		t.Fatal("missing unlock epoch")
	}
	ctx := context.WithValue(context.Background(), accountEpochKey{}, epoch)
	turn := uuid.NewString()
	running := mobileSession{mobileSummary: mobileSummary{ID: id}, Turns: []mobileTurn{{ID: turn, Prompt: "长任务", Status: "running"}}}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.stopTerminalConversation(ctx, running, turn)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		// Concurrent stop actions may find the operation lock taken.
		if err != nil && !errors.Is(err, errTerminalBusy) {
			t.Fatal(err)
		}
	}
	if n := terminalEscapes(t, terminal, view, "first"); n != 1 {
		t.Fatalf("%d Esc for one running Claude turn", n)
	}
	data, _ := os.ReadFile(filepath.Join(home, "calls.jsonl"))
	if !strings.Contains(string(data), `"action": "stop"`) {
		t.Fatalf("the guest was not asked to stop: %s", data)
	}
	if _, err := s.stopTerminalConversation(ctx, mobileSession{mobileSummary: mobileSummary{ID: id}, Turns: []mobileTurn{{ID: turn, Status: "completed"}}}, turn); err == nil || !strings.Contains(err.Error(), "本轮运行状态已变化") {
		t.Fatal("finished turn", err)
	}
	if n := terminalEscapes(t, terminal, view, "second"); n != 0 {
		t.Fatalf("%d Esc for a finished Claude turn", n)
	}
}

func (g *codexFakeGuest) delay(delays map[string]float64) {
	g.t.Helper()
	data, _ := json.Marshal(delays)
	if err := os.WriteFile(filepath.Join(g.home, "delays.json"), data, 0600); err != nil {
		g.t.Fatal(err)
	}
}

func (g *codexFakeGuest) await(marks ...string) string {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, mark := range marks {
			if _, err := os.Stat(filepath.Join(g.home, mark)); err == nil {
				return mark
			}
		}
		if time.Now().After(deadline) {
			g.t.Fatal("the guest did not reach", marks)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Reading a Codex CLI's help the first time takes seconds. A desktop open
// has the guest do it before the operation lock, so the lock is free
// meanwhile, and asks for it under the lock only when that answer was
// complete (from the guest's cache).
func TestCodexFirstOpenProbesOutsideTheOperationLock(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	row := listedCodexSession(t, s, guest, project, "首次打开")
	record, _ := s.mobileCLI.record(s.profile, row.ID)
	caps := map[string]any{"installed": true, "complete": true, "version": "0.171.0", "resume": true, "resume_no_daemon": true}
	guest.reply("view", map[string]any{"thread_id": record.CLIID, "account": "default", "workspace": guestPath(project), "turns": []any{}, "can_send": true, "owner": "", "cli": caps})
	guest.reply("probe", caps)
	guest.delay(map[string]float64{"probe": 1, "view-probe": 1})
	opened := make(chan *httptest.ResponseRecorder, 1)
	go func() { opened <- request(s, "POST", "/api/cli/sessions/"+row.ID+"/open", s.origin, "{}", true) }()
	guest.await("started-probe", "started-view-probe")
	if !s.op.TryLock() {
		t.Fatal("the open held the operation lock while the guest read the CLI's help")
	}
	s.op.Unlock()
	if w := <-opened; w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if views := guest.calls("view"); len(views) != 1 || views[0]["probe"] != true {
		t.Fatal("views", views)
	}
	// An incomplete answer: the check under the lock does not probe, and
	// the open goes on (tui-launch checks resume support on its own).
	second := listedCodexSession(t, s, guest, project, "再次打开")
	secondRecord, _ := s.mobileCLI.record(s.profile, second.ID)
	guest.reply("view", map[string]any{"thread_id": secondRecord.CLIID, "account": "default", "workspace": guestPath(project), "turns": []any{}, "can_send": true, "owner": ""})
	guest.reply("probe", map[string]any{"installed": true, "complete": false})
	if w := request(s, "POST", "/api/cli/sessions/"+second.ID+"/open", s.origin, "{}", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if views := guest.calls("view"); len(views) != 2 || views[1]["probe"] != false {
		t.Fatal("views", views)
	}
}

// An archive's guest stop of a phone turn (up to 8 s) runs after the
// operation lock; a restore meanwhile waits for it, so the stop never
// reaches a turn started after the restore.
func TestCodexArchiveStopsThePhoneTurnOutsideTheOperationLock(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	phone := listedCodexSession(t, s, guest, project, "归档停止")
	record, _ := s.mobileCLI.record(s.profile, phone.ID)
	if err := s.codexConnection(record); err != nil {
		t.Fatal(err)
	}
	guest.reply("stop", map[string]any{"ok": true})
	guest.delay(map[string]float64{"stop": 1})
	archived := make(chan *httptest.ResponseRecorder, 1)
	go func() { archived <- request(s, "POST", "/api/cli/sessions/"+phone.ID+"/archive", s.origin, "{}", true) }()
	guest.await("started-stop")
	if !s.op.TryLock() {
		t.Fatal("the archive held the operation lock while the guest stopped the turn")
	}
	s.op.Unlock()
	if record, _ := s.mobileCLI.record(s.profile, phone.ID); !record.Archived {
		t.Fatal("not archived before the guest stop")
	}
	if err := s.setSessionArchived(phone.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(guest.home, "finished-stop")); err != nil {
		t.Fatal("the restore did not wait for the archive's stop")
	}
	if w := <-archived; w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if record, _ := s.mobileCLI.record(s.profile, phone.ID); record.Archived {
		t.Fatal("restore lost")
	}
	s.mobileCLI.mu.Lock()
	pending := len(s.mobileCLI.archiveStops)
	s.mobileCLI.mu.Unlock()
	if pending != 0 {
		t.Fatal("archive stop not cleared")
	}
}

// Session lists poll every 250 ms; the Codex accounts file is read again
// only when it changed.
func TestCodexAccountLabelsFollowTheFile(t *testing.T) {
	s := testServer(t)
	id := strings.Repeat("cd", 12)
	save := func(label string) {
		t.Helper()
		if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{{ID: id, Label: label, Enabled: true, Status: "ready"}}, Scheduling: (claudeScheduling{}).defaults()}); err != nil {
			t.Fatal(err)
		}
	}
	save("工作")
	if labels := s.codexAccountLabels(); labels[id] != "工作" {
		t.Fatal(labels)
	}
	path := filepath.Join(s.profile, "codex-accounts.json")
	if os.Geteuid() != 0 {
		// Unreadable but unchanged: answered from the cache, not read.
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if labels := s.codexAccountLabels(); labels[id] != "工作" {
			t.Fatal("the unchanged file was read again", labels)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save("个人")
	if labels := s.codexAccountLabels(); labels[id] != "个人" {
		t.Fatal("a saved change was not read", labels)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if labels := s.codexAccountLabels(); len(labels) != 0 {
		t.Fatal("labels of a removed file", labels)
	}
}

func boundCodexTerminal(t *testing.T, s *Server, project, id string) *terminalSession {
	t.Helper()
	terminal, err := s.terminals.startCodexSession(terminalChildCommand("echo"), project, "codex", codexSelection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal.mu.Lock()
	terminal.sharedID = id
	terminal.mu.Unlock()
	return terminal
}

// Archiving a Codex session closes its desktop terminal and stops a phone
// turn; the phone cannot close the desktop's terminal.
func TestCodexArchiveClosesTheTerminalAndStopsThePhoneTurn(t *testing.T) {
	s, project, guest := newCodexFixture(t)
	t.Cleanup(s.terminals.close)
	desk := listedCodexSession(t, s, guest, project, "电脑上")
	terminal := boundCodexTerminal(t, s, project, desk.ID)
	for _, path := range []string{"/api/mobile/cli/sessions/" + desk.ID + "/archive", "/api/mobile/cli/sessions/" + desk.ID + "/delete"} {
		if w := request(s, "POST", path, s.origin, "{}", true); w.Code != 409 || !strings.Contains(w.Body.String(), "请在电脑上关闭") {
			t.Fatal("phone archive of the desktop's session", path, w.Code, w.Body.String())
		}
	}
	if w := request(s, "POST", "/api/mobile/terminals/"+terminal.id+"/close", s.origin, "{}", true); w.Code != 409 {
		t.Fatal("phone close of the desktop's terminal", w.Code, w.Body.String())
	}
	if s.terminals.boundCodex(desk.ID) == nil {
		t.Fatal("the phone closed the terminal")
	}
	// Closing the live tab on the desktop archives, as for Claude.
	if err := s.closeTerminalID(terminal.id); err != nil {
		t.Fatal(err)
	}
	if record, _ := s.mobileCLI.record(s.profile, desk.ID); !record.Archived || s.terminals.get(terminal.id) != nil {
		t.Fatal("closing the live tab did not archive", record.Archived)
	}
	if stops := guest.calls("stop"); len(stops) != 0 {
		t.Fatal("a desktop-owned archive stopped a phone turn", stops)
	}
	// What is left of an ended bound terminal only goes away.
	ended := listedCodexSession(t, s, guest, project, "已退出")
	gone := boundCodexTerminal(t, s, project, ended.ID)
	view := gone.subscribe()
	readTerminalUntil(t, view, "terminal-ready")
	view.Close()
	if err := gone.enqueueInput(context.Background(), terminalInputLine("exit")); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, gone.done)
	if err := s.closeTerminalID(gone.id); err != nil {
		t.Fatal(err)
	}
	if record, _ := s.mobileCLI.record(s.profile, ended.ID); record.Archived || s.terminals.get(gone.id) != nil {
		t.Fatal("closing an ended tab archived", record.Archived)
	}
	// A phone turn is stopped by the archive.
	phone := listedCodexSession(t, s, guest, project, "手机上")
	record, _ := s.mobileCLI.record(s.profile, phone.ID)
	if err := s.codexConnection(record); err != nil {
		t.Fatal(err)
	}
	guest.reply("discover", []map[string]any{{"thread_id": record.CLIID, "account": "default", "connection": record.Connection, "workspace": guestPath(project), "title": "手机上", "updated_at": 1763600000.0, "owner": "phone", "running": true, "active": true}})
	if sessions, _ := codexSessions(t, s); !slices.ContainsFunc(sessions, func(row mobileSummary) bool { return row.ID == phone.ID && row.Owner == "phone" }) {
		t.Fatal("phone turn not listed", sessions)
	}
	guest.reply("stop", map[string]any{"ok": true})
	// The guest is asked even when this server has not seen the turn, as
	// after a restart.
	s.mobileCLI.mu.Lock()
	delete(s.mobileCLI.codexOwner, phone.ID)
	delete(s.mobileCLI.codexLive, phone.ID)
	s.mobileCLI.mu.Unlock()
	if w := request(s, "POST", "/api/mobile/cli/sessions/"+phone.ID+"/archive", s.origin, "{}", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if stops := guest.calls("stop"); len(stops) != 1 || stops[0]["connection"] != record.Connection || stops[0]["reason"] != "archive" || stops[0]["request_id"] != nil {
		t.Fatal("archive stop", stops)
	}
	// With the environment stopped the archive does not wait on the guest.
	idle := listedCodexSession(t, s, guest, project, "环境已停止")
	idleRecord, _ := s.mobileCLI.record(s.profile, idle.ID)
	if err := s.codexConnection(idleRecord); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	started := time.Now()
	if err := s.setSessionArchived(idle.ID, true, nil); err != nil || time.Since(started) > 2*time.Second || len(guest.calls("stop")) != 1 {
		t.Fatal("archive with the environment stopped", err, time.Since(started), guest.calls("stop"))
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	// Purge then goes to the guest, which refuses while anything owns the thread.
	guest.reply("purge", map[string]any{"error": "这段会话正在电脑的 Codex 终端中打开，请先关闭该终端再删除"})
	if w := request(s, "POST", "/api/cli/sessions/"+phone.ID+"/purge", s.origin, "{\"confirm\":true}", true); w.Code != 400 || !strings.Contains(w.Body.String(), "请先关闭该终端") {
		t.Fatal("purge while owned", w.Code, w.Body.String())
	}
	if record, _ := s.mobileCLI.record(s.profile, phone.ID); !record.Deleting || record.Purged {
		t.Fatal("a refused purge lost its marker", record)
	}
	guest.reply("purge", map[string]any{"ok": true})
	if w := request(s, "POST", "/api/cli/sessions/"+phone.ID+"/purge", s.origin, "{\"confirm\":true}", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if purges := guest.calls("purge"); len(purges) != 2 || purges[1]["thread"] != record.CLIID || purges[1]["connection"] != record.Connection {
		t.Fatal("purge", purges)
	}
	if tomb, _ := s.mobileCLI.record(s.profile, phone.ID); !tomb.Purged || tomb.CLIID != record.CLIID {
		t.Fatal("tombstone", tomb)
	}
	if w := request(s, "POST", "/api/cli/sessions/"+phone.ID+"/purge", s.origin, "{}", true); w.Code != 400 || !strings.Contains(w.Body.String(), "原始历史") {
		t.Fatal("confirmation copy", w.Code, w.Body.String())
	}
}

// shippedCodexWorkbench serves Codex sessions through the shipped helper over
// the test SSH, with a stand-in Codex CLI. Its exec turns wait while ~/hold
// exists.
func shippedCodexWorkbench(t *testing.T) (*Server, string, string) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	project, _ := filepath.EvalSymlinks(t.TempDir())
	s.preferences.Workspace = project
	s.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	installCodexRouteFixture(t, home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	codex := `#!/usr/bin/python3
import json, os, pathlib, sys, time, uuid
args, home = sys.argv[1:], pathlib.Path(os.environ.get("CODEX_HOME") or pathlib.Path.home() / ".codex")
if args == ["--version"]:
    print("codex-cli 0.171.0")
    sys.exit(0)
if args[-1:] == ["--help"]:
    print("Commands:\n  exec    Run Codex non-interactively\n  resume  Resume a previous session\n\nOptions:\n  -C, --cd <DIR>\n      --add-dir <DIR>\n      --no-daemon\n")
    sys.exit(0)
def rollout(thread):
    return next(home.glob("sessions/*/*/*/rollout-*-" + thread + ".jsonl"))
if args[:1] != ["exec"]:
    # The interactive TUI keeps its thread's rollout open until it quits.
    held = rollout(args[-1]).open("a") if "resume" in args else None
    with (pathlib.Path.home() / "tui.jsonl").open("a") as log:
        log.write(json.dumps({"argv": args, "pid": os.getpid()}) + "\n")
    print("codex tui ready", flush=True)
    for line in sys.stdin:
        if line.strip() == "q":
            break
        if held is None:
            # A new thread's rollout appears with its first message.
            thread = str(uuid.uuid4())
            path = home / "sessions/2026/11/21" / ("rollout-2026-11-21T10-00-00-" + thread + ".jsonl")
            path.parent.mkdir(parents=True, exist_ok=True)
            held = path.open("a")
            held.write(json.dumps({"timestamp": time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime()), "type": "session_meta",
                                   "payload": {"id": thread, "cwd": os.getcwd(), "source": "cli", "originator": "codex-tui"}}) + "\n")
        for kind, payload in (("event_msg", {"type": "task_started"}),
                              ("response_item", {"type": "message", "role": "user", "content": [{"type": "input_text", "text": line.strip()}]}),
                              ("response_item", {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "tui: " + line.strip()}]}),
                              ("event_msg", {"type": "task_complete"})):
            held.write(json.dumps({"type": kind, "payload": payload}) + "\n")
        held.flush()
        print("answered " + line.strip(), flush=True)
    sys.exit(0)
text = sys.stdin.read()
if "resume" in args:
    thread = args[args.index("resume") + 1]
    path = rollout(thread)
else:
    thread = str(uuid.uuid4())
    path = home / "sessions/2026/11/20" / ("rollout-2026-11-20T10-00-00-" + thread + ".jsonl")
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps({"type": "session_meta", "payload": {"id": thread, "cwd": os.getcwd(), "source": "exec"}}) + "\n")
print(json.dumps({"type": "thread.started", "thread_id": thread}), flush=True)
def row(kind, payload):
    with path.open("a") as stream:
        stream.write(json.dumps({"type": kind, "payload": payload}) + "\n")
row("event_msg", {"type": "task_started"})
row("response_item", {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]})
while (pathlib.Path.home() / "hold").exists():
    time.sleep(0.05)
row("response_item", {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "reply: " + text}]})
row("event_msg", {"type": "task_complete"})
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(codex), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export PATH="+shellescape.Quote(bin)+":/usr/bin:/bin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"})
	s.ready = true
	return s, project, home
}

// A Codex session through the shipped helper on both devices: the phone's
// turn, the desktop terminal's Codex under its lease, refusals on both lines,
// and release when the TUI quits. A stand-in CLI writes the rollout records.
func TestCodexDesktopTerminalThroughShippedHelper(t *testing.T) {
	s, project, home := shippedCodexWorkbench(t)
	id := uuid.NewString()
	if w := request(s, "POST", "/api/mobile/cli/sessions", s.origin, `{"id":"`+id+`","workspace":"`+project+`","provider":"codex"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/api/mobile/cli/sessions/" + id
	send := func(text string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": text})
		return request(s, "POST", path+"/messages", s.origin, string(body), true)
	}
	read := func() mobileSession {
		w := request(s, "GET", path, "", "", true)
		var view mobileSession
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return view
	}
	wait := func(what string, done func(mobileSession) bool) mobileSession {
		deadline := time.Now().Add(25 * time.Second)
		for {
			if view := read(); done(view) {
				return view
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %+v", what, read())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	idle := func(view mobileSession) bool {
		return len(view.Turns) > 0 && view.Turns[len(view.Turns)-1].Status == "completed" && view.Owner == "" && view.CanSend != nil && *view.CanSend
	}
	if w := send("hello"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	wait("first phone turn", idle)
	record, _ := s.mobileCLI.record(s.profile, id)
	thread := record.CLIID

	// The desktop opens it as a Codex terminal resumed on the thread.
	w := request(s, "POST", "/api/cli/sessions/"+id+"/open", s.origin, "{}", true)
	var opened mobileSession
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &opened) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	terminal := s.terminals.get(opened.DesktopTerminalID)
	screen := terminal.subscribe()
	readTerminalUntil(t, screen, "codex tui ready")
	screen.Close()
	var tui struct {
		Argv []string
		PID  int
	}
	data, _ := os.ReadFile(filepath.Join(home, "tui.jsonl"))
	if json.Unmarshal(data, &tui) != nil || strings.Join(tui.Argv, " ") != "resume -C "+guestPath(project)+" --no-daemon "+thread {
		t.Fatalf("TUI started with %s", data)
	}
	view := read()
	if view.Owner != "desktop" || view.CanSend == nil || *view.CanSend || view.TerminalAvailable {
		t.Fatalf("desktop-owned view %+v", view)
	}
	if w := send("from the phone"); w.Code != 409 {
		t.Fatal("phone message while the desktop's Codex runs", w.Code, w.Body.String())
	}
	// The guest's lease refuses too, for a writer this server does not know.
	record, _ = s.mobileCLI.record(s.profile, id)
	var guestState struct {
		Lease struct {
			ID  string `json:"id"`
			PID int    `json:"pid"`
		} `json:"lease"`
	}
	stateData, _ := os.ReadFile(filepath.Join(home, ".local/share/cage/codex-sessions", record.Connection, "state.json"))
	if json.Unmarshal(stateData, &guestState) != nil || guestState.Lease.ID != record.CodexLease || guestState.Lease.PID != tui.PID {
		t.Fatalf("guest lease %s, TUI pid %d", stateData, tui.PID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var refused struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	direct := map[string]any{"connection": record.Connection, "request_id": uuid.NewString(), "text": "bypass", "thread": thread, "account": "default",
		"subject": "", "cwd": record.GuestWorkspace, "additional_directories": []string{}}
	if err := s.runCodexSessions(ctx, "send", direct, &refused); err != nil || refused.OK || !strings.Contains(refused.Error, "电脑的 Codex 终端") {
		t.Fatal("guest send under a lease", refused, err)
	}
	// Quitting Codex ends the terminal and releases the thread.
	if err := terminal.enqueueInput(context.Background(), "q\r"); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, terminal.done)
	wait("release", func(view mobileSession) bool { return view.Owner == "" && view.CanSend != nil && *view.CanSend })
	if w := send("after the desktop"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	view = wait("phone turn after the desktop", func(view mobileSession) bool { return len(view.Turns) == 2 && idle(view) })
	if view.Turns[1].Text != "reply: after the desktop" {
		t.Fatalf("turn %+v", view.Turns[1])
	}
}

// A session made from the desktop menu through the shipped helper ends as one
// row on both devices whatever its Codex did: a message seen while it ran, a
// message only after it quit, or nothing at all.
func TestCodexMenuSessionThroughShippedHelper(t *testing.T) {
	for _, tc := range []struct {
		name          string
		message, peek bool
	}{{"message seen while open", true, true}, {"message then quit", true, false}, {"no message", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, project, _ := shippedCodexWorkbench(t)
			body, _ := json.Marshal(map[string]string{"kind": "codex", "workspace": project})
			w := request(s, "POST", "/api/terminals?providers=claude,codex", s.origin, string(body), true)
			var created map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created["cli_session_provider"] != codexProvider {
				t.Fatal(w.Code, w.Body.String())
			}
			id := created["cli_session_id"].(string)
			terminal := s.terminals.get(created["id"].(string))
			screen := terminal.subscribe()
			defer screen.Close()
			readTerminalUntil(t, screen, "codex tui ready")
			lists := func() (desktop, phone []mobileSummary) {
				for _, path := range []string{"/api/cli/sessions?providers=claude,codex", "/api/mobile/cli/sessions"} {
					w := request(s, "GET", path, "", "", true)
					var answer struct {
						Sessions []mobileSummary `json:"sessions"`
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil {
						t.Fatal(path, w.Code, w.Body.String())
					}
					rows := slices.DeleteFunc(answer.Sessions, func(row mobileSummary) bool { return row.Provider != codexProvider })
					if desktop == nil {
						desktop = rows
					} else {
						phone = rows
					}
				}
				return desktop, phone
			}
			if tc.message {
				if err := terminal.enqueueInput(context.Background(), "hello from the tui\r"); err != nil {
					t.Fatal(err)
				}
				readTerminalUntil(t, screen, "answered hello from the tui")
			}
			if tc.peek {
				desktop, phone := lists()
				for _, rows := range [][]mobileSummary{desktop, phone} {
					if len(rows) != 1 || rows[0].ID != id || rows[0].Owner != "desktop" {
						t.Fatalf("while open: %+v", rows)
					}
				}
			}
			if err := terminal.enqueueInput(context.Background(), "q\r"); err != nil {
				t.Fatal(err)
			}
			awaitTerminalSignal(t, terminal.done)
			deadline := time.Now().Add(20 * time.Second)
			for {
				desktop, phone := lists()
				want := 0
				if tc.message {
					want = 1
				}
				settled := len(desktop) == want && len(phone) == want
				for _, rows := range [][]mobileSummary{desktop, phone} {
					for _, row := range rows {
						settled = settled && row.ID == id && row.Owner == ""
					}
				}
				if settled {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after quitting: desktop %+v phone %+v", desktop, phone)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !tc.message {
				return
			}
			w = request(s, "GET", "/api/mobile/cli/sessions/"+id, "", "", true)
			var view mobileSession
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || len(view.Turns) != 1 || view.Turns[0].Text != "tui: hello from the tui" || view.CanSend == nil || !*view.CanSend {
				t.Fatalf("view %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// After a workbench restart the old terminal is gone but its Codex may still
// hold the thread under its guest lease: the new server reads that as another
// process, refuses the phone and the desktop, and offers no stale terminal.
func TestCodexOwnershipAfterARestartComesFromTheGuestLease(t *testing.T) {
	s, project, home := shippedCodexWorkbench(t)
	id := uuid.NewString()
	if w := request(s, "POST", "/api/mobile/cli/sessions", s.origin, `{"id":"`+id+`","workspace":"`+project+`","provider":"codex"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/api/mobile/cli/sessions/" + id
	send := func(s *Server) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": "hello"})
		return request(s, "POST", path+"/messages", s.origin, string(body), true)
	}
	wait := func(s *Server, what string, done func(mobileSession) bool) mobileSession {
		deadline := time.Now().Add(25 * time.Second)
		for {
			w := request(s, "GET", path, "", "", true)
			var view mobileSession
			if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &view) == nil && done(view) {
				return view
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d %s", what, w.Code, w.Body.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if w := send(s); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	wait(s, "first turn", func(view mobileSession) bool {
		return len(view.Turns) == 1 && view.Owner == "" && view.CanSend != nil && *view.CanSend
	})
	w := request(s, "POST", "/api/cli/sessions/"+id+"/open", s.origin, "{}", true)
	var opened mobileSession
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &opened) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	terminal := s.terminals.get(opened.DesktopTerminalID)
	screen := terminal.subscribe()
	readTerminalUntil(t, screen, "codex tui ready")
	screen.Close()
	if record, _ := s.mobileCLI.record(s.profile, id); record.TerminalID != terminal.id || record.CodexLease == "" {
		t.Fatalf("record before the restart %+v", record)
	}

	// A new server over the same profile; the old one's Codex keeps running.
	restarted := &Server{account: s.account, profile: s.profile, configPath: s.configPath, origin: s.origin, token: s.token, session: s.session, preferences: s.preferences}
	restarted.projectShares = map[string]*projectShare{project: {done: make(chan struct{})}}
	t.Cleanup(restarted.terminals.close)
	mobileTestSSH(t, restarted, home, []string{"HOME=" + home, "PATH=" + filepath.Join(home, "bin") + ":/usr/bin:/bin"})
	restarted.ready = true
	view := wait(restarted, "guest lease", func(view mobileSession) bool { return view.Owner == "external" })
	if view.CanSend == nil || *view.CanSend || view.DesktopTerminalID != "" || view.TerminalAvailable || view.ConnectionNotice != codexExternalNotice {
		t.Fatalf("view after the restart %+v", view)
	}
	answer, _ := desktopCodexList(t, restarted, "?providers=claude,codex")
	if len(answer.Sessions) != 1 || answer.Sessions[0].Owner != "external" || !answer.Sessions[0].Running {
		t.Fatalf("listed after the restart %+v", answer.Sessions)
	}
	if w := send(restarted); w.Code != 409 || !strings.Contains(w.Body.String(), "电脑的 Codex 终端") {
		t.Fatal("phone message under the old terminal's lease", w.Code, w.Body.String())
	}
	if w := request(restarted, "POST", "/api/cli/sessions/"+id+"/open", restarted.origin, "{}", true); w.Code != 409 || len(restarted.terminals.snapshot()) != 0 {
		t.Fatal("desktop open under the old terminal's lease", w.Code, w.Body.String())
	}
	// When that Codex quits, the thread is free again.
	if err := terminal.enqueueInput(context.Background(), "q\r"); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, terminal.done)
	wait(restarted, "release", func(view mobileSession) bool { return view.Owner == "" && view.CanSend != nil && *view.CanSend })
	if w := send(restarted); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// An open on the desktop and a phone message racing for one thread never both
// win: the server's operation lock orders them, the bound terminal refuses the
// phone, a running phone turn refuses the open.
func TestCodexOpenAndPhoneSendNeverBothWin(t *testing.T) {
	s, project, home := shippedCodexWorkbench(t)
	id := uuid.NewString()
	if w := request(s, "POST", "/api/mobile/cli/sessions", s.origin, `{"id":"`+id+`","workspace":"`+project+`","provider":"codex"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/api/mobile/cli/sessions/" + id
	send := func(text string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": text})
		return request(s, "POST", path+"/messages", s.origin, string(body), true)
	}
	wait := func(what string, done func(mobileSession) bool) {
		deadline := time.Now().Add(25 * time.Second)
		for {
			w := request(s, "GET", path, "", "", true)
			var view mobileSession
			if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &view) == nil && done(view) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s", what, w.Body.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	idle := func(view mobileSession) bool {
		return view.Owner == "" && view.CanSend != nil && *view.CanSend && len(view.Turns) > 0
	}
	if w := send("first"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	wait("first turn", idle)
	hold := filepath.Join(home, "hold")
	wins := map[string]int{}
	for round := 0; round < 4; round++ {
		if err := os.WriteFile(hold, nil, 0600); err != nil {
			t.Fatal(err)
		}
		var opened, sent *httptest.ResponseRecorder
		var group sync.WaitGroup
		gate := make(chan struct{})
		group.Add(2)
		go func() {
			defer group.Done()
			<-gate
			// Odd rounds let the phone's send take the operation lock first.
			for deadline := time.Now().Add(5 * time.Second); round%2 == 1 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if !s.op.TryLock() {
					break
				}
				s.op.Unlock()
			}
			opened = request(s, "POST", "/api/cli/sessions/"+id+"/open", s.origin, "{}", true)
		}()
		go func() { defer group.Done(); <-gate; sent = send(fmt.Sprintf("round %d", round)) }()
		close(gate)
		group.Wait()
		if opened.Code == 200 && sent.Code == 200 {
			t.Fatalf("round %d: both won", round)
		}
		switch {
		case opened.Code == 200:
			wins["desktop"]++
			t.Logf("round %d: phone refused: %s", round, strings.TrimSpace(sent.Body.String()))
			if sent.Code != 409 {
				t.Fatalf("round %d: phone %d %s", round, sent.Code, sent.Body.String())
			}
			var view mobileSession
			_ = json.Unmarshal(opened.Body.Bytes(), &view)
			terminal := s.terminals.get(view.DesktopTerminalID)
			screen := terminal.subscribe()
			readTerminalUntil(t, screen, "codex tui ready")
			screen.Close()
			if err := terminal.enqueueInput(context.Background(), "q\r"); err != nil {
				t.Fatal(err)
			}
			awaitTerminalSignal(t, terminal.done)
			s.terminals.remove(terminal.id)
			_ = os.Remove(hold)
		case sent.Code == 200:
			wins["phone"]++
			t.Logf("round %d: open refused: %s", round, strings.TrimSpace(opened.Body.String()))
			if opened.Code != 409 {
				t.Fatalf("round %d: open %d %s", round, opened.Code, opened.Body.String())
			}
			if bound := s.terminals.boundCodex(id); bound != nil {
				t.Fatalf("round %d: a terminal was bound beside the phone turn", round)
			}
			_ = os.Remove(hold)
		default:
			t.Fatalf("round %d: neither won: open %d %s, send %d %s", round, opened.Code, opened.Body.String(), sent.Code, sent.Body.String())
		}
		wait(fmt.Sprintf("round %d release", round), idle)
	}
	t.Logf("wins %v", wins)
}

type httpResponse struct {
	code int
	body string
}

func mustEpoch(t *testing.T, s *Server) context.Context {
	t.Helper()
	epoch, ok := s.account.access()
	if !ok {
		t.Fatal("missing unlock epoch")
	}
	return epoch
}

// The shipped helper runs each phone message as a detached exec turn. A
// stand-in CLI writes the rollout records of a real turn; nothing else is fake.
func TestCodexSessionThroughShippedHelper(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	s := testServer(t)
	project, _ := filepath.EvalSymlinks(t.TempDir())
	s.preferences.Workspace = project
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	codex := `#!/usr/bin/python3
import json, os, pathlib, sys, time, uuid
args, home = sys.argv[1:], pathlib.Path.home() / ".codex"
text = sys.stdin.read()
if "resume" in args:
    thread = args[args.index("resume") + 1]
    path = next(home.glob("sessions/*/*/*/rollout-*-" + thread + ".jsonl"))
else:
    thread = str(uuid.uuid4())
    path = home / "sessions/2026/11/20" / ("rollout-2026-11-20T10-00-00-" + thread + ".jsonl")
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps({"type": "session_meta", "payload": {"id": thread, "cwd": os.getcwd(), "source": "exec"}}) + "\n")
print(json.dumps({"type": "thread.started", "thread_id": thread}), flush=True)
def row(kind, payload):
    with path.open("a") as stream:
        stream.write(json.dumps({"type": kind, "payload": payload}) + "\n")
row("event_msg", {"type": "task_started"})
row("response_item", {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]})
if (pathlib.Path.home() / "fake-sleep").exists():
    time.sleep(30)
row("response_item", {"type": "function_call", "name": "exec_command", "arguments": json.dumps({"cmd": "ls"}), "call_id": "call-" + str(len(text))})
row("response_item", {"type": "function_call_output", "call_id": "call-" + str(len(text)), "output": "ok"})
row("response_item", {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "reply: " + text}]})
row("event_msg", {"type": "task_complete"})
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(codex), 0700); err != nil {
		t.Fatal(err)
	}
	// The guest login shell must find only the stand-in CLI, never one
	// installed on the developer's machine.
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export PATH="+shellescape.Quote(bin)+":/usr/bin:/bin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"})
	t.Cleanup(func() {
		states, _ := filepath.Glob(filepath.Join(home, ".local/share/cage/codex-sessions/*/state.json"))
		for _, path := range states {
			var state struct {
				Run struct {
					PID int `json:"pid"`
				} `json:"run"`
			}
			if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &state) == nil && state.Run.PID > 0 {
				_ = syscall.Kill(-state.Run.PID, syscall.SIGKILL)
			}
		}
	})
	s.ready = true
	id := uuid.NewString()
	if w := request(s, "POST", "/api/mobile/cli/sessions", s.origin, `{"id":"`+id+`","workspace":"`+project+`","provider":"codex"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/api/mobile/cli/sessions/" + id
	send := func(text string) string {
		requestID := uuid.NewString()
		body, _ := json.Marshal(map[string]string{"request_id": requestID, "text": text})
		if w := request(s, "POST", path+"/messages", s.origin, string(body), true); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return requestID
	}
	wait := func(done func(mobileSession) bool) mobileSession {
		deadline := time.Now().Add(20 * time.Second)
		for {
			w := request(s, "GET", path, "", "", true)
			var view mobileSession
			if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &view) == nil && done(view) {
				return view
			}
			if time.Now().After(deadline) {
				logs, _ := exec.Command("sh", "-c", "cat "+home+"/.local/share/cage/codex-sessions/*/exec.log").CombinedOutput()
				t.Fatalf("session did not settle: %d %s\n%s", w.Code, w.Body.String(), logs)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	last := func(view mobileSession) mobileTurn { return view.Turns[len(view.Turns)-1] }
	first := send("hello")
	view := wait(func(view mobileSession) bool {
		return len(view.Turns) == 1 && last(view).Status == "completed" && view.CanSend != nil && *view.CanSend
	})
	turn := last(view)
	if turn.ID != first || turn.Prompt != "hello" || turn.Text != "reply: hello" || len(turn.Tools) != 1 || turn.Tools[0].Name != "exec_command" || turn.Tools[0].Detail != "ls" || turn.Tools[0].Status != "completed" {
		t.Fatalf("first turn: %#v", turn)
	}
	record, _ := s.mobileCLI.record(s.profile, id)
	if !record.Resume || !validCLIUUID(record.CLIID) || record.Title != "hello" {
		t.Fatalf("thread not bound: %#v", record)
	}
	thread := record.CLIID
	if sessions, notice := codexSessions(t, s); notice != "" || len(sessions) != 1 || sessions[0].ID != id || sessions[0].Provider != codexProvider || sessions[0].Running {
		t.Fatal("listing", sessions, notice)
	}
	second := send("again")
	view = wait(func(view mobileSession) bool { return len(view.Turns) == 2 && last(view).Status == "completed" })
	if last(view).ID != second || last(view).Text != "reply: again" || view.Turns[0].ID != first {
		t.Fatalf("resumed turn: %#v", view.Turns)
	}
	if record, _ = s.mobileCLI.record(s.profile, id); record.CLIID != thread {
		t.Fatal("resume changed the thread")
	}
	if err := os.WriteFile(filepath.Join(home, "fake-sleep"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	third := send("long task")
	view = wait(func(view mobileSession) bool {
		return len(view.Turns) == 3 && last(view).ID == third && last(view).Status == "running"
	})
	if view.CanSend == nil || *view.CanSend || !view.Active {
		t.Fatalf("running turn: %#v", view)
	}
	body, _ := json.Marshal(map[string]string{"request_id": uuid.NewString(), "text": "too soon"})
	if w := request(s, "POST", path+"/messages", s.origin, string(body), true); w.Code != 409 {
		t.Fatal("second message during a running turn", w.Code, w.Body.String())
	}
	if w := request(s, "POST", path+"/stop", s.origin, `{"request_id":"`+third+`"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	wait(func(view mobileSession) bool {
		return last(view).Status == "stopped" && view.CanSend != nil && *view.CanSend
	})
}
