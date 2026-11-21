package workbench

// Tests for the third review of the update policy
// (review3-update/findings.md). Each one failed on the reviewed code.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
	"github.com/sylvinus/agent-vm/internal/version"
)

// H1: an agent may read the update state and look for a version
// (appUpdate status and check), never download, install or cancel; nothing
// outside the registry in agent_access.go is queued for the window.
func TestReviewAgentCannotQueueAppUpdate(t *testing.T) {
	s := agentTestController(t)
	for _, refused := range []string{
		`{"action":"appUpdate","operation":"install","confirmed":["job"]}`,
		`{"action":"appUpdate","operation":"download"}`,
		`{"action":"appUpdate","operation":"cancel"}`,
		`{"action":"workbenchReady"}`,
		`{"action":"quitNow"}`,
		`{"action":"clipboardRead"}`,
		`{"action":"browserDownloadAction","id":"x","operation":"open"}`,
		`{"action":"frobnicate"}`,
		`{"action":"accountDevice","operation":"unlock","kind":"automatic"}`,
		`{"action":"accountDevice","operation":"enable","kind":"automatic"}`,
	} {
		input := json.RawMessage(`{"connection_id":"local","request":` + refused + `}`)
		if data, err := AgentCall(context.Background(), s.configPath, "native.submit", input); err == nil {
			t.Errorf("queued %s: %s", refused, data)
		}
	}
	for _, allowed := range []string{
		`{"action":"accountDevice","operation":"status"}`,
		`{"action":"accountDevice","operation":"unlock","kind":"biometric"}`,
		`{"action":"clientPreferences","operation":"get"}`,
		// Reading the update state and looking for a version never install
		// anything (agent_access.go).
		`{"action":"appUpdate","operation":"status"}`,
		`{"action":"appUpdate","operation":"check"}`,
	} {
		input := json.RawMessage(`{"connection_id":"local","request":` + allowed + `}`)
		if _, err := AgentCall(context.Background(), s.configPath, "native.submit", input); err != nil {
			t.Errorf("refused %s: %v", allowed, err)
		}
	}
}

// L10: the confirmation list (and the guest read behind it) is the window's.
func TestReviewAgentCannotReadInterruptions(t *testing.T) {
	s := agentTestController(t)
	_, err := AgentRequest(context.Background(), s.configPath, AgentTarget{}, "GET", "/api/app-update/interruptions", nil, "", "")
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Code != "native_transport_required" {
		t.Fatalf("agent read the confirmation list: %v", err)
	}
}

func saveReviewChain(t *testing.T, f *updateFixture, c policy.Chain, at time.Time, attempt string) {
	t.Helper()
	c.Schema, c.StartedAt, c.UpdatedAt = policy.ChainSchema, at, at
	if c.ID == "" {
		c.ID = "0123456789abcdef"
	}
	if err := policy.SaveChain(f.s.updateProfile(), &c); err != nil {
		t.Fatal(err)
	}
	ledger := policy.Ledger{}
	ledger.Begin(policy.Attempt{ID: c.ID, Version: c.To, From: c.From, Trigger: c.Trigger, StartedAt: at})
	if attempt != "" {
		ledger.Finish(c.ID, c.To, attempt == "ok", policy.StepDone, "", at)
	}
	if err := policy.SaveLedger(f.s.updateProfile(), ledger); err != nil {
		t.Fatal(err)
	}
}

type chainCounters struct{ starts, syncs, networks atomic.Int32 }

func countChainSteps(f *updateFixture) *chainCounters {
	c := &chainCounters{}
	f.s.appUpdate.start = func() error { c.starts.Add(1); return nil }
	f.s.appUpdate.sync = func() error { c.syncs.Add(1); return nil }
	f.s.appUpdate.network = func() error { c.networks.Add(1); return nil }
	return c
}

