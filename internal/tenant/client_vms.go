package tenant

import (
	"net/http"
	"strings"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
)

// serveVirtualMachines answers the owner-scoped VM controls used by the
// workbench sidebar. It reports whether the request matched this API so the
// ordinary workspace proxy and its internal-path blocklist are not involved.
func (s *Server) serveVirtualMachines(w http.ResponseWriter, r *http.Request, id string) bool {
	// The cloud entry's enrolment (devices.go); closed, it is not found like
	// any other /api/client/ path.
	if r.URL.Path == "/api/client/cloud-entry" && cloudentry.Open() {
		if r.Method != http.MethodPost {
			notFound(w)
			return true
		}
		s.clientCloudEntry(w, r, id)
		return true
	}
	if r.URL.Path == "/api/client/virtual-machines" {
		switch r.Method {
		case http.MethodGet:
			s.clientListVMs(w, r, id)
		case http.MethodPost:
			s.clientCreateVM(w, r, id)
		default:
			notFound(w)
		}
		return true
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/client/virtual-machines/")
	if !ok {
		return false
	}
	target, ok := strings.CutSuffix(rest, "/open")
	if r.Method != http.MethodPost || !ok || !vmIDPattern.MatchString(target) {
		notFound(w)
		return true
	}
	s.clientOpenVM(w, r, id, target)
	return true
}

// clientSessionLocked validates the VM grant that proves the portal session
// and the ownership of the VM whose origin received the request.
func (s *Server) clientSessionLocked(w http.ResponseWriter, r *http.Request, id string) *loginSession {
	grant := s.grants[cookieValue(r, s.site.vmCookie)]
	if !s.validGrant(grant, id, s.now()) {
		unauthenticated(w)
		return nil
	}
	if len(r.Header.Values("X-Cage-Expected-User")) != 0 && !exactHeader(r.Header, "X-Cage-Expected-User", grant.login.user.ID) {
		problem(w, http.StatusConflict, "session_changed", "The signed-in account has changed. Refresh your session and try again.")
		return nil
	}
	return grant.login
}

func (s *Server) clientListVMs(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	session := s.clientSessionLocked(w, r, id)
	if session == nil {
		s.mu.Unlock()
		return
	}
	list := s.ownedVMsLocked(session.user.ID)
	s.mu.Unlock()
	sortVMs(list)
	jsonReply(w, http.StatusOK, map[string]any{"vms": list, "current_id": id})
}

func (s *Server) clientCreateVM(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.clientSessionLocked(w, r, id)
	if session == nil {
		return
	}
	record, code := s.createVMRecordLocked(session, input.Name, "")
	switch code {
	case "":
		jsonReply(w, http.StatusCreated, map[string]vm{"vm": record.vm})
	case "vm_limit":
		problem(w, http.StatusConflict, "vm_limit", "The workspace quota has been reached.")
	case "runtime_unavailable":
		unavailable(w)
	default:
		invalidInput(w)
	}
}

// clientOpenVM validates that the target belongs to the same portal account
// as the current VM. The workbench does not issue a second ticket here: the
// portal frame performs the actual open, and joins the startup task started
// below when the VM was not ready. No guest endpoint is exposed.
func (s *Server) clientOpenVM(w http.ResponseWriter, r *http.Request, id, target string) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.clientSessionLocked(w, r, id)
	if session == nil {
		return
	}
	record, exists := s.store.VMs[target]
	if !exists || record.Owner != session.user.ID {
		vmNotFound(w)
		return
	}
	if s.store.blocked {
		unavailable(w)
		return
	}
	now := s.now()
	if !s.workspaceReadyLocked(record, now) {
		if !s.allowRate("open", session.user.ID, 120, now) {
			rateLimited(w)
			return
		}
		s.startVMLocked(record, now)
	}
	jsonReply(w, http.StatusOK, map[string]any{"vm_id": target, "vm": s.visibleVMLocked(record)})
}
