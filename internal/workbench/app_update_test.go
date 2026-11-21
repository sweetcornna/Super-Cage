package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/appupdate/policy"
	"github.com/sylvinus/agent-vm/internal/remotehost"
	"github.com/sylvinus/agent-vm/internal/version"
)

// updateFixture is a workbench with every update seam replaced: no guest,
// no host input reader, no network, no real stop.
type updateFixture struct {
	s        *Server
	now      time.Time
	stops    atomic.Int32
	shutdown atomic.Int32
	guests   atomic.Int32
	checks   atomic.Int32
	idle     time.Duration
	guest    func(context.Context) (policy.Reading, error)
	mu       sync.Mutex
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	previous := version.Version
	version.Version = "0.1.15"
	t.Cleanup(func() { version.Version = previous })
	f := &updateFixture{s: testServer(t), now: time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC), idle: 2 * policy.UserIdle}
	s := f.s
	s.shutdown = func() { f.shutdown.Add(1) }
	a := &s.appUpdate
	a.clock = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	a.inputIdle = func(context.Context) (time.Duration, error) { return f.idle, nil }
	a.guest = func(ctx context.Context) (policy.Reading, error) {
		f.guests.Add(1)
		if f.guest != nil {
			return f.guest(ctx)
		}
		return policy.Reading{}, nil
	}
	a.check = func(context.Context) (*appupdate.Available, error) {
		f.checks.Add(1)
		return &appupdate.Available{Version: "0.1.16", PublishedAt: f.now.Add(-48 * time.Hour).Format(time.RFC3339), Installable: true}, nil
	}
	a.stop = func() error { f.stops.Add(1); return nil }
	a.unavailable = func() string { return "" }
	return f
}

func (f *updateFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *updateFixture) setMode(t *testing.T, mode policy.Mode) {
	t.Helper()
	if err := policy.SaveSettings(f.s.updateProfile(), policy.Settings{Mode: mode, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	f.s.appUpdate.mu.Lock()
	f.s.appUpdate.mode = mode
	f.s.appUpdate.mu.Unlock()
}

func (f *updateFixture) handoff(t *testing.T, trigger, requestID string, confirmed ...string) (int, AppUpdatePrepared) {
	t.Helper()
	body, _ := json.Marshal(appUpdateHandoffRequest{Token: f.s.token, Version: "0.1.16", Trigger: trigger, Request: requestID, Confirmed: confirmed})
	w := request(f.s, "POST", "/api/desktop-local/app-update-handoff", f.s.origin, string(body), false)
	var reply AppUpdatePrepared
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	return w.Code, reply
}

// silentRequestFromTicks runs two idle hourly ticks and returns the request
// the second one wrote for the native shell.
func (f *updateFixture) silentRequestFromTicks(t *testing.T) silentRequest {
	t.Helper()
	ctx := context.Background()
	f.s.appUpdateTick(ctx)
	if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); !os.IsNotExist(err) {
		t.Fatal("first idle tick requested an installation before idle held")
	}
	f.advance(time.Hour)
	f.s.appUpdateTick(ctx)
	data, err := os.ReadFile(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile))
	if err != nil {
		t.Fatalf("second idle tick wrote no request: %v; waiting=%+v", err, f.s.appUpdateStatusView().Waiting)
	}
	var written silentRequest
	if err = json.Unmarshal(data, &written); err != nil || written.ID == "" || written.Version != "0.1.16" {
		t.Fatalf("request %s: %v", data, err)
	}
	if strings.Contains(string(data), "fingerprint") {
		t.Fatal("the activity fingerprint left the process")
	}
	return written
}

func (f *updateFixture) assertNothingInstalled(t *testing.T) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if f.stops.Load() != 0 || f.shutdown.Load() != 0 {
		t.Fatalf("stopped=%d shutdown=%d", f.stops.Load(), f.shutdown.Load())
	}
	f.s.mu.Lock()
	sealed, closing := f.s.appUpdateSealed, f.s.closing
	f.s.mu.Unlock()
	if sealed || closing {
		t.Fatalf("refusal left admission closed: sealed=%t closing=%t", sealed, closing)
	}
	if ledger, err := policy.LoadLedger(f.s.updateProfile()); err != nil || len(ledger.Attempts) != 0 {
		t.Fatalf("refusal recorded an attempt: %+v %v", ledger, err)
	}
	if c, err := policy.LoadChain(f.s.updateProfile()); err != nil || c != nil {
		t.Fatalf("refusal began a chain: %+v %v", c, err)
	}
}

