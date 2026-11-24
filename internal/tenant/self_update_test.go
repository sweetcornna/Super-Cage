package tenant

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func buildFakeLinuxELF(arch string, payload string) []byte {
	var hdr [64]byte
	copy(hdr[:4], []byte{0x7f, 'E', 'L', 'F'})
	hdr[4] = 2 // 64-bit
	hdr[5] = 1 // little-endian
	switch arch {
	case "amd64":
		binary.LittleEndian.PutUint16(hdr[18:20], 62)
	case "arm64":
		binary.LittleEndian.PutUint16(hdr[18:20], 183)
	}
	return append(hdr[:], []byte(payload)...)
}

func buildTestServerBundle(t *testing.T, ver, arch string, extraEntries map[string]string) ([]byte, string) {
	t.Helper()
	prefix := "Super-Cage-NoVirt-" + ver + "-linux-" + arch + "/"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string][]byte{
		prefix + "cage-tenant":          buildFakeLinuxELF(arch, "gateway-"+ver),
		prefix + "cage":                 buildFakeLinuxELF(arch, "cage-"+ver),
		prefix + "ui/tenant/index.html": []byte("<!doctype html><title>v" + ver + "</title>"),
		prefix + "ui/tenant/app.mjs":    []byte("// portal v" + ver + "\n"),
		prefix + "ui/tenant/styles.css": []byte("/* styles v" + ver + " */\n"),
	}
	for k, v := range extraEntries {
		files[k] = []byte(v)
	}
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

type fakeWorkspaceUpdater struct {
	mu      sync.Mutex
	calls   []string
	failID  string
	skipID  string
	failErr error
}

func (f *fakeWorkspaceUpdater) UpdateWorkspace(_ context.Context, id, _ string, targetVersion string, logf func(string)) (bool, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, id+":"+targetVersion)
	failID, skipID, failErr := f.failID, f.skipID, f.failErr
	f.mu.Unlock()
	logf("mock workspace update " + id)
	if id == failID {
		return false, "", failErr
	}
	if id == skipID {
		return true, "实例未运行", nil
	}
	return false, "已更新至 v" + targetVersion, nil
}

