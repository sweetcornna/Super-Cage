package workbench

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// shortCodeForTest shows a code on host as the add-device dialog does and
// returns its 8 digits.
func shortCodeForTest(t *testing.T, host *Server) string {
	t.Helper()
	w := deviceRequest(host, "POST", "/api/devices/invite", "{}", deviceCall{})
	var offer struct{ Short, Manual string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &offer) != nil || len(offer.Short) != mobilelink.DeviceCodeDigits || !strings.HasPrefix(offer.Manual, "CAGE-") {
		t.Fatalf("host code: HTTP %d %s", w.Code, w.Body.String())
	}
	return offer.Short
}

type hostInviteView struct {
	State   string
	Request *struct{ Name, Platform, Check string }
	Device  *struct{ ID, Name string }
	Error   string
}

func hostInviteForTest(t *testing.T, host *Server, wait string) hostInviteView {
	t.Helper()
	w := deviceRequest(host, "GET", "/api/devices/invite?wait="+wait, "", deviceCall{})
	var view hostInviteView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("host invite: HTTP %d %s", w.Code, w.Body.String())
	}
	return view
}

// hostRequestForTest waits until the host shows a joiner's request. Like the
// dialog it reads at once, then holds a read only while it is still waiting.
func hostRequestForTest(t *testing.T, host *Server) hostInviteView {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if view := hostInviteForTest(t, host, "0"); view.State != "waiting" {
			return view
		}
		hostInviteForTest(t, host, "5")
	}
	t.Fatal("the host never showed a request")
	return hostInviteView{}
}

// joinByCodeForTest posts the code as a person types it. The host's socket
// joins its room a moment after the code exists; until then the code is not
// found, so a miss is retried briefly.
func joinByCodeForTest(t *testing.T, s *Server, code string) *httpResult {
	t.Helper()
	return joinByCodeTries(t, s, code, 10)
}

func joinByCodeTries(t *testing.T, s *Server, code string, retries int) *httpResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": mobilelink.FormatDeviceCode(code), "name": "家里的 Mac mini"})
	for try := 0; ; try++ {
		w := deviceRequest(s, "POST", "/api/client/devices/join", string(body), deviceCall{})
		if w.Code == http.StatusConflict && strings.Contains(w.Body.String(), "没有找到这个配对码") && try < retries {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var view deviceJoinView
		_ = json.Unmarshal(w.Body.Bytes(), &view)
		return &httpResult{code: w.Code, body: w.Body.String(), view: view}
	}
}

type httpResult struct {
	code int
	body string
	view deviceJoinView
}

func joinStateForTest(t *testing.T, s *Server, wait string) deviceJoinView {
	t.Helper()
	w := deviceRequest(s, "GET", "/api/client/devices/join?wait="+wait, "", deviceCall{})
	var view deviceJoinView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("join state: HTTP %d %s", w.Code, w.Body.String())
	}
	return view
}

// joinStateAfter waits until the join leaves state from. Like the dialog it
// reads at once, then holds a read only while the state is still from.
func joinStateAfter(t *testing.T, s *Server, from string) deviceJoinView {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if view := joinStateForTest(t, s, "0"); view.State != from {
			return view
		}
		joinStateForTest(t, s, "5")
	}
	t.Fatalf("the join stayed in %s", from)
	return deviceJoinView{}
}

func shortCodeJoinerForTest(t *testing.T, relayOrigin string) *Server {
	t.Helper()
	s := testServer(t)
	s.remote = newRemoteGateway(s.profile)
	t.Cleanup(s.remote.close)
	t.Cleanup(s.devices.shutdown)
	s.devices.restAfter = time.Hour
	s.preferences.MobileRelay = relayOrigin
	return s
}

