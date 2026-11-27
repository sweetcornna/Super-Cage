package tenant

import (
	"cmp"
	"context"
	"errors"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ticketTTL       = 60 * time.Second
	maxTickets      = 4096
	maxLoginTickets = 16
	readinessTTL    = 30 * time.Second
)

// faultProbeTimeout bounds the check that confirms a suspected workspace
// fault. A check that runs out of time is inconclusive: a busy guest or a
// slow transport start is not proof that open streams are broken.
var faultProbeTimeout = 3 * time.Second

func validVMName(name string) bool {
	length := utf8.RuneCountInString(name)
	return length >= 1 && length <= 64 && strings.IndexFunc(name, unicode.IsControl) == -1
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	session := s.expectedSessionLocked(w, r)
	if session == nil {
		s.mu.Unlock()
		return
	}
	list := s.ownedVMsLocked(session.user.ID)
	s.mu.Unlock()
	sortVMs(list)
	jsonReply(w, http.StatusOK, map[string]any{"vms": list, "limit": s.cfg.AccountVMLimit})
}

// Caller holds s.mu.
func (s *Server) ownedVMsLocked(owner string) []vm {
	list := make([]vm, 0)
	for _, record := range s.store.VMs {
		if record.Owner == owner {
			list = append(list, s.visibleVMLocked(record))
		}
	}
	return list
}

func sortVMs(list []vm) {
	slices.SortFunc(list, func(left, right vm) int {
		if order := left.CreatedAt.Compare(right.CreatedAt); order != 0 {
			return order
		}
		return cmp.Compare(left.ID, right.ID)
	})
}

