package cage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGateStatusCarriesTheRemainingLeaseAndClientTimesIt(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.ProxyURL = "http://127.0.0.1:9"
	path := filepath.Join(dir, "config.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	g, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close("test complete") })
	stop, err := StartGateControl(path, dir, g)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client, err := OpenGateClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if closed.LeaseRemainingMS != nil || !closed.Deadline().IsZero() {
		t.Fatalf("a closed gate reports a lease: %+v deadline=%s", closed.LeaseRemainingMS, closed.Deadline())
	}
	fixtureGrant(t, g)
	sent := time.Now()
	state, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.LeaseRemainingMS == nil || *state.LeaseRemainingMS <= 0 || *state.LeaseRemainingMS > GateLease.Milliseconds() {
		t.Fatalf("lease_remaining_ms: %v", state.LeaseRemainingMS)
	}
	deadline := state.Deadline()
	if !strings.Contains(deadline.String(), " m=") {
		t.Fatalf("the client's deadline is not on the monotonic clock: %s", deadline)
	}
	if real := g.Status().Deadline(); deadline.After(real) || real.Sub(deadline) > time.Second || deadline.Before(sent) {
		t.Fatalf("client deadline %s, gate deadline %s", deadline, real)
	}
	// The wire form keeps until and adds the remaining lease.
	data, _ = json.Marshal(state)
	if !strings.Contains(string(data), "\"lease_remaining_ms\":") || !strings.Contains(string(data), "\"until\":") {
		t.Fatalf("status JSON: %s", data)
	}
}

func TestLocalDeadlineFromOlderHostAgents(t *testing.T) {
	sent := time.Now()
	wall := sent.Round(0)
	remaining := int64(40_000)
	for _, tc := range []struct {
		name  string
		state GateStatus
		want  time.Duration // from sent; negative: no deadline
	}{
		{"closed", GateStatus{Mode: GateClosed, Until: wall.Add(time.Minute)}, -1},
		{"current agent", GateStatus{Mode: GateOpen, Until: wall.Add(-10 * time.Minute), LeaseRemainingMS: &remaining}, 40 * time.Second},
		{"older agent, wall end ahead", GateStatus{Mode: GateOpen, Until: wall.Add(50 * time.Second)}, 50 * time.Second},
		{"older agent, wall clock stepped back", GateStatus{Mode: GateOpen, Until: wall.Add(10 * time.Minute)}, GateLease},
		{"older agent, check stepped back", GateStatus{Mode: GateChecking, Until: wall.Add(time.Hour)}, gateCheckLease},
		{"older agent after the host slept", GateStatus{Mode: GateOpen, Until: wall.Add(-10 * time.Minute)}, olderGateUnknownLease},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.state.localDeadline(sent)
			if tc.want < 0 {
				if !got.IsZero() {
					t.Fatalf("deadline %s for a closed gate", got)
				}
				return
			}
			if d := got.Sub(sent); d < tc.want-time.Millisecond || d > tc.want+time.Millisecond || !strings.Contains(got.String(), " m=") {
				t.Fatalf("deadline %s after sent (%s), want %s on the monotonic clock", d, got, tc.want)
			}
		})
	}
}
