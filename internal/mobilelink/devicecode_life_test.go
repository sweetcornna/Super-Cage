package mobilelink

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// shortJoinWindow shortens the joiner's fixed window for one test.
func shortJoinWindow(t *testing.T, window time.Duration) {
	t.Helper()
	saved := deviceJoinWindow
	deviceJoinWindow = window
	t.Cleanup(func() { deviceJoinWindow = saved })
}

// A relay that answers room creation with an expiry an hour away does not
// decide how long the code lives: the code, the dialog's countdown, the QR's
// e and the host's wait for the joiner's proof (AcceptDevice applies Expires)
// all end within two minutes. A shorter room keeps its own expiry.
func TestDeviceInviteBoundsTheRelaysExpiry(t *testing.T) {
	saved := deviceCodeKDF
	deviceCodeKDF.time, deviceCodeKDF.memory = 1, 64
	t.Cleanup(func() { deviceCodeKDF = saved })
	generous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/rooms" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(relay.Invitation{ID: freshLookupForTest(), HostToken: freshLookupForTest(), ClientToken: freshLookupForTest(), ExpiresAt: time.Now().Add(time.Hour)})
			return
		}
		http.NotFound(w, r)
	}))
	defer generous.Close()
	host, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// Expires is a whole second, taken during the call: at most two minutes
	// after the call returns, at least two minutes (less the second) after it began.
	within := func(expires, before, after time.Time, life time.Duration) bool {
		return !expires.After(after.Add(life)) && !expires.Before(before.Add(life-time.Second))
	}
	before := time.Now()
	inv, err := NewDeviceInvite(context.Background(), generous.URL, "", host)
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if !within(inv.Expires, before, after, 2*time.Minute) {
		t.Fatalf("a relay answering an hour: the code lives %v", inv.Expires.Sub(before).Round(time.Second))
	}
	var qr PairCode
	if err := json.Unmarshal([]byte(strings.TrimPrefix(inv.Raw, "CAGE1:")), &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Expires != inv.Expires.Unix() || inv.Pair.Expires != inv.Expires.Unix() {
		t.Fatalf("QR e %d, pair %d, invite %d", qr.Expires, inv.Pair.Expires, inv.Expires.Unix())
	}

	f := newDeviceFixture(t, 30*time.Second)
	before = time.Now()
	short, err := NewDeviceInvite(context.Background(), f.relay.URL, "", f.host)
	after = time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if !within(short.Expires, before, after, 30*time.Second) {
		t.Fatalf("a 30-second room: the code lives %v", short.Expires.Sub(before).Round(time.Second))
	}
}

// A relay that hands the joiner a room expiring in an hour and then withholds
// the host's first message (its proof of the code) buys no time: joining,
// TLS and the host's proof are bounded by a short fixed window from the
// lookup, not by the relay's expiry. No check number is ever shown.
func TestDeviceCodeJoinWindowEndsAStalledJoin(t *testing.T) {
	f := newDeviceFixture(t, time.Hour)
	shortJoinWindow(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	inv, err := NewDeviceInvite(ctx, f.relay.URL, "", f.host)
	if err != nil {
		t.Fatal(err)
	}
	// The host's seat as this relay presents it: TLS completes, frame 1 never comes.
	ws, err := join(ctx, f.relay.URL, inv.Pair.Room, "host", inv.hostToken)
	if err != nil {
		t.Fatal(err)
	}
	temp, err := remotelink.IdentityFromKey(ed25519.NewKeyFromSeed(inv.Pair.Seed))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if conn, err := remotelink.AcceptAfterJoin(ctx, ws, f.host, temp.Pin, true); err == nil {
			<-ctx.Done()
			_ = conn.Close()
		}
	}()
	lookup, _, err := deviceCodeKeys(ctx, inv.Code, f.relay.URL)
	if err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); ; time.Sleep(5 * time.Millisecond) {
		room, err := LookupRoom(ctx, f.relay.URL, lookup)
		if err == nil {
			if time.Until(room.ExpiresAt) < 50*time.Minute {
				t.Fatalf("the relay should offer an hour, offers %v", time.Until(room.ExpiresAt))
			}
			break
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("host never joined its room:", err)
		}
	}
	start := time.Now()
	_, err = f.redeem(ctx, inv.Code, func(string) { t.Error("a check number without the host's proof") })
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("join with frame 1 withheld: %v (caller's context: %v)", err, ctx.Err())
	}
	if elapsed > deviceJoinWindow+3*time.Second {
		t.Fatalf("the stalled join lasted %v; the window is %v", elapsed.Round(10*time.Millisecond), deviceJoinWindow)
	}
}

// The fixed window covers only finding the room and both proofs: the host's
// 允许 and the joiner's own confirmation keep their designed bounds.
func TestDeviceCodeJoinWindowLeavesThePersonsTime(t *testing.T) {
	f := newDeviceFixture(t, time.Hour)
	shortJoinWindow(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	slow := func() { time.Sleep(1500 * time.Millisecond) }
	inv, done := f.serve(ctx, func(context.Context, DeviceRequest) bool { slow(); return true })
	if _, err := f.redeemWith(ctx, inv.Code, nil, func(context.Context, string) bool { slow(); return true }); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if asked, saved := f.counts(); asked != 1 || saved != 1 {
		t.Fatalf("asked %d, saved %d", asked, saved)
	}
}
