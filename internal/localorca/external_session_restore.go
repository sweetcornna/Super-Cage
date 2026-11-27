package localorca

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// AgentPlacement is the metadata needed to find an existing Orca pane. Orca
// also returns titles and snippets in these commands. They are never retained.
type AgentPlacement struct {
	SessionID  string `json:"orcaSessionId"`
	Kind       string `json:"kind"`
	Provider   string `json:"agent"`
	WorktreeID string `json:"worktreeId"`
	HostID     string `json:"hostId"`
	PaneKey    string `json:"paneKey"`
	State      string `json:"state"`
}

type TerminalPlacement struct {
	Handle          string `json:"handle"`
	TabID           string `json:"tabId"`
	LeafID          string `json:"leafId"`
	WorktreeID      string `json:"worktreeId"`
	WorktreePath    string `json:"worktreePath"`
	ExecutionHostID string `json:"executionHostId"`
	IncarnationID   string `json:"incarnationId"`
	Connected       bool   `json:"connected"`
	Writable        bool   `json:"writable"`
	Orphaned        bool   `json:"orphaned"`
}

func (c *Client) recoveryCLI(ctx context.Context, environment string, out any, args ...string) error {
	return c.recoveryCLIWithin(ctx, 8*time.Second, environment, out, args...)
}

func (c *Client) recoveryCLIWithin(ctx context.Context, duration time.Duration, environment string, out any, args ...string) error {
	if !validSelectorRe.MatchString(environment) {
		return errors.New("Orca 连接标识无效")
	}
	raw, err := c.execCLI(ctx, duration, append(args, "--environment", environment, "--json")...)
	if err != nil {
		return errors.New("无法查询原 Orca 连接")
	}
	result, err := parseEnvelope(raw)
	if err != nil || json.Unmarshal(result, out) != nil {
		return errors.New("Orca 未返回可验证的恢复状态")
	}
	return nil
}

func (c *Client) AgentPlacements(ctx context.Context, environment, workspace string) ([]AgentPlacement, error) {
	var result struct {
		Agents    []AgentPlacement `json:"agents"`
		Truncated bool             `json:"truncated"`
	}
	err := c.recoveryCLI(ctx, environment, &result, "agent", "list", "--worktree", "path:"+workspace)
	if err == nil && (result.Agents == nil || result.Truncated) {
		return nil, errors.New("Orca 的原工作区会话列表不完整")
	}
	return result.Agents, err
}

func (c *Client) AgentPlacement(ctx context.Context, environment, sessionID string) (string, []AgentPlacement, error) {
	var result struct {
		ID     string           `json:"orcaSessionId"`
		Places []AgentPlacement `json:"places"`
	}
	err := c.recoveryCLI(ctx, environment, &result, "agent", "show", "--id", sessionID)
	return result.ID, result.Places, err
}

func (c *Client) TerminalPlacements(ctx context.Context, environment, workspace string) ([]TerminalPlacement, error) {
	var result struct {
		Terminals []TerminalPlacement `json:"terminals"`
		Truncated bool                `json:"truncated"`
	}
	err := c.recoveryCLI(ctx, environment, &result, "terminal", "list", "--worktree", "path:"+workspace, "--limit", "200")
	if err == nil && (result.Terminals == nil || result.Truncated || len(result.Terminals) >= 200) {
		return nil, errors.New("Orca 的原工作区终端列表不完整")
	}
	return result.Terminals, err
}

func (c *Client) FocusRecoveryTerminal(ctx context.Context, environment, handle, worktreeID string) (bool, error) {
	var result struct {
		Focus struct {
			Handle     string `json:"handle"`
			WorktreeID string `json:"worktreeId"`
			Navigated  bool   `json:"navigated"`
		} `json:"focus"`
	}
	err := c.recoveryCLI(ctx, environment, &result, "terminal", "switch", "--terminal", handle)
	return result.Focus.Navigated && result.Focus.Handle == handle && result.Focus.WorktreeID == worktreeID, err
}

func (c *Client) VerifyRecoveryWorkspace(ctx context.Context, environment, worktreeID, workspace, host string) (bool, error) {
	var result struct {
		Worktree struct {
			ID              string `json:"id"`
			Path            string `json:"path"`
			HostID          string `json:"hostId"`
			ExecutionHostID string `json:"executionHostId"`
		} `json:"worktree"`
	}
	err := c.recoveryCLI(ctx, environment, &result, "worktree", "show", "--worktree", "id:"+worktreeID)
	if err != nil {
		return false, err
	}
	w := result.Worktree
	actualHost := w.HostID
	if w.ExecutionHostID != "" {
		actualHost = w.ExecutionHostID
	}
	if actualHost == "" {
		actualHost = "local"
	}
	return w.ID == worktreeID && w.Path == workspace && actualHost == host, nil
}

func (c *Client) StartRecoveryTerminal(ctx context.Context, environment, worktreeID, command string, focus bool) (TerminalPlacement, error) {
	var result struct {
		Terminal TerminalPlacement `json:"terminal"`
	}
	args := []string{"terminal", "create", "--worktree", "id:" + worktreeID, "--command", command, "--keep-open"}
	if focus {
		args = append(args, "--focus")
	}
	err := c.recoveryCLI(ctx, environment, &result, args...)
	return result.Terminal, err
}

func (c *Client) WaitForRecoveryExit(ctx context.Context, environment, handle string) (bool, error) {
	var result struct {
		Wait struct {
			Handle    string `json:"handle"`
			Condition string `json:"condition"`
			Status    string `json:"status"`
			Satisfied bool   `json:"satisfied"`
			ExitCode  *int   `json:"exitCode"`
		} `json:"wait"`
	}
	err := c.recoveryCLIWithin(ctx, 34*time.Second, environment, &result, "terminal", "wait", "--terminal", handle, "--for", "exit", "--timeout-ms", "30000")
	wait := result.Wait
	return wait.Handle == handle && wait.Condition == "exit" && wait.Status == "exited" &&
		wait.Satisfied && wait.ExitCode != nil && *wait.ExitCode == 0, err
}
