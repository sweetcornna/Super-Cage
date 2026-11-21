//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cage"
)

// The entry's tests run every piece for real except the guest: Cage's own
// hop (s.sshCommand, the system ssh with the instance's ssh.config) reaches
// a private OpenSSH sshd started in inetd mode (sshd -i) for each
// connection, as in experiment E1, with its own configuration, host key and
// authorized_keys in the test's directory. The production hop pins
// ProxyCommand=none, so the sshd sits behind a 127.0.0.1 listener that hands
// each accepted connection to a new sshd -i as its stdin and stdout, like
// inetd. That sshd runs as the current user on this computer, so its
// ForceCommand shim moves HOME, ZDOTDIR and XDG_* into the test directory
// before anything reads them, and nothing reaches the real home.

const entryTestSSHD = "/usr/sbin/sshd"

// entryGuest is the stand-in guest.
type entryGuest struct {
	dir      string
	listener net.Listener
	mu       sync.Mutex
	procs    map[int]*os.Process
	accepted atomic.Int32
}

func (g *entryGuest) commands() []string {
	data, _ := os.ReadFile(filepath.Join(g.dir, "commands.log"))
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func entryWriteKey(t *testing.T, path string) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "entry-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// startEntryGuest prepares the private sshd and points the server's hop at it.
func startEntryGuest(t *testing.T, s *Server, dir string) *entryGuest {
	t.Helper()
	if _, err := os.Stat(entryTestSSHD); err != nil {
		t.Skip("OpenSSH sshd is required")
	}
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH ssh is required")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	g := &entryGuest{dir: filepath.Join(dir, "guest"), procs: map[int]*os.Process{}}
	home := filepath.Join(g.dir, "home")
	for _, d := range []string{home, filepath.Join(home, ".config"), filepath.Join(home, ".local", "share"), filepath.Join(home, ".cache")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hostKey := entryWriteKey(t, filepath.Join(g.dir, "host_ed25519"))
	hopKey := entryWriteKey(t, filepath.Join(g.dir, "hop_ed25519"))
	if err := os.WriteFile(filepath.Join(g.dir, "authorized_keys"), ssh.MarshalAuthorizedKey(hopKey.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	// The guest's /etc/claude-cage/environment.
	if err := os.WriteFile(filepath.Join(g.dir, "environment"), []byte("TZ=Asia/Tokyo\nLANG=ja_JP.UTF-8\nLC_ALL=ja_JP.UTF-8\nCAGE_LANGUAGE=ja\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := fmt.Sprintf("#!/bin/sh\nexport HOME=%[1]q ZDOTDIR=%[1]q XDG_CONFIG_HOME=%[1]q/.config XDG_DATA_HOME=%[1]q/.local/share XDG_CACHE_HOME=%[1]q/.cache XDG_STATE_HOME=%[1]q/.local/state\n"+
		"printf '%%s\\n' \"$SSH_ORIGINAL_COMMAND\" >> %[2]q\ncd \"$HOME\" || exit 1\nexec /bin/sh -c \"$SSH_ORIGINAL_COMMAND\"\n", home, filepath.Join(g.dir, "commands.log"))
	if err := os.WriteFile(filepath.Join(g.dir, "shim.sh"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	// Like a Lima guest, this sshd would forward and accept environment
	// variables: if the endpoint passed any of it on, the tests would see it.
	config := fmt.Sprintf(`HostKey %s
AuthorizedKeysFile %s
PidFile none
UsePAM no
StrictModes no
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowTcpForwarding yes
AllowStreamLocalForwarding yes
AllowAgentForwarding yes
X11Forwarding no
PermitTunnel no
AcceptEnv LANG LC_* TZ CAGE_* TERM_PROGRAM
PermitUserEnvironment no
PermitUserRC no
PrintMotd no
PrintLastLog no
LogLevel VERBOSE
SetEnv TZ=Asia/Tokyo LANG=ja_JP.UTF-8 LC_ALL=ja_JP.UTF-8 ZDOTDIR=%s
ForceCommand %s
`, filepath.Join(g.dir, "host_ed25519"), filepath.Join(g.dir, "authorized_keys"), home, filepath.Join(g.dir, "shim.sh"))
	if err := os.WriteFile(filepath.Join(g.dir, "sshd_config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(entryTestSSHD, "-t", "-f", filepath.Join(g.dir, "sshd_config")).CombinedOutput(); err != nil {
		t.Fatalf("sshd config: %v %s", err, out)
	}
	g.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go g.serve()
	t.Cleanup(g.close)
	instance := s.instanceDir()
	if err := os.MkdirAll(instance, 0o700); err != nil {
		t.Fatal(err)
	}
	port := g.listener.Addr().(*net.TCPAddr).Port
	known := filepath.Join(g.dir, "hop_known_hosts")
	if err := os.WriteFile(known, append([]byte(fmt.Sprintf("[127.0.0.1]:%d ", port)), ssh.MarshalAuthorizedKey(hostKey.PublicKey())...), 0o600); err != nil {
		t.Fatal(err)
	}
	hop := fmt.Sprintf("Host *\n HostName 127.0.0.1\n Port %d\n User %s\n StrictHostKeyChecking yes\n UserKnownHostsFile %q\n GlobalKnownHostsFile /dev/null\n IdentityFile %q\n", port, current.Username, known, filepath.Join(g.dir, "hop_ed25519"))
	if err := os.WriteFile(filepath.Join(instance, "ssh.config"), []byte(hop), 0o600); err != nil {
		t.Fatal(err)
	}
	entrySetRunning(t, s, true)
	return g
}

func (g *entryGuest) serve() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			return
		}
		g.accepted.Add(1)
		file, err := conn.(*net.TCPConn).File()
		conn.Close()
		if err != nil {
			continue
		}
		cmd := exec.Command(entryTestSSHD, "-i", "-f", filepath.Join(g.dir, "sshd_config"), "-E", filepath.Join(g.dir, "sshd.log"))
		cmd.Stdin, cmd.Stdout = file, file
		if cmd.Start() != nil {
			file.Close()
			continue
		}
		file.Close()
		g.mu.Lock()
		g.procs[cmd.Process.Pid] = cmd.Process
		g.mu.Unlock()
		go func() {
			_ = cmd.Wait()
			g.mu.Lock()
			delete(g.procs, cmd.Process.Pid)
			g.mu.Unlock()
		}()
	}
}

// close stops the listener and every sshd this guest started, by PID.
func (g *entryGuest) close() {
	g.listener.Close()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, process := range g.procs {
		_ = process.Kill()
	}
}

func entrySetRunning(t *testing.T, s *Server, running bool) {
	t.Helper()
	marker := filepath.Join(s.instanceDir(), "cage-running.json")
	if running {
		if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = os.Remove(marker)
	}
	setGuestReady(s, running)
}

// entryFixture is a controller with the entry's paths in the test directory.
type entryFixture struct {
	t        *testing.T
	s        *Server
	dir      string
	paths    entryPaths
	guest    *entryGuest
	installs atomic.Int32
	computer entryComputer
	device   ssh.Signer
	hostKey  ssh.PublicKey
	// filters are the stand-in escape filters terminal sessions used.
	filtersMu sync.Mutex
	filters   []*entryRecordingFilter
}

// entryRecordingFilter stands in for internal/termfilter: it drops the OSC 52
// query the real filter drops, passes the rest, and records how the endpoint
// drove it ("live N", "replay N", "abort").
type entryRecordingFilter struct {
	mu     sync.Mutex
	replay bool
	events []string
}

func (r *entryRecordingFilter) Append(dst, src []byte) []byte {
	r.mu.Lock()
	kind := "live"
	if r.replay {
		kind = "replay"
	}
	r.events = append(r.events, fmt.Sprintf("%s %d", kind, len(src)))
	r.mu.Unlock()
	return append(dst, bytes.ReplaceAll(src, []byte("\x1b]52;c;?\x07"), nil)...)
}

func (r *entryRecordingFilter) SetReplay(on bool) {
	r.mu.Lock()
	r.replay = on
	r.mu.Unlock()
}

func (r *entryRecordingFilter) Abort(dst []byte) []byte {
	r.mu.Lock()
	r.events = append(r.events, "abort")
	r.mu.Unlock()
	return dst
}

func (r *entryRecordingFilter) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// entryTestController is agentTestController with symlinks resolved, so the
// project path a terminal checks is the one the profile names.
func entryTestController(t *testing.T) *Server {
	t.Helper()
	s := agentTestController(t)
	resolved, err := filepath.EvalSymlinks(s.profile)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != s.profile {
		s.profile, s.configPath = resolved, filepath.Join(resolved, "config.json")
	}
	return s
}

// entryRealHomeGuard fails the test when anything of the entry's in the real
// home of this computer's user changed while it ran. It only reads. OpenSSH
// expands ~ from the user database, not from $HOME, so the guard does too.
func entryRealHomeGuard(t *testing.T) {
	t.Helper()
	current, err := user.Current()
	if err != nil || current.HomeDir == "" {
		t.Fatal("no home directory", err)
	}
	home := current.HomeDir
	watched := []string{
		filepath.Join(home, ".ssh", "config"),
		filepath.Join(home, ".ssh", "known_hosts"),
		filepath.Join(home, ".ssh", entryClientDir),
		filepath.Join(home, ".claude-cage", "entry"),
		filepath.Join(home, ".claude-cage", "local-terminal.json"),
		filepath.Join(home, ".local", "share", "super-cage"),
		filepath.Join(home, "Library", "Application Support", "Super Cage", "entry"),
	}
	stamp := func(path string) string {
		info, err := os.Lstat(path)
		if err != nil {
			return "absent"
		}
		return fmt.Sprint(info.Mode(), info.ModTime().UnixNano(), info.Size())
	}
	before := map[string]string{}
	for _, path := range watched {
		before[path] = stamp(path)
	}
	t.Cleanup(func() {
		for path, was := range before {
			if now := stamp(path); now != was {
				t.Errorf("the real %s changed during the test (%s -> %s)", path, was, now)
			}
		}
	})
}

func newEntryFixture(t *testing.T, guest bool) *entryFixture {
	t.Helper()
	entryRealHomeGuard(t)
	s := entryTestController(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &entryFixture{t: t, s: s, dir: dir}
	f.paths = entryPaths{
		SSHDir: filepath.Join(dir, "client", ".ssh"),
		// Like ~/Library/Application Support, with a space, and a % that
		// ssh would expand: the system ssh tests connect through it.
		DeviceDir:   filepath.Join(dir, "client", "Application Support", "Super Cage 100%", "entry"),
		ComputerDir: s.profile,
		Proxy: func(environment string) string {
			return entryQuote(os.Args[0]) + " -test.run=^TestEntryProxyHelper$ -test.count=1"
		},
		Cage: "/opt/super-cage/cage",
	}
	s.entry.paths = &f.paths
	run := strings.Replace(cage.EntryRunScript(), "environment=/etc/claude-cage/environment", "environment="+filepath.Join(dir, "guest", "environment"), 1)
	if err := os.MkdirAll(filepath.Join(dir, "guest"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.entry.runPath = filepath.Join(dir, "guest", "run")
	if err := os.WriteFile(s.entry.runPath, []byte(run), 0o755); err != nil {
		t.Fatal(err)
	}
	s.entry.install = func(context.Context) error { f.installs.Add(1); return nil }
	// Terminal sessions need the escape filter (design 5.7); a recording
	// stand-in takes the place of internal/termfilter here.
	previousFilter := entryNewOutputFilter
	entryNewOutputFilter = func(bool) entryOutputFilter {
		filter := &entryRecordingFilter{}
		f.filtersMu.Lock()
		f.filters = append(f.filters, filter)
		f.filtersMu.Unlock()
		return filter
	}
	t.Cleanup(func() { entryNewOutputFilter = previousFilter })
	workspace := s.environmentDir()
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	s.preferences.Workspace = workspace
	if guest {
		f.guest = startEntryGuest(t, s, dir)
	}
	t.Cleanup(func() {
		s.entry.registry.cutAll(errEntryClosing)
		s.terminals.closeAll()
	})
	return f
}

// on turns the entry on through the same call cage entry on makes.
func (f *entryFixture) on() entryStatusView {
	f.t.Helper()
	var view entryStatusView
	if err := f.call("POST", "/api/entry/on", map[string]any{}, &view); err != nil {
		f.t.Fatal(err)
	}
	f.loadDevice()
	return view
}

func (f *entryFixture) loadDevice() {
	f.t.Helper()
	computer, ok, err := entryReadComputer(f.paths)
	if err != nil || !ok {
		f.t.Fatal("device key missing", err)
	}
	f.computer = computer
	data, err := os.ReadFile(f.paths.deviceKey())
	if err != nil {
		f.t.Fatal(err)
	}
	f.device, err = ssh.ParsePrivateKey(data)
	if err != nil {
		f.t.Fatal(err)
	}
	signer, err := entryHostSigner(f.s.profile, false)
	if err != nil {
		f.t.Fatal(err)
	}
	f.hostKey = signer.PublicKey()
}

func (f *entryFixture) call(method, path string, input, output any) error {
	return entryCall(context.Background(), f.s.configPath, method, path, input, output)
}

// dial opens the entry the way cage entry proxy does and starts an SSH
// client on it with key.
func (f *entryFixture) dial(key ssh.Signer) (*ssh.Client, error) {
	ws, hostKey, err := entryDial(context.Background(), f.s.configPath, f.computer.ID)
	if err != nil {
		return nil, err
	}
	if hostKey == nil || !entryKeysEqual(hostKey, f.hostKey) {
		ws.Close()
		return nil, fmt.Errorf("host key header %v", hostKey)
	}
	conn := newEntryWSConn(ws)
	config := &ssh.ClientConfig{User: "whoever", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(f.hostKey), Timeout: 10 * time.Second}
	client, channels, requests, err := ssh.NewClientConn(conn, "cage-test", config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(client, channels, requests), nil
}

func (f *entryFixture) client() *ssh.Client {
	f.t.Helper()
	client, err := f.dial(f.device)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { client.Close() })
	return client
}

// entryRun runs a command without a pty and returns its stdout, stderr and
// status. Each env variable is offered first and must be refused.
func entryRun(t *testing.T, client *ssh.Client, command string, stdin []byte, env map[string]string) (string, string, int) {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for name, value := range env {
		if session.Setenv(name, value) == nil {
			t.Fatalf("the endpoint accepted env %s", name)
		}
	}
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	session.Stdin = bytes.NewReader(stdin)
	err = session.Run(command)
	status := 0
	if exit, ok := err.(*ssh.ExitError); ok {
		status = exit.ExitStatus()
	} else if err != nil {
		t.Fatalf("%q: %v (stderr %q)", command, err, stderr.String())
	}
	return stdout.String(), stderr.String(), status
}

// sshEnv is the environment of the system ssh and its ProxyCommand: the
// helper test reads the controller's configuration path and the entry's
// paths from it. HOME points into the test directory.
func (f *entryFixture) sshEnv(extra ...string) []string {
	paths, _ := json.Marshal(map[string]string{"ssh": f.paths.SSHDir, "device": f.paths.DeviceDir, "computer": f.paths.ComputerDir})
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + filepath.Join(f.dir, "client"), "CAGE_ENTRY_TEST_HELPER=1", "CAGE_ENTRY_TEST_CONFIG=" + f.s.configPath, "CAGE_ENTRY_TEST_ENV=" + defaultLocalVMID, "CAGE_ENTRY_TEST_PATHS=" + string(paths)}
	return append(env, extra...)
}

// systemSSH runs the system ssh with a configuration file.
func (f *entryFixture) systemSSH(ctx context.Context, config string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, sshExecutable(), append([]string{"-F", config}, args...)...)
	cmd.Env = f.sshEnv()
	cmd.Dir = f.dir
	return cmd
}

// TestEntryProxyHelper is the ProxyCommand of the generated configuration in
// these tests: the test binary runs entryProxy, the code cage entry proxy
// runs, with the test's configuration path and entry paths.
func TestEntryProxyHelper(t *testing.T) {
	if os.Getenv("CAGE_ENTRY_TEST_HELPER") == "" {
		t.Skip("helper process only")
	}
	var dirs map[string]string
	_ = json.Unmarshal([]byte(os.Getenv("CAGE_ENTRY_TEST_PATHS")), &dirs)
	paths := entryPaths{SSHDir: dirs["ssh"], DeviceDir: dirs["device"], ComputerDir: dirs["computer"]}
	os.Exit(entryProxy(context.Background(), os.Getenv("CAGE_ENTRY_TEST_CONFIG"), os.Getenv("CAGE_ENTRY_TEST_ENV"), paths, os.Stdin, os.Stdout, os.Stderr))
}

// entryAwait waits for cond, or fails after the budget.
func entryAwait(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
