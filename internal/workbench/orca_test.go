package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/localorca"
)

func TestOrcaConfigLifecycleAutoPairAndSecurityBoundaries(t *testing.T) {
	s := testServer(t)
	s.ready = true
	defer s.closeOrcaTunnel()

	// 1. Disabled by default: zero guest SSH calls on GET /api/orca!
	var runtimeCalls atomic.Int32
	s.agentCLIs.runtimeHook = func(_ context.Context, subcmd string, input any, output any) error {
		runtimeCalls.Add(1)
		switch subcmd {
		case "orca-status":
			return json.Unmarshal([]byte(`{"installed":true,"version":"1.4.277","running":false,"port":16768,"bind_ip":"127.0.0.1","bind_mode":"loopback"}`), output)
		case "orca-start", "orca-restart":
			return json.Unmarshal([]byte(`{"installed":true,"version":"1.4.277","running":true,"pid":4242,"port":16768,"bind_ip":"127.0.0.1","bind_mode":"loopback"}`), output)
		case "orca-stop":
			return json.Unmarshal([]byte(`{"running":false}`), output)
		case "orca-pair":
			return json.Unmarshal([]byte(`{"available":true,"scope":"desktop","pairing_url":"orca://pair?code=testtoken123&endpoint=ws%3A%2F%2F127.0.0.1%3A16769","endpoint":"ws://127.0.0.1:16769","device_id":"dev-1"}`), output)
		case "orca-sync-repos":
			return json.Unmarshal([]byte(`{"added":["/workspace/demo"],"existing":0}`), output)
		}
		return nil
	}
	s.orca.localDetect = func() localorca.Installation {
		return localorca.Installation{Installed: true, AppPath: "/Applications/Orca.app", CLIPath: "/fake/orca", Version: "1.4.277"}
	}

	var addedName, addedCode string
	var openedDesktop atomic.Int32
	var pairedEnvPresent atomic.Bool
	mockClient := localorca.NewClient(s.orca.localDetect())
	mockClient.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "environment" && args[1] == "list":
			if pairedEnvPresent.Load() {
				return []byte(`{"id":"local","ok":true,"result":{"environments":[{"id":"env-existing","name":"lab1-cage","endpoints":[{"kind":"websocket","endpoint":"ws://127.0.0.1:16768"}]},{"id":"env-cage","name":"Super Cage","endpoints":[{"kind":"websocket","endpoint":"ws://127.0.0.1:16769"}]}]}}`), nil
			}
			// Simulate user already having an existing environment on 16768 so Cage MUST skip 16768!
			return []byte(`{"id":"local","ok":true,"result":{"environments":[{"id":"env-existing","name":"lab1-cage","endpoints":[{"kind":"websocket","endpoint":"ws://127.0.0.1:16768"}]}]}}`), nil
		case len(args) >= 2 && args[0] == "environment" && args[1] == "add":
			for i := 0; i < len(args)-1; i++ {
				if args[i] == "--name" {
					addedName = args[i+1]
				}
				if args[i] == "--pairing-code" {
					addedCode = args[i+1]
				}
			}
			pairedEnvPresent.Store(true)
			return []byte(`{"id":"local","ok":true,"result":{"environment":{"id":"env-cage","name":"Super Cage"}}}`), nil
		case len(args) >= 1 && args[0] == "open":
			openedDesktop.Add(1)
			return []byte(`{"id":"local","ok":true,"result":{"opened":true}}`), nil
		}
		return []byte(`{"ok":true}`), nil
	}
	s.orca.localClient = mockClient

	w := request(s, "GET", "/api/orca", "", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/orca: %d %s", w.Code, w.Body.String())
	}
	if runtimeCalls.Load() != 0 {
		t.Fatalf("disabled GET /api/orca unexpectedly called guest runtime %d times", runtimeCalls.Load())
	}
	var st map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["enabled"] != false || st["can_auto_pair"] != true {
		t.Fatalf("unexpected default orca status: %+v", st)
	}

	// 2. Reject invalid bind_mode, port, memory limit, and server name.
	for _, bad := range []string{
		`{"bind_mode":"0.0.0.0"}`,
		`{"guest_port":80}`,
		`{"terminal_memory_max":"abc"}`,
		`{"server_name":"--inject"}`,
	} {
		if bw := request(s, "POST", "/api/orca/config", s.origin, bad, true); bw.Code != http.StatusBadRequest {
			t.Fatalf("invalid config %s accepted: %d %s", bad, bw.Code, bw.Body.String())
		}
	}

	// 3. Enabling auto-allocates host_tunnel_port skipping already-registered 16768!
	w = request(s, "POST", "/api/orca/config", s.origin, `{"enabled":true,"server_name":"My Cage Orca","terminal_memory_max":"2G"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("valid orca config rejected: %d %s", w.Code, w.Body.String())
	}
	var enabledSt map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &enabledSt)
	allocatedPort := int(enabledSt["host_tunnel_port"].(float64))
	if allocatedPort == 16768 || allocatedPort < 16768 || allocatedPort > 16868 {
		t.Fatalf("expected auto-allocated port to skip registered 16768 and land in [16769..16868], got %d", allocatedPort)
	}

	// 4. Remote client MUST be refused on POST /api/orca/open (owner-only local app launch).
	remoteReq := httptest.NewRequest("POST", s.origin+"/api/orca/open", strings.NewReader(`{}`))
	remoteReq.Header.Set("Origin", s.origin)
	remoteReq.Header.Set("Content-Type", "application/json")
	remoteReq.Header.Set("X-Cage-Remote-Client", "1")
	remoteReq.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	rw := httptest.NewRecorder()
	s.handler().ServeHTTP(rw, remoteReq)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("remote client allowed to call /api/orca/open: %d %s", rw.Code, rw.Body.String())
	}

	// 5. Mock guest echo server behind tunnelDialHook to prove end-to-end auto-pair + live TCP relay + instant lock revocation!
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = conn.Write([]byte("orca:" + line))
				}
			}(c)
		}
	}()
	s.orca.tunnelDialHook = func(_ context.Context, _ int) (string, error) {
		return echoLn.Addr().String(), nil
	}

	// Update host_tunnel_port to 16769 (or use allocatedPort) so mock list matches.
	w = request(s, "POST", "/api/orca/open", s.origin, `{}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/orca/open failed: %d %s", w.Code, w.Body.String())
	}
	var openRes map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &openRes)
	if openRes["opened"] != true || openRes["newly_paired"] != true || addedName != "My Cage Orca" || !strings.HasPrefix(addedCode, "orca://pair?") {
		t.Fatalf("unexpected open/pair result: %+v name=%q code=%q", openRes, addedName, addedCode)
	}
	if openedDesktop.Load() != 1 {
		t.Fatalf("expected OpenDesktop called once, got %d", openedDesktop.Load())
	}

	// 6. Verify live TCP relay over the persistent host loopback tunnel!
	tunnelPort := int(openRes["tunnel_port"].(float64))
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strings.TrimPrefix(openRes["endpoint"].(string), "ws://127.0.0.1:")))
	if err != nil {
		t.Fatalf("dial orca tunnel port %d failed: %v", tunnelPort, err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hello\n"))
	br := bufio.NewReader(conn)
	reply, err := br.ReadString('\n')
	if err != nil || reply != "orca:hello\n" {
		t.Fatalf("tunnel relay mismatch: %q %v", reply, err)
	}

	// 7. Locking Cage MUST immediately sever active tunnel connections AND block /api/orca with 423!
	s.account.lock()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, readErr := br.ReadByte()
	if readErr == nil {
		t.Fatal("active Orca tunnel connection survived Cage lock!")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("Orca tunnel timed out instead of closing immediately on Cage lock: %v", readErr)
	}
	if lw := request(s, "GET", "/api/orca", "", "", true); lw.Code != http.StatusLocked {
		t.Fatalf("locked Cage exposed /api/orca: %d", lw.Code)
	}
}
