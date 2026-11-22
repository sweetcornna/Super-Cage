package workbench

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/relay"
)

// request sends method path as the desktop window (trusted) or a plain
// session (a browser opened by ticket), optionally marked as the agent transport.
func (rig *deviceRig) request(method, path, body string, trusted, agent bool) *httptest.ResponseRecorder {
	s := rig.s
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	if trusted {
		r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
	}
	if agent {
		r.Header.Set(agentRequestHeader, "1")
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

// Only the desktop window mints a code: the agent transport is refused by the
// CLI and by the server (it carries the trusted-view cookie too), and a
// session without the window's trusted view is refused.
func TestDevicesMintIsWindowOnly(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	for _, path := range []string{"/api/devices/invite", "/api/mobile-pair"} {
		if refusal := agentRouteRefusal("POST", path); refusal == nil || refusal.Code != "native_transport_required" || refusal.NextAction != "open_desktop" {
			t.Fatalf("the CLI sends POST %s: %v", path, refusal)
		}
		if w := rig.request("POST", path, "{}", true, true); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "只能在桌面窗口操作") {
			t.Fatalf("agent header on %s: %d %s", path, w.Code, w.Body.String())
		}
		if w := rig.request("POST", path, "{}", false, false); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Super Cage 窗口") {
			t.Fatalf("no trusted view on %s: %d %s", path, w.Code, w.Body.String())
		}
		for _, mode := range []string{"desktop", "client"} {
			rig.s.desktopEntry, rig.s.clientOnly = mode == "desktop", mode == "client"
			w := rig.request("POST", path, "{}", true, false)
			rig.s.desktopEntry, rig.s.clientOnly = false, false
			if w.Code != http.StatusNotFound {
				t.Fatalf("the %s process answered POST %s itself: %d", mode, path, w.Code)
			}
		}
		if w := rig.request("POST", path, "{}", true, false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"manual":"CAGE-`) {
			t.Fatalf("the window could not mint on %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if rig.relayHits.Load() == 0 {
		t.Fatal("no code reached the relay")
	}
}

// The add-device dialog's routes: list, mint, held read of the code's state,
// cancel (the code dies), remove; and an expired code reads as expired.
func TestDevicesRoutesDriveTheAddDialog(t *testing.T) {
	rig := newDeviceRig(t, relay.Config{})
	type listReply struct {
		Devices []struct {
			ID, Kind, Platform, Name string
			AddedAt                  time.Time  `json:"added_at"`
			LastSeen                 *time.Time `json:"last_seen"`
			Connected                bool
		}
		Limit     int
		KeepAwake struct{ Active bool } `json:"keep_awake"`
		Invite    *struct{ State string }
		Problem   string
	}
	list := func() (listReply, string) {
		t.Helper()
		w := rig.request("GET", "/api/devices", "", true, false)
		var reply listReply
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		return reply, w.Body.String()
	}
	type inviteReply struct {
		State   string
		Expires *time.Time
		Device  *struct{ ID, Name string }
	}
	invite := func(wait string) inviteReply {
		t.Helper()
		w := rig.request("GET", "/api/devices/invite?wait="+wait, "", true, false)
		var reply inviteReply
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("invite state: %d %s", w.Code, w.Body.String())
		}
		return reply
	}
	if reply, raw := list(); len(reply.Devices) != 0 || reply.Limit != 5 || reply.Invite != nil || reply.KeepAwake.Active || !strings.Contains(raw, `"devices":[]`) {
		t.Fatalf("empty list: %s", raw)
	}
	start := time.Now()
	if reply := invite("25"); reply.State != "none" || time.Since(start) > time.Second {
		t.Fatalf("no code: %+v after %v", reply, time.Since(start))
	}
	if w := rig.request("GET", "/api/devices/invite?wait=26", "", true, false); w.Code != http.StatusBadRequest {
		t.Fatalf("wait=26: %d", w.Code)
	}

	w := rig.request("POST", "/api/devices/invite", "{}", true, false)
	var minted struct {
		Image, Manual string
		Expires       time.Time
		Short         *string
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &minted) != nil || !strings.HasPrefix(minted.Image, "data:image/png;base64,") || minted.Short == nil || len(*minted.Short) != 8 || !minted.Expires.After(time.Now()) {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	if reply := invite("0"); reply.State != "waiting" || reply.Expires == nil {
		t.Fatalf("showing code: %+v", reply)
	}
	if reply, _ := list(); reply.Invite == nil || reply.Invite.State != "waiting" {
		t.Fatalf("list while a code shows: %+v", reply.Invite)
	}
	rig.hostWaiting(t, minted.Manual)
	held := make(chan inviteReply, 1)
	go func() { held <- invite("25") }()
	time.Sleep(100 * time.Millisecond)
	device, err := rig.redeem(minted.Manual)
	if err != nil {
		t.Fatal(err)
	}
	var joined inviteReply
	select {
	case joined = <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the held read did not answer the join")
	}
	if joined.State != "joined" || joined.Device == nil || len(joined.Device.ID) != 32 || joined.Device.Name != "" {
		t.Fatalf("joined: %+v", joined)
	}
	reply, raw := list()
	if len(reply.Devices) != 1 || reply.Devices[0].ID != joined.Device.ID || reply.Devices[0].Kind != "phone" || reply.Devices[0].Platform != "" ||
		reply.Devices[0].Name != "" || reply.Devices[0].LastSeen != nil || reply.Devices[0].AddedAt.IsZero() || !reply.KeepAwake.Active {
		t.Fatalf("new device before its report: %s", raw)
	}
	client := rig.connect(t, device)
	if reply, err := client.Do(rig.ctx, "POST", "/api/mobile/device", []byte(`{"platform":"linux","model":"实验室工作站"}`)); err != nil || reply.Status != 200 {
		t.Fatalf("report: %+v %v", reply, err)
	}
	if reply := invite("0"); reply.State != "joined" || reply.Device == nil || reply.Device.Name != "实验室工作站" {
		t.Fatalf("joined device after its report: %+v", reply)
	}
	reply, raw = list()
	if reply.Devices[0].Kind != "computer" || reply.Devices[0].Platform != "linux" || !reply.Devices[0].Connected || reply.Devices[0].LastSeen == nil {
		t.Fatalf("reported device: %s", raw)
	}
	record := rig.device(t, device)
	for _, secret := range []string{device.host.Lookup, device.identity.Pin, device.host.HostPin, base64Std(record.Seed), "seed", "lookup", "\"pin\"", "admin"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the device list carries %q", secret)
		}
	}

	// Closing the dialog kills its code.
	w = rig.request("POST", "/api/devices/invite", "{}", true, false)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &minted) != nil {
		t.Fatalf("second mint: %d", w.Code)
	}
	rig.hostWaiting(t, minted.Manual)
	if w := rig.request("DELETE", "/api/devices/invite", "", true, false); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"none"`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if _, err := rig.redeem(minted.Manual); err == nil {
		t.Fatal("a cancelled code paired a device")
	}
	if reply := invite("0"); reply.State != "none" {
		t.Fatalf("after cancel: %+v", reply)
	}

	// Removal.
	if w := rig.request("DELETE", "/api/devices/"+strings.Repeat("0", 32), "", true, false); w.Code != http.StatusNotFound {
		t.Fatalf("unknown device: %d", w.Code)
	}
	if w := rig.request("DELETE", "/api/devices/"+joined.Device.ID, "", false, true); w.Code != 200 || !strings.Contains(w.Body.String(), joined.Device.ID) {
		t.Fatalf("remove (as an agent may): %d %s", w.Code, w.Body.String())
	}
	rig.gone(t, device, 3*time.Second)
	if reply, raw := list(); len(reply.Devices) != 0 || reply.KeepAwake.Active {
		t.Fatalf("after removal: %s", raw)
	}

	// A code the relay let expire reads as expired.
	short := newDeviceRig(t, relay.Config{PairTimeout: 400 * time.Millisecond})
	if w := short.request("POST", "/api/devices/invite", "{}", true, false); w.Code != 200 {
		t.Fatalf("mint: %d", w.Code)
	}
	w = short.request("GET", "/api/devices/invite?wait=10", "", true, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"expired"`) {
		t.Fatalf("expired code: %d %s", w.Code, w.Body.String())
	}
}

