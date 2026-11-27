package workbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func codexFixture(id, email string) codexAccount {
	return codexAccount{ID: id, Label: email, Enabled: true, Status: "ready", Email: email, Organization: "workspace"}
}

func exhaustedCodexQuota(now time.Time) *codexQuota {
	return &codexQuota{Primary: &codexWindow{UsedPercentage: 100, ResetsAt: now.Add(time.Hour).Unix(), WindowMinutes: 300, ObservedAt: now.Unix()}, ObservedAt: now.Unix()}
}

func TestCodexSchedulingSkipsDisabledSignedOutAndExhaustedAccounts(t *testing.T) {
	now := time.Now()
	a, b, c, d := strings.Repeat("a", 24), strings.Repeat("b", 24), strings.Repeat("c", 24), strings.Repeat("d", 24)
	list := codexAccountList{Scheduling: (claudeScheduling{}).defaults(), Accounts: []codexAccount{
		codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test"), codexFixture(c, "c@example.test"), codexFixture(d, "d@example.test"),
	}}
	list.Accounts[0].Enabled = false
	list.Accounts[1].Quota = exhaustedCodexQuota(now)
	list.Accounts[2].Status = "signed_out"
	if id := pickCodexAccount(list, nil, nil, "", "", now); id != d {
		t.Fatalf("balanced picked %q, want the only eligible account", id)
	}
	list.Scheduling.Mode = "sequential"
	if id := pickCodexAccount(list, nil, nil, "", "", now); id != d {
		t.Fatalf("sequential picked %q", id)
	}
	if id := pickCodexAccount(list, nil, map[string]bool{d: true}, "", "", now); id != "" {
		t.Fatalf("assigned an account that is signing in: %q", id)
	}
	// The exhausted window becomes available at its actual reset.
	if id := pickCodexAccount(list, nil, nil, "", "", now.Add(2*time.Hour)); id != b {
		t.Fatalf("reset window stayed blocked: %q", id)
	}
	// A stale reading below 100% is unknown, not a block.
	list.Accounts[1].Quota.Primary.UsedPercentage = 90
	if room, ok := list.Accounts[1].headroom(now.Add(30 * time.Minute)); !ok || room != -1 {
		t.Fatal("stale reading was treated as fresh", room, ok)
	}
}

