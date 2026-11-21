//go:build !windows

package workbench

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// --- row 7: locale, TZ and client environment ---

func TestEntryExecRunsInTheGuestWithItsLocale(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	client := f.client()
	env := map[string]string{"LANG": "zh_CN.UTF-8", "LC_CTYPE": "zh_CN.UTF-8", "TZ": "America/New_York", "CAGE_LEAK": "1", "TERM_PROGRAM": "ghostty"}
	out, stderr, status := entryRun(t, client, `printf '%s|%s|%s|%s|%s|%s|%s|%s\n' "$TZ" "$LANG" "$LC_ALL" "$LC_CTYPE" "$CAGE_LEAK" "$TERM_PROGRAM" "$CAGE_WORKBENCH" "$CAGE_LANGUAGE"`, nil, env)
	if status != 0 {
		t.Fatalf("status %d stderr %q", status, stderr)
	}
	// CAGE_LANGUAGE comes only from the guest's environment file (the
	// private sshd's SetEnv models PAM's TZ and LANG, not it).
	if strings.TrimSpace(out) != "Asia/Tokyo|ja_JP.UTF-8|ja_JP.UTF-8||||1|ja" {
		t.Fatalf("environment = %q", out)
	}
	for _, line := range f.guest.commands() {
		if strings.Contains(line, "zh_CN") || strings.Contains(line, "America/New_York") || strings.Contains(line, "sudo") {
			t.Fatalf("the guest saw %q", line)
		}
	}
	if f.installs.Load() != 1 {
		t.Fatalf("wrapper installs = %d", f.installs.Load())
	}
}

// entryShell is an ssh session with a pty, read line by line.
type entryShell struct {
	t       *testing.T
	session *ssh.Session
	stdin   io.WriteCloser
	mu      sync.Mutex
	output  bytes.Buffer
	changed chan struct{}
}

func entryOpenShell(t *testing.T, client *ssh.Client, cols, rows int, command string) *entryShell {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if session.Setenv("LANG", "zh_CN.UTF-8") == nil {
		t.Fatal("env accepted")
	}
	if err := session.RequestPty("xterm-ghostty", rows, cols, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	shell := &entryShell{t: t, session: session, stdin: stdin, changed: make(chan struct{}, 1)}
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := stdout.Read(buffer)
			shell.mu.Lock()
			shell.output.Write(buffer[:n])
			shell.mu.Unlock()
			select {
			case shell.changed <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	if command == "" {
		err = session.Shell()
	} else {
		err = session.Start(command)
	}
	if err != nil {
		t.Fatal(err)
	}
	return shell
}

// expect waits for a line with prefix and returns its rest.
func (s *entryShell) expect(prefix string) string {
	s.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		text := s.output.String()
		s.mu.Unlock()
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimRight(line, "\r")
			if i := strings.Index(line, prefix); i >= 0 && !strings.Contains(line, "printf") {
				return line[i+len(prefix):]
			}
		}
		select {
		case <-s.changed:
		case <-deadline:
			s.t.Fatalf("no %q in %q", prefix, text)
		}
	}
}

func (s *entryShell) send(text string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdin, text); err != nil {
		s.t.Fatal(err)
	}
}

func entrySSHTerminals(s *Server) []map[string]any {
	var out []map[string]any
	for _, summary := range s.terminalSummaries() {
		if origin, ok := summary["origin"].(map[string]any); ok && origin["kind"] == "ssh" {
			out = append(out, summary)
		}
	}
	return out
}

// --- design 5.4: a pty session is a Cage terminal ---