func writeInstallerResult(t *testing.T, f *updateFixture, value map[string]any) {
	t.Helper()
	data, _ := json.Marshal(value)
	if err := os.WriteFile(filepath.Join(policy.Dir(f.s.updateProfile()), "last-result.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// M3: a record from long ago is never acted on, even when the version matches.
func TestReviewStaleChainRecordIsRetired(t *testing.T) {
	f := newUpdateFixture(t)
	version.Version = "0.1.16"
	counts := countChainSteps(f)
	saveReviewChain(t, f, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.Click, Step: policy.StepInstalling,
		Environment: policy.Environment{Running: true}}, f.now.Add(-30*24*time.Hour), "")
	f.s.resumeAppUpdateChain()
	c, _ := policy.LoadChain(f.s.updateProfile())
	if counts.starts.Load() != 0 || c == nil || c.Step == policy.StepDone {
		t.Fatalf("a 30-day-old record started the environment: starts %d chain %+v", counts.starts.Load(), c)
	}
}

// M1: the new version marks the attempt ok only after the installer's own
// result confirms its launch.
func TestReviewAttemptOkOnlyAfterInstallerConfirms(t *testing.T) {
	f := newUpdateFixture(t)
	version.Version = "0.1.16"
	countChainSteps(f)
	saveReviewChain(t, f, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.SilentUpdate, Step: policy.StepInstalling}, f.now, "")
	f.s.resumeAppUpdateChain()
	ledger, _ := policy.LoadLedger(f.s.updateProfile())
	if last := ledger.Last(); last == nil || last.Result == "ok" {
		t.Fatalf("attempt marked ok without the installer's confirmation: %+v", last)
	}
}

// M1: a rollback after done is a failure of that version.
func TestReviewRollbackAfterDoneIsAFailure(t *testing.T) {
	f := newUpdateFixture(t)
	version.Version = "0.1.15"
	countChainSteps(f)
	saveReviewChain(t, f, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.SilentUpdate, Step: policy.StepDone}, f.now, "ok")
	f.s.resumeAppUpdateChain()
	ledger, _ := policy.LoadLedger(f.s.updateProfile())
	if !ledger.FailedBefore("0.1.16") {
		t.Fatalf("rollback after done left the version eligible: %+v", ledger.Attempts)
	}
}

// M3: a record that stopped at stopping proves nothing about the
// environment; it is retired with a note and never acted on.
func TestReviewStoppingRecordIsNotActedOn(t *testing.T) {
	f := newUpdateFixture(t)
	counts := countChainSteps(f)
	saveReviewChain(t, f, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.SilentUpdate, Step: policy.StepStopping,
		Environment: policy.Environment{Running: true}}, f.now, "")
	f.s.resumeAppUpdateChain()
	if counts.starts.Load() != 0 {
		t.Fatal("a stopping record started the environment")
	}
}

// M3: only a result naming the attempt proves a rollback.
func TestReviewRollbackNeedsTheAttemptID(t *testing.T) {
	f := newUpdateFixture(t)
	counts := countChainSteps(f)
	saveReviewChain(t, f, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.SilentUpdate, Step: policy.StepInstalling,
		Environment: policy.Environment{Running: true}}, f.now, "")
	writeInstallerResult(t, f, map[string]any{"success": false, "version": "0.1.16", "message": "安装验证失败，已恢复原应用"})
	f.s.resumeAppUpdateChain()
	if counts.starts.Load() != 0 {
		t.Fatal("a result without the attempt id restored the environment")
	}
}

// M3: a network pause set after the record was written is kept.
func TestReviewPauseSetAfterRecordIsKept(t *testing.T) {
	f := newUpdateFixture(t)
	version.Version = "0.1.16"
	counts := countChainSteps(f)
	saveReviewChain(t, f, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.Click, Step: policy.StepNetwork,
		Environment: policy.Environment{Running: true}}, f.now, "")
	f.s.mu.Lock()
	f.s.networkPaused = true
	f.s.mu.Unlock()
	f.s.resumeAppUpdateChain()
	if counts.networks.Load() != 0 {
		t.Fatal("the chain ran the network check over a pause the person set")
	}
}

// L5: the tick and the decision use the same hold.
func TestReviewTickAndDecisionAgreeOnHold(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	ctx := context.Background()
	f.s.appUpdateTick(ctx)
	f.advance(policy.HoldPeriod - 2*time.Second)
	f.s.appUpdateTick(ctx)
	if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); err != nil {
		t.Fatalf("held 59m58s, which the decision accepts, but the tick did not ask: %v", err)
	}
}

