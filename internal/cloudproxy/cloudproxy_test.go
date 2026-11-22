package cloudproxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
)

// The device key is read without x/crypto/ssh; checked against it here.
func TestDeviceSignerReadsWhatSuperCageWrites(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(private, "super-cage-entry")
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(block)
	key, ok := parseOpenSSHEd25519(data)
	if !ok || !key.Equal(private) || !key.Public().(ed25519.PublicKey).Equal(public) {
		t.Fatal("the device key was not read")
	}
	sshKey, _ := ssh.NewPublicKey(public)
	if cloudentry.AuthorizedKey(public) != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshKey))) {
		t.Fatal("authorized key forms differ")
	}
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(private, "x", []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parseOpenSSHEd25519(pem.EncodeToMemory(encrypted)); ok {
		t.Fatal("an encrypted key was read")
	}
	for _, bad := range [][]byte{nil, []byte("garbage"), bytes.Replace(data, []byte("OPENSSH"), []byte("RSA"), 2), data[:len(data)/2]} {
		if _, ok := parseOpenSSHEd25519(bad); ok {
			t.Errorf("read %q", bad)
		}
	}
}

type cloudFixture struct {
	t        *testing.T
	paths    entryproxy.Paths
	key      ed25519.PrivateKey
	record   CloudRecord
	env      string
	server   *httptest.Server
	mu       sync.Mutex
	seen     []http.Header
	answer   func(w http.ResponseWriter, r *http.Request) bool
	hostKey  string
	dials    int
	verified bool
}

