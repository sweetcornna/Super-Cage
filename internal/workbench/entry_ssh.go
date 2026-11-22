package workbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
	"github.com/sylvinus/agent-vm/internal/localterm"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// The Cage entry endpoint (design 5.4): an SSH server that runs on one
// WebSocket at a time, inside this controller. SSH from the owner's own
// tools ends here, in Cage's code, never in the guest. Every rule is a
// missing handler: the endpoint knows session channels and nothing else.
//   - publickey for the enrolled device key only; no other method.
//   - session channels (at most entryMaxChannels per connection):
//       pty + shell/exec  -> a Cage terminal (listed, on the phone, the
//                            same launcher, the 12-terminal limit), its
//                            output through cage attach's escape filter;
//       exec, shell        -> the guest's run script over Cage's own hop;
//       subsystem sftp     -> the guest's sftp-server over the same hop;
//       Herdr's connect line -> the environment's Herdr core, through the
//                            Herdr filter (entry_herdr.go); Herdr's TUI
//                            bridge is refused.
//   - no handler for any forwarding, agent, X11, tunnel or client
//     environment: those channels are rejected, those requests answered no.

// entryWrapperTimeout bounds one install of the guest's run script; a
// variable for tests.
var entryWrapperTimeout = 20 * time.Second

const (
	entryMaxChannels      = 8
	entryHandshakeTimeout = 20 * time.Second
	entryHopWaitDelay     = 2 * time.Second
	// entryCutGrace is how long a cut waits for terminal sessions to write
	// their reset before it closes the connection.
	entryCutGrace = 250 * time.Millisecond
	// entryOperationWait bounds how long a new terminal waits for another
	// terminal's creation or a monitor round to release the operation lock.
	entryOperationWait     = 10 * time.Second
	entryServerVersion     = "SSH-2.0-SuperCage"
	entryLockedNotice      = "Super Cage 已锁定：这个终端仍在运行。在应用中解锁后，用 ssh -t 重新接入，或在 Super Cage 里打开它。"
	entryTerminalLimitText = "已有 12 个终端：先关闭一个"
)

// entryService is the Server's local SSH entry.
type entryService struct {
	registry entryRegistry
	// mu serialises changes to the entry's records. It is never held while
	// anything waits on the guest: off and revoke take it.
	mu sync.Mutex
	// wrapper is the guest's run script install, with its own lock.
	wrapper entryWrapper
	// Test seams: injected paths, the guest's run script and its install.
	paths   *entryPaths
	runPath string
	install func(context.Context) error
	// kept is the idle check of kept ssh terminals.
	kept entryKeptCheck
	// herdr holds the delay after Herdr lines the environment ended at once.
	herdr entryHerdrState
}

// entryFocusOn turns on focus reports in the ssh client's terminal, as cage
// attach does in its window (attach.focusOn): they tell whether the owner is
// at that window. They are the viewer's: the program gets them only if it
// asked for them too.
const entryFocusOn = "\x1b[?1004h"

func (s *Server) entryPaths() (entryPaths, error) {
	if s.entry.paths != nil {
		return *s.entry.paths, nil
	}
	return defaultEntryPaths(s.profile, s.exe)
}

func (s *Server) entryRunPath() string {
	if s.entry.runPath != "" {
		return s.entry.runPath
	}
	return cage.EntryRunPath
}

// entryEnvironmentStopped cuts every connection when the environment
// stops: the environment's state changed first, so the next dial is refused.
func (s *Server) entryEnvironmentStopped() {
	s.entry.registry.cutAll(errEntryStopped)
	s.entryGuestStarting()
}

// entryGuestStarting forgets the installed run script: a start may bring up a
// new guest.
func (s *Server) entryGuestStarting() {
	w := &s.entry.wrapper
	w.mu.Lock()
	w.ready = false
	w.generation++
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
}

// entryWrapper is the state of the guest's run script install. One install
// runs at a time, under its own lock and its own deadline, and callers wait
// for it only as long as their own context lasts: a cut never waits on it,
// and neither do off, revoke or a new connection.
type entryWrapper struct {
	mu         sync.Mutex
	ready      bool
	generation int           // counts guest starts; an install for an older guest does not count
	running    chan struct{} // closed when the install in progress ends
	cancel     context.CancelFunc
}

// entryCancelWrapper stops an install in progress: the entry was turned off
// or the guest is going away.
func (s *Server) entryCancelWrapper() {
	w := &s.entry.wrapper
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
}

var errEntryWrapper = errors.New("无法在环境里准备本机终端组件，请确认环境正在运行")

// entryStartWrapper starts the install unless it is done or running, and
// returns what to wait for (nil when the run script is in place).
func (s *Server) entryStartWrapper() <-chan struct{} {
	w := &s.entry.wrapper
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready {
		return nil
	}
	if w.running != nil {
		return w.running
	}
	done := make(chan struct{})
	w.running = done
	generation := w.generation
	install := s.entry.install
	if install == nil {
		install = func(ctx context.Context) error {
			_, err := s.guestControl(ctx, cage.EntryInstallScript(cage.EntryRunPath, cage.EntryHerdrProfilePath), "entry")
			return err
		}
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), entryWrapperTimeout)
	w.cancel = cancel
	go func() {
		err := install(ctx)
		cancel()
		w.mu.Lock()
		w.ready = err == nil && w.generation == generation
		w.running, w.cancel = nil, nil
		w.mu.Unlock()
		close(done)
	}()
	return done
}

