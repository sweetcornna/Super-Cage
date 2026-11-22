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
	"sync/atomic"
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
	// Reset only writes ResetSequence (--reset): the launch script runs it
	// after an attach that may not have reset the window itself.
	Reset bool
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
		fmt.Fprintln(stderr, termfilter.Plain(err.Error()))
		return 2
	}
	if cfg.Reset {
		_, _ = stdout.WriteString(ResetSequence)
		return 0
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
		// Messages can quote the server; they are printed only as text.
		fmt.Fprint(stderr, "\r\n"+strings.ReplaceAll(termfilter.Plain(result.Message), "\n", "\r\n")+"\r\n")
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
	flags.BoolVar(&cfg.Reset, "reset", false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() > 1 {
		return cfg, errors.New(attachUsage)
	}
	if cfg.Reset {
		return cfg, nil
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
		// It is sent in a request header: the launcher's alphabet only.
		if !connectionID(*connection) {
			return cfg, errors.New("连接编号无效：只能由字母、数字、_ 和 - 组成，最多 128 个字符")
		}
		cfg.Target = controlclient.AgentTarget{Scope: "selected", Connection: *connection}
	}
	if termfilter.Controls(cfg.Project) {
		// The path reaches a command line, and a copied command.
		return cfg, errors.New("项目路径无效：不能含有控制字符")
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

	// What the attach writes itself waits while the program has a clipboard
	// write open in the window (outMu guards these and output writes).
	outMu     sync.Mutex
	streaming bool // the last output left a clipboard write open
	focusOwed bool // focus reports to turn back on after a reset
	bellOwed  bool // a bell to ring

	restore   func() // the local terminal's mode before the attach
	leaveOnce sync.Once

	// Whether the owner is at this window, for clipboard writes: focus
	// reports when the application sends them, else recent input.
	focusState atomic.Int32 // focusUnknown, focusIn or focusOut
	lastInput  atomic.Int64 // when the owner last typed or used the mouse (Unix ns)
	inPaste    bool         // input is inside a bracketed paste
	dropPaste  bool         // the rest of a paste cut by a reconnect is dropped
	endPaste   bool         // the program received a paste's start, not its end
	resets     int          // the filter's Resets, to turn focus reports on again

	filter  *termfilter.Filter
	in      []byte // one read of output; output is never held whole
	buf     []byte
	offset  uint64
	replay  int
	ended   bool
	started bool // the first stream was accepted
}

// ClipboardWindow is how long after the owner's last key or mouse input a
// clipboard write passes, in an application that never reports focus.
var ClipboardWindow = 3 * time.Second

// updateNeeded is the answer to a workload from before the viewer rules.
const updateNeeded = "需要更新 Super Cage：这个环境的 Super Cage 版本还不能在本机终端中打开。"

// TitlePush and ResetSequence are termfilter's, which the SSH entry shares.
const (
	TitlePush     = termfilter.TitlePush
	ResetSequence = termfilter.ResetSequence
)

// StreamHook sees every stream an attach opens; tests use it to drop
// one mid-output.
var StreamHook func(*controlwire.Conn)

// ResumeHook, when set, runs before each reconnect attempt. Tests only.
var ResumeHook func(ctx context.Context)

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
	a.filter = termfilter.New(termfilter.Options{ClipboardWrite: a.settings.ClipboardWrite, ClipboardNow: a.ownerHere})
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
	a.restore = func() {}
	if tty.MakeRaw != nil {
		var err error
		if a.restore, err = tty.MakeRaw(); err != nil {
			ws.Close()
			return Exit{1, "无法把这个窗口切换到终端模式：" + err.Error()}
		}
	}
	defer a.guard()
	if _, err := tty.Out.Write([]byte(TitlePush + focusOn)); err != nil {
		a.leave()
		ws.Close()
		return Exit{}
	}
	exit = a.run(ctx, ws)
	a.leave()
	if options.FilterReport != nil {
		options.FilterReport(a.filter.Report())
	}
	return exit
}

// focusOn turns on focus reports in the local application. They are the
// viewer's: the program gets them only if it asked for them too.
const focusOn = "\x1b[?1004h"

const (
	focusUnknown int32 = iota
	focusIn
	focusOut
)

// ownerHere says whether a clipboard write may pass now: while the window
// has focus, or, in an application that has not reported focus, within
// ClipboardWindow of the owner's last key or mouse input. A write that
// arrives otherwise is dropped, not kept for later.
func (a *attacher) ownerHere() bool {
	switch a.focusState.Load() {
	case focusIn:
		return true
	case focusOut:
		return false
	}
	last := a.lastInput.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < ClipboardWindow
}

// noteInput reads what the local application sent before it goes on: keys,
// pastes and mouse reports mark the owner as here; focus reports set the
// focus and are kept from the program unless it asked for them. Answers to
// queries are neither.
func (a *attacher) noteInput(data []byte) []byte {
	out := make([]byte, 0, len(data))
	owner := false
	for i := 0; i < len(data); {
		if a.inPaste {
			end := bytes.Index(data[i:], []byte(attachPasteEnd))
			owner = true
			if end < 0 {
				out = append(out, data[i:]...)
				break
			}
			end += len(attachPasteEnd)
			out = append(out, data[i:i+end]...)
			i += end
			a.inPaste = false
			continue
		}
		n, kind, _ := termfilter.NextInput(data[i:])
		unit := data[i : i+n]
		i += n
		switch kind {
		case termfilter.InputFocusIn, termfilter.InputFocusOut:
			state := focusIn
			if kind == termfilter.InputFocusOut {
				state = focusOut
			}
			a.focusState.Store(state)
			if !a.filter.GuestFocus() {
				continue
			}
		case termfilter.InputKey:
			owner = true
			a.inPaste = string(unit) == attachPasteStart
		case termfilter.InputMouse:
			owner = true
		}
		out = append(out, unit...)
	}
	if owner {
		a.lastInput.Store(time.Now().UnixNano())
	}
	return out
}

// connectionID reports whether id has the form the desktop's launcher
// accepts: ^[A-Za-z0-9_-]{1,128}$.
func connectionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// bell rings the local terminal's bell, once no clipboard write is open in
// it: inside one the bell would end it early.
func (a *attacher) bell() {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	if a.streaming {
		a.bellOwed = true
		return
	}
	_, _ = a.tty.Out.Write([]byte("\a"))
}

// leave returns the local terminal to how it was, once: the reset, then the
// terminal mode. Every way out after the first output comes through here, a
// panic in any of the attach's goroutines included.
func (a *attacher) leave() {
	a.leaveOnce.Do(func() {
		cols := 0
		if a.tty.Size != nil {
			cols, _, _ = a.tty.Size()
		}
		_, _ = a.tty.Out.Write(a.filter.End(nil, cols))
		if a.restore != nil {
			a.restore()
		}
	})
}

// guard is deferred by every goroutine of a running attach: a panic leaves
// the window usable before it ends the program.
func (a *attacher) guard() {
	if r := recover(); r != nil {
		a.leave()
		panic(r)
	}
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
	if known := controlclient.Identity(err); known != nil {
		return Exit{1, known.Error()}
	}
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
			return Exit{1, updateNeeded}
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
	if identity := controlclient.IdentityAgentError(err); identity != nil {
		return identity
	}
	if err != nil {
		return &controlclient.AgentError{Code: "service_unavailable", Message: err.Error()}
	}
	if status < 200 || status >= 300 {
		return &attachHTTPError{status: status, text: strings.TrimSpace(string(data))}
	}
	if output != nil && json.Unmarshal(data, output) != nil {
		return errUnreadable
	}
	return nil
}

var errUnreadable = errors.New("Super Cage 的回答无法读取")

// checkViewers asks the workload, before --new or --resume creates
// anything, whether it applies the viewer rules. A server from before them
// answers 404, 405 or its page, and gets no terminal it could not show.
func (a *attacher) checkViewers(ctx context.Context) (Exit, bool) {
	var answer struct {
		Protocol int `json:"protocol"`
	}
	err := a.request(ctx, "GET", "/api/terminals/viewers", nil, &answer)
	var refused *attachHTTPError
	switch {
	case err == nil && answer.Protocol >= ViewerProtocol:
		return Exit{}, true
	case err == nil, errors.Is(err, errUnreadable),
		errors.As(err, &refused) && (refused.status == 404 || refused.status == 405):
		return Exit{1, updateNeeded}, false
	case errors.As(err, &refused):
		return refused.exit(), false
	}
	return attachFailure(err), false
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
		return Exit{1, updateNeeded}
	}
	if e.text != "" && len(e.text) < 300 && !strings.HasPrefix(e.text, "{") {
		return Exit{1, termfilter.Plain(e.text)}
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
	if a.cfg.Resume != "" || a.cfg.NewKind != "" {
		if exit, ok := a.checkViewers(ctx); !ok {
			return exit, false
		}
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
		return nil, Exit{1, updateNeeded}, false
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
		if a.endPaste {
			// A paste the program had begun was cut by the reconnect.
			a.endPaste = false
			_ = a.sendInput([]byte(attachPasteEnd))
		}
		frames := make(chan error, 1)
		go func() {
			defer a.guard()
			frames <- a.readStream(ws)
		}()
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
				if data = a.noteInput(data); len(data) == 0 {
					continue
				}
				if a.cfg.Observe {
					if bytes.ContainsAny(data, "\x03\x04") {
						detached = true
						break pump
					}
					continue
				}
				if a.dropPaste {
					end := bytes.Index(data, []byte(attachPasteEnd))
					if end < 0 {
						continue
					}
					data, a.dropPaste = data[end+len(attachPasteEnd):], false
					if len(data) == 0 {
						continue
					}
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
		next, exit, ok := a.reconnect(ctx, streamErr, inputs, inputDone, paste)
		if !ok {
			return exit
		}
		ws = next
	}
}

// reconnectNotice is shown once per reconnect, when the owner types.
const reconnectNotice = "\r\n正在重新连接…（这期间输入的内容不会发送）\r\n"

// reconnect resumes the stream while the owner can still type. Keys typed
// meanwhile are dropped, not sent late, and the first says so on screen; a
// paste cut by the drop is dropped to its end. Ctrl-C or Ctrl-D still
// detach an observer.
func (a *attacher) reconnect(ctx context.Context, cause error, inputs <-chan []byte, inputDone <-chan struct{}, paste *attachPaste) (*controlwire.Conn, Exit, bool) {
	// A paste still being gathered was for the dropped stream; one already
	// passed on (longer than attach holds) is ended once the stream is back.
	forwarded := a.inPaste && paste.pending == nil
	paste.stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		ws   *controlwire.Conn
		exit Exit
		ok   bool
	}
	done := make(chan outcome, 1)
	go func() {
		defer a.guard()
		ws, exit, ok := a.resume(ctx, cause)
		done <- outcome{ws, exit, ok}
	}()
	detach := func() (*controlwire.Conn, Exit, bool) {
		cancel()
		if r := <-done; r.ws != nil {
			r.ws.Close()
		}
		return nil, Exit{}, false
	}
	told := false
	for {
		select {
		case r := <-done:
			a.dropPaste, a.endPaste = a.inPaste, forwarded && r.ok
			return r.ws, r.exit, r.ok
		case <-inputDone:
			return detach()
		case data := <-inputs:
			data = a.noteInput(data)
			if a.cfg.Observe && bytes.ContainsAny(data, "\x03\x04") {
				return detach()
			}
			if !told && typedKey(data) && !a.filter.Streaming() {
				told = true
				_, _ = a.tty.Out.Write([]byte(reconnectNotice))
			}
		}
	}
}

// typedKey reports whether input holds a key, not only reports the terminal
// application sent by itself.
func typedKey(data []byte) bool {
	for i := 0; i < len(data); {
		n, kind, _ := termfilter.NextInput(data[i:])
		if kind == termfilter.InputKey {
			return true
		}
		i += n
	}
	return false
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
				// shows: start from a clean screen, after ending what the
				// last bytes left open.
				// Not a full reset: that would erase the owner's scrollback.
				a.buf = append(a.filter.Abort(a.buf[:0]), termfilter.ClearReset...)
				a.focusOwed = true
				if err := a.writeOwn(); err != nil {
					return err
				}
			}
			a.offset, a.replay = *frame.Offset, max(frame.Size, 0)
		case "exit":
			a.ended = true
		}
	}
}