func TestEntryTerminalIsACageTerminal(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	if err := f.call("POST", "/api/entry/devices/"+f.computer.ID+"/rename", map[string]string{"name": "测试电脑"}, nil); err != nil {
		t.Fatal(err)
	}
	client := f.client()
	shell := entryOpenShell(t, client, 120, 40, "")
	entryAwait(t, 10*time.Second, "the ssh terminal in the list", func() bool { return len(entrySSHTerminals(f.s)) == 1 })
	listed := entrySSHTerminals(f.s)[0]
	origin := listed["origin"].(map[string]any)
	if listed["kind"] != "shell" || origin["device"] != "测试电脑" || origin["kept"] != false || listed["closed"] != false {
		t.Fatalf("listed as %v", listed)
	}
	// The phone lists the same terminals.
	if w := request(f.s, "GET", "/api/terminals", "", "", true); !strings.Contains(w.Body.String(), "测试电脑") {
		t.Fatalf("terminal list %s", w.Body)
	}
	shell.send("printf 'T=%s|%s|%s|%s|%s\\n' \"$TERM\" \"$COLORTERM\" \"$(stty size)\" \"$TZ\" \"$LANG\"\n")
	if got := shell.expect("T="); got != "xterm-256color|truecolor|40 120|Asia/Tokyo|ja_JP.UTF-8" {
		t.Fatalf("terminal = %q", got)
	}
	if err := shell.session.WindowChange(50, 140); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	shell.send("printf 'S=%s\\n' \"$(stty size)\"\n")
	if got := shell.expect("S="); got != "50 140" {
		t.Fatalf("size after window-change = %q", got)
	}
	// A Cage terminal from the menu sees the same environment.
	menu, err := f.s.createTerminal(terminalRequest{Kind: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	if err := menu.enqueueInput(context.Background(), "printf 'M=%s|%s|%s\\n' \"$TZ\" \"$LANG\" \"$LC_ALL\"\n"); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 10*time.Second, "the menu terminal's answer", func() bool {
		return bytes.Contains(menu.outputAfter(0, terminalHistoryLimit).Data, []byte("M=Asia/Tokyo|ja_JP.UTF-8|ja_JP.UTF-8"))
	})
	shell.send("printf 'L=%s\\n' \"$LC_ALL\"\n")
	if got := shell.expect("L="); got != "ja_JP.UTF-8" {
		t.Fatalf("LC_ALL = %q", got)
	}
	shell.send("exit 7\n")
	err = shell.session.Wait()
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 7 {
		t.Fatalf("exit = %v", err)
	}
	entryAwait(t, 5*time.Second, "the ended ssh terminal to leave the list", func() bool { return len(entrySSHTerminals(f.s)) == 0 })
}

func TestEntryTerminalEndsWithTheConnectionUnlessKept(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	for _, keep := range []bool{false, true} {
		client, err := f.dial(f.device)
		if err != nil {
			t.Fatal(err)
		}
		shell := entryOpenShell(t, client, 100, 30, "")
		shell.send("printf 'R=%s\\n' ready\n")
		shell.expect("R=")
		listed := entrySSHTerminals(f.s)
		if len(listed) != 1 {
			t.Fatalf("listed %v", listed)
		}
		id := listed[0]["id"].(string)
		if keep {
			// "保留" is a terminal action: the desktop and the phone may both
			// press it, like closing a terminal.
			if w := request(f.s, "POST", "/api/terminals/"+id+"/keep", f.s.origin, "{}", true); w.Code != 200 || !strings.Contains(w.Body.String(), "\"kept\":true") {
				t.Fatalf("keep: %d %s", w.Code, w.Body)
			}
		}
		client.Close()
		if keep {
			time.Sleep(300 * time.Millisecond)
			if session := f.s.terminals.get(id); session == nil || session.summary()["closed"] == true {
				t.Fatal("a kept terminal ended with its connection")
			}
			f.s.terminals.remove(id)
		} else {
			entryAwait(t, 5*time.Second, "the terminal to end with its connection", func() bool { return f.s.terminals.get(id) == nil })
		}
	}
}

// Several ssh -t sessions that start together each get their terminal.
func TestEntryTerminalsStartTogether(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	client := f.client()
	var wg sync.WaitGroup
	shells := make([]*entryShell, 6)
	for i := range shells {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shells[i] = entryOpenShell(t, client, 80, 24, "")
		}()
	}
	wg.Wait()
	for i, shell := range shells {
		shell.send(fmt.Sprintf("printf 'N=%%s\\n' %d\n", i))
		if got := shell.expect("N="); got != strconv.Itoa(i) {
			t.Fatalf("shell %d answered %q", i, got)
		}
	}
	if n := len(entrySSHTerminals(f.s)); n != 6 {
		t.Fatalf("%d terminals listed", n)
	}
}