// L6: the final look is not starved by a slow decision read.
func TestReviewFinalLookHasItsOwnTime(t *testing.T) {
	f := newUpdateFixture(t)
	// A decision read that uses up the decision's whole time.
	f.s.updateRegistry().RegisterDecision("slow", "无法确认慢速来源", func(ctx context.Context) (policy.Reading, error) {
		<-ctx.Done()
		return policy.Reading{}, ctx.Err()
	})
	if code, reply := f.handoff(t, "click", "", "unknown:slow"); code != 200 {
		t.Fatalf("confirmed click refused after a slow guest read: %d %+v", code, reply)
	}
}

// L7: an unreadable attempts file is kept aside, not replaced.
func TestReviewUnreadableLedgerIsKeptAside(t *testing.T) {
	f := newUpdateFixture(t)
	dir := policy.Dir(f.s.updateProfile())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempts.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, reply := f.handoff(t, "click", ""); code != 200 {
		t.Fatalf("click: %d %+v", code, reply)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "attempts.unreadable-*.json"))
	if len(kept) != 1 {
		t.Fatalf("unreadable record not kept aside: %v", kept)
	}
	if data, _ := os.ReadFile(kept[0]); string(data) != "{broken" {
		t.Fatalf("kept copy differs: %q", data)
	}
}

// L8: a remote connection is a blocker, as the settings text says.
func TestReviewRemoteConnectionBlocks(t *testing.T) {
	f := newUpdateFixture(t)
	request(f.s, "GET", "/api/settings", "", "", true)
	snapshot := f.s.updateRegistry().Snapshot(context.Background(), time.Now(), false)
	for _, b := range snapshot.Blockers {
		if b.Kind == policy.KindRemote {
			return
		}
	}
	t.Fatalf("a remote session did not block: %+v", snapshot.Blockers)
}

// L1: the guest's answer cannot choose blocker ids or put arbitrary text in
// the confirmation.
func TestReviewGuestCannotChooseIDsOrLabels(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
	var agents []string
	for i := 0; i < 200; i++ {
		agents = append(agents, "evil name "+strings.Repeat("x", i))
	}
	agents = append(agents, "claude", "codex")
	shells := []map[string]any{}
	for i := 0; i < 50; i++ {
		shells = append(shells, map[string]any{"sid": 1000 + i*7, "foreground": "vim\u001b[31m<b>" + strings.Repeat("长", 300)})
	}
	data, _ := json.Marshal(map[string]any{"shells": shells, "agents": agents, "services": map[string]bool{}})
	reading, err := appUpdateGuestBlockers(data, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(reading.Blockers) > 3+4 {
		t.Fatalf("guest chose the number of blockers: %d", len(reading.Blockers))
	}
	for _, b := range reading.Blockers {
		if !pattern.MatchString(b.ID) {
			t.Fatalf("guest-made id %q", b.ID)
		}
		if utf8.RuneCountInString(b.Label) > 80 || strings.ContainsAny(b.Label, "<>") || strings.IndexFunc(b.Label, unicode.IsControl) >= 0 {
			t.Fatalf("guest text in the dialog: %q", b.Label)
		}
	}
	one := func(foreground string) string {
		data, _ := json.Marshal(map[string]any{"shells": []map[string]any{{"sid": 41, "foreground": foreground}}, "agents": []string{}, "services": map[string]bool{}})
		reading, err := appUpdateGuestBlockers(data, 1)
		if err != nil || len(reading.Blockers) != 1 {
			t.Fatalf("%v %+v", err, reading)
		}
		return reading.Blockers[0].ID
	}
	if one("vim") == one("make") {
		t.Fatal("confirming vim would also confirm the build that replaced it")
	}
}

// L2: the seal is released on every path, also a panic.
func TestReviewSealReleasedOnPanic(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	req := f.silentRequestFromTicks(t)
	f.s.appUpdate.inputIdle = func(context.Context) (time.Duration, error) { panic("boom") }
	func() {
		defer func() { _ = recover() }()
		f.handoff(t, "silent", req.ID)
	}()
	f.s.mu.Lock()
	sealed := f.s.appUpdateSealed
	f.s.mu.Unlock()
	if sealed {
		t.Fatal("a panic left new work refused until restart")
	}
	if !f.s.op.TryLock() {
		t.Fatal("a panic kept the operation lock")
	}
	f.s.op.Unlock()
}

// The unattended path ships closed on every platform until its chain has
// run on a real machine.
func TestReviewSilentGateClosedEverywhere(t *testing.T) {
	yes := func(string) bool { return true }
	for goos, exe := range map[string]string{
		"darwin":  "/Applications/Super Cage.app/Contents/Resources/cage",
		"windows": `C:\Users\me\AppData\Local\Programs\Super Cage\cage.exe`,
		"linux":   "/usr/bin/cage",
	} {
		if reason := silentUnavailableReason(goos, "0.1.15", exe, yes); reason == "" {
			t.Errorf("silent offered on %s before its real-machine run", goos)
		}
	}
}

// M5: with a shell that never gets as far as the decision, silent mode
// does not download the same release every hour.
func TestReviewSilentDownloadsAreBounded(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	ctx := context.Background()
	requests := map[string]bool{}
	path := filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)
	for hour := 0; hour < 72; hour++ {
		f.s.appUpdateTick(ctx)
		var r silentRequest
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &r) == nil {
			requests[r.ID] = true
		}
		f.advance(time.Hour)
	}
	if len(requests) > 4 {
		t.Fatalf("%d unattended downloads of one release in 72 hours", len(requests))
	}
}

