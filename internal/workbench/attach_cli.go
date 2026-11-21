package workbench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/localterm"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// cage attach shows a Cage terminal in the terminal application it runs in.
// It speaks the same terminal stream as the desktop's web terminal, through
// the same authenticated controller connection as cage agent: no new
// credential and no new listener. The terminal keeps running when the
// window closes, like a closed tab.

const attachUsage = "用法：cage attach [--terminal ID | --new shell|claude|codex | --resume 会话ID] [--project 路径] [--claude-account ID | --codex-account ID] [--connection ID|current] [--observe] [--config 路径]"

// attachConfig is what one cage attach was asked to do.
type attachConfig struct {
	configPath    string
	target        AgentTarget
	terminal      string
	newKind       string
	project       string
	claudeAccount string
	codexAccount  string
	resume        string
	observe       bool
}

// attachTTY is the local terminal. The real one is a raw-mode tty; tests use
// a pseudo-terminal or pipes.
type attachTTY struct {
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	size    func() (cols, rows int, ok bool)
	resized <-chan struct{}
	makeRaw func() (restore func(), err error)
	// closed ends the attach without ending the terminal: the window closed.
	closed <-chan struct{}
}

// attachExit is how an attach ended: its exit code and the one line it
// prints once the local terminal is restored.
type attachExit struct {
	code    int
	message string
}

var (
	attachTerminalID = regexp.MustCompile("^[0-9a-f]{16}$")
	attachSessionID  = regexp.MustCompile("^[A-Za-z0-9_.-]{1,128}$")
	attachAccountID  = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")
)

// AttachMain runs cage attach and returns its exit code.
func AttachMain(args []string, stdin, stdout *os.File, stderr io.Writer) int {
	cfg, err := parseAttachArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	tty, stop := attachLocalTTY(stdin, stdout, stderr)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-tty.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	result := runAttach(ctx, cfg, tty)
	if result.message != "" {
		fmt.Fprint(stderr, "\r\n"+result.message+"\r\n")
	}
	return result.code
}

