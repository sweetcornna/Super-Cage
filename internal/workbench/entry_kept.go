package workbench

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Kept ssh terminals. A terminal the local SSH entry opened stays past its
// connection when the owner keeps it (保留) or a lock cuts the connection.
// Kept terminals have their own cap, entryKeptLimit, instead of the shared
// limit of 12, are listed on the local terminal's settings page with 结束,
// and one that has had no viewer and no running command for entryKeptIdle
// ends. The check runs every entryKeptIdle/3, so such a terminal ends
// between 30 and 40 minutes after it went idle, and only while something is
// kept: no timer runs otherwise.

// entryKeptLimit and entryKeptIdle are variables for tests.
var (
	entryKeptLimit = 4
	entryKeptIdle  = 30 * time.Minute
)

func errEntryKeptFull() error {
	return fmt.Errorf("最多保留 %d 个本机终端：先在 Super Cage 的本机终端设置里结束不用的", entryKeptLimit)
}

// entryKeptCheck is the idle check's timer and what it saw last time, and
// the last lock's decision about the terminals it cut.
type entryKeptCheck struct {
	mu        sync.Mutex
	timer     *time.Timer
	idle      map[string]time.Time // kept terminals seen idle, since when
	lockEpoch context.Context      // the unlock that the last lock ended
	lockKeep  map[string]bool      // the terminals that lock keeps
}

// keep marks an ssh terminal kept, within entryKeptLimit.
func (m *terminalManager) keep(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kept[id] {
		return nil
	}
	if m.sessions[id] == nil {
		return errTerminalNotReady
	}
	if len(m.kept) >= entryKeptLimit {
		return errEntryKeptFull()
	}
	if m.kept == nil {
		m.kept = map[string]bool{}
	}
	m.kept[id] = true
	return nil
}

// unkeepAll forgets every kept ssh terminal and returns them.
func (m *terminalManager) unkeepAll() []*terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sessions []*terminalSession
	for id := range m.kept {
		if session := m.sessions[id]; session != nil {
			sessions = append(sessions, session)
		}
	}
	m.kept = nil
	return sessions
}

// keptCount is how many ssh terminals are kept.
func (m *terminalManager) keptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.kept)
}

func (m *terminalManager) keptSessions() []*terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sessions []*terminalSession
	for id := range m.kept {
		if session := m.sessions[id]; session != nil {
			sessions = append(sessions, session)
		}
	}
	slices.SortFunc(sessions, func(a, b *terminalSession) int { return a.sequence - b.sequence })
	return sessions
}

// entryKeep keeps an ssh terminal and starts the idle check; nothing is
// kept while the entry is off.
func (s *Server) entryKeep(session *terminalSession) error {
	if !s.entryEnabled() {
		return errEntryOff
	}
	if err := s.terminals.keep(session.id); err != nil {
		return err
	}
	session.mu.Lock()
	session.kept = true
	session.mu.Unlock()
	s.entry.kept.mu.Lock()
	if s.entry.kept.timer == nil {
		s.entry.kept.timer = time.AfterFunc(entryKeptIdle/3, s.entryCheckKept)
	}
	s.entry.kept.mu.Unlock()
	return nil
}

// entryKeepOnLock keeps a terminal that the lock ending epoch cut, when
// that lock's decision keeps it. The decision is made once per lock, over
// every ssh terminal it cuts, so which ones stay does not depend on the
// order their connections notice the lock: within entryKeptLimit, the ones
// with the most recent input, the newest first among equals.
func (s *Server) entryKeepOnLock(epoch context.Context, session *terminalSession) bool {
	session.mu.Lock()
	kept := session.kept
	session.mu.Unlock()
	if kept {
		return true
	}
	s.entry.kept.mu.Lock()
	if s.entry.kept.lockEpoch != epoch {
		s.entry.kept.lockEpoch, s.entry.kept.lockKeep = epoch, s.entryLockDecision()
	}
	keep := s.entry.kept.lockKeep[session.id]
	s.entry.kept.mu.Unlock()
	return keep && s.entryKeep(session) == nil
}

// entryLockDecision picks the cut ssh terminals a lock keeps: every one an
// ssh client shows, not kept yet and still running, ranked by entryKeepRank,
// as many as entryKeptLimit leaves room for.
func (s *Server) entryLockDecision() map[string]bool {
	s.terminals.mu.Lock()
	room := entryKeptLimit - len(s.terminals.kept)
	var cut []*terminalSession
	for id, session := range s.terminals.sessions {
		if !s.terminals.kept[id] {
			cut = append(cut, session)
		}
	}
	s.terminals.mu.Unlock()
	cut = slices.DeleteFunc(cut, func(session *terminalSession) bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.origin != "ssh" || session.sshViewers == 0 || session.closed || session.ended
	})
	keep := map[string]bool{}
	for _, session := range entryKeepRank(cut)[:max(0, min(room, len(cut)))] {
		keep[session.id] = true
	}
	return keep
}

// entryKeepRank orders terminals by their last input, most recent first,
// and the newest first among equals.
func entryKeepRank(sessions []*terminalSession) []*terminalSession {
	type ranked struct {
		session  *terminalSession
		input    time.Time
		sequence int
	}
	order := make([]ranked, 0, len(sessions))
	for _, session := range sessions {
		session.mu.Lock()
		order = append(order, ranked{session, session.lastInput, session.sequence})
		session.mu.Unlock()
	}
	slices.SortFunc(order, func(a, b ranked) int {
		if c := b.input.Compare(a.input); c != 0 {
			return c
		}
		return b.sequence - a.sequence
	})
	out := make([]*terminalSession, len(order))
	for i, item := range order {
		out[i] = item.session
	}
	return out
}