func TestEntryTerminalLimit(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	f.s.terminals.mu.Lock()
	if f.s.terminals.sessions == nil {
		f.s.terminals.sessions = map[string]*terminalSession{}
	}
	for i := 0; i < 12; i++ {
		f.s.terminals.sessions[string(rune('a'+i))] = &terminalSession{id: string(rune('a' + i)), done: make(chan struct{})}
	}
	f.s.terminals.mu.Unlock()
	defer func() {
		f.s.terminals.mu.Lock()
		f.s.terminals.sessions = nil
		f.s.terminals.mu.Unlock()
	}()
	shell := entryOpenShell(t, f.client(), 100, 30, "")
	err := shell.session.Wait()
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 {
		t.Fatalf("exit = %v", err)
	}
	entryAwait(t, 5*time.Second, "the limit message", func() bool {
		shell.mu.Lock()
		defer shell.mu.Unlock()
		return strings.Contains(shell.output.String(), entryTerminalLimitText)
	})
}

// --- rows 1-5: no forwarding of any kind, no agent, X11, tunnel, env ---

func entryLiveConn(t *testing.T, s *Server) *entryConn {
	t.Helper()
	var found *entryConn
	entryAwait(t, 5*time.Second, "the live connection", func() bool {
		s.entry.registry.mu.Lock()
		defer s.entry.registry.mu.Unlock()
		for c := range s.entry.registry.conns {
			found = c
		}
		return found != nil
	})
	return found
}

func TestEntryRefusesForwardingAgentX11TunnelAndEnvironment(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	client := f.client()
	conn := entryLiveConn(t, f.s)
	for _, request := range []struct {
		kind    string
		payload []byte
	}{
		{"tcpip-forward", ssh.Marshal(struct {
			Address string
			Port    uint32
		}{"127.0.0.1", 0})},
		{"streamlocal-forward@openssh.com", ssh.Marshal(struct{ Path string }{filepath.Join(f.dir, "remote.sock")})},
		{"cancel-tcpip-forward", ssh.Marshal(struct {
			Address string
			Port    uint32
		}{"127.0.0.1", 0})},
	} {
		if ok, _, err := client.SendRequest(request.kind, true, request.payload); ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", request.kind, ok, err)
		}
	}
	direct := ssh.Marshal(struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}{"127.0.0.1", 22, "127.0.0.1", 40000})
	for _, kind := range []string{"direct-tcpip", "direct-streamlocal@openssh.com", "tun@openssh.com", "x11", "auth-agent@openssh.com", "forwarded-tcpip"} {
		_, _, err := client.OpenChannel(kind, direct)
		var open *ssh.OpenChannelError
		if !errors.As(err, &open) || open.Reason != ssh.Prohibited {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, kind := range []string{"auth-agent-req@openssh.com", "x11-req", "break"} {
		if ok, err := session.SendRequest(kind, true, nil); ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", kind, ok, err)
		}
	}
	for _, name := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TZ", "TERM_PROGRAM", "SSH_AUTH_SOCK", "DISPLAY", "LD_PRELOAD"} {
		if session.Setenv(name, "x") == nil {
			t.Fatalf("env %s accepted", name)
		}
	}
	if session.RequestSubsystem("cage-attach@supercage") == nil {
		t.Fatal("an unknown subsystem was accepted")
	}
	refused := conn.refusals()
	for _, kind := range []string{"global:tcpip-forward", "global:streamlocal-forward@openssh.com", "channel:direct-tcpip", "channel:direct-streamlocal@openssh.com", "channel:tun@openssh.com", "channel:x11", "channel:auth-agent@openssh.com", "auth-agent-req@openssh.com", "x11-req", "env", "subsystem:cage-attach@supercage"} {
		if refused[kind] == 0 {
			t.Fatalf("%s did not reach the endpoint: %v", kind, refused)
		}
	}
	if refused["env"] != 8 {
		t.Fatalf("env refusals %v", refused)
	}
}

// --- design 5.4: Herdr's line is refused until its filter exists ---