func (s *Server) visibleVMLocked(record vmRecord) vm {
	visible := record.vm
	workspace := s.workspaces[record.ID]
	switch {
	case s.starting[record.ID]:
		visible.State = starting
	case workspace != nil && !workspace.checkedAt.IsZero():
		visible.State = ready
	case workspace != nil || s.startFailed[record.ID]:
		visible.State, visible.Message = failed, startupMessage
	}
	return visible
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		SystemID string `json:"system_id"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.expectedSessionLocked(w, r)
	if session == nil {
		return
	}
	record, code := s.createVMRecordLocked(session, input.Name, input.SystemID)
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

// createVMRecordLocked persists a new VM for the signed-in account. An empty
// system ID selects the first administrator-configured system, which is also
// the default for clients that only submit a name.
func (s *Server) createVMRecordLocked(session *loginSession, name, systemID string) (vmRecord, string) {
	name = strings.TrimSpace(name)
	if !validVMName(name) {
		return vmRecord{}, "invalid_input"
	}
	if systemID == "" {
		systemID = s.defaultSystem.ID
	}
	system, configured := s.systems[systemID]
	if !configured {
		return vmRecord{}, "invalid_input"
	}
	count := 0
	for _, record := range s.store.VMs {
		if record.Owner == session.user.ID {
			count++
		}
	}
	if count >= s.cfg.AccountVMLimit || len(s.store.VMs) >= s.cfg.TotalVMLimit {
		return vmRecord{}, "vm_limit"
	}
	id := secret(16)
	for s.store.VMs[id].ID != "" {
		id = secret(16)
	}
	record := vmRecord{
		vm:    vm{ID: id, Name: name, SystemID: system.ID, SystemLabel: system.Label, State: stopped, CreatedAt: s.now().UTC()},
		Owner: session.user.ID, Image: system.Image,
	}
	if s.saveVM(record) != nil {
		return vmRecord{}, "runtime_unavailable"
	}
	return record, ""
}

func (s *Server) saveVM(record vmRecord) error {
	updated := s.store.database
	updated.VMs = maps.Clone(updated.VMs)
	updated.VMs[record.ID] = record
	return s.store.save(updated)
}

func (s *Server) openVM(w http.ResponseWriter, r *http.Request, id string) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.expectedSessionLocked(w, r)
	if session == nil {
		return
	}
	record, exists := s.store.VMs[id]
	if !exists || record.Owner != session.user.ID {
		vmNotFound(w)
		return
	}
	if s.store.blocked {
		unavailable(w)
		return
	}
	now := s.now()
	if !s.allowRate("open", session.user.ID, 120, now) {
		rateLimited(w)
		return
	}
	s.noteActivityLocked(record.ID, now)
	if s.workspaceReadyLocked(record, now) {
		url, ok := s.issueTicketLocked(session, record.ID, now)
		if !ok {
			rateLimited(w)
			return
		}
		jsonReply(w, http.StatusOK, map[string]any{"vm": s.visibleVMLocked(record), "workspace_url": url})
		return
	}
	s.startVMLocked(record, now)
	jsonReply(w, http.StatusAccepted, map[string]vm{"vm": s.visibleVMLocked(record)})
}

// Caller holds s.mu.
func (s *Server) workspaceReadyLocked(record vmRecord, now time.Time) bool {
	workspace := s.workspaces[record.ID]
	return workspace != nil && !workspace.checkedAt.IsZero() && now.Sub(workspace.checkedAt) < readinessTTL && s.visibleVMLocked(record).State == ready
}

// issueTicketLocked returns a one-time workspace entry URL for the target
// record. The caller must hold s.mu and have validated the session and owner.
// The bool is false when the per-session or global ticket budget is exhausted.
func (s *Server) issueTicketLocked(session *loginSession, id string, now time.Time) (string, bool) {
	if session.ticketCount >= maxLoginTickets || len(s.tickets) >= maxTickets {
		s.prune(now)
		if session.ticketCount >= maxLoginTickets || len(s.tickets) >= maxTickets {
			return "", false
		}
	}
	token := secret(32)
	s.tickets[token] = entryTicket{grant: vmGrant{login: session, vmID: id}, expires: now.Add(ticketTTL)}
	session.ticketCount++
	return s.site.vmOrigin(id) + "/_tenant/enter#" + token, true
}

// startVMLocked joins or creates the idempotent preparation task for a VM.
// Caller holds s.mu.
func (s *Server) startVMLocked(record vmRecord, now time.Time) {
	id := record.ID
	current := s.workspaces[id]
	if s.starting[id] || (current != nil && now.Before(current.nextProbe)) {
		return
	}
	s.starting[id] = true
	delete(s.startFailed, id)
	var faults uint64
	if current != nil {
		faults = current.faults
	}
	spec := LaunchSpec{VMID: id, SystemID: record.SystemID, Image: record.Image}
	replacement := s.replacements[id]
	s.workers.Go(func() { s.prepare(spec, current, faults, replacement) })
}

func (s *Server) invalidateWorkspace(id string, workspace *Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspaces[id] != workspace {
		return
	}
	s.markWorkspaceUnready(id, workspace)
}

// Caller holds s.mu.
func (s *Server) markWorkspaceUnready(id string, workspace *Workspace) {
	workspace.faults++
	workspace.checkedAt = time.Time{}
	s.deleteWorkspaceTickets(id)
}

func (s *Server) confirmWorkspaceFault(id string, workspace *Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.context.Err() != nil || s.workspaces[id] != workspace || workspace.checkedAt.IsZero() || now.Before(workspace.nextFaultProbe) || !workspace.checkingSession.CompareAndSwap(false, true) {
		return
	}
	workspace.nextFaultProbe = now.Add(3 * time.Second)
	faults := workspace.faults
	s.workers.Go(func() {
		defer workspace.checkingSession.Store(false)
		ctx, cancel := context.WithTimeout(workspace.ctx, faultProbeTimeout)
		defer cancel()
		stop := context.AfterFunc(s.context, cancel)
		defer stop()
		if err := workspace.check(ctx); err == nil || workspace.ctx.Err() != nil || ctx.Err() != nil || isTimeout(err) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.context.Err() == nil && s.workspaces[id] == workspace && workspace.faults == faults {
			s.markWorkspaceUnready(id, workspace)
		}
	})
}

func (s *Server) deleteWorkspaceTickets(id string) {
	for token, ticket := range s.tickets {
		if ticket.grant.vmID == id {
			s.deleteTicket(token, ticket)
		}
	}
}

func (s *Server) prepare(spec LaunchSpec, previous *Workspace, faults, replacement uint64) {
	id := spec.VMID
	select {
	case s.startSlots <- struct{}{}:
		defer func() { <-s.startSlots }()
	case <-s.context.Done():
		return
	}
	ctx, cancel := context.WithTimeout(s.context, 5*time.Minute)
	defer cancel()
	// A workspace frozen while idle resumes before it is checked. If the
	// resume fails, the check fails and Prepare starts the instance.
	_ = s.wake(ctx, id)
	workspace := previous
	if previous != nil && previous.ctx.Err() != nil {
		workspace = nil
	} else if previous != nil {
		probe, done := context.WithTimeout(ctx, 5*time.Second)
		stop := context.AfterFunc(previous.ctx, done)
		err := previous.recheck(probe)
		probeErr := probe.Err()
		stop()
		done()
		if err != nil {
			s.mu.Lock()
			if s.replacements[id] != replacement {
				delete(s.starting, id)
				s.mu.Unlock()
				return
			}
			if probeErr != nil || errors.Is(err, errIncusQuery) || isTimeout(err) {
				// A slow or unavailable probe cannot prove that active streams are broken.
				s.markWorkspaceUnready(id, previous)
				previous.nextProbe = s.now().Add(2 * time.Second)
				delete(s.starting, id)
				s.mu.Unlock()
				return
			}
			workspace = nil
			s.markWorkspaceUnready(id, previous)
			// Retain the authenticated generation until its successor is
			// published. Old browser grants may retry after a failed handshake.
			previous.close()
			s.mu.Unlock()
		}
	}
	if workspace == nil {
		var err error
		workspace, err = s.cfg.Runtime.Prepare(ctx, spec)
		if err != nil {
			if s.context.Err() == nil {
				log.Printf("cage-tenant: workspace %s preparation failed: %v", id, err)
			}
			if workspace != nil {
				workspace.close()
				workspace = nil
			}
		}
		s.mu.Lock()
		superseded := s.replacements[id] != replacement
		s.mu.Unlock()
		if workspace != nil && (ctx.Err() != nil || superseded) {
			workspace.close()
			workspace = nil
		}
		if workspace != nil && s.cfg.GuestAccount != nil {
			// The portal already authenticated the owner. An explicitly
			// configured guest account lets that owner enter the VM without a
			// second prompt. A VM that already has a different account is left
			// untouched and keeps its existing account screen and vault.
			accountCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			entered, accountErr := workspace.ensureGuestAccount(accountCtx, s.cfg.GuestAccount, id, previous)
			cancel()
			if accountErr != nil {
				if s.context.Err() == nil {
					log.Printf("cage-tenant: workspace %s guest account setup failed: %v", id, accountErr)
				}
				workspace.close()
				workspace = nil
			} else if !entered && s.context.Err() == nil {
				log.Printf("cage-tenant: workspace %s keeps its existing guest account", id)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.starting, id)
	if s.context.Err() != nil || s.replacements[id] != replacement {
		if workspace != nil && workspace != previous {
			workspace.close()
		}
		return
	}
	if workspace == nil {
		s.startFailed[id] = true
		if previous != nil {
			previous.nextProbe = s.now().Add(2 * time.Second)
		}
		return
	}
	if workspace != previous || workspace.faults == faults {
		workspace.checkedAt = s.now()
	}
	s.workspaces[id] = workspace
	s.awakeLocked(id, s.now())
}