// entryUnkeepAll is turning the entry off for kept terminals (R3-5): they
// go on as ordinary Super Cage terminals, without 已保留 and without the
// idle check, so an entry that is off runs no timer. One whose connection
// the off is cutting stays too (released). A keep that races with it finds
// the entry off (entryKeep).
func (s *Server) entryUnkeepAll() {
	for _, session := range s.terminals.unkeepAll() {
		session.mu.Lock()
		session.kept, session.released = false, true
		session.mu.Unlock()
	}
	s.entryUnkept()
}

// entryUnkept stops the idle check once nothing is kept.
func (s *Server) entryUnkept() {
	s.entry.kept.mu.Lock()
	defer s.entry.kept.mu.Unlock()
	if s.entry.kept.timer != nil && s.terminals.keptCount() == 0 && s.entry.kept.timer.Stop() {
		s.entry.kept.timer, s.entry.kept.idle = nil, nil
	}
}

// entryKeptTimerRunning tells whether the idle check is scheduled or runs.
func (s *Server) entryKeptTimerRunning() bool {
	s.entry.kept.mu.Lock()
	defer s.entry.kept.mu.Unlock()
	return s.entry.kept.timer != nil
}

// entryCheckKept ends the kept terminals that have been idle for
// entryKeptIdle, and schedules the next check while something is kept.
func (s *Server) entryCheckKept() {
	s.entry.kept.mu.Lock()
	previous := s.entry.kept.idle
	s.entry.kept.mu.Unlock()
	now := time.Now()
	idle := map[string]time.Time{}
	for _, session := range s.terminals.keptSessions() {
		if s.entryKeptBusy(session) {
			continue
		}
		since, seen := previous[session.id]
		if !seen {
			idle[session.id] = now
			continue
		}
		if now.Sub(since) >= entryKeptIdle {
			diagnostics.Log("info", "entry", "a kept ssh terminal ended after "+entryKeptIdle.String()+" without a viewer or a running command")
			s.terminals.remove(session.id)
			continue
		}
		idle[session.id] = since
	}
	s.entry.kept.mu.Lock()
	defer s.entry.kept.mu.Unlock()
	s.entry.kept.timer, s.entry.kept.idle = nil, idle
	if s.terminals.keptCount() > 0 {
		s.entry.kept.timer = time.AfterFunc(entryKeptIdle/3, s.entryCheckKept)
	}
}

// entryKeptBusy: the terminal has a viewer (an ssh client or the window),
// or runs a command. An interactive terminal runs one when its shell has a
// child in the guest; ssh -t <command> runs its command until it ends. A
// check that fails counts as busy, so no work ends on a guess.
func (s *Server) entryKeptBusy(session *terminalSession) bool {
	session.mu.Lock()
	watched, ended, key := len(session.clients) > 0, session.closed || session.ended, session.originPID
	session.mu.Unlock()
	switch {
	case ended:
		return false
	case watched || key == "":
		return true
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 20*time.Second)
	defer cancel()
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", `f="$HOME/.cache/cage-entry/terminals/`+key+`.pid"; [ -s "$f" ] || exit 3; exec pgrep -P "$(cat "$f")" >/dev/null`)
	cmd.WaitDelay = time.Second
	var exit *exec.ExitError
	// pgrep exits 1 when the shell has no child.
	return !(errors.As(cmd.Run(), &exit) && exit.ExitCode() == 1)
}

// entryPIDScript records an interactive ssh terminal's shell process id in
// the guest: the shell that runs the terminal's script execs bash, so $$ is
// bash's. It first removes the records of shells that are gone.
func entryPIDScript(key string) string {
	return `{ d="$HOME/.cache/cage-entry/terminals"; mkdir -p "$d" && for f in "$d"/*.pid; do [ -e "$f" ] && ! kill -0 "$(cat "$f")" && rm -f "$f"; done; echo $$ > "$d/` + key + `.pid"; } 2>/dev/null; `
}

// entryKeptView is a kept terminal on the settings page.
type entryKeptView struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Device string `json:"device"`
	Ended  bool   `json:"ended"`
}

func (s *Server) entryKeptViews() []entryKeptView {
	views := []entryKeptView{}
	for _, session := range s.terminals.keptSessions() {
		session.mu.Lock()
		views = append(views, entryKeptView{ID: session.id, Title: session.title, Device: session.originDevice, Ended: session.closed || session.ended})
		session.mu.Unlock()
	}
	return views
}

// keepTerminal is "保留" on a terminal an ssh session opened: it stays when
// the connection ends, within entryKeptLimit.
func (s *Server) keepTerminal(w http.ResponseWriter, r *http.Request) {
	session := s.terminals.get(r.PathValue("id"))
	if session == nil {
		http.NotFound(w, r)
		return
	}
	session.mu.Lock()
	ssh := session.origin == "ssh"
	session.mu.Unlock()
	if !ssh {
		http.Error(w, "只有来自本机终端的终端需要保留", http.StatusConflict)
		return
	}
	if err := s.entryKeep(session); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	jsonReply(w, session.summary())
}
