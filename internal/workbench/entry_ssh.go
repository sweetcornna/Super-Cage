package workbench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// The Cage entry endpoint (design 5.4): an SSH server that runs on one
// WebSocket at a time, inside this controller. SSH from the owner's own
// tools ends here, in Cage's code, never in the guest. Every rule is a
// missing handler: the endpoint knows session channels and nothing else.
//   - publickey for the enrolled device key only; no other method.
//   - session channels (at most entryMaxChannels per connection):
//       pty + shell/exec  -> a Cage terminal (listed, on the phone, the
//                            same launcher, the 12-terminal limit);
//       exec, shell        -> the guest's run script over Cage's own hop;
//       subsystem sftp     -> the guest's sftp-server over the same hop;
//       Herdr's connect line -> refused until step 3 brings its filter.
//   - no handler for any forwarding, agent, X11, tunnel or client
//     environment: those channels are rejected, those requests answered no.

const (
	entryMaxChannels       = 8
	entryHandshakeTimeout  = 20 * time.Second
	entryWrapperTimeout    = 20 * time.Second
	entryHopWaitDelay      = 2 * time.Second
	entryServerVersion     = "SSH-2.0-SuperCage"
	entryHerdrRefusal      = "Super Cage：本机终端还不能接入 Herdr，需要更新 Super Cage 到支持 Herdr 的版本。"
	entryTerminalLimitText = "已有 12 个终端：先关闭一个"
)

// entryService is the Server's local SSH entry.
type entryService struct {
	registry entryRegistry
	// mu serialises changes to the entry's records and the wrapper install.
	mu           sync.Mutex
	wrapperReady bool
	// Test seams: injected paths, the guest's run script and its install.
	paths   *entryPaths
	runPath string
	install func(context.Context) error
}

// entryOutputFilter is the hook for the escape filter of design 5.7
// (internal/termfilter, built on branch local-terminal-attach). Until it is
// plugged in, the output of the entry's terminal sessions reaches the owner's
// terminal application unfiltered, and
// TestEntryTerminalOutputUsesEscapeFilter fails. To plug it in:
//
//	entryNewOutputFilter = func(clipboardWrite bool) entryOutputFilter {
//		return termfilter.New(termfilter.Options{ClipboardWrite: clipboardWrite})
//	}
type entryOutputFilter interface {
	Append(dst, src []byte) []byte
}

// entryNewOutputFilter returns the filter of one terminal session, or nil.
// HOOK(termfilter): nil until internal/termfilter is merged.
var entryNewOutputFilter func(clipboardWrite bool) entryOutputFilter

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
	s.entry.mu.Lock()
	s.entry.wrapperReady = false
	s.entry.mu.Unlock()
}