// With an SSH host or cloud workspace selected, the device routes are refused
// in the desktop gateway and never reach that host; with this computer's own
// workload selected they are forwarded to it.
func TestDevicesRoutesNeverReachARemoteTarget(t *testing.T) {
	gateway, local := gatewayFixture(t)
	gateway.desktopEntry = true
	d := gateway.remote.desktop
	generation := chooseLocal(t, d, d.context().ConnectionID)
	send := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, gateway.origin+path, strings.NewReader("{}"))
		r.Header.Set("Origin", gateway.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: gateway.cookieName(), Value: gateway.session})
		r.Header.Set(connectionHeader, generation)
		w := httptest.NewRecorder()
		gateway.handler().ServeHTTP(w, r)
		return w
	}
	if w := send("GET", "/api/devices"); w.Code != http.StatusOK || local.commands.Load() != 1 {
		t.Fatalf("the local workload's device list: %d, requests %d", w.Code, local.commands.Load())
	}
	d.mu.Lock()
	d.active.mode = "remote"
	d.mu.Unlock()
	for _, route := range [][2]string{{"GET", "/api/devices"}, {"POST", "/api/devices/invite"}, {"GET", "/api/devices/invite"}, {"DELETE", "/api/devices/invite"}, {"DELETE", "/api/devices/" + strings.Repeat("a", 32)}} {
		if w := send(route[0], route[1]); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "再管理设备") || local.commands.Load() != 1 {
			t.Fatalf("%s %s with a remote target: %d %s, requests %d", route[0], route[1], w.Code, w.Body.String(), local.commands.Load())
		}
	}
}