func parseAttachArgs(args []string) (attachConfig, error) {
	var cfg attachConfig
	flags := flag.NewFlagSet("cage attach", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", os.Getenv(cage.ConfigEnv), "")
	connection := flags.String("connection", "", "")
	flags.StringVar(&cfg.terminal, "terminal", "", "")
	flags.StringVar(&cfg.newKind, "new", "", "")
	flags.StringVar(&cfg.project, "project", "", "")
	flags.StringVar(&cfg.claudeAccount, "claude-account", "", "")
	flags.StringVar(&cfg.codexAccount, "codex-account", "", "")
	flags.StringVar(&cfg.resume, "resume", "", "")
	flags.BoolVar(&cfg.observe, "observe", false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() > 1 {
		return cfg, errors.New(attachUsage)
	}
	// The environment operand: "local" (the default) or "current", the
	// desktop's current connection.
	switch flags.Arg(0) {
	case "", "local":
	case "current":
		if *connection != "" {
			return cfg, errors.New(attachUsage)
		}
		*connection = "current"
	default:
		return cfg, errors.New("环境只能是 local 或 current。" + attachUsage)
	}
	chosen := 0
	for _, value := range []string{cfg.terminal, cfg.newKind, cfg.resume} {
		if value != "" {
			chosen++
		}
	}
	switch {
	case chosen > 1:
		return cfg, errors.New("--terminal、--new 和 --resume 只能选一个。" + attachUsage)
	case cfg.terminal != "" && !attachTerminalID.MatchString(cfg.terminal):
		return cfg, errors.New("终端 ID 无效")
	case cfg.resume != "" && !attachSessionID.MatchString(cfg.resume):
		return cfg, errors.New("会话 ID 无效")
	case cfg.newKind != "" && cfg.newKind != "shell" && cfg.newKind != "claude" && cfg.newKind != "codex":
		return cfg, errors.New("--new 只能是 shell、claude 或 codex")
	case cfg.claudeAccount != "" && (cfg.newKind != "claude" || !attachAccountID.MatchString(cfg.claudeAccount)):
		return cfg, errors.New("--claude-account 只能和 --new claude 一起使用")
	case cfg.codexAccount != "" && (cfg.newKind != "codex" || !attachAccountID.MatchString(cfg.codexAccount)):
		return cfg, errors.New("--codex-account 只能和 --new codex 一起使用")
	case cfg.project != "" && cfg.newKind == "":
		return cfg, errors.New("--project 只能和 --new 一起使用")
	case cfg.observe && (cfg.newKind != "" || cfg.resume != ""):
		return cfg, errors.New("--observe 只能查看已有终端")
	}
	if chosen == 0 {
		cfg.newKind = "shell"
	}
	if *config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, err
		}
		*config = filepath.Join(home, ".claude-cage", "config.json")
	}
	path, err := filepath.Abs(*config)
	if err != nil {
		return cfg, err
	}
	cfg.configPath = path
	cfg.target = AgentTarget{Scope: "local"}
	if *connection != "" {
		cfg.target = AgentTarget{Scope: "selected", Connection: *connection}
	}
	if cfg.project != "" && cfg.target.Scope == "local" && !filepath.IsAbs(cfg.project) {
		if cfg.project, err = filepath.Abs(cfg.project); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// attacher is one running attach.
type attacher struct {
	cfg      attachConfig
	tty      attachTTY
	c        *controllerClient
	settings localterm.Settings
	viewer   string
	terminal string

	sendMu sync.Mutex
	ws     *websocket.Conn

	filter  *termfilter.Filter
	buf     []byte
	offset  uint64
	replay  int
	ended   bool
	started bool // the first stream was accepted
}

// attachStreamHook sees every stream an attach opens; tests use it to drop
// one mid-output.
var attachStreamHook func(*websocket.Conn)

// The stream to the controller is a single WebSocket; these bound how long
// a silent one is trusted, as the web terminal does (terminal-reconnect.mjs).
var (
	attachKeepalive   = 25 * time.Second
	attachSilence     = 45 * time.Second
	attachRetryWindow = 60 * time.Second
)

func runAttach(ctx context.Context, cfg attachConfig, tty attachTTY) attachExit {
	a := &attacher{cfg: cfg, tty: tty}
	a.settings, _ = localterm.LoadSettings(filepath.Dir(cfg.configPath))
	a.filter = termfilter.New(termfilter.Options{ClipboardWrite: a.settings.ClipboardWrite})
	a.viewer = attachViewerID()
	if exit, ok := a.connectController(ctx); !ok {
		return exit
	}
	defer a.c.close()
	if exit, ok := a.chooseTerminal(ctx); !ok {
		return exit
	}
	ws, exit, ok := a.dial(ctx)
	if !ok {
		return exit
	}
	restore := func() {}
	if tty.makeRaw != nil {
		var err error
		if restore, err = tty.makeRaw(); err != nil {
			ws.Close()
			return attachExit{1, "无法把这个窗口切换到终端模式：" + err.Error()}
		}
	}
	exit = a.run(ctx, ws)
	restore()
	return exit
}

func attachViewerID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "attach-" + hex.EncodeToString(b[:])
}

func (a *attacher) connectController(ctx context.Context) (attachExit, bool) {
	target := a.cfg.target
	if target.Connection == "current" {
		current, err := attachCurrentConnection(ctx, a.cfg.configPath)
		if err != nil {
			return attachExit{1, err.Error()}, false
		}
		target.Connection = current
		a.cfg.target = target
	}
	c, err := agentConnectTarget(ctx, a.cfg.configPath, target)
	if err != nil {
		return attachFailure(err), false
	}
	c.connectionID = agentConnection(target)
	a.c = c
	return attachExit{}, true
}

// attachCurrentConnection reads the desktop's current connection, so a cloud
// workspace open in the desktop can be attached without copying its id.
func attachCurrentConnection(ctx context.Context, configPath string) (string, error) {
	response, err := AgentRequest(ctx, configPath, AgentTarget{Scope: "desktop"}, "GET", "/api/client/context", nil, "", "")
	if err != nil {
		return "", errors.New(attachFailure(err).message)
	}
	var current ClientContext
	if json.Unmarshal(response.Data, &current) != nil || current.ConnectionID == "" {
		return "", errors.New("Super Cage 没有打开任何环境：先在 Super Cage 里选择一个环境。")
	}
	return current.ConnectionID, nil
}

// attachFailure turns a transport error into the owner's language.
func attachFailure(err error) attachExit {
	var ae *AgentError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "service_unavailable":
			return attachExit{1, "Super Cage 没有在运行：打开 Super Cage 后再运行这条命令。"}
		case "controller_mismatch", "config_path_mismatch":
			return attachExit{1, "这个 cage 命令和正在运行的 Super Cage 不是同一个版本：请用 Super Cage 自带的 cage。"}
		case "account_locked":
			return attachExit{3, "Super Cage 已锁定：先在 Super Cage 里解锁，再运行这条命令。"}
		case "state_conflict":
			return attachExit{1, "Super Cage 已切换到其他环境，这个终端所在的连接已断开。"}
		case "unsupported_operation":
			return attachExit{1, "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"}
		}
	}
	return attachExit{1, "无法连接 Super Cage：" + err.Error()}
}

