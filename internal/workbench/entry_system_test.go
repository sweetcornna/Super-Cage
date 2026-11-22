//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/agent"
)

// These tests drive the system ssh, scp, sftp and rsync through the
// generated configuration. Its ProxyCommand is this test binary running the
// code of cage entry proxy (TestEntryProxyHelper).

// entryEnded records the refusals of every connection that ends.
type entryEnded struct {
	mu       sync.Mutex
	refusals []map[string]int
}

func (e *entryEnded) watch(s *Server) {
	s.entry.registry.mu.Lock()
	s.entry.registry.observe = func(c *entryConn) {
		e.mu.Lock()
		e.refusals = append(e.refusals, c.refusals())
		e.mu.Unlock()
	}
	s.entry.registry.mu.Unlock()
}

func (e *entryEnded) total(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, refused := range e.refusals {
		n += refused[kind]
	}
	return n
}

func (e *entryEnded) reset() {
	e.mu.Lock()
	e.refusals = nil
	e.mu.Unlock()
}

func entryCombined(cmd *exec.Cmd) (string, string, int) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	return stdout.String(), stderr.String(), code
}

// bareConfig is the generated block without any of its protective options:
// the same ProxyCommand, key and pinned host key, so that the system ssh
// really asks for what the generated block would never let it ask for.
func (f *entryFixture) bareConfig(extra string) string {
	f.t.Helper()
	path := filepath.Join(f.dir, "bare_config")
	text := fmt.Sprintf("Host cage-default\n  HostName cage-default.invalid\n  User cage\n  ProxyCommand %s\n  HostKeyAlias cage-default\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  IdentityFile %s\n  IdentitiesOnly yes\n  ControlPath none\n  BatchMode yes\n%s",
		f.paths.Proxy(defaultLocalVMID), entryConfigPath(f.paths.knownHosts(defaultLocalVMID)), entryConfigPath(f.paths.deviceKey()), extra)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func TestEntrySystemSSHCommandsScpSftpRsync(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	calls := len(testSSHCalls())
	config := f.paths.clientFile()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, stderr, code := entryCombined(f.systemSSH(ctx, config, "cage-default", "echo hello; exit 3"))
	if out != "hello\n" || code != 3 {
		t.Fatalf("ssh: %q %q %d", out, stderr, code)
	}
	home := filepath.Join(f.guest.dir, "home")
	payload := make([]byte, 300<<10)
	_, _ = rand.Read(payload)
	local := filepath.Join(f.dir, "client", "payload.bin")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	// scp and sftp run /usr/bin/ssh unless -S names another: the shim.
	scp := exec.CommandContext(ctx, "/usr/bin/scp", "-S", sshExecutable(), "-F", config, "-q", local, "cage-default:payload-copy.bin")
	scp.Env, scp.Dir = f.sshEnv(), f.dir
	if out, stderr, code := entryCombined(scp); code != 0 {
		t.Fatalf("scp up: %q %q %d", out, stderr, code)
	}
	if copied, err := os.ReadFile(filepath.Join(home, "payload-copy.bin")); err != nil || !bytes.Equal(copied, payload) {
		t.Fatalf("scp up delivered %d bytes, %v", len(copied), err)
	}
	back := filepath.Join(f.dir, "client", "back.bin")
	scp = exec.CommandContext(ctx, "/usr/bin/scp", "-S", sshExecutable(), "-F", config, "-q", "cage-default:payload-copy.bin", back)
	scp.Env, scp.Dir = f.sshEnv(), f.dir
	if out, stderr, code := entryCombined(scp); code != 0 {
		t.Fatalf("scp down: %q %q %d", out, stderr, code)
	}
	if copied, err := os.ReadFile(back); err != nil || !bytes.Equal(copied, payload) {
		t.Fatal("scp down differs", err)
	}
	batch := filepath.Join(f.dir, "batch")
	_ = os.WriteFile(batch, []byte("pwd\nput "+local+" sftp-copy.bin\nls -l sftp-copy.bin\n"), 0o600)
	sftp := exec.CommandContext(ctx, "/usr/bin/sftp", "-S", sshExecutable(), "-F", config, "-b", batch, "cage-default")
	sftp.Env, sftp.Dir = f.sshEnv(), f.dir
	out, stderr, code = entryCombined(sftp)
	if code != 0 || !strings.Contains(out, home) || !strings.Contains(out, "sftp-copy.bin") {
		t.Fatalf("sftp: %q %q %d", out, stderr, code)
	}
	if copied, err := os.ReadFile(filepath.Join(home, "sftp-copy.bin")); err != nil || !bytes.Equal(copied, payload) {
		t.Fatal("sftp put differs", err)
	}
	tree := filepath.Join(f.dir, "client", "tree")
	_ = os.MkdirAll(filepath.Join(tree, "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(tree, "a.txt"), []byte("alpha\n"), 0o600)
	_ = os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("beta\n"), 0o600)
	rsync := exec.CommandContext(ctx, "/usr/bin/rsync", "-a", "-e", sshExecutable()+" -F "+config, tree+"/", "cage-default:synced/")
	rsync.Env, rsync.Dir = f.sshEnv(), f.dir
	if out, stderr, code := entryCombined(rsync); code != 0 {
		t.Fatalf("rsync: %q %q %d", out, stderr, code)
	}
	if b, err := os.ReadFile(filepath.Join(home, "synced", "sub", "b.txt")); err != nil || string(b) != "beta\n" {
		t.Fatal("rsync result", err)
	}
	// ssh -tt: a terminal session through the system client.
	tty := f.systemSSH(ctx, config, "-tt", "cage-default")
	stdin, _ := tty.StdinPipe()
	output := &entrySyncBuffer{}
	tty.Stdout, tty.Stderr = output, output
	if err := tty.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdin, "printf 'P=%s|%s\\n' \"$TERM\" \"$TZ\"; exit\n")
	if err := tty.Wait(); err != nil {
		t.Fatalf("ssh -tt: %v %q", err, output.String())
	}
	if !strings.Contains(output.String(), "P=xterm-256color|Asia/Tokyo") {
		t.Fatalf("ssh -tt output %q", output.String())
	}
	// Each of them ran the shim, never the system ssh straight.
	// (scp speaks SFTP: it and sftp ask for the sftp subsystem.)
	for _, want := range []string{"cage-default echo hello", "cage-default sftp", "rsync --server", "-tt cage-default"} {
		testSSHCalledSince(t, calls, want)
	}
	if n := strings.Count(strings.Join(testSSHCalls()[calls:], "\n"), "-s -- cage-default sftp"); n < 3 {
		t.Fatalf("scp, scp and sftp ran the shim %d times", n)
	}
}

// Design 7.1 row 18: a live connection's processes carry no secret on their
// command lines: the ProxyCommand names an environment, nothing else.
func TestEntryLiveConnectionArgumentsHoldNoSecret(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := f.systemSSH(ctx, f.paths.clientFile(), "-tt", "cage-default")
	stdin, _ := cmd.StdinPipe()
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	entryAwait(t, 10*time.Second, "the ssh terminal", func() bool { return f.s.entry.registry.counts().Shell == 1 })
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(f.paths.deviceKey())
	keyBody := strings.Split(strings.TrimSpace(string(key)), "\n")[1]
	children := map[string]bool{strconv.Itoa(cmd.Process.Pid): true}
	var seen []string
	for range 4 {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && children[fields[1]] {
				children[fields[0]] = true
			}
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !children[fields[0]] {
			continue
		}
		seen = append(seen, strings.Join(fields[2:], " "))
		for _, secret := range []string{f.s.token, f.s.session, trustedViewProof(f.s.token), keyBody} {
			if strings.Contains(line, secret) {
				t.Fatalf("a secret is on the command line: %q", line)
			}
		}
	}
	if len(seen) < 2 {
		t.Fatalf("found only %q", seen)
	}
	t.Logf("processes of the connection: %q", seen)
}

// socksConnect asks a SOCKS5 proxy for 127.0.0.1:22 and returns its reply code.
func socksConnect(address string) (byte, error) {
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return 0, err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return 0, err
	}
	request := []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 0}
	binary.BigEndian.PutUint16(request[8:], 22)
	if _, err := conn.Write(request); err != nil {
		return 0, err
	}
	answer := make([]byte, 2)
	if _, err := io.ReadFull(conn, answer); err != nil {
		return 0xff, nil
	}
	return answer[1], nil
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// waitListening waits until ssh's local forward listens.
func waitListening(t *testing.T, port int) {
	t.Helper()
	entryAwait(t, 10*time.Second, "ssh's local listener", func() bool {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			conn.Close()
		}
		return err == nil
	})
}

