package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

func claudeAuthorizationURL(raw string) bool {
	if len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t \\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery == "" {
		return false
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if path != "/oauth/authorize" && path != "/cai/oauth/authorize" {
		return false
	}
	switch strings.ToLower(u.Host) {
	case "claude.ai", "claude.com", "console.anthropic.com", "platform.claude.com", "platform.anthropic.com":
		return true
	}
	return false
}

func hostAllowed(host string, allowedHosts []string) bool {
	if host == "" {
		return false
	}
	for _, candidate := range allowedHosts {
		if strings.ToLower(strings.TrimSpace(candidate)) == host {
			return true
		}
	}
	return false
}

// agentAuthorizationURL validates official HTTPS OAuth and device-code URLs for
// any built-in or custom Agent CLI registered in Cage, strictly honoring allowedHosts.
func agentAuthorizationURL(raw string, allowedHosts []string) bool {
	if claudeAuthorizationURL(raw) {
		u, err := url.Parse(raw)
		return err == nil && hostAllowed(strings.ToLower(u.Hostname()), allowedHosts)
	}
	return agentAuthorizationURLOnly(raw, allowedHosts)
}

func agentAuthorizationURLOnly(raw string, allowedHosts []string) bool {
	if len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t \\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if !hostAllowed(host, allowedHosts) {
		return false
	}
	// Claude hosts always keep strict path + query checking.
	switch host {
	case "claude.ai", "claude.com", "console.anthropic.com", "platform.claude.com", "platform.anthropic.com":
		return false
	}
	// Require a meaningful login/oauth/auth/device path so arbitrary repo or
	// marketing URLs with query parameters don't hijack ephemeral login tabs.
	lowerPath := strings.ToLower(strings.TrimSuffix(u.EscapedPath(), "/"))
	for _, token := range []string{"oauth", "auth", "login", "device", "signin", "sign-in", "authorize", "verify", "cli"} {
		if strings.Contains(lowerPath, token) {
			return true
		}
	}
	return false
}

func terminalAllowsLoginURL(raw, agentID string, allowedHosts []string) bool {
	if claudeAuthorizationURL(raw) {
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		if agentID != "" && agentID != "claude" && !hostAllowed(strings.ToLower(u.Hostname()), allowedHosts) {
			return false
		}
		return hostAllowed(strings.ToLower(u.Hostname()), allowedHosts)
	}
	return agentAuthorizationURLOnly(raw, allowedHosts)
}

func (s *Server) terminalLoginLink(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL string `json:"url"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	terminal := s.terminals.get(r.PathValue("id"))
	allowedHosts := s.allAgentOAuthDomains()
	agentID := ""
	if terminal != nil {
		terminal.mu.Lock()
		agentID = terminal.agentID
		if agentID != "" {
			// Agent terminals are strictly scoped to their own OAuth allowlist
			// (even when empty, e.g., API-key-only agents never inherit other providers).
			allowedHosts = slices.Clone(terminal.agentOAuthDomains)
		}
		terminal.mu.Unlock()
	}
	if !terminalAllowsLoginURL(input.URL, agentID, allowedHosts) {
		http.Error(w, "请选择受支持的 Agent 官方授权链接", 400)
		return
	}
	if terminal == nil {
		http.NotFound(w, r)
		return
	}
	terminal.loginLinkMu.Lock()
	defer terminal.loginLinkMu.Unlock()
	if !terminal.ownsConversation() {
		http.Error(w, "登录终端已关闭，请重新发起登录", 409)
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		http.Error(w, "请先启动工作环境", 409)
		return
	}

	login := terminal.id
	if terminal.loginGuestID != "" {
		// Match the official CLI helper's private browser and cleanup key.
		login = terminal.loginGuestID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	bodyMap := map[string]any{"login": login, "url": input.URL}
	if !claudeAuthorizationURL(input.URL) {
		bodyMap["allowed_hosts"] = allowedHosts
	}
	payload, _ := json.Marshal(bodyMap)
	data, err := s.guestAPI(ctx, token, "POST", "/login-open", payload)
	if terminal.loginGuestID == "" {
		terminal.loginLinkCleanup.Do(func() {
			go func() {
				lifetime := s.lifetimeContext()
				select {
				case <-lifetime.Done():
					return
				case <-terminal.done:
				}
				terminal.loginLinkMu.Lock()
				defer terminal.loginLinkMu.Unlock()
				ctx, cancel := context.WithTimeout(lifetime, 8*time.Second)
				defer cancel()
				body, _ := json.Marshal(map[string]string{"login": login})
				_, _ = s.guestAPI(ctx, token, "POST", "/login-finish", body)
			}()
		})
	}
	var tab struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(data, &tab) != nil || tab.ID == "" {
		http.Error(w, "Cage 浏览器未能打开授权页，请检查浏览器连接并重试", 502)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonReply(w, tab)
}
