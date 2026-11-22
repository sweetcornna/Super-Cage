package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

type gatewayLocalFixture struct {
	endpoint                         *httptest.Server
	connections, suspended, commands atomic.Int32
	status                           DesktopLocalStatus
	release                          func(DesktopLocalStatus, bool) error
	owned                            bool
}

func gatewayFixture(t *testing.T) (*Server, *gatewayLocalFixture) {
	t.Helper()
	s := testServer(t)
	f := &gatewayLocalFixture{status: DesktopLocalStatus{Known: true, Available: true}}
	f.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/suspend":
			var input map[string]any
			if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid account request", 400)
				return
			}
			f.suspended.Add(1)
		default:
			f.commands.Add(1)
		}
		_, _ = w.Write([]byte(`{"upstream":"local-controller"}`))
	}))
	t.Cleanup(f.endpoint.Close)
	s.remote = newDesktopGateway(s.profile, f)
	t.Cleanup(s.remote.close)
	return s, f
}

func (f *gatewayLocalFixture) Connect(ctx context.Context) (*remotehost.Session, error) {
	f.connections.Add(1)
	return &remotehost.Session{Origin: f.endpoint.URL, Client: f.endpoint.Client(), Transport: f.endpoint.Client().Transport, Context: ctx, Manifest: remotehost.PublicManifest{Platform: "linux", InstanceID: strings.Repeat("a", 64)}}, nil
}
func (f *gatewayLocalFixture) Status(context.Context) (DesktopLocalStatus, error) {
	return f.status, nil
}
func (f *gatewayLocalFixture) Release(_ context.Context, expected DesktopLocalStatus, stop bool) error {
	if f.release == nil {
		return errors.New("unexpected release")
	}
	return f.release(expected, stop)
}

func (f *gatewayLocalFixture) ReleaseOwnedIdle(context.Context, DesktopLocalStatus) (bool, error) {
	return f.owned, nil
}

