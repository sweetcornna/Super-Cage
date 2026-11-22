package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
)

// TestCageEntryRelayBench measures design 5.13's numbers for the gateway's
// part of the cloud entry on this computer:
//
//   - memory per relayed connection: heap and stacks of N live relays through
//     the real handler (admission, dial, splice), over the fixture's pipes;
//
//   - added latency: round trips of one-byte binary messages through
//     relayEntry over loopback TCP, against the same echo without it.
//
//     CAGE_ENTRY_RELAY_BENCH_OUT=/path/relay.json go test -count=1 -run '^TestCageEntryRelayBench$' -v ./internal/tenant
func TestCageEntryRelayBench(t *testing.T) {
	out := os.Getenv("CAGE_ENTRY_RELAY_BENCH_OUT")
	if out == "" {
		t.Skip("set CAGE_ENTRY_RELAY_BENCH_OUT to a file for the results")
	}
	results := map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": runtime.NumCPU()}

	// Memory: N relays held open at once.
	f := newEntryFixture(t, nil)
	host := entryHost(f.owner.vm)
	// Below the per-device admission rate (60 a minute).
	const n = 50
	f.guest.sockets = make(chan *websocket.Conn, n)
	settle := func() (uint64, int) {
		var stats runtime.MemStats
		for range 3 {
			runtime.GC()
			time.Sleep(50 * time.Millisecond)
		}
		runtime.ReadMemStats(&stats)
		return stats.HeapInuse + stats.StackInuse, runtime.NumGoroutine()
	}
	// One connection first, so lazily made state is not counted per relay.
	warm, status, _ := f.client.dial(host, signed(f.owner, host, f.server.now()))
	if status != 101 {
		t.Fatal(status)
	}
	warmBench := f.guest.socket(t)
	beforeBytes, beforeRoutines := settle()
	clients := make([]*websocket.Conn, 0, n)
	benches := make([]*websocket.Conn, 0, n)
	for range n {
		client, status, _ := f.client.dial(host, signed(f.owner, host, f.server.now()))
		if status != 101 {
			t.Fatal(status)
		}
		clients = append(clients, client)
		benches = append(benches, f.guest.socket(t))
	}
	// Each relay carries one message both ways, so its buffers are in use.
	for i := range n {
		_ = clients[i].WriteMessage(websocket.BinaryMessage, []byte("x"))
		_, _, _ = benches[i].ReadMessage()
		_ = benches[i].WriteMessage(websocket.BinaryMessage, []byte("y"))
		_, _, _ = clients[i].ReadMessage()
	}
	afterBytes, afterRoutines := settle()
	// Both ends of the fixture live in this process: the owner's and the
	// workbench's sockets and the fixture's pipe servers count too, so this
	// is an upper bound for the gateway's share.
	results["relays"] = n
	results["bytes_per_relay_upper_bound"] = (afterBytes - beforeBytes) / n
	results["goroutines_per_relay_with_both_fixture_ends"] = float64(afterRoutines-beforeRoutines) / n
	for i := range n {
		_ = clients[i].Close()
		_ = benches[i].Close()
	}
	_ = warm.Close()
	_ = warmBench.Close()

	// Latency over loopback TCP.
	echo := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil || conn.WriteMessage(kind, data) != nil {
				return
			}
		}
	})}
	echoListener, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = echo.Serve(echoListener) }()
	defer echo.Close()
	echoURL := "ws://" + echoListener.Addr().String() + "/"
	relay := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bench, _, err := websocket.DefaultDialer.Dial(echoURL, nil)
		if err != nil {
			return
		}
		upgrader := websocket.Upgrader{ReadBufferSize: entryRelayBuffer, WriteBufferSize: entryRelayBuffer}
		client, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		relayEntry(ctx, cancel, client, bench, func() {})
	})}
	relayListener, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = relay.Serve(relayListener) }()
	defer relay.Close()
	measure := func(url string) []time.Duration {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var samples []time.Duration
		for i := range 2000 {
			start := time.Now()
			if conn.WriteMessage(websocket.BinaryMessage, []byte{byte(i)}) != nil {
				t.Fatal("write")
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			if i >= 100 {
				samples = append(samples, time.Since(start))
			}
		}
		return samples
	}
	stats := func(samples []time.Duration) map[string]float64 {
		slices.Sort(samples)
		at := func(q float64) float64 { return float64(samples[int(q*float64(len(samples)-1))].Microseconds()) / 1000 }
		return map[string]float64{"median_ms": at(0.5), "p95_ms": at(0.95), "max_ms": at(1)}
	}
	var direct, relayed []time.Duration
	for range 3 {
		direct = append(direct, measure(echoURL)...)
		relayed = append(relayed, measure("ws://"+relayListener.Addr().String()+"/")...)
	}
	d, r := stats(direct), stats(relayed)
	results["direct_round_trip"] = d
	results["relayed_round_trip"] = r
	results["added_median_ms"] = r["median_ms"] - d["median_ms"]
	results["added_p95_ms"] = r["p95_ms"] - d["p95_ms"]
	results["mark_interval_s"] = cloudentry.MarkInterval.Seconds()
	data, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(data))
}