// L3: a stop that outlives its time keeps the operation until it ends, and
// it begins like the stop button (the network monitor ends first).
func TestReviewStopTimeoutKeepsTheOperation(t *testing.T) {
	f := newUpdateFixture(t)
	a := &f.s.appUpdate
	a.stop = nil
	release := make(chan struct{})
	var monitor atomic.Int32
	f.s.mu.Lock()
	f.s.monitorCancel = func() { monitor.Add(1) }
	f.s.mu.Unlock()
	a.stopRun = func() error { <-release; return nil }
	a.stopTimeout = 50 * time.Millisecond
	f.s.op.Lock()
	ended, err := f.s.appUpdateStop(true)
	if err == nil || ended == nil {
		t.Fatalf("stop past its time: %v %v", err, ended)
	}
	// Like the stop button: once when the network pauses, once as the stop
	// job's own preparation.
	if monitor.Load() != 2 {
		t.Fatalf("the update's stop skipped the stop button's preparation: %d", monitor.Load())
	}
	var after atomic.Int32
	f.s.releaseAfterStop(ended, func() { after.Add(1) })
	time.Sleep(50 * time.Millisecond)
	if f.s.op.TryLock() || after.Load() != 0 {
		t.Fatal("the operation was given back while the stop still ran")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !f.s.op.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("the operation was never given back")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.s.op.Unlock()
	for after.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after.Load() != 1 {
		t.Fatal("the environment was not brought back after the stop ended")
	}
}

// M5: files that failed verification are never fetched again unattended;
// changed files are looked at again at once.
func TestReviewShellReportEndsOrRefreshesSilentRequests(t *testing.T) {
	for _, tc := range []struct {
		stage, code string
		again       bool
	}{
		{"verify", "verification", false},
		{"download", "verification", false},
		{"download", "changed", true},
	} {
		t.Run(tc.stage+"/"+tc.code, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.setMode(t, policy.Silent)
			req := f.silentRequestFromTicks(t)
			outcome, _ := json.Marshal(map[string]string{"request": req.ID, "version": req.Version, "release": req.Release, "stage": tc.stage, "code": tc.code, "message": "签名无效"})
			if err := os.WriteFile(filepath.Join(policy.Dir(f.s.updateProfile()), silentOutcomeFile), outcome, 0o600); err != nil {
				t.Fatal(err)
			}
			checks := f.checks.Load()
			requests := map[string]bool{}
			path := filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)
			for hour := 0; hour < 48; hour++ {
				f.advance(time.Hour)
				f.s.appUpdateTick(context.Background())
				var r silentRequest
				if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &r) == nil && r.ID != req.ID {
					requests[r.ID] = true
				}
			}
			if (len(requests) > 0) != tc.again {
				t.Fatalf("requests after the report: %d", len(requests))
			}
			if tc.again && f.checks.Load() == checks {
				t.Fatal("changed files were not looked at again")
			}
			if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentOutcomeFile)); !os.IsNotExist(err) {
				t.Fatal("report left behind")
			}
		})
	}
}