// One process, two computers and a loopback relay: the host shows an
// 8-digit code, the joining computer types it, both windows show the same
// check number, the host's person clicks 允许, and the joined computer opens
// the host's phone surface.
func TestJoinedComputerByShortCode(t *testing.T) {
	relayOrigin, _ := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)
	s := shortCodeJoinerForTest(t, relayOrigin)
	if view := joinStateForTest(t, s, "0"); view.State != "none" {
		t.Fatalf("state before a join: %+v", view)
	}
	code := shortCodeForTest(t, host)
	joined := joinByCodeForTest(t, s, code)
	if joined.code != 200 || joined.view.State != "approval" || len(joined.view.Check) != 4 {
		t.Fatalf("join by code: HTTP %d %s", joined.code, joined.body)
	}
	request := hostRequestForTest(t, host)
	if request.State != "request" || request.Request == nil || request.Request.Check != joined.view.Check || request.Request.Platform != runtime.GOOS || request.Request.Name == "" {
		t.Fatalf("host shows %+v, joiner shows %s", request, joined.view.Check)
	}
	if w := deviceRequest(host, "POST", "/api/devices/invite/approve", `{"check":"`+request.Request.Check+`"}`, deviceCall{}); w.Code != 200 {
		t.Fatalf("允许: HTTP %d %s", w.Code, w.Body.String())
	}
	// 允许 saves nothing on either side: this window confirms the number too.
	asked := joinStateAfter(t, s, "approval")
	if asked.State != "confirm" || asked.Check != joined.view.Check {
		t.Fatalf("after 允许: %+v", asked)
	}
	if computers, _ := s.loadJoinedComputers(); len(computers) != 0 || hostDeviceCount(host) != 0 {
		t.Fatalf("saved before the joiner confirmed: %d here, %d on the host", len(computers), hostDeviceCount(host))
	}
	if hostView := hostInviteForTest(t, host, "0"); hostView.State != "confirming" {
		t.Fatalf("host while the joiner confirms: %+v", hostView)
	}
	w := deviceRequest(s, "POST", "/api/client/devices/join/confirm", `{"check":"`+asked.Check+`"}`, deviceCall{})
	var view deviceJoinView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.State != "joined" || !joinedComputerID.MatchString(view.ID) || view.Name != "家里的 Mac mini" {
		t.Fatalf("一致: HTTP %d %s", w.Code, w.Body.String())
	}
	if hostView := hostInviteForTest(t, host, "5"); hostView.State != "joined" || hostView.Device == nil {
		t.Fatalf("host after the joiner confirmed: %+v", hostView)
	}
	record := readPrivateJoined(t, s)
	if len(record.Computers) != 1 || record.Computers[0].ID != view.ID {
		t.Fatalf("saved: %+v", record)
	}
	key, err := remotelink.IdentityFromKey(ed25519.NewKeyFromSeed(record.Computers[0].Seed))
	if err != nil {
		t.Fatal(err)
	}
	paired := onlyDevice(t, host)
	if paired.Pin != key.Pin || paired.Kind != "computer" || paired.Platform != runtime.GOOS {
		t.Fatalf("host enrolled %+v", paired.deviceRecord)
	}
	connection := openComputerForTest(t, s, view.ID)
	sessionsThroughLink(t, s, connection)
}