// request calls the workload's API on the authenticated connection.
func (a *attacher) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.c.origin+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", a.c.origin)
	req.Header.Set(connectionHeader, agentConnection(a.cfg.target))
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *a.c.client
	client.Timeout = 2 * time.Minute
	response, err := client.Do(req)
	if err != nil {
		return &AgentError{Code: "service_unavailable", Message: err.Error()}
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &attachHTTPError{status: response.StatusCode, text: strings.TrimSpace(string(data))}
	}
	if output != nil && json.Unmarshal(data, output) != nil {
		return errors.New("Super Cage 的回答无法读取")
	}
	return nil
}

// attachHTTPError is a refusal from the workload, with its own sentence.
type attachHTTPError struct {
	status int
	text   string
}

func (e *attachHTTPError) Error() string { return e.text }

// message is what the owner reads: the server's sentence, already in the
// owner's language, or the agent transport's reading of the status.
func (e *attachHTTPError) exit() attachExit {
	switch e.status {
	case http.StatusLocked:
		return attachExit{3, "Super Cage 已锁定：先在 Super Cage 里解锁，再运行这条命令。"}
	case http.StatusNotFound:
		return attachExit{1, "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"}
	}
	if e.text != "" && len(e.text) < 300 && !strings.HasPrefix(e.text, "{") {
		return attachExit{1, e.text}
	}
	return attachFailure(agentHTTPError(e.status))
}

// chooseTerminal finds or creates the terminal to show, through the same
// requests the desktop makes, so account selection, pinned accounts and the
// Codex lease follow the desktop's own path.
func (a *attacher) chooseTerminal(ctx context.Context) (attachExit, bool) {
	fail := func(err error) (attachExit, bool) {
		var refused *attachHTTPError
		if errors.As(err, &refused) {
			return refused.exit(), false
		}
		return attachFailure(err), false
	}
	switch {
	case a.cfg.resume != "":
		var view struct {
			Terminal string `json:"desktop_terminal_id"`
		}
		if err := a.request(ctx, "POST", "/api/cli/sessions/"+url.PathEscape(a.cfg.resume)+"/open", map[string]any{}, &view); err != nil {
			return fail(err)
		}
		if !attachTerminalID.MatchString(view.Terminal) {
			return attachExit{1, "这个会话没有在终端中打开：在 Super Cage 里查看它的状态。"}, false
		}
		a.terminal = view.Terminal
	case a.cfg.newKind != "":
		request := map[string]any{"workspace": a.cfg.project, "kind": a.cfg.newKind}
		if a.cfg.claudeAccount != "" {
			request["claude_account_id"] = a.cfg.claudeAccount
		}
		if a.cfg.codexAccount != "" {
			request["codex_account_id"] = a.cfg.codexAccount
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := a.request(ctx, "POST", "/api/terminals", request, &created); err != nil {
			return fail(err)
		}
		if !attachTerminalID.MatchString(created.ID) {
			return attachExit{1, "Super Cage 没有返回新终端"}, false
		}
		a.terminal = created.ID
	default:
		var list []map[string]any
		if err := a.request(ctx, "GET", "/api/terminals", nil, &list); err != nil {
			return fail(err)
		}
		for _, item := range list {
			if item["id"] == a.cfg.terminal {
				a.terminal = a.cfg.terminal
			}
		}
		if a.terminal == "" {
			return attachExit{1, "找不到这个终端：它可能已经关闭。"}, false
		}
	}
	return attachExit{}, true
}

func (a *attacher) streamPath() string {
	path := fmt.Sprintf("/api/terminals/%s/stream?after=%d&viewer=%s", a.terminal, a.offset, a.viewer)
	if a.cfg.observe {
		path += "&observe=1"
	}
	return path
}

// dial opens the stream and checks that the server applies the viewer rules.
// A server that does not announce them gets no fallback: it is too old.
func (a *attacher) dial(ctx context.Context) (*websocket.Conn, attachExit, bool) {
	ws, err := agentDialSocket(ctx, a.c, a.cfg.target, a.streamPath())
	if err != nil {
		return nil, attachFailure(err), false
	}
	ws.SetReadLimit(terminalInputMessageLimit)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	kind, data, err := ws.ReadMessage()
	var hello struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
	}
	if err != nil || kind != websocket.TextMessage || json.Unmarshal(data, &hello) != nil || hello.Type != "viewer" || hello.Protocol < terminalViewerProtocol {
		ws.Close()
		if err != nil && a.started {
			return nil, attachExit{1, "连接中断：" + err.Error()}, false
		}
		return nil, attachExit{1, "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"}, false
	}
	_ = ws.SetReadDeadline(time.Time{})
	a.started = true
	return ws, attachExit{}, true
}

