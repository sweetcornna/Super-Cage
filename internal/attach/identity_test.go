package attach

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/controlwire"
)

// A server that cannot prove it is the controller ends a reconnect with the
// sentence that says so once the controller's record, read again, still
// names it (a restarted controller has a new key), having been sent nothing
// but challenges; it is not retried as an unavailable controller.
func TestReconnectStopsAtAnUnprovenController(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+" cookie="+r.Header.Get("Cookie"))
		mu.Unlock()
		w.Write([]byte(`{"version":1,"proof":"` + strings.Repeat("0", 64) + `"}`))
	}))
	defer impostor.Close()
	profile := t.TempDir()
	record := fmt.Sprintf(`{"port":%d,"token":"%s","pid":1,"auth":1,"proof_key":"%s"}`, impostor.Listener.Addr().(*net.TCPAddr).Port, strings.Repeat("a", 64), strings.Repeat("5e", 32))
	if err := os.WriteFile(filepath.Join(profile, "control.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := controlclient.OpenLock(profile, os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlclient.Lock(lock); err != nil {
		t.Fatal(err)
	}
	defer func() { controlclient.Unlock(lock); lock.Close() }()
	c, err := controlwire.Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	a := &attacher{c: c, terminal: "0123456789abcdef", check: func(controlclient.ControllerIdentity) bool { return true },
		cfg: Config{ConfigPath: filepath.Join(profile, "config.json"), Target: controlclient.AgentTarget{Scope: "local"}}, tty: TTY{Closed: make(chan struct{})}}
	start := time.Now()
	ws, exit, ok := a.resume(context.Background(), nil)
	if ws != nil || ok || exit != (Exit{1, controlclient.ErrControllerUnproven.Error()}) || time.Since(start) > 2*time.Second {
		t.Fatalf("%v %+v %v after %v", ws, exit, ok, time.Since(start))
	}
	mu.Lock()
	defer mu.Unlock()
	// The account check, the stream and the launch after reading the record
	// again: three challenges, nothing else.
	if len(paths) != 3 || paths[0] != controlclient.ProofPath+" cookie=" || paths[1] != paths[0] || paths[2] != paths[0] {
		t.Fatal(paths)
	}
	if got := attachFailure(controlclient.IdentityAgentError(controlclient.ErrControllerOutdated)); got != (Exit{1, controlclient.ErrControllerOutdated.Error()}) {
		t.Fatal(got)
	}
}

// A controller restarted on the same port has a new proof key: the attach
// reads the record again and goes on with the new key, rather than taking
// the old key's failure for an impostor.
func TestReconnectFollowsANewProofKey(t *testing.T) {
	var key atomic.Value
	key.Store(strings.Repeat("5e", 32))
	var launches atomic.Int32
	var port int
	profile := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case controlclient.ProofPath:
			var in struct{ Nonce string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			local := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "proof": controlclient.ProofMAC(key.Load().(string), r.Host, r.RemoteAddr, local.String(), in.Nonce)})
		case "/api/launch":
			launches.Add(1)
			json.NewEncoder(w).Encode(controlclient.LaunchResponse{URL: fmt.Sprintf("http://localhost:%d/#ticket", port), ConfigPath: filepath.Join(profile, "config.json")})
		case "/api/session":
			w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	port = server.Listener.Addr().(*net.TCPAddr).Port
	publish := func() {
		record := fmt.Sprintf(`{"port":%d,"token":"%s","pid":1,"auth":1,"proof_key":"%s"}`, port, strings.Repeat("a", 64), key.Load().(string))
		if err := os.WriteFile(filepath.Join(profile, "control.json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	lock, err := controlclient.OpenLock(profile, os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlclient.Lock(lock); err != nil {
		t.Fatal(err)
	}
	defer func() { controlclient.Unlock(lock); lock.Close() }()
	c, err := controlwire.Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	// The restart: a new key, published in the record.
	key.Store(strings.Repeat("6f", 32))
	publish()
	a := &attacher{c: c, terminal: "0123456789abcdef", check: func(controlclient.ControllerIdentity) bool { return true },
		cfg: Config{ConfigPath: filepath.Join(profile, "config.json"), Target: controlclient.AgentTarget{Scope: "local"}}, tty: TTY{Closed: make(chan struct{})}}
	_, exit, ok := a.resume(context.Background(), nil)
	// The stream is gone in this stand-in (404): what matters is that the
	// new key was used, not refused as an impostor.
	if ok || exit.Message == controlclient.ErrControllerUnproven.Error() || launches.Load() != 1 {
		t.Fatalf("%+v %v, %d launches", exit, ok, launches.Load())
	}
}