func TestJoinByShortCodeRefusalsAndCancel(t *testing.T) {
	relayOrigin, joinerHits := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)
	s := shortCodeJoinerForTest(t, relayOrigin)

	// Window only, like every joining-side route.
	for _, call := range []deviceCall{{agent: true}, {untrusted: true}} {
		for _, request := range [][3]string{{"POST", "/api/client/devices/join", `{"code":"4821 9037"}`}, {"GET", "/api/client/devices/join", ""}, {"DELETE", "/api/client/devices/join", ""}, {"POST", "/api/client/devices/join/confirm", `{"check":"1234"}`}, {"POST", "/api/client/devices/join/reject", "{}"}} {
			if w := deviceRequest(s, request[0], request[1], request[2], call); w.Code != http.StatusForbidden {
				t.Fatalf("%s %s %+v: HTTP %d", request[0], request[1], call, w.Code)
			}
		}
	}
	if joinerHits.Load() != 0 {
		t.Fatal("refused joins reached the relay")
	}
	if w := deviceRequest(s, "POST", "/api/client/devices/join/confirm", `{"check":"1234"}`, deviceCall{}); w.Code != http.StatusConflict {
		t.Fatalf("一致 with no join: HTTP %d", w.Code)
	}

	// 拒绝 on the host.
	code := shortCodeForTest(t, host)
	if joined := joinByCodeForTest(t, s, code); joined.code != 200 || joined.view.State != "approval" {
		t.Fatalf("join: HTTP %d %s", joined.code, joined.body)
	}
	hostRequestForTest(t, host)
	if w := deviceRequest(host, "POST", "/api/devices/invite/reject", "{}", deviceCall{}); w.Code != 200 {
		t.Fatalf("拒绝: HTTP %d %s", w.Code, w.Body.String())
	}
	if view := joinStateForTest(t, s, "10"); view.State != "failed" || view.Error != mobilelink.ErrDeviceRejected.Error() {
		t.Fatalf("after 拒绝: %+v", view)
	}
	if computers, err := s.loadJoinedComputers(); err != nil || len(computers) != 0 {
		t.Fatalf("a refused join was saved: %v %v", computers, err)
	}

	// A code no host shows.
	code = shortCodeForTest(t, host)
	wrong := code[:7] + string(rune('0'+(code[7]-'0'+1)%10))
	if missed := joinByCodeTries(t, s, wrong, 0); missed.code != http.StatusConflict || !strings.Contains(missed.body, "没有找到这个配对码") {
		t.Fatalf("wrong code: HTTP %d %s", missed.code, missed.body)
	}

	// The window closes the dialog, or this computer locks, while the host
	// waits for its person: the join stops, the host's request ends (its
	// person could click 允许 only after that), and nothing is saved.
	for _, stop := range []string{"dialog", "lock"} {
		code := shortCodeForTest(t, host)
		if joined := joinByCodeForTest(t, s, code); joined.view.State != "approval" {
			t.Fatalf("%s: join: HTTP %d %s", stop, joined.code, joined.body)
		}
		request := hostRequestForTest(t, host)
		waiting := make(chan deviceJoinView, 1)
		go func() { waiting <- joinStateForTest(t, s, "10") }()
		time.Sleep(50 * time.Millisecond)
		if stop == "dialog" {
			if w := deviceRequest(s, "DELETE", "/api/client/devices/join", "", deviceCall{}); w.Code != 200 {
				t.Fatalf("cancel: HTTP %d", w.Code)
			}
		} else {
			s.devices.closeAll()
		}
		select {
		case view := <-waiting:
			if view.State != "none" {
				t.Fatalf("%s: held read ended with %+v", stop, view)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not end the held read", stop)
		}
		if view := hostInviteForTest(t, host, "5"); view.State != "failed" || view.Error != mobilelink.ErrDeviceJoinerLeft.Error() {
			t.Fatalf("%s: host still asks: %+v", stop, view)
		}
		if w := deviceRequest(host, "POST", "/api/devices/invite/approve", `{"check":"`+request.Request.Check+`"}`, deviceCall{}); w.Code != http.StatusConflict {
			t.Fatalf("%s: a late 允许: HTTP %d", stop, w.Code)
		}
		if computers, _ := s.loadJoinedComputers(); len(computers) != 0 {
			t.Fatalf("%s: a cancelled join was saved", stop)
		}
	}
	if count := hostDeviceCount(host); count != 0 {
		t.Fatalf("host enrolled %d refused or cancelled joiners", count)
	}
}

func hostDeviceCount(host *Server) int {
	host.phones.mu.Lock()
	defer host.phones.mu.Unlock()
	return len(host.phones.devices)
}

