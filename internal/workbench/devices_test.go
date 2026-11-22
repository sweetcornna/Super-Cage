package workbench

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/keepawake"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// deviceRig is one workbench Server with a loopback relay that counts every
// request, and a counter of the local session exchanges the device loops make.
type deviceRig struct {
	s         *Server
	relay     string
	relayHits atomic.Int64
	hostSeats sync.Map // room ids whose host seat was joined
	sessions  atomic.Int64
	holds     atomic.Int32 // keep-awake holds now
	maxHolds  atomic.Int32
	ctx       context.Context
}

func newDeviceRig(t *testing.T, config relay.Config) *deviceRig {
	t.Helper()
	rig := &deviceRig{}
	config.PublicRooms, config.AdminToken = true, strings.Repeat("x", 32)
	edge, err := relay.New(config)
	if err != nil {
		t.Fatal(err)
	}
	relayHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.relayHits.Add(1)
		if room, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/rooms/"), "/host"); ok {
			rig.hostSeats.Store(room, true)
		}
		edge.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { edge.Close(); relayHTTP.Close() })
	rig.relay = relayHTTP.URL
	s, _ := grantedToolUpdateServer(t)
	s.keepAwake = func(ctx context.Context, report func(keepawake.State)) {
		now := rig.holds.Add(1)
		for {
			most := rig.maxHolds.Load()
			if now <= most || rig.maxHolds.CompareAndSwap(most, now) {
				break
			}
		}
		report(keepawake.State{Active: true})
		<-ctx.Done()
		rig.holds.Add(-1)
		report(keepawake.State{})
	}
	handler := s.handler()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session" {
			rig.sessions.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	s.origin = "http://" + server.Listener.Addr().String()
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(s.stopDevices)
	s.preferences.MobileRelay = rig.relay
	rig.s = s
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	rig.ctx = ctx
	return rig
}

// window sends a request as the desktop window of this computer does.
func (rig *deviceRig) window(method, path, body string) *httptest.ResponseRecorder {
	s := rig.s
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

type testDevice struct {
	identity *remotelink.Identity
	host     mobilelink.PairedHost
}

// mint shows a code in the desktop window, as the settings page does.
func (rig *deviceRig) mint(t *testing.T) string {
	t.Helper()
	w := rig.window("POST", "/api/mobile-pair", "{}")
	var code struct{ Manual string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &code) != nil || !strings.HasPrefix(code.Manual, "CAGE-") {
		t.Fatalf("mint: HTTP %d %s", w.Code, w.Body.String())
	}
	return code.Manual
}

// redeem pairs a new device identity with code, as today's phone app does.
func (rig *deviceRig) redeem(code string) (*testDevice, error) {
	identity, err := remotelink.NewIdentity()
	if err != nil {
		return nil, err
	}
	credentials, err := mobilelink.RedeemPair(rig.ctx, code, rig.relay, identity.Pin)
	if err != nil {
		return nil, err
	}
	d := &testDevice{identity: identity}
	return d, json.Unmarshal([]byte(credentials), &d.host)
}

func (rig *deviceRig) pair(t *testing.T) *testDevice {
	t.Helper()
	d, err := rig.redeem(rig.mint(t))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// connect finds the device's room through the relay and opens its link.
func (rig *deviceRig) connect(t *testing.T, d *testDevice) *mobilelink.Client {
	t.Helper()
	for {
		room, err := mobilelink.LookupRoom(rig.ctx, rig.relay, d.host.Lookup)
		if err == nil {
			client, err := mobilelink.ConnectClient(rig.ctx, rig.relay, room.ID, room.ClientToken, d.identity, d.host.HostPin)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			return client
		}
		if !errors.Is(err, mobilelink.ErrNoInvitation) || rig.ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// hostWaiting waits until this computer holds the host seat of code's room.
// A code that dies before that leaves its room on the relay until the relay's
// own pairing time; no host ever answers it.
func (rig *deviceRig) hostWaiting(t *testing.T, code string) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, "CAGE-"))
	var pair mobilelink.PairCode
	if err != nil || json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "CAGE1:")), &pair) != nil {
		t.Fatal("unreadable code")
	}
	eventually(t, "the host to wait in its room", 5*time.Second, func() bool {
		_, ok := rig.hostSeats.Load(pair.Room)
		return ok
	})
}