func TestSilentTickDoesNothingInManualMode(t *testing.T) {
	f := newUpdateFixture(t)
	for range 3 {
		f.s.appUpdateTick(context.Background())
		f.advance(time.Hour)
	}
	if f.checks.Load() != 0 || f.guests.Load() != 0 {
		t.Fatalf("manual mode checked=%d read guest=%d", f.checks.Load(), f.guests.Load())
	}
	if _, err := os.Stat(policy.Dir(f.s.updateProfile())); !os.IsNotExist(err) {
		t.Fatalf("manual mode wrote update files: %v", err)
	}
}

func TestAppUpdateHandoffManualModeNeverInstallsSilently(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	written := f.silentRequestFromTicks(t)
	// The person switches back to manual after the request was written.
	w := trustedUpdateRequest(f.s, "POST", "/api/app-update/policy", `{"mode":"manual"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); !os.IsNotExist(err) {
		t.Fatal("switching to manual left the request for the shell")
	}
	code, reply := f.handoff(t, "silent", written.ID)
	if code != http.StatusConflict || reply.Status != "blocked" {
		t.Fatalf("manual mode installed: %d %+v", code, reply)
	}
	// Even a request id the process still remembered is refused by mode.
	f.s.appUpdate.mu.Lock()
	f.s.appUpdate.request = &silentRequest{ID: written.ID, Version: "0.1.16", DecidedAt: f.now, fingerprint: f.s.updateRegistry().Snapshot(context.Background(), f.now, false).Fingerprint}
	f.s.appUpdate.hold = policy.HoldTracker{Since: f.now.Add(-2 * time.Hour), Fingerprint: f.s.appUpdate.request.fingerprint}
	f.s.appUpdate.mu.Unlock()
	code, reply = f.handoff(t, "silent", written.ID)
	if code != http.StatusConflict || reply.Code != policy.ManualMode {
		t.Fatalf("manual mode answered %d %+v", code, reply)
	}
	f.assertNothingInstalled(t)
}

func TestAppUpdateSilentInstallsOnlyAfterIdleHeld(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	written := f.silentRequestFromTicks(t)
	if code, reply := f.handoff(t, "silent", "forged"); code != http.StatusConflict || reply.Code != policy.Changed {
		t.Fatalf("unknown request id: %d %+v", code, reply)
	}
	code, reply := f.handoff(t, "silent", written.ID)
	if code != http.StatusOK || reply.Status != "prepared" {
		t.Fatalf("idle silent update refused: %d %+v", code, reply)
	}
	deadline := time.Now().Add(time.Second)
	for f.shutdown.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.stops.Load() != 1 || f.shutdown.Load() != 1 {
		t.Fatalf("stopped=%d shutdown=%d", f.stops.Load(), f.shutdown.Load())
	}
	ledger, _ := policy.LoadLedger(f.s.updateProfile())
	last := ledger.Last()
	if last == nil || last.Trigger != policy.SilentUpdate || last.Result != "" || !ledger.FailedBefore("0.1.16") {
		t.Fatalf("attempt not recorded as unfinished before install: %+v", last)
	}
	c, err := policy.LoadChain(f.s.updateProfile())
	if err != nil || c == nil || c.Step != policy.StepInstalling || c.From != "0.1.15" || c.To != "0.1.16" {
		t.Fatalf("chain %+v %v", c, err)
	}
	if _, err = f.s.submitJob("start", func() error { return nil }); !errors.Is(err, errJobsClosed) {
		t.Fatalf("new job after prepared: %v", err)
	}
}

func TestAppUpdateBlockerAppearingAfterRequestWins(t *testing.T) {
	for _, tc := range []struct {
		name   string
		appear func(*updateFixture)
		want   policy.Code
	}{
		{"job started", func(f *updateFixture) { f.s.setJobForTest(Job{ID: "j", Kind: "start", Running: true}) }, policy.Busy},
		{"phone connected", func(f *updateFixture) { f.s.notePhoneRequest() }, policy.Busy},
		{"work came and went", func(f *updateFixture) { f.s.noteAppUpdateActivity() }, policy.Changed},
		{"guest command", func(f *updateFixture) {
			f.guest = func(context.Context) (policy.Reading, error) {
				return policy.Reading{Blockers: []policy.Blocker{{ID: "terminal-command:7", Kind: policy.KindTerminal, Interruptible: true, Label: "终端里还有命令在运行（make）"}}}, nil
			}
		}, policy.Busy},
		{"person returned", func(f *updateFixture) { f.idle = time.Minute }, policy.UserActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.setMode(t, policy.Silent)
			written := f.silentRequestFromTicks(t)
			tc.appear(f)
			code, reply := f.handoff(t, "silent", written.ID)
			if code != http.StatusConflict || reply.Code != tc.want {
				t.Fatalf("got %d %+v, want %s", code, reply, tc.want)
			}
			f.assertNothingInstalled(t)
			if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); !os.IsNotExist(err) {
				t.Fatal("refused request stays for the shell")
			}
			// A second try with the same request is refused without a new decision.
			if code, reply = f.handoff(t, "silent", written.ID); code != http.StatusConflict || reply.Code != policy.Changed {
				t.Fatalf("refused request reused: %d %+v", code, reply)
			}
		})
	}
}

func TestAppUpdateBlockerDuringDecisionWins(t *testing.T) {
	// Work the seal does not cover (a phone request) arrives while the guest
	// is being read: the final look after closing admission catches it.
	for _, trigger := range []string{"click", "silent"} {
		t.Run(trigger, func(t *testing.T) {
			f := newUpdateFixture(t)
			requestID := ""
			if trigger == "silent" {
				f.setMode(t, policy.Silent)
				requestID = f.silentRequestFromTicks(t).ID
			}
			f.guest = func(context.Context) (policy.Reading, error) {
				f.s.notePhoneRequest()
				return policy.Reading{}, nil
			}
			code, reply := f.handoff(t, trigger, requestID)
			if code != http.StatusConflict || reply.Status != "blocked" {
				t.Fatalf("late phone did not win: %d %+v", code, reply)
			}
			f.assertNothingInstalled(t)
		})
	}
}

func TestAppUpdateClickNeedsConfirmationOfWhatItInterrupts(t *testing.T) {
	f := newUpdateFixture(t)
	busy := policy.Blocker{ID: "terminal-command:7", Kind: policy.KindTerminal, Interruptible: true, Label: "终端里还有命令在运行（make）"}
	f.guest = func(context.Context) (policy.Reading, error) {
		return policy.Reading{Blockers: []policy.Blocker{busy}}, nil
	}
	code, reply := f.handoff(t, "click", "")
	if code != http.StatusConflict || reply.Code != policy.ConfirmRequired || len(reply.Blockers) != 1 || reply.Blockers[0].ID != busy.ID {
		t.Fatalf("unconfirmed click: %d %+v", code, reply)
	}
	f.assertNothingInstalled(t)
	f.s.setJobForTest(Job{ID: "j", Kind: "storage", Running: true})
	if code, reply = f.handoff(t, "click", "", busy.ID, "job:storage"); code != http.StatusConflict || reply.Code != policy.MustWait {
		t.Fatalf("confirmed click interrupted a must-wait job: %d %+v", code, reply)
	}
	f.assertNothingInstalled(t)
	f.s.setJobForTest(Job{ID: "j", Kind: "storage"})
	// Manual mode does not matter to a click.
	if code, reply = f.handoff(t, "click", "", busy.ID); code != http.StatusOK {
		t.Fatalf("confirmed click refused: %d %+v", code, reply)
	}
	ledger, _ := policy.LoadLedger(f.s.updateProfile())
	if last := ledger.Last(); last == nil || last.Trigger != policy.Click {
		t.Fatalf("click attempt %+v", last)
	}
}

func TestAppUpdateSealRefusesNewWorkDuringDecision(t *testing.T) {
	f := newUpdateFixture(t)
	var jobErr, uploadErr error
	f.guest = func(context.Context) (policy.Reading, error) {
		_, jobErr = f.s.submitJob("start", func() error { return nil })
		_, uploadErr = f.s.beginUpload("p", nil, "a.txt", "a.txt", 1, false)
		return policy.Reading{}, errors.New("guest did not answer")
	}
	code, reply := f.handoff(t, "click", "")
	if !errors.Is(jobErr, errAppUpdateSealed) || !errors.Is(uploadErr, errAppUpdateSealed) {
		t.Fatalf("seal admitted work: job=%v upload=%v", jobErr, uploadErr)
	}
	// An unreadable guest is an unknown blocker: a click must confirm it.
	if code != http.StatusConflict || reply.Code != policy.ConfirmRequired || len(reply.Blockers) != 1 || !reply.Blockers[0].Unknown {
		t.Fatalf("unknown guest: %d %+v", code, reply)
	}
	f.assertNothingInstalled(t)
	if _, err := f.s.submitJob("start", func() error { return nil }); err != nil {
		t.Fatalf("refusal left jobs sealed: %v", err)
	}
}

func TestAppUpdateUnknownGuestIsBusy(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	written := f.silentRequestFromTicks(t)
	f.guest = func(context.Context) (policy.Reading, error) { return policy.Reading{}, errors.New("ssh: timeout") }
	code, reply := f.handoff(t, "silent", written.ID)
	if code != http.StatusConflict || reply.Code != policy.Busy || len(reply.Blockers) == 0 || !reply.Blockers[0].Unknown {
		t.Fatalf("unknown guest installed: %d %+v", code, reply)
	}
	f.assertNothingInstalled(t)
}

func TestAppUpdateStopFailureRestoresAndIsNotRetried(t *testing.T) {
	f := newUpdateFixture(t)
	f.setMode(t, policy.Silent)
	written := f.silentRequestFromTicks(t)
	f.s.appUpdate.stop = func() error { return errors.New("客机没有响应") }
	code, reply := f.handoff(t, "silent", written.ID)
	if code != http.StatusConflict || reply.Code != "stop_failed" || f.shutdown.Load() != 0 {
		t.Fatalf("stop failure: %d %+v", code, reply)
	}
	ledger, _ := policy.LoadLedger(f.s.updateProfile())
	if last := ledger.Last(); last == nil || last.Result != "failed" || last.Step != policy.StepStopping {
		t.Fatalf("attempt %+v", last)
	}
	c, _ := policy.LoadChain(f.s.updateProfile())
	if c == nil || c.Step != policy.StepFailed || c.FailedStep != policy.StepStopping {
		t.Fatalf("chain %+v", c)
	}
	// The next idle hours never try the same version again.
	f.advance(2 * time.Hour)
	f.s.appUpdateTick(context.Background())
	f.advance(time.Hour)
	f.s.appUpdateTick(context.Background())
	if _, err := os.Stat(filepath.Join(policy.Dir(f.s.updateProfile()), silentRequestFile)); !os.IsNotExist(err) {
		t.Fatal("failed version requested again")
	}
	if w := f.s.appUpdateStatusView().Waiting; w == nil || w.Code != policy.FailedBefore {
		t.Fatalf("waiting %+v", w)
	}
}

func trustedUpdateRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func TestAppUpdatePolicyRequiresTrustedDesktopAndUnlockedAccount(t *testing.T) {
	f := newUpdateFixture(t)
	s := f.s
	if w := request(s, "POST", "/api/app-update/policy", s.origin, `{"mode":"silent"}`, true); w.Code != http.StatusForbidden {
		t.Fatalf("session without desktop proof: %d", w.Code)
	}
	if w := request(s, "GET", "/api/app-update", "", "", true); w.Code != http.StatusForbidden {
		t.Fatalf("status without desktop proof: %d", w.Code)
	}
	if w := trustedUpdateRequest(s, "POST", "/api/app-update/policy", `{"mode":"always"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode: %d", w.Code)
	}
	s.appUpdate.unavailable = func() string { return "安装位置需要管理员权限，不能静默更新；可以点击升级" }
	if w := trustedUpdateRequest(s, "POST", "/api/app-update/policy", `{"mode":"silent"}`); w.Code != http.StatusConflict {
		t.Fatalf("silent where unavailable: %d", w.Code)
	}
	s.appUpdate.unavailable = func() string { return "" }
	s.account.lock()
	if w := trustedUpdateRequest(s, "POST", "/api/app-update/policy", `{"mode":"silent"}`); w.Code != http.StatusLocked {
		t.Fatalf("locked account changed the mode: %d", w.Code)
	}
	if settings, _ := policy.LoadSettings(s.updateProfile()); settings.Mode != policy.Manual {
		t.Fatalf("mode %s", settings.Mode)
	}
	s.account.setKey(make([]byte, 32))
	w := trustedUpdateRequest(s, "POST", "/api/app-update/policy", `{"mode":"silent"}`)
	var st policy.Status
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Mode != policy.Silent || !st.SilentAvailable {
		t.Fatalf("enable silent: %d %s", w.Code, w.Body.String())
	}
	if settings, _ := policy.LoadSettings(s.updateProfile()); settings.Mode != policy.Silent {
		t.Fatalf("stored mode %s", settings.Mode)
	}
}

func TestAppUpdateInterruptionsListsWhatAClickStops(t *testing.T) {
	f := newUpdateFixture(t)
	f.guest = func(context.Context) (policy.Reading, error) {
		return policy.Reading{Blockers: []policy.Blocker{{ID: "app:claude-desktop", Kind: policy.KindApp, Interruptible: true, Label: "Claude Desktop 正在运行"}}}, nil
	}
	f.s.setJobForTest(Job{ID: "j", Kind: "environment-update", Running: true})
	w := trustedUpdateRequest(f.s, "GET", "/api/app-update/interruptions", "")
	var got appUpdateInterruptions
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(got.Blockers) != 1 || got.Blockers[0].ID != "app:claude-desktop" || len(got.MustWait) != 1 || got.MustWait[0].Label != "「更新环境组件」正在进行" || len(got.Confirm) != 1 {
		t.Fatalf("interruptions %+v", got)
	}
}

func TestAppUpdateRoutesNotForwardedToCloud(t *testing.T) {
	var contacted atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Add(1)
		jsonReply(w, map[string]string{"mode": "silent"})
	}))
	defer cloud.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g := newRemoteGateway(t.TempDir())
	g.desktop = &desktopGateway{generation: "selected", active: &desktopTarget{mode: "remote", ctx: ctx, session: &remotehost.Session{Origin: cloud.URL, Context: ctx, Transport: cloud.Client().Transport}}}
	s := &Server{remote: g}
	for _, route := range []string{"/api/app-update", "/api/app-update/policy", "/api/app-update/interruptions"} {
		r := httptest.NewRequest("POST", route, strings.NewReader(`{"mode":"silent"}`))
		r.Header.Set(connectionHeader, "selected")
		w := httptest.NewRecorder()
		if !s.routeRemote(w, r) || w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "local_required") {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
		}
	}
	if contacted.Load() != 0 {
		t.Fatal("an update route reached the cloud workspace")
	}
	if !appUpdateRoute("/api/app-update") || !appUpdateRoute("/api/app-update/policy") || appUpdateRoute("/api/app-updates") {
		t.Fatal("route matcher")
	}
	if !nativeWorkbenchRoute("/api/app-update/interruptions") {
		t.Fatal("native guests refuse update routes")
	}
	_, err := AgentRequest(context.Background(), filepath.Join(t.TempDir(), "config.json"), AgentTarget{}, "POST", "/api/app-update/policy", strings.NewReader(`{"mode":"silent"}`), "application/json", "")
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Code != "native_transport_required" {
		t.Fatalf("agent transport changed the update mode: %v", err)
	}
}

