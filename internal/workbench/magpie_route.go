package workbench

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/mattn/go-shellwords"
)

var errMagpieRouteUnknown = errors.New("此启动的 Magpie 路由待确认；请在 Magpie 设置检查对应配置，或在客机同一目录使用 cage-agent route status 查看。")

type magpieRoute struct {
	State     string `json:"state"`
	Scope     string `json:"scope,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
	Legacy    bool   `json:"legacy,omitempty"`
	Review    string `json:"review,omitempty"`
	Saved     bool   `json:"saved,omitempty"`
}

// Only direct commands can be inspected without running shell code. Custom
// shell launchers (including ccs and claude-magpie) own their own routing.
func magpieCommandArgs(agent, command string) ([]string, bool) {
	if strings.ContainsAny(command, "$`;&|<>\r\n") {
		return nil, false
	}
	argv, err := shellwords.Parse(command)
	if err != nil || len(argv) == 0 || path.Base(argv[0]) != agent {
		return nil, false
	}
	for _, arg := range argv[1:] {
		if arg == "resume" || arg == "unarchive" || arg == "--resume" || arg == "-r" || arg == "--continue" || arg == "-c" && agent == "claude" || strings.HasPrefix(arg, "--resume=") || strings.HasPrefix(arg, "-r=") {
			return nil, false
		}
	}
	return append([]string{}, argv[1:]...), true
}

func (s *Server) magpieRoutes(ctx context.Context, workspace string, commands map[string][]string, action, review string) (map[string]magpieRoute, error) {
	result := make(map[string]magpieRoute)
	if s.windowsGuest != nil || !s.vmRunning() && s.agentCLIs.runtimeHook == nil {
		return result, nil
	}
	if workspace != "" {
		workspace = s.cliWorkspace(workspace)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	err := s.runAgentRuntime(ctx, "magpie-route", map[string]any{"agents": commands, "workspace": workspace, "action": action, "review": review}, &result)
	return result, err
}

func (s *Server) magpieNewLaunch(agent, command, workspace string) (bool, error) {
	args, direct := magpieCommandArgs(agent, command)
	if !direct {
		return false, nil
	}
	s.mu.Lock()
	update := s.guestUpdateRequired
	s.mu.Unlock()
	if update {
		return false, nil // existing component gate still owns managed launches
	}
	result, err := s.magpieRoutes(s.lifetimeContext(), workspace, map[string][]string{agent: args}, "status", "")
	if err != nil {
		return false, err
	}
	switch result[agent].State {
	case "magpie", "native":
		return true, nil
	case "unknown", "unsupported":
		return false, errMagpieRouteUnknown
	default:
		return false, nil
	}
}

func (s *Server) magpieNativeAutoEligible(agent string) bool {
	switch agent {
	case "claude":
		s.claudeAccounts.mu.Lock()
		defer s.claudeAccounts.mu.Unlock()
		list, err := s.loadClaudeAccounts()
		return err == nil && canonicalSchedulingMode(list.Scheduling.Mode) != "manual"
	case "codex":
		s.codexAccounts.mu.Lock()
		defer s.codexAccounts.mu.Unlock()
		list, err := s.loadCodexAccounts()
		return err == nil && canonicalSchedulingMode(list.Scheduling.Mode) != "manual"
	}
	return false
}

// This UI operates on the exact configured desktop command for a saved project.
// Other shells and Orca-injected homes confirm from inside that scope instead.
func (s *Server) magpieRouteHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agent, Action, Workspace, Review string
	}
	if !readJSON(w, r, &in) {
		return
	}
	if (in.Agent != "claude" && in.Agent != "codex") || (in.Action != "status" && in.Action != "magpie" && in.Action != "cage" && in.Action != "revoke") {
		http.Error(w, "无效的启动路由操作", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	prefs := s.preferences
	ready := s.ready && !s.closing && !s.guestUpdateRequired
	s.mu.Unlock()
	if s.windowsGuest != nil {
		jsonReply(w, magpieRoute{State: "unsupported"})
		return
	}
	if !ready {
		http.Error(w, "请先启动支持此功能的环境并同步环境组件", http.StatusConflict)
		return
	}
	command := claudeTerminalCommand(prefs)
	if in.Agent == "codex" {
		command = codexTerminalCommand(prefs)
	}
	args, direct := magpieCommandArgs(in.Agent, command)
	if !direct {
		jsonReply(w, magpieRoute{State: "unsupported"})
		return
	}
	workspace := s.workspace()
	if in.Workspace != "" {
		var err error
		workspace, err = s.projectWorkspace(in.Workspace)
		if err != nil {
			requestError(w, err)
			return
		}
	}
	result, err := s.magpieRoutes(r.Context(), workspace, map[string][]string{in.Agent: args}, in.Action, in.Review)
	if err != nil {
		requestError(w, err)
		return
	}
	state := result[in.Agent]
	if state.State == "" {
		state.State = "unsupported"
	}
	jsonReply(w, state)
}