// entryEnsureWrapper installs the guest's run script the first time a channel
// needs it after a guest start (cage.EntryInstallScript), waiting at most as
// long as ctx.
func (s *Server) entryEnsureWrapper(ctx context.Context) error {
	done := s.entryStartWrapper()
	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return errEntryWrapper
	}
	w := &s.entry.wrapper
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ready {
		return errEntryWrapper
	}
	return nil
}

// entryCheck decides a dial before the upgrade, in the order of design 5.1.
// The account lock is decided by the request middleware (423) before this.
func (s *Server) entryCheck(device string) (entryDevice, *entryRefusal) {
	if s.desktopEntry || s.clientOnly {
		return entryDevice{}, errEntryUnsupported
	}
	st, err := entryLoadState(s.profile)
	if err != nil || !st.Enabled {
		return entryDevice{}, errEntryOff
	}
	record := st.device(device)
	if record == nil {
		return entryDevice{}, errEntryUnknown
	}
	if paths, err := s.entryPaths(); err == nil {
		if _, err := entryLoadRevoked(paths); err != nil {
			return entryDevice{}, errEntryRevocationsUnreadable
		}
	}
	if record.Revoked != nil || s.entryComputerKeyRevoked(device) {
		return entryDevice{}, errEntryRevoked
	}
	if s.macosGuest != nil || s.windowsGuest != nil {
		return entryDevice{}, errEntryUnsupported
	}
	s.mu.Lock()
	closing, ready, paused := s.closing, s.ready, s.networkPaused
	s.mu.Unlock()
	switch {
	case closing:
		return entryDevice{}, errEntryClosing
	case !s.vmRunning():
		return entryDevice{}, errEntryStopped
	case !ready:
		return entryDevice{}, errEntryStarting
	case paused:
		return entryDevice{}, errEntryPaused
	}
	return *record, nil
}

// entryComputerKeyRevoked: device is this computer and its key was revoked
// in another environment whose controller did not run then.
func (s *Server) entryComputerKeyRevoked(device string) bool {
	paths, err := s.entryPaths()
	if err != nil {
		return false
	}
	computer, ok, _ := entryReadComputer(paths)
	if !ok || computer.ID != device {
		return false
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(computer.Public))
	return err == nil && entryKeyRevoked(paths, key)
}

func entryRefuse(w http.ResponseWriter, refusal *entryRefusal) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(refusal.Status)
	_, _ = fmt.Fprintf(w, "{\"code\":%q,\"message\":%q}\n", refusal.Code, refusal.Message)
}

// entrySSH is GET /api/entry/ssh?device=<id>: the WebSocket the ProxyCommand
// opens. A dial never starts a stopped environment.
func (s *Server) entrySSH(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) {
		entryRefuse(w, errEntryLocalOnly)
		return
	}
	if r.Header.Get(agentRequestHeader) != "" {
		entryRefuse(w, errEntryWindowOnly)
		return
	}
	device := r.URL.Query().Get("device")
	conn := &entryConn{device: device}
	conn.epoch, _ = r.Context().Value(accountEpochKey{}).(context.Context)
	if conn.epoch == nil {
		entryRefuse(w, errEntryLocked)
		return
	}
	conn.ctx, conn.cancel = context.WithCancelCause(r.Context())
	defer conn.cancel(nil)
	// A lock cuts the connection like any other event, with its cause; the
	// account middleware ends the request too.
	stopLock := context.AfterFunc(conn.epoch, func() { conn.cancel(errEntryLocked) })
	defer stopLock()
	// Register before deciding, so an event between the decision and the
	// registration still reaches this connection: every event changes its
	// state first and cuts after.
	s.entry.registry.add(conn)
	defer s.entry.registry.remove(conn)
	record, refusal := s.entryCheck(device)
	if refusal == nil && conn.ctx.Err() != nil {
		refusal = errEntryClosing
		if cause, ok := context.Cause(conn.ctx).(*entryRefusal); ok {
			refusal = cause
		}
	}
	if refusal != nil {
		entryRefuse(w, refusal)
		return
	}
	signer, err := entryHostSigner(s.profile, false)
	if err != nil {
		entryRefuse(w, errEntryOff)
		return
	}
	header := http.Header{entryproxy.HostKeyHeader: {strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == s.origin }, ReadBufferSize: 16 << 10, WriteBufferSize: 16 << 10}
	ws, err := upgrader.Upgrade(w, r, header)
	if err != nil {
		return
	}
	defer ws.Close()
	s.entryServe(conn, newEntryWSConn(ws), record, signer)
}

// entryServe runs the SSH server of one connection until it ends or is cut.
func (s *Server) entryServe(conn *entryConn, transport net.Conn, record entryDevice, signer ssh.Signer) {
	// A cut lets terminal sessions write their reset first, for at most
	// entryCutGrace (entryRunTerminal).
	stop := context.AfterFunc(conn.ctx, func() {
		conn.flush.wait(entryCutGrace)
		_ = transport.Close()
	})
	defer stop()
	config := &ssh.ServerConfig{
		ServerVersion: entryServerVersion,
		MaxAuthTries:  6,
		// Any user name is accepted and ignored; only the device's key counts.
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.entryKeyAccepted(record, key) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(signer)
	handshake := time.AfterFunc(entryHandshakeTimeout, func() { _ = transport.Close() })
	server, channels, requests, err := ssh.NewServerConn(transport, config)
	handshake.Stop()
	if err != nil {
		return
	}
	closeServer := context.AfterFunc(conn.ctx, func() {
		conn.flush.wait(entryCutGrace)
		_ = server.Close()
	})
	defer closeServer()
	defer server.Close()
	s.entryNoteUse(record.ID)
	go entryGlobalRequests(conn, requests)
	var sessions sync.WaitGroup
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			// direct-tcpip (-L, -D, -W), direct-streamlocal@openssh.com,
			// tun@openssh.com (-w), x11, auth-agent@openssh.com: no handler.
			conn.refuse("channel:" + incoming.ChannelType())
			_ = incoming.Reject(ssh.Prohibited, "Super Cage 的本机终端不提供转发")
			continue
		}
		if conn.channels.Add(1) > entryMaxChannels {
			conn.channels.Add(-1)
			_ = incoming.Reject(ssh.ResourceShortage, "一个连接最多同时打开 8 个会话")
			continue
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer conn.channels.Add(-1)
			s.entrySession(conn, incoming)
		}()
	}
	// The connection ended: end what its sessions started.
	conn.cancel(nil)
	sessions.Wait()
	diagnostics.Log("info", "entry", fmt.Sprintf("connection of device %s ended", entryShortID(record.ID)))
}

func entryShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// entryKeyAccepted is the key of the enrolled device, or this computer's
// current key when the device is this computer and its key was renewed in
// another environment (the record then follows it).
func (s *Server) entryKeyAccepted(record entryDevice, key ssh.PublicKey) bool {
	paths, err := s.entryPaths()
	if err != nil || entryKeyRevoked(paths, key) {
		return false
	}
	if enrolled, _, _, _, err := ssh.ParseAuthorizedKey([]byte(record.PublicKey)); err == nil && entryKeysEqual(enrolled, key) {
		return true
	}
	computer, ok, err := entryReadComputer(paths)
	if err != nil || !ok || computer.ID != record.ID {
		return false
	}
	current, _, _, _, err := ssh.ParseAuthorizedKey([]byte(computer.Public))
	if err != nil || !entryKeysEqual(current, key) {
		return false
	}
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	st, err := entryLoadState(s.profile)
	if device := st.device(record.ID); err == nil && device != nil && device.Revoked == nil {
		device.PublicKey = computer.Public
		_ = entrySaveState(s.profile, st)
		return true
	}
	return false
}

func entryKeysEqual(a, b ssh.PublicKey) bool {
	return a.Type() == b.Type() && string(a.Marshal()) == string(b.Marshal())
}

func (s *Server) entryNoteUse(device string) {
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	st, err := entryLoadState(s.profile)
	if record := st.device(device); err == nil && record != nil {
		now := time.Now().UTC()
		record.LastUsed = &now
		_ = entrySaveState(s.profile, st)
	}
}

// entryGlobalRequests answers connection-level requests: keepalives yes,
// everything else (tcpip-forward and streamlocal-forward@openssh.com for -R
// among them) no.
func entryGlobalRequests(conn *entryConn, requests <-chan *ssh.Request) {
	for request := range requests {
		if request.Type == "keepalive@openssh.com" {
			_ = request.Reply(true, nil)
			continue
		}
		conn.refuse("global:" + request.Type)
		_ = request.Reply(false, nil)
	}
}

type entryPTYRequest struct {
	Term                      string
	Cols, Rows, Width, Height uint32
	Modes                     string
}

type entryWindowChange struct {
	Cols, Rows, Width, Height uint32
}

