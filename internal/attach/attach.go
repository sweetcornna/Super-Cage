// Package attach is cage attach: it shows a Cage terminal in the terminal
// application it runs in. It speaks the same terminal stream as the
// desktop's web terminal, after the same controller checks and session as
// cage agent: no new credential and no new listener. The terminal keeps
// running when the window closes, like a closed tab.
//
// It runs in the slim cage-attach program (cmd/cage-attach), and in cage
// itself when that program is not beside it, so it must stay free of
// net/http and internal/workbench; see cmd/cage-attach.
package attach

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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/controlwire"
	"github.com/sylvinus/agent-vm/internal/localterm"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// What the workbench serves; internal/workbench tests that they agree.
const (
	// ViewerProtocol is the viewer frame's protocol this client speaks.
	ViewerProtocol = 1
	// MaxImageBytes bounds an image the web terminal accepts.
	MaxImageBytes = 20 << 20
	// ConfigEnv names the environment variable with the config path.
	ConfigEnv = "CLAUDE_CAGE_CONFIG"
	// WriteTimeout bounds one write to the stream, as the server's does.
	WriteTimeout = 30 * time.Second
	// textLimit bounds a control frame of the stream; output is streamed.
	textLimit = 1 << 20
)

// ValidSize is the terminal size the workbench accepts.
func ValidSize(cols, rows uint16) bool {
	return cols > 1 && cols < 600 && rows > 1 && rows < 400
}

// Options are what the program running attach decides.
type Options struct {
	// Check accepts the controller's build: the same executable for cage
	// itself, the same release for cage-attach.
	Check controlclient.Check
	// FilterReport, when set, receives the output filter's report of what
	// it dropped once the stream ends.
	FilterReport func(report string)
}

const attachUsage = "用法：cage attach [--terminal ID | --new shell|claude|codex | --resume 会话ID] [--project 路径] [--claude-account ID | --codex-account ID] [--connection ID|current] [--observe] [--config 路径]"

// Config is what one cage attach was asked to do.
type Config struct {
	ConfigPath    string
	Target        controlclient.AgentTarget
	Terminal      string
	NewKind       string
	Project       string
	ClaudeAccount string
	CodexAccount  string
	Resume        string
	Observe       bool
	// FilterStats prints what the output filter dropped when the attach
	// ends (the hidden --filter-stats).
	FilterStats bool
}

// TTY is the local terminal. The real one is a raw-mode tty; tests use
// a pseudo-terminal or pipes.
type TTY struct {
	In      io.Reader
	Out     io.Writer
	ErrOut  io.Writer
	Size    func() (cols, rows int, ok bool)
	Resized <-chan struct{}
	MakeRaw func() (restore func(), err error)
	// Closed ends the attach without ending the terminal: the window closed.
	Closed <-chan struct{}
}

// Exit is how an attach ended: its exit code and the one line it
// prints once the local terminal is restored.
type Exit struct {
	Code    int
	Message string
}

var (
	attachTerminalID = regexp.MustCompile("^[0-9a-f]{16}$")
	attachSessionID  = regexp.MustCompile("^[A-Za-z0-9_.-]{1,128}$")
	attachAccountID  = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")
)

// Main runs cage attach and returns its exit code.
func Main(args []string, stdin, stdout *os.File, stderr io.Writer, options Options) int {
	cfg, err := ParseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	tty, stop := localTTY(stdin, stdout, stderr)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-tty.Closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	var stats string
	if cfg.FilterStats {
		options.FilterReport = func(report string) { stats = report }
	}
	result := Run(ctx, cfg, tty, options)
	if result.Message != "" {
		fmt.Fprint(stderr, "\r\n"+result.Message+"\r\n")
	}
	if cfg.FilterStats {
		if stats == "" {
			stats = "没有丢弃任何序列。\n"
		}
		fmt.Fprint(stderr, "已丢弃的序列：\n"+stats)
	}
	return result.Code
}

