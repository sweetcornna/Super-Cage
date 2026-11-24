package agentcatalog

import (
	"slices"
	"strings"
	"testing"
)

func TestBuiltInCatalogIntegrity(t *testing.T) {
	specs := BuiltIn()
	if len(specs) != 15 {
		t.Fatalf("expected 15 built-in mainstream agents, got %d", len(specs))
	}
	expectedIDs := []string{
		"claude", "codex", "antigravity", "opencode", "copilot", "qwen",
		"amp", "cline", "cursor", "vibe", "kimi", "aider",
		"droid", "goose", "pi",
	}
	seenID := map[string]bool{}
	for _, spec := range specs {
		if seenID[spec.ID] {
			t.Fatalf("duplicate built-in id %q", spec.ID)
		}
		seenID[spec.ID] = true
		if !agentIDPattern.MatchString(spec.ID) {
			t.Fatalf("invalid built-in id %q", spec.ID)
		}
		if !binaryPattern.MatchString(spec.Binary) {
			t.Fatalf("invalid built-in binary %q for %s", spec.Binary, spec.ID)
		}
		if len(spec.Channels) == 0 {
			t.Fatalf("built-in %s has no install channels", spec.ID)
		}
		for _, ch := range spec.Channels {
			if err := ValidateInstallChannel(ch); err != nil {
				t.Fatalf("built-in %s invalid channel %q: %v", spec.ID, ch.ID, err)
			}
		}
		if spec.Isolation.Mode != "claude_native" {
			if err := validateIsolation(&spec.Isolation); err != nil {
				t.Fatalf("built-in %s invalid isolation: %v", spec.ID, err)
			}
		}
		if err := validateAuth(&spec.Auth); err != nil {
			t.Fatalf("built-in %s invalid auth: %v", spec.ID, err)
		}
		if spec.Terminal.AddDirFlag != "" && !flagPattern.MatchString(spec.Terminal.AddDirFlag) {
			t.Fatalf("built-in %s invalid add_dir_flag %q", spec.ID, spec.Terminal.AddDirFlag)
		}
		if !hexColorPattern.MatchString(spec.AccentColor) {
			t.Fatalf("built-in %s invalid accent color %q", spec.ID, spec.AccentColor)
		}
	}
	for _, id := range expectedIDs {
		if !seenID[id] {
			t.Fatalf("missing expected built-in agent %q", id)
		}
	}
	// Verify Claude strips cloud provider flags on OAuth.
	claude, ok := Lookup("claude", nil)
	if !ok {
		t.Fatal("claude missing")
	}
	for _, v := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_GATEWAY"} {
		if !slices.Contains(claude.Auth.StripEnvOnOAuth, v) {
			t.Fatalf("claude missing strip env %q", v)
		}
	}
	// Verify Antigravity CLI (agy) replaced Gemini CLI in the built-in catalog.
	if _, hasGemini := Lookup("gemini", nil); hasGemini {
		t.Fatal("gemini should not be in the built-in catalog")
	}
	agy, ok := Lookup("antigravity", nil)
	if !ok || agy.Binary != "agy" || agy.DefaultLaunchCommand != "agy" ||
		len(agy.Channels) == 0 || agy.Channels[0].ScriptURL != "https://antigravity.google/cli/install.sh" {
		t.Fatalf("antigravity catalog spec mismatch: %+v", agy)
	}
	if agy.Terminal.AddDirFlag != "--add-dir" || !slices.Equal(agy.Terminal.PresetFlags, []string{"--continue", "--mode auto", "--sandbox"}) {
		t.Fatalf("antigravity terminal config mismatch: %+v", agy.Terminal)
	}
	if !slices.Contains(agy.Auth.OAuthDomains, "antigravity.google") || !slices.Contains(agy.Auth.OAuthDomains, "accounts.google.com") {
		t.Fatalf("antigravity oauth domains mismatch: %+v", agy.Auth.OAuthDomains)
	}
	for _, v := range []string{"GEMINI_API_KEY", "AGY_ADC_AUTH", "GOOGLE_CLOUD_PROJECT", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if !slices.Contains(agy.Auth.StripEnvOnOAuth, v) {
			t.Fatalf("antigravity missing strip env %q", v)
		}
	}
	// Verify Codex supports OAuth, device code, API key, AddDirFlag, and preset flags.
	codex, ok := Lookup("codex", nil)
	if !ok || !codex.SupportsAuth("oauth") || !codex.SupportsAuth("device_code") || !codex.SupportsAuth("api_key") {
		t.Fatalf("codex auth modes incomplete: %+v", codex.Auth)
	}
	if codex.Terminal.AddDirFlag != "--add-dir" || !slices.Equal(codex.Terminal.PresetFlags, []string{"--full-auto", "--search", "--sandbox read-only"}) {
		t.Fatalf("codex terminal config mismatch: %+v", codex.Terminal)
	}
	// Verify Copilot login args, OAuth domains, and credential files.
	copilot, ok := Lookup("copilot", nil)
	if !ok || !slices.Equal(copilot.Auth.LoginArgs, []string{"login"}) || !slices.Equal(copilot.Auth.DeviceLoginArgs, []string{"login", "--device-code"}) ||
		!slices.Contains(copilot.Auth.OAuthDomains, "auth.githubcopilot.com") || !slices.Contains(copilot.Auth.CredentialFiles, "github-copilot/hosts.json") {
		t.Fatalf("copilot auth config mismatch: %+v", copilot.Auth)
	}
	// Verify Cursor, Kimi, Cline, Vibe false-positive credential files are removed and isolation fixed.
	cursor, ok := Lookup("cursor", nil)
	if !ok || cursor.Isolation.Mode != "env_dir" || cursor.Isolation.EnvVar != "CURSOR_CONFIG_DIR" ||
		cursor.Isolation.ExtraEnv["XDG_CONFIG_HOME"] != "{config}" || slices.Contains(cursor.Auth.CredentialFiles, "cli-config.json") {
		t.Fatalf("cursor config mismatch: iso=%+v auth=%+v", cursor.Isolation, cursor.Auth)
	}
	kimi, ok := Lookup("kimi", nil)
	if !ok || !slices.Contains(kimi.Channels[0].Prerequisites, "node>=22") ||
		kimi.Isolation.ExtraEnv["KIMI_SHARE_DIR"] != "{data}" || kimi.Isolation.ExtraEnv["KIMI_CODE_HOME"] != "{config}" ||
		slices.Contains(kimi.Auth.CredentialFiles, "config.toml") {
		t.Fatalf("kimi config mismatch: ch=%+v iso=%+v auth=%+v", kimi.Channels, kimi.Isolation, kimi.Auth)
	}
	cline, ok := Lookup("cline", nil)
	if !ok || slices.Contains(cline.Auth.CredentialFiles, "config.json") || slices.Contains(cline.Auth.CredentialFiles, "data/settings/globalState.json") {
		t.Fatalf("cline credential files mismatch: %+v", cline.Auth.CredentialFiles)
	}
	vibe, ok := Lookup("vibe", nil)
	if !ok || slices.Contains(vibe.Auth.CredentialFiles, "config.toml") || !slices.Contains(vibe.Auth.CredentialFiles, "auth.json") {
		t.Fatalf("vibe credential files mismatch: %+v", vibe.Auth.CredentialFiles)
	}
	// Verify new built-ins: droid, goose, pi.
	droid, ok := Lookup("droid", nil)
	if !ok || droid.Isolation.EnvVar != "FACTORY_HOME" || !slices.Contains(droid.Auth.OAuthDomains, "auth.factory.ai") {
		t.Fatalf("droid config mismatch: %+v", droid)
	}
	goose, ok := Lookup("goose", nil)
	if !ok || goose.Isolation.Mode != "xdg" || goose.Auth.BaseURLEnvVar != "OPENAI_HOST" || goose.Auth.ModelEnvVar != "GOOSE_MODEL" {
		t.Fatalf("goose config mismatch: %+v", goose)
	}
	pi, ok := Lookup("pi", nil)
	if !ok || pi.Isolation.EnvVar != "PI_CODING_AGENT_DIR" || !slices.Contains(pi.Auth.OAuthDomains, "pi.dev") {
		t.Fatalf("pi config mismatch: %+v", pi)
	}
	// Verify AllOAuthDomains includes all newly added domains.
	allDomains := AllOAuthDomains(nil)
	for _, host := range []string{"auth.githubcopilot.com", "app.factory.ai", "auth.factory.ai", "factory.ai", "pi.dev"} {
		if !slices.Contains(allDomains, host) {
			t.Fatalf("AllOAuthDomains missing %q", host)
		}
	}
}

