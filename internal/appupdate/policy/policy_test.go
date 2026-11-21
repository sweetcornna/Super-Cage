package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

var now = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)

func TestMissingOrCorruptPolicyIsManual(t *testing.T) {
	profile := t.TempDir()
	if s, err := LoadSettings(profile); err != nil || s.Mode != Manual {
		t.Fatalf("missing: %+v %v", s, err)
	}
	for _, body := range []string{"{", `{"mode":"auto"}`, `{"mode":"SILENT"}`, `{}`} {
		if err := os.MkdirAll(Dir(profile), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(Dir(profile), "policy.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := LoadSettings(profile); err == nil || s.Mode != Manual {
			t.Fatalf("%s read as %+v %v", body, s, err)
		}
	}
	if err := SaveSettings(profile, Settings{Mode: "auto"}); err == nil {
		t.Fatal("invalid mode saved")
	}
	if err := SaveSettings(profile, Settings{Mode: Silent, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadSettings(profile); err != nil || s.Mode != Silent {
		t.Fatalf("round trip: %+v %v", s, err)
	}
	if err := host.CheckPrivateFile(filepath.Join(Dir(profile), "policy.json")); err != nil {
		t.Fatal(err)
	}
}

func idleFacts() (Request, Facts) {
	snapshot := Snapshot{At: now, Fingerprint: "f1", Decision: true}
	return Request{Trigger: SilentUpdate, Version: "0.1.16", Fingerprint: "f1"}, Facts{
		Current:        "0.1.15",
		Settings:       Settings{Mode: Silent},
		Snapshot:       snapshot,
		Hold:           HoldTracker{Since: now.Add(-HoldPeriod), Fingerprint: "f1"},
		InputIdle:      2 * UserIdle,
		InputIdleKnown: true,
		PublishedAt:    now.Add(-2 * ReleaseAge).Format(time.RFC3339),
		Now:            now,
	}
}

func TestSilentAllowedOnlyWhenEverythingHolds(t *testing.T) {
	r, f := idleFacts()
	if d := Decide(r, f); !d.Allowed() {
		t.Fatalf("idle machine refused: %+v", d)
	}
	busy := Blocker{ID: "terminal:a", Kind: KindTerminal, Label: "终端「dev」里还有命令在运行（make）", Interruptible: true}
	for _, tc := range []struct {
		name   string
		mutate func(*Request, *Facts)
		want   Code
	}{
		{"manual mode never installs unattended", func(_ *Request, f *Facts) { f.Settings.Mode = Manual }, ManualMode},
		{"unreadable mode is manual", func(_ *Request, f *Facts) { f.SettingsErr = errors.New("bad") }, ManualMode},
		{"platform cannot be updated unattended", func(_ *Request, f *Facts) { f.SilentUnavailable = "Linux 版本使用系统安装包更新" }, SilentUnavailable},
		{"unreadable ledger", func(_ *Request, f *Facts) { f.LedgerErr = errors.New("bad") }, Unreadable},
		{"failed before", func(_ *Request, f *Facts) {
			f.Ledger.Begin(Attempt{Version: "0.1.16"})
			f.Ledger.Finish("0.1.16", false, StepTools, "x", now)
		}, FailedBefore},
		{"unfinished attempt (crash mid-install) counts as failed", func(_ *Request, f *Facts) { f.Ledger.Begin(Attempt{Version: "0.1.16"}) }, FailedBefore},
		{"release younger than a day", func(_ *Request, f *Facts) { f.PublishedAt = now.Add(-time.Hour).Format(time.RFC3339) }, TooNew},
		{"release without publication time", func(_ *Request, f *Facts) { f.PublishedAt = "" }, TooNew},
		{"timer snapshot without the decision reads", func(_ *Request, f *Facts) { f.Snapshot.Decision = false }, Unreadable},
		{"a blocker", func(_ *Request, f *Facts) { f.Snapshot.Blockers = []Blocker{busy} }, Busy},
		{"unknown is busy", func(_ *Request, f *Facts) {
			f.Snapshot.Blockers = []Blocker{{ID: "unknown:guest", Kind: KindUnknown, Label: "无法确认环境内终端的状态", Interruptible: true, Unknown: true}}
		}, Busy},
		{"activity since the request", func(_ *Request, f *Facts) { f.Snapshot.Fingerprint = "f2"; f.Hold.Fingerprint = "f2" }, Changed},
		{"idle not held long enough", func(_ *Request, f *Facts) { f.Hold.Since = now.Add(-HoldPeriod / 2) }, NotHeld},
		{"idle held just short of the slack", func(_ *Request, f *Facts) { f.Hold.Since = now.Add(-HoldPeriod + HoldSlack + time.Second) }, NotHeld}, {"hold of another fingerprint", func(r *Request, f *Facts) { r.Fingerprint = ""; f.Hold.Fingerprint = "f0" }, NotHeld},
		{"host input unknown", func(_ *Request, f *Facts) { f.InputIdleKnown = false }, UserUnknown},
		{"someone at the keyboard", func(_ *Request, f *Facts) { f.InputIdle = UserIdle - time.Minute }, UserActive},
		{"prerelease from a stable version", func(r *Request, _ *Facts) { r.Version = "0.1.16-rc.1" }, NotEligible},
		{"downgrade", func(r *Request, _ *Facts) { r.Version = "0.1.14" }, NotEligible},
		{"same version", func(r *Request, _ *Facts) { r.Version = "0.1.15" }, NotEligible},
		{"unknown trigger", func(r *Request, _ *Facts) { r.Trigger = "timer" }, NotEligible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, f := idleFacts()
			tc.mutate(&r, &f)
			d := Decide(r, f)
			if d.Code != tc.want || d.Allowed() || d.Message == "" {
				t.Fatalf("got %+v, want %s", d, tc.want)
			}
		})
	}
	// Two looks of the hourly ticker a little less than an hour apart.
	jr, jf := idleFacts()
	jf.Hold.Since = jf.Now.Add(-HoldPeriod + time.Second)
	if d := Decide(jr, jf); !d.Allowed() {
		t.Fatalf("hourly ticker jitter refused: %+v", d)
	}
}

func TestClickNeedsConfirmationOfNewInterruptions(t *testing.T) {
	_, f := idleFacts()
	f.Settings.Mode = Manual // a click never depends on the mode
	f.Ledger.Begin(Attempt{Version: "0.1.16"})
	f.PublishedAt = ""
	f.InputIdleKnown = false
	terminal := Blocker{ID: "terminal:a", Kind: KindTerminal, Label: "终端「dev」", Interruptible: true}
	unknown := Blocker{ID: "unknown:guest", Kind: KindUnknown, Label: "无法确认环境内终端的状态", Interruptible: true, Unknown: true}
	job := Blocker{ID: "job:environment-update", Kind: KindJob, Label: "「更新环境组件」正在进行"}
	click := Request{Trigger: Click, Version: "0.1.16"}
	if d := Decide(click, f); !d.Allowed() {
		t.Fatalf("idle click refused: %+v", d)
	}
	f.Snapshot.Blockers = []Blocker{terminal, unknown}
	d := Decide(click, f)
	if d.Code != ConfirmRequired || len(d.Blockers) != 2 {
		t.Fatalf("unconfirmed interruptions: %+v", d)
	}
	click.Confirmed = ConfirmationIDs(d.Blockers)
	if d = Decide(click, f); !d.Allowed() {
		t.Fatalf("confirmed click refused: %+v", d)
	}
	// Work that started after the confirmation wins over the click.
	f.Snapshot.Blockers = append(f.Snapshot.Blockers, Blocker{ID: "turn:phone:1", Kind: KindTurn, Label: "手机发起的 Claude 轮次正在运行", Interruptible: true})
	if d = Decide(click, f); d.Code != ConfirmRequired {
		t.Fatalf("new interruption after confirmation: %+v", d)
	}
	// A running operation must finish even when the person confirmed it.
	f.Snapshot.Blockers = []Blocker{job}
	click.Confirmed = []string{job.ID}
	if d = Decide(click, f); d.Code != MustWait || !strings.Contains(d.Message, "更新环境组件") {
		t.Fatalf("must wait: %+v", d)
	}
	for _, version := range []string{"0.1.16-rc.1", "0.1.14", "0.1.15", "v0.1.16", "0.1"} {
		if d = Decide(Request{Trigger: Click, Version: version}, Facts{Current: "0.1.15"}); d.Code != NotEligible {
			t.Fatalf("click to %s: %+v", version, d)
		}
	}
	if d = Decide(Request{Trigger: Click, Version: "0.2.0-rc.2"}, Facts{Current: "0.2.0-rc.1"}); !d.Allowed() {
		t.Fatalf("prerelease channel: %+v", d)
	}
}

func TestLedgerKeepsUnfinishedAttemptsAsFailures(t *testing.T) {
	profile := t.TempDir()
	var l Ledger
	l.Begin(Attempt{Version: "0.1.16", From: "0.1.15", Trigger: SilentUpdate, StartedAt: now})
	if err := SaveLedger(profile, l); err != nil {
		t.Fatal(err)
	}
	// A crash here leaves the attempt unfinished.
	read, err := LoadLedger(profile)
	if err != nil || !read.FailedBefore("0.1.16") || read.FailedBefore("0.1.17") {
		t.Fatalf("unfinished attempt: %+v %v", read, err)
	}
	if !read.Finish("0.1.16", true, StepDone, "", now) || read.FailedBefore("0.1.16") {
		t.Fatalf("finished ok: %+v", read)
	}
	if read.Finish("0.1.16", false, StepTools, "x", now) {
		t.Fatal("finished an attempt twice")
	}
	for i := 0; i < ledgerLimit+5; i++ {
		read.Begin(Attempt{Version: "9.9.9"})
	}
	if err = SaveLedger(profile, read); err != nil {
		t.Fatal(err)
	}
	if read, err = LoadLedger(profile); err != nil || len(read.Attempts) != ledgerLimit || read.Last().Version != "9.9.9" {
		t.Fatalf("bounded: %d %v", len(read.Attempts), err)
	}
	if err = os.WriteFile(ledgerPath(profile), []byte("["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadLedger(profile); err == nil {
		t.Fatal("corrupt ledger read as empty")
	}
}

func TestRegistryUnknownIsBusyAndFingerprintCoversActivity(t *testing.T) {
	var r Registry
	jobs := "job-1"
	r.Register("jobs", "无法确认操作状态", func(context.Context) (Reading, error) { return Reading{Activity: []string{jobs}}, nil })
	guestErr := errors.New("guest did not answer")
	r.RegisterDecision("guest", "无法确认环境内终端的状态", func(context.Context) (Reading, error) { return Reading{}, guestErr })
	timer := r.Snapshot(context.Background(), now, false)
	if !timer.Idle() || timer.Decision {
		t.Fatalf("timer snapshot must skip decision feeders: %+v", timer)
	}
	decision := r.Snapshot(context.Background(), now, true)
	if decision.Idle() || !decision.Blockers[0].Unknown || decision.Blockers[0].Label != "无法确认环境内终端的状态" {
		t.Fatalf("failed guest read must be busy: %+v", decision)
	}
	if decision.Fingerprint != timer.Fingerprint {
		t.Fatal("decision-only feeders changed the fingerprint")
	}
	guestErr = nil
	if s := r.Snapshot(context.Background(), now, true); !s.Idle() {
		t.Fatalf("readable guest: %+v", s)
	}
	// A job that started and ended between two looks changes the fingerprint.
	jobs = "job-2"
	if s := r.Snapshot(context.Background(), now, false); s.Fingerprint == timer.Fingerprint {
		t.Fatal("job id not in fingerprint")
	}
	before := r.Snapshot(context.Background(), now, false)
	r.Touch()
	if after := r.Snapshot(context.Background(), now, false); after.Fingerprint == before.Fingerprint {
		t.Fatal("admission touch not in fingerprint")
	}
	r.Register("broken", "无法确认手机状态", func(context.Context) (Reading, error) { panic("boom") })
	r.Register("nameless", "无法确认下载状态", func(context.Context) (Reading, error) {
		return Reading{Blockers: []Blocker{{ID: "x"}}}, nil
	})
	s := r.Snapshot(context.Background(), now, false)
	if len(s.Blockers) != 2 || !s.Blockers[0].Unknown || !s.Blockers[1].Unknown {
		t.Fatalf("panicking or nameless feeder must be unknown: %+v", s.Blockers)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, b := range r.Snapshot(ctx, now, false).Blockers {
		if !b.Unknown {
			t.Fatalf("expired context must read unknown: %+v", b)
		}
	}
}

func TestExternalHoldBlocksUntilReleased(t *testing.T) {
	var r Registry
	idle := r.Snapshot(context.Background(), now, true)
	release := r.Hold("ssh-1", "有 SSH 会话连接到环境", true)
	held := r.Snapshot(context.Background(), now, true)
	if held.Idle() || held.Blockers[0].ID != "external:ssh-1" || held.Fingerprint == idle.Fingerprint {
		t.Fatalf("hold: %+v", held)
	}
	release()
	release()
	after := r.Snapshot(context.Background(), now, true)
	if !after.Idle() || after.Fingerprint == held.Fingerprint || after.Fingerprint == idle.Fingerprint {
		t.Fatalf("released hold: %+v", after)
	}
}

func TestHoldResetsWhenFingerprintChanges(t *testing.T) {
	var h HoldTracker
	h.Observe(Snapshot{At: now, Fingerprint: "a"})
	if got := h.HeldFor(now.Add(HoldPeriod), "a"); got != HoldPeriod {
		t.Fatalf("held %v", got)
	}
	h.Observe(Snapshot{At: now.Add(HoldPeriod), Fingerprint: "a"})
	if h.Since != now {
		t.Fatal("unchanged idle restarted the hold")
	}
	h.Observe(Snapshot{At: now.Add(HoldPeriod), Fingerprint: "b"})
	if h.Since != now.Add(HoldPeriod) || h.HeldFor(now.Add(HoldPeriod+time.Minute), "a") != 0 {
		t.Fatalf("changed fingerprint kept the hold: %+v", h)
	}
	h.Observe(Snapshot{At: now.Add(2 * HoldPeriod), Fingerprint: "b", Blockers: []Blocker{{ID: "x", Label: "x"}}})
	if !h.Since.IsZero() || h.HeldFor(now.Add(3*HoldPeriod), "b") != 0 {
		t.Fatalf("busy snapshot kept the hold: %+v", h)
	}
}

func TestEligibilityAndReleaseAge(t *testing.T) {
	for _, tc := range []struct {
		version, current string
		ok               bool
	}{
		{"0.1.16", "0.1.15", true},
		{"0.2.0-rc.2", "0.2.0-rc.1", true},
		{"0.2.0", "0.2.0-rc.1", true},
		{"0.1.16-rc.1", "0.1.15", false},
		{"0.1.15", "0.1.15", false},
		{"0.1.14", "0.1.15", false},
		{"0.1.16", "development", false},
		{"0.1.16+build", "0.1.15", false},
		{"00.1.16", "0.1.15", false},
	} {
		if err := Eligible(tc.version, tc.current); (err == nil) != tc.ok {
			t.Fatalf("%s from %s: %v", tc.version, tc.current, err)
		}
	}
	for _, tc := range []struct {
		published string
		ok        bool
	}{
		{now.Add(-ReleaseAge).Format(time.RFC3339), true},
		{now.Add(-ReleaseAge + time.Second).Format(time.RFC3339), false},
		{now.Add(time.Hour).Format(time.RFC3339), false},
		{"", false},
		{"yesterday", false},
	} {
		if OldEnough(tc.published, now) != tc.ok {
			t.Fatalf("published %q", tc.published)
		}
	}
}

func TestChainResumeAfterReplacementRollbackAndCrash(t *testing.T) {
	profile := t.TempDir()
	if c, err := LoadChain(profile); c != nil || err != nil {
		t.Fatalf("no record: %+v %v", c, err)
	}
	c := &Chain{Schema: ChainSchema, ID: "1", From: "0.1.15", To: "0.1.16", Trigger: SilentUpdate, Step: StepStopping,
		Environment: Environment{Running: true, Workspace: "/p", Terminals: []Terminal{{Title: "dev", Kind: "shell"}}}, StartedAt: now, UpdatedAt: now}
	if err := c.Advance(StepDone, now); err == nil {
		t.Fatal("stopping cannot jump to done")
	}
	if err := c.Advance(StepInstalling, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveChain(profile, c); err != nil {
		t.Fatal(err)
	}
	read, err := LoadChain(profile)
	if err != nil || read.Step != StepInstalling || !read.Environment.Running {
		t.Fatalf("read back: %+v %v", read, err)
	}
	for _, tc := range []struct {
		name    string
		step    Step
		running string
		want    Resumption
	}{
		{"new version relaunched", StepInstalling, "0.1.16", Resumption{Continue: StepRelaunched}},
		{"rollback: old version again", StepInstalling, "0.1.15", Resumption{RolledBack: true}},
		{"crash while stopping, old version", StepStopping, "0.1.15", Resumption{RolledBack: true}},
		{"a third version", StepInstalling, "0.1.17", Resumption{Attention: true}},
		{"restart during restore continues", StepRestoring, "0.1.16", Resumption{Continue: StepRestoring}},
		{"restore step under another version", StepRestoring, "0.1.15", Resumption{Attention: true}},
		{"done", StepDone, "0.1.16", Resumption{}},
		{"failed", StepFailed, "0.1.15", Resumption{}},
		{"unknown step", "teleporting", "0.1.16", Resumption{Attention: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := *read
			record.Step = tc.step
			if got := Resume(&record, tc.running); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
	future := *read
	future.Schema = 2
	if got := Resume(&future, "0.1.16"); !got.Attention || View(&future).Step != StepAttention {
		t.Fatalf("unknown schema acted on: %+v", got)
	}
	if got := Resume(nil, "0.1.16"); got != (Resumption{}) {
		t.Fatal("no record must do nothing")
	}
	read.Fail(StepRestoring, "环境未能启动", now)
	v := View(read)
	if v.Step != StepFailed || v.Next != "start_environment" || !strings.HasSuffix(v.NotesURL, "/releases/tag/v0.1.16") {
		t.Fatalf("view: %+v", v)
	}
	if err = os.WriteFile(chainPath(profile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadChain(profile); err == nil {
		t.Fatal("corrupt chain read as absent")
	}
}
