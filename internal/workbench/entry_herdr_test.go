//go:build !windows

package workbench

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/herdrlink"
)

// The Herdr link's tests run the real endpoint, the real run script and the
// real hop; the core in the guest is this test binary (TestEntryHerdrFakeCore),
// which speaks Herdr's wire protocol and tries what a hostile environment
// would: a banner, a terminal.send and a notes call-back into the owner's
// terminals, and terminal output with clipboard and file-transfer sequences.
// The owner's core is the test itself, over the entry's SSH client.

// TestEntryHerdrFakeCore is the stand-in herdr-core.
func TestEntryHerdrFakeCore(t *testing.T) {
	dir := os.Getenv("CAGE_HERDR_FAKE_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	os.Exit(entryFakeCore(dir, flag.Args()))
}

const entryFakeStream = "guest-says \x1b]52;c;?\x07\x1b]52;c;aGk=\x07\x1b]1337;File=:AAAA\x07done\r\n"

func entryFakeCore(dir string, args []string) int {
	logf := func(format string, a ...any) {
		file, err := os.OpenFile(filepath.Join(dir, "core.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(file, format+"\n", a...)
			file.Close()
		}
	}
	logf("start %s pid=%d runtime=%s session=%s tz=%s", strings.Join(args, " "), os.Getpid(), os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("HC_SESSION"), os.Getenv("TZ"))
	if len(args) == 0 || args[0] != "connect" {
		return 0
	}
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return 1
	}
	_ = os.WriteFile(filepath.Join(dir, "hello.json"), line, 0o600)
	var hello map[string]any
	_ = json.Unmarshal(line, &hello)
	params, _ := hello["params"].(map[string]any)
	framed := params["framed"] == true
	var mu sync.Mutex
	write := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = os.Stdout.Write(b)
	}
	write([]byte("Welcome to the guest (a banner)\n"))
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": hello["id"], "result": map[string]any{"core": map[string]any{"id": "fakecore", "name": "guest"}, "version": "0.1.21", "framed": framed}})
	write(append(reply, '\n'))
	if _, err := os.Stat(filepath.Join(dir, "quit")); err == nil {
		logf("quit")
		return 0
	}
	if !framed {
		_, _ = io.Copy(io.Discard, in)
		logf("eof")
		return 0
	}
	write(herdrlink.Frame(0, herdrlink.KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":900,\"method\":\"terminal.send\",\"params\":{\"terminal\":\"t3\",\"text\":\"typed-by-the-guest\\r\"}}")))
	write(herdrlink.Frame(0, herdrlink.KindData, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"notes\",\"params\":{\"parent\":\"t3\",\"text\":\"noted-by-the-guest\"}}")))
	for {
		var header [herdrlink.HeaderSize]byte
		if _, err := io.ReadFull(in, header[:]); err != nil {
			logf("eof")
			return 0
		}
		channel, kind, size := binary.LittleEndian.Uint32(header[0:4]), header[4], binary.LittleEndian.Uint32(header[5:9])
		payload := make([]byte, size)
		if _, err := io.ReadFull(in, payload); err != nil {
			logf("eof")
			return 0
		}
		switch {
		case channel == 0:
			var message map[string]any
			_ = json.Unmarshal(payload, &message)
			if message["id"] == float64(900) {
				failure, _ := message["error"].(map[string]any)
				data, _ := failure["data"].(map[string]any)
				logf("answer to the call-back: %v", data["kind"])
				continue
			}
			method, _ := message["method"].(string)
			logf("call %s", method)
			result := map[string]any{}
			if method == "terminal.attach" {
				result["channel"] = 5
			}
			r, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": result})
			write(herdrlink.Frame(0, herdrlink.KindData, r))
			if method == "terminal.attach" {
				write(herdrlink.Frame(5, herdrlink.KindData, []byte(entryFakeStream)))
			}
		case kind == herdrlink.KindCredit:
			logf("credit %d %d", channel, binary.LittleEndian.Uint32(payload))
		default:
			logf("frame %d %d %q", channel, kind, payload)
		}
	}
}