// The device routes are answered by every guest's workload, not refused as
// unsupported on macOS or Windows guests.
func TestDevicesRoutesReachEveryGuest(t *testing.T) {
	for _, path := range []string{"/api/devices", "/api/devices/invite", "/api/devices/" + strings.Repeat("a", 32)} {
		if !nativeWorkbenchRoute(path) || !windowsWorkbenchRoute(path) || !macOSWorkbenchRoute(path) {
			t.Errorf("%s is not a route of every guest", path)
		}
		if phoneRoomRoute(path) {
			t.Errorf("%s is a phone route", path)
		}
	}
	for _, guest := range []string{"macos", "windows"} {
		s := testServer(t)
		if guest == "macos" {
			s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"}
		} else {
			s.windowsGuest = &guestos.WindowsProfile{}
		}
		r := httptest.NewRequest("GET", s.origin+"/api/devices", nil)
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"limit":5`) {
			t.Errorf("%s guest: %d %s", guest, w.Code, w.Body.String())
		}
	}
}

// A paired device, a phone or a joined computer, reaches only the phone
// surface: for every route any listener registers and every window-only
// route, a path outside /api/mobile/ is neither a phone route on the host nor
// sent by the device's link. The one window-only route under /api/mobile/ is
// the device's own report.
func TestDevicesLinkReachesOnlyThePhoneSurface(t *testing.T) {
	patterns := map[string]bool{}
	for _, listener := range []string{"workload", "desktop", "client"} {
		for _, guest := range []string{"linux", "macos", "windows"} {
			s := testServer(t)
			s.desktopEntry, s.clientOnly = listener == "desktop", listener == "client"
			switch guest {
			case "macos":
				s.macosGuest = &guestos.MacOSProfile{Instance: "test", User: "test"}
			case "windows":
				s.windowsGuest = &guestos.WindowsProfile{}
			}
			var routes []string
			if err := json.Unmarshal(request(s, "GET", "/api/agent/routes", "", "", true).Body.Bytes(), &routes); err != nil || len(routes) < 50 {
				t.Fatal(listener, guest, err)
			}
			for _, route := range routes {
				patterns[route] = true
			}
		}
	}
	var windowMobile []string
	for _, rule := range agentWindowRoutes {
		patterns[rule.Pattern] = true
		if _, path, _ := strings.Cut(rule.Pattern, " "); strings.HasPrefix(path, "/api/mobile/") || strings.HasPrefix(rule.Pattern, "/api/mobile/") {
			windowMobile = append(windowMobile, rule.Pattern)
		}
	}
	if !slices.Equal(windowMobile, []string{"POST /api/mobile/device"}) {
		t.Fatalf("window-only routes under /api/mobile/: %v", windowMobile)
	}
	for _, required := range []string{"POST /api/devices/invite", "DELETE /api/devices/{id}", "POST /api/account/recovery-generate", "POST /api/entry/on"} {
		if !patterns[required] {
			t.Fatalf("the walk did not see %s", required)
		}
	}
	rig := newDeviceRig(t, relay.Config{})
	client := rig.connect(t, rig.pair(t))
	segment := regexp.MustCompile(`\{[^/]*\}`)
	checked := 0
	for pattern := range patterns {
		methods, path, ok := strings.Cut(pattern, " ")
		if !ok {
			methods, path = "GET,POST", pattern
		}
		path = segment.ReplaceAllString(path, "x")
		if strings.HasSuffix(path, "/") {
			path += "x"
		}
		if !strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api/mobile/") {
			continue
		}
		if phoneRoomRoute(path) {
			t.Errorf("%s is a phone route", path)
		}
		for _, method := range strings.Split(methods, ",") {
			var body []byte
			if method != "GET" && method != "HEAD" {
				body = []byte("{}")
			}
			if reply, err := client.Do(rig.ctx, method, path, body); err == nil {
				t.Errorf("a device link sent %s %s (HTTP %d)", method, path, reply.Status)
			}
			checked++
		}
	}
	if checked < 100 {
		t.Fatalf("only %d routes checked", checked)
	}
	if reply, err := client.Do(rig.ctx, "POST", "/api/account/recovery-generate", []byte(`{"password":"x"}`)); err == nil {
		t.Fatalf("a recovery key over a device link: HTTP %d", reply.Status)
	}
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