// discoverable waits until the device's room is registered, without joining it.
func (rig *deviceRig) discoverable(t *testing.T, d *testDevice) {
	t.Helper()
	for {
		_, err := mobilelink.LookupRoom(rig.ctx, rig.relay, d.host.Lookup)
		if err == nil {
			return
		}
		if !errors.Is(err, mobilelink.ErrNoInvitation) || rig.ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gone waits until the relay no longer knows the device's room.
func (rig *deviceRig) gone(t *testing.T, d *testDevice, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		_, err := mobilelink.LookupRoom(rig.ctx, rig.relay, d.host.Lookup)
		if errors.Is(err, mobilelink.ErrNoInvitation) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay still offers the room of a removed device: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readProjects(t *testing.T, ctx context.Context, client *mobilelink.Client) int {
	t.Helper()
	reply, err := client.Do(ctx, "GET", "/api/mobile/cli/projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	return reply.Status
}

func (rig *deviceRig) deviceCount() int {
	rig.s.phones.mu.Lock()
	defer rig.s.phones.mu.Unlock()
	return len(rig.s.phones.devices)
}

func (rig *deviceRig) device(t *testing.T, d *testDevice) *pairedDevice {
	t.Helper()
	rig.s.phones.mu.Lock()
	defer rig.s.phones.mu.Unlock()
	for _, p := range rig.s.phones.devices {
		if p.Lookup == d.host.Lookup {
			return p
		}
	}
	t.Fatal("device not in the list")
	return nil
}

// With no device paired and no code showing nothing runs: no goroutine, no
// keep-awake hold and not one relay request.
func TestDevicesIdleHostCostsNothing(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	runtime.GC()
	before := runtime.NumGoroutine()
	if err := rig.s.restoreDevices(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if hits := rig.relayHits.Load(); hits != 0 {
		t.Fatalf("an idle host made %d relay requests", hits)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("an idle host started %d goroutines", after-before)
	}
	rig.s.phones.mu.Lock()
	power := rig.s.phones.power.cancel
	rig.s.phones.mu.Unlock()
	if power != nil || rig.holds.Load() != 0 {
		t.Fatal("an idle host holds the computer awake")
	}
	if w := rig.window("GET", "/api/mobile-pair", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"paired":false`) {
		t.Fatalf("idle pairing status: %d %s", w.Code, w.Body.String())
	}
	if hits := rig.relayHits.Load(); hits != 0 {
		t.Fatalf("reading the status made %d relay requests", hits)
	}
}

// Two devices through one relay to one Server: the local session opens only
// for an authenticated device, removal cuts one device at once and leaves the
// other, a lock cuts both and an unlock restores both without re-pairing, one
// keep-awake hold covers every device, and the sixth device is refused.
func TestDevicesServeSeveralDevices(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	s, ctx := rig.s, rig.ctx
	var holdOpen atomic.Bool
	opened := make(chan struct{}, 8)
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/phone-browser/open" && holdOpen.Load() {
			opened <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	}))

	a := rig.pair(t)
	rig.discoverable(t, a)
	time.Sleep(100 * time.Millisecond)
	if n := rig.sessions.Load(); n != 0 {
		t.Fatalf("a paired device that has not connected cost %d local session exchanges", n)
	}
	clientA := rig.connect(t, a)
	if status := readProjects(t, ctx, clientA); status != 200 {
		t.Fatalf("device A: %d", status)
	}
	if n := rig.sessions.Load(); n != 1 {
		t.Fatalf("device A opened %d local sessions", n)
	}
	b := rig.pair(t)
	clientB := rig.connect(t, b)
	if status := readProjects(t, ctx, clientB); status != 200 {
		t.Fatalf("device B: %d", status)
	}
	if rig.deviceCount() != 2 || rig.maxHolds.Load() != 1 || rig.holds.Load() != 1 {
		t.Fatalf("devices %d, keep-awake holds %d (most %d)", rig.deviceCount(), rig.holds.Load(), rig.maxHolds.Load())
	}

	// A request of device A is in flight when A is removed.
	holdOpen.Store(true)
	inFlight := make(chan error, 1)
	go func() {
		reply, err := clientA.Do(ctx, "POST", "/api/mobile/browser/open", []byte(`{"id":"5d1bfa83-6a3e-4c4e-9a52-0d1f0e6a9b11","width":390,"height":640}`))
		if err == nil && reply.Status == 200 {
			err = errors.New("opened")
		}
		inFlight <- err
	}()
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("device A's request never reached the guest")
	}
	deviceA := rig.device(t, a)
	s.phones.mu.Lock()
	err := s.removeDeviceLocked(deviceA)
	s.phones.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	removed := time.Now()
	select {
	case <-inFlight:
		if took := time.Since(removed); took > time.Second {
			t.Fatalf("removed device's request ended after %v", took)
		}
	case <-time.After(time.Second):
		t.Fatal("removed device's request is still running")
	}
	holdOpen.Store(false)
	rig.gone(t, a, 3*time.Second)
	if reply, err := clientA.Do(ctx, "GET", "/api/mobile/cli/projects", nil); err == nil && reply.Status == 200 {
		t.Fatal("removed device still reads projects")
	}
	if status := readProjects(t, ctx, clientB); status != 200 {
		t.Fatalf("device B after removing A: %d", status)
	}
	s.phones.browser.op.Lock()
	browser := s.phones.browser.session
	s.phones.browser.op.Unlock()
	if browser != nil {
		t.Fatal("the removed device's browser session is still open")
	}
	if rig.holds.Load() != 1 {
		t.Fatal("removing one of two devices released keep-awake")
	}
	list, err := readDevicesFile(s.devicesFile())
	if err != nil || list == nil || len(list.Devices) != 1 || list.Devices[0].Lookup != b.host.Lookup {
		t.Fatalf("device list on disk after removal: %+v %v", list, err)
	}

	// Lock cuts every device and its surface; unlock restores them.
	reply, err := clientB.Do(ctx, "POST", "/api/mobile/browser/open", []byte(`{"id":"7b0e1c55-51a4-4d0e-8f3e-2a7c9d1e4f22","width":390,"height":640}`))
	if err != nil || reply.Status != 200 {
		t.Fatalf("device B browser: %+v %v", reply, err)
	}
	c := rig.pair(t)
	clientC := rig.connect(t, c)
	s.account.lock()
	for name, client := range map[string]*mobilelink.Client{"B": clientB, "C": clientC} {
		if status := readProjects(t, ctx, client); status != http.StatusLocked {
			t.Fatalf("device %s while locked: %d", name, status)
		}
	}
	eventually(t, "the lock to close the browser surface", 3*time.Second, func() bool {
		s.phones.browser.op.Lock()
		defer s.phones.browser.op.Unlock()
		return s.phones.browser.session == nil
	})
	if w := rig.window("POST", "/api/mobile-pair", "{}"); w.Code != http.StatusLocked {
		t.Fatalf("minted while locked: %d", w.Code)
	}
	s.account.setKey(make([]byte, 32))
	for name, client := range map[string]*mobilelink.Client{"B": clientB, "C": clientC} {
		if status := readProjects(t, ctx, client); status != 200 {
			t.Fatalf("device %s after unlock: %d", name, status)
		}
	}

	// Up to five devices, phones and computers together.
	for rig.deviceCount() < deviceLimit {
		rig.pair(t)
	}
	if w := rig.window("POST", "/api/mobile-pair", "{}"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "最多添加 5 台设备") {
		t.Fatalf("sixth device: %d %s", w.Code, w.Body.String())
	}
	if rig.maxHolds.Load() != 1 {
		t.Fatalf("%d keep-awake holds at once", rig.maxHolds.Load())
	}
	// Removing the last device releases the hold.
	s.phones.mu.Lock()
	for len(s.phones.devices) > 0 {
		if err := s.removeDeviceLocked(s.phones.devices[0]); err != nil {
			s.phones.mu.Unlock()
			t.Fatal(err)
		}
	}
	s.phones.mu.Unlock()
	eventually(t, "keep-awake to end with the last device", 3*time.Second, func() bool { return rig.holds.Load() == 0 })
}

// A code dies on lock, on a newer code, and after the relay's pairing time;
// it pairs one device only.
func TestDevicesCodesDie(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	s := rig.s
	// Refresh: the first code no longer pairs.
	first := rig.mint(t)
	rig.hostWaiting(t, first)
	second := rig.mint(t)
	if _, err := rig.redeem(first); err == nil {
		t.Fatal("a replaced code paired a device")
	}
	// One use: a second device cannot use the same code.
	if _, err := rig.redeem(second); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.redeem(second); err == nil {
		t.Fatal("a code paired two devices")
	}
	// Lock: the code dies and its room is gone from the relay.
	code := rig.mint(t)
	rig.hostWaiting(t, code)
	s.account.lock()
	locked := time.Now()
	if _, err := rig.redeem(code); err == nil {
		t.Fatal("a code paired a device after the computer was locked")
	}
	if took := time.Since(locked); took > 5*time.Second {
		t.Fatalf("the locked code's room stayed on the relay (%v)", took)
	}
	s.account.setKey(make([]byte, 32))
	if rig.deviceCount() != 1 {
		t.Fatalf("devices: %d", rig.deviceCount())
	}
	// Timeout: a code whose room expired.
	short := newDeviceRig(t, relay.Config{PairTimeout: 300 * time.Millisecond})
	late := short.mint(t)
	time.Sleep(500 * time.Millisecond)
	if _, err := short.redeem(late); err == nil {
		t.Fatal("an expired code paired a device")
	}
}

// A phone paired by today's version, in today's file, keeps working with no
// re-pairing: the same lookup hash is registered and its old pin connects.
// Deleting that file (an older version removed the phone) drops it.
func TestDevicesTodaysPhoneKeepsWorking(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	identity, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, 32)
	seed[0] = 1
	host, err := remotelink.IdentityFromKey(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := lookupToken()
	if err != nil {
		t.Fatal(err)
	}
	fixture := `{"Seed":"` + base64.StdEncoding.EncodeToString(seed) + `","Pin":"` + identity.Pin + `","Relay":"` + rig.relay + `","Lookup":"` + lookup + `","Admin":"","device":{"platform":"ios","model":"iPhone 16 Pro"}}`
	if err := os.WriteFile(rig.s.phoneFile(), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rig.s.restoreDevices(); err != nil {
		t.Fatal(err)
	}
	phone := &testDevice{identity: identity, host: mobilelink.PairedHost{Version: 1, Relay: rig.relay, HostPin: host.Pin, Lookup: lookup}}
	client := rig.connect(t, phone)
	if status := readProjects(t, rig.ctx, client); status != 200 {
		t.Fatalf("migrated phone: %d", status)
	}
	if w := rig.window("GET", "/api/mobile-pair", ""); !strings.Contains(w.Body.String(), `"model":"iPhone 16 Pro"`) || !strings.Contains(w.Body.String(), `"paired":true`) {
		t.Fatalf("migrated phone status: %s", w.Body.String())
	}
	if b, err := os.ReadFile(rig.s.phoneFile()); err != nil || string(b) != fixture {
		t.Fatal("the phone file of earlier versions was changed")
	}
	client.Close()
	rig.s.stopDevices()
	rig.gone(t, phone, 3*time.Second)

	// Removed by an older version: gone on the next start.
	if err := os.Remove(rig.s.phoneFile()); err != nil {
		t.Fatal(err)
	}
	next := newDeviceRig(t, relay.Config{})
	next.s.profile = rig.s.profile
	if err := next.s.restoreDevices(); err != nil {
		t.Fatal(err)
	}
	if next.deviceCount() != 0 {
		t.Fatal("a phone removed by an older version came back")
	}
	if list, err := readDevicesFile(next.s.devicesFile()); err != nil || list == nil || len(list.Devices) != 0 {
		t.Fatalf("list after the legacy phone was removed: %+v %v", list, err)
	}
}
