package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// relayGateControl puts a relay in front of the gate's control socket that
// rewrites every status the gate answers with. Shifting until reproduces what
// the workbench reads after its wall clock and the gate's monotonic lease
// came apart: back for a host that slept (the lease did not run while the
// wall clock did), forward for a wall clock stepped back.
func relayGateControl(t *testing.T, instanceDir string, rewrite func(map[string]any)) {
	t.Helper()
	path := filepath.Join(instanceDir, "cage-gate.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]string
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	upstreamSocket := record["socket"]
	dir, err := os.MkdirTemp("", "cgr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "r.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", upstreamSocket)
	}}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request, err := http.NewRequestWithContext(r.Context(), r.Method, "http://cage"+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(500)
			return
		}
		request.Header = r.Header.Clone()
		response, err := upstream.Do(request)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer response.Body.Close()
		reply, _ := io.ReadAll(response.Body)
		if response.StatusCode == 200 {
			decoder := json.NewDecoder(bytes.NewReader(reply))
			decoder.UseNumber()
			var state map[string]any
			if decoder.Decode(&state) == nil {
				rewrite(state)
				reply, _ = json.Marshal(state)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(reply)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	record["socket"] = socket
	if err := writePrivate(path, record); err != nil {
		t.Fatal(err)
	}
}

// shiftUntil moves the status's wall-clock lease end by d.
func shiftUntil(state map[string]any, d time.Duration) {
	value, _ := state["until"].(string)
	until, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || until.IsZero() {
		return
	}
	state["until"] = until.Add(d).Format(time.RFC3339Nano)
}

func TestMonitorAfterHostSleepReadsGuestAndRenews(t *testing.T) {
	// The host slept ten minutes: the wall clock moved on, the gate's lease
	// (monotonic) did not. The first round must read the guest and renew.
	for _, tc := range []struct {
		name  string
		older bool
	}{
		{"current host agent", false},
		{"older host agent without the remaining lease", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gate := unconfirmedMonitorServer(t)
			cfg, _ := loadConfig(s.configPath)
			relayGateControl(t, s.instanceDir(), func(state map[string]any) {
				shiftUntil(state, -10*time.Minute)
				if tc.older {
					delete(state, "lease_remaining_ms")
				}
			})
			reads := 0
			fakeMonitorReads(t, func(ctx context.Context) (guestObservation, error) {
				reads++
				return matchingGuestSettings(cfg), nil
			}, func(ctx context.Context) (browserObservation, error) {
				reads++
				return matchingBrowserSettings(cfg), nil
			})
			before := gate.Status()
			for !time.Now().Add(cage.GateLease).After(before.Until) {
				time.Sleep(time.Millisecond)
			}
			s.monitorSafetyWithProbe(exitProbeOK)
			after := gate.Status()
			if reads != 2 {
				t.Fatalf("the first round after the sleep read the guest %d times, want 2 (monitor: %v)", reads, monitorTimingRecords(t, s, "safety"))
			}
			if after.Mode != cage.GateOpen || !after.Until.After(before.Until) || s.safety.deferral.RunID != "" || s.safety.Explanation != nil {
				t.Fatalf("a confirmed round after the sleep did not renew: before=%+v after=%+v deferral=%+v explanation=%+v", before.Until, after.Until, s.safety.deferral, s.safety.Explanation)
			}
		})
	}
}

func TestMonitorWithWallClockSteppedBackFollowsTheLease(t *testing.T) {
	// The gate's wall readings are ten minutes ahead of the workbench's, as
	// after a backward step of the wall clock. Reads must still end within
	// the real lease, and its expiry must be explained and rechecked.
	s, gate := unconfirmedMonitorServer(t)
	shortMonitorTiming(t)
	gate.ShortenLeaseForTest(2500 * time.Millisecond)
	if err := gate.Renew(gate.Status().Report.ID); err != nil {
		t.Fatal(err)
	}
	relayGateControl(t, s.instanceDir(), func(state map[string]any) { shiftUntil(state, 10*time.Minute) })
	s.publishGate(gate.Status())
	granted := gate.Status()
	var deadlines []time.Time
	fakeMonitorReads(t, func(ctx context.Context) (guestObservation, error) {
		deadline, _ := ctx.Deadline()
		deadlines = append(deadlines, deadline)
		<-ctx.Done()
		return guestObservation{}, noAnswer(ctx.Err())
	}, func(context.Context) (browserObservation, error) {
		return browserObservation{}, errors.New("unexpected")
	})
	stop := time.Now().Add(8 * time.Second)
	for gate.Status().Epoch == granted.Epoch && time.Now().Before(stop) {
		s.monitorSafetyWithProbe(exitProbeOK)
		time.Sleep(20 * time.Millisecond)
	}
	for i, deadline := range deadlines {
		if deadline.After(granted.Until.Add(-monitorRenewMargin)) {
			t.Errorf("read %d could end after the lease's renewal margin: %s > %s", i, deadline, granted.Until.Add(-monitorRenewMargin))
		}
	}
	s.monitorSafetyWithProbe(exitProbeOK)
	if s.recovery.RunID == "" {
		t.Fatalf("no recheck after the lease ran out: %+v", s.recovery)
	}
	if !strings.Contains(s.safety.monitorError, "联网许可到期前仍无法读取系统时区和语言") {
		t.Fatalf("the expiry was not explained: %q", s.safety.monitorError)
	}
	if s.safety.deferral.RunID != "" {
		t.Fatalf("the expired lease's deferral was kept: %+v", s.safety.deferral)
	}
}

func TestMonitorRetryTimesFollowTheMonotonicLease(t *testing.T) {
	// Every time the monitor waits for carries a monotonic reading, so no
	// wall-clock step can hold a retry back or bring it forward.
	s, gate := unconfirmedMonitorServer(t)
	shortMonitorTiming(t)
	monitorLastAttempt = 2 * time.Second // The first deferral is the last attempt.
	gate.ShortenLeaseForTest(2500 * time.Millisecond)
	if err := gate.Renew(gate.Status().Report.ID); err != nil {
		t.Fatal(err)
	}
	s.publishGate(gate.Status())
	fakeMonitorReads(t, func(ctx context.Context) (guestObservation, error) {
		<-ctx.Done()
		return guestObservation{}, noAnswer(ctx.Err())
	}, func(context.Context) (browserObservation, error) {
		return browserObservation{}, errors.New("unexpected")
	})
	s.monitorSafetyWithProbe(exitProbeOK)
	d := s.safety.deferral
	if d.RunID == "" {
		t.Fatal("no deferral recorded")
	}
	if !strings.Contains(d.RetryAt.String(), " m=") {
		t.Fatalf("the next attempt is a wall-clock time only: %s", d.RetryAt)
	}
}

func TestStatusShowsTheLeaseAsTheGateTimesIt(t *testing.T) {
	// After a sleep the gate's wall-clock end has passed while its lease
	// still runs. The desktop and the phone compare until with their own
	// clock, so both must show the gate open with an end still ahead.
	s, _ := unconfirmedMonitorServer(t)
	relayGateControl(t, s.instanceDir(), func(state map[string]any) { shiftUntil(state, -10*time.Minute) })
	client, err := s.gateClient()
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.publishGate(state)
	response := httptest.NewRecorder()
	s.status(response, httptest.NewRequest("GET", "/api/status", nil))
	var status struct {
		Safety struct {
			Gate *cage.GateStatus `json:"gate"`
		} `json:"safety"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if gate := status.Safety.Gate; gate == nil || gate.Mode != cage.GateOpen || !gate.Until.After(time.Now().Add(time.Minute)) {
		t.Fatalf("/api/status gate after the sleep: %+v", gate)
	}
	response = httptest.NewRecorder()
	s.mobileEnvironment(response, httptest.NewRequest("GET", "/api/mobile/environment", nil))
	var phone struct {
		Network struct {
			Mode  string    `json:"mode"`
			Until time.Time `json:"until"`
		} `json:"network"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &phone); err != nil {
		t.Fatal(err)
	}
	if phone.Network.Mode != cage.GateOpen || !phone.Network.Until.After(time.Now().Add(time.Minute)) {
		t.Fatalf("phone network after the sleep: %s", response.Body.String())
	}
}
