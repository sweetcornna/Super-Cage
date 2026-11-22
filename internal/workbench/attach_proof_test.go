package workbench

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/controlwire"
)

// cage-attach's client (controlwire, without net/http) against the real
// controller: it proves the controller on every connection before the
// launcher token, the ticket or a cookie, and dials 127.0.0.1 itself, so a
// squatter on the [::1] twin of the port gets nothing.
func TestSlimClientProvesTheController(t *testing.T) {
	s := testServer(t)
	s.proofKey = randomToken()
	listener := ephemeralTwinPort(t)
	port := listener.Addr().(*net.TCPAddr).Port
	recording := startTestController(t, s, listener, s.controlRecord)
	q := newSquatter(t, port)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	c, err := controlwire.Connect(s.profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlclient.Session(ctx, c); err != nil {
		t.Fatal(err)
	}
	if status, _, err := c.Do(ctx, "GET", "/api/settings", nil, false, 0); err != nil || status != 200 {
		t.Fatal(status, err)
	}
	if ws, err := c.Dial(ctx, controlclient.AgentTarget{Scope: "local"}, "/api/terminals/none/stream"); err == nil {
		ws.Close()
	}
	if conns, data := q.seen(); conns != 0 {
		t.Fatalf("the squatter on [::1]:%d got %d connections; it read:\n%s", port, conns, data)
	}
	// Each connection's first request is the challenge: launch, session,
	// settings and the WebSocket each came after one.
	log := strings.Split(recording.log(), "\n")
	if len(log) < 7 || !strings.HasPrefix(log[0], "POST "+controllerProofPath+" ") || !strings.HasPrefix(log[1], "POST /api/launch ") {
		t.Fatalf("the controller read:\n%s", strings.Join(log, "\n"))
	}
	proofs := 0
	for _, line := range log {
		if strings.HasPrefix(line, "POST "+controllerProofPath+" ") {
			proofs++
		}
	}
	if proofs != 4 {
		t.Fatalf("%d challenges for 4 connections:\n%s", proofs, strings.Join(log, "\n"))
	}
}

// A controller from before the proof (a record without auth) is not
// contacted by cage-attach; an impostor holding the port is sent only the
// challenge.
func TestSlimClientSendsNothingUnproven(t *testing.T) {
	t.Run("record before the proof", func(t *testing.T) {
		s := testServer(t)
		listener := ephemeralTwinPort(t)
		recording := startTestController(t, s, listener, legacyRecord(s))
		if _, err := controlwire.Connect(s.profile); !errors.Is(err, controlclient.ErrControllerOutdated) {
			t.Fatal(err)
		}
		if seen := recording.log(); seen != "" {
			t.Fatalf("an old controller was contacted:\n%s", seen)
		}
	})
	t.Run("impostor", func(t *testing.T) {
		profile := t.TempDir()
		holdControllerLock(t, profile)
		var port int
		recording, port := impostorController(t, func(w http.ResponseWriter, r *http.Request) { impostorLaunch(port)(w, r) })
		token := randomToken()
		record := controlFile{Port: port, Token: token, PID: 1, Auth: controlAuth, ProofKey: randomToken()}
		if err := writePrivate(filepath.Join(profile, "control.json"), record); err != nil {
			t.Fatal(err)
		}
		if _, err := controlwire.Connect(profile); !errors.Is(err, controlclient.ErrControllerUnproven) {
			t.Fatal(err)
		}
		if seen := recording.log(); strings.Contains(seen, token) || strings.Count(seen, "\n") != 0 || !strings.HasPrefix(seen, "POST "+controllerProofPath+" ") {
			t.Fatalf("the impostor read:\n%s", seen)
		}
	})
}