// installFakeCore puts the stand-in at the guest's ~/.local/bin/herdr-core.
func (f *entryFixture) installFakeCore() string {
	f.t.Helper()
	dir := filepath.Join(f.dir, "fakecore")
	bin := filepath.Join(f.guest.dir, "home", ".local", "bin")
	for _, d := range []string{dir, bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			f.t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nCAGE_HERDR_FAKE_DIR=" + entryQuote(dir) + " exec " + entryQuote(os.Args[0]) + " -test.run='^TestEntryHerdrFakeCore$' -test.count=1 -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr-core"), []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func entryFakeLog(dir string) string {
	data, _ := os.ReadFile(filepath.Join(dir, "core.log"))
	return string(data)
}

// entryHerdrOwner is the owner's core: one Herdr line over the entry.
type entryHerdrOwner struct {
	t       *testing.T
	session *ssh.Session
	in      io.WriteCloser
	out     *bufio.Reader
	stderr  *bytes.Buffer
	frames  chan entryWireFrame
	ended   chan struct{}
}

type entryWireFrame struct {
	channel uint32
	kind    byte
	payload []byte
}

func openHerdrOwner(t *testing.T, client *ssh.Client, line string) *entryHerdrOwner {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	in, _ := session.StdinPipe()
	out, _ := session.StdoutPipe()
	o := &entryHerdrOwner{t: t, session: session, in: in, out: bufio.NewReader(out), stderr: &bytes.Buffer{}, frames: make(chan entryWireFrame, 64), ended: make(chan struct{})}
	session.Stderr = o.stderr
	if err := session.Start(line); err != nil {
		t.Fatal(err)
	}
	return o
}

const entryOwnerHostName = "owners-macbook-pro"

// hello says hello as Herdr's core does and returns the reply line; framed
// starts reading frames.
func (o *entryHerdrOwner) hello(framed bool) string {
	o.t.Helper()
	hello := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"hello\",\"params\":{\"generation\":1,\"framed\":%v,\"core\":{\"id\":\"0wner\",\"name\":%q},\"calls_you\":\"cage-default\",\"features\":[]}}\n", framed, entryOwnerHostName)
	if _, err := io.WriteString(o.in, hello); err != nil {
		o.t.Fatal(err)
	}
	line, err := o.out.ReadString('\n')
	if err != nil {
		o.t.Fatalf("no hello reply: %v %q", err, o.stderr.String())
	}
	if framed {
		go func() {
			defer close(o.ended)
			for {
				var header [herdrlink.HeaderSize]byte
				if _, err := io.ReadFull(o.out, header[:]); err != nil {
					return
				}
				payload := make([]byte, binary.LittleEndian.Uint32(header[5:9]))
				if _, err := io.ReadFull(o.out, payload); err != nil {
					return
				}
				o.frames <- entryWireFrame{binary.LittleEndian.Uint32(header[0:4]), header[4], payload}
			}
		}()
	}
	return line
}

func (o *entryHerdrOwner) send(frame []byte) {
	o.t.Helper()
	if _, err := o.in.Write(frame); err != nil {
		o.t.Fatal(err)
	}
}

// until reads frames until done says so.
func (o *entryHerdrOwner) until(what string, done func(entryWireFrame) bool) []entryWireFrame {
	o.t.Helper()
	var seen []entryWireFrame
	timeout := time.After(15 * time.Second)
	for {
		select {
		case frame := <-o.frames:
			seen = append(seen, frame)
			if done(frame) {
				return seen
			}
		case <-timeout:
			o.t.Fatalf("timed out waiting for %s; saw %d frames, stderr %q", what, len(seen), o.stderr.String())
		}
	}
}

// attach opens a terminal stream as Herdr's viewer does and returns what
// reached the owner on channel 0 and on the stream.
func (o *entryHerdrOwner) attach() (calls []string, stream string) {
	o.t.Helper()
	o.send(herdrlink.Frame(0, herdrlink.KindData, []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"terminal.attach\",\"params\":{\"terminal\":\"t1\"}}")))
	for _, frame := range o.until("the stream", func(f entryWireFrame) bool {
		return f.channel == 5 && strings.Contains(string(f.payload), "done")
	}) {
		switch frame.channel {
		case 0:
			calls = append(calls, string(frame.payload))
		case 5:
			stream += string(frame.payload)
		}
	}
	return calls, stream
}

func entryHerdrFixture(t *testing.T) (*entryFixture, string) {
	t.Helper()
	f := newEntryFixture(t, true)
	f.on()
	return f, f.installFakeCore()
}