// startAgent serves an empty agent on a short socket path, so ssh -A has
// something to forward.
func startAgent(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cage-entry-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	keyring := agent.NewKeyring()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _ = agent.ServeAgent(keyring, conn) }()
		}
	}()
	return path
}

// TestEntrySystemSSHRefusals: rows 1-5 and 7 with the system ssh. The bare
// configuration lets ssh ask for everything, and the endpoint must have
// received and refused each request; the generated configuration keeps ssh
// from asking at all.
func TestEntrySystemSSHRefusals(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	ended := &entryEnded{}
	ended.watch(f.s)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bare := f.bareConfig("")
	refusedBy := func(kind string, run func()) int {
		t.Helper()
		ended.reset()
		run()
		entryAwait(t, 5*time.Second, "the connection to end", func() bool { return f.s.entry.registry.counts().Total == 0 })
		time.Sleep(50 * time.Millisecond)
		return ended.total(kind)
	}
	// -R: remote forwarding.
	if n := refusedBy("global:tcpip-forward", func() {
		_, stderr, code := entryCombined(f.systemSSH(ctx, bare, "-o", "ExitOnForwardFailure=yes", "-R", "127.0.0.1:0:127.0.0.1:9", "cage-default", "true"))
		if code != 255 || !strings.Contains(stderr, "remote port forwarding failed") {
			t.Fatalf("-R: %d %q", code, stderr)
		}
	}); n == 0 {
		t.Fatal("-R never reached the endpoint")
	}
	// -W: stdio forwarding, a direct-tcpip channel.
	if n := refusedBy("channel:direct-tcpip", func() {
		_, stderr, code := entryCombined(f.systemSSH(ctx, bare, "-W", "127.0.0.1:22", "cage-default"))
		if code != 255 || !strings.Contains(stderr, "forwarding failed") {
			t.Fatalf("-W: %d %q", code, stderr)
		}
	}); n == 0 {
		t.Fatal("-W never reached the endpoint")
	}
	// -L and -D: the channel opens only when something connects locally.
	for _, flag := range []string{"-L", "-D"} {
		port := freePort(t)
		spec := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:22", port)
		if flag == "-D" {
			spec = fmt.Sprintf("127.0.0.1:%d", port)
		}
		n := refusedBy("channel:direct-tcpip", func() {
			forward := f.systemSSH(ctx, bare, flag, spec, "-N", "cage-default")
			if err := forward.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = forward.Process.Kill(); _ = forward.Wait() }()
			waitListening(t, port)
			address := fmt.Sprintf("127.0.0.1:%d", port)
			if flag == "-D" {
				if code, err := socksConnect(address); err != nil || code == 0 {
					t.Fatalf("-D: SOCKS reply %d %v", code, err)
				}
				return
			}
			conn, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, _ := conn.Read(make([]byte, 64)); n != 0 {
				t.Fatalf("%s carried %d bytes", flag, n)
			}
			conn.Close()
			entryAwait(t, 5*time.Second, "the refusal", func() bool {
				conn := entryLiveConn(t, f.s)
				return conn.refusals()["channel:direct-tcpip"] > 0
			})
		})
		if n == 0 {
			t.Fatalf("%s never reached the endpoint", flag)
		}
	}
	// -A: agent forwarding.
	socket := startAgent(t)
	if n := refusedBy("auth-agent-req@openssh.com", func() {
		cmd := f.systemSSH(ctx, bare, "-A", "-o", "IdentityAgent="+socket, "cage-default", `printf '[%s]' "$SSH_AUTH_SOCK"`)
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+socket)
		out, stderr, code := entryCombined(cmd)
		if code != 0 || out != "[]" {
			t.Fatalf("-A: %q %q %d", out, stderr, code)
		}
	}); n == 0 {
		t.Fatal("-A never reached the endpoint")
	}
	// -X and -Y: X11 forwarding. ssh asks only with a DISPLAY and, for -X,
	// cookie data from xauth, so a stand-in xauth answers.
	xauth := filepath.Join(f.dir, "xauth")
	if err := os.WriteFile(xauth, []byte("#!/bin/sh\ncase \" $* \" in *\" list \"*) echo 'localhost/unix:0  MIT-MAGIC-COOKIE-1  00112233445566778899aabbccddeeff';; esac\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-X", "-Y"} {
		if n := refusedBy("x11-req", func() {
			cmd := f.systemSSH(ctx, bare, flag, "-o", "XAuthLocation="+xauth, "cage-default", `printf '[%s]' "$DISPLAY"`)
			cmd.Env = append(cmd.Env, "DISPLAY=:0")
			out, stderr, code := entryCombined(cmd)
			if code != 0 || out != "[]" {
				t.Fatalf("%s: %q %q %d", flag, out, stderr, code)
			}
		}); n == 0 {
			t.Fatalf("%s never reached the endpoint", flag)
		}
	}
	// -w: a tunnel device. ssh opens its local tun device first, which this
	// computer refuses without root, so the request may never leave the
	// client; the endpoint's refusal of tun@openssh.com is proved with the
	// x/crypto client (TestEntryRefusesForwardingAgentX11TunnelAndEnvironment).
	n := refusedBy("channel:tun@openssh.com", func() {
		_, stderr, code := entryCombined(f.systemSSH(ctx, bare, "-o", "ExitOnForwardFailure=yes", "-w", "any", "cage-default", "true"))
		t.Logf("-w: exit %d, %q", code, strings.TrimSpace(stderr))
	})
	t.Logf("-w: the endpoint received and refused %d tun@openssh.com channel(s)", n)
	// SendEnv / SetEnv, and the system configuration's own SendEnv LANG LC_*.
	for _, extra := range []string{"  SendEnv LANG LC_* TZ TERM_PROGRAM\n  SetEnv CAGE_LEAK=1\n", "  SetEnv CAGE_LEAK=1\nInclude /etc/ssh/ssh_config\n"} {
		config := f.bareConfig(extra)
		if n := refusedBy("env", func() {
			cmd := f.systemSSH(ctx, config, "cage-default", `printf '%s|%s|%s|%s|%s' "$LANG" "$LC_CTYPE" "$TZ" "$CAGE_LEAK" "$TERM_PROGRAM"`)
			cmd.Env = append(cmd.Env, "LANG=zh_CN.UTF-8", "LC_CTYPE=zh_CN.UTF-8", "TZ=America/New_York", "TERM_PROGRAM=ghostty")
			out, stderr, code := entryCombined(cmd)
			if code != 0 || out != "ja_JP.UTF-8||Asia/Tokyo||" {
				t.Fatalf("env: %q %q %d", out, stderr, code)
			}
		}); n == 0 {
			t.Fatalf("no env request reached the endpoint with %q", extra)
		}
	}
	// The generated block, included at the top of the owner's ssh config,
	// wins over the owner's own Host * defaults (for ssh the first value
	// obtained wins), so with those defaults ssh asks for no forwarding,
	// agent or X11. Flags on ssh's command line beat every config file, and
	// those are what the endpoint's refusals above are for.
	user := filepath.Join(f.dir, "user_config")
	defaults := fmt.Sprintf("%s\nHost *\n  ForwardAgent yes\n  IdentityAgent %s\n  ForwardX11 yes\n  ForwardX11Trusted yes\n  XAuthLocation %s\n  RemoteForward 127.0.0.1:0 127.0.0.1:9\n  LocalForward 127.0.0.1:%d 127.0.0.1:22\n  DynamicForward 127.0.0.1:%d\n  SendEnv LANG LC_*\n  SetEnv CAGE_LEAK=1\n", f.paths.includeLine(), socket, xauth, freePort(t), freePort(t))
	if err := os.WriteFile(user, []byte(defaults), 0o600); err != nil {
		t.Fatal(err)
	}
	n = refusedBy("global:tcpip-forward", func() {
		cmd := f.systemSSH(ctx, user, "cage-default", "echo fine")
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+socket, "DISPLAY=:0", "LANG=zh_CN.UTF-8")
		if out, stderr, code := entryCombined(cmd); code != 0 || out != "fine\n" {
			t.Fatalf("generated: %q %q %d", out, stderr, code)
		}
	})
	if n != 0 || ended.total("auth-agent-req@openssh.com") != 0 || ended.total("x11-req") != 0 || ended.total("channel:direct-tcpip") != 0 {
		t.Fatal("the owner's defaults made ssh ask for forwarding through the generated block")
	}
	// SendEnv adds up across Host blocks, so the owner's SendEnv still
	// reaches the endpoint, which refuses it.
	t.Logf("with the owner's defaults the endpoint refused %d env request(s)", ended.total("env"))
}

