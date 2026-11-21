package attach

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/controlwire"
)

// A server that cannot prove it is the controller ends a reconnect at once
// with the sentence that says so, having been sent nothing but challenges;
// it is not retried as an unavailable controller.
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
	c, err := controlwire.Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	a := &attacher{c: c, terminal: "0123456789abcdef", cfg: Config{Target: controlclient.AgentTarget{Scope: "local"}}, tty: TTY{Closed: make(chan struct{})}}
	start := time.Now()
	ws, exit, ok := a.resume(context.Background(), nil)
	if ws != nil || ok || exit != (Exit{1, controlclient.ErrControllerUnproven.Error()}) || time.Since(start) > 2*time.Second {
		t.Fatalf("%v %+v %v after %v", ws, exit, ok, time.Since(start))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != controlclient.ProofPath+" cookie=" {
		t.Fatal(paths)
	}
	if got := attachFailure(controlclient.IdentityAgentError(controlclient.ErrControllerOutdated)); got != (Exit{1, controlclient.ErrControllerOutdated.Error()}) {
		t.Fatal(got)
	}
}
