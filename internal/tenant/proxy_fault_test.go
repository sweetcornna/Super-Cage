package tenant

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// faultFixture is a workspace whose guest drops the connection of a POST to
// savePath, as a workbench does when its idle timer closes a pooled connection
// at the moment the gateway writes a request on it. controllerDown makes the
// health check fail with a 503.
type faultFixture struct {
	*portalFixture
	controllerDown atomic.Bool
	alice          *http.Cookie
	record         vm
	cookie         *http.Cookie
	workspace      *Workspace
}

const savePath = "/api/fixture/save"

func newFaultFixture(t *testing.T) *faultFixture {
	t.Helper()
	f := &faultFixture{portalFixture: newFixture(t, false)}
	f.runtime.guestHandler = func(id string) http.Handler {
		guest := fixtureGuest(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/client/about" && f.controllerDown.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if r.URL.Path == savePath {
				_, _ = io.Copy(io.Discard, r.Body)
				panic(http.ErrAbortHandler)
			}
			guest.ServeHTTP(w, r)
		})
	}
	f.alice, _ = f.login("register", "alice")
	f.record = f.create(f.alice, "workspace")
	f.cookie = f.connect(f.alice, f.record.ID)
	f.server.mu.Lock()
	f.workspace = f.server.workspaces[f.record.ID]
	f.server.mu.Unlock()
	return f
}

// settle waits until no fault probe is running.
func (f *faultFixture) settle() {
	f.t.Helper()
	waitFixture(f.t, func() bool { return !f.workspace.checkingSession.Load() })
}

func TestTenantLostReusedConnectionKeepsHealthyWorkspaceReady(t *testing.T) {
	f := newFaultFixture(t)
	unused := f.ticket(f.alice, f.record.ID)
	// The first request leaves one idle guest connection behind; the POST then
	// reuses it and the guest drops it, which the transport reports as a bare EOF.
	wantStatus(t, f.request("GET", vmOrigin(f.record.ID), "/", "", f.cookie), 200, "")
	wantStatus(t, f.request("POST", vmOrigin(f.record.ID), savePath, `{"name":"notes"}`, f.cookie), 503, "runtime_unavailable")
	f.settle()
	f.state(f.alice, f.record.ID, ready)
	wantStatus(t, f.request("GET", vmOrigin(f.record.ID), "/", "", f.cookie), 200, "")
	wantStatus(t, f.enter(f.record.ID, unused), 204, "")
}

func TestTenantLostReusedConnectionWithDeadControllerMarksWorkspaceUnready(t *testing.T) {
	f := newFaultFixture(t)
	unused := f.ticket(f.alice, f.record.ID)
	wantStatus(t, f.request("GET", vmOrigin(f.record.ID), "/", "", f.cookie), 200, "")
	f.controllerDown.Store(true)
	wantStatus(t, f.request("POST", vmOrigin(f.record.ID), savePath, `{"name":"notes"}`, f.cookie), 503, "runtime_unavailable")
	f.state(f.alice, f.record.ID, failed)
	wantStatus(t, f.enter(f.record.ID, unused), 401, "unauthenticated")
}

func TestTenantLostFreshConnectionStillFaultsAtOnce(t *testing.T) {
	f := newFaultFixture(t)
	unused := f.ticket(f.alice, f.record.ID)
	// No idle connection: the POST dials a new one. A new connection that fails
	// means the transport itself is broken, and the workspace is rechecked
	// without waiting for a probe.
	f.workspace.transport.CloseIdleConnections()
	wantStatus(t, f.request("POST", vmOrigin(f.record.ID), savePath, `{"name":"notes"}`, f.cookie), 503, "runtime_unavailable")
	f.state(f.alice, f.record.ID, failed)
	wantStatus(t, f.enter(f.record.ID, unused), 401, "unauthenticated")
}

func TestWorkspacePoolReleasesIdleConnectionsBeforeTheWorkbenchDoes(t *testing.T) {
	// The workbench closes a connection after 60 idle seconds
	// (internal/workbench/server.go).
	workspace := newWorkspace(45001, nil)
	defer workspace.close()
	for name, idle := range map[string]time.Duration{"requests": workspace.transport.IdleConnTimeout, "control": workspace.control.IdleConnTimeout} {
		if idle <= 0 || idle >= 60*time.Second {
			t.Fatalf("%s pool keeps idle connections for %s, want less than the workbench's 60s", name, idle)
		}
	}
}