func TestSelfUpdateConfigValidationAndAuthorization(t *testing.T) {
	base := newFixture(t, false)
	base.server.Close()

	// Enabled without admin username must be rejected.
	badCfg := base.config
	badCfg.StateDir = filepath.Join(t.TempDir(), "state")
	badCfg.SelfUpdate = SelfUpdateConfig{
		Enabled:     true,
		GatewayPath: filepath.Join(t.TempDir(), "cage-tenant"),
		UIDir:       t.TempDir(),
	}
	if _, err := NewServer(badCfg); err == nil {
		t.Fatal("accepted self-update without admins")
	}

	// Enabled with admin "alice": alice sees self_update=true, bob gets 403.
	uiDir := t.TempDir()
	for _, f := range []string{"index.html", "app.mjs", "styles.css"} {
		writeTestFile(t, filepath.Join(uiDir, f), "old-"+f, 0o644)
	}
	gwPath := writeTestFile(t, filepath.Join(t.TempDir(), "cage-tenant"), "old-gateway", 0o755)
	base.config.SelfUpdate = SelfUpdateConfig{
		Enabled:        true,
		Admins:         []string{"alice"},
		GatewayPath:    gwPath,
		UIDir:          uiDir,
		TargetArch:     "amd64",
		CurrentVersion: "0.1.19",
	}
	base.openServer()
	alice, _ := base.login("register", "alice")
	bob, _ := base.login("register", "bob")

	// Session exposes self_update only for admin.
	aliceSession := base.request("GET", portalOrigin, "/api/tenant/session", "", alice)
	wantStatus(t, aliceSession, http.StatusOK, "")
	if !strings.Contains(aliceSession.Body.String(), `"self_update":true`) {
		t.Fatalf("admin session missing self_update: %s", aliceSession.Body.String())
	}
	bobSession := base.request("GET", portalOrigin, "/api/tenant/session", "", bob)
	wantStatus(t, bobSession, http.StatusOK, "")
	if strings.Contains(bobSession.Body.String(), "self_update") {
		t.Fatalf("non-admin session exposed self_update: %s", bobSession.Body.String())
	}

	// Non-admin cannot access GET/POST /api/tenant/update*.
	wantStatus(t, base.request("GET", portalOrigin, "/api/tenant/update", "", bob), http.StatusForbidden, "forbidden")
	wantStatus(t, base.request("POST", portalOrigin, "/api/tenant/update/check", "{}", bob), http.StatusForbidden, "forbidden")
	wantStatus(t, base.request("POST", portalOrigin, "/api/tenant/update/apply", "{}", bob), http.StatusForbidden, "forbidden")

	// Admin can read current status.
	statusResp := base.request("GET", portalOrigin, "/api/tenant/update", "", alice)
	wantStatus(t, statusResp, http.StatusOK, "")
	var status SelfUpdateStatus
	if err := json.Unmarshal(statusResp.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.CurrentVersion != "0.1.19" || status.Arch != "amd64" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestSelfUpdateEndToEndCheckApplyAndVerify(t *testing.T) {
	bundleBytes, bundleSHA := buildTestServerBundle(t, "0.1.20", "amd64", nil)
	checksumsBody := bundleSHA + "  Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz\n"

	var releaseServer *httptest.Server
	releaseServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			jsonReply(w, http.StatusOK, []map[string]any{
				{
					"tag_name":     "v0.1.20",
					"body":         "Permanent startup fix and web self-update",
					"html_url":     releaseServer.URL + "/tag/v0.1.20",
					"published_at": "2026-11-24T05:00:00Z",
					"assets": []map[string]any{
						{
							"name":                 "Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz",
							"browser_download_url": releaseServer.URL + "/download/Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz",
							"size":                 len(bundleBytes),
							"digest":               "sha256:" + bundleSHA,
							"state":                "uploaded",
						},
						{
							"name":                 "SHA256SUMS",
							"browser_download_url": releaseServer.URL + "/download/SHA256SUMS",
							"size":                 len(checksumsBody),
							"state":                "uploaded",
						},
					},
				},
			})
		case "/download/SHA256SUMS":
			_, _ = w.Write([]byte(checksumsBody))
		case "/download/Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz":
			_, _ = w.Write(bundleBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	base := newFixture(t, false)
	base.server.Close()

	uiDir := t.TempDir()
	for _, f := range []string{"index.html", "app.mjs", "styles.css"} {
		writeTestFile(t, filepath.Join(uiDir, f), "old-"+f, 0o644)
	}
	gwDir := t.TempDir()
	gwPath := writeTestFile(t, filepath.Join(gwDir, "cage-tenant"), "old-gateway", 0o755)
	wsUpdater := &fakeWorkspaceUpdater{}
	restarted := make(chan string, 1)

	base.config.SelfUpdate = SelfUpdateConfig{
		Enabled:          true,
		Admins:           []string{"alice"},
		GatewayPath:      gwPath,
		UIDir:            uiDir,
		ReleasesURL:      releaseServer.URL + "/releases",
		TargetArch:       "amd64",
		CurrentVersion:   "0.1.19",
		HTTPClient:       releaseServer.Client(),
		AllowInsecureURL: true,
		RestartDelay:     10 * time.Millisecond,
		WorkspaceUpdater: wsUpdater,
		VerifyStaged: func(_ context.Context, stageDir, targetVersion string) error {
			if err := verifyLinuxELFArch(filepath.Join(stageDir, "cage-tenant"), "amd64"); err != nil {
				return err
			}
			if err := verifyLinuxELFArch(filepath.Join(stageDir, "cage"), "amd64"); err != nil {
				return err
			}
			return nil
		},
		OnRestart: func(path string) { restarted <- path },
	}
	base.openServer()
	alice, _ := base.login("register", "alice")
	vmA := base.create(alice, "dev-vm")

	// Check update.
	checkResp := base.request("POST", portalOrigin, "/api/tenant/update/check", "{}", alice)
	wantStatus(t, checkResp, http.StatusOK, "")
	var checked SelfUpdateStatus
	if err := json.Unmarshal(checkResp.Body.Bytes(), &checked); err != nil {
		t.Fatal(err)
	}
	if !checked.UpdateAvailable || checked.LatestVersion != "0.1.20" {
		t.Fatalf("expected 0.1.20 available, got %+v", checked)
	}

	// Apply update.
	applyResp := base.request("POST", portalOrigin, "/api/tenant/update/apply", `{"version":"0.1.20"}`, alice)
	wantStatus(t, applyResp, http.StatusAccepted, "")

	select {
	case gotPath := <-restarted:
		if gotPath != gwPath {
			t.Fatalf("OnRestart got %q, want %q", gotPath, gwPath)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for self-update restart; status=%+v", base.server.selfUpdate.statusSnapshot())
	}

	// Verify workspace was updated, UI was swapped, gateway was swapped, and post-update grace file was written.
	wsUpdater.mu.Lock()
	calls := append([]string(nil), wsUpdater.calls...)
	wsUpdater.mu.Unlock()
	if len(calls) != 1 || calls[0] != vmA.ID+":0.1.20" {
		t.Fatalf("unexpected workspace updates: %v", calls)
	}
	if data, err := os.ReadFile(filepath.Join(uiDir, "index.html")); err != nil || !strings.Contains(string(data), "v0.1.20") {
		t.Fatalf("UI not updated: %q, err=%v", string(data), err)
	}
	if data, err := os.ReadFile(gwPath); err != nil || !strings.Contains(string(data), "gateway-0.1.20") {
		t.Fatalf("gateway binary not updated: %q, err=%v", string(data), err)
	}
	if grace := readPostUpdateGrace(base.config.StateDir, time.Now()); grace.IsZero() {
		t.Fatal("post-update memory grace file was not written")
	}
}

func TestSelfUpdateRejectsTamperedSHA256AndLeavesLiveFilesUntouched(t *testing.T) {
	bundleBytes, _ := buildTestServerBundle(t, "0.1.20", "amd64", nil)
	badSHA := strings.Repeat("0", 64)
	checksumsBody := badSHA + "  Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz\n"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			jsonReply(w, http.StatusOK, []map[string]any{{
				"tag_name":     "v0.1.20",
				"published_at": "2026-11-24T05:00:00Z",
				"assets": []map[string]any{
					{"name": "Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz", "browser_download_url": srv.URL + "/dl/bundle", "size": len(bundleBytes), "state": "uploaded"},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/sums", "size": len(checksumsBody), "state": "uploaded"},
				},
			}})
		case "/dl/sums":
			_, _ = w.Write([]byte(checksumsBody))
		case "/dl/bundle":
			_, _ = w.Write(bundleBytes)
		}
	}))
	defer srv.Close()

	base := newFixture(t, false)
	base.server.Close()
	uiDir := t.TempDir()
	for _, f := range []string{"index.html", "app.mjs", "styles.css"} {
		writeTestFile(t, filepath.Join(uiDir, f), "old-"+f, 0o644)
	}
	gwPath := writeTestFile(t, filepath.Join(t.TempDir(), "cage-tenant"), "old-gateway", 0o755)
	wsUpdater := &fakeWorkspaceUpdater{}
	base.config.SelfUpdate = SelfUpdateConfig{
		Enabled:          true,
		Admins:           []string{"alice"},
		GatewayPath:      gwPath,
		UIDir:            uiDir,
		ReleasesURL:      srv.URL + "/releases",
		TargetArch:       "amd64",
		CurrentVersion:   "0.1.19",
		HTTPClient:       srv.Client(),
		AllowInsecureURL: true,
		WorkspaceUpdater: wsUpdater,
	}
	base.openServer()
	alice, _ := base.login("register", "alice")
	wantStatus(t, base.request("POST", portalOrigin, "/api/tenant/update/apply", "{}", alice), http.StatusAccepted, "")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := base.server.selfUpdate.statusSnapshot()
		if !st.Job.Running {
			if st.Job.Phase != "error" || !strings.Contains(st.Job.Error, "SHA-256") {
				t.Fatalf("expected SHA-256 failure, got %+v", st.Job)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if data, _ := os.ReadFile(gwPath); string(data) != "old-gateway" {
		t.Fatalf("gateway modified despite SHA mismatch: %q", string(data))
	}
	if len(wsUpdater.calls) != 0 {
		t.Fatalf("workspace updated despite SHA mismatch: %v", wsUpdater.calls)
	}
}

func TestSelfUpdateAbortsGatewaySwapWhenWorkspaceFails(t *testing.T) {
	bundleBytes, bundleSHA := buildTestServerBundle(t, "0.1.20", "amd64", nil)
	checksumsBody := bundleSHA + "  Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			jsonReply(w, http.StatusOK, []map[string]any{{
				"tag_name":     "v0.1.20",
				"published_at": "2026-11-24T05:00:00Z",
				"assets": []map[string]any{
					{"name": "Super-Cage-NoVirt-0.1.20-linux-amd64.tar.gz", "browser_download_url": srv.URL + "/dl/bundle", "size": len(bundleBytes), "state": "uploaded"},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/sums", "size": len(checksumsBody), "state": "uploaded"},
				},
			}})
		case "/dl/sums":
			_, _ = w.Write([]byte(checksumsBody))
		case "/dl/bundle":
			_, _ = w.Write(bundleBytes)
		}
	}))
	defer srv.Close()

	base := newFixture(t, false)
	base.server.Close()
	uiDir := t.TempDir()
	for _, f := range []string{"index.html", "app.mjs", "styles.css"} {
		writeTestFile(t, filepath.Join(uiDir, f), "old-"+f, 0o644)
	}
	gwPath := writeTestFile(t, filepath.Join(t.TempDir(), "cage-tenant"), "old-gateway", 0o755)
	wsUpdater := &fakeWorkspaceUpdater{failErr: errors.New("verify-vm-assets failed")}
	base.config.SelfUpdate = SelfUpdateConfig{
		Enabled:          true,
		Admins:           []string{"alice"},
		GatewayPath:      gwPath,
		UIDir:            uiDir,
		ReleasesURL:      srv.URL + "/releases",
		TargetArch:       "amd64",
		CurrentVersion:   "0.1.19",
		HTTPClient:       srv.Client(),
		AllowInsecureURL: true,
		WorkspaceUpdater: wsUpdater,
		VerifyStaged:     func(context.Context, string, string) error { return nil },
	}
	base.openServer()
	alice, _ := base.login("register", "alice")
	vmA := base.create(alice, "prod-vm")
	wsUpdater.failID = vmA.ID

	wantStatus(t, base.request("POST", portalOrigin, "/api/tenant/update/apply", "{}", alice), http.StatusAccepted, "")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := base.server.selfUpdate.statusSnapshot()
		if !st.Job.Running {
			if st.Job.Phase != "error" || !strings.Contains(st.Job.Error, "verify-vm-assets") {
				t.Fatalf("expected workspace failure, got %+v", st.Job)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if data, _ := os.ReadFile(gwPath); string(data) != "old-gateway" {
		t.Fatalf("gateway swapped despite workspace failure")
	}
}

func TestPostUpdateGraceHoldsElasticShrinkWhileAllowingGrowth(t *testing.T) {
	base := newFixture(t, false)
	base.server.Close()
	if err := writePostUpdateGrace(base.config.StateDir, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := &memoryRuntime{fakeRuntime: base.runtime, elastic: true}
	base.config.Runtime, base.config.MemoryInterval = runtime, 10*time.Minute
	base.openServer()
	login, _ := base.login("register", "alice")
	record := base.create(login, "post-update")
	base.connect(login, record.ID)

	// Host wants to shrink this idle VM, but post-update grace must hold shrinking.
	runtime.state = MemoryState{
		HostTotal: 32 * testGiB, HostAvailable: 20 * testGiB, Ceiling: 10 * testGiB,
		Instances: map[string]InstanceMemory{
			record.ID: stoppedVM(8 * testGiB),
		},
	}
	base.server.balanceOnce()
	if changes := runtime.changes(); len(changes) != 0 {
		t.Fatalf("shrank VM during post-update grace: %+v", changes)
	}

	// Growth for a starting nested guest is STILL honored immediately during grace.
	entry := stoppedVM(2 * testGiB)
	entry.NestedShort, entry.NestedPressure, entry.NestedStart = 6*testGiB, true, true
	runtime.mu.Lock()
	runtime.state.Instances[record.ID] = entry
	runtime.mu.Unlock()
	base.server.balanceOnce()
	if changes := runtime.changes(); len(changes) != 1 || changes[0].to <= 2*testGiB {
		t.Fatalf("expected immediate growth during post-update grace, got %+v", changes)
	}
}

func TestExtractServerBundleRejectsTraversalAndWrongArch(t *testing.T) {
	badTar, _ := buildTestServerBundle(t, "0.1.20", "amd64", map[string]string{"../escape": "evil"})
	badPath := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(badPath, badTar, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractServerBundle(badPath, t.TempDir()); err == nil {
		t.Fatal("accepted path traversal in tarball")
	}

	arm64Tar, _ := buildTestServerBundle(t, "0.1.20", "arm64", nil)
	armPath := filepath.Join(t.TempDir(), "arm.tar.gz")
	if err := os.WriteFile(armPath, arm64Tar, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := extractServerBundle(armPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyLinuxELFArch(staged.cagePath, "amd64"); err == nil {
		t.Fatal("accepted arm64 ELF for amd64 host")
	}
}
