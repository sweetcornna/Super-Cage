package workbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/localterm"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// The "本机终端" client routes belong to the desktop on the owner's computer:
// two switches, kept in the desktop's local client configuration, and opening
// a Cage terminal in the owner's terminal application. There is nothing to
// choose: Herdr opens it when it is installed, else Ghostty, else the system
// Terminal. They never reach an environment or the portal.
//
//	GET  /api/client/local-terminal               this computer's settings
//	PUT  /api/client/local-terminal               save them
//	GET  /api/client/local-terminal/app           the application that opens, detected now
//	POST /api/client/local-terminal/install-herdr install Herdr desktop app on this computer
//	POST /api/client/local-terminal/open          open a terminal, project or session

// localTerminalRunner runs launch steps; tests replace it so that nothing
// is ever launched.
var localTerminalRunner localterm.Runner = localterm.ExecRunner{}

// localTerminalDetect finds the installed applications; tests replace it.
var localTerminalDetect = localterm.Detect

// localTerminalInstallHerdr installs Herdr desktop app on this computer; tests
// replace it so that nothing is downloaded or executed.
var localTerminalInstallHerdr = installHerdrDesktop

func installHerdrEnv(environ []string) []string {
	kept := localterm.LaunchEnvironment(environ)
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			kept = append(kept, entry)
		}
	}
	return kept
}

func installHerdrDesktop(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return errors.New("当前系统暂不支持安装 Herdr 桌面端")
	}
	tmp, err := os.CreateTemp("", "super-cage-herdr-install-*.sh")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Chmod(0o600)
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	const script = `set -eu
curl --proto =https --proto-redir =https --tlsv1.2 --connect-timeout 20 --max-time 240 -fsSL https://releases.herdr.dev/app/install.sh -o "$1"
sh "$1"`
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script, "super-cage-herdr-install", tmpPath)
	cmd.Env = installHerdrEnv(os.Environ())
	configureLocalTermProcGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if line := strings.TrimSpace(lines[i]); line != "" {
				return errors.New(line)
			}
		}
		return err
	}
	return nil
}

// localTerminalOpenEvery and localTerminalOpenPerMinute bound the launches
// of one desktop: each can open a window or raise macOS's Automation prompt.
var localTerminalOpenEvery, localTerminalOpenPerMinute = time.Second, 10

// localTerminalGate is the desktop's state for opening windows: one at a
// time, and the recent launches the bounds count.
type localTerminalGate struct {
	opening         atomic.Bool
	installingHerdr atomic.Bool
	mu              sync.Mutex
	opens           []time.Time
}