func newCloudFixture(t *testing.T) *cloudFixture {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	f := &cloudFixture{t: t, paths: entryproxy.Paths{SSHDir: filepath.Join(dir, "ssh"), DeviceDir: filepath.Join(dir, "device")}, key: private}
	block, _ := ssh.MarshalPrivateKey(private, "super-cage-entry")
	mustWrite(t, filepath.Join(f.paths.DeviceDir, entryproxy.DeviceKey), pem.EncodeToMemory(block))
	mustWrite(t, filepath.Join(f.paths.DeviceDir, entryproxy.DeviceKey+".pub"), []byte(cloudentry.AuthorizedKey(public)+"\n"))
	vm := strings.Repeat("ab", 16)
	f.env = cloudentry.Environment(vm)
	f.record = CloudRecord{Version: 1, Portal: "portal.example.test:9443", VM: vm, Device: strings.Repeat("0d", 16)}
	data, _ := json.Marshal(f.record)
	mustWrite(t, RecordPath(f.paths, f.env), data)
	_, hostPublic, _ := ed25519.GenerateKey(rand.Reader)
	f.hostKey = cloudentry.AuthorizedKey(hostPublic.Public().(ed25519.PublicKey))
	mustWrite(t, f.paths.KnownHosts(f.env), []byte("cage-"+f.env+" "+f.hostKey+"\n"))
	host := "vm-" + vm + ".portal.example.test:9443"
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Header.Clone())
		f.dials++
		answer := f.answer
		f.mu.Unlock()
		auth, ok := cloudentry.Parse(r.Header.Get("Authorization"))
		if r.URL.Path != cloudentry.Path || r.Host != host || !ok || auth.Device != f.record.Device || !auth.Fresh(time.Now()) || !auth.Verify(public, host, vm) || r.Header.Get("Origin") != "" {
			http.Error(w, "refused", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.verified = true
		f.mu.Unlock()
		if answer != nil && answer(w, r) {
			return
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, http.Header{cloudentry.HostKeyHeader: {f.hostKey}})
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil || conn.WriteMessage(kind, append([]byte("echo:"), data...)) != nil {
				return
			}
		}
	}))
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(f.server.Certificate())
	address := f.server.Listener.Addr().String()
	saved := cloudDial
	cloudDial = func(ctx context.Context, dialled, serverName string) (net.Conn, error) {
		if dialled != host || serverName != "vm-"+vm+".portal.example.test" {
			t.Errorf("dialled %q as %q", dialled, serverName)
		}
		return tlsDial(ctx, address, "example.com", roots)
	}
	t.Cleanup(func() { cloudDial = saved })
	return f
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *cloudFixture) run(stdin io.Reader) (int, string, string) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := Run(ctx, f.paths, f.env, stdin, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// In a build without the cloud entry, cloud mode says so and dials nothing.
func TestCloudModeClosed(t *testing.T) {
	f := newCloudFixture(t)
	code, _, stderr := f.run(strings.NewReader(""))
	if code != entryproxy.Failure || !strings.Contains(stderr, cloudentry.ClosedMessage) || f.dials != 0 {
		t.Fatalf("%d %q, %d dials", code, stderr, f.dials)
	}
	var out, errs bytes.Buffer
	if code := Main(context.Background(), []string{"entry", "proxy", f.env}, strings.NewReader(""), &out, &errs); code != entryproxy.Failure || !strings.Contains(errs.String(), cloudentry.ClosedMessage) {
		t.Fatalf("Main: %d %q", code, errs.String())
	}
	// cage-attach answers a cloud environment without TLS, closed or not.
	errs.Reset()
	if code := entryproxy.Main(context.Background(), []string{"proxy", f.env}, filepath.Join(t.TempDir(), "config.json"), nil, strings.NewReader(""), &out, &errs); code != entryproxy.Failure || !strings.Contains(errs.String(), cloudentry.ClosedMessage) {
		t.Fatalf("cage-attach: %d %q", code, errs.String())
	}
}

// Lifted: a signed admission over TLS, the pinned host key, then ssh's
// bytes both ways. No Origin, no cookie; Cage-Entry-Wake only when the
// ssh is interactive.
func TestCloudModeRelays(t *testing.T) {
	cloudentry.Lift(t)
	for _, terminal := range []bool{false, true} {
		f := newCloudFixture(t)
		saved := interactive
		interactive = func() bool { return terminal }
		code, stdout, stderr := f.run(strings.NewReader("SSH-2.0-OpenSSH_9.9\r\n"))
		interactive = saved
		if code != 0 || stdout != "echo:SSH-2.0-OpenSSH_9.9\r\n" || stderr != "" || !f.verified {
			t.Fatalf("%d %q %q", code, stdout, stderr)
		}
		seen := f.seen[0]
		if (seen.Get(cloudentry.HeaderWake) == "1") != terminal || seen.Get("Cookie") != "" || seen.Get(cloudentry.HeaderDevice) != "" {
			t.Fatalf("interactive %v: headers %v", terminal, seen)
		}
	}
}

// A host key other than the pinned one is refused before a byte is relayed,
// and the pin is not rewritten.
func TestCloudModeRefusesAnotherHostKey(t *testing.T) {
	cloudentry.Lift(t)
	f := newCloudFixture(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	pinned, _ := os.ReadFile(f.paths.KnownHosts(f.env))
	f.hostKey = cloudentry.AuthorizedKey(other.Public().(ed25519.PublicKey))
	code, stdout, stderr := f.run(strings.NewReader("secret"))
	after, _ := os.ReadFile(f.paths.KnownHosts(f.env))
	if code != entryproxy.Failure || stdout != "" || !strings.Contains(stderr, ErrCloudHostKeyChanged) || !bytes.Equal(pinned, after) {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
}

// The gateway's refusals reach the owner in its words; an interactive dial
// waits for a workspace it started.
func TestCloudModeRefusalsAndStart(t *testing.T) {
	cloudentry.Lift(t)
	f := newCloudFixture(t)
	f.answer = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"asleep","message":"工作区已休眠：在终端里直接用 ssh 连接会唤醒它"}`)
		return true
	}
	if code, _, stderr := f.run(strings.NewReader("")); code != entryproxy.Failure || !strings.Contains(stderr, "工作区已休眠") {
		t.Fatalf("%d %q", code, stderr)
	}
	starting := 2
	f.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if starting == 0 {
			return false
		}
		starting--
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"code":"starting","message":"工作区正在启动，稍后再连接"}`)
		return true
	}
	savedRetry, savedInteractive := cloudRetry, interactive
	cloudRetry, interactive = 10*time.Millisecond, func() bool { return true }
	t.Cleanup(func() { cloudRetry, interactive = savedRetry, savedInteractive })
	code, stdout, stderr := f.run(strings.NewReader("x"))
	if code != 0 || stdout != "echo:x" || strings.Count(stderr, "工作区正在启动") != 1 {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	// A non-interactive dial does not wait.
	starting = 1
	interactive = func() bool { return false }
	if code, _, stderr := f.run(strings.NewReader("")); code != entryproxy.Failure || !strings.Contains(stderr, "工作区正在启动") {
		t.Fatalf("%d %q", code, stderr)
	}
	// A portal without the cloud entry answers 404.
	f.answer = func(w http.ResponseWriter, r *http.Request) bool {
		http.NotFound(w, r)
		return true
	}
	if code, _, stderr := f.run(strings.NewReader("")); code != entryproxy.Failure || !strings.Contains(stderr, "这个门户还没有开放本机终端") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestCloudRecords(t *testing.T) {
	cloudentry.Lift(t)
	f := newCloudFixture(t)
	if _, err := ReadCloudRecord(f.paths, f.env); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"default", "cloud-xyz", cloudentry.Environment(strings.Repeat("cd", 16))} {
		if _, err := ReadCloudRecord(f.paths, env); err == nil {
			t.Errorf("%q read", env)
		}
	}
	for host, ok := range map[string]bool{
		"portal.example": true, "portal.example:9443": true, "portal.example:443": false, "Portal.example": false,
		"portal": false, "portal.example:0": false, "portal.example:99999x": false, "-.example": false, "a-b.example": true, "a..b": false, "evil.example/x": false, "": false,
	} {
		if validPortalHost(host) != ok {
			t.Errorf("%q: %v", host, !ok)
		}
	}
}
