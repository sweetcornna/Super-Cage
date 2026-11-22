package cage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Readiness polls quickly at first: three attempts take well under the two
// seconds that fixed one-second pauses cost.
func TestGuestReadinessPollsQuicklyAtFirst(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	var browserRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/browser" && browserRequests.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		_, _ = fmt.Fprint(w, `{"ready":true}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	script, config := filepath.Join(dir, "wait.py"), filepath.Join(dir, "browser.json")
	if err := os.WriteFile(script, []byte(guestWaitServices), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"token":"fixture-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	endpoints, _ := json.Marshal([][]any{{server.URL + "/browser", true}})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", `import runpy,sys,json;runpy.run_path(sys.argv[1])['wait'](sys.argv[2],json.loads(sys.argv[3]),timeout=10)`, script, config, string(endpoints))
	cmd.Env = append(os.Environ(), "no_proxy=*", "NO_PROXY=*")
	started := time.Now()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("readiness: %v %s", err, output)
	}
	// Python start-up included; two fixed one-second pauses took at least 2 s.
	if elapsed := time.Since(started); browserRequests.Load() != 3 || elapsed > 1500*time.Millisecond {
		t.Fatalf("three attempts took %s (requests %d)", elapsed, browserRequests.Load())
	}
}