func candidateState(t *testing.T, d *desktopGateway, want string) *desktopCandidate {
	t.Helper()
	deadline := time.Now().Add(time.Second * 5)
	for time.Now().Before(deadline) {
		c := d.context().Candidate
		if c != nil && c.Connection.State == want {
			return c
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatalf("candidate did not reach %s: %+v", want, d.context().Candidate)
	return nil
}

func gatewayRequest(s *Server, path, generation string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", s.origin+path, nil)
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	if generation != "" {
		r.Header.Set(connectionHeader, generation)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func chooseLocal(t *testing.T, d *desktopGateway, id string) string {
	t.Helper()
	c, e := d.prepare(switchRequest{ConnectionID: id, Mode: "local"})
	if e != nil {
		t.Fatal(e)
	}
	candidateState(t, d, "connected")
	if e = d.commit(context.Background(), switchRequest{ConnectionID: id, CandidateID: c.ID}); e != nil {
		t.Fatal(e)
	}
	return c.ID
}

func TestDesktopCandidateFailurePreservesLiveViewAndStaleLocalIDs(t *testing.T) {
	s, f := gatewayFixture(t)
	d := s.remote.desktop
	none := d.context().ConnectionID
	if c := d.context(); c.Mode != "none" || c.ConnectionID == "" || f.connections.Load() != 0 {
		t.Fatal("cold shell started a local controller", c)
	}
	if got := gatewayRequest(s, "/api/account", none); got.Code != 409 {
		t.Fatal("unselected view exposed a local business API", got.Code)
	}
	local := chooseLocal(t, d, none)
	if got := gatewayRequest(s, "/api/status", local); got.Code != 200 || !strings.Contains(got.Body.String(), "local-controller") {
		t.Fatal("selected view did not go to the local controller", got.Code, got.Body.String())
	}
	// The SSH connector fails on our nonexistent fixture key, before network access.
	host, err := s.remote.store.Upsert(remotehost.HostProfile{Name: "fixture", Host: "127.0.0.1", Port: 22, User: "cage", Auth: remotehost.AuthConfig{Kind: "key_file", IdentityFile: filepath.Join(t.TempDir(), "absent")}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := d.prepare(switchRequest{ConnectionID: local, Mode: "remote", HostID: host.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidateState(t, d, "failed")
	if got := gatewayRequest(s, "/api/status", remote.ID); got.Code != 409 {
		t.Fatal("uncommitted target dispatched a business API", got.Code)
	}
	if err = d.commit(context.Background(), switchRequest{ConnectionID: local, CandidateID: remote.ID, LocalAction: "stop"}); err == nil {
		t.Fatal("failed candidate could stop or replace the current view")
	}
	if got := gatewayRequest(s, "/api/status", local); got.Code != 200 || f.commands.Load() != 2 || f.suspended.Load() != 0 {
		t.Fatal("candidate failure affected the live view")
	}
	if err = d.cancelCandidate(switchRequest{ConnectionID: local, CandidateID: remote.ID}); err != nil {
		t.Fatal(err)
	}
	if err = d.disconnect(context.Background(), switchRequest{ConnectionID: local}); err != nil {
		t.Fatal(err)
	}
	second := chooseLocal(t, d, d.context().ConnectionID)
	if second == local || f.suspended.Load() != 1 {
		t.Fatal("local reconnection reused a generation or failed to suspend the old view")
	}
	for _, stale := range []string{"", none, "local", local, remote.ID} {
		if got := gatewayRequest(s, "/api/status", stale); got.Code != 409 {
			t.Fatalf("obsolete ID %q reached local runtime, code %d", stale, got.Code)
		}
	}
	if got := gatewayRequest(s, "/api/status", second); got.Code != 200 {
		t.Fatal("current connection rejected", got.Code)
	}
}

func TestDesktopRemoteCommitRequiresDecisionBeforeReleasingObservedLocal(t *testing.T) {
	s, f := gatewayFixture(t)
	d := s.remote.desktop
	original := chooseLocal(t, d, d.context().ConnectionID)
	f.status = DesktopLocalStatus{Available: true, Known: true, Running: true, ServiceRunning: true, LiveTerminals: 1, ControllerID: "current-controller"}
	// Supply a completed transport here; SSH key authentication and forwarding
	// are tested by remotehost's real SSH fixture. This test owns the commit race.
	ctx, cancel := context.WithCancel(context.Background())
	session, err := f.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate := &desktopTarget{id: "remote-generation", mode: "remote", ctx: ctx, cancel: cancel, session: session}
	d.mu.Lock()
	d.pending = candidate
	d.mu.Unlock()
	in := switchRequest{ConnectionID: original, CandidateID: candidate.id, LocalAction: "auto"}
	if err := d.commit(context.Background(), in); err == nil {
		t.Fatal("auto released a busy local controller")
	}
	in.LocalAction = "stop"
	in.LocalControllerID = "obsolete-controller"
	if err := d.commit(context.Background(), in); err == nil {
		t.Fatal("obsolete controller ID stopped the replacement")
	}
	in.LocalControllerID = f.status.ControllerID
	f.release = func(observed DesktopLocalStatus, stop bool) error {
		if !stop || observed.ControllerID != f.status.ControllerID {
			t.Fatal("wrong stop target")
		}
		if got := gatewayRequest(s, "/api/status", original); got.Code != http.StatusConflict {
			t.Fatal("another window admitted a business request while releasing the local controller")
		}
		return errDesktopLocalLocked
	}
	if err := d.commit(context.Background(), in); !errors.Is(err, errDesktopLocalLocked) {
		t.Fatalf("locked account result %v", err)
	}
	if d.context().ConnectionID != original || f.suspended.Load() != 0 {
		t.Fatal("failed stop changed current selection")
	}
	if got := gatewayRequest(s, "/api/status", original); got.Code != http.StatusOK {
		t.Fatal("failed stop left admission frozen for the retained local view")
	}
	in.LocalAction = "keep"
	if err := d.commit(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if now := d.context(); now.Mode != "remote" || now.ConnectionID != candidate.id || now.Candidate != nil || f.suspended.Load() != 1 {
		t.Fatalf("incorrect committed view or unsuspended previous view: %+v", now)
	}
	if got := gatewayRequest(s, "/api/status", original); got.Code != 409 {
		t.Fatal("old local request reached selected remote")
	}
	if err := d.disconnect(context.Background(), switchRequest{ConnectionID: original}); err == nil {
		t.Fatal("stale disconnect closed remote selection")
	}
}

func TestRemoteToRemoteSwitchHonorsBackgroundLocalDecision(t *testing.T) {
	for _, action := range []string{"keep", "stop"} {
		t.Run(action, func(t *testing.T) {
			s, local := gatewayFixture(t)
			d := s.remote.desktop
			local.status = DesktopLocalStatus{Known: true, Available: true, ServiceRunning: true, Running: true, LiveTerminals: 1, ControllerID: "background-controller"}
			// Use already authenticated views to isolate the decision made after
			// B's handshake while A is still the selected workbench.
			remoteView := func(id string) *desktopTarget {
				ctx, cancel := context.WithCancel(context.Background())
				session, err := local.Connect(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return &desktopTarget{id: id, mode: "remote", ctx: ctx, cancel: cancel, session: session}
			}
			a, b := remoteView("remote-A"), remoteView("remote-B")
			d.mu.Lock()
			d.generation, d.active, d.pending = a.id, a, b
			d.mu.Unlock()
			request := switchRequest{ConnectionID: a.id, CandidateID: b.id, LocalAction: "auto"}
			if err := d.commit(context.Background(), request); err == nil {
				t.Fatal("background local work did not require an explicit choice")
			}
			if got := gatewayRequest(s, "/api/status", a.id); got.Code != http.StatusOK {
				t.Fatal("A became unavailable while waiting for the local decision")
			}
			request.LocalAction, request.LocalControllerID = action, local.status.ControllerID
			stops := 0
			local.release = func(expected DesktopLocalStatus, stop bool) error {
				stops++
				if !stop || expected.ControllerID != local.status.ControllerID {
					t.Fatal("stopped the wrong controller")
				}
				if got := gatewayRequest(s, "/api/status", a.id); got.Code != http.StatusOK {
					t.Fatal("A was released before the local stop completed")
				}
				return nil
			}
			if err := d.commit(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if (action == "stop" && stops != 1) || (action == "keep" && stops != 0) || local.suspended.Load() != 0 {
				t.Fatalf("incorrect workload action: stop count %d, account suspensions %d", stops, local.suspended.Load())
			}
			if d.context().ConnectionID != b.id || a.ctx.Err() == nil {
				t.Fatal("successful commit did not switch exclusively to B")
			}
		})
	}
}

func TestRemoteAutoReleasesOnlyOwnedLocalService(t *testing.T) {
	for _, tc := range []struct {
		name             string
		localView, owned bool
		wantSuspend      int32
	}{
		{"external-service-with-no-local-view", false, false, 0},
		{"external-service-with-local-view", true, false, 1},
		{"owned-service-with-local-view", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := gatewayFixture(t)
			d := s.remote.desktop
			f.status = DesktopLocalStatus{Known: true, Available: true, ServiceRunning: true, ControllerID: "idle-service"}
			f.owned = tc.owned
			if tc.localView {
				chooseLocal(t, d, d.context().ConnectionID)
			}
			err := d.leaveLocal(context.Background(), switchRequest{LocalAction: "auto"}, d.active)
			if err != nil || f.suspended.Load() != tc.wantSuspend {
				t.Fatalf("auto used the wrong owner or directly stopped a controller: err=%v suspensions=%d", err, f.suspended.Load())
			}
		})
	}
}

// Quitting the app suspends the local view it shows, which is what ends the
// entry's ssh connections (TestEntryQuitCutsLikeLock): the window's own
// request before it quits goes on to the local controller, and closing the
// gateway, when the app is gone, sends it too.
func TestDesktopQuitSuspendsTheLocalView(t *testing.T) {
	s, f := gatewayFixture(t)
	d := s.remote.desktop
	local := chooseLocal(t, d, d.context().ConnectionID)
	r := httptest.NewRequest("POST", s.origin+"/api/account/suspend", strings.NewReader("{}"))
	r.Header.Set("Origin", s.origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(connectionHeader, local)
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != 200 || f.suspended.Load() != 1 {
		t.Fatalf("the window's suspend before quitting: %d %s, %d suspensions", w.Code, w.Body.String(), f.suspended.Load())
	}
	s.remote.close()
	if f.suspended.Load() != 2 {
		t.Fatalf("closing the gateway suspended the local view %d times", f.suspended.Load()-1)
	}
}

// A local view left for another local view keeps its controller unlocked
// while the app runs, and with it that environment's ssh entry. Quitting the
// app counts as lock for the entry, so closing the gateway suspends every
// local view this run left behind that is still running, not only the one it
// shows.
func TestDesktopQuitSuspendsLocalViewsLeftBehind(t *testing.T) {
	s := testServer(t)
	first, second := newLocalVMFixture(t, "a"), newLocalVMFixture(t, "b")
	s.remote = newDesktopGateway(s.profile, first)
	d := s.remote.desktop
	d.localFor = func(id string) (DesktopLocalController, error) {
		if id == defaultLocalVMID {
			return first, nil
		}
		return second, nil
	}
	t.Cleanup(s.remote.close)
	shown := chooseLocal(t, d, d.context().ConnectionID)
	c, err := d.prepare(switchRequest{ConnectionID: shown, Mode: "local", VMID: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	candidateState(t, d, "connected")
	if err := d.commit(context.Background(), switchRequest{ConnectionID: shown, CandidateID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if !first.unlocked.Load() || !second.unlocked.Load() {
		t.Fatal("switching between local views locked one")
	}
	// Back to the first: the other is the one left behind now.
	shown = d.context().ConnectionID
	if c, err = d.prepare(switchRequest{ConnectionID: shown, Mode: "local"}); err != nil {
		t.Fatal(err)
	}
	candidateState(t, d, "connected")
	if err := d.commit(context.Background(), switchRequest{ConnectionID: shown, CandidateID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if len(d.left) != 1 || d.left["0123456789abcdef"] == nil || !first.unlocked.Load() || !second.unlocked.Load() {
		t.Fatalf("after switching back: left %v", d.left)
	}
	s.remote.close()
	if first.unlocked.Load() || second.unlocked.Load() {
		t.Fatalf("quitting left a local view unlocked: default %v, other %v", first.unlocked.Load(), second.unlocked.Load())
	}
}

// quitView is a connected local view whose controller answers each suspend
// with answer; n counts its suspend requests from 1.
func quitView(t *testing.T, vmID string, answer func(n int32, w http.ResponseWriter, r *http.Request)) (*desktopTarget, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/account/suspend" {
			http.NotFound(w, r)
			return
		}
		// Read to the end, so the server notices the client giving up.
		_, _ = io.Copy(io.Discard, r.Body)
		answer(calls.Add(1), w, r)
	}))
	t.Cleanup(endpoint.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session := &remotehost.Session{Origin: endpoint.URL, Client: endpoint.Client(), Transport: endpoint.Client().Transport, Context: ctx}
	return &desktopTarget{id: randomToken(), mode: "local", vmID: vmID, ctx: ctx, cancel: cancel, session: session}, calls
}

// Quitting locks every local view at once, and asks once more, with a
// longer deadline, a view that failed or did not answer (final review B,
// finding 4): a controller busy past the first deadline still gets its
// account locked, and hung controllers hold quit up for one view's two
// tries, never for each view in turn, and never past the bound. A view
// still not locked is logged by its environment only.
func TestDesktopQuitSuspendRetriesEveryViewAtOnceWithinTheBound(t *testing.T) {
	first, retry, bound := desktopQuitSuspendFirst, desktopQuitSuspendRetry, desktopQuitSuspendBound
	previous := diagnostics.Default()
	logs := t.TempDir()
	diagnostics.SetDefault(diagnostics.New(logs))
	t.Cleanup(func() {
		desktopQuitSuspendFirst, desktopQuitSuspendRetry, desktopQuitSuspendBound = first, retry, bound
		diagnostics.SetDefault(previous)
	})
	// A hung controller answers once the client gives up, or at the end of
	// the subtest (release), before its endpoint closes.
	hangers := func(release chan struct{}) (hangOnce, hangAlways func(int32, http.ResponseWriter, *http.Request)) {
		hang := func(r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
		hangOnce = func(n int32, _ http.ResponseWriter, r *http.Request) {
			if n == 1 {
				hang(r)
			}
		}
		return hangOnce, func(_ int32, _ http.ResponseWriter, r *http.Request) { hang(r) }
	}
	ok := func(int32, http.ResponseWriter, *http.Request) {}
	failOnce := func(n int32, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	}
	logged := func() string {
		var all []byte
		entries, _ := os.ReadDir(filepath.Join(logs, "logs"))
		for _, entry := range entries {
			data, _ := os.ReadFile(filepath.Join(logs, "logs", entry.Name()))
			all = append(all, data...)
		}
		return string(all)
	}

	t.Run("retry", func(t *testing.T) {
		desktopQuitSuspendFirst, desktopQuitSuspendRetry, desktopQuitSuspendBound = 200*time.Millisecond, 600*time.Millisecond, 5*time.Second
		release := make(chan struct{})
		hangOnce, hangAlways := hangers(release)
		shown, shownCalls := quitView(t, "", hangOnce)
		failing, failingCalls := quitView(t, "0123456789abcdef", failOnce)
		fine, fineCalls := quitView(t, "1123456789abcdef", ok)
		d := &desktopGateway{generation: randomToken(), active: shown, left: map[string]*desktopTarget{"0123456789abcdef": failing, "1123456789abcdef": fine}}
		hung := map[string]*atomic.Int32{}
		for _, id := range []string{"2123456789abcdef", "3123456789abcdef", "4123456789abcdef"} {
			view, calls := quitView(t, id, hangAlways)
			d.left[id], hung[id] = view, calls
		}
		t.Cleanup(func() { close(release) })
		started := time.Now()
		d.close()
		elapsed := time.Since(started)
		if shownCalls.Load() != 2 || failingCalls.Load() != 2 || fineCalls.Load() != 1 {
			t.Fatalf("suspends: shown %d, failing once %d, fine %d", shownCalls.Load(), failingCalls.Load(), fineCalls.Load())
		}
		for id, calls := range hung {
			if calls.Load() != 2 {
				t.Fatalf("hung %s was asked %d times", id, calls.Load())
			}
		}
		// One view after another, the three hung ones alone take 2.4 s.
		if elapsed < 750*time.Millisecond || elapsed > 1600*time.Millisecond {
			t.Fatalf("quit took %v", elapsed)
		}
		if d.left != nil || d.active != nil {
			t.Fatalf("closing kept views: left %v, active %v", d.left, d.active)
		}
		text := logged()
		for id := range hung {
			if !strings.Contains(text, "quitting did not lock local environment "+id+" after a retry") {
				t.Fatalf("no log line for %s: %s", id, text)
			}
		}
		for _, leak := range []string{"0123456789abcdef", "1123456789abcdef", " default ", "127.0.0.1", shown.id} {
			if strings.Contains(text, leak) {
				t.Fatalf("the log names %q: %s", leak, text)
			}
		}
	})
	t.Run("bound", func(t *testing.T) {
		desktopQuitSuspendFirst, desktopQuitSuspendRetry, desktopQuitSuspendBound = 200*time.Millisecond, 5*time.Second, 500*time.Millisecond
		release := make(chan struct{})
		_, hangAlways := hangers(release)
		view, calls := quitView(t, "", hangAlways)
		t.Cleanup(func() { close(release) })
		d := &desktopGateway{generation: randomToken(), active: view}
		started := time.Now()
		d.close()
		if elapsed := time.Since(started); calls.Load() != 2 || elapsed < 450*time.Millisecond || elapsed > 1500*time.Millisecond {
			t.Fatalf("a hung view: %d suspends, quit took %v", calls.Load(), elapsed)
		}
	})
}