// allow records a launch, or refuses one past the bounds. Every launch
// counts, whether it opened a window or not.
func (g *localTerminalGate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	recent := g.opens[:0]
	for _, at := range g.opens {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	g.opens = recent
	if n := len(recent); n >= localTerminalOpenPerMinute || n > 0 && now.Sub(recent[n-1]) < localTerminalOpenEvery {
		return false
	}
	g.opens = append(g.opens, now)
	return true
}

// localTerminalUnlocked asks the environment the desktop shows whether its
// account is unlocked, as its lock screen does. The desktop's client routes
// are served before any account check, and the account is the workload's.
func (s *Server) localTerminalUnlocked(ctx context.Context, connection string) (bool, error) {
	session, _, err := s.remote.desktop.acquire(connection)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, session.Origin+"/api/account", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Origin", session.Origin)
	response, err := session.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusLocked:
		return false, nil
	default:
		return false, errors.New(response.Status)
	}
	var account struct {
		Unlocked bool `json:"unlocked"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&account); err != nil {
		return false, err
	}
	return account.Unlocked, nil
}

const (
	localTerminalLocked     = "Super Cage 已锁定：解锁后再打开本机终端"
	localTerminalUnanswered = "无法确认工作环境是否已解锁，请稍后再试"
)

var localTerminalConnection = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")

func (s *Server) localTerminalDir() string { return filepath.Dir(s.localTerminalConfig()) }

func (s *Server) localTerminalRequest(w http.ResponseWriter, r *http.Request, path string) {
	if s.remote == nil || s.remote.desktop == nil {
		http.Error(w, "本机终端只能在 Super Cage 桌面应用里使用", http.StatusNotFound)
		return
	}
	switch {
	case path == "local-terminal" && r.Method == http.MethodGet:
		settings, err := localterm.LoadSettings(s.localTerminalDir())
		response := map[string]any{"settings": settings, "supported": len(localterm.Apps()) > 0}
		if err != nil {
			response["warning"] = err.Error()
		}
		jsonReply(w, response)
	case path == "local-terminal" && r.Method == http.MethodPut:
		var settings localterm.Settings
		if !readJSON(w, r, &settings) {
			return
		}
		// Not while the environment shown is locked, or cannot say whether
		// it is, as for opening a window. With none shown there is no lock
		// to keep.
		if current := s.remote.context(); current.Mode != "none" && current.ConnectionID != "" {
			switch unlocked, err := s.localTerminalUnlocked(r.Context(), current.ConnectionID); {
			case err != nil:
				http.Error(w, localTerminalUnanswered, http.StatusConflict)
				return
			case !unlocked:
				http.Error(w, localTerminalLocked, http.StatusLocked)
				return
			}
		}
		if err := writePrivate(filepath.Join(s.localTerminalDir(), localterm.SettingsFile), settings); err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]any{"settings": settings, "supported": len(localterm.Apps()) > 0})
	case path == "local-terminal/app" && r.Method == http.MethodGet:
		// Detection runs only when this is asked, never with the settings.
		var app any
		if found, ok := localterm.Pick(localTerminalDetect()); ok {
			app = found.App
		}
		jsonReply(w, map[string]any{"app": app})
	case path == "local-terminal/install-herdr" && r.Method == http.MethodPost:
		s.installLocalTerminalHerdr(w, r)
	case path == "local-terminal/open" && r.Method == http.MethodPost:
		s.openLocalTerminal(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) installLocalTerminalHerdr(w http.ResponseWriter, r *http.Request) {
	if len(localterm.Apps()) == 0 {
		http.Error(w, "当前系统暂不支持安装 Herdr 桌面端", http.StatusConflict)
		return
	}
	if current := s.remote.context(); current.Mode != "none" && current.ConnectionID != "" {
		switch unlocked, err := s.localTerminalUnlocked(r.Context(), current.ConnectionID); {
		case err != nil:
			http.Error(w, localTerminalUnanswered, http.StatusConflict)
			return
		case !unlocked:
			http.Error(w, localTerminalLocked, http.StatusLocked)
			return
		}
	}
	if !s.localTerminal.installingHerdr.CompareAndSwap(false, true) {
		http.Error(w, "正在安装 Herdr，请稍候完成后再试", http.StatusConflict)
		return
	}
	defer s.localTerminal.installingHerdr.Store(false)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := localTerminalInstallHerdr(ctx); err != nil {
		http.Error(w, "安装 Herdr 失败："+err.Error(), http.StatusBadGateway)
		return
	}
	var app any
	if found, ok := localterm.Pick(localTerminalDetect()); ok {
		app = found.App
	}
	jsonReply(w, map[string]any{"app": app, "installed": true})
}

// localTerminalOpen is what to open: one existing terminal, a new terminal
// in a project, or a conversation resumed through Cage's launcher.
type localTerminalOpen struct {
	TerminalID      string `json:"terminal_id,omitempty"`
	Observe         bool   `json:"observe,omitempty"`
	Project         string `json:"project,omitempty"`
	Kind            string `json:"kind,omitempty"`
	ClaudeAccountID string `json:"claude_account_id,omitempty"`
	CodexAccountID  string `json:"codex_account_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
}

// localTerminalArgs builds the arguments of cage attach: ids, a project path
// and the desktop's connection, never a secret. attach.ParseArgs is the
// judge of what is valid, so the window never opens on a refusal.
func localTerminalArgs(configPath, connection string, input localTerminalOpen) ([]string, error) {
	args := []string{"--config", configPath, "--connection", connection}
	switch {
	case input.TerminalID != "":
		args = append(args, "--terminal", input.TerminalID)
		if input.Observe {
			args = append(args, "--observe")
		}
	case input.SessionID != "":
		args = append(args, "--resume", input.SessionID)
	case input.Project != "":
		kind := input.Kind
		if kind == "" {
			kind = "shell"
		}
		args = append(args, "--new", kind, "--project", input.Project)
		if input.ClaudeAccountID != "" {
			args = append(args, "--claude-account", input.ClaudeAccountID)
		}
		if input.CodexAccountID != "" {
			args = append(args, "--codex-account", input.CodexAccountID)
		}
	default:
		return nil, errors.New("请选择要打开的终端、项目或会话")
	}
	if !filepath.IsAbs(configPath) || !localTerminalConnection.MatchString(connection) {
		return nil, errors.New("连接已经切换，请重新打开当前工作台")
	}
	if _, err := attach.ParseArgs(args); err != nil {
		return nil, err
	}
	if input.Project != "" && (!strings.HasPrefix(input.Project, "/") || termfilter.Controls(input.Project)) {
		return nil, errors.New("项目路径无效")
	}
	return args, nil
}

func (s *Server) openLocalTerminal(w http.ResponseWriter, r *http.Request) {
	var input localTerminalOpen
	if !readJSON(w, r, &input) {
		return
	}
	// The window opens on the environment this desktop shows now; a request
	// from a view of an earlier connection is refused.
	current := s.remote.context()
	connection := requestConnection(r)
	if current.Mode == "none" || connection == "" || connection != current.ConnectionID {
		http.Error(w, "连接已经切换，请重新打开当前工作台", http.StatusConflict)
		return
	}
	switch unlocked, err := s.localTerminalUnlocked(r.Context(), connection); {
	case err != nil:
		http.Error(w, localTerminalUnanswered, http.StatusConflict)
		return
	case !unlocked:
		http.Error(w, localTerminalLocked, http.StatusLocked)
		return
	}
	found, ok := localterm.Pick(localTerminalDetect())
	if !ok {
		http.Error(w, "这台电脑上找不到 Herdr、Ghostty 或系统终端", http.StatusConflict)
		return
	}
	args, err := localTerminalArgs(s.localTerminalConfig(), connection, input)
	if err != nil {
		requestError(w, err)
		return
	}
	command, err := s.localTerminalCommand()
	if err != nil {
		requestError(w, err)
		return
	}
	// Every window has its own files, even for one terminal opened twice.
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := hex.EncodeToString(b[:])
	if input.TerminalID != "" {
		name = strings.ToLower(input.TerminalID) + "-" + name[:8]
	}
	scratch := filepath.Join(s.localTerminalDir(), "local-terminal")
	localterm.PruneScratch(scratch, 10*time.Minute)
	plan, err := localterm.PlanLaunch(localterm.PlanInput{App: found, Command: append(command, args...), Name: name, Scratch: scratch})
	if err != nil {
		requestError(w, err)
		return
	}
	// One open at a time: one may wait while macOS asks the owner to allow
	// controlling the application, and after "Allow" every waiting open
	// would become a window.
	gate := &s.localTerminal
	if !gate.opening.CompareAndSwap(false, true) {
		http.Error(w, "正在打开上一个本机终端窗口：如果 macOS 在询问权限，先选择后再试", http.StatusConflict)
		return
	}
	defer gate.opening.Store(false)
	if !gate.allow(time.Now()) {
		http.Error(w, "打开得太频繁：请稍等片刻再试", http.StatusTooManyRequests)
		return
	}
	// The launch outlives the request: stopping osascript mid-prompt leaves
	// the prompt on screen with nothing behind it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 150*time.Second)
	defer cancel()
	if err := localterm.Launch(ctx, plan, localTerminalRunner); err != nil {
		http.Error(w, "无法在 "+found.Name+" 中打开："+err.Error(), http.StatusBadGateway)
		return
	}
	jsonReply(w, map[string]any{"app": found.Name})
}

// pruneLocalTerminal removes what launches of an earlier run left behind:
// the desktop is starting, so none of it is still on its way to a window.
func (s *Server) pruneLocalTerminal() {
	localterm.PruneScratch(filepath.Join(s.localTerminalDir(), "local-terminal"), 0)
}

func (s *Server) localTerminalConfig() string {
	if s.appUpdateConfigPath != "" {
		return s.appUpdateConfigPath
	}
	return s.configPath
}

// localTerminalCommand is the program that attaches, by absolute path: the
// slim cage-attach beside this app's cage, which keeps an attached window
// small, or else cage attach.
func (s *Server) localTerminalCommand() (localterm.Command, error) {
	path := s.exe
	if path == "" || !filepath.IsAbs(path) {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		path = executable
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("本机终端应用还不支持 Windows")
	}
	if slim := attach.Beside(path); slim != "" {
		return localterm.Command{slim}, nil
	}
	return localterm.Command{path, "attach"}, nil
}