func ParseArgs(args []string) (Config, error) {
	var cfg Config
	flags := flag.NewFlagSet("cage attach", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", os.Getenv(ConfigEnv), "")
	connection := flags.String("connection", "", "")
	flags.StringVar(&cfg.Terminal, "terminal", "", "")
	flags.StringVar(&cfg.NewKind, "new", "", "")
	flags.StringVar(&cfg.Project, "project", "", "")
	flags.StringVar(&cfg.ClaudeAccount, "claude-account", "", "")
	flags.StringVar(&cfg.CodexAccount, "codex-account", "", "")
	flags.StringVar(&cfg.Resume, "resume", "", "")
	flags.BoolVar(&cfg.Observe, "observe", false, "")
	flags.BoolVar(&cfg.FilterStats, "filter-stats", false, "")
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
	for _, value := range []string{cfg.Terminal, cfg.NewKind, cfg.Resume} {
		if value != "" {
			chosen++
		}
	}
	switch {
	case chosen > 1:
		return cfg, errors.New("--terminal、--new 和 --resume 只能选一个。" + attachUsage)
	case cfg.Terminal != "" && !attachTerminalID.MatchString(cfg.Terminal):
		return cfg, errors.New("终端 ID 无效")
	case cfg.Resume != "" && !attachSessionID.MatchString(cfg.Resume):
		return cfg, errors.New("会话 ID 无效")
	case cfg.NewKind != "" && cfg.NewKind != "shell" && cfg.NewKind != "claude" && cfg.NewKind != "codex":
		return cfg, errors.New("--new 只能是 shell、claude 或 codex")
	case cfg.ClaudeAccount != "" && (cfg.NewKind != "claude" || !attachAccountID.MatchString(cfg.ClaudeAccount)):
		return cfg, errors.New("--claude-account 只能和 --new claude 一起使用")
	case cfg.CodexAccount != "" && (cfg.NewKind != "codex" || !attachAccountID.MatchString(cfg.CodexAccount)):
		return cfg, errors.New("--codex-account 只能和 --new codex 一起使用")
	case cfg.Project != "" && cfg.NewKind == "":
		return cfg, errors.New("--project 只能和 --new 一起使用")
	case cfg.Observe && (cfg.NewKind != "" || cfg.Resume != ""):
		return cfg, errors.New("--observe 只能查看已有终端")
	}
	if chosen == 0 {
		cfg.NewKind = "shell"
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
	cfg.ConfigPath = path
	cfg.Target = controlclient.AgentTarget{Scope: "local"}
	if *connection != "" {
		cfg.Target = controlclient.AgentTarget{Scope: "selected", Connection: *connection}
	}
	if cfg.Project != "" && cfg.Target.Scope == "local" && !filepath.IsAbs(cfg.Project) {
		if cfg.Project, err = filepath.Abs(cfg.Project); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// attacher is one running attach.
type attacher struct {
	cfg      Config
	tty      TTY
	check    controlclient.Check
	c        *controlwire.Client
	settings localterm.Settings
	viewer   string
	terminal string

	sendMu sync.Mutex
	ws     *controlwire.Conn

	filter  *termfilter.Filter
	in      []byte // one read of output; output is never held whole
	buf     []byte
	offset  uint64
	replay  int
	ended   bool
	started bool // the first stream was accepted
}

// StreamHook sees every stream an attach opens; tests use it to drop
// one mid-output.
var StreamHook func(*controlwire.Conn)

// The stream to the controller is a single WebSocket. The server speaks at
// least every 20 seconds (a heartbeat frame and a ping, which is answered),
// so a stream silent for longer than Silence is dead: a sleeping laptop or a
// stalled tunnel. Waiting for the next frame needs no timer of this
// program's own; the deadline only moves.
var (
	Silence     = 45 * time.Second
	RetryWindow = 60 * time.Second
)

// Run attaches until the terminal ends, the window closes or the stream
// cannot be resumed.
func Run(ctx context.Context, cfg Config, tty TTY, options Options) Exit {
	a := &attacher{cfg: cfg, tty: tty, check: options.Check}
	if a.check == nil {
		return Exit{1, "无法确认正在运行的 Super Cage 的版本。"}
	}
	a.settings, _ = localterm.LoadSettings(filepath.Dir(cfg.ConfigPath))
	a.filter = termfilter.New(termfilter.Options{ClipboardWrite: a.settings.ClipboardWrite})
	a.viewer = attachViewerID()
	if exit, ok := a.connectController(ctx); !ok {
		return exit
	}
	defer a.c.Close()
	if exit, ok := a.chooseTerminal(ctx); !ok {
		return exit
	}
	ws, exit, ok := a.dial(ctx)
	if !ok {
		return exit
	}
	restore := func() {}
	if tty.MakeRaw != nil {
		var err error
		if restore, err = tty.MakeRaw(); err != nil {
			ws.Close()
			return Exit{1, "无法把这个窗口切换到终端模式：" + err.Error()}
		}
	}
	exit = a.run(ctx, ws)
	restore()
	if options.FilterReport != nil {
		options.FilterReport(a.filter.Report())
	}
	return exit
}

func attachViewerID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "attach-" + hex.EncodeToString(b[:])
}

func (a *attacher) connectController(ctx context.Context) (Exit, bool) {
	target := a.cfg.Target
	if target.Connection == "current" {
		current, err := a.currentConnection(ctx)
		if err != nil {
			return Exit{1, err.Error()}, false
		}
		target.Connection = current
		a.cfg.Target = target
	}
	c, err := a.connect(ctx, target)
	if err != nil {
		return attachFailure(err), false
	}
	a.c = c
	return Exit{}, true
}

// connect opens an authenticated connection to the target, after the same
// checks as cage agent.
func (a *attacher) connect(ctx context.Context, target controlclient.AgentTarget) (*controlwire.Client, error) {
	conn, err := controlclient.AgentConnectTarget(ctx, a.cfg.ConfigPath, target, a.check, controlwire.ConnectConn)
	if err != nil {
		return nil, err
	}
	c := conn.(*controlwire.Client)
	c.ConnectionID = controlclient.Connection(target)
	return c, nil
}

// currentConnection reads the desktop's current connection, so a cloud
// workspace open in the desktop can be attached without copying its id. It
// asks as cage agent request --scope desktop does.
func (a *attacher) currentConnection(ctx context.Context) (string, error) {
	c, err := a.connect(ctx, controlclient.AgentTarget{Scope: "desktop"})
	if err == nil {
		defer c.Close()
		var status int
		var data []byte
		status, data, err = c.Do(ctx, "GET", "/api/client/context", nil, false, 2*time.Minute)
		if err == nil && (status < 200 || status >= 300) {
			err = controlclient.HTTPError(status)
		}
		if err == nil {
			var current struct {
				ConnectionID string `json:"connection_id"`
			}
			if json.Unmarshal(data, &current) != nil || current.ConnectionID == "" {
				return "", errors.New("Super Cage 没有打开任何环境：先在 Super Cage 里选择一个环境。")
			}
			return current.ConnectionID, nil
		}
	}
	return "", errors.New(attachFailure(err).Message)
}

// attachFailure turns a transport error into the owner's language.
func attachFailure(err error) Exit {
	var ae *controlclient.AgentError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "service_unavailable":
			return Exit{1, "Super Cage 没有在运行：打开 Super Cage 后再运行这条命令。"}
		case "controller_mismatch", "config_path_mismatch":
			return Exit{1, "这个 cage 命令和正在运行的 Super Cage 不是同一个版本：请用 Super Cage 自带的 cage。"}
		case "account_locked":
			return Exit{3, "Super Cage 已锁定：先在 Super Cage 里解锁，再运行这条命令。"}
		case "state_conflict":
			return Exit{1, "Super Cage 已切换到其他环境，这个终端所在的连接已断开。"}
		case "unsupported_operation":
			return Exit{1, "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"}
		}
	}
	return Exit{1, "无法连接 Super Cage：" + err.Error()}
}

// request calls the workload's API on the authenticated connection.
func (a *attacher) request(ctx context.Context, method, path string, input, output any) error {
	var body []byte
	if input != nil {
		var err error
		if body, err = json.Marshal(input); err != nil {
			return err
		}
	}
	status, data, err := a.c.Do(ctx, method, path, body, input != nil, 2*time.Minute)
	if err != nil {
		return &controlclient.AgentError{Code: "service_unavailable", Message: err.Error()}
	}
	if status < 200 || status >= 300 {
		return &attachHTTPError{status: status, text: strings.TrimSpace(string(data))}
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
func (e *attachHTTPError) exit() Exit {
	switch e.status {
	case 423: // locked
		return Exit{3, "Super Cage 已锁定：先在 Super Cage 里解锁，再运行这条命令。"}
	case 404:
		return Exit{1, "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"}
	}
	if e.text != "" && len(e.text) < 300 && !strings.HasPrefix(e.text, "{") {
		return Exit{1, e.text}
	}
	return attachFailure(controlclient.HTTPError(e.status))
}

// chooseTerminal finds or creates the terminal to show, through the same
// requests the desktop makes, so account selection, pinned accounts and the
// Codex lease follow the desktop's own path.
func (a *attacher) chooseTerminal(ctx context.Context) (Exit, bool) {
	fail := func(err error) (Exit, bool) {
		var refused *attachHTTPError
		if errors.As(err, &refused) {
			return refused.exit(), false
		}
		return attachFailure(err), false
	}
	switch {
	case a.cfg.Resume != "":
		var view struct {
			Terminal string `json:"desktop_terminal_id"`
		}
		if err := a.request(ctx, "POST", "/api/cli/sessions/"+url.PathEscape(a.cfg.Resume)+"/open", map[string]any{}, &view); err != nil {
			return fail(err)
		}
		if !attachTerminalID.MatchString(view.Terminal) {
			return Exit{1, "这个会话没有在终端中打开：在 Super Cage 里查看它的状态。"}, false
		}
		a.terminal = view.Terminal
	case a.cfg.NewKind != "":
		request := map[string]any{"workspace": a.cfg.Project, "kind": a.cfg.NewKind}
		if a.cfg.ClaudeAccount != "" {
			request["claude_account_id"] = a.cfg.ClaudeAccount
		}
		if a.cfg.CodexAccount != "" {
			request["codex_account_id"] = a.cfg.CodexAccount
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := a.request(ctx, "POST", "/api/terminals", request, &created); err != nil {
			return fail(err)
		}
		if !attachTerminalID.MatchString(created.ID) {
			return Exit{1, "Super Cage 没有返回新终端"}, false
		}
		a.terminal = created.ID
	default:
		var list []map[string]any
		if err := a.request(ctx, "GET", "/api/terminals", nil, &list); err != nil {
			return fail(err)
		}
		for _, item := range list {
			if item["id"] == a.cfg.Terminal {
				a.terminal = a.cfg.Terminal
			}
		}
		if a.terminal == "" {
			return Exit{1, "找不到这个终端：它可能已经关闭。"}, false
		}
	}
	return Exit{}, true
}

func (a *attacher) streamPath() string {
	path := fmt.Sprintf("/api/terminals/%s/stream?after=%d&viewer=%s", a.terminal, a.offset, a.viewer)
	if a.cfg.Observe {
		path += "&observe=1"
	}
	return path
}

// dial opens the stream and checks that the server applies the viewer rules.
// A server that does not announce them gets no fallback: it is too old.
func (a *attacher) dial(ctx context.Context) (*controlwire.Conn, Exit, bool) {
	ws, err := a.c.Dial(ctx, a.cfg.Target, a.streamPath())
	if err != nil {
		return nil, attachFailure(err), false
	}
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	kind, data, err := ws.ReadMessage(textLimit)
	var hello struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
	}
	if err != nil || kind != controlwire.TextMessage || json.Unmarshal(data, &hello) != nil || hello.Type != "viewer" || hello.Protocol < ViewerProtocol {
		ws.Close()
		if err != nil && a.started {
			return nil, Exit{1, "连接中断：" + err.Error()}, false
		}
		return nil, Exit{1, "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"}, false
	}
	_ = ws.SetReadDeadline(time.Time{})
	a.started = true
	return ws, Exit{}, true
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
	_ = a.ws.SetWriteDeadline(time.Now().Add(WriteTimeout))
	return a.ws.WriteMessage(controlwire.TextMessage, data)
}

func (a *attacher) sendSize() {
	if a.tty.Size == nil {
		return
	}
	if cols, rows, ok := a.tty.Size(); ok && ValidSize(uint16(cols), uint16(rows)) {
		_ = a.send(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	}
}

// run carries the stream until the terminal ends, the window closes or the
// stream cannot be resumed. A dropped stream resumes after its last byte.
func (a *attacher) run(ctx context.Context, ws *controlwire.Conn) Exit {
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
		if StreamHook != nil {
			StreamHook(ws)
		}
		a.sendSize()
		frames := make(chan error, 1)
		go func() { frames <- a.readStream(ws) }()
		var streamErr error
		detached, readerDone := false, false
	pump:
		for {
			select {
			case <-ctx.Done():
				detached = true
				break pump
			case <-a.tty.Closed:
				detached = true
				break pump
			case <-inputDone:
				detached = true
				break pump
			case <-a.tty.Resized:
				a.sendSize()
			case data := <-inputs:
				if a.cfg.Observe {
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
			case streamErr = <-frames:
				readerDone = true
				break pump
			}
		}
		a.sendMu.Lock()
		if detached {
			_ = ws.WriteClose()
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
			return Exit{}
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

// readStream copies one stream to the local terminal until it ends. Output
// passes through one fixed buffer: a burst never grows the heap.
func (a *attacher) readStream(ws *controlwire.Conn) error {
	if a.in == nil {
		a.in = make([]byte, 32<<10)
	}
	for {
		if Silence > 0 {
			_ = ws.SetReadDeadline(time.Now().Add(Silence))
		}
		kind, r, err := ws.NextReader()
		if err != nil {
			return err
		}
		if kind == controlwire.BinaryMessage {
			for {
				n, err := r.Read(a.in)
				if n > 0 {
					if err := a.writeOutput(a.in[:n]); err != nil {
						return err
					}
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(r, textLimit+1))
		if err != nil {
			return err
		}
		if len(data) > textLimit {
			return errors.New("stream frame too large")
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
				if _, err := a.tty.Out.Write([]byte("\x1bc")); err != nil {
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
			if _, err := a.tty.Out.Write(a.buf); err != nil {
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
	if a.tty.In == nil {
		return
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := a.tty.In.Read(buffer)
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
func (a *attacher) resume(ctx context.Context, cause error) (*controlwire.Conn, Exit, bool) {
	deadline := time.Now().Add(RetryWindow)
	delay := 250 * time.Millisecond
	for {
		if exit, final := a.explain(ctx); final {
			return nil, exit, false
		}
		ws, err := a.c.Dial(ctx, a.cfg.Target, a.streamPath())
		var ae *controlclient.AgentError
		if errors.As(err, &ae) && ae.Code == "connection_failed" {
			// The desktop may have restarted on another port.
			if c, connectErr := a.connect(ctx, a.cfg.Target); connectErr == nil {
				a.c.Close()
				a.c = c
				ws, err = a.c.Dial(ctx, a.cfg.Target, a.streamPath())
			}
		}
		if err == nil {
			_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
			kind, data, readErr := ws.ReadMessage(textLimit)
			if readErr == nil && kind == controlwire.TextMessage && bytes.Contains(data, []byte("\"viewer\"")) {
				_ = ws.SetReadDeadline(time.Time{})
				return ws, Exit{}, true
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
			return nil, Exit{1, message}, false
		}
		select {
		case <-ctx.Done():
			return nil, Exit{}, false
		case <-a.tty.Closed:
			return nil, Exit{}, false
		case <-time.After(delay):
		}
		delay = min(2*delay, 2*time.Second)
	}
}

// explain asks the workload why a stream ended. final is true when it will
// not come back: locked, terminal gone, environment stopped.
func (a *attacher) explain(ctx context.Context) (Exit, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var account struct {
		Unlocked *bool `json:"unlocked"`
	}
	if err := a.request(ctx, "GET", "/api/account", nil, &account); err != nil {
		var refused *attachHTTPError
		if errors.As(err, &refused) && refused.status != 503 && refused.status != 429 {
			return refused.exit(), true
		}
		return Exit{}, false
	}
	if account.Unlocked != nil && !*account.Unlocked {
		return Exit{3, "Super Cage 已锁定，已断开：解锁后再运行同一条命令即可回到这个终端。"}, true
	}
	var list []map[string]any
	if err := a.request(ctx, "GET", "/api/terminals", nil, &list); err != nil {
		return Exit{}, false
	}
	for _, item := range list {
		if item["id"] == a.terminal {
			if item["closed"] == true {
				return a.endedExit(ctx), true
			}
			return Exit{}, false
		}
	}
	return a.endedExit(ctx), true
}

// endedExit says why a terminal ended: the environment stopped, or its
// program exited.
func (a *attacher) endedExit(ctx context.Context) Exit {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var status struct {
		Running *bool `json:"vm_running"`
	}
	if a.request(ctx, "GET", "/api/status", nil, &status) == nil && status.Running != nil && !*status.Running {
		return Exit{0, "环境已停止，终端已随之结束。"}
	}
	return Exit{0, "终端已结束。"}
}