func (a *attacher) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.ws == nil {
		return errors.New("stream closed")
	}
	_ = a.ws.SetWriteDeadline(time.Now().Add(terminalWriteTimeout))
	return a.ws.WriteMessage(websocket.TextMessage, data)
}

func (a *attacher) sendSize() {
	if a.tty.size == nil {
		return
	}
	if cols, rows, ok := a.tty.size(); ok && validTerminalSize(uint16(cols), uint16(rows)) {
		_ = a.send(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	}
}

// run carries the stream until the terminal ends, the window closes or the
// stream cannot be resumed. A dropped stream resumes after its last byte.
func (a *attacher) run(ctx context.Context, ws *websocket.Conn) attachExit {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inputs := make(chan []byte, 16)
	inputDone := make(chan struct{})
	go a.readInput(inputs, inputDone)
	paste := &attachPaste{a: a}
	for {
		a.sendMu.Lock()
		a.ws = ws
		a.sendMu.Unlock()
		if attachStreamHook != nil {
			attachStreamHook(ws)
		}
		a.sendSize()
		frames := make(chan error, 1)
		go func() { frames <- a.readStream(ws) }()
		keepalive := time.NewTicker(attachKeepalive)
		var streamErr error
		detached, readerDone := false, false
	pump:
		for {
			select {
			case <-ctx.Done():
				detached = true
				break pump
			case <-a.tty.closed:
				detached = true
				break pump
			case <-inputDone:
				detached = true
				break pump
			case <-a.tty.resized:
				a.sendSize()
			case data := <-inputs:
				if a.cfg.observe {
					if bytes.ContainsAny(data, "\x03\x04") {
						detached = true
						break pump
					}
					continue
				}
				if err := paste.feed(ctx, data); err != nil {
					streamErr = err
					break pump
				}
			case <-paste.timer():
				if err := paste.flush(); err != nil {
					streamErr = err
					break pump
				}
			case <-keepalive.C:
				_ = a.send(map[string]string{"type": "ping"})
			case streamErr = <-frames:
				readerDone = true
				break pump
			}
		}
		keepalive.Stop()
		a.sendMu.Lock()
		if detached {
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		}
		ws.Close()
		a.ws = nil
		a.sendMu.Unlock()
		if !readerDone {
			// The reader owns offset and replay until it has stopped.
			if err := <-frames; streamErr == nil {
				streamErr = err
			}
		}
		if detached {
			return attachExit{}
		}
		if a.ended {
			return a.endedExit(ctx)
		}
		next, exit, ok := a.resume(ctx, streamErr)
		if !ok {
			return exit
		}
		ws = next
	}
}

// readStream copies one stream to the local terminal until it ends.
func (a *attacher) readStream(ws *websocket.Conn) error {
	for {
		_ = ws.SetReadDeadline(time.Now().Add(attachSilence))
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if kind == websocket.BinaryMessage {
			if err := a.writeOutput(data); err != nil {
				return err
			}
			continue
		}
		var frame struct {
			Type   string  `json:"type"`
			Offset *uint64 `json:"offset"`
			Reset  bool    `json:"reset"`
			Size   int     `json:"size"`
		}
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "history":
			if frame.Offset == nil {
				continue
			}
			if frame.Reset || *frame.Offset != a.offset {
				// The bytes that follow do not continue what this window
				// shows: start from a clean screen.
				a.filter.Reset()
				if _, err := a.tty.out.Write([]byte("\x1bc")); err != nil {
					return err
				}
			}
			a.offset, a.replay = *frame.Offset, max(frame.Size, 0)
		case "exit":
			a.ended = true
		}
	}
}