// writeOwn writes filtered output, then, unless it left a clipboard write
// open, what the attach owes the window itself.
func (a *attacher) writeOwn() error {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	a.streaming = a.filter.Streaming()
	if !a.streaming {
		if a.focusOwed {
			a.buf, a.focusOwed = append(a.buf, focusOn...), false
		}
		if a.bellOwed {
			a.buf, a.bellOwed = append(a.buf, '\a'), false
		}
	}
	if len(a.buf) == 0 {
		return nil
	}
	_, err := a.tty.Out.Write(a.buf)
	return err
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
		if resets := a.filter.Resets(); resets != a.resets {
			// A full or soft reset turned focus reports off as well.
			a.resets = resets
			a.focusOwed = true
		}
		if a.replay > 0 {
			a.replay -= n
		}
		data = data[n:]
		if err := a.writeOwn(); err != nil {
			return err
		}
	}
	if cap(a.buf) > 1<<20 {
		a.buf = nil
	}
	return nil
}

func (a *attacher) readInput(inputs chan<- []byte, done chan<- struct{}) {
	defer a.guard()
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
		if ResumeHook != nil {
			ResumeHook(ctx)
		}
		if exit, final := a.explain(ctx); final {
			return nil, exit, false
		}
		ws, err := a.c.Dial(ctx, a.cfg.Target, a.streamPath())
		var ae *controlclient.AgentError
		if errors.As(err, &ae) && ae.Code == "connection_failed" || controlclient.Identity(err) != nil {
			// The desktop may have restarted: on another port, or on the
			// same one with a new proof key. Its record says.
			if c, connectErr := a.connect(ctx, a.cfg.Target); connectErr == nil {
				a.c.Close()
				a.c = c
				ws, err = a.c.Dial(ctx, a.cfg.Target, a.streamPath())
			} else if controlclient.Identity(connectErr) != nil {
				// What its record names cannot prove itself: stop, having
				// sent it nothing.
				return nil, attachFailure(connectErr), false
			} else if controlclient.Identity(err) != nil {
				err = &controlclient.AgentError{Code: "connection_failed"}
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