func TestValidateCustomSpecSecurityAndDefaults(t *testing.T) {
	valid := AgentSpec{
		ID:     "my-coder",
		Name:   "My Coder",
		Binary: "my-coder",
		Channels: []InstallChannel{
			{ID: "npm-main", Kind: "npm", Label: "npm", Package: "@myorg/my-coder"},
		},
		Isolation: IsolationSpec{
			Mode:            "env_dir",
			EnvVar:          "MY_CODER_HOME",
			SharedResources: []string{"AGENTS.md", "skills"},
		},
		Auth: AuthSpec{
			Methods:       []string{"oauth", "api_key"},
			OAuthDomains:  []string{"auth.mycoder.dev"},
			APIKeyEnvVars: []string{"MY_CODER_API_KEY"},
			BaseURLEnvVar: "MY_CODER_BASE_URL",
		},
		Terminal: TerminalSpec{
			AddDirFlag: "--add-dir",
		},
	}
	normalized, err := ValidateCustomSpec(valid)
	if err != nil {
		t.Fatalf("valid custom spec rejected: %v", err)
	}
	if normalized.BuiltIn || normalized.DefaultLaunchCommand != "my-coder" || normalized.ShortName != "My Coder" || normalized.Terminal.AddDirFlag != "--add-dir" {
		t.Fatalf("unexpected normalized fields: %+v", normalized)
	}

	// Verify OAuth domain aggregation includes custom domain.
	domains := AllOAuthDomains([]AgentSpec{normalized})
	if !slices.Contains(domains, "auth.mycoder.dev") || !slices.Contains(domains, "auth.openai.com") {
		t.Fatalf("missing oauth domains: %v", domains)
	}

	cases := []struct {
		name   string
		mutate func(*AgentSpec)
	}{
		{"built-in collision", func(s *AgentSpec) { s.ID = "codex" }},
		{"reserved id", func(s *AgentSpec) { s.ID = "shell" }},
		{"shell metachar binary", func(s *AgentSpec) { s.Binary = "my;rm -rf /" }},
		{"npm injection", func(s *AgentSpec) { s.Channels[0].Package = "@org/pkg --registry=http://evil" }},
		{"http script url", func(s *AgentSpec) { s.Channels = []InstallChannel{{ID: "s", Kind: "script", Label: "s", ScriptURL: "http://example.com/install.sh"}} }},
		{"script userinfo", func(s *AgentSpec) { s.Channels = []InstallChannel{{ID: "s", Kind: "script", Label: "s", ScriptURL: "https://user:pass@example.com/install.sh"}} }},
		{"LD_PRELOAD env", func(s *AgentSpec) { s.Auth.APIKeyEnvVars = []string{"LD_PRELOAD"} }},
		{"PATH env", func(s *AgentSpec) { s.Isolation.EnvVar = "PATH" }},
		{"CAGE_ prefix env", func(s *AgentSpec) { s.Auth.APIKeyEnvVars = []string{"CAGE_SECRET"} }},
		{"localhost oauth domain", func(s *AgentSpec) { s.Auth.OAuthDomains = []string{"localhost"} }},
		{"ip oauth domain", func(s *AgentSpec) { s.Auth.OAuthDomains = []string{"127.0.0.1"} }},
		{"shared path traversal", func(s *AgentSpec) { s.Isolation.SharedResources = []string{"../.ssh"} }},
		{"shared credentials file", func(s *AgentSpec) { s.Isolation.SharedResources = []string{"auth.json"} }},
		{"multiline launch command", func(s *AgentSpec) { s.DefaultLaunchCommand = "my-coder\nid" }},
		{"builtin channel in custom", func(s *AgentSpec) { s.Channels[0].Kind = "builtin" }},
		{"invalid add_dir_flag no dash", func(s *AgentSpec) { s.Terminal.AddDirFlag = "add-dir" }},
		{"invalid add_dir_flag injection", func(s *AgentSpec) { s.Terminal.AddDirFlag = "--add-dir;id" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid.Clone()
			tc.mutate(&candidate)
			if _, err := ValidateCustomSpec(candidate); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestValidateLaunchCommandLength(t *testing.T) {
	if err := ValidateLaunchCommand(strings.Repeat("a", 4097)); err == nil {
		t.Fatal("overlong command accepted")
	}
	if err := ValidateLaunchCommand("codex --full-auto"); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}
}