func TestEntryHerdrLinkPassesThroughTheFilter(t *testing.T) {
	f, fake := entryHerdrFixture(t)
	client := f.client()
	owner := openHerdrOwner(t, client, fmt.Sprintf(entryHerdrConnectLine, ""))
	reply := owner.hello(true)
	if !strings.Contains(reply, "\"id\":1") || !strings.Contains(reply, "fakecore") {
		t.Fatalf("the first line at the owner is not the hello reply (a banner passed?): %q", reply)
	}
	calls, stream := owner.attach()
	if len(calls) != 1 || !strings.Contains(calls[0], "\"channel\":5") {
		t.Fatalf("channel 0 at the owner: %q", calls)
	}
	if stream != "guest-says done\r\n" {
		t.Fatalf("the stream at the owner: %q", stream)
	}
	view := f.s.entryView()
	if view.Connections.Herdr != 1 || view.Herdr == nil || view.Herdr.State != "connected" {
		t.Fatalf("while connected: %+v %+v", view.Connections, view.Herdr)
	}
	entryAwait(t, 10*time.Second, "the denial and the credit at the guest", func() bool {
		log := entryFakeLog(fake)
		removed := len(entryFakeStream) - len("guest-says done\r\n")
		return strings.Contains(log, "answer to the call-back: denied") && strings.Contains(log, "credit 5 "+strconv.Itoa(removed))
	})
	hello, _ := os.ReadFile(filepath.Join(fake, "hello.json"))
	if strings.Contains(string(hello), entryOwnerHostName) || !strings.Contains(string(hello), "\"name\":\""+herdrlink.PeerName+"\"") || !strings.Contains(string(hello), "\"id\":\"0wner\"") {
		t.Fatalf("the hello the guest got: %s", hello)
	}
	log := entryFakeLog(fake)
	if !strings.Contains(log, "start connect") || !strings.Contains(log, "runtime="+filepath.Join(f.guest.dir, "home", ".cache", "cage-herdr", "run")) || !strings.Contains(log, "tz=Asia/Tokyo") {
		t.Fatalf("the core ran as %q", log)
	}
	// The owner hangs up: the core sees the end, the line counts as done,
	// and the environment is a machine in Herdr from now on.
	owner.in.Close()
	if err := owner.session.Wait(); err != nil {
		t.Fatalf("the line ended with %v (stderr %q)", err, owner.stderr.String())
	}
	entryAwait(t, 10*time.Second, "the core's end", func() bool { return strings.Contains(entryFakeLog(fake), "eof") })
	view = f.s.entryView()
	if view.Connections.Herdr != 0 || view.Herdr.State != "ready" || view.Herdr.Command != "hc machine add "+view.HostAlias && !strings.Contains(view.Herdr.Command, "--via") {
		t.Fatalf("after: %+v %+v", view.Connections, view.Herdr)
	}
}

func TestEntryHerdrProbeAndMissingCore(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	client := f.client()
	// hc machine add's first hello, before the core is installed.
	_, stderr, status := entryRun(t, client, fmt.Sprintf(entryHerdrConnectLine, ""), []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"hello\",\"params\":{\"generation\":1}}\n"), nil)
	if status != 127 || stderr != "herdr-core is not installed here\n" {
		t.Fatalf("status %d stderr %q", status, stderr)
	}
	// Then the probe of an installed core: plain, answered, hung up.
	fake := f.installFakeCore()
	out, _, status := entryRun(t, client, fmt.Sprintf(entryHerdrConnectLine, "HC_SESSION=work "), []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"hello\",\"params\":{\"generation\":1,\"core\":{\"id\":\"0wner\",\"name\":\""+entryOwnerHostName+"\"}}}\n"), nil)
	if status != 0 || !strings.HasPrefix(out, "{\"id\":1,") && !strings.HasPrefix(out, "{\"jsonrpc\"") || strings.Contains(out, "banner") {
		t.Fatalf("probe: %d %q", status, out)
	}
	if log := entryFakeLog(fake); !strings.Contains(log, "session=work") {
		t.Fatalf("the probe's session: %q", log)
	}
	if st, _ := entryLoadState(f.s.profile); st.HerdrSeen != nil {
		t.Fatal("a probe counted as a machine link")
	}
}

