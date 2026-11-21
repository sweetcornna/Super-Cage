package workbench

import (
	"net/http"

	"github.com/sylvinus/agent-vm/internal/webentry"
)

// phoneEntry reports whether "/" answers with the phone UI (mobile.html) rather
// than the desktop workbench. The phone UI talks to /api/mobile/* with the
// browser's session cookie, so only a server that answers those routes for a
// browser offers it: the desktop app's own gateway and client, and macOS or
// Windows guests (which admit phone routes only from a paired phone or a
// trusted view) always serve the desktop workbench.
//
// Behind the tenant gateway the browser's cookies and user agent never reach
// this server; the gateway applies the same rule and sends only its result.
func (s *Server) phoneEntry(r *http.Request) bool {
	if !s.phoneEntryAvailable() {
		return false
	}
	choice, ok := webentry.FromHint(r.Header)
	if !ok {
		choice = webentry.Choose(r.Header)
	}
	return choice == webentry.Phone && s.phoneReady(r)
}

// phoneEntryAvailable reports whether this server can ever answer "/" with the
// phone UI. Its desktop document says so (staticHandler), and only then does the
// page hand a phone-sized touch device over or offer the 手机版 switch: never in
// the desktop app's own window, on a macOS or Windows guest, or on a client.
func (s *Server) phoneEntryAvailable() bool {
	return !s.desktopEntry && !s.clientOnly && s.windowsGuest == nil && s.macosGuest == nil
}

// phoneReady reports whether the phone UI can work for this browser as it is.
// The phone UI cannot redeem a launch ticket, sign in, unlock the account or
// recover storage; the desktop document does all of these, so it is served
// until they are done (its page guard then hands a phone over, see
// ui/src/entry-layout.mjs). First setup is not checked here: a tenant VM's
// setup state is not something this rule should be able to get wrong, and the
// phone UI's environment page reports a server that cannot start.
func (s *Server) phoneReady(r *http.Request) bool {
	c, err := r.Cookie(s.cookieName())
	if err != nil || !equalSecret(c.Value, s.session) {
		return false
	}
	if _, unlocked := s.account.accessFor(s.hasTrustedView(r), false); !unlocked {
		return false
	}
	return s.requireStorageReady() == nil
}