func TestEntryRefusesHerdrConnectLine(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	client := f.client()
	refused := []string{
		`sh -c 'p=$(command -v herdr-core) || p=$HOME/.local/bin/herdr-core; [ -x "$p" ] || { echo "herdr-core is not installed here" >&2; exit 127; }; exec "$p" connect'`,
		`sh -c 'p=$(command -v herdr-core) || p=$HOME/.local/bin/herdr-core; [ -x "$p" ] || { echo "herdr-core is not installed here" >&2; exit 127; }; HC_SESSION=work exec "$p" connect'`,
		`exec "$HOME/.local/bin/herdr-core" connect --session work`,
		`hc connect`,
		`cmd /d /c "if exist .\AppData\Local\Programs\herdr-core\herdr-core.exe (.\AppData\Local\Programs\herdr-core\herdr-core.exe connect) else (echo herdr-core is not installed here 1>&2 & exit /b 127)"`,
	}
	for _, line := range refused {
		_, stderr, status := entryRun(t, client, line, nil, nil)
		if status != 1 || !strings.Contains(stderr, "Herdr") {
			t.Fatalf("%q: status %d stderr %q", line, status, stderr)
		}
	}
	for _, line := range f.guest.commands() {
		if strings.Contains(line, "connect") {
			t.Fatalf("a Herdr line reached the guest: %q", line)
		}
	}
	for _, line := range []string{"uname -sm", "herdr-core --version || true", `sh -c 'd=$HOME/.local/bin; echo "$d/herdr-core server restart"'`, "echo reconnect"} {
		if entryHerdrLine(line) {
			t.Fatalf("%q was taken for Herdr's line", line)
		}
		if _, stderr, status := entryRun(t, client, line, nil, nil); status != 0 {
			t.Fatalf("%q: status %d %q", line, status, stderr)
		}
	}
}

// --- sftp ---

func TestEntrySFTPSubsystemReachesSFTPServer(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	session, err := f.client().NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	if err := session.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	// SSH_FXP_INIT, version 3, as one length-prefixed packet.
	if _, err := stdin.Write([]byte{0, 0, 0, 5, 1, 0, 0, 0, 3}); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(bufio.NewReader(stdout), header); err != nil {
		t.Fatal(err)
	}
	if header[4] != 2 {
		t.Fatalf("SSH_FXP_VERSION expected, got %v", header)
	}
}

// --- rows 12-15 and off: every cut ends the connection within a second ---

// entryCut opens a command (cat, with its stdin open) and a terminal on one
// connection, fires the event, and returns how long the client waited for
// the connection to end.
func entryCut(t *testing.T, f *entryFixture, fire func()) time.Duration {
	t.Helper()
	client, err := f.dial(f.device)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	command, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := command.StdinPipe()
	defer stdin.Close()
	echoed := &entrySyncBuffer{}
	command.Stdout = echoed
	if err := command.Start("cat"); err != nil {
		t.Fatal(err)
	}
	shell := entryOpenShell(t, client, 100, 30, "")
	shell.send("printf 'R=%s\\n' ready\n")
	shell.expect("R=")
	if _, err := stdin.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	entryAwait(t, 5*time.Second, "the command's echo", func() bool { return strings.Contains(echoed.String(), "ping") })
	ended := make(chan time.Time, 1)
	go func() {
		_ = client.Wait()
		ended <- time.Now()
	}()
	start := time.Now()
	fire()
	select {
	case at := <-ended:
		elapsed := at.Sub(start)
		entryAwait(t, 5*time.Second, "the ssh terminal to end", func() bool { return len(entrySSHTerminals(f.s)) == 0 })
		return elapsed
	case <-time.After(5 * time.Second):
		t.Fatal("the connection outlived the cut")
	}
	return 0
}

func TestEntryCutsWithinOneSecond(t *testing.T) {
	cases := []struct {
		name    string
		fire    func(*entryFixture)
		refusal string
	}{
		{"lock", func(f *entryFixture) { f.s.account.lock() }, errEntryLocked.Message},
		{"stop", func(f *entryFixture) {
			if err := f.s.stopEnvironment(); err != nil {
				f.t.Error(err)
			}
			entrySetRunning(f.t, f.s, false)
		}, errEntryStopped.Message},
		{"pause", func(f *entryFixture) { _ = f.s.pauseNetworkNow("测试暂停") }, errEntryPaused.Message},
		{"revoke", func(f *entryFixture) {
			if err := f.call("POST", "/api/entry/devices/"+f.computer.ID+"/revoke", map[string]any{}, nil); err != nil {
				f.t.Error(err)
			}
		}, errEntryRevoked.Message},
		{"off", func(f *entryFixture) {
			if err := f.call("POST", "/api/entry/off", map[string]any{}, nil); err != nil {
				f.t.Error(err)
			}
		}, errEntryOff.Message},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEntryFixture(t, true)
			f.on()
			elapsed := entryCut(t, f, func() { c.fire(f) })
			t.Logf("%s: connection ended %v after the event", c.name, elapsed)
			if elapsed > time.Second {
				t.Fatalf("%s took %v", c.name, elapsed)
			}
			_, err := f.dial(f.device)
			if err == nil || !strings.Contains(entryClientError(err), c.refusal) {
				t.Fatalf("next dial: %v", err)
			}
			// A dial never starts a stopped environment.
			if c.name == "stop" && f.s.vmRunning() {
				t.Fatal("the dial started the environment")
			}
		})
	}
}