// The joining window confirms the check number before anything is saved. A
// host that knows the code and approves at once (a relay that read the code
// can do that) gets nothing without this person's click on the number the
// real host never showed.
func TestJoinByShortCodeNeedsTheJoinersConfirmation(t *testing.T) {
	saved := mobilelink.DeviceApprovalTimeout
	mobilelink.DeviceApprovalTimeout = 3 * time.Second
	t.Cleanup(func() { mobilelink.DeviceApprovalTimeout = saved })
	relayOrigin, _ := deviceTestRelay(t)
	s := shortCodeJoinerForTest(t, relayOrigin)

	t.Run("forged approval", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		forger, err := remotelink.NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		invite, err := mobilelink.NewDeviceInvite(ctx, relayOrigin, "", forger)
		if err != nil {
			t.Fatal(err)
		}
		var planted atomic.Int32
		outcome := make(chan error, 1)
		go func() {
			outcome <- mobilelink.AcceptDevice(ctx, invite, forger, func(context.Context, mobilelink.DeviceRequest) bool { return true }, func(mobilelink.DeviceRequest, mobilelink.PairedHost) error {
				planted.Add(1)
				return nil
			})
		}()
		joined := joinByCodeForTest(t, s, invite.Code)
		if joined.code != 200 || len(joined.view.Check) != 4 {
			t.Fatalf("join: HTTP %d %s", joined.code, joined.body)
		}
		view := joinStateAfter(t, s, "approval")
		if view.State != "confirm" || view.Check != joined.view.Check {
			t.Fatalf("after the forged 允许: %+v", view)
		}
		wrong := view.Check[:3] + string(rune('0'+(view.Check[3]-'0'+1)%10))
		if w := deviceRequest(s, "POST", "/api/client/devices/join/confirm", `{"check":"`+wrong+`"}`, deviceCall{}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "核对数字不一致") {
			t.Fatalf("一致 with another number: HTTP %d %s", w.Code, w.Body.String())
		}
		if view := joinStateForTest(t, s, "0"); view.State != "confirm" {
			t.Fatalf("a wrong number ended the question: %+v", view)
		}
		// No click: the question times out and nothing is saved anywhere.
		if view = joinStateAfter(t, s, "confirm"); view.State != "failed" || !strings.Contains(view.Error, "没有及时确认") {
			t.Fatalf("no click: %+v", view)
		}
		select {
		case err := <-outcome:
			if !errors.Is(err, mobilelink.ErrDeviceNotConfirmed) {
				t.Fatalf("forger: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the forger still waits")
		}
		if computers, _ := s.loadJoinedComputers(); len(computers) != 0 || planted.Load() != 0 {
			t.Fatalf("saved without a click: %d here, %d planted", len(computers), planted.Load())
		}
	})

	t.Run("不一致", func(t *testing.T) {
		host := deviceTestHost(t, relayOrigin)
		joined := joinByCodeForTest(t, s, shortCodeForTest(t, host))
		if joined.code != 200 || joined.view.State != "approval" {
			t.Fatalf("join: HTTP %d %s", joined.code, joined.body)
		}
		request := hostRequestForTest(t, host)
		if w := deviceRequest(host, "POST", "/api/devices/invite/approve", `{"check":"`+request.Request.Check+`"}`, deviceCall{}); w.Code != 200 {
			t.Fatalf("允许: HTTP %d %s", w.Code, w.Body.String())
		}
		if view := joinStateAfter(t, s, "approval"); view.State != "confirm" {
			t.Fatalf("after 允许: %+v", view)
		}
		w := deviceRequest(s, "POST", "/api/client/devices/join/reject", "{}", deviceCall{})
		var view deviceJoinView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.State != "failed" || !strings.Contains(view.Error, "核对数字不一致") {
			t.Fatalf("不一致: HTTP %d %s", w.Code, w.Body.String())
		}
		if hostView := hostInviteForTest(t, host, "5"); hostView.State != "failed" || !strings.Contains(hostView.Error, "没有确认核对数字") {
			t.Fatalf("host after 不一致: %+v", hostView)
		}
		if computers, _ := s.loadJoinedComputers(); len(computers) != 0 || hostDeviceCount(host) != 0 {
			t.Fatalf("saved after 不一致: %d here, %d on the host", len(computers), hostDeviceCount(host))
		}
	})
}
