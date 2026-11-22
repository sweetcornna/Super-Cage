//go:build !windows

package workbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/herdrlink"
)

// TestEntryHerdrRealCores is design E14 as a test: two real Herdr cores, the
// owner's on this computer and the environment's behind the entry, with the
// endpoint's Herdr filter between them. It runs only when CAGE_HERDR_CORE
// names a herdr-core binary for this computer, a scratch copy and never an
// installed app's. Both cores run from copies in the test's directory, with
// HOME, XDG_* and runtime folders of their own and SSH_CONNECTION set, so
// that a macOS core starts as a plain process and never through launchd:
//
//	CAGE_HERDR_CORE=/path/herdr-core go test -count=1 -run '^TestEntryHerdrRealCores$' -v ./internal/workbench
//
// It also measures what design 5.13 budgets for Herdr: the endpoint's memory
// for one Herdr line, the inner hop, the environment's core and its connect
// process, and the time from a key to its echo, through the entry and
// straight to the same core (CAGE_HERDR_BENCH_OUT keeps the numbers).
func TestEntryHerdrRealCores(t *testing.T) {
	source := os.Getenv("CAGE_HERDR_CORE")
	if source == "" {
		t.Skip("set CAGE_HERDR_CORE to a herdr-core binary for this computer")
	}
	f := newEntryFixture(t, true)
	h := newEntryHerdrCores(t, f, source)
	f.on()
	results := map[string]any{"goos": runtime.GOOS}
	view := f.s.entryView()
	if view.Herdr == nil || view.Herdr.State != "not_set_up" {
		t.Fatalf("before: %+v", view.Herdr)
	}

	// The owner adds the environment with the command the page prints.
	before := entryHeapKB()
	out, code := h.shell(view.Herdr.Command)
	if code != 0 || !strings.Contains(out, "\"name\":\"cage-default\"") {
		t.Fatalf("%s: %d %s", view.Herdr.Command, code, out)
	}
	t.Logf("%s: %s", view.Herdr.Command, strings.TrimSpace(out))
	entryAwait(t, 20*time.Second, "the machine link", func() bool {
		v := f.s.entryView()
		return v.Herdr != nil && v.Herdr.State == "connected" && v.Connections.Herdr == 1
	})
	time.Sleep(time.Second)
	results["endpoint_kb_one_herdr_line"] = entryHeapKB() - before

	// Work runs there with the environment's settings, its core in Super
	// Cage's runtime folder.
	out, code = h.hc(false, "run", "--json", "--on", "cage-default", "--", "sh", "-c", "echo ok-$((1+1)); echo TZ=$TZ; echo RT=$XDG_RUNTIME_DIR")
	if code != 0 || !strings.Contains(out, "ok-2") || !strings.Contains(out, "TZ=Asia/Tokyo") || !strings.Contains(out, "RT="+filepath.Join(h.short, "g")) {
		t.Fatalf("run --on: %d %s", code, out)
	}

	// E14: a terminal of the owner's starts work in the environment, and
	// that work tries to type into the owner's terminal. The filter refuses
	// it, and the note Herdr gives a parent when its child ends never comes.
	h.script(filepath.Join(h.ownerHome, "parent.sh"), entryHerdrParent)
	h.script(filepath.Join(h.guestHome, "child.sh"), entryHerdrChild)
	if out, code := h.hc(false, "run", "--bg", "--name", "parent", "--", "sh", filepath.Join(h.ownerHome, "parent.sh")); code != 0 {
		t.Fatalf("parent: %d %s", code, out)
	}
	entryAwait(t, 30*time.Second, "the child's call-back", func() bool {
		_, err := os.Stat(filepath.Join(h.guestHome, "send.txt"))
		return err == nil
	})
	sent, _ := os.ReadFile(filepath.Join(h.guestHome, "send.txt"))
	self, _ := os.ReadFile(filepath.Join(h.guestHome, "self.json"))
	if !strings.Contains(string(self), "\""+herdrlink.PeerName+"/") || !strings.Contains(string(sent), herdrlink.DeniedMessage) || strings.Contains(string(sent), "send-exit=0") {
		t.Fatalf("the environment's call-back:\n%s\n%s", self, sent)
	}
	t.Logf("in the environment: %s", strings.TrimSpace(string(sent)))
	time.Sleep(3 * time.Second) // the child's end note, had it passed
	screen, _ := h.hc(false, "read", "parent")
	if !strings.Contains(screen, "parent-waiting") || strings.Contains(screen, "hi-from-guest") || strings.Contains(screen, "<hc>") {
		t.Fatalf("the owner's terminal shows %q", screen)
	}

	// What "在本机终端中打开" with Herdr chosen runs: a shell there, in the
	// background, that Herdr's window can then show.
	start := time.Now()
	out, code = h.hc(false, "run", "--bg", "--on", "cage-default", "--name", "opened", "--cwd", h.guestHome, "--", "bash", "-l")
	results["hc_run_bg_ms"] = ms(time.Since(start))
	if code != 0 {
		t.Fatalf("run --bg --on: %d %s", code, out)
	}
	if listed, _ := h.hc(false, "get", "--json", "cage-default/opened"); !strings.Contains(listed, "\"opened\"") || !strings.Contains(listed, h.guestHome) {
		t.Fatalf("the opened shell: %s", listed)
	} else {
		t.Logf("opened there: %s", listed)
	}
	h.hc(true, "kill", "opened")

	// The environment knows the owner's core only as Super Cage.
	host, _ := os.Hostname()
	host = strings.ToLower(strings.TrimSuffix(host, ".local"))
	peers := h.guestState("peers.json")
	if !strings.Contains(peers, "\""+herdrlink.PeerName+"\"") || host != "" && strings.Contains(strings.ToLower(peers), host) {
		t.Fatalf("the environment's peers: %s", peers)
	}

	// A terminal of the environment on the owner's screen: its bytes come
	// through the stream filter, the clipboard write is taken out.
	h.script(filepath.Join(h.guestHome, "probe.sh"), entryHerdrProbe)
	if out, code := h.hc(false, "run", "--bg", "--on", "cage-default", "--name", "probe", "--", "sh", "probe.sh"); code != 0 {
		t.Fatalf("probe: %d %s", code, out)
	}
	if out, code := h.hc(false, "run", "--bg", "--name", "viewer", "--", "hc", "attach", "cage-default/probe"); code != 0 {
		t.Fatalf("viewer: %d %s", code, out)
	}
	h.awaitScreen("viewer", "probe-waiting")
	if err := os.WriteFile(filepath.Join(h.guestHome, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.awaitScreen("viewer", "after-clip")
	if screen, _ := h.hc(false, "read", "viewer"); !strings.Contains(screen, "stream-ok") {
		t.Fatalf("the viewer shows %q", screen)
	}

	// Memory with the line open and three terminals there.
	results["inner_hop_ssh_rss_kb"] = entryChildRSS(t, os.Getpid(), sshExecutable())
	var listed []string
	for _, p := range h.processes() {
		listed = append(listed, fmt.Sprintf("%d kB %s", p.rss, p.args))
	}
	results["processes"] = listed
	if out, code := h.hc(true, "server", "status", "--json"); code == 0 {
		results["guest_status"] = json.RawMessage(strings.TrimSpace(out))
	}

	// Key to echo through the entry (a second line to the same core) and
	// straight to the core, without the entry and its two SSH layers.
	owner := openHerdrOwner(t, f.client(), fmt.Sprintf(entryHerdrConnectLine, ""))
	if reply := owner.hello(true); !strings.Contains(reply, "\"id\":1") {
		t.Fatalf("hello: %q", reply)
	}
	viaEntry := entryHerdrEcho(t, owner, h.guestHome, "echo-entry", 200)
	owner.in.Close()
	_ = owner.session.Wait()
	direct := h.directOwner()
	straight := entryHerdrEcho(t, direct, h.guestHome, "echo-direct", 200)
	direct.in.Close()
	entryStats, directStats := entryStatsMS(viaEntry), entryStatsMS(straight)
	results["echo_via_entry_ms"], results["echo_direct_ms"] = entryStats, directStats
	results["echo_added_ms"] = map[string]float64{"median": entryStats["median"] - directStats["median"], "p95": entryStats["p95"] - directStats["p95"]}

	// Nothing lives there any more, the entry goes off: the line is cut and
	// the idle stop ends the environment's core; the owner's re-dials start
	// nothing.
	h.hc(false, "kill", "viewer")
	h.hc(false, "kill", "parent")
	h.hc(true, "kill", "probe")
	entryAwait(t, 10*time.Second, "no live terminal there", func() bool {
		out, _ := h.hc(true, "server", "status", "--json")
		return strings.Contains(out, "\"live\":0")
	})
	start = time.Now()
	if err := f.call("POST", "/api/entry/off", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 2*time.Second, "the cut", func() bool { return f.s.entry.registry.counts().Herdr == 0 })
	results["cut_ms"] = ms(time.Since(start))
	entryAwait(t, 30*time.Second, "the idle stop of the environment's core", func() bool { return len(h.guestProcesses()) == 0 })
	results["guest_core_stopped_after_ms"] = ms(time.Since(start))
	time.Sleep(5 * time.Second)
	if left := h.guestProcesses(); len(left) > 0 || f.s.entry.registry.counts().Herdr != 0 {
		t.Fatalf("with the entry off: %v", left)
	}

	// What the endpoint saw, from each line's summary.
	summaries := h.summaries()
	joined := strings.Join(summaries, "\n")
	for _, want := range []string{"hello:renamed", "dropped:terminal_send", "dropped:notes", "stream:filtered"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s in the lines' summaries:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dropped:unannounced") {
		t.Errorf("a frame of the environment's on a stream nobody opened:\n%s", joined)
	}
	results["line_summaries"] = summaries
	encoded, _ := json.MarshalIndent(results, "", "  ")
	t.Logf("herdr: %s", encoded)
	if path := os.Getenv("CAGE_HERDR_BENCH_OUT"); path != "" {
		_ = os.WriteFile(path, encoded, 0o600)
	}
}

const entryHerdrParent = `#!/bin/sh
hc run --bg --on cage-default -- sh child.sh >parent-run.txt 2>&1
echo parent-waiting
i=0
while [ $i -lt 120 ]; do sleep 1; i=$((i+1)); done
`

const entryHerdrChild = `#!/bin/sh
core="$HOME/.local/bin/herdr-core"
"$core" get --json "$HC_TERMINAL_ID" >self.json 2>&1
parent=$(sed -n 's/.*"parent": *"\([^"]*\)".*/\1/p' self.json)
"$core" send "$parent" hi-from-guest >send.tmp 2>&1
echo "send-exit=$?" >>send.tmp
mv send.tmp send.txt
`

const entryHerdrProbe = `#!/bin/sh
echo probe-waiting
i=0
while [ ! -e go ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done
printf 'stream-%s\n' ok
printf '\033]52;c;aGk=\007'
printf 'after-%s\n' clip
i=0
while [ $i -lt 120 ]; do sleep 1; i=$((i+1)); done
`

// entryHerdrCores are the two cores of TestEntryHerdrRealCores.
type entryHerdrCores struct {
	t         *testing.T
	f         *entryFixture
	short     string // under /tmp: macOS allows 104 bytes for a socket path
	diag      string
	ownerHome string
	ownerCore string
	ownerEnv  []string
	guestHome string
	guestCore string
	guestEnv  []string
}

func newEntryHerdrCores(t *testing.T, f *entryFixture, source string) *entryHerdrCores {
	t.Helper()
	short, err := os.MkdirTemp("/tmp", "chr-")
	if err != nil {
		t.Fatal(err)
	}
	h := &entryHerdrCores{t: t, f: f, short: short, diag: filepath.Join(f.dir, "diagnostics"), ownerHome: filepath.Join(f.dir, "owner", "home"), guestHome: filepath.Join(f.guest.dir, "home")}
	// The guest's run script, with a short runtime folder.
	script, err := os.ReadFile(f.s.entry.runPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(script), "herdr_runtime=\"$HOME/.cache/cage-herdr/run\"", "herdr_runtime="+filepath.Join(short, "g"), 1)
	if moved == string(script) {
		t.Fatal("the run script's runtime folder line changed")
	}
	if err := os.WriteFile(f.s.entry.runPath, []byte(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	ownerBin := filepath.Join(f.dir, "owner", "bin")
	h.ownerCore = filepath.Join(ownerBin, "herdr-core")
	h.guestCore = filepath.Join(h.guestHome, ".local", "bin", "herdr-core")
	binary, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{h.ownerCore, h.guestCore} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("herdr-core", filepath.Join(ownerBin, "hc")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{h.ownerHome, filepath.Join(short, "o")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.ownerEnv = append(entryHerdrCoreEnv(h.ownerHome, filepath.Join(short, "o")), "PATH="+ownerBin+":/usr/bin:/bin:/usr/sbin:/sbin")
	// The owner's core runs ssh with the generated configuration, whose
	// ProxyCommand is this test binary (TestEntryProxyHelper).
	for _, value := range f.sshEnv() {
		if !strings.HasPrefix(value, "PATH=") && !strings.HasPrefix(value, "HOME=") {
			h.ownerEnv = append(h.ownerEnv, value)
		}
	}
	h.guestEnv = append(entryHerdrCoreEnv(h.guestHome, filepath.Join(short, "g")), "PATH="+filepath.Dir(h.guestCore)+":/usr/bin:/bin:/usr/sbin:/sbin")
	previous := diagnostics.Default()
	diagnostics.SetDefault(diagnostics.New(h.diag))
	t.Cleanup(func() {
		h.stop()
		diagnostics.SetDefault(previous)
	})
	return h
}

func entryHerdrCoreEnv(home, runtimeDir string) []string {
	return []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "XDG_RUNTIME_DIR=" + runtimeDir,
		"SSH_CONNECTION=127.0.0.1 1 127.0.0.1 22", "LANG=C.UTF-8"}
}

func (h *entryHerdrCores) command(guest bool, name string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	h.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = h.ownerEnv, h.ownerHome
	if guest {
		cmd.Env, cmd.Dir = h.guestEnv, h.guestHome
	}
	return cmd
}

// hc runs hc on this computer or, with guest, in the environment.
func (h *entryHerdrCores) hc(guest bool, args ...string) (string, int) {
	core := h.ownerCore
	if guest {
		core = h.guestCore
	}
	out, stderr, code := entryCombined(h.command(guest, core, args...))
	return out + stderr, code
}

// shell runs a command line as the owner would type it, hc on the PATH.
func (h *entryHerdrCores) shell(line string) (string, int) {
	out, stderr, code := entryCombined(h.command(false, "/bin/sh", "-c", line))
	return out + stderr, code
}

func (h *entryHerdrCores) script(path, text string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
		h.t.Fatal(err)
	}
}

func (h *entryHerdrCores) awaitScreen(terminal, text string) {
	h.t.Helper()
	entryAwait(h.t, 20*time.Second, text+" in "+terminal, func() bool {
		screen, _ := h.hc(false, "read", terminal)
		return strings.Contains(screen, text)
	})
}

// guestState reads a file of the environment's core state.
func (h *entryHerdrCores) guestState(name string) string {
	var text string
	_ = filepath.WalkDir(filepath.Join(h.guestHome, ".local", "state"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			data, _ := os.ReadFile(path)
			text += string(data)
		}
		return nil
	})
	return text
}

// directOwner speaks to the environment's core as the owner's core would,
// over herdr-core connect run right there: no entry, no SSH.
func (h *entryHerdrCores) directOwner() *entryHerdrOwner {
	h.t.Helper()
	cmd := h.command(true, h.guestCore, "connect")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	o := &entryHerdrOwner{t: h.t, in: in, out: bufio.NewReader(out), stderr: &bytes.Buffer{}, frames: make(chan entryWireFrame, 64), ended: make(chan struct{})}
	cmd.Stderr = o.stderr
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if reply := o.hello(true); !strings.Contains(reply, "\"id\":1") {
		h.t.Fatalf("hello: %q", reply)
	}
	return o
}

type entryHerdrProcess struct {
	pid, rss int
	args     string
}

// processes are this test's: its cores, their terminals' scripts, anything
// under its folders.
func (h *entryHerdrCores) processes() []entryHerdrProcess {
	out, err := exec.Command("ps", "-A", "-o", "pid=,rss=,args=").Output()
	if err != nil {
		h.t.Fatal(err)
	}
	var found []entryHerdrProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		args := strings.Join(fields[2:], " ")
		for _, mark := range []string{filepath.Dir(h.ownerCore), h.guestCore, h.short, h.ownerHome, h.guestHome} {
			if strings.Contains(args, mark) {
				pid, _ := strconv.Atoi(fields[0])
				rss, _ := strconv.Atoi(fields[1])
				found = append(found, entryHerdrProcess{pid, rss, args})
				break
			}
		}
	}
	return found
}

func (h *entryHerdrCores) guestProcesses() []entryHerdrProcess {
	var found []entryHerdrProcess
	for _, p := range h.processes() {
		if strings.Contains(p.args, h.guestCore) {
			found = append(found, p)
		}
	}
	return found
}

// summaries are the endpoint's log lines for ended Herdr lines.
func (h *entryHerdrCores) summaries() []string {
	var found []string
	_ = filepath.WalkDir(h.diag, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			var event struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && strings.Contains(event.Message, "Herdr line") {
				found = append(found, event.Message)
			}
		}
		return nil
	})
	return found
}

