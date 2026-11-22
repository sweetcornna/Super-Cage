package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

// shortCodeHostForTest shows an 8-digit code on a host of today's build the
// way the host side will (one room for the QR and the code, 允许 after the
// check number) and pairs the joining computer as that host's phone. decide
// plays the person at the host.
func shortCodeHostForTest(t *testing.T, host *Server, relayOrigin string, decide func(context.Context, mobilelink.DeviceRequest) bool) (string, <-chan error) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := remotelink.IdentityFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	invite, err := mobilelink.NewDeviceInvite(ctx, relayOrigin, "", identity)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- mobilelink.AcceptDevice(ctx, invite, identity, decide, func(request mobilelink.DeviceRequest) (mobilelink.PairedHost, error) {
			var token [32]byte
			if _, err := rand.Read(token[:]); err != nil {
				return mobilelink.PairedHost{}, err
			}
			record := phoneRecord{Seed: key.Seed(), Relay: relayOrigin, Pin: request.Pin, Lookup: base64.RawURLEncoding.EncodeToString(token[:])}
			host.phones.mu.Lock()
			defer host.phones.mu.Unlock()
			if err := writePrivate(host.phoneFile(), record); err != nil {
				return mobilelink.PairedHost{}, err
			}
			host.attachPhone(record)
			return mobilelink.PairedHost{Version: 1, Relay: relayOrigin, HostPin: identity.Pin, Lookup: record.Lookup}, nil
		})
	}()
	return invite.Code, done
}

// joinByCodeForTest posts the code as a person types it. The host's socket
// joins its room a moment after the code exists; until then the code is not
// found, so a miss is retried briefly.
func joinByCodeForTest(t *testing.T, s *Server, code string) *httpResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": mobilelink.FormatDeviceCode(code), "name": "家里的 Mac mini"})
	for try := 0; ; try++ {
		w := deviceRequest(s, "POST", "/api/client/devices/join", string(body), deviceCall{})
		if w.Code == http.StatusConflict && strings.Contains(w.Body.String(), "没有找到这个配对码") && try < 10 {
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

// A computer joins with the 8-digit code: its window gets the check number,
// the host's person compares it and clicks 允许, and the joined computer opens
// the host's phone surface. Host, joiner and relay in one process.
func TestJoinedComputerByShortCode(t *testing.T) {
	relayOrigin, _ := deviceTestRelay(t)
	host := deviceTestHost(t, relayOrigin)
	s := shortCodeJoinerForTest(t, relayOrigin)
	shown := make(chan string, 1)
	var asked mobilelink.DeviceRequest
	code, hostDone := shortCodeHostForTest(t, host, relayOrigin, func(ctx context.Context, request mobilelink.DeviceRequest) bool {
		asked = request
		select {
		case check := <-shown:
			return check == request.Check
		case <-ctx.Done():
			return false
		}
	})
	if view := joinStateForTest(t, s, "0"); view.State != "none" {
		t.Fatalf("state before a join: %+v", view)
	}
	joined := joinByCodeForTest(t, s, code)
	if joined.code != 200 || joined.view.State != "approval" || len(joined.view.Check) != 4 {
		t.Fatalf("join by code: HTTP %d %s", joined.code, joined.body)
	}
	shown <- joined.view.Check
	view := joinStateForTest(t, s, "10")
	if view.State != "joined" || !joinedComputerID.MatchString(view.ID) || view.Name != "家里的 Mac mini" {
		t.Fatalf("after 允许: %+v", view)
	}
	select {
	case err := <-hostDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host did not finish")
	}
	record := readPrivateJoined(t, s)
	if len(record.Computers) != 1 || record.Computers[0].ID != view.ID {
		t.Fatalf("saved: %+v", record)
	}
	key, err := remotelink.IdentityFromKey(ed25519.NewKeyFromSeed(record.Computers[0].Seed))
	if err != nil {
		t.Fatal(err)
	}
	host.phones.mu.Lock()
	hostRecord := host.phones.record
	host.phones.mu.Unlock()
	if hostRecord == nil || hostRecord.Pin != key.Pin || asked.Pin != key.Pin || asked.Platform != runtime.GOOS || asked.Name == "" {
		t.Fatalf("host enrolled %+v after asking %+v", hostRecord, asked)
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
		for _, request := range [][3]string{{"POST", "/api/client/devices/join", `{"code":"4821 9037"}`}, {"GET", "/api/client/devices/join", ""}, {"DELETE", "/api/client/devices/join", ""}} {
			if w := deviceRequest(s, request[0], request[1], request[2], call); w.Code != http.StatusForbidden {
				t.Fatalf("%s %s %+v: HTTP %d", request[0], request[1], call, w.Code)
			}
		}
	}
	if joinerHits.Load() != 0 {
		t.Fatal("refused joins reached the relay")
	}

	// 拒绝 on the host.
	code, hostDone := shortCodeHostForTest(t, host, relayOrigin, func(context.Context, mobilelink.DeviceRequest) bool { return false })
	joined := joinByCodeForTest(t, s, code)
	if joined.code != 200 || joined.view.State != "approval" {
		t.Fatalf("join: HTTP %d %s", joined.code, joined.body)
	}
	if view := joinStateForTest(t, s, "10"); view.State != "failed" || view.Error != mobilelink.ErrDeviceRejected.Error() {
		t.Fatalf("after 拒绝: %+v", view)
	}
	if err := <-hostDone; !errors.Is(err, mobilelink.ErrDeviceRejected) {
		t.Fatalf("host: %v", err)
	}
	if computers, err := s.loadJoinedComputers(); err != nil || len(computers) != 0 {
		t.Fatalf("a refused join was saved: %v %v", computers, err)
	}

	// A code no host shows.
	wrong := "0000000" + string(rune('0'+(code[7]-'0'+1)%10))
	if missed := joinByCodeForTest(t, s, wrong); missed.code != http.StatusConflict || !strings.Contains(missed.body, "没有找到这个配对码") {
		t.Fatalf("wrong code: HTTP %d %s", missed.code, missed.body)
	}

	// The window closes the dialog, or this computer locks, while the host
	// waits for its person: the join stops, the host's prompt ends (its person
	// would click 允许 only after that), and nothing is saved on either side.
	for _, stop := range []string{"dialog", "lock"} {
		code, hostDone = shortCodeHostForTest(t, host, relayOrigin, func(ctx context.Context, _ mobilelink.DeviceRequest) bool {
			<-ctx.Done()
			return true
		})
		if joined := joinByCodeForTest(t, s, code); joined.view.State != "approval" {
			t.Fatalf("%s: join: HTTP %d %s", stop, joined.code, joined.body)
		}
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
		select {
		case err := <-hostDone:
			if !errors.Is(err, mobilelink.ErrDeviceJoinerLeft) {
				t.Fatalf("%s: host: %v", stop, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the host's prompt outlived the join", stop)
		}
		if computers, _ := s.loadJoinedComputers(); len(computers) != 0 {
			t.Fatalf("%s: a cancelled join was saved", stop)
		}
	}
}