func TestEntryHerdrShortLinesWait(t *testing.T) {
	f, fake := entryHerdrFixture(t)
	client := f.client()
	if err := os.WriteFile(filepath.Join(fake, "quit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The environment ends the line at once: the next one waits.
	owner := openHerdrOwner(t, client, fmt.Sprintf(entryHerdrConnectLine, ""))
	owner.hello(true)
	_ = owner.session.Wait()
	_, stderr, status := entryRun(t, client, fmt.Sprintf(entryHerdrConnectLine, ""), nil, nil)
	if status != 255 || !strings.Contains(stderr, "秒后再连接") {
		t.Fatalf("status %d stderr %q", status, stderr)
	}
	if wait := f.s.entry.herdr.wait(f.computer.ID); wait <= 0 || wait > time.Second {
		t.Fatalf("the first delay is %v", wait)
	}
	// The owner's own hang-ups never count (hc machine add's probes).
	f.s.entry.herdr.ended(f.computer.ID, time.Millisecond, true)
	if wait := f.s.entry.herdr.wait(f.computer.ID); wait > 0 {
		t.Fatal("an owner hang-up delayed the next line")
	}
	var h entryHerdrState
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		h.ended("d", time.Millisecond, false)
		if got := h.delay["d"].next; got != want {
			t.Fatalf("delay %d: %v, want %v", i, got, want)
		}
	}
	h.ended("d", entryHerdrShortLine, false)
	if h.wait("d") > 0 {
		t.Fatal("a line that lasted kept the delay")
	}
}

func TestEntryHerdrCutsLikeSSH(t *testing.T) {
	post := func(path string) func(*entryFixture) {
		return func(f *entryFixture) {
			if err := f.call("POST", path, map[string]any{}, nil); err != nil {
				f.t.Error(err)
			}
		}
	}
	for _, c := range []struct {
		name    string
		fire    func(*entryFixture)
		refusal *entryRefusal
	}{
		{"lock", func(f *entryFixture) { f.s.account.lock() }, errEntryLocked},
		{"off", post("/api/entry/off"), errEntryOff},
		{"revoke", func(f *entryFixture) { post("/api/entry/devices/" + f.computer.ID + "/revoke")(f) }, errEntryRevoked},
		{"pause", func(f *entryFixture) { _ = f.s.pauseNetworkNow("测试暂停") }, errEntryPaused},
		{"stop", func(f *entryFixture) {
			if err := f.s.stopEnvironment(); err != nil {
				f.t.Error(err)
			}
			entrySetRunning(f.t, f.s, false)
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, fake := entryHerdrFixture(t)
			client, err := f.dial(f.device)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			owner := openHerdrOwner(t, client, fmt.Sprintf(entryHerdrConnectLine, ""))
			owner.hello(true)
			owner.attach()
			start := time.Now()
			c.fire(f)
			select {
			case <-owner.ended:
			case <-time.After(5 * time.Second):
				t.Fatal("the line was not cut")
			}
			if took := time.Since(start); took > time.Second {
				t.Fatalf("the cut took %v", took)
			} else {
				t.Logf("%s: the owner's end of the line closed %v after the event", c.name, took.Round(time.Millisecond))
			}
			entryAwait(t, 10*time.Second, "the core's end", func() bool { return strings.Contains(entryFakeLog(fake), "eof") })
			if c.refusal != nil {
				if _, err := f.dial(f.device); err == nil || !strings.Contains(err.Error(), c.refusal.Message) {
					t.Fatalf("the next dial: %v", err)
				}
			}
		})
	}
}

// After a line ends nothing of Herdr's runs on its account: the line's
// process is gone and the idle check, armed for a moment, has run.
func TestEntryHerdrLeavesNothingRunning(t *testing.T) {
	f, fake := entryHerdrFixture(t)
	client := f.client()
	owner := openHerdrOwner(t, client, fmt.Sprintf(entryHerdrConnectLine, ""))
	owner.hello(true)
	owner.attach()
	owner.in.Close()
	_ = owner.session.Wait()
	entryAwait(t, 10*time.Second, "the core's end", func() bool { return strings.Contains(entryFakeLog(fake), "eof") })
	var pid int
	for _, line := range strings.Split(entryFakeLog(fake), "\n") {
		if strings.HasPrefix(line, "start connect") {
			_, _ = fmt.Sscanf(line[strings.Index(line, "pid="):], "pid=%d", &pid)
		}
	}
	if pid == 0 {
		t.Fatal("no pid")
	}
	entryAwait(t, 10*time.Second, "the line's process to end", func() bool { return syscall.Kill(pid, 0) != nil })
	runtimeDir := filepath.Join(f.guest.dir, "home", ".cache", "cage-herdr", "run")
	entryAwait(t, 10*time.Second, "the idle check to run", func() bool {
		_, armed := os.Stat(filepath.Join(runtimeDir, "cage-idle-default.pid"))
		marks, _ := filepath.Glob(filepath.Join(runtimeDir, "cage-lines", "default.*"))
		return os.IsNotExist(armed) && len(marks) == 0
	})
}

// What the environment writes to a Herdr line's error output reaches the
// owner's core as plain words, without the lines that would make it believe
// ssh wants a login (review 3, H1's class: guest bytes the owner's core
// reads).
func TestEntryHerdrErrorsArePlainWordsWithoutLoginLines(t *testing.T) {
	var got bytes.Buffer
	errs := &entryHerdrErrors{plain: &entryPlainWriter{w: &got, left: entryHerdrStderrLimit}}
	for _, chunk := range []string{
		"herdr-core is not \x1b]52;c;aGk=\x07installed here\n",
		"Permission den", "ied (publickey).\n",
		"Permission\u200b denied (password)\n",
		"Received disconnect: Too many authentication failures\n",
		"last words\u202e without an end",
	} {
		if _, err := errs.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errs.Close(); err != nil {
		t.Fatal(err)
	}
	if want := "herdr-core is not ]52;c;aGk=installed here\nlast words without an end"; got.String() != want {
		t.Fatalf("got %q, want %q", got.String(), want)
	}
}