// entryHerdrLine recognises what looks like Herdr's connect line (herdr-core
// machines.rs:242-266, "exec \"$p\" connect" after locating herdr-core): any
// command that names herdr-core, or hc as a word, together with the word
// connect. Only the exact line becomes a Herdr link (entryHerdrClassify);
// the rest are refused, so a newer Herdr whose line changed cannot open an
// unfiltered link. Probes (uname -sm, herdr-core --version) do not say
// connect and stay ordinary commands.
var (
	entryHerdrName        = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])hc([^A-Za-z0-9_-]|$)`)
	entryHerdrConnectWord = regexp.MustCompile(`(^|[^A-Za-z0-9_-])connect([^A-Za-z0-9_-]|$)`)
)

func entryHerdrLine(command string) bool {
	return (strings.Contains(command, "herdr-core") || entryHerdrName.MatchString(command)) && entryHerdrConnectWord.MatchString(command)
}

// entryOutput is one ssh window's side of the escape filter, written the way
// cage attach writes its window (attach.writeOutput, restart, flushLocked).
// Only the goroutine that writes the window's output uses it.
type entryOutput struct {
	filter    *termfilter.Filter
	resets    int  // the filter's Resets, to turn focus reports on again
	focusOwed bool // focus reports to turn back on after a reset
}

// frame appends one batch as the ssh client gets it. replay marks history
// the owner may have seen elsewhere: it only draws, its queries, clipboard
// writes and notifications already acted. restart is output that does not
// continue what the window shows (a re-join, or history past what the
// client received): what the last bytes left open is ended and the window
// cleared first, as the filter clears for ESC c (Clear). The scrollback
// erase in it passes, attach's default; the entry has no --keep-scrollback.
func (o *entryOutput) frame(out, data []byte, replay, restart bool) []byte {
	if restart {
		// Focus reports go back on at once, as attach's restart flushes.
		out = append(o.filter.Clear(o.filter.Abort(out)), entryFocusOn...)
		o.focusOwed = false
	}
	o.filter.SetReplay(replay)
	out = o.filter.Append(out, data)
	o.filter.SetReplay(false)
	if resets := o.filter.Resets(); resets != o.resets {
		// A full or soft reset turned focus reports off as well.
		o.resets, o.focusOwed = resets, true
	}
	// Nothing of the window's own goes inside a clipboard write the program
	// left open.
	if o.focusOwed && !o.filter.Streaming() {
		out, o.focusOwed = append(out, entryFocusOn...), false
	}
	return out
}

// entryPresence says whether the owner is at the ssh client's window, for
// clipboard writes: cage attach's rule (attach.ownerHere). The window
// reports focus once entryFocusOn turned that on; in an application that
// never reports focus, a write passes within attach.ClipboardWindow of the
// owner's last key or mouse input in that window. A write that arrives
// otherwise is dropped, not kept for later.
type entryPresence struct {
	focus     atomic.Int32 // entryFocusUnknown, entryFocusIn or entryFocusOut
	lastInput atomic.Int64 // when the owner last typed or used the mouse (Unix ns)
	inPaste   bool         // the input goroutine's: inside a bracketed paste
}

const (
	entryFocusUnknown int32 = iota
	entryFocusIn
	entryFocusOut
)

const (
	entryPasteStart = "\x1b[200~"
	entryPasteEnd   = "\x1b[201~"
)

func (p *entryPresence) here() bool {
	switch p.focus.Load() {
	case entryFocusIn:
		return true
	case entryFocusOut:
		return false
	}
	last := p.lastInput.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < attach.ClipboardWindow
}

// note reads the client's input as cage attach reads its window's
// (attach.noteInput): focus reports set the state and reach the program only
// when it asked for them (guestFocus); keys, pastes and mouse buttons mark
// the owner as here; pointer motion and answers to queries do not.
func (p *entryPresence) note(data []byte, guestFocus func() bool) []byte {
	out := make([]byte, 0, len(data))
	owner := false
	for i := 0; i < len(data); {
		if p.inPaste {
			owner = true
			end := bytes.Index(data[i:], []byte(entryPasteEnd))
			if end < 0 {
				out = append(out, data[i:]...)
				break
			}
			end += len(entryPasteEnd)
			out = append(out, data[i:i+end]...)
			i += end
			p.inPaste = false
			continue
		}
		n, kind, _ := termfilter.NextInput(data[i:])
		unit := data[i : i+n]
		i += n
		switch kind {
		case termfilter.InputFocusIn, termfilter.InputFocusOut:
			state := entryFocusIn
			if kind == termfilter.InputFocusOut {
				state = entryFocusOut
			}
			p.focus.Store(state)
			if !guestFocus() {
				continue
			}
		case termfilter.InputKey:
			owner = true
			p.inPaste = string(unit) == entryPasteStart
		case termfilter.InputMouse:
			owner = true
		}
		out = append(out, unit...)
	}
	if owner {
		p.lastInput.Store(time.Now().UnixNano())
	}
	return out
}

// entryViewerID is the viewer identity of one ssh window, of its own kind
// beside cage attach's "attach-" (terminalViewerPattern).
func entryViewerID() string { return "ssh-" + randomToken()[:24] }

func entryExitStatus(channel ssh.Channel, status int) {
	if status < 0 || status > 255 {
		status = 255
	}
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
}

// entrySession serves one session channel: its requests in this goroutine,
// its program in another.
func (s *Server) entrySession(conn *entryConn, incoming ssh.NewChannel) {
	channel, requests, err := incoming.Accept()
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(conn.ctx)
	var program sync.WaitGroup
	defer func() {
		cancel()
		program.Wait()
		_ = channel.Close()
	}()
	var pty *entryPTYRequest
	var resize func(cols, rows uint32)
	var hangup func()
	started := false
	start := func(run func()) {
		started = true
		program.Add(1)
		go func() {
			defer program.Done()
			defer entryGuard(channel, pty != nil)
			run()
		}()
	}
	for request := range requests {
		switch request.Type {
		case "pty-req":
			var input entryPTYRequest
			if started || pty != nil || ssh.Unmarshal(request.Payload, &input) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			pty = &input
			_ = request.Reply(true, nil)
		case "window-change":
			var input entryWindowChange
			if ssh.Unmarshal(request.Payload, &input) == nil && input.Cols < 1<<16 && input.Rows < 1<<16 {
				if resize != nil {
					resize(input.Cols, input.Rows)
				} else if pty != nil {
					pty.Cols, pty.Rows = input.Cols, input.Rows
				}
			}
			_ = request.Reply(false, nil)
		case "shell", "exec", "subsystem":
			if started {
				_ = request.Reply(false, nil)
				continue
			}
			var command string
			if request.Type != "shell" {
				var input struct{ Value string }
				if ssh.Unmarshal(request.Payload, &input) != nil {
					_ = request.Reply(false, nil)
					continue
				}
				command = input.Value
			}
			switch {
			case request.Type == "subsystem" && command != "sftp":
				conn.refuse("subsystem:" + command)
				_ = request.Reply(false, nil)
				continue
			case request.Type == "subsystem" && pty != nil:
				// N3 of the second review: sftp's bytes go unfiltered, so not
				// to a terminal (ssh -t -s). sftp, scp and rsync never ask
				// for a pty.
				conn.refuse("subsystem-pty")
				_ = request.Reply(false, nil)
				continue
			}
			herdr, session := entryHerdrNone, ""
			if request.Type == "exec" {
				herdr, session = entryHerdrClassify(command)
			}
			refusal := ""
			switch {
			case herdr == entryHerdrTUI:
				refusal = entryHerdrTUIRefusal
			case herdr == entryHerdrDrift, herdr == entryHerdrConnect && pty != nil:
				// Herdr never asks for a terminal on its link.
				refusal = entryHerdrDriftRefusal
			}
			if refusal != "" {
				conn.refuse("herdr")
				_ = request.Reply(true, nil)
				start(func() {
					_, _ = io.WriteString(channel.Stderr(), refusal+"\n")
					entryExitStatus(channel, 1)
					_ = channel.Close()
				})
				continue
			}
			if herdr == entryHerdrConnect {
				_ = request.Reply(true, nil)
				start(func() { s.entryHerdrLink(ctx, conn, channel, session) })
				continue
			}
			_ = request.Reply(true, nil)
			if request.Type == "subsystem" {
				hopCtx, stopHop := context.WithCancel(ctx)
				hangup = stopHop
				start(func() { defer stopHop(); s.entryRunHop(hopCtx, conn, channel, "sftp", "") })
				continue
			}
			if pty != nil {
				view := &entryTerminalView{cols: pty.Cols, rows: pty.Rows}
				resize = view.resize
				request := *pty
				// Only an interactive ssh -t is offered the terminals a lock
				// left running; a command always gets a new terminal.
				offer := command == ""
				start(func() { s.entryTerminal(ctx, conn, channel, view, request, command, offer) })
				continue
			}
			mode := "exec"
			switch {
			case request.Type == "shell":
				mode = "shell"
			case herdr == entryHerdrInstall:
				// Herdr's install restarts its core: with Herdr's
				// environment, so it is the core the link reaches.
				mode = "herdr-exec"
			}
			hopCtx, stopHop := context.WithCancel(ctx)
			hangup = stopHop
			start(func() { defer stopHop(); s.entryRunHop(hopCtx, conn, channel, mode, command) })
		case "signal":
			// The hop is an OpenSSH client, which cannot pass a signal on.
			// INT, TERM and HUP end it, and the guest command then gets
			// SIGHUP from its ended session (run's pdeathsig).
			var input struct{ Name string }
			if ssh.Unmarshal(request.Payload, &input) == nil && hangup != nil && (input.Name == "INT" || input.Name == "TERM" || input.Name == "HUP") {
				hangup()
			}
			_ = request.Reply(false, nil)
		case "keepalive@openssh.com":
			_ = request.Reply(true, nil)
		case "eow@openssh.com":
			_ = request.Reply(false, nil)
		default:
			// env (SendEnv/SetEnv, LANG and LC_* from macOS), x11-req,
			// auth-agent-req@openssh.com, break and anything unknown.
			conn.refuse(request.Type)
			_ = request.Reply(false, nil)
		}
	}
}

// entryRunHop runs one channel in the guest over Cage's own hop, the way
// guestControl runs its scripts, through the guest's run script.
func (s *Server) entryRunHop(ctx context.Context, conn *entryConn, channel ssh.Channel, mode, command string) {
	counter := &conn.execs
	if mode == "sftp" {
		counter = &conn.sftps
	}
	counter.Add(1)
	defer counter.Add(-1)
	status := 255
	defer func() {
		entryExitStatus(channel, status)
		_ = channel.Close()
	}()
	if err := s.entryEnsureWrapper(ctx); err != nil {
		_, _ = io.WriteString(channel.Stderr(), "Super Cage："+err.Error()+"\n")
		return
	}
	if mode == "herdr-exec" {
		// The install restarted the core: it stops again when idle.
		defer s.entryHerdrIdle(entryHerdrInstallSession(command))
	}
	// Like guestControl: sshd hands the line to the guest user's login
	// shell, which exec replaces at once with a bash that reads no profile
	// or rc file, and that bash with the run script: nothing the guest
	// wrote in those files runs before it, and no shell stays between the
	// guest's sshd and the command.
	script := s.entryRunScript(mode)
	if mode == "exec" || mode == "herdr-exec" {
		script = s.entryRunScript(mode, command)
	}
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", script)
	cmd.WaitDelay = entryHopWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		_, _ = io.WriteString(channel.Stderr(), "Super Cage：无法连接环境\n")
		return
	}
	go func() {
		// The client's EOF reaches the guest command as its stdin's end.
		_, _ = io.Copy(stdin, channel)
		_ = stdin.Close()
	}()
	var outputs sync.WaitGroup
	outputs.Add(2)
	go func() { defer outputs.Done(); _, _ = io.Copy(channel, stdout) }()
	go func() { defer outputs.Done(); _, _ = io.Copy(channel.Stderr(), stderr) }()
	// Everything the command wrote reaches the client before its status.
	outputs.Wait()
	err = cmd.Wait()
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
		status = 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		status = exit.ExitCode()
	}
}

// createEntryTerminal opens a Cage terminal for an ssh session with a pty:
// the same script, hop and limit as a terminal from the menu (kind shell),
// with the client's window size and the device as its origin. The client's
// TERM is not used: every Cage terminal is xterm-256color (design 5.8).
// entryLockOperation takes the environment's operation lock, waiting
// briefly: several ssh -t sessions often start together (a script, a tmux
// layout, Herdr's TUI), and each must get its terminal instead of "环境正在执
// 行其他操作". It retries like lockProjectOperation and stops at once when the
// connection is cut.
func (s *Server) entryLockOperation(ctx context.Context) bool {
	if s.op.TryLock() {
		return true
	}
	wait, cancel := context.WithTimeout(ctx, entryOperationWait)
	defer cancel()
	retry := time.NewTicker(projectOperationRetry)
	defer retry.Stop()
	for {
		select {
		case <-wait.Done():
			return false
		case <-retry.C:
			if s.op.TryLock() {
				if ctx.Err() != nil {
					s.op.Unlock()
					return false
				}
				return true
			}
		}
	}
}

func (s *Server) createEntryTerminal(conn *entryConn, pty entryPTYRequest, command string) (*terminalSession, error) {
	if !s.entryLockOperation(conn.ctx) {
		return nil, errTerminalBusy
	}
	defer s.op.Unlock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if s.macosGuest != nil || s.windowsGuest != nil {
		return nil, errEntryUnsupported
	}
	if !s.vmRunning() || s.busy() {
		return nil, errTerminalNotReady
	}
	if err := s.terminals.canStart(); err != nil {
		return nil, err
	}
	workspace, err := s.connectProject("", false)
	if err != nil {
		return nil, err
	}
	script := terminalScript(s.cliWorkspace(workspace), "shell", "")
	pid := ""
	if command != "" {
		script = entryCommandScript(s.cliWorkspace(workspace), command)
	} else {
		pid = randomToken()[:16]
		script = entryPIDScript(pid) + script
	}
	cols, rows := uint16(100), uint16(30)
	if validTerminalSize(uint16(min(pty.Cols, 1<<16-1)), uint16(min(pty.Rows, 1<<16-1))) {
		cols, rows = uint16(pty.Cols), uint16(pty.Rows)
	}
	cmd := s.sshCommand(s.lifetimeContext(), "-tt", s.sshTarget(), "--", script)
	session, err := s.terminals.startSessionWithPTY(cmd, workspace, "shell", claudeSelection{}, nil, func() (terminalPTY, error) {
		return startTerminal(cmd, cols, rows)
	})
	if err != nil {
		return nil, err
	}
	name := "这台电脑"
	if st, err := entryLoadState(s.profile); err == nil {
		if record := st.device(conn.device); record != nil {
			name = record.Name
		}
	}
	session.mu.Lock()
	session.origin, session.originDevice, session.originID, session.originPID = "ssh", name, conn.device, pid
	session.mu.Unlock()
	return session, nil
}

// entryCommandScript is terminalScript for "ssh -t host command": the
// command runs in the same environment as a terminal's shell.
func entryCommandScript(workspace, command string) string {
	return "cd " + shellescape.Quote(workspace) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 bash -lc " + shellescape.Quote(command)
}

// entryTerminalView is the client's window and, once chosen, the terminal a
// pty session shows with its viewer peer. window-change requests arrive
// before the terminal exists while the session offers to re-join one. Sizes
// apply by the viewer rules (peerResize), as for cage attach: a re-joined
// terminal takes this window's size only when no other window decides it.
type entryTerminalView struct {
	mu         sync.Mutex
	cols, rows uint32
	session    *terminalSession
	peer       *terminalPeer
}

func (v *entryTerminalView) resize(cols, rows uint32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cols, v.rows = cols, rows
	v.applyLocked()
}

// show makes session, seen through peer, the one the client's size applies
// to; nil stops that.
func (v *entryTerminalView) show(session *terminalSession, peer *terminalPeer) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.session, v.peer = session, peer
	v.applyLocked()
}

func (v *entryTerminalView) applyLocked() {
	if v.session == nil || v.cols >= 1<<16 || v.rows >= 1<<16 || !validTerminalSize(uint16(v.cols), uint16(v.rows)) {
		return
	}
	_ = v.session.peerResize(v.peer, uint16(v.cols), uint16(v.rows))
}

func (v *entryTerminalView) size() (uint32, uint32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cols, v.rows
}

// entryTerminal is the program of a session with a pty: it re-joins a
// terminal a lock left running when the owner picks one, or opens a new one,
// and shows it.
func (s *Server) entryTerminal(ctx context.Context, conn *entryConn, channel ssh.Channel, view *entryTerminalView, pty entryPTYRequest, command string, offer bool) {
	var session *terminalSession
	var typed []byte
	if offer {
		if waiting := s.entryWaitingTerminals(conn.device); len(waiting) > 0 {
			chosen, rest, ok := entryOfferRejoin(channel, waiting)
			if !ok {
				entryExitStatus(channel, 0)
				_ = channel.Close()
				return
			}
			if chosen != nil && !chosen.isClosed() {
				session, typed = chosen, rest
			}
		}
	}
	rejoin := session != nil
	if !rejoin {
		pty.Cols, pty.Rows = view.size()
		created, err := s.createEntryTerminal(conn, pty, command)
		if err != nil {
			message := err.Error()
			if errors.Is(err, errTerminalLimit) {
				message = entryTerminalLimitText
			}
			_, _ = io.WriteString(channel, "Super Cage："+message+"\r\n")
			entryExitStatus(channel, 1)
			_ = channel.Close()
			return
		}
		session = created
	}
	s.entryRunTerminal(ctx, conn, channel, view, session, rejoin, typed)
}

// entryWaitingTerminals are this device's ssh terminals that run without an
// ssh client: the ones a lock cut, or the owner kept.
func (s *Server) entryWaitingTerminals(device string) []*terminalSession {
	s.terminals.mu.Lock()
	sessions := make([]*terminalSession, 0, len(s.terminals.sessions))
	for _, session := range s.terminals.sessions {
		sessions = append(sessions, session)
	}
	s.terminals.mu.Unlock()
	var waiting []*terminalSession
	for _, session := range sessions {
		session.mu.Lock()
		ok := session.origin == "ssh" && session.originID == device && session.kept && session.sshViewers == 0 && !session.closed
		session.mu.Unlock()
		if ok {
			waiting = append(waiting, session)
		}
	}
	slices.SortFunc(waiting, func(a, b *terminalSession) int { return a.sequence - b.sequence })
	return waiting
}

func (t *terminalSession) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// entryOfferRejoin lists the waiting terminals on the client's terminal and
// reads the owner's choice: a number re-joins that terminal, Enter alone
// opens a new one (chosen nil), Ctrl-C or Ctrl-D ends the session (ok
// false). What the owner typed after Enter is returned for the terminal.
func entryOfferRejoin(channel ssh.Channel, waiting []*terminalSession) (chosen *terminalSession, rest []byte, ok bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "Super Cage：这台电脑有 %d 个终端仍在运行：\r\n", len(waiting))
	for i, session := range waiting {
		session.mu.Lock()
		title := session.title
		session.mu.Unlock()
		fmt.Fprintf(&b, "  %d  %s\r\n", i+1, entryPrintable(title, 48))
	}
	prompt := "输入编号重新接入，直接按回车打开新终端，Ctrl-C 退出："
	b.WriteString(prompt)
	if _, err := io.WriteString(channel, b.String()); err != nil {
		return nil, nil, false
	}
	var answer []byte
	buffer := make([]byte, 256)
	for {
		n, err := channel.Read(buffer)
		for i := 0; i < n; i++ {
			switch c := buffer[i]; {
			case c >= '0' && c <= '9' && len(answer) < 3:
				answer = append(answer, c)
				_, _ = channel.Write([]byte{c})
			case c == 0x7f || c == 0x08:
				if len(answer) > 0 {
					answer = answer[:len(answer)-1]
					_, _ = io.WriteString(channel, "\b \b")
				}
			case c == 0x03 || c == 0x04:
				_, _ = io.WriteString(channel, "\r\n")
				return nil, nil, false
			case c == '\r' || c == '\n':
				_, _ = io.WriteString(channel, "\r\n")
				if len(answer) == 0 {
					return nil, nil, true
				}
				if number, err := strconv.Atoi(string(answer)); err == nil && number >= 1 && number <= len(waiting) {
					rest = bytes.TrimLeft(buffer[i+1:n], "\n")
					return waiting[number-1], append([]byte(nil), rest...), true
				}
				answer = answer[:0]
				_, _ = io.WriteString(channel, "没有这个编号。"+prompt)
			}
		}
		if err != nil {
			return nil, nil, false
		}
	}
}

// entryPrintable is text from the guest (a terminal's title) made safe to
// print: no control characters, at most limit runes.
func entryPrintable(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	if utf8.RuneCountInString(text) > limit {
		text = string([]rune(text)[:limit-1]) + "…"
	}
	if text == "" {
		return "终端"
	}
	return text
}

// entryRunTerminal makes the channel a viewer of the Cage terminal. Like
// ssh, closing the connection ends the terminal, unless the owner kept it;
// a lock keeps it (F4 of the review): the work goes on, and after the
// owner unlocks, a new ssh -t offers to re-join it.
//
// The channel is a window as cage attach's is, with the same escape filter
// and settings (the per-computer local-terminal.json): the title is saved
// before the first output, every byte from the guest goes through the
// filter, history only draws, and every way out (the program ends, the
// owner hangs up, a cut by off, revoke or a closing Super Cage, a lock)
// writes the filter's exit reset (End) before Super Cage's own last line
// and the exit status. A cut waits up to entryCutGrace for that
// (conn.flush). It is a viewer of its own identity under the viewer rules:
// answers to queries pass only from the size owner, typing takes the size.
// Dropped images are not uploaded over ssh: pastes reach the program as text.
func (s *Server) entryRunTerminal(ctx context.Context, conn *entryConn, channel ssh.Channel, view *entryTerminalView, session *terminalSession, rejoin bool, typed []byte) {
	conn.shells.Add(1)
	defer conn.shells.Add(-1)
	conn.flush.begin()
	defer conn.flush.end()
	session.mu.Lock()
	session.sshViewers++
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.sshViewers--
		session.mu.Unlock()
	}()
	settings := localterm.DefaultSettings()
	if paths, err := s.entryPaths(); err == nil {
		settings, _ = localterm.LoadSettings(paths.ComputerDir)
	}
	presence := &entryPresence{}
	output := &entryOutput{filter: termfilter.New(termfilter.Options{ClipboardWrite: settings.ClipboardWrite, ClipboardNow: presence.here})}
	// An internal error in either direction ends this session only (R3-2):
	// its window gets the exit reset, the controller and the connection's
	// other sessions go on.
	run, stopRun := context.WithCancel(ctx)
	defer stopRun()
	var failed atomic.Bool
	// The peer exists before the subscription, so queries in live output
	// are counted for this window from the first byte.
	peer := session.openPeer(entryViewerID(), false)
	defer session.closePeer(peer)
	view.show(session, peer)
	defer view.show(nil, nil)
	subscription := session.subscribe()
	defer subscription.Close()
	outputDone := make(chan struct{})
	var outputOnce sync.Once
	endOutput := func() { outputOnce.Do(func() { close(outputDone) }) }
	defer func() {
		if r := recover(); r != nil {
			endOutput()
			entryLogPanic("output", r)
			cols, _ := view.size()
			_, _ = channel.Write(entryPanicEnd(output, int(min(cols, 1<<16-1))))
			entryExitStatus(channel, 255)
			_ = channel.Close()
			session.mu.Lock()
			kept := session.kept
			session.mu.Unlock()
			if !kept {
				s.terminals.remove(session.id)
			}
		}
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				entryLogPanic("input", r)
				failed.Store(true)
				stopRun()
			}
		}()
		send := func(data []byte) bool {
			if data = entryNote(presence, data, output.filter.GuestFocus); len(data) == 0 {
				return true
			}
			input, err := session.peerInput(peer, string(data))
			if err != nil {
				return false
			}
			return input == "" || session.enqueueInputWait(ctx, input, outputDone) == nil
		}
		if len(typed) > 0 && !send(typed) {
			return
		}
		buffer := make([]byte, 32<<10)
		for {
			n, err := channel.Read(buffer)
			if n > 0 && !send(buffer[:n]) {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	// History only draws, as in cage attach; a re-joined terminal's does not
	// continue what the client's window shows.
	session.showQueries(peer, subscription.History)
	out := append([]byte(termfilter.TitlePush), entryFocusOn...)
	out = entryFrame(output, out, subscription.History, true, rejoin)
	ended := false
	_, err := channel.Write(out)
	for err == nil && run.Err() == nil {
		batch := subscription.next(terminalFrameLimit)
		if len(batch.Data) > 0 {
			if batch.Resync {
				// The client fell behind: the kept history again, as a
				// viewer that resumes gets it.
				session.showQueries(peer, batch.Data)
			}
			out = entryFrame(output, out[:0], batch.Data, batch.Resync, batch.Resync && batch.Reset)
			_, err = channel.Write(out)
			continue
		}
		if batch.Done {
			ended = batch.Ended
			break
		}
		select {
		case <-run.Done():
		case <-subscription.Wake():
			continue
		}
		break
	}
	endOutput()
	locked := !ended && !failed.Load() && conn.epoch.Err() != nil
	// A lock keeps the terminal, unless entryKeptLimit terminals are kept
	// already: then it ends, and says so (entryKeepOnLock decides which).
	full := locked && !s.entryKeepOnLock(conn.epoch, session)
	session.mu.Lock()
	kept := session.kept
	session.mu.Unlock()
	cols, _ := view.size()
	// The exit reset on every way out; nothing from the guest follows it.
	// To a client that is gone the write fails, which costs nothing.
	tail := output.filter.End(nil, int(min(cols, 1<<16-1)))
	switch cause := context.Cause(conn.ctx); {
	case ended:
	case failed.Load():
		tail = append(tail, entryInternalError...)
	case full:
		tail = append(tail, "\r\nSuper Cage 已锁定："+errEntryKeptFull().Error()+"，这个终端随连接结束。\r\n"...)
	case locked:
		tail = append(tail, "\r\n"+entryLockedNotice+"\r\n"...)
	case conn.ctx.Err() != nil:
		if refusal, ok := cause.(*entryRefusal); ok {
			tail = append(tail, "\r\nSuper Cage："+refusal.Message+"\r\n"...)
		}
	}
	_, _ = channel.Write(tail)
	if ended {
		code := session.exitStatus()
		if code < 0 {
			code = 129
		}
		entryExitStatus(channel, code)
	}
	_ = channel.Close()
	if !kept {
		s.terminals.remove(session.id)
	}
}

// entryFrame and entryNote are an ssh terminal's output and input steps,
// variables so that a test can make one fail (R3-2).
var (
	entryFrame = (*entryOutput).frame
	entryNote  = (*entryPresence).note
)

// entryInternalError is the line after the exit reset when an internal
// error ended the session.
const entryInternalError = "\r\nSuper Cage：内部错误，这个连接的终端已结束。\r\n"

// entryLogPanic records an internal error that ended one session, with
// where it happened.
func entryLogPanic(where string, r any) {
	stack := debug.Stack()
	if len(stack) > 4<<10 {
		stack = stack[:4<<10]
	}
	diagnostics.Log("error", "entry", fmt.Sprintf("an ssh session's %s ended on an internal error: %v\n%s", where, r, stack))
}

// entryPanicEnd is the exit reset after an internal error: the window's
// own filter's, or a fresh one's should that fail too.
func entryPanicEnd(output *entryOutput, cols int) (tail []byte) {
	defer func() {
		if recover() != nil {
			tail = termfilter.New(termfilter.Options{}).End(nil, cols)
		}
		tail = append(tail, entryInternalError...)
	}()
	return output.filter.End(nil, cols)
}

// entryGuard ends one session on an internal error outside its terminal's
// own loop (a rejoin offer, a hop, a Herdr line), not the controller: it
// logs, gives a terminal the exit reset and closes the channel.
func entryGuard(channel ssh.Channel, terminal bool) {
	r := recover()
	if r == nil {
		return
	}
	entryLogPanic("session", r)
	if terminal {
		_, _ = channel.Write(append(termfilter.New(termfilter.Options{}).End(nil, 0), entryInternalError...))
	}
	entryExitStatus(channel, 255)
	_ = channel.Close()
}

// entryWSConn carries one byte stream in binary WebSocket messages.
type entryWSConn struct {
	ws              *websocket.Conn
	readMu, writeMu sync.Mutex
	reader          io.Reader
}

func newEntryWSConn(ws *websocket.Conn) *entryWSConn {
	ws.SetReadLimit(1 << 20)
	return &entryWSConn{ws: ws}
}

func (c *entryWSConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		if c.reader == nil {
			kind, reader, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

func (c *entryWSConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *entryWSConn) Close() error                       { return c.ws.Close() }
func (c *entryWSConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *entryWSConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *entryWSConn) SetDeadline(t time.Time) error      { return nil }
func (c *entryWSConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *entryWSConn) SetWriteDeadline(t time.Time) error { return nil }
