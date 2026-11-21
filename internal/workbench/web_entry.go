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
	if s.desktopEntry || s.clientOnly || s.windowsGuest != nil || s.macosGuest != nil {
		return false
	}
	if choice, ok := webentry.FromHint(r.Header); ok {
		return choice == webentry.Phone
	}
	return webentry.Choose(r.Header) == webentry.Phone
}