// The race between a dial's checks and an event: the connection registers
// before it is decided, so an event either refuses it or cuts it.
func TestEntryEventDuringDialNeverLeavesAConnection(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	for i := 0; i < 10; i++ {
		f.s.mu.Lock()
		f.s.networkPaused = false
		f.s.mu.Unlock()
		var wg sync.WaitGroup
		var client *ssh.Client
		var dialErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, dialErr = f.dial(f.device)
		}()
		_ = f.s.pauseNetworkNow("测试暂停")
		wg.Wait()
		if dialErr == nil {
			done := make(chan struct{})
			go func() { _ = client.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("a connection admitted during a pause survived it")
			}
		}
	}
}

// --- row 17: devices are listed, renamed and revoked ---

func TestEntryDevicesRevokeAndRenew(t *testing.T) {
	f := newEntryFixture(t, true)
	view := f.on()
	if len(view.Devices) != 1 || !view.Devices[0].Current || view.Devices[0].Fingerprint != ssh.FingerprintSHA256(f.device.PublicKey()) {
		t.Fatalf("devices %+v", view.Devices)
	}
	// Another key is refused, whatever the user name.
	other := entryWriteKey(t, filepath.Join(f.dir, "other"))
	if _, err := f.dial(other); err == nil {
		t.Fatal("a key that is not enrolled was accepted")
	}
	client := f.client()
	if out, _, _ := entryRun(t, client, "echo ok", nil, nil); strings.TrimSpace(out) != "ok" {
		t.Fatal(out)
	}
	var status entryStatusView
	if err := f.call("GET", "/api/entry", nil, &status); err != nil || status.Devices[0].LastUsed == nil {
		t.Fatalf("last use not recorded: %v %+v", err, status.Devices)
	}
	old := f.device
	if err := f.call("POST", "/api/entry/devices/"+f.computer.ID+"/revoke", map[string]any{}, &status); err != nil || !status.Revoked || !status.Devices[0].Revoked {
		t.Fatalf("revoke: %v %+v", err, status)
	}
	if _, err := f.dial(old); err == nil || !strings.Contains(entryClientError(err), "撤销") {
		t.Fatalf("revoked device: %v", err)
	}
	// Turning it on again renews this computer's key; the old one stays out.
	f.on()
	if entryKeysEqual(f.device.PublicKey(), old.PublicKey()) {
		t.Fatal("the key was not renewed")
	}
	if _, err := f.dial(old); err == nil {
		t.Fatal("the revoked key works again")
	}
	if out, _, _ := entryRun(t, f.client(), "echo again", nil, nil); strings.TrimSpace(out) != "again" {
		t.Fatal(out)
	}
	if err := f.call("POST", "/api/entry/devices/nope/revoke", map[string]any{}, nil); err == nil {
		t.Fatal("revoked an unknown device")
	}
	if err := f.call("POST", "/api/entry/devices/"+f.computer.ID+"/rename", map[string]string{"name": "bad\nname"}, nil); err == nil {
		t.Fatal("accepted a name with a control character")
	}
}

// --- design 5.7 hook: the escape filter ---

// TestEntryTerminalOutputUsesEscapeFilter fails until internal/termfilter
// (branch local-terminal-attach) is plugged into entryNewOutputFilter. A
// guest's OSC 52 clipboard query must never reach the owner's application.
func TestEntryTerminalOutputUsesEscapeFilter(t *testing.T) {
	if entryNewOutputFilter == nil {
		t.Fatal("HOOK(termfilter): entryNewOutputFilter is not plugged in; the entry's terminal output reaches the owner's terminal unfiltered")
	}
	f := newEntryFixture(t, true)
	f.on()
	shell := entryOpenShell(t, f.client(), 100, 30, "")
	shell.send("printf 'A=1\\033]52;c;?\\007B=2\\n'\n")
	shell.expect("B=")
	shell.mu.Lock()
	defer shell.mu.Unlock()
	if bytes.Contains(shell.output.Bytes(), []byte("\x1b]52;c;?")) {
		t.Fatal("an OSC 52 clipboard query reached the ssh client")
	}
}

// entrySyncBuffer is a bytes.Buffer safe for a writer goroutine and a reader.
type entrySyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *entrySyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *entrySyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

var _ = os.Getpid