// TestEntryProxyRefusalMessages: every refused dial reaches the owner as one
// Chinese line on ssh's stderr, with ssh's failure status 255.
func TestEntryProxyRefusalMessages(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	config := f.paths.clientFile()
	expect := func(want string, env ...string) {
		t.Helper()
		cmd := f.systemSSH(ctx, config, "cage-default", "true")
		cmd.Env = append(cmd.Env, env...)
		_, stderr, code := entryCombined(cmd)
		if code != 255 || !strings.HasPrefix(stderr, "Super Cage："+want) || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("want %q alone: %d %q", want, code, stderr)
		}
	}
	entrySetRunning(t, f.s, false)
	expect(errEntryStopped.Message)
	entrySetRunning(t, f.s, true)
	setGuestReady(f.s, false)
	expect(errEntryStarting.Message)
	setGuestReady(f.s, true)
	_ = f.s.pauseNetworkNow("测试暂停")
	expect(errEntryPaused.Message)
	f.s.mu.Lock()
	f.s.networkPaused = false
	f.s.mu.Unlock()
	expect("Super Cage 没有在运行", "CAGE_ENTRY_TEST_CONFIG="+filepath.Join(f.dir, "nowhere", "config.json"))
	identity := f.s.identity
	f.s.identity.Build = "another-build"
	writeControl := func() {
		t.Helper()
		port := strings.TrimPrefix(f.s.origin, "http://localhost:")
		var number int
		fmt.Sscan(port, &number)
		if err := writePrivate(filepath.Join(f.s.profile, "control.json"), f.s.controlRecord(number)); err != nil {
			t.Fatal(err)
		}
	}
	writeControl()
	expect("需要更新 Super Cage")
	f.s.identity = identity
	writeControl()
	if err := f.call("POST", "/api/entry/devices/"+f.computer.ID+"/revoke", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	expect(errEntryRevoked.Message)
	f.on()
	f.s.account.lock()
	expect(errEntryLocked.Message)
}