// entryEnsureWrapper installs the guest's run script the first time a channel
// needs it after a guest start (cage.EntryInstallScript).
func (s *Server) entryEnsureWrapper(ctx context.Context) error {
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	if s.entry.wrapperReady {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, entryWrapperTimeout)
	defer cancel()
	install := s.entry.install
	if install == nil {
		install = func(ctx context.Context) error {
			_, err := s.guestControl(ctx, cage.EntryInstallScript(cage.EntryRunPath), "entry")
			return err
		}
	}
	if err := install(ctx); err != nil {
		return errors.New("无法在环境里准备本机终端组件，请确认环境正在运行")
	}
	s.entry.wrapperReady = true
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
	if record.Revoked != nil {
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
	device := r.URL.Query().Get("device")
	conn := &entryConn{device: device}
	conn.ctx, conn.cancel = context.WithCancelCause(r.Context())
	defer conn.cancel(nil)
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
	header := http.Header{"Cage-Entry-Host-Key": {strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}}
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
	stop := context.AfterFunc(conn.ctx, func() { _ = transport.Close() })
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
	closeServer := context.AfterFunc(conn.ctx, func() { _ = server.Close() })
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
	if enrolled, _, _, _, err := ssh.ParseAuthorizedKey([]byte(record.PublicKey)); err == nil && entryKeysEqual(enrolled, key) {
		return true
	}
	paths, err := s.entryPaths()
	if err != nil {
		return false
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

// entryHerdrConnect recognises Herdr's connect line (herdr-core
// machines.rs:242-266, "exec \"$p\" connect" after locating herdr-core) and
// drifted variants of it: any command that names herdr-core, or hc as a
// word, together with the word connect. Until step 3 brings the Herdr filter
// these are refused, so a newer Herdr whose line changed still cannot open
// an unfiltered link. Probes and installs (uname -sm, herdr-core --version,
// herdr-core server restart) do not say connect and stay ordinary commands.
var (
	entryHerdrName    = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])hc([^A-Za-z0-9_-]|$)`)
	entryHerdrConnect = regexp.MustCompile(`(^|[^A-Za-z0-9_-])connect([^A-Za-z0-9_-]|$)`)
)

func entryHerdrLine(command string) bool {
	return (strings.Contains(command, "herdr-core") || entryHerdrName.MatchString(command)) && entryHerdrConnect.MatchString(command)
}

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
	var resize func(cols, rows uint16)
	var hangup func()
	started := false
	start := func(run func()) {
		started = true
		program.Add(1)
		go func() {
			defer program.Done()
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
					resize(uint16(input.Cols), uint16(input.Rows))
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
			case request.Type == "exec" && entryHerdrLine(command):
				conn.refuse("herdr")
				_ = request.Reply(true, nil)
				start(func() {
					_, _ = io.WriteString(channel.Stderr(), entryHerdrRefusal+"\n")
					entryExitStatus(channel, 1)
					_ = channel.Close()
				})
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
				terminal, err := s.createEntryTerminal(conn, *pty, command)
				if err != nil {
					message := err.Error()
					if errors.Is(err, errTerminalLimit) {
						message = entryTerminalLimitText
					}
					start(func() {
						_, _ = io.WriteString(channel, "Super Cage："+message+"\r\n")
						entryExitStatus(channel, 1)
						_ = channel.Close()
					})
					continue
				}
				resize = func(cols, rows uint16) { entryResize(terminal, cols, rows) }
				start(func() { s.entryRunTerminal(ctx, conn, channel, terminal) })
				continue
			}
			mode := "exec"
			if request.Type == "shell" {
				mode = "shell"
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
	script := shellescape.Quote(s.entryRunPath()) + " " + mode
	if mode == "exec" {
		script += " " + shellescape.Quote(command)
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
func (s *Server) createEntryTerminal(conn *entryConn, pty entryPTYRequest, command string) (*terminalSession, error) {
	if !s.op.TryLock() {
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
	if command != "" {
		script = entryCommandScript(s.cliWorkspace(workspace), command)
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
	session.origin, session.originDevice = "ssh", name
	session.mu.Unlock()
	return session, nil
}

// entryCommandScript is terminalScript for "ssh -t host command": the
// command runs in the same environment as a terminal's shell.
func entryCommandScript(workspace, command string) string {
	return "cd " + shellescape.Quote(workspace) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 bash -lc " + shellescape.Quote(command)
}

// entryResize applies the ssh client's window size.
// HOOK(viewers): after local-terminal-attach is merged, give the ssh viewer a
// peer (openPeer) and use peerInput/peerResize like terminalStream, so the
// size owner rules hold for it too.
func entryResize(session *terminalSession, cols, rows uint16) {
	if validTerminalSize(cols, rows) {
		_ = session.file.Resize(cols, rows)
	}
}

// entryRunTerminal makes the channel a viewer of the Cage terminal. Like
// ssh, closing the connection ends the terminal, unless the owner kept it.
func (s *Server) entryRunTerminal(ctx context.Context, conn *entryConn, channel ssh.Channel, session *terminalSession) {
	conn.shells.Add(1)
	defer conn.shells.Add(-1)
	paths, _ := s.entryPaths()
	var filter entryOutputFilter
	if entryNewOutputFilter != nil {
		filter = entryNewOutputFilter(entryLoadLocalSettings(paths).ClipboardWrite)
	}
	subscription := session.subscribe()
	defer subscription.Close()
	outputDone := make(chan struct{})
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, err := channel.Read(buffer)
			if n > 0 && session.enqueueInputWait(ctx, string(buffer[:n]), outputDone) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	ended := false
	var out []byte
	for !ended {
		batch := subscription.next(terminalFrameLimit)
		if len(batch.Data) > 0 {
			data := batch.Data
			if filter != nil {
				out = filter.Append(out[:0], data)
				data = out
			}
			if _, err := channel.Write(data); err != nil {
				break
			}
			continue
		}
		if batch.Done {
			ended = batch.Ended
			break
		}
		select {
		case <-ctx.Done():
		case <-subscription.Wake():
			continue
		}
		break
	}
	close(outputDone)
	session.mu.Lock()
	kept := session.kept
	session.mu.Unlock()
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