// writeOutput filters output for the local application. Replayed history
// only draws: its queries, clipboard writes and notifications already acted.
func (a *attacher) writeOutput(data []byte) error {
	a.offset += uint64(len(data))
	for len(data) > 0 {
		n := len(data)
		if a.replay > 0 {
			n = min(n, a.replay)
		}
		a.filter.SetReplay(a.replay > 0)
		a.buf = a.filter.Append(a.buf[:0], data[:n])
		if a.replay > 0 {
			a.replay -= n
		}
		data = data[n:]
		if len(a.buf) > 0 {
			if _, err := a.tty.out.Write(a.buf); err != nil {
				return err
			}
		}
	}
	if cap(a.buf) > 1<<20 {
		a.buf = nil
	}
	return nil
}

func (a *attacher) readInput(inputs chan<- []byte, done chan<- struct{}) {
	defer close(done)
	if a.tty.in == nil {
		return
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := a.tty.in.Read(buffer)
		if n > 0 {
			inputs <- append([]byte(nil), buffer[:n]...)
		}
		if err != nil {
			return
		}
	}
}

func (a *attacher) sendInput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return a.send(map[string]string{"type": "input", "data": string(data)})
}

// resume reconnects after a dropped stream, or explains why it cannot.
func (a *attacher) resume(ctx context.Context, cause error) (*websocket.Conn, attachExit, bool) {
	deadline := time.Now().Add(attachRetryWindow)
	delay := 250 * time.Millisecond
	for {
		if exit, final := a.explain(ctx); final {
			return nil, exit, false
		}
		ws, err := agentDialSocket(ctx, a.c, a.cfg.target, a.streamPath())
		var ae *AgentError
		if errors.As(err, &ae) && ae.Code == "connection_failed" {
			// The desktop may have restarted on another port.
			if c, connectErr := agentConnectTarget(ctx, a.cfg.configPath, a.cfg.target); connectErr == nil {
				c.connectionID = agentConnection(a.cfg.target)
				a.c.close()
				a.c = c
				ws, err = agentDialSocket(ctx, a.c, a.cfg.target, a.streamPath())
			}
		}
		if err == nil {
			ws.SetReadLimit(terminalInputMessageLimit)
			_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
			kind, data, readErr := ws.ReadMessage()
			if readErr == nil && kind == websocket.TextMessage && bytes.Contains(data, []byte("\"viewer\"")) {
				_ = ws.SetReadDeadline(time.Time{})
				return ws, attachExit{}, true
			}
			ws.Close()
		} else if errors.As(err, &ae) && ae.Code != "connection_failed" && ae.Code != "service_busy" {
			return nil, attachFailure(err), false
		}
		if time.Now().After(deadline) {
			message := "连接中断，已停止重试：再次运行同一条命令即可回到这个终端。"
			if cause != nil {
				message += "（" + cause.Error() + "）"
			}
			return nil, attachExit{1, message}, false
		}
		select {
		case <-ctx.Done():
			return nil, attachExit{}, false
		case <-a.tty.closed:
			return nil, attachExit{}, false
		case <-time.After(delay):
		}
		delay = min(2*delay, 2*time.Second)
	}
}

// explain asks the workload why a stream ended. final is true when it will
// not come back: locked, terminal gone, environment stopped.
func (a *attacher) explain(ctx context.Context) (attachExit, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var account struct {
		Unlocked *bool `json:"unlocked"`
	}
	if err := a.request(ctx, "GET", "/api/account", nil, &account); err != nil {
		var refused *attachHTTPError
		if errors.As(err, &refused) && refused.status != http.StatusServiceUnavailable && refused.status != http.StatusTooManyRequests {
			return refused.exit(), true
		}
		return attachExit{}, false
	}
	if account.Unlocked != nil && !*account.Unlocked {
		return attachExit{3, "Super Cage 已锁定，已断开：解锁后再运行同一条命令即可回到这个终端。"}, true
	}
	var list []map[string]any
	if err := a.request(ctx, "GET", "/api/terminals", nil, &list); err != nil {
		return attachExit{}, false
	}
	for _, item := range list {
		if item["id"] == a.terminal {
			if item["closed"] == true {
				return a.endedExit(ctx), true
			}
			return attachExit{}, false
		}
	}
	return a.endedExit(ctx), true
}

// endedExit says why a terminal ended: the environment stopped, or its
// program exited.
func (a *attacher) endedExit(ctx context.Context) attachExit {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var status struct {
		Running *bool `json:"vm_running"`
	}
	if a.request(ctx, "GET", "/api/status", nil, &status) == nil && status.Running != nil && !*status.Running {
		return attachExit{0, "环境已停止，终端已随之结束。"}
	}
	return attachExit{0, "终端已结束。"}
}
