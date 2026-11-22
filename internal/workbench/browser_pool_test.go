package workbench

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A burst of concurrent requests opens one connection each. The next burst,
// after a pause, must find them all still open: each missing one is an SSH
// handshake in front of a wheel step.
func TestBrowserClientKeepsAWholeBurstOfConnectionsWarm(t *testing.T) {
	const burst = 6
	var opened atomic.Int32
	var arrived sync.WaitGroup
	release := make(chan struct{})
	guest := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			arrived.Done()
			<-release
		}
		_, _ = w.Write([]byte("{}"))
	}))
	guest.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	guest.Start()
	defer guest.Close()
	client := localBrowserClient(t, t.Context(), guest)
	run := func() {
		var done sync.WaitGroup
		arrived.Add(burst)
		for range burst {
			done.Add(1)
			go func() {
				defer done.Done()
				if _, err := client.request(t.Context(), "token", http.MethodPost, "/hold", []byte("{}")); err != nil {
					t.Error(err)
				}
			}()
		}
		arrived.Wait()
		release <- struct{}{}
		for range burst - 1 {
			release <- struct{}{}
		}
		done.Wait()
	}
	run()
	if opened.Load() != burst {
		t.Fatalf("the first burst opened %d connections, want %d", opened.Load(), burst)
	}
	time.Sleep(50 * time.Millisecond)
	run()
	if opened.Load() != burst {
		t.Fatalf("the second burst opened %d new connections; the pool did not keep them", opened.Load()-burst)
	}
}

// The host must close an idle connection before the agent does, or a request
// can be written to a connection the agent is closing at that moment.
func TestBrowserClientClosesIdleConnectionsBeforeTheAgentDoes(t *testing.T) {
	source, err := os.ReadFile("../cage/browser-agent.mjs")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`server\.keepAliveTimeout = (\d+);`).FindSubmatch(source)
	if match == nil {
		t.Fatal("the agent no longer sets server.keepAliveTimeout")
	}
	agent, _ := strconv.Atoi(string(match[1]))
	transport := newBrowserClient(t.Context()).http.Transport.(*http.Transport)
	if host := transport.IdleConnTimeout; host <= 30*time.Second || host >= time.Duration(agent)*time.Millisecond {
		t.Fatalf("idle timeout %v must be long enough for a reader's pauses and shorter than the agent's %d ms", host, agent)
	}
}
