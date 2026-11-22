package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

// The window of the installed app holds only what its process gives it: a
// launch ticket redeemed at POST /api/session (ui/src/api.js) for the session
// cookie. The trusted-view cookie stays in the launcher's own jar
// (trusted_login.go), and the window sends no X-Cage-Agent. These tests join
// and open another computer as that window does, in the two processes that
// answer it: the desktop gateway and the client-only process. Every other
// joining test calls deviceRequest, which sets the trusted-view cookie by
// hand.
type realWindow struct {
	s   *Server
	jar *cookiejar.Jar
}

// openRealWindow redeems a launch ticket and keeps only the cookies the
// answer sets.
func openRealWindow(t *testing.T, s *Server) *realWindow {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	window := &realWindow{s: s, jar: jar}
	if w := window.call("POST", "/api/session", `{"token":"`+s.issueTicket()+`"}`, nil); w.Code != 200 {
		t.Fatalf("session: HTTP %d %s", w.Code, w.Body.String())
	}
	origin, _ := url.Parse(s.origin)
	if cookies := jar.Cookies(origin); len(cookies) != 1 || cookies[0].Name != s.cookieName() {
		t.Fatalf("the window holds %v", cookies)
	}
	return window
}

// call sends what the webview's fetch sends: the jar's cookies, Origin on all
// but GET, a JSON type only with a body, and the given headers.
func (window *realWindow) call(method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	s := window.s
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	if method != "GET" {
		r.Header.Set("Origin", s.origin)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	at, _ := url.Parse(s.origin + path)
	for _, c := range window.jar.Cookies(at) {
		r.AddCookie(c)
	}
	for name, value := range header {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	window.jar.SetCookies(at, w.Result().Cookies())
	return w
}

func (window *realWindow) joinState(t *testing.T, wait string) deviceJoinView {
	t.Helper()
	w := window.call("GET", "/api/client/devices/join?wait="+wait, "", nil)
	var view deviceJoinView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("join state: HTTP %d %s", w.Code, w.Body.String())
	}
	return view
}

// joinToConfirm types host's short code, has the host's person click 允许
// and holds the read until this window is asked to confirm the number.
func (window *realWindow) joinToConfirm(t *testing.T, host *Server) deviceJoinView {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": mobilelink.FormatDeviceCode(shortCodeForTest(t, host)), "name": "家里的 Mac mini"})
	var view deviceJoinView
	for try := 0; ; try++ {
		w := window.call("POST", "/api/client/devices/join", string(body), nil)
		if w.Code == http.StatusConflict && strings.Contains(w.Body.String(), "没有找到这个配对码") && try < 10 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.State != "approval" {
			t.Fatalf("join: HTTP %d %s", w.Code, w.Body.String())
		}
		break
	}
	request := hostRequestForTest(t, host)
	if w := deviceRequest(host, "POST", "/api/devices/invite/approve", `{"check":"`+request.Request.Check+`"}`, deviceCall{}); w.Code != 200 {
		t.Fatalf("允许: HTTP %d %s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if asked := window.joinState(t, "5"); asked.State != "approval" {
			if asked.State != "confirm" || asked.Check != view.Check {
				t.Fatalf("after 允许: %+v", asked)
			}
			return asked
		}
	}
	t.Fatal("the join stayed in approval")
	return deviceJoinView{}
}

func realWindowGateway(t *testing.T, relayOrigin string) *Server {
	gateway := testServer(t)
	gateway.account = nil
	gateway.desktopEntry = true
	joinerProfile(t, gateway, relayOrigin)
	gateway.remote = newDesktopGateway(gateway.profile, newDeviceLocalFake(t))
	t.Cleanup(gateway.remote.close)
	t.Cleanup(gateway.devices.shutdown)
	chooseLocal(t, gateway.remote.desktop, gateway.remote.desktop.generation)
	return gateway
}

func realWindowClientOnly(t *testing.T, relayOrigin string) *Server {
	client := testServer(t)
	client.account = nil
	client.clientOnly = true
	joinerProfile(t, client, relayOrigin)
	client.remote = newRemoteGateway(client.profile)
	client.remote.clientOnly = true
	t.Cleanup(client.remote.close)
	t.Cleanup(client.devices.shutdown)
	return client
}

func TestRealWindowJoinsAndOpensAnotherComputer(t *testing.T) {
	relayOrigin, _ := deviceTestRelay(t)
	for name, process := range map[string]func(*testing.T, string) *Server{"desktop gateway": realWindowGateway, "client-only": realWindowClientOnly} {
		t.Run(name, func(t *testing.T) {
			s := process(t, relayOrigin)
			window := openRealWindow(t, s)

			// Every route passes the window gate (whatever it then answers
			// for an unknown computer or no join).
			unknown := strings.Repeat("0", 32)
			for _, request := range [][4]string{
				{"GET", "/api/client/devices", ""},
				{"DELETE", "/api/client/devices", ""},
				{"POST", "/api/client/devices/join", `{"code":"x"}`},
				{"GET", "/api/client/devices/join", ""},
				{"DELETE", "/api/client/devices/join", ""},
				{"POST", "/api/client/devices/join/confirm", `{"check":"1234"}`},
				{"POST", "/api/client/devices/join/reject", "{}"},
				{"POST", "/api/client/devices/" + unknown + "/open", "{}"},
				{"POST", "/api/client/devices/" + unknown + "/close", ""},
				{"DELETE", "/api/client/devices/" + unknown, ""},
				{"GET", "/api/mobile/cli/sessions", "", deviceConnectionPrefix + unknown + ":1"},
			} {
				var header map[string]string
				if request[3] != "" {
					header = map[string]string{connectionHeader: request[3]}
				}
				if w := window.call(request[0], request[1], request[2], header); w.Code == http.StatusForbidden {
					t.Errorf("%s %s: HTTP %d %s", request[0], request[1], w.Code, strings.TrimSpace(w.Body.String()))
				}
			}
			if t.Failed() {
				t.FailNow()
			}

			ok := func(what string, w *httptest.ResponseRecorder) {
				t.Helper()
				if w.Code != 200 {
					t.Fatalf("%s: HTTP %d %s", what, w.Code, w.Body.String())
				}
			}
			ok("list", window.call("GET", "/api/client/devices", "", nil))

			// 不一致 ends a join; 一致 saves it.
			window.joinToConfirm(t, deviceTestHost(t, relayOrigin))
			w := window.call("POST", "/api/client/devices/join/reject", "{}", nil)
			var view deviceJoinView
			if ok("不一致", w); json.Unmarshal(w.Body.Bytes(), &view) != nil || view.State != "failed" {
				t.Fatalf("不一致: %s", w.Body.String())
			}
			asked := window.joinToConfirm(t, deviceTestHost(t, relayOrigin))
			w = window.call("POST", "/api/client/devices/join/confirm", `{"check":"`+asked.Check+`"}`, nil)
			if ok("一致", w); json.Unmarshal(w.Body.Bytes(), &view) != nil || view.State != "joined" || !joinedComputerID.MatchString(view.ID) {
				t.Fatalf("一致: %s", w.Body.String())
			}
			id := view.ID
			if w = window.call("GET", "/api/client/devices", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), id) {
				t.Fatalf("list after the join: HTTP %d %s", w.Code, w.Body.String())
			}

			// 打开, then the phone surface on the device: connection as
			// transport.mjs sends it.
			w = window.call("POST", "/api/client/devices/"+id+"/open", "{}", nil)
			var opened struct{ Connection string }
			if ok("open", w); json.Unmarshal(w.Body.Bytes(), &opened) != nil || !strings.HasPrefix(opened.Connection, deviceConnectionPrefix+id+":") {
				t.Fatalf("open: %s", w.Body.String())
			}
			onLink := map[string]string{connectionHeader: opened.Connection}
			if w = window.call("GET", "/api/mobile/cli/sessions", "", onLink); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("sessions through the link: HTTP %d %s", w.Code, w.Body.String())
			}

			// An agent is refused on every one of these routes, and this
			// window's session is still not the launcher.
			agent := map[string]string{agentRequestHeader: "1"}
			onLinkAgent := map[string]string{agentRequestHeader: "1", connectionHeader: opened.Connection}
			for _, request := range []struct {
				method, path, body string
				header             map[string]string
			}{
				{"GET", "/api/client/devices", "", agent},
				{"DELETE", "/api/client/devices", "", agent},
				{"POST", "/api/client/devices/join", `{"code":"4821 9037"}`, agent},
				{"GET", "/api/client/devices/join", "", agent},
				{"DELETE", "/api/client/devices/join", "", agent},
				{"POST", "/api/client/devices/join/confirm", `{"check":"1234"}`, agent},
				{"POST", "/api/client/devices/join/reject", "{}", agent},
				{"POST", "/api/client/devices/" + id + "/open", "{}", agent},
				{"POST", "/api/client/devices/" + id + "/close", "", agent},
				{"DELETE", "/api/client/devices/" + id, "", agent},
				{"GET", "/api/mobile/cli/sessions", "", onLinkAgent},
			} {
				if w := window.call(request.method, request.path, request.body, request.header); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), errDeviceWindowOnly.Error()) {
					t.Fatalf("agent %s %s: HTTP %d %s", request.method, request.path, w.Code, w.Body.String())
				}
			}
			if w = window.call("POST", "/api/agent/native", `{"request":{}}`, nil); w.Code != http.StatusForbidden {
				t.Fatalf("native submission from the window: HTTP %d %s", w.Code, w.Body.String())
			}

			// 返回本机 (device-bar.mjs): a bare keepalive POST, no body, no
			// connection.
			ok("close", window.call("POST", "/api/client/devices/"+id+"/close", "", nil))
			if w = window.call("GET", "/api/mobile/cli/sessions", "", onLink); w.Code != http.StatusConflict {
				t.Fatalf("the link after 返回本机: HTTP %d %s", w.Code, w.Body.String())
			}
			ok("remove", window.call("DELETE", "/api/client/devices/"+id, "", nil))
			if w = window.call("GET", "/api/client/devices", "", nil); w.Code != 200 || strings.Contains(w.Body.String(), id) {
				t.Fatalf("list after the removal: HTTP %d %s", w.Code, w.Body.String())
			}

			// 清除并重新加入 for a list that cannot be read.
			if err := os.WriteFile(s.joinedComputersPath(), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			if w = window.call("GET", "/api/client/devices", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), errJoinedUnreadable.Error()) {
				t.Fatalf("unreadable list: HTTP %d %s", w.Code, w.Body.String())
			}
			ok("clear", window.call("DELETE", "/api/client/devices", "", nil))
		})
	}
}