// stop ends both cores the way Herdr does. Only the processes the cores
// named (core and holders) are killed if they outlive that; anything else of
// this test's still running fails the test.
func (h *entryHerdrCores) stop() {
	var named []int
	for _, guest := range []bool{false, true} {
		dir := filepath.Join(h.short, "o")
		if guest {
			dir = filepath.Join(h.short, "g")
		}
		if sockets, _ := filepath.Glob(filepath.Join(dir, "herdr-core", "*.sock")); len(sockets) == 0 {
			continue
		}
		if out, code := h.hc(guest, "server", "status", "--json"); code == 0 {
			var status struct {
				PID     int `json:"pid"`
				Holders []struct {
					PID int `json:"pid"`
				} `json:"holders"`
			}
			if json.Unmarshal([]byte(out), &status) == nil && status.PID > 0 {
				named = append(named, status.PID)
				for _, holder := range status.Holders {
					named = append(named, holder.PID)
				}
			}
		}
		h.hc(guest, "server", "stop")
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(h.processes()) > 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range h.processes() {
		h.t.Errorf("still running after the cores stopped: %d %s", p.pid, p.args)
		if slices.Contains(named, p.pid) {
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
	_ = os.RemoveAll(h.short)
}

func entryHeapKB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapInuse+m.StackInuse) / 1024
}

// entryHerdrEcho starts "stty raw -echo; cat" in a terminal of the core behind
// o, attaches to it as Herdr's viewer does, and returns the time from each
// key to its echo on the stream (keys 10 ms apart).
func entryHerdrEcho(t *testing.T, o *entryHerdrOwner, dir, name string, keys int) []time.Duration {
	t.Helper()
	script := filepath.Join(dir, name+".sh")
	if err := os.WriteFile(script, []byte("stty raw -echo\necho echo-ready\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	next := 10
	call := func(method string, params any) map[string]any {
		next++
		id := next
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		o.send(herdrlink.Frame(0, herdrlink.KindData, body))
		var reply map[string]any
		o.until(method, func(f entryWireFrame) bool {
			var m map[string]any
			if f.channel != 0 || json.Unmarshal(f.payload, &m) != nil {
				return false
			}
			got, ok := m["id"].(float64)
			if ok && int(got) == id {
				reply = m
				return true
			}
			return false
		})
		result, ok := reply["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: %v", method, reply)
		}
		return result
	}
	call("terminal.run", map[string]any{"argv": []string{"sh", script}, "name": name, "cwd": dir})
	attached := call("terminal.attach", map[string]any{"terminal": name, "cols": 80, "rows": 24})
	channel := uint32(attached["channel"].(float64))
	var painted []byte
	o.until("the echo terminal", func(f entryWireFrame) bool {
		if f.channel == channel {
			painted = append(painted, f.payload...)
		}
		return bytes.Contains(painted, []byte("echo-ready"))
	})
	samples := make([]time.Duration, 0, keys)
	for i := 0; i < keys; i++ {
		key := byte('a' + i%26)
		start := time.Now()
		o.send(herdrlink.Frame(channel, herdrlink.KindData, []byte{key}))
		o.until("an echo", func(f entryWireFrame) bool { return f.channel == channel && bytes.IndexByte(f.payload, key) >= 0 })
		samples = append(samples, time.Since(start))
		time.Sleep(10 * time.Millisecond)
	}
	call("terminal.kill", map[string]any{"terminal": name})
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f := <-o.frames:
			if f.channel == channel && f.kind == herdrlink.KindClose {
				t.Logf("%s: the core's CLOSE carries %q", name, f.payload)
				return samples
			}
		case <-timeout:
			t.Logf("%s: no CLOSE from the core", name)
			return samples
		}
	}
}
