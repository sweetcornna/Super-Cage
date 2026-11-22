package workbench

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// mintShort shows a code in the desktop window and returns its 8 digits and
// the long code of the same room.
func (rig *deviceRig) mintShort(t *testing.T) (string, string) {
	t.Helper()
	w := rig.window("POST", "/api/devices/invite", "{}")
	var offer struct{ Short, Manual string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &offer) != nil || len(offer.Short) != mobilelink.DeviceCodeDigits || !strings.HasPrefix(offer.Manual, "CAGE-") {
		t.Fatalf("mint: HTTP %d %s", w.Code, w.Body.String())
	}
	rig.hostWaiting(t, offer.Manual)
	return offer.Short, offer.Manual
}

// redeemShort joins with the 8 digits as another computer does, with its own
// key. The check number goes to checks; its person confirms the number.
func (rig *deviceRig) redeemShort(code string, checks chan<- string) (*testDevice, error) {
	return rig.redeemShortWith(code, checks, func(context.Context, string) bool { return true })
}

func (rig *deviceRig) redeemShortWith(code string, checks chan<- string, confirm func(context.Context, string) bool) (*testDevice, error) {
	identity, err := remotelink.NewIdentity()
	if err != nil {
		return nil, err
	}
	host, err := mobilelink.RedeemDeviceCode(rig.ctx, code, rig.relay, identity, mobilelink.DeviceHello{Name: "MacBook Air", Platform: "darwin"}, func(check string) {
		if checks != nil {
			checks <- check
		}
	}, confirm)
	return &testDevice{identity: identity, host: host}, err
}

func (rig *deviceRig) inviteView(t *testing.T, wait string) hostInviteView {
	t.Helper()
	w := rig.window("GET", "/api/devices/invite?wait="+wait, "")
	var view hostInviteView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("invite state: HTTP %d %s", w.Code, w.Body.String())
	}
	return view
}

// settled reads the invite at once, then holds reads while it still waits
// for a joiner (as the dialog does), and returns the first other state.
func (rig *deviceRig) settled(t *testing.T, from ...string) hostInviteView {
	t.Helper()
	if len(from) == 0 {
		from = []string{"waiting"}
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		view := rig.inviteView(t, "0")
		if !slicesContains(from, view.State) {
			return view
		}
		rig.inviteView(t, "5")
	}
	t.Fatalf("invite stayed in %v", from)
	return hostInviteView{}
}

func slicesContains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func received[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
	panic("unreachable")
}

type redeemed struct {
	device *testDevice
	err    error
}

func (rig *deviceRig) redeemShortAsync(code string, checks chan<- string) <-chan redeemed {
	return rig.redeemShortAsyncWith(code, checks, func(context.Context, string) bool { return true })
}

func (rig *deviceRig) redeemShortAsyncWith(code string, checks chan<- string, confirm func(context.Context, string) bool) <-chan redeemed {
	done := make(chan redeemed, 1)
	go func() {
		d, err := rig.redeemShortWith(code, checks, confirm)
		done <- redeemed{d, err}
	}()
	return done
}

