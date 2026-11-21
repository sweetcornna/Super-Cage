package workbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/localterm"
)

// The "本机终端" client routes belong to the desktop on the owner's computer:
// the choice of application and two switches, kept in the desktop's local
// client configuration, and opening a Cage terminal in that application.
// They never reach an environment or the portal.
//
//	GET  /api/client/local-terminal        this computer's settings
//	PUT  /api/client/local-terminal        save them
//	GET  /api/client/local-terminal/apps   known applications, detected now
//	POST /api/client/local-terminal/open   open a terminal, project or session

// localTerminalRunner runs launch steps; tests replace it so that nothing
// is ever launched.
var localTerminalRunner localterm.Runner = localterm.ExecRunner{}

// localTerminalDetect finds the installed applications; tests replace it.
var localTerminalDetect = localterm.Detect

var localTerminalConnection = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")

func (s *Server) localTerminalDir() string { return filepath.Dir(s.localTerminalConfig()) }

type localTerminalApp struct {
	localterm.App
	Installed bool `json:"installed"`
}

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
		if err := settings.Validate(); err != nil {
			requestError(w, err)
			return
		}
		if app := localterm.Lookup(settings.App); app != nil && !app.Hosts {
			requestError(w, errors.New(app.Note))
			return
		}
		if err := writePrivate(filepath.Join(s.localTerminalDir(), localterm.SettingsFile), settings); err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]any{"settings": settings, "supported": len(localterm.Apps()) > 0})
	case path == "local-terminal/apps" && r.Method == http.MethodGet:
		installed := map[string]bool{}
		for _, app := range localTerminalDetect() {
			installed[app.ID] = true
		}
		apps := []localTerminalApp{}
		for _, app := range localterm.Apps() {
			apps = append(apps, localTerminalApp{App: app, Installed: installed[app.ID]})
		}
		jsonReply(w, map[string]any{"apps": apps})
	case path == "local-terminal/open" && r.Method == http.MethodPost:
		s.openLocalTerminal(w, r)
	default:
		http.NotFound(w, r)
	}
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
	if input.Project != "" && (!strings.HasPrefix(input.Project, "/") || strings.ContainsAny(input.Project, "\x00\r\n")) {
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
	dir := s.localTerminalDir()
	settings, _ := localterm.LoadSettings(dir)
	app := localterm.Lookup(settings.App)
	if app == nil {
		http.Error(w, "还没有选择本机终端应用：在设置的“本机终端应用”里选择一个", http.StatusConflict)
		return
	}
	var found *localterm.Installed
	for _, candidate := range localTerminalDetect() {
		if candidate.ID == app.ID {
			found = &candidate
			break
		}
	}
	if found == nil {
		http.Error(w, app.Name+" 没有安装在这台电脑上", http.StatusConflict)
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
	home, err := os.UserHomeDir()
	if err != nil {
		requestError(w, err)
		return
	}
	name := strings.ToLower(input.TerminalID)
	if name == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		name = hex.EncodeToString(b[:])
	}
	scratch := filepath.Join(dir, "local-terminal")
	localterm.PruneScratch(scratch, home, 10*time.Minute)
	plan, err := localterm.PlanLaunch(localterm.PlanInput{App: *found, Command: append(command, args...), Name: name, Scratch: scratch, Home: home})
	if err != nil {
		requestError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := localterm.Launch(ctx, plan, localTerminalRunner); err != nil {
		http.Error(w, "无法在 "+app.Name+" 中打开："+err.Error(), http.StatusBadGateway)
		return
	}
	jsonReply(w, map[string]any{"app": app.Name, "automation": app.Automation})
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
