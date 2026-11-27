package tenant

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const protectedAccountFixture = "/api/fixture/protected"

// Rotate the private session on process replacement. Protected endpoints
// require a current controller cookie first, then an unlocked account.
func restartableAccountGuest(id string, account *fixtureAccountGuest, restarted *atomic.Bool) http.Handler {
	base := account.handler(id)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := fixtureSession
		if restarted.Load() {
			current = strings.Repeat("9", 64)
		}
		switch r.URL.Path {
		case "/api/launch":
			base.ServeHTTP(w, r)
			return
		case "/api/session":
			rec := httptest.NewRecorder()
			base.ServeHTTP(rec, r)
			if rec.Code == http.StatusOK {
				http.SetCookie(w, &http.Cookie{Name: guestCookie, Value: current, Path: "/"})
			}
			w.WriteHeader(rec.Code)
			_, _ = io.Copy(w, rec.Body)
			return
		}
		if r.Header.Get("Cookie") != guestCookie+"="+current {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == protectedAccountFixture {
			if _, unlocked, _, _, _, _, _ := account.snapshot(); !unlocked {
				w.WriteHeader(http.StatusLocked)
				return
			}
			jsonReply(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		r.Header.Set("Cookie", guestCookie+"="+fixtureSession)
		base.ServeHTTP(w, r)
	})
}

func TestTenantManagedAccountReconnect(t *testing.T) {
	for _, test := range []struct {
		name         string
		action       string
		restarted    bool
		update       bool
		badPassword  bool
		otherAccount bool
		legacy       bool
		wantUnlocked bool
		wantAttempts int
	}{
		{name: "expired controller session rechecks", restarted: true, wantUnlocked: true, wantAttempts: 1},
		{name: "self-update keeps the existing owner grant", restarted: true, update: true, wantUnlocked: true, wantAttempts: 1},
		{name: "manual lock on same generation", action: "lock", update: true},
		{name: "manual lock survives replacing controller", action: "lock", restarted: true, update: true},
		{name: "suspend on same controller", action: "suspend", update: true},
		{name: "changed password stays locked", restarted: true, badPassword: true, wantAttempts: 1},
		{name: "different account stays locked", restarted: true, otherAccount: true},
		{name: "older controller without lock policy stays locked", restarted: true, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, false)
			account := &fixtureAccountGuest{}
			var restarted atomic.Bool
			f.runtime.guestHandler = func(id string) http.Handler { return restartableAccountGuest(id, account, &restarted) }
			owner, _ := f.login("register", "alice")
			record := f.create(owner, "managed workspace")
			other, _ := f.login("register", "bob")
			otherVM := f.create(other, "another workspace")
			grant := f.connect(owner, record.ID)
			unusedTicket := f.ticket(owner, record.ID)
			request := func() *httptest.ResponseRecorder {
				return f.request("GET", vmOrigin(record.ID), protectedAccountFixture, "", grant)
			}
			wantStatus(t, request(), http.StatusOK, "")

			if test.action != "" {
				wantStatus(t, f.request("POST", vmOrigin(record.ID), "/api/account/"+test.action, "{}", grant), http.StatusOK, "")
				wantStatus(t, request(), http.StatusLocked, "")
				f.state(owner, record.ID, ready)
				if _, _, _, _, _, _, attempts := account.snapshot(); attempts != 0 {
					t.Fatalf("a 423 alone tried the stored credential %d times", attempts)
				}
			} else {
				account.setLocked()
			}
			if test.badPassword {
				account.setExistingAccount(defaultGuestAccountUsername, "new user password")
			}
			if test.otherAccount {
				account.setExistingAccount("other-user", "different user password")
			}
			account.mu.Lock()
			account.omitLockPolicy = test.legacy
			account.mu.Unlock()
			restarted.Store(test.restarted)

			if test.update {
				f.server.dropWorkspaceConnection(record.ID)
			} else {
				// The browser sees the original response. Any retry is an
				// independent request with the same authorized portal grant.
				wantStatus(t, request(), http.StatusUnauthorized, "")
			}
			f.state(owner, record.ID, failed)
			wantStatus(t, f.request("GET", vmOrigin(otherVM.ID), protectedAccountFixture, "", grant), http.StatusUnauthorized, "unauthenticated")
			wantStatus(t, f.enter(record.ID, unusedTicket), http.StatusUnauthorized, "unauthenticated")
			wantStatus(t, request(), http.StatusServiceUnavailable, "runtime_unavailable")
			f.state(owner, record.ID, ready)
			status := http.StatusLocked
			if test.wantUnlocked {
				status = http.StatusOK
			}
			wantStatus(t, request(), status, "")
			exists, unlocked, _, _, _, creates, attempts := account.snapshot()
			if !exists || unlocked != test.wantUnlocked || creates != 1 || attempts != test.wantAttempts {
				t.Fatalf("account state was wrong after reconnect: exists=%v unlocked=%v creates=%d unlock attempts=%d",
					exists, unlocked, creates, attempts)
			}
		})
	}
}

func TestTenantControllerReplacementDiscardsInFlightPreparation(t *testing.T) {
	f := newFixture(t, false)
	owner, _ := f.login("register", "alice")
	record := f.create(owner, "workspace")
	grant := f.connect(owner, record.ID)
	request := func() *httptest.ResponseRecorder {
		return f.request("GET", vmOrigin(record.ID), "/api/client/about", "", grant)
	}
	gate := make(chan struct{})
	f.runtime.gate = gate
	f.server.dropWorkspaceConnection(record.ID)
	wantStatus(t, request(), http.StatusServiceUnavailable, "runtime_unavailable")
	f.state(owner, record.ID, starting)

	f.server.dropWorkspaceConnection(record.ID)
	close(gate)
	f.state(owner, record.ID, failed)
	wantStatus(t, request(), http.StatusServiceUnavailable, "runtime_unavailable")
	f.state(owner, record.ID, ready)
	wantStatus(t, request(), http.StatusOK, "")
	f.runtime.mu.Lock()
	calls := f.runtime.calls[record.ID]
	f.runtime.mu.Unlock()
	if calls != 3 {
		t.Fatalf("expected first entry, superseded prepare and a fresh handshake, got %d prepares", calls)
	}
}