// The 8-digit code: the joiner's request shows the same check number on the
// host, only the window can answer it, and 允许 adds the computer.
func TestDevicesShortCodeApproval(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	short, _ := rig.mintShort(t)
	checks, asked, confirmed := make(chan string, 1), make(chan string, 1), make(chan bool, 1)
	done := rig.redeemShortAsyncWith(short, checks, func(_ context.Context, check string) bool {
		asked <- check
		return <-confirmed
	})
	shown := received(t, checks)
	view := rig.settled(t)
	if view.State != "request" || view.Request == nil || *view.Request != (struct{ Name, Platform, Check string }{"MacBook Air", "darwin", shown}) {
		t.Fatalf("host shows %+v, joiner shows %s", view, shown)
	}
	approve := `{"check":"` + shown + `"}`
	for _, call := range []deviceCall{{agent: true}, {untrusted: true}} {
		for _, path := range []string{"/api/devices/invite/approve", "/api/devices/invite/reject"} {
			if w := deviceRequest(rig.s, "POST", path, approve, call); w.Code != http.StatusForbidden {
				t.Fatalf("%s %+v: HTTP %d", path, call, w.Code)
			}
		}
	}
	other := "0000"
	if shown == other {
		other = "0001"
	}
	if w := rig.window("POST", "/api/devices/invite/approve", `{"check":"`+other+`"}`); w.Code != http.StatusConflict {
		t.Fatalf("允许 with another check number: HTTP %d", w.Code)
	}
	if w := rig.window("POST", "/api/devices/invite/approve", approve); w.Code != http.StatusOK {
		t.Fatalf("允许: HTTP %d %s", w.Code, w.Body.String())
	}
	// 允许 alone saves nothing: the joiner's person confirms the number first.
	if check := received(t, asked); check != shown {
		t.Fatalf("joiner asked to confirm %s, showed %s", check, shown)
	}
	if view := rig.inviteView(t, "0"); view.State != "confirming" || rig.deviceCount() != 0 {
		t.Fatalf("after 允许, before the joiner confirmed: %+v, %d devices", view, rig.deviceCount())
	}
	confirmed <- true
	result := received(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if w := rig.window("POST", "/api/devices/invite/approve", approve); w.Code != http.StatusConflict {
		t.Fatalf("a second 允许: HTTP %d", w.Code)
	}
	if view := rig.settled(t, "waiting", "request", "confirming"); view.State != "joined" || view.Device == nil || view.Device.Name != "MacBook Air" {
		t.Fatalf("after 允许: %+v", view)
	}
	paired := rig.device(t, result.device)
	if paired.Pin != result.device.identity.Pin || paired.Kind != "computer" || paired.Platform != "darwin" || paired.Name != "MacBook Air" {
		t.Fatalf("enrolled %+v", paired.deviceRecord)
	}
	if status := readProjects(t, rig.ctx, rig.connect(t, result.device)); status != http.StatusOK {
		t.Fatalf("the joined computer's link: HTTP %d", status)
	}
	if _, err := rig.redeemShort(short, nil); !errors.Is(err, mobilelink.ErrDeviceCodeUnknown) {
		t.Fatalf("the code after it was used: %v", err)
	}
}

func TestDevicesShortCodeRefusals(t *testing.T) {
	t.Run("拒绝", func(t *testing.T) {
		rig := newDeviceRig(t, relay.Config{})
		short, _ := rig.mintShort(t)
		done := rig.redeemShortAsync(short, nil)
		if view := rig.settled(t); view.State != "request" {
			t.Fatalf("no request: %+v", view)
		}
		if w := rig.window("POST", "/api/devices/invite/reject", "{}"); w.Code != http.StatusOK {
			t.Fatalf("拒绝: HTTP %d", w.Code)
		}
		if result := received(t, done); !errors.Is(result.err, mobilelink.ErrDeviceRejected) {
			t.Fatalf("joiner: %v", result.err)
		}
		if view := rig.settled(t, "waiting", "request"); view.State != "rejected" || rig.deviceCount() != 0 {
			t.Fatalf("after 拒绝: %+v, %d devices", view, rig.deviceCount())
		}
	})
	t.Run("joiner sees another number", func(t *testing.T) {
		rig := newDeviceRig(t, relay.Config{})
		short, _ := rig.mintShort(t)
		checks := make(chan string, 1)
		done := rig.redeemShortAsyncWith(short, checks, func(context.Context, string) bool { return false })
		shown := received(t, checks)
		rig.settled(t)
		if w := rig.window("POST", "/api/devices/invite/approve", `{"check":"`+shown+`"}`); w.Code != http.StatusOK {
			t.Fatalf("允许: HTTP %d", w.Code)
		}
		if result := received(t, done); !errors.Is(result.err, mobilelink.ErrDeviceNotConfirmed) {
			t.Fatalf("joiner: %v", result.err)
		}
		if view := rig.settled(t, "waiting", "request", "confirming"); view.State != "failed" || !strings.Contains(view.Error, "没有确认核对数字") || rig.deviceCount() != 0 {
			t.Fatalf("after the joiner declined: %+v, %d devices", view, rig.deviceCount())
		}
	})
	t.Run("no answer", func(t *testing.T) {
		saved := mobilelink.DeviceApprovalTimeout
		mobilelink.DeviceApprovalTimeout = 300 * time.Millisecond
		defer func() { mobilelink.DeviceApprovalTimeout = saved }()
		rig := newDeviceRig(t, relay.Config{})
		short, _ := rig.mintShort(t)
		if result := received(t, rig.redeemShortAsync(short, nil)); !errors.Is(result.err, mobilelink.ErrDeviceApprovalTimeout) {
			t.Fatalf("joiner: %v", result.err)
		}
		if view := rig.settled(t, "waiting", "request"); view.State != "expired" || view.Error == "" || rig.deviceCount() != 0 {
			t.Fatalf("after no answer: %+v, %d devices", view, rig.deviceCount())
		}
	})
	t.Run("wrong proof", func(t *testing.T) {
		// Whoever holds the QR's room seat but not the code (a photo of the
		// QR, or the relay itself) gets one guess at the code's key.
		rig := newDeviceRig(t, relay.Config{})
		short, manual := rig.mintShort(t)
		raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(manual, "CAGE-"))
		var pair mobilelink.PairCode
		if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "CAGE1:")), &pair); err != nil {
			t.Fatal(err)
		}
		ws, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(rig.relay, "http")+"/v1/rooms/"+pair.Room+"/client", http.Header{"Authorization": {"Bearer " + pair.Token}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		guesser, _ := remotelink.NewIdentity()
		conn, err := remotelink.PairClient(rig.ctx, ws, guesser)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		guess := make([]byte, 32)
		_, _ = rand.Read(guess)
		if _, err := remotelink.JoinHost(rig.ctx, conn, guesser, guess, "Guess", "linux"); !errors.Is(err, remotelink.ErrPairFailed) {
			t.Fatalf("guessed key: %v", err)
		}
		if view := rig.settled(t, "waiting"); view.State != "failed" || view.Error != mobilelink.ErrDeviceCodeFailed.Error() || rig.deviceCount() != 0 {
			t.Fatalf("after a wrong proof: %+v, %d devices", view, rig.deviceCount())
		}
		if _, err := rig.redeemShort(short, nil); !errors.Is(err, mobilelink.ErrDeviceCodeUnknown) {
			t.Fatalf("the code after a wrong proof: %v", err)
		}
	})
	t.Run("second joiner", func(t *testing.T) {
		rig := newDeviceRig(t, relay.Config{})
		short, _ := rig.mintShort(t)
		checks := make(chan string, 1)
		first := rig.redeemShortAsync(short, checks)
		shown := received(t, checks)
		if _, err := rig.redeemShort(short, nil); !errors.Is(err, mobilelink.ErrDeviceCodeUnknown) {
			t.Fatalf("second joiner: %v", err)
		}
		if view := rig.settled(t); view.State != "request" || view.Request.Check != shown {
			t.Fatalf("host after a second joiner: %+v", view)
		}
		if w := rig.window("POST", "/api/devices/invite/approve", `{"check":"`+shown+`"}`); w.Code != http.StatusOK {
			t.Fatalf("允许: HTTP %d", w.Code)
		}
		// The host saves once the joiner's confirmation arrives.
		if result, view := received(t, first), rig.settled(t, "waiting", "request", "confirming"); result.err != nil || view.State != "joined" || rig.deviceCount() != 1 {
			t.Fatalf("first joiner: %v, %+v, %d devices", result.err, view, rig.deviceCount())
		}
	})
	t.Run("code shown by another host", func(t *testing.T) {
		rig := newDeviceRig(t, relay.Config{})
		var registrations atomic.Int32
		conflict := func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/rooms" || r.ContentLength == 0 {
				return false
			}
			if registrations.Add(1) == 1 {
				_, _ = io.Copy(io.Discard, r.Body)
				http.Error(w, "existing device connection", http.StatusConflict)
				return true
			}
			return false
		}
		rig.intercept.Store(&conflict)
		short, _ := rig.mintShort(t)
		if registrations.Load() != 2 {
			t.Fatalf("registrations: %d", registrations.Load())
		}
		checks := make(chan string, 1)
		done := rig.redeemShortAsync(short, checks)
		shown := received(t, checks)
		rig.settled(t)
		if w := rig.window("POST", "/api/devices/invite/approve", `{"check":"`+shown+`"}`); w.Code != http.StatusOK {
			t.Fatalf("允许: HTTP %d", w.Code)
		}
		if result := received(t, done); result.err != nil {
			t.Fatalf("the new code: %v", result.err)
		}
	})
}
