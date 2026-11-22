//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestEntryBench measures design 5.13's acceptance numbers on this computer:
// the latency an SSH entry terminal adds per keystroke over Cage's web
// terminal on the same environment, the memory of cage entry proxy, of the
// endpoint per connection and of the inner hop, and the time of a whole
// "ssh cage-x true". It needs a cage binary built from this tree (without
// -race), whose identity the test controller takes on, so that the proxy is
// the real command:
//
//	go build -o /path/cage ./cmd/cage
//	CAGE_ENTRY_BENCH_CAGE=/path/cage CAGE_ENTRY_BENCH_OUT=/path/bench.json \
//	  go test -count=1 -run '^TestEntryBench$' -v ./internal/workbench
//
// The method is design 5.13's (pty_latency2.py): both paths run
// "stty raw -echo; exec cat" in a terminal of the environment and send 150
// keys 30 ms apart; a key's latency is the time until its echo arrives.
func TestEntryBench(t *testing.T) {
	binary := os.Getenv("CAGE_ENTRY_BENCH_CAGE")
	if binary == "" {
		t.Skip("set CAGE_ENTRY_BENCH_CAGE to a cage binary built from this tree")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("darwin and linux only")
	}
	f := newEntryFixture(t, true)
	// The hop of the web terminal and of the entry is the same; with
	// CAGE_ENTRY_BENCH_HOP_FAST=1 it runs without OpenSSH's keystroke timing
	// obfuscation, whose 20 ms chaff otherwise hides the entry's own cost.
	if os.Getenv("CAGE_ENTRY_BENCH_HOP_FAST") != "" {
		hop := filepath.Join(f.s.instanceDir(), "ssh.config")
		data, _ := os.ReadFile(hop)
		if err := os.WriteFile(hop, append(data, " IgnoreUnknown ObscureKeystrokeTiming\n ObscureKeystrokeTiming no\n"...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(f.dir, "client")
	// The paths cage entry proxy derives from HOME.
	f.paths.SSHDir = filepath.Join(home, ".ssh")
	if runtime.GOOS == "darwin" {
		f.paths.DeviceDir = filepath.Join(home, "Library", "Application Support", "Super Cage", "entry")
	} else {
		f.paths.DeviceDir = filepath.Join(home, ".local", "share", "super-cage", "entry")
	}
	proxy := entryProxyCommand(binary, defaultLocalVMID)
	if runtime.GOOS == "darwin" {
		proxy = "/usr/bin/time -l " + proxy
	}
	f.paths.Proxy = func(string) string { return proxy }
	f.on()
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	hashStart := time.Now()
	sum := sha256.Sum256(data)
	hashTime := time.Since(hashStart)
	f.s.identity.Build = fmt.Sprintf("%x", sum)
	config := f.paths.clientFile()
	sshEnv := append(f.sshEnv(), "CLAUDE_CAGE_CONFIG="+f.s.configPath)
	results := map[string]any{"binary_bytes": len(data), "identity_hash_ms": ms(hashTime), "cpus": runtime.NumCPU(), "goos": runtime.GOOS}

	// A whole connection: ssh, proxy start, identity, dial, handshake, the
	// wrapper over the inner hop, the command, and the close.
	var connects []time.Duration
	for i := 0; i < 10; i++ {
		cmd := exec.Command(sshExecutable(), "-F", config, "cage-default", "true")
		cmd.Env, cmd.Dir = sshEnv, f.dir
		start := time.Now()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh true: %v %s", err, out)
		}
		connects = append(connects, time.Since(start))
	}
	results["ssh_true_ms"] = entryStatsMS(connects)

	// Latency, web terminal and SSH entry alternating, two rounds each.
	web := map[string][]time.Duration{}
	for round := 0; round < 2; round++ {
		web["web"] = append(web["web"], entryWebEcho(t, f)...)
		lat, proxyRSS, sshRSS, timeOut := entrySSHEcho(t, f, config, sshEnv)
		web["ssh"] = append(web["ssh"], lat...)
		results[fmt.Sprintf("round%d_proxy_rss_kb", round)] = proxyRSS
		results[fmt.Sprintf("round%d_ssh_client_rss_kb", round)] = sshRSS
		if peak := entryMaxRSS(timeOut); peak > 0 {
			results[fmt.Sprintf("round%d_proxy_peak_rss_kb", round)] = peak / 1024
		}
	}
	webStats, sshStats := entryStatsMS(web["web"]), entryStatsMS(web["ssh"])
	results["web_terminal_ms"] = webStats
	results["ssh_entry_ms"] = sshStats
	results["added_ms"] = map[string]float64{"median": sshStats["median"] - webStats["median"], "p95": sshStats["p95"] - webStats["p95"]}

	// Memory per connection: ten idle exec channels (cat) and ten idle
	// terminals, each its own system ssh.
	for _, kind := range []string{"exec", "pty"} {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		var clients []*exec.Cmd
		var errs []*entrySyncBuffer
		for i := 0; i < 10; i++ {
			args := []string{"-F", config, "-T", "cage-default", "cat"}
			if kind == "pty" {
				args = []string{"-F", config, "-tt", "cage-default"}
			}
			cmd := exec.Command(sshExecutable(), args...)
			cmd.Env, cmd.Dir = sshEnv, f.dir
			stdin, _ := cmd.StdinPipe()
			defer stdin.Close()
			stderr := &entrySyncBuffer{}
			cmd.Stderr = stderr
			errs = append(errs, stderr)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			clients = append(clients, cmd)
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			counts := f.s.entry.registry.counts()
			if kind == "pty" && counts.Shell == 10 || kind == "exec" && counts.Exec == 10 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if counts := f.s.entry.registry.counts(); kind == "pty" && counts.Shell != 10 || kind == "exec" && counts.Exec != 10 {
			for _, e := range errs {
				t.Logf("client stderr: %q", e.String())
			}
			t.Fatalf("%s: %+v", kind, counts)
		}
		entryAwait(t, time.Second, "ten connections", func() bool {
			counts := f.s.entry.registry.counts()
			if kind == "pty" {
				return counts.Shell == 10
			}
			return counts.Exec == 10
		})
		time.Sleep(time.Second)
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		used := func(m runtime.MemStats) float64 { return float64(m.HeapInuse+m.StackInuse) / 1024 }
		results[kind+"_endpoint_kb_per_connection"] = (used(after) - used(before)) / 10
		hops := entryChildRSS(t, os.Getpid(), sshExecutable())
		results[kind+"_inner_hop_ssh_rss_kb"] = hops
		for _, cmd := range clients {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		entryAwait(t, 10*time.Second, "the connections to end", func() bool { return f.s.entry.registry.counts().Total == 0 })
	}
	encoded, _ := json.MarshalIndent(results, "", "  ")
	t.Logf("bench: %s", encoded)
	if out := os.Getenv("CAGE_ENTRY_BENCH_OUT"); out != "" {
		_ = os.WriteFile(out, encoded, 0o600)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func entryStatsMS(samples []time.Duration) map[string]float64 {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	n := len(sorted)
	return map[string]float64{"n": float64(n), "median": ms(sorted[n/2]), "p95": ms(sorted[int(math.Ceil(0.95*float64(n)))-1]), "max": ms(sorted[n-1]), "min": ms(sorted[0])}
}

// entryEchoBuffer collects a stream and wakes a waiter on each write.
type entryEchoBuffer struct {
	mu      sync.Mutex
	data    []byte
	changed chan struct{}
}

func newEntryEchoBuffer() *entryEchoBuffer { return &entryEchoBuffer{changed: make(chan struct{}, 1)} }

func (b *entryEchoBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (b *entryEchoBuffer) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.data)
}

func (b *entryEchoBuffer) waitFor(t *testing.T, marker []byte, from int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		b.mu.Lock()
		found := from <= len(b.data) && bytes.Contains(b.data[from:], marker)
		b.mu.Unlock()
		if found {
			return
		}
		select {
		case <-b.changed:
		case <-deadline:
			b.mu.Lock()
			defer b.mu.Unlock()
			t.Fatalf("no %q after %d in %q", marker, from, b.data)
		}
	}
}

const entryEchoCommand = "stty raw -echo; printf 'RE%sDY\\n' A; exec cat"

func entryKeys(t *testing.T, buffer *entryEchoBuffer, send func(string) error) []time.Duration {
	t.Helper()
	var samples []time.Duration
	for i := 0; i < 150; i++ {
		key := string(rune('a' + i%26))
		from := buffer.size()
		start := time.Now()
		if err := send(key); err != nil {
			t.Fatal(err)
		}
		buffer.waitFor(t, []byte(key), from)
		samples = append(samples, time.Since(start))
		time.Sleep(30 * time.Millisecond)
	}
	return samples
}

// entryWebEcho is the web terminal's path: the browser's WebSocket stream
// of a Cage terminal.
func entryWebEcho(t *testing.T, f *entryFixture) []time.Duration {
	t.Helper()
	session, err := f.s.createTerminal(terminalRequest{Kind: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.terminals.remove(session.id)
	headers := http.Header{"Origin": {f.s.origin}, "Cookie": {f.s.cookieName() + "=" + f.s.session}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.s.origin, "http")+"/api/terminals/"+session.id+"/stream", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	buffer := newEntryEchoBuffer()
	go func() {
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				_, _ = buffer.Write(data)
			}
		}
	}()
	var mu sync.Mutex
	send := func(text string) error {
		mu.Lock()
		defer mu.Unlock()
		return ws.WriteJSON(map[string]string{"type": "input", "data": text})
	}
	_ = send(entryEchoCommand + "\r")
	buffer.waitFor(t, []byte("READY"), 0)
	return entryKeys(t, buffer, send)
}

// entrySSHEcho is the entry's path: ssh -tt through the generated block and
// cage entry proxy. It also reads the RSS of the proxy and of ssh.
func entrySSHEcho(t *testing.T, f *entryFixture, config string, env []string) ([]time.Duration, int, int, string) {
	t.Helper()
	cmd := exec.Command(sshExecutable(), "-F", config, "-tt", "cage-default", entryEchoCommand)
	cmd.Env, cmd.Dir = env, f.dir
	stdin, _ := cmd.StdinPipe()
	buffer := newEntryEchoBuffer()
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = buffer, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buffer.waitFor(t, []byte("READY"), 0)
	samples := entryKeys(t, buffer, func(key string) error { _, err := io.WriteString(stdin, key); return err })
	proxyRSS := entryChildRSS(t, cmd.Process.Pid, "")
	sshRSS := entryProcessRSS(t, cmd.Process.Pid)
	if runtime.GOOS == "darwin" {
		for _, p := range entryProcesses(t) {
			if p.ppid == cmd.Process.Pid {
				for _, child := range entryProcesses(t) {
					if child.ppid == p.pid {
						out, _ := exec.Command("/usr/bin/footprint", strconv.Itoa(child.pid)).CombinedOutput()
						t.Logf("proxy footprint:\n%s", entryFirstLines(string(out), 12))
					}
				}
			}
		}
		out, _ := exec.Command("/usr/bin/footprint", strconv.Itoa(cmd.Process.Pid)).CombinedOutput()
		t.Logf("ssh footprint:\n%s", entryFirstLines(string(out), 12))
	}
	_, _ = stdin.Write([]byte{4})
	_ = stdin.Close()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return samples, proxyRSS, sshRSS, stderr.String()
}

type entryProcess struct {
	pid, ppid, rss int
	command        string
}

func entryProcesses(t *testing.T) []entryProcess {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=,comm=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var list []entryProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		ppid, _ := strconv.Atoi(fields[1])
		rss, _ := strconv.Atoi(fields[2])
		list = append(list, entryProcess{pid, ppid, rss, strings.Join(fields[3:], " ")})
	}
	return list
}

func entryProcessRSS(t *testing.T, pid int) int {
	for _, p := range entryProcesses(t) {
		if p.pid == pid {
			return p.rss
		}
	}
	return 0
}

// entryChildRSS is the median RSS in KB of pid's children, those running
// command when it is set; the proxy is ssh's child (through time -l).
func entryChildRSS(t *testing.T, pid int, command string) int {
	t.Helper()
	processes := entryProcesses(t)
	var values []int
	for _, p := range processes {
		if p.ppid != pid || (command != "" && p.command != command) {
			continue
		}
		rss := p.rss
		if strings.HasSuffix(p.command, "time") {
			for _, child := range processes {
				if child.ppid == p.pid {
					rss = child.rss
				}
			}
		}
		values = append(values, rss)
	}
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return values[len(values)/2]
}

// entryMaxRSS reads /usr/bin/time -l's "maximum resident set size" (bytes).
func entryMaxRSS(stderr string) int {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "maximum resident set size") {
			value, _ := strconv.Atoi(strings.Fields(line)[0])
			return value
		}
	}
	return 0
}

func entryFirstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	return strings.Join(lines[:min(n, len(lines))], "\n")
}

var _ = context.Background