// M6: the age counts from the newest file, not from the publication.
func TestReviewReplacedFilesAreTooNew(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	f.s.appUpdate.check = func(context.Context) (*appupdate.Available, error) {
		return &appupdate.Available{Version: "0.1.16", PublishedAt: f.now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			ChangedAt: f.now.Add(-time.Hour).Format(time.RFC3339), Release: fixtureRelease, Installable: true}, nil
	}
	ctx := context.Background()
	f.s.appUpdateTick(ctx)
	f.advance(policy.HoldPeriod)
	f.s.appUpdateTick(ctx)
	if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); !os.IsNotExist(err) {
		t.Fatal("a release whose image was replaced an hour ago was requested")
	}
	if w := f.s.appUpdateStatusView().Waiting; w == nil || w.Code != policy.TooNew {
		t.Fatalf("waiting %+v", w)
	}
}

// H2: an unattended request is never made in a dark wake, with the lid
// closed, on a low battery, while another app keeps the display on, or when
// any of that cannot be read; the decision at install time looks again.
func TestReviewHostConditionsRefuseSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		host *policy.HostState
		err  error
		code policy.Code
	}{
		{"dark wake", &policy.HostState{DarkWake: true}, nil, policy.DarkWake},
		{"lid closed", &policy.HostState{LidClosed: true}, nil, policy.LidClosed},
		{"low battery", &policy.HostState{OnBattery: true, Battery: 20}, nil, policy.LowBattery},
		{"a call keeps the display on", &policy.HostState{DisplayHeld: []string{"zoom.us"}}, nil, policy.DisplayHeld},
		{"unreadable", nil, errors.New("pmset failed"), policy.HostUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.setMode(t, policy.Silent)
			req := f.silentRequestFromTicks(t)
			f.s.appUpdate.host = func(context.Context) (*policy.HostState, error) { return tc.host, tc.err }
			code, reply := f.handoff(t, "silent", req.ID)
			if code != 409 || reply.Code != tc.code || f.stops.Load() != 0 {
				t.Fatalf("handoff %d %+v", code, reply)
			}
			f.advance(2 * policy.HoldPeriod)
			f.s.appUpdateTick(context.Background())
			if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); !os.IsNotExist(err) {
				t.Fatal("the tick asked anyway")
			}
		})
	}
}