func TestCodexBalancedSchedulingUsesLoadHeadroomRotationAndAffinity(t *testing.T) {
	now := time.Now()
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := codexAccountList{Scheduling: (claudeScheduling{}).defaults(), Accounts: []codexAccount{codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test")}}
	if id := pickCodexAccount(list, map[string]int{a: 1}, nil, b, "", now); id != b {
		t.Fatal("ignored live terminals", id)
	}
	if id := pickCodexAccount(list, nil, nil, a, "", now); id != b {
		t.Fatal("equal accounts did not rotate", id)
	}
	list.Accounts[0].Quota = &codexQuota{Primary: &codexWindow{UsedPercentage: 10, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}, ObservedAt: now.Unix()}
	list.Accounts[1].Quota = &codexQuota{Secondary: &codexWindow{UsedPercentage: 80, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}, ObservedAt: now.Unix()}
	if id := pickCodexAccount(list, nil, nil, a, "", now); id != a {
		t.Fatal("did not prefer the larger remaining window", id)
	}
	if id := pickCodexAccount(list, nil, nil, a, b, now); id != a {
		t.Fatal("project affinity overrode a much larger headroom", id)
	}
	list.Accounts[1].Quota.Secondary.UsedPercentage = 12
	if id := pickCodexAccount(list, nil, nil, a, b, now); id != b {
		t.Fatal("comparable headroom did not keep the project's account", id)
	}
}

func TestCodexAllocationCountsPhoneTurns(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := codexAccountList{Scheduling: (claudeScheduling{}).defaults(), Accounts: []codexAccount{codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test")}}
	if err := s.saveCodexAccounts(list); err != nil {
		t.Fatal(err)
	}
	none := func(context.Context, []string) (map[string]codexGuestQuota, error) { return nil, nil }
	if selection, err := s.allocateCodexAccount(list, "", none); err != nil || selection.ID != a {
		t.Fatal("idle roster", selection, err)
	}
	id := "6a1e8b4e-6f1d-4c1a-9b2e-0c3d4e5f6a7b"
	s.mobileCLI.mu.Lock()
	s.mobileCLI.catalog = map[string]mobileSummary{id: {ID: id, Provider: codexProvider, CodexAccountID: a}}
	s.mobileCLI.codexOwner = map[string]string{id: "phone"}
	s.mobileCLI.mu.Unlock()
	if selection, err := s.allocateCodexAccount(list, "", none); err != nil || selection.ID != b {
		t.Fatal("a phone turn's account was not counted", selection, err)
	}
	s.mobileCLI.mu.Lock()
	s.mobileCLI.codexOwner[id] = ""
	s.mobileCLI.mu.Unlock()
	if selection, _ := s.allocateCodexAccount(list, "", none); selection.ID != a {
		t.Fatal("an idle session still counted", selection)
	}
}

func TestCodexEmptyRosterFallsBackToDefaultCodexHome(t *testing.T) {
	s := testServer(t)
	s.codexAccounts.probe = func(context.Context, string) (codexGuestSnapshot, error) { return codexGuestSnapshot{}, nil }
	if selection, err := s.selectCodexAccount("", false, "/project"); err != nil || selection != (codexSelection{}) {
		t.Fatalf("empty roster did not use ~/.codex: %+v %v", selection, err)
	}
	// An unfinished login has no account yet and keeps the existing behavior.
	login, err := s.selectCodexAccount("", true, "")
	if err != nil || !validClaudeID(login.ID) || login.GuestID != login.ID || login.Subject != "" {
		t.Fatalf("login selection: %+v %v", login, err)
	}
	if selection, err := s.selectCodexAccount("", false, ""); err != nil || selection.ID != "" {
		t.Fatalf("pending login changed default selection: %+v %v", selection, err)
	}
	if _, err := s.selectCodexAccount(strings.Repeat("e", 24), false, ""); err == nil {
		t.Fatal("a Codex terminal chose its own account")
	}
	list, _ := s.loadCodexAccounts()
	list.Accounts = []codexAccount{codexFixture(strings.Repeat("a", 24), "a@example.test")}
	list.Accounts[0].Enabled = false
	if err := s.saveCodexAccounts(list); err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectCodexAccount("", false, ""); err == nil {
		t.Fatal("a paused roster silently switched to ~/.codex")
	}
	if err := os.WriteFile(filepath.Join(s.profile, "codex-accounts.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectCodexAccount("", false, ""); err == nil {
		t.Fatal("corrupt roster silently switched to ~/.codex")
	}
}

func TestCodexAllocationReadsGuestQuotaBoundToIdentity(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := codexAccountList{Scheduling: (claudeScheduling{}).defaults(), Accounts: []codexAccount{codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test")}}
	if err := s.saveCodexAccounts(list); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var calls int
	read := func(_ context.Context, ids []string) (map[string]codexGuestQuota, error) {
		calls++
		if len(ids) != 2 {
			t.Fatalf("expected one batch of participating accounts: %v", ids)
		}
		return map[string]codexGuestQuota{
			a: {codexQuota: *exhaustedCodexQuota(now), Identity: claudeIdentityHash("a@example.test", "workspace")},
			b: {codexQuota: *exhaustedCodexQuota(now), Identity: claudeIdentityHash("someone-else@example.test", "workspace")},
		}, nil
	}
	selection, err := s.allocateCodexAccount(list, "", read)
	if err != nil || selection.ID != b || selection.GuestID != b || selection.Subject != claudeIdentityHash("b@example.test", "workspace") || calls != 1 {
		t.Fatalf("ignored A's exhaustion or accepted another identity's reading: %+v %v", selection, err)
	}
	selection, err = s.allocateCodexAccount(list, "", func(context.Context, []string) (map[string]codexGuestQuota, error) {
		return nil, errors.New("VM temporarily unavailable")
	})
	if err != nil || selection.ID != b {
		t.Fatalf("guest failure erased confirmed exhaustion: %+v %v", selection, err)
	}
	saved, _ := s.loadCodexAccounts()
	if saved.Accounts[1].Quota != nil || saved.Accounts[0].Quota.Primary.UsedPercentage != 100 || saved.Accounts[0].Quota.Primary.WindowMinutes != 300 {
		t.Fatal("observations did not persist with their source identity", saved.Accounts)
	}
	// Live Codex terminals count toward balance; Claude terminals do not.
	s.terminals.sessions = map[string]*terminalSession{
		"codex":  {codexAccountID: b, kind: "codex"},
		"claude": {claudeAccountID: a, kind: "claude"},
	}
	saved.Accounts[0].Quota = nil
	if err := s.saveCodexAccounts(saved); err != nil {
		t.Fatal(err)
	}
	if selection, err = s.allocateCodexAccount(saved, "", func(context.Context, []string) (map[string]codexGuestQuota, error) { return nil, nil }); err != nil || selection.ID != a {
		t.Fatalf("live Codex terminal was not counted: %+v %v", selection, err)
	}
}

func TestCodexSchedulingIsIndependentFromClaude(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	if err := s.saveClaudeAccounts(claudeAccountList{Accounts: []claudeAccount{{ID: a, Label: "A", Enabled: true, Status: "ready", Email: "a@example.test"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test")}}); err != nil {
		t.Fatal(err)
	}
	claudeBefore, _ := os.ReadFile(filepath.Join(s.profile, "claude-accounts.json"))
	w := request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"sequential","switch_policy":"wait"},"order":["`+b+`","`+a+`"]}`, true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	codex, _ := s.loadCodexAccounts()
	if codex.Scheduling.Mode != "sequential" || codex.Scheduling.SwitchPolicy != "wait" ||
		codex.Scheduling.SelectedAccountRevision == "" || codex.Accounts[0].ID != b {
		t.Fatalf("Codex scheduling not saved: %+v", codex)
	}
	claudeAfter, _ := os.ReadFile(filepath.Join(s.profile, "claude-accounts.json"))
	claude, err := s.loadClaudeAccounts()
	if string(claudeBefore) != string(claudeAfter) || err != nil || claude.Scheduling != (claudeScheduling{Mode: "balanced", SwitchPolicy: "ask"}) {
		t.Fatalf("Codex scheduling changed Claude: %+v %v", claude, err)
	}
	w = request(s, "POST", "/api/claude-accounts", s.origin, `{"action":"scheduling","scheduling":{"mode":"sequential","switch_policy":"allow"}}`, true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	if codex, _ = s.loadCodexAccounts(); codex.Scheduling.SwitchPolicy != "wait" {
		t.Fatal("Claude scheduling changed Codex", codex.Scheduling)
	}
	for _, body := range []string{
		`{"action":"scheduling","scheduling":{"mode":"random","switch_policy":"ask"}}`,
		`{"action":"scheduling","scheduling":{"mode":"balanced","switch_policy":"ask"},"order":["` + a + `","` + a + `"]}`,
		`{"action":"update","id":"` + a + `","label":"` + strings.Repeat("x", 65) + `"}`,
		`{"action":"unknown"}`,
	} {
		if w = request(s, "POST", "/api/codex-accounts", s.origin, body, true); w.Code == http.StatusOK {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestCodexAllocationCountsGuestShellLaunch(t *testing.T) {
	s := testServer(t)
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	list := codexAccountList{
		Scheduling: (claudeScheduling{}).defaults(),
		Accounts:   []codexAccount{codexFixture(a, "a@example.test"), codexFixture(b, "b@example.test")},
	}
	if err := s.saveCodexAccounts(list); err != nil {
		t.Fatal(err)
	}
	selection, err := s.allocateCodexAccount(list, "", func(context.Context, []string) (map[string]codexGuestQuota, error) {
		return map[string]codexGuestQuota{a: {PlainLaunches: 1}}, nil
	})
	if err != nil || selection.ID != b {
		t.Fatalf("SSH launch was not counted for workbench allocation: %+v %v", selection, err)
	}
}

func TestCodexAccountLoadCombinesPhoneAndManagedService(t *testing.T) {
	s := testServer(t)
	id := strings.Repeat("a", 24)
	s.mobileCLI.codexOwner = map[string]string{"conversation": "phone"}
	s.mobileCLI.catalog = map[string]mobileSummary{"conversation": {CodexAccountID: id}}
	if active, _ := s.codexAccountLoad(); active[id] != 1 {
		t.Fatalf("dormant service hid phone load: %v", active)
	}
	s.t3code.config = defaultT3CodeConfig()
	s.t3code.config.Enabled = true
	s.t3code.configLoaded = true
	s.t3code.statusAt = time.Now()
	s.t3code.statusCache = t3CodeGuestStatus{Running: true, PID: 1234, Port: s.t3code.config.Port, Accounts: map[string]string{"codex": id}}
	if active, _ := s.codexAccountLoad(); active[id] != 2 {
		t.Fatalf("running service was not counted with phone load: %v", active)
	}
}

func TestCodexAccountPanelUpdatesAndRemoval(t *testing.T) {
	s := testServer(t)
	a := strings.Repeat("a", 24)
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{codexFixture(a, "a@example.test")}}); err != nil {
		t.Fatal(err)
	}
	w := request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"update","id":"`+a+`","label":"工作","enabled":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	var view codexPanel
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Accounts) != 1 || view.Accounts[0].Label != "工作" || view.Accounts[0].Enabled || !view.GuestSupported {
		t.Fatalf("update not reflected: %+v %v", view, err)
	}
	s.terminals.sessions = map[string]*terminalSession{"live": {codexAccountID: a, kind: "codex", done: make(chan struct{})}}
	if w = request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"remove","id":"`+a+`"}`, true); w.Code != http.StatusConflict {
		t.Fatal("removed an account with a live terminal", w.Code)
	}
	close(s.terminals.sessions["live"].done)
	if w = request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"remove","id":"`+a+`"}`, true); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	if list, _ := s.loadCodexAccounts(); len(list.Accounts) != 0 {
		t.Fatal("account was not removed", list)
	}
	s.account.lock()
	if w = request(s, "GET", "/api/codex-accounts", "", "", true); w.Code != http.StatusLocked {
		t.Fatal("locked Cage exposed Codex accounts", w.Code)
	}
}

func TestCodexDeviceLoginAddsAccountOnce(t *testing.T) {
	s := testServer(t)
	s.ready = true
	reading := codexGuestSnapshot{LoggedIn: true, AuthMode: "chatgpt", Email: "Owner@Example.test", Organization: "workspace", Plan: "plus"}
	s.codexAccounts.probe = func(_ context.Context, id string) (codexGuestSnapshot, error) {
		if id == "default" {
			return codexGuestSnapshot{}, nil
		}
		return reading, nil
	}
	login, err := s.selectCodexAccount("", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.confirmCodexLogin(context.Background(), login.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := s.loadCodexAccounts()
	if len(list.Accounts) != 1 || list.PendingLogin != "" || list.Accounts[0].ID != login.ID || list.Accounts[0].Status != "ready" ||
		list.Accounts[0].Email != "owner@example.test" || list.Accounts[0].Label != "owner@example.test" || list.Accounts[0].Plan != "plus" {
		t.Fatalf("login did not add a ready account: %+v", list)
	}
	selection, err := s.selectCodexAccount("", false, "")
	if err != nil || selection.ID != login.ID || selection.Subject != claudeIdentityHash("owner@example.test", "workspace") {
		t.Fatalf("new account was not scheduled: %+v %v", selection, err)
	}
	again, _ := s.selectCodexAccount("", true, "")
	if err = s.confirmCodexLogin(context.Background(), again.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.loadCodexAccounts(); len(list.Accounts) != 1 || !strings.Contains(s.codexAccounts.loginNotice, "已在 Codex 账号列表中") {
		t.Fatalf("same subscription was added twice: %+v %q", list, s.codexAccounts.loginNotice)
	}
	reading = codexGuestSnapshot{}
	abandoned, _ := s.selectCodexAccount("", true, "")
	if err = s.confirmCodexLogin(context.Background(), abandoned.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.loadCodexAccounts(); list.PendingLogin != "" || len(list.Accounts) != 1 || !strings.Contains(s.codexAccounts.loginNotice, "未完成") {
		t.Fatalf("unfinished login left a draft: %+v", list)
	}
	// Signing the account out in the guest stops scheduling it.
	if err = s.refreshCodex(context.Background(), login.ID, false); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.loadCodexAccounts(); list.Accounts[0].Status != "signed_out" {
		t.Fatal("signed-out account stayed ready", list.Accounts[0])
	}
	if _, err = s.selectCodexAccount("", false, ""); err == nil {
		t.Fatal("signed-out roster fell back silently")
	}
}

func TestCodexLoginRecordsPendingSignInBeforeReusingItsDirectory(t *testing.T) {
	s := testServer(t)
	s.ready = true
	pending := strings.Repeat("c", 24)
	owner := codexGuestSnapshot{LoggedIn: true, AuthMode: "chatgpt", Email: "owner@example.test", Organization: "workspace"}
	var mu sync.Mutex
	readings := map[string]codexGuestSnapshot{pending: owner}
	var probeErr error
	probes := map[string]int{}
	s.codexAccounts.probe = func(_ context.Context, id string) (codexGuestSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		probes[id]++
		return readings[id], probeErr
	}
	reset := func() {
		if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{}, PendingLogin: pending}); err != nil {
			t.Fatal(err)
		}
		s.codexAccounts.mu.Lock()
		// A background check moments ago must not let a login skip its own.
		s.codexAccounts.attempted = map[string]time.Time{pending: time.Now()}
		s.codexAccounts.mu.Unlock()
		mu.Lock()
		probes = map[string]int{}
		mu.Unlock()
	}
	reset()
	login, err := s.selectCodexAccount("", true, "")
	list, _ := s.loadCodexAccounts()
	if err != nil || login.ID == pending || list.PendingLogin != login.ID || len(list.Accounts) != 1 || list.Accounts[0].ID != pending ||
		list.Accounts[0].Email != "owner@example.test" || probes[pending] != 1 {
		t.Fatalf("a new login reused an unrecorded sign-in: %+v %+v %v %v", login, list, probes, err)
	}

	// An API key login cannot become an account; the next login uses a fresh directory.
	reset()
	readings[pending] = codexGuestSnapshot{AuthMode: "api_key"}
	if login, err = s.selectCodexAccount("", true, ""); err != nil || login.ID == pending {
		t.Fatalf("a new login would overwrite an API key login: %+v %v", login, err)
	}
	if list, _ = s.loadCodexAccounts(); len(list.Accounts) != 0 || list.PendingLogin != login.ID {
		t.Fatalf("unrecordable login was added: %+v", list)
	}

	// A signed-out directory is reused; a failed check refuses the login.
	reset()
	readings[pending] = codexGuestSnapshot{}
	if login, err = s.selectCodexAccount("", true, ""); err != nil || login.ID != pending {
		t.Fatalf("signed-out pending directory was not reused: %+v %v", login, err)
	}
	reset()
	probeErr = errors.New("guest unavailable")
	if _, err = s.selectCodexAccount("", true, ""); err == nil {
		t.Fatal("started a login without checking the pending directory")
	}
	probeErr = nil

	// A login started while a probe of the pending directory runs waits for
	// it and never enters the directory that probe records.
	reset()
	readings[pending] = owner
	entered, release := make(chan struct{}), make(chan struct{})
	s.codexAccounts.probe = func(_ context.Context, id string) (codexGuestSnapshot, error) {
		mu.Lock()
		probes[id]++
		mu.Unlock()
		close(entered)
		<-release
		return owner, nil
	}
	s.codexAccounts.mu.Lock()
	s.codexAccounts.attempted = nil
	s.codexAccounts.mu.Unlock()
	background := make(chan error, 1)
	go func() { background <- s.recoverCodexPending(context.Background(), pending, false) }()
	<-entered
	started := make(chan codexSelection, 1)
	go func() {
		selection, err := s.selectCodexAccount("", true, "")
		if err != nil {
			t.Error(err)
		}
		started <- selection
	}()
	select {
	case selection := <-started:
		t.Fatalf("login did not wait for the pending check: %+v", selection)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err = <-background; err != nil {
		t.Fatal(err)
	}
	if selection := <-started; selection.ID == pending || probes[pending] != 1 {
		t.Fatalf("login entered the recorded directory: %+v %v", selection, probes)
	}

	// A background check never records a pending directory with a live login.
	reset()
	s.codexAccounts.probe = func(context.Context, string) (codexGuestSnapshot, error) { return owner, nil }
	s.terminals.sessions = map[string]*terminalSession{"login": {codexAccountID: pending, kind: "codex-login", done: make(chan struct{})}}
	if err = s.refreshCodex(context.Background(), pending, false); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.loadCodexAccounts(); len(list.Accounts) != 0 || list.PendingLogin != pending {
		t.Fatalf("background check recorded a directory under a live login: %+v", list)
	}
}

func TestCodexRefusedTerminalRechecksItsAccount(t *testing.T) {
	s := testServer(t)
	s.ready = true
	t.Cleanup(s.terminals.close)
	a := strings.Repeat("a", 24)
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{codexFixture(a, "a@example.test")}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	probes := 0
	release := make(chan struct{})
	s.codexAccounts.probe = func(context.Context, string) (codexGuestSnapshot, error) {
		mu.Lock()
		probes++
		mu.Unlock()
		<-release
		return codexGuestSnapshot{}, nil
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return probes }
	selection := codexSelection{ID: a, GuestID: a, Subject: claudeIdentityHash("a@example.test", "workspace")}
	// A normal exit after use leaves the account alone.
	session, err := s.terminals.startCodexSession(terminalChildCommand("echo"), "/project", "codex", selection, s.codexTerminalFinished(a, false))
	if err != nil {
		t.Fatal(err)
	}
	if err = session.enqueueInput(context.Background(), terminalInputLine("exit")); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, session.done)
	// The helper's refusal ends the terminal at once with a failure status.
	refused, err := s.terminals.startCodexSession(terminalChildCommand("refused"), "/project", "codex", selection, s.codexTerminalFinished(a, false))
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, refused.done)
	if code := refused.exitStatus(); code != 78 || session.exitStatus() != 0 {
		t.Fatalf("exit status: refused %d, normal %d", code, session.exitStatus())
	}
	deadline := time.Now().Add(10 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Further refusals while the check runs do not start another probe.
	s.recheckCodexAccount(a)
	s.recheckCodexAccount(a)
	close(release)
	for time.Now().Before(deadline) {
		s.codexAccounts.mu.Lock()
		busy := s.codexAccounts.refreshing[a] != nil
		s.codexAccounts.mu.Unlock()
		if list, _ := s.loadCodexAccounts(); !busy && list.Accounts[0].Status == "signed_out" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if list, _ := s.loadCodexAccounts(); list.Accounts[0].Status != "signed_out" || count() != 1 {
		t.Fatalf("refused account stayed schedulable or was probed %d times: %+v", count(), list.Accounts[0])
	}
	if _, err = s.selectCodexAccount("", false, ""); err == nil {
		t.Fatal("allocation still chose the refused account")
	}
}

func TestCodexUsageRefreshRecordsQuotaAndIdentityChanges(t *testing.T) {
	s := testServer(t)
	s.ready = true
	a := strings.Repeat("a", 24)
	if err := s.saveCodexAccounts(codexAccountList{Accounts: []codexAccount{codexFixture(a, "a@example.test")}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	report := codexGuestSnapshot{LoggedIn: true, Email: "a@example.test", Organization: "workspace", Plan: "pro", UsageStatus: "reported",
		Quota: &codexQuota{Primary: &codexWindow{UsedPercentage: 42, ResetsAt: now.Add(time.Hour).Unix(), WindowMinutes: 300}, Secondary: &codexWindow{UsedPercentage: 7, ResetsAt: now.Add(72 * time.Hour).Unix(), WindowMinutes: 10080}, ObservedAt: now.Unix()}}
	var asked []string
	s.codexAccounts.usageProbe = func(_ context.Context, id, subject string) (codexGuestSnapshot, error) {
		asked = append(asked, id+"/"+subject)
		return report, nil
	}
	if w := request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"refresh","id":"`+a+`"}`, true); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	list, _ := s.loadCodexAccounts()
	got := list.Accounts[0]
	if len(asked) != 1 || asked[0] != a+"/"+claudeIdentityHash("a@example.test", "workspace") || got.UsageStatus != "reported" || got.Plan != "pro" ||
		got.Quota == nil || got.Quota.Primary.UsedPercentage != 42 || got.Quota.Secondary.WindowMinutes != 10080 {
		t.Fatalf("usage not recorded: %+v %v", got, asked)
	}
	if room, ok := got.headroom(now); !ok || room != 58 {
		t.Fatal("headroom", room, ok)
	}
	report = codexGuestSnapshot{UsageStatus: "identity_changed"}
	if w := request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"refresh","id":"`+a+`"}`, true); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	if list, _ = s.loadCodexAccounts(); list.Accounts[0].Status != "changed" || list.Accounts[0].Quota.Primary.UsedPercentage != 42 {
		t.Fatal("identity change not recorded or quota lost", list.Accounts[0])
	}
}

func TestCodexTerminalScriptsKeepTheSelectedAccountInTheShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest Bash integration runs on Unix")
	}
	home := t.TempDir()
	const managed = "111111111111111111111111"
	command := `shim=none; [ "${PATH%%:*}" = /usr/local/lib/cage-codex-bin ] && shim=first; printf '%s' "$CODEX_HOME|$CAGE_CODEX_ACCOUNT_ID|$CAGE_CODEX_SUBJECT|$shim" > observed; printf x >> runs`
	for _, tc := range []struct{ account, want string }{
		{"", home + "/custom-codex|||none"},
		{managed, home + "/.local/share/cage/codex-accounts/" + managed + "/home|" + managed + "|expected|first"},
	} {
		work := filepath.Join(t.TempDir(), "project with ' quote")
		if err := os.Mkdir(work, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", codexTerminalScript(work, command, tc.account, "expected", false))
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "CODEX_HOME=" + home + "/custom-codex"}
		cmd.Stdin = strings.NewReader("printf shell-returned > shell-result\nexit\n")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("guest launch: %v: %s", err, b)
		}
		observed, err := os.ReadFile(filepath.Join(work, "observed"))
		if err != nil || string(observed) != tc.want {
			t.Fatalf("account environment: %q, %v", observed, err)
		}
		runs, _ := os.ReadFile(filepath.Join(work, "runs"))
		returned, _ := os.ReadFile(filepath.Join(work, "shell-result"))
		if string(runs) != "x" || string(returned) != "shell-returned" {
			t.Fatalf("command ran %q times or shell did not return: %q", runs, returned)
		}
		if tc.account != "" {
			info, err := os.Stat(filepath.Join(home, ".local/share/cage/codex-accounts", managed, "home"))
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("CODEX_HOME not private: %v %v", info, err)
			}
		}
	}
	// The helper's refusal status ends a managed terminal instead of opening a shell.
	work := t.TempDir()
	cmd := exec.Command("bash", "-c", codexTerminalScript(work, "(exit 78)", managed, "expected", false))
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader("printf shell-returned > shell-result\nexit\n")
	var exit *exec.ExitError
	if b, err := cmd.CombinedOutput(); !errors.As(err, &exit) || exit.ExitCode() != 78 {
		t.Fatalf("refusal did not end the terminal: %v: %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(work, "shell-result")); !os.IsNotExist(err) {
		t.Fatal("a shell opened after the account was refused")
	}
	if got := codexTerminalScript("/w", "codex", managed, "", true); got != `cd "$HOME" && exec /usr/local/bin/cage-codex-account login `+managed {
		t.Fatal(got)
	}
}

func TestCodexTerminalSessionsCarryTheirAccount(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	account := codexSelection{ID: strings.Repeat("a", 24), GuestID: strings.Repeat("a", 24), Subject: "subject"}
	session, err := manager.startCodexSession(terminalChildCommand("echo"), "/project", "codex", account, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary := session.summary()
	if summary["codex_account_id"] != account.ID || summary["codex_account_source"] != "managed" || summary["claude_account_id"] != "" || !strings.HasPrefix(session.title, "Codex ") {
		t.Fatalf("Codex terminal binding: %+v %q", summary, session.title)
	}
	if _, err := manager.existingCodexLogin(account.ID, true); err == nil {
		t.Fatal("started a device login while the account has a live terminal")
	}
	if existing, err := manager.existingClaudeLogin(account.ID, true); existing != nil || err != nil {
		t.Fatal("Codex terminal blocked a Claude login")
	}
	login, err := manager.startCodexSession(terminalChildCommand("echo"), "/project", "codex-login", codexSelection{ID: strings.Repeat("b", 24), GuestID: strings.Repeat("b", 24)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	view := login.subscribe()
	defer view.Close()
	if err = login.enqueueInput(context.Background(), terminalInputLine("ABCD-EFGH")); err != nil {
		t.Fatal(err)
	}
	readTerminalUntil(t, view, "child:ABCD-EFGH")
	// The one-time code must survive reconnecting the viewer.
	again := login.subscribe()
	defer again.Close()
	if !strings.Contains(string(again.History), "child:ABCD-EFGH") || login.title != "Codex 账号登录" {
		t.Fatalf("device login lost its output on reconnect: %q", again.History)
	}
	if existing, err := manager.existingCodexLogin(strings.Repeat("b", 24), true); err != nil || existing != login {
		t.Fatal("repeated login did not reattach", existing, err)
	}
}

func TestCodexTerminalPreferenceAndNativeGuests(t *testing.T) {
	s := testServer(t)
	cfg, _ := cage.Load(s.configPath)
	input := settingsView(cfg, s.preferences)
	if input.CodexTerminalCommand != "codex" || input.ClaudeTerminalCommand != "claude" {
		t.Fatal("missing defaults", input.CodexTerminalCommand, input.ClaudeTerminalCommand)
	}
	input.CodexTerminalCommand = "  codex --search  "
	_, p, err := proposedSettings(input, cfg, s.profile)
	if err != nil || p.CodexTerminalCommand != "codex --search" || p.ClaudeTerminalCommand != "claude" {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"codex\nexit", "codex\r", "codex\x00", strings.Repeat("x", 4097)} {
		input.CodexTerminalCommand = bad
		if _, _, err := proposedSettings(input, cfg, s.profile); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := s.createTerminal(terminalRequest{Kind: "shell", CodexAccountID: strings.Repeat("a", 24)}); err == nil {
		t.Fatal("a regular terminal accepted a Codex account")
	}
	s.macosGuest = &guestos.MacOSProfile{}
	for _, kind := range []string{"codex", "codex-login"} {
		if _, err := s.createTerminal(terminalRequest{Kind: kind}); err == nil || !strings.Contains(err.Error(), "Linux") {
			t.Fatalf("%s on macOS guest: %v", kind, err)
		}
	}
	if w := request(s, "POST", "/api/codex-accounts", s.origin, `{"action":"refresh","id":"`+strings.Repeat("a", 24)+`"}`, true); w.Code == http.StatusOK {
		t.Fatal("refreshed a Codex account on a native guest")
	}
}

func TestCodexAccountAgentOperations(t *testing.T) {
	found := map[string]AgentOperation{}
	for _, op := range AgentOperations() {
		found[op.Name] = op
	}
	get, change := found["codex-accounts.get"], found["codex-accounts.change"]
	if get.method != "GET" || get.path != "/api/codex-accounts" || get.Mutates || change.method != "POST" || change.path != "/api/codex-accounts" || !change.Mutates {
		t.Fatalf("Codex agent operations: %+v %+v", get, change)
	}
	properties, _ := change.InputSchema["properties"].(map[string]any)
	for _, field := range []string{"action", "id", "label", "enabled", "scheduling", "order"} {
		if properties[field] == nil {
			t.Fatalf("codex-accounts.change lacks %q: %v", field, properties)
		}
	}
	if !nativeWorkbenchRoute("/api/codex-accounts") {
		t.Fatal("native client cannot reach Codex accounts")
	}
}

func TestCodexGuestHelperWireFormat(t *testing.T) {
	python, err := exec.LookPath("python3")
	if runtime.GOOS == "windows" || err != nil {
		t.Skip("guest helper integration needs Unix and python3")
	}
	helper, err := filepath.Abs("../cage/codex-account.py")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	account := strings.Repeat("1", 24)
	codexHome := filepath.Join(home, ".local/share/cage/codex-accounts", account, "home")
	bin := filepath.Join(home, ".local/bin")
	for _, dir := range []string{codexHome, bin} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"Owner@Example.test","https://api.openai.com/auth":{"chatgpt_plan_type":"pro","chatgpt_account_id":"workspace"}}`))
	auth := `{"auth_mode":"chatgpt","tokens":{"id_token":"h.` + claims + `.s","access_token":"private-access","refresh_token":"private-refresh"}}`
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nif [ \"$1 $2\" = \"login status\" ] && [ -f \"$CODEX_HOME/auth.json\" ]; then echo 'Logged in using ChatGPT' >&2; exit 0; fi\necho 'Not logged in' >&2; exit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sessions := filepath.Join(codexHome, "sessions", now.Format("2006/01/02"))
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100,"window_minutes":300,"resets_at":%d}}}}`,
		now.Format("2006-01-02T15:04:05.000Z"), now.Add(time.Hour).Unix())
	if err := os.WriteFile(filepath.Join(sessions, "rollout-test.jsonl"), []byte(event+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(codexHome, "auth.json"), past, past); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		cmd := exec.Command(python, append([]string{helper}, args...)...)
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE=1"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(string(out), "private-") {
			t.Fatal("helper exported a token")
		}
		return out
	}
	var snapshot codexGuestSnapshot
	if err := json.Unmarshal(run("snapshot", account), &snapshot); err != nil || snapshot.subject() != claudeIdentityHash("owner@example.test", "workspace") || snapshot.Plan != "pro" {
		t.Fatalf("snapshot contract: %+v %v", snapshot, err)
	}
	if snapshot.Quota == nil || snapshot.Quota.Primary == nil || snapshot.Quota.Primary.UsedPercentage != 100 || snapshot.Quota.Primary.WindowMinutes != 300 {
		t.Fatalf("rollout quota contract: %+v", snapshot.Quota)
	}
	var quotas map[string]codexGuestQuota
	if err := json.Unmarshal(run("quotas", account), &quotas); err != nil || quotas[account].Identity != snapshot.subject() {
		t.Fatalf("quotas contract: %+v %v", quotas, err)
	}
	list := codexAccountList{Accounts: []codexAccount{{ID: account, Enabled: true}}}
	if err := list.observe(account, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := list.Accounts[0].headroom(time.Now()); ok || list.Accounts[0].Status != "ready" {
		t.Fatal("exhausted rollout window did not block scheduling", list.Accounts[0])
	}
}

func TestDefaultCodexProbeUsesRegularLoginEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest Bash integration runs on Unix")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("echo startup-banner\nexport CODEX_HOME=/shell/codex\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "show-environment"), []byte("#!/bin/sh\nprintf '%s' \"$CODEX_HOME|$1|$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(codexGuestReportCommand("default", ""), "/usr/local/bin/cage-codex-account", `"$HOME/show-environment"`, 1)
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	if b, err := cmd.Output(); err != nil || string(b) != "/shell/codex|snapshot|default" {
		t.Fatalf("login env or JSON stream changed: %q, %v", b, err)
	}
	if got := codexGuestReportCommand(strings.Repeat("a", 24), strings.Repeat("b", 64)); got != "exec /usr/local/bin/cage-codex-account usage "+strings.Repeat("a", 24)+" "+strings.Repeat("b", 64) {
		t.Fatal(got)
	}
}