func TestAppUpdateGuestProbeAnswer(t *testing.T) {
	reading, err := appUpdateGuestBlockers([]byte(`{"shells":[{"sid":10,"foreground":""},{"sid":11,"foreground":"make"},{"sid":12,"foreground":"?"}],"agents":["claude","claude","codex"],"services":{"cage-claude-desktop.service":true,"cage-claude-science.service":false}}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range reading.Blockers {
		ids = append(ids, b.ID)
		if !b.Interruptible || b.Label == "" {
			t.Fatalf("blocker %+v", b)
		}
	}
	if strings.Join(ids, ",") != "terminal-command:11,terminal-command:12,agent:claude,agent:codex,app:claude-desktop" {
		t.Fatalf("blockers %v", ids)
	}
	// A terminal the host holds that the guest does not report is unknown.
	reading, err = appUpdateGuestBlockers([]byte(`{"shells":[],"agents":[],"services":{}}`), 1)
	if err != nil || len(reading.Blockers) != 1 || !reading.Blockers[0].Unknown {
		t.Fatalf("missing terminal: %+v %v", reading, err)
	}
	for _, bad := range []string{``, `{}`, `not json`, `{"shells":[]}`} {
		if _, err = appUpdateGuestBlockers([]byte(bad), 0); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSilentUnavailableReason(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	app := "/Applications/Super Cage.app/Contents/Resources/cage"
	for _, tc := range []struct {
		goos, current, exe string
		writable           func(string) bool
		available          bool
	}{
		{"darwin", "0.1.15", app, yes, true},
		{"darwin", "0.1.15", app, no, false},
		{"darwin", "0.1.15", "/Users/me/go/bin/cage", yes, false},
		{"darwin", "dev", app, yes, false},
		{"windows", "0.1.15", `C:\Users\me\AppData\Local\Programs\Super Cage\cage.exe`, yes, true},
		{"windows", "0.1.15", `C:\Program Files\Super Cage\cage.exe`, no, false},
		{"linux", "0.1.15", "/usr/bin/cage", yes, false},
	} {
		reason := silentUnavailableReason(tc.goos, tc.current, tc.exe, tc.writable)
		if (reason == "") != tc.available {
			t.Fatalf("%s %s %s: %q", tc.goos, tc.current, tc.exe, reason)
		}
	}
}

func writeChain(t *testing.T, s *Server, c policy.Chain) {
	t.Helper()
	c.Schema, c.ID, c.StartedAt, c.UpdatedAt = policy.ChainSchema, "c1", time.Now(), time.Now()
	if err := policy.SaveChain(s.updateProfile(), &c); err != nil {
		t.Fatal(err)
	}
	ledger := policy.Ledger{}
	ledger.Begin(policy.Attempt{Version: c.To, From: c.From, Trigger: c.Trigger, StartedAt: time.Now()})
	if err := policy.SaveLedger(s.updateProfile(), ledger); err != nil {
		t.Fatal(err)
	}
}

func TestAppUpdateChainResumesAfterReplacement(t *testing.T) {
	t.Run("environment was stopped", func(t *testing.T) {
		f := newUpdateFixture(t)
		version.Version = "0.1.16"
		writeChain(t, f.s, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.Click, Step: policy.StepInstalling})
		f.s.resumeAppUpdateChain()
		c, _ := policy.LoadChain(f.s.updateProfile())
		ledger, _ := policy.LoadLedger(f.s.updateProfile())
		if c == nil || c.Step != policy.StepDone || ledger.Last().Result != "ok" {
			t.Fatalf("chain %+v attempt %+v", c, ledger.Last())
		}
	})
	t.Run("environment restarted and components updated", func(t *testing.T) {
		f := newUpdateFixture(t)
		version.Version = "0.1.16"
		var started, synced atomic.Int32
		f.s.appUpdate.start = func() error {
			started.Add(1)
			f.s.mu.Lock()
			f.s.guestUpdateRequired = true
			f.s.networkPaused = true
			f.s.mu.Unlock()
			return nil
		}
		f.s.appUpdate.sync = func() error { synced.Add(1); return nil }
		writeChain(t, f.s, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.SilentUpdate, Step: policy.StepInstalling, Environment: policy.Environment{Running: true}})
		f.s.resumeAppUpdateChain()
		c, _ := policy.LoadChain(f.s.updateProfile())
		if c == nil || c.Step != policy.StepDone || started.Load() != 1 || synced.Load() != 1 {
			t.Fatalf("chain %+v started=%d synced=%d", c, started.Load(), synced.Load())
		}
	})
	t.Run("network gate stays closed", func(t *testing.T) {
		f := newUpdateFixture(t)
		version.Version = "0.1.16"
		f.s.appUpdate.start = func() error { return nil }
		writeChain(t, f.s, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.Click, Step: policy.StepRelaunched, Environment: policy.Environment{Running: true}})
		f.s.resumeAppUpdateChain()
		c, _ := policy.LoadChain(f.s.updateProfile())
		ledger, _ := policy.LoadLedger(f.s.updateProfile())
		if c == nil || c.Step != policy.StepFailed || c.FailedStep != policy.StepNetwork || ledger.Last().Result != "failed" || policy.View(c).Next != "check_network" {
			t.Fatalf("chain %+v attempt %+v", c, ledger.Last())
		}
	})
	t.Run("installer rolled back", func(t *testing.T) {
		f := newUpdateFixture(t)
		var started atomic.Int32
		f.s.appUpdate.start = func() error { started.Add(1); return nil }
		if err := policy.WriteRecord(f.s.updateProfile(), "last-result.json", map[string]any{"success": false, "message": "签名校验失败，已恢复原应用"}); err != nil {
			t.Fatal(err)
		}
		writeChain(t, f.s, policy.Chain{From: "0.1.15", To: "0.1.16", Trigger: policy.SilentUpdate, Step: policy.StepInstalling, Environment: policy.Environment{Running: true}})
		f.s.resumeAppUpdateChain()
		c, _ := policy.LoadChain(f.s.updateProfile())
		ledger, _ := policy.LoadLedger(f.s.updateProfile())
		if c == nil || c.Step != policy.StepFailed || c.Error != "签名校验失败，已恢复原应用" || ledger.Last().Result != "failed" || started.Load() != 1 {
			t.Fatalf("chain %+v attempt %+v started=%d", c, ledger.Last(), started.Load())
		}
	})
}
