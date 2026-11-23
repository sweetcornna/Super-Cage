//go:build !windows

package workbench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// claudeSwitchGuest runs the host's real SSH and helper bootstrap against a
// loopback guest whose channel helper is replaced by script. It records the
// operations the host asked for in calls.jsonl.
func claudeSwitchGuest(t *testing.T, script string) (*Server, string, *mobileRecord, string) {
	t.Helper()
	s, id, project := metadataFixture(t)
	s.ready = true
	home := t.TempDir()
	mobileTestSSH(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/local/bin"})
	helper := filepath.Join(home, ".local/share/cage/cli-channel-code", cliChannelVersion+".py")
	if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
		t.Fatal(err)
	}
	code := "import json,sys,pathlib\nmode=sys.argv[1]\nrequest=json.load(sys.stdin)\nwith (pathlib.Path.home()/'calls.jsonl').open('a') as f: f.write(json.dumps({'mode':mode,'switch':request.get('switch_id'),'root':request.get('root'),'force':request.get('force'),'request':request.get('request_id'),'text':request.get('text')})+'\\n')\n" + script
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := claudeAccountList{Accounts: []claudeAccount{{ID: a, Label: "A", Email: "a@example.test", Enabled: true, Status: "ready", HandoffGroup: "personal"}, {ID: b, Label: "B", Email: "b@example.test", Enabled: true, Status: "ready", HandoffGroup: "personal"}}}
	if err := s.saveClaudeAccounts(list); err != nil {
		t.Fatal(err)
	}
	s.claudeAccounts.probe = func(_ context.Context, guest string) (claudeGuestSnapshot, error) {
		return claudeGuestSnapshot{LoggedIn: true, Email: guest[:1] + "@example.test"}, nil
	}
	record, err := s.mobileCLI.record(s.profile, id)
	if err != nil {
		t.Fatal(err)
	}
	record.Launcher, record.Account = "claude", claudeSelection{a, a, claudeIdentityHash("a@example.test", "")}
	record.CLIID, record.Connection = uuid.NewString(), uuid.NewString()
	record.GuestConfig, record.GuestWorkspace = "/guest/accounts/a/config", guestPath(project)+"/packages/app"
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	return s, id, record, home
}

func claudeSwitchContext(s *Server) context.Context {
	epoch, _ := s.account.access()
	return context.WithValue(context.Background(), accountEpochKey{}, epoch)
}

// A refused preparation leaves no handoff behind: the next attempt needs no
// cancel, and an earlier attempt that an older version left half done is
// replaced by a new choice instead of blocking it.
func TestClaudeSwitchRefusalLeavesNoPendingHandoff(t *testing.T) {
	s, id, record, home := claudeSwitchGuest(t, "print(json.dumps({'error':'switch:project'} if mode=='account-switch' else {'ok':True}))\n")
	b := strings.Repeat("b", 24)
	stale := uuid.NewString()
	record.Switch = &claudeAccountSwitch{ID: stale, Phase: "preparing", Source: claudeExecution{record.CLIID, record.Account, record.GuestConfig}, Target: claudeSelection{b, b, claudeIdentityHash("b@example.test", "")}, TargetLabel: "B", TargetID: uuid.NewString(), Connection: record.Connection, Error: "历史项目与当前项目不一致"}
	if err := writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	err := s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: attempt, SourceID: record.CLIID, Target: b, Confirm: true}, false)
	if err == nil || !strings.Contains(err.Error(), "另一个项目文件夹") {
		t.Fatal("refusal must name the thing and the next step:", err)
	}
	stored, _ := s.mobileCLI.record(s.profile, id)
	if stored.Switch != nil || stored.Account.ID != record.Account.ID || stored.CLIID != record.CLIID {
		t.Fatalf("a refused preparation left state behind: %+v", stored.Switch)
	}
	calls, _ := os.ReadFile(filepath.Join(home, "calls.jsonl"))
	want := []string{`"mode": "account-switch-cancel", "switch": "` + stale, `"mode": "account-switch", "switch": "` + attempt, `"mode": "account-switch-cancel", "switch": "` + attempt}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != len(want) {
		t.Fatalf("guest operations: %s", calls)
	}
	for i, line := range lines {
		if !strings.Contains(line, want[i]) {
			t.Fatalf("guest operation %d = %s, want %s", i, line, want[i])
		}
	}
	if !strings.Contains(lines[1], `"root": "`+guestPath(s.preferences.Workspace)+`"`) {
		t.Fatal("the bound project folder must reach the guest as root:", lines[1])
	}
	// A second attempt with yet another request starts cleanly.
	err = s.switchClaudeAccount(claudeSwitchContext(s), id, claudeSwitchInput{RequestID: uuid.NewString(), SourceID: record.CLIID, Target: b, Confirm: true}, false)
	if err == nil || strings.Contains(err.Error(), "交接") && strings.Contains(err.Error(), "完成") {
		t.Fatal("a new attempt was blocked by the failed one:", err)
	}
}
