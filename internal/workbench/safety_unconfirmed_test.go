package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

// unconfirmedMonitorServer is a granted Linux environment whose exit is bound
// to a static IP with a cached location, so a monitoring round reaches the
// guest reads without any network access.
func unconfirmedMonitorServer(t *testing.T) (*Server, *cage.EgressGate) {
	t.Helper()
	s := testServer(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	p, _ := managedproxy.Load(s.profile)
	p.Port = port
	if err = p.Save(s.profile); err != nil {
		t.Fatal(err)
	}
	cfg, _ := loadConfig(s.configPath)
	cfg.ProxyURL = p.Endpoint()
	cfg.ExitPolicy = cage.ExitPolicy{Mode: "static", IP: "8.8.8.8"}
	if err := writePrivate(s.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.profile, "proxy"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.profile, "proxy", "config.yaml"), []byte("interface-name: en0\nmixed-port: "+strconv.Itoa(port)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()))
	if err := writePrivate(filepath.Join(dir, "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	gate, err := cage.NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gate.Close("test complete") })
	stop, err := cage.StartGateControl(s.configPath, dir, gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if err := gate.BeginCheck(); err != nil {
		t.Fatal(err)
	}
	grantTestGate(t, gate)
	s.ready = true
	s.setJobForTest(Job{})
	s.publishGate(gate.Status())
	s.safety.VerifiedAt = time.Now().Add(-time.Minute)
	s.locations = map[string]Location{"8.8.8.8": {IP: "8.8.8.8", CountryCode: "US", CheckedAt: time.Now()}}
	return s, gate
}

func exitProbeOK(context.Context, cage.Config) (string, error) { return "8.8.8.8", nil }

func matchingGuestSettings(cfg cage.Config) guestObservation {
	offset := 0 // The fixture's zone is Etc/UTC.
	return guestObservation{TZ: cfg.Timezone, SystemTimezone: cfg.Timezone, Lang: cfg.Locale, LCAll: cfg.Locale, LocaleValid: true,
		LocaltimeTarget: "/usr/share/zoneinfo/" + cfg.Timezone, UTCOffsetExpected: &offset, UTCOffsetActual: &offset, UTCOffsetSystem: &offset}
}

func matchingBrowserSettings(cfg cage.Config) browserObservation {
	return browserObservation{Timezone: cfg.Timezone, ExpectedTimezone: cfg.Timezone, Language: cfg.BrowserLanguage, Platform: "Linux x86_64"}
}

// fakeMonitorReads replaces the monitor's guest reads for one test.
func fakeMonitorReads(t *testing.T, guest func(context.Context) (guestObservation, error), browser func(context.Context) (browserObservation, error)) {
	t.Helper()
	previousGuest, previousBrowser := monitorGuestSettings, monitorBrowserSettings
	t.Cleanup(func() { monitorGuestSettings, monitorBrowserSettings = previousGuest, previousBrowser })
	monitorGuestSettings = func(_ *Server, ctx context.Context, _ map[string]any) (guestObservation, error) { return guest(ctx) }
	monitorBrowserSettings = func(_ *Server, ctx context.Context, _ cage.Config) (browserObservation, error) { return browser(ctx) }
}

func TestUnconfirmedSettingsReadKeepsLeaseWithoutRenewing(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		guestErr, browserErr error
		stage                string
	}{
		// The reads classify their failures (guest_reads.go); these are
		// the ones that returned no answer.
		{"system settings read timed out", noAnswer(context.DeadlineExceeded), nil, "读不到系统的时区和语言"},
		{"system settings probe killed", noAnswer(errors.New("客机检测未返回完整结果: signal: killed")), nil, "读不到系统的时区和语言"},
		{"browser audit returned no settings", nil, noAnswer(errors.New("浏览器检测未返回完整结果，请更新或重启环境")), "读不到浏览器的时区和语言"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			reads := 0
			fakeMonitorReads(t, func(ctx context.Context) (guestObservation, error) {
				reads++
				if _, ok := ctx.Deadline(); !ok {
					t.Error("guest read without a deadline")
				}
				if tc.guestErr != nil {
					return guestObservation{}, tc.guestErr
				}
				return matchingGuestSettings(cfg), nil
			}, func(ctx context.Context) (browserObservation, error) {
				reads++
				if tc.browserErr != nil {
					return browserObservation{}, tc.browserErr
				}
				return matchingBrowserSettings(cfg), nil
			})
			before := gate.Status()
			verified := s.safety.VerifiedAt
			s.monitorSafetyWithProbe(exitProbeOK)
			after := gate.Status()
			if after.Mode != cage.GateOpen || !after.Until.Equal(before.Until) || after.Epoch != before.Epoch || !s.safety.VerifiedAt.Equal(verified) {
				t.Fatalf("an unconfirmed read changed the lease: before=%+v after=%+v", before, after)
			}
			if s.recovery.RunID != "" {
				t.Fatal("a recheck was scheduled while the lease is still valid")
			}
			e := s.safety.Explanation
			// lease_until is the remaining lease the gate reported, counted
			// from when the status request left, in whole seconds: never
			// later than the gate's and less than a second early.
			if e == nil || e.State != networkUnconfirmed || e.LeaseUntil == nil || e.LeaseUntil.After(before.Until) ||
				before.Until.Sub(*e.LeaseUntil) > 1100*time.Millisecond || e.DeadlineMs != e.LeaseUntil.UnixMilli() || !strings.Contains(e.Reason, tc.stage) {
				t.Fatalf("explanation: %+v", e)
			}
			if message := s.currentNetworkErrorLocked(); !strings.Contains(message, "现有联网许可未延长") || !strings.Contains(message, "联网许可到期前仍无法确认时，外网会自动暂停") || strings.ContainsAny(message, "0123456789") {
				t.Fatalf("status does not say the lease runs out: %s", message)
			}
			if label := s.networkStateLabel("ready", "已联网"); label != "已联网，监测重试中" {
				t.Fatalf("proxy label: %s", label)
			}
			response := httptest.NewRecorder()
			s.status(response, httptest.NewRequest("GET", "/api/status", nil))
			var status struct {
				Safety struct {
					Gate        *cage.GateStatus `json:"gate"`
					Explanation map[string]any   `json:"explanation"`
				} `json:"safety"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Safety.Gate == nil || status.Safety.Gate.Mode != cage.GateOpen || status.Safety.Explanation["state"] != "unconfirmed" || status.Safety.Explanation["lease_until"] == nil {
				t.Fatalf("/api/status safety block: %s", response.Body.String())
			}

			// The next tick does not read the guest again at once.
			attempted := reads
			s.monitorSafetyWithProbe(exitProbeOK)
			if reads != attempted {
				t.Fatal("retried a starving guest without spacing")
			}
			records := monitorTimingRecords(t, s, "safety")
			if len(records) < 2 || records[len(records)-2]["result"] != "observation_unconfirmed" || records[len(records)-1]["result"] != "retry_not_due" {
				t.Fatalf("monitor results: %v", records)
			}

			// A later complete round confirms the guest and renews.
			s.safety.deferral.RetryAt = time.Now().Add(-time.Second)
			fakeMonitorReads(t, func(context.Context) (guestObservation, error) { return matchingGuestSettings(cfg), nil },
				func(context.Context) (browserObservation, error) { return matchingBrowserSettings(cfg), nil })
			for !time.Now().Add(cage.GateLease).After(before.Until) {
				time.Sleep(time.Millisecond)
			}
			s.monitorSafetyWithProbe(exitProbeOK)
			renewed := gate.Status()
			if renewed.Mode != cage.GateOpen || !renewed.Until.After(before.Until) || s.safety.Explanation != nil || s.safety.monitorError != "" || s.safety.deferral.RunID != "" {
				t.Fatalf("a confirmed round did not renew and clear the notice: %+v explanation=%+v error=%q", renewed, s.safety.Explanation, s.safety.monitorError)
			}
		})
	}
}

// shortMonitorTiming scales the monitor's retry timing to a lease of seconds.
func shortMonitorTiming(t *testing.T) {
	t.Helper()
	limit, margin, minimum, spacing, last := monitorReadLimit, monitorRenewMargin, monitorReadMinimum, monitorRetrySpacing, monitorLastAttempt
	t.Cleanup(func() {
		monitorReadLimit, monitorRenewMargin, monitorReadMinimum, monitorRetrySpacing, monitorLastAttempt = limit, margin, minimum, spacing, last
	})
	monitorReadLimit, monitorRenewMargin, monitorReadMinimum = 400*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	monitorRetrySpacing, monitorLastAttempt = 300*time.Millisecond, 700*time.Millisecond
}

func TestUnconfirmedReadsUntilDeadlineEndWithGateTimer(t *testing.T) {
	s, gate := unconfirmedMonitorServer(t)
	shortMonitorTiming(t)
	writeGuestPressure(t, s) // The field case: the guest is short of memory throughout.
	gate.ShortenLeaseForTest(2500 * time.Millisecond)
	if err := gate.Renew(gate.Status().Report.ID); err != nil {
		t.Fatal(err)
	}
	s.publishGate(gate.Status())
	granted := gate.Status()
	var starts, deadlines []time.Time
	fakeMonitorReads(t, func(ctx context.Context) (guestObservation, error) {
		starts = append(starts, time.Now())
		deadline, _ := ctx.Deadline()
		deadlines = append(deadlines, deadline)
		<-ctx.Done() // A guest that never answers.
		return guestObservation{}, noAnswer(ctx.Err())
	}, func(context.Context) (browserObservation, error) {
		t.Error("browser read after an unconfirmed system read")
		return browserObservation{}, errors.New("unexpected")
	})
	stop := time.Now().Add(8 * time.Second)
	for gate.Status().Mode == cage.GateOpen && time.Now().Before(stop) {
		s.monitorSafetyWithProbe(exitProbeOK)
		if current := gate.Status(); current.Mode == cage.GateOpen && (!current.Until.Equal(granted.Until) || current.Epoch != granted.Epoch) {
			t.Fatalf("an unconfirmed round renewed or replaced the lease: granted=%+v current=%+v", granted, current)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Status reports an expired lease as closed before the timer's own
	// transition runs; wait for that transition, which is what closes the
	// connections and starts a new epoch.
	for gate.Status().Epoch == granted.Epoch && time.Now().Before(stop) {
		time.Sleep(5 * time.Millisecond)
	}
	closed := gate.Status()
	if closed.Mode != cage.GateClosed || closed.Reason != gateLeaseExpired || closed.Epoch == granted.Epoch || time.Now().Before(granted.Until) {
		t.Fatalf("the lease did not end by the gate's own timer: %+v", closed)
	}
	if len(starts) < 2 || len(starts) > 5 {
		t.Fatalf("reads within the lease: %d", len(starts))
	}
	for i, deadline := range deadlines {
		if deadline.After(granted.Until.Add(-monitorRenewMargin)) {
			t.Fatalf("read %d could end after the renewal margin: %s > %s", i, deadline, granted.Until)
		}
	}
	// The next round explains the expiry and schedules the full recheck.
	s.monitorSafetyWithProbe(exitProbeOK)
	if s.recovery.RunID == "" || s.recovery.Epoch != closed.Epoch {
		t.Fatalf("no recheck after the lease ran out: %+v", s.recovery)
	}
	if !strings.Contains(s.safety.monitorError, "联网许可到期前仍无法读取系统时区和语言") {
		t.Fatalf("closure reason: %q", s.safety.monitorError)
	}
	e := s.safety.Explanation
	if e == nil || e.State != networkRecheckScheduled || e.RecheckAt == nil || !e.MemoryShort ||
		strings.Count(e.Reason, "环境内存不足") != 1 || !strings.HasSuffix(e.Reason, "安全检查读不到系统的时区和语言，所以暂时关闭了外网。") {
		t.Fatalf("explanation after expiry: %+v", e)
	}
	if message := s.currentNetworkErrorLocked(); strings.Count(message, "环境内存不足") > 1 || !strings.Contains(message, "自动重检稍后进行") {
		t.Fatalf("network_error after expiry: %s", message)
	}
}

func TestSlowButCorrectAnswerStillRenews(t *testing.T) {
	// With the default timing a read keeps the monitor's whole budget, as
	// before: a slow guest that answers correctly is not cut short.
	s, gate := unconfirmedMonitorServer(t)
	cfg, _ := loadConfig(s.configPath)
	var budget time.Duration
	fakeMonitorReads(t, func(ctx context.Context) (guestObservation, error) {
		deadline, _ := ctx.Deadline()
		budget = time.Until(deadline)
		return matchingGuestSettings(cfg), nil
	}, func(context.Context) (browserObservation, error) { return matchingBrowserSettings(cfg), nil })
	s.monitorSafetyWithProbe(exitProbeOK)
	if budget < 40*time.Second {
		t.Fatalf("a guest read on a fresh lease gets only %s", budget)
	}
	// A slow correct answer inside a short lease renews it.
	s, gate = unconfirmedMonitorServer(t)
	shortMonitorTiming(t)
	gate.ShortenLeaseForTest(2500 * time.Millisecond)
	if err := gate.Renew(gate.Status().Report.ID); err != nil {
		t.Fatal(err)
	}
	s.publishGate(gate.Status())
	before := gate.Status()
	fakeMonitorReads(t, func(context.Context) (guestObservation, error) {
		time.Sleep(300 * time.Millisecond)
		return matchingGuestSettings(cfg), nil
	}, func(context.Context) (browserObservation, error) {
		time.Sleep(300 * time.Millisecond)
		return matchingBrowserSettings(cfg), nil
	})
	s.monitorSafetyWithProbe(exitProbeOK)
	if after := gate.Status(); after.Mode != cage.GateOpen || !after.Until.After(before.Until) || s.safety.Explanation != nil {
		t.Fatalf("a slow correct answer did not renew: before=%+v after=%+v", before, after)
	}
}

func TestConfirmedWrongSettingsCloseAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		guest   func(*Server, cage.Config) (guestObservation, error)
		browser func(cage.Config) browserObservation
	}{
		{name: "system time zone differs", guest: func(_ *Server, cfg cage.Config) (guestObservation, error) {
			v := matchingGuestSettings(cfg)
			v.TZ = "Asia/Tokyo"
			return v, nil
		}},
		{name: "system time zone file has no offset", guest: func(_ *Server, cfg cage.Config) (guestObservation, error) {
			v := matchingGuestSettings(cfg)
			v.UTCOffsetSystem = nil
			return v, nil
		}},
		{name: "browser time zone differs", browser: func(cfg cage.Config) browserObservation {
			v := matchingBrowserSettings(cfg)
			v.Timezone = "Asia/Tokyo"
			return v
		}},
		{name: "browser platform differs", browser: func(cfg cage.Config) browserObservation {
			v := matchingBrowserSettings(cfg)
			v.Platform = "Win32"
			return v
		}},
		{name: "configuration changed during an unanswered read", guest: func(s *Server, cfg cage.Config) (guestObservation, error) {
			changed := cfg
			changed.Timezone = "Asia/Tokyo"
			if err := writePrivate(s.configPath, changed); err != nil {
				return guestObservation{}, err
			}
			return guestObservation{}, noAnswer(context.DeadlineExceeded)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			fakeMonitorReads(t, func(context.Context) (guestObservation, error) {
				if tc.guest != nil {
					return tc.guest(s, cfg)
				}
				return matchingGuestSettings(cfg), nil
			}, func(context.Context) (browserObservation, error) {
				if tc.browser != nil {
					return tc.browser(cfg), nil
				}
				return matchingBrowserSettings(cfg), nil
			})
			s.monitorSafetyWithProbe(exitProbeOK)
			if state := gate.Status(); state.Mode != cage.GateClosed {
				t.Fatalf("a confirmed wrong value left the gate open: %+v", state)
			}
			if s.recovery.RunID != "" {
				t.Fatal("a confirmed wrong value scheduled an automatic recheck")
			}
			if e := s.safety.Explanation; e == nil || e.State != networkCheckRequired {
				t.Fatalf("explanation: %+v", e)
			}
		})
	}
}