func TestHostConditionsParse(t *testing.T) {
	full := []byte("Current System Capabilities are: CPU Graphics Audio Network \nCurrent Power State: 4\n")
	dark := []byte("Current System Capabilities are: CPU Network \nCurrent Power State: 4\n")
	ac := []byte("Now drawing from 'AC Power'\n -InternalBattery-0 (id=24379491)\t44%; charging; 8:44 remaining present: true\n")
	battery := []byte("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=24379491)\t31%; discharging; 2:10 remaining present: true\n")
	desktop := []byte("Now drawing from 'AC Power'\n")
	assertions := []byte(`Assertion status system-wide:
   PreventUserIdleDisplaySleep    1
Listed by owning process:
   pid 21066(caffeinate): [0x0003277d00019cee] 00:03:11 PreventUserIdleSystemSleep named: "caffeinate command-line tool"
   pid 372(powerd): [0x0002dab900108921] 00:00:02 InternalPreventDisplaySleep named: "com.apple.powermanagement.delayDisplayOff"
   pid 372(powerd): [0x0002dab900108922] 00:00:02 PreventUserIdleDisplaySleep named: "powerd"
   pid 900(zoom.us): [0x0002dab900108923] 00:10:00 PreventUserIdleDisplaySleep named: "zoom call"
   pid 901(Keynote): [0x0002dab900108924] 00:10:00 NoDisplaySleepAssertion named: "slideshow"
   pid 77(cage): [0x0002dab900108925] 00:10:00 PreventUserIdleDisplaySleep named: "self"
Kernel Assertions: 0x100=MAGICWAKE
`)
	quiet := []byte("Listed by owning process:\n   pid 21066(caffeinate): [0x1] 00:03:11 PreventUserIdleSystemSleep named: \"x\"\n")
	open := []byte("+-o IOPMrootDomain  <class IOPMrootDomain>\n      \"AppleClamshellCausesSleep\" = No\n      \"AppleClamshellState\" = No\n")
	closed := []byte("+-o IOPMrootDomain  <class IOPMrootDomain>\n      \"AppleClamshellCausesSleep\" = Yes\n      \"AppleClamshellState\" = Yes\n")
	clamshellMode := []byte("+-o IOPMrootDomain  <class IOPMrootDomain>\n      \"AppleClamshellCausesSleep\" = No\n      \"AppleClamshellState\" = Yes\n")
	noLid := []byte("+-o IOPMrootDomain  <class IOPMrootDomain>\n")
	own := map[int]bool{77: true}

	state, err := parseHostConditions(full, ac, quiet, open, own)
	if err != nil || state.DarkWake || state.LidClosed || state.OnBattery || len(state.DisplayHeld) != 0 {
		t.Fatalf("idle Mac on power: %+v %v", state, err)
	}
	if state, err = parseHostConditions(dark, desktop, quiet, noLid, own); err != nil || !state.DarkWake || state.LidClosed {
		t.Fatalf("dark wake: %+v %v", state, err)
	}
	if state, err = parseHostConditions(full, battery, assertions, closed, own); err != nil || !state.OnBattery || state.Battery != 31 || !state.LidClosed ||
		strings.Join(state.DisplayHeld, ",") != "Keynote,zoom.us" {
		t.Fatalf("battery, lid, display: %+v %v", state, err)
	}
	if state, err = parseHostConditions(full, ac, quiet, clamshellMode, own); err != nil || state.LidClosed {
		t.Fatalf("clamshell mode with a display: %+v %v", state, err)
	}
	for _, tc := range [][4][]byte{
		{[]byte(""), ac, quiet, open},
		{full, []byte("No power source"), quiet, open},
		{full, []byte("Now drawing from 'Battery Power'\n"), quiet, open},
		{full, ac, []byte("Assertion status system-wide:\n"), open},
		{full, ac, quiet, []byte("")},
	} {
		if _, err := parseHostConditions(tc[0], tc[1], tc[2], tc[3], own); err == nil {
			t.Fatalf("accepted %q", tc)
		}
	}
}

// A request file of an earlier process is gone after a start.
func TestReviewStaleSilentRequestRemovedAtStart(t *testing.T) {
	f := newUpdateFixture(t)
	path := filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)
	if err := policy.WriteRecord(f.s.updateProfile(), silentRequestFile, map[string]any{"id": "0123456789abcdef", "version": "0.1.16"}); err != nil {
		t.Fatal(err)
	}
	f.s.resumeAppUpdateChain()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a request no process stands behind survived the start")
	}
}

// Manual mode costs nothing on the hourly tick: no allocation, no file.
func TestManualTickAllocatesNothing(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Manual)
	ctx := context.Background()
	if allocs := testing.AllocsPerRun(100, func() { f.s.appUpdateTick(ctx) }); allocs != 0 {
		t.Fatalf("manual tick allocates %v", allocs)
	}
}

// M2: prepare-update names the attempt as soon as the environment stopped,
// before the workload's exit or the release can fail, and a failure after
// that is an abandonment that names the same attempt.
func TestReviewStopIsToldFirstAndAbandonmentNamesTheAttempt(t *testing.T) {
	reply := AppUpdatePrepared{Status: "prepared", Attempt: "0123456789abcdef"}
	failure := errors.New("后台尚未退出")
	for _, c := range []struct {
		name           string
		exit, release  error
		status, events string
	}{
		{"exit fails", failure, nil, "abandoned", "stopped:0123456789abcdef,exit"},
		{"release fails", nil, failure, "abandoned", "stopped:0123456789abcdef,exit,release"},
		{"prepared", nil, nil, "prepared", "stopped:0123456789abcdef,exit,release"},
	} {
		var events []string
		result, err := finishAppUpdateHandoff(reply,
			func(attempt string) { events = append(events, "stopped:"+attempt) },
			func() error { events = append(events, "exit"); return c.exit },
			func() error { events = append(events, "release"); return c.release })
		if got := strings.Join(events, ","); got != c.events {
			t.Errorf("%s: events %s, want %s", c.name, got, c.events)
		}
		if result.Status != c.status || result.Attempt != reply.Attempt || (c.status == "abandoned") != (err != nil) {
			t.Errorf("%s: %+v %v", c.name, result, err)
		}
	}
}
