//go:build !windows

package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/cloudproxy"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

// cloudDesktop is a desktop controller connected to a cloud workspace whose
// gateway answers the enrolment.
type cloudDesktop struct {
	s        *Server
	session  *remotehost.Session
	paths    entryPaths
	vm       string
	mu       sync.Mutex
	keys     []string
	revoked  map[string]bool
	host     string
	hostKey  string
	requests int
}

func newCloudDesktop(t *testing.T) *cloudDesktop {
	t.Helper()
	entryRealHomeGuard(t)
	s := entryTestController(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	d := &cloudDesktop{s: s, vm: strings.Repeat("c4", 16), revoked: map[string]bool{}}
	d.host = "vm-" + d.vm + ".portal.example.test:9443"
	_, hostPublic, _ := ed25519.GenerateKey(rand.Reader)
	d.hostKey = cloudentry.AuthorizedKey(hostPublic.Public().(ed25519.PublicKey))
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Name      string
			PublicKey string `json:"public_key"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/client/cloud-entry" || json.NewDecoder(r.Body).Decode(&input) != nil {
			http.NotFound(w, r)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.requests++
		d.keys = append(d.keys, input.PublicKey)
		if d.revoked[input.PublicKey] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"revoked","message":"这台电脑的钥匙已在门户撤销"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"device_id": strings.Repeat("de", 16), "host_key": d.hostKey + " host", "vm_id": d.vm, "host": d.host})
	}))
	t.Cleanup(gateway.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.session = &remotehost.Session{Origin: gateway.URL, Client: gateway.Client(), Context: ctx}
	s.remote = newDesktopGateway(s.profile, nil)
	profile, err := s.remote.store.Upsert(remotehost.HostProfile{Kind: remotehost.TenantKind, Portal: "https://portal.example.test:9443", VMID: d.vm, User: "alice", Name: "云端工作区"})
	if err != nil {
		t.Fatal(err)
	}
	s.remote.desktop.active = &desktopTarget{id: s.remote.desktop.generation, mode: "remote", hostID: profile.ID, ctx: ctx, session: d.session}
	d.paths = entryPaths{
		SSHDir: filepath.Join(dir, "client", ".ssh"), DeviceDir: filepath.Join(dir, "client", "Application Support", "Super Cage", "entry"),
		ComputerDir: s.profile, Proxy: func(environment string) string { return entryProxyCommand("/opt/super-cage/cage", environment) }, Cage: "/opt/super-cage/cage",
	}
	s.entry.paths = &d.paths
	return d
}

func (d *cloudDesktop) call(t *testing.T, method, path string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: d.s.cookieName() + "-desktop", Value: trustedViewProof(d.s.token)})
	w := httptest.NewRecorder()
	answered := d.s.cloudEntryRoute(w, r, d.session)
	return w, answered
}

// Closed: the desktop answers none of the cloud page, the context has no
// cloud row, and nothing is enrolled or written.
func TestCloudEntryDesktopClosed(t *testing.T) {
	d := newCloudDesktop(t)
	for _, path := range []string{"/api/entry", "/api/entry/on"} {
		if _, answered := d.call(t, map[string]string{"/api/entry": "GET", "/api/entry/on": "POST"}[path], path); answered {
			t.Fatalf("%s answered with the gate closed", path)
		}
	}
	if d.s.remote.context().CloudEntry || d.requests != 0 {
		t.Fatal("cloud row or enrolment with the gate closed")
	}
	if _, err := os.Stat(d.paths.SSHDir); !os.IsNotExist(err) {
		t.Fatal("~/.ssh written with the gate closed")
	}
}

// Lifted: on enrols this computer's key through the link, pins the host key
// the gateway relayed, writes the Host block with cage-cloud-entry's command
// and the record the proxy reads; off removes them.
func TestCloudEntryDesktopOnOff(t *testing.T) {
	cloudentry.Lift(t)
	d := newCloudDesktop(t)
	if !d.s.remote.context().CloudEntry {
		t.Fatal("no cloud row")
	}
	w, answered := d.call(t, "GET", "/api/entry")
	var view entryStatusView
	if !answered || w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Enabled || view.State != "off" || view.Environment != "cloud-"+d.vm {
		t.Fatalf("%v %d %s", answered, w.Code, w.Body.String())
	}
	w, _ = d.call(t, "POST", "/api/entry/on")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.Enabled || view.HostAlias == "" || view.Commands == nil || !strings.HasSuffix(view.Commands.CageSSH, " ssh cloud-"+d.vm) {
		t.Fatalf("on: %d %s", w.Code, w.Body.String())
	}
	public, err := os.ReadFile(d.paths.deviceKey() + ".pub")
	if err != nil || len(d.keys) != 1 || strings.TrimSpace(string(public)) != d.keys[0] {
		t.Fatalf("enrolled %v, device key %q", d.keys, public)
	}
	environment := cloudentry.Environment(d.vm)
	block, _ := os.ReadFile(d.paths.hostFile(environment))
	if !strings.Contains(string(block), "ProxyCommand /opt/super-cage/cage entry proxy "+environment+"\n") || !strings.Contains(string(block), "HostKeyAlias cage-"+environment+"\n") {
		t.Fatalf("host block:\n%s", block)
	}
	pinned, err := entryPinnedHostKey(d.paths, environment)
	want, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(d.hostKey))
	if err != nil || !entryKeysEqual(pinned, want) {
		t.Fatal("host key not pinned")
	}
	proxyPaths := entryproxy.Paths{SSHDir: d.paths.SSHDir, DeviceDir: d.paths.DeviceDir}
	record, err := cloudproxy.ReadCloudRecord(proxyPaths, environment)
	if err != nil || record.Portal != "portal.example.test:9443" || record.Device != strings.Repeat("de", 16) || !entryproxy.HostKeyPinned(proxyPaths, environment, d.hostKey) {
		t.Fatalf("%+v %v", record, err)
	}
	// The proxy reads the key Super Cage wrote.
	if _, err := cloudproxy.DeviceSigner(proxyPaths); err != nil {
		t.Fatal(err)
	}
	// The local orphan sweep leaves a cloud block alone.
	if err := entrySweepOrphans(d.paths); err != nil || entryReadAlias(d.paths.hostFile(environment)) == "" {
		t.Fatal("swept")
	}
	w, _ = d.call(t, "POST", "/api/entry/off")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Enabled {
		t.Fatalf("off: %d %s", w.Code, w.Body.String())
	}
	if _, err := cloudproxy.ReadCloudRecord(proxyPaths, environment); err == nil {
		t.Fatal("record left")
	}
	if _, err := os.Stat(d.paths.hostFile(environment)); !os.IsNotExist(err) {
		t.Fatal("host block left")
	}
}

// A key revoked at the portal is replaced, as a locally revoked one is; a
// gateway answer for another workspace writes nothing.
func TestCloudEntryDesktopRevokedAndMismatched(t *testing.T) {
	cloudentry.Lift(t)
	d := newCloudDesktop(t)
	computer, err := entryEnsureComputer(d.paths, false)
	if err != nil {
		t.Fatal(err)
	}
	d.revoked[strings.TrimSpace(computer.Public)] = true
	if w, _ := d.call(t, "POST", "/api/entry/on"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(d.keys) != 2 || d.keys[0] == d.keys[1] {
		t.Fatalf("keys %v", d.keys)
	}
	d2 := newCloudDesktop(t)
	d2.host = "vm-" + strings.Repeat("0f", 16) + ".portal.example.test:9443"
	if w, _ := d2.call(t, "POST", "/api/entry/on"); w.Code == 200 {
		t.Fatal("a gateway answer for another workspace was accepted")
	}
	if _, err := os.Stat(d2.paths.hostFile(cloudentry.Environment(d2.vm))); !os.IsNotExist(err) {
		t.Fatal("host block written for a mismatched answer")
	}
}

// The page's cloud row follows the server's field.
func TestCloudEntryProxyCommand(t *testing.T) {
	dir := t.TempDir()
	cage := filepath.Join(dir, "cage")
	for _, name := range []string{"cage", "cage-attach", cloudproxy.ProgramName()} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := entryProxyCommand(cage, "cloud-"+strings.Repeat("a", 32)); !strings.Contains(got, cloudproxy.ProgramName()+" entry proxy cloud-") {
		t.Fatal(got)
	}
	if got := entryProxyCommand(cage, "default"); !strings.Contains(got, "cage-attach entry proxy default") {
		t.Fatal(got)
	}
	_ = os.Remove(filepath.Join(dir, cloudproxy.ProgramName()))
	if got := entryProxyCommand(cage, "cloud-"+strings.Repeat("a", 32)); strings.Contains(got, "cage-attach") || !strings.Contains(got, "cage entry proxy cloud-") {
		t.Fatal(got)
	}
}
