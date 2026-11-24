// Package agentcatalog defines the unified, data-driven specification for
// built-in mainstream coding Agent CLIs and user-registered custom agents.
// All installation channels, isolation boundaries, OAuth domains, credential
// probes and terminal optimizations are driven by this single source of truth.
package agentcatalog

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// InstallChannel describes one official (or user-defined) installation source.
type InstallChannel struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"` // builtin, npm, uv, pip, script, existing
	Label         string   `json:"label"`
	Package       string   `json:"package,omitempty"`
	ScriptURL     string   `json:"script_url,omitempty"`
	Prerequisites []string `json:"prerequisites,omitempty"` // e.g. "node>=22", "python3", "uv", "git"
	CommandHint   string   `json:"command_hint,omitempty"`
	Recommended   bool     `json:"recommended,omitempty"`
}

// IsolationSpec defines how per-account configuration and state directories are
// isolated inside the guest without leaking credentials across accounts.
type IsolationSpec struct {
	Mode               string            `json:"mode"` // claude_native, env_dir, env_home, xdg, flag_dir, api_key_only
	EnvVar             string            `json:"env_var,omitempty"`
	ConfigSubdir       string            `json:"config_subdir,omitempty"` // e.g. ".gemini" when Mode == "env_home"
	ConfigFlag         string            `json:"config_flag,omitempty"`   // e.g. "--config-dir" when Mode == "flag_dir"
	ExtraEnv           map[string]string `json:"extra_env,omitempty"`     // supports {profile}, {config}, {data}, {state}
	DefaultConfigPaths []string          `json:"default_config_paths,omitempty"`
	SharedResources    []string          `json:"shared_resources,omitempty"` // relative paths safely symlinked across accounts
}

// AuthSpec defines supported authentication flows and non-secret identity checks.
type AuthSpec struct {
	Methods         []string `json:"methods"` // oauth, device_code, api_key
	LoginArgs       []string `json:"login_args,omitempty"`
	DeviceLoginArgs []string `json:"device_login_args,omitempty"`
	StatusArgs      []string `json:"status_args,omitempty"`
	OAuthDomains    []string `json:"oauth_domains,omitempty"`
	APIKeyEnvVars   []string `json:"api_key_env_vars,omitempty"`
	BaseURLEnvVar   string   `json:"base_url_env_var,omitempty"`
	ModelEnvVar     string   `json:"model_env_var,omitempty"`
	StripEnvOnOAuth []string `json:"strip_env_on_oauth,omitempty"`
	CredentialFiles []string `json:"credential_files,omitempty"`
	LoginHint       string   `json:"login_hint,omitempty"`
}

// TerminalSpec defines agent-specific terminal ergonomics and defaults.
type TerminalSpec struct {
	ImagePasteShortcut bool              `json:"image_paste_shortcut"`
	ShiftEnterMode     string            `json:"shift_enter_mode"` // bracketed_newline, csi_u, plain
	MouseCopyHint      bool              `json:"mouse_copy_hint"`
	AddDirFlag         string            `json:"add_dir_flag,omitempty"`
	PresetFlags        []string          `json:"preset_flags,omitempty"`
	EnvDefaults        map[string]string `json:"env_defaults,omitempty"`
}

// AgentSpec is the complete contract for an Agent CLI.
type AgentSpec struct {
	ID                   string           `json:"id"`
	Name                 string           `json:"name"`
	ShortName            string           `json:"short_name"`
	Vendor               string           `json:"vendor"`
	Category             string           `json:"category"` // first-party, multi-provider, open-source, custom
	Description          string           `json:"description"`
	Homepage             string           `json:"homepage,omitempty"`
	QuickWebURL          string           `json:"quick_web_url,omitempty"`
	AccentColor          string           `json:"accent_color"`
	Glyph                string           `json:"glyph"`
	BuiltIn              bool             `json:"built_in"`
	Binary               string           `json:"binary"`
	FallbackBinaries     []string         `json:"fallback_binaries,omitempty"`
	VersionArgs          []string         `json:"version_args,omitempty"`
	DefaultLaunchCommand string           `json:"default_launch_command"`
	Channels             []InstallChannel `json:"channels"`
	Isolation            IsolationSpec    `json:"isolation"`
	Auth                 AuthSpec         `json:"auth"`
	Terminal             TerminalSpec     `json:"terminal"`
}

var (
	agentIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	binaryPattern       = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	envVarPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	npmPackagePattern   = regexp.MustCompile(`^(@[a-z0-9-~][a-z0-9-._~]*/)?[a-z0-9-~][a-z0-9-._~]*$`)
	pyPackagePattern    = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,126}[a-zA-Z0-9])?$`)
	domainPattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	hexColorPattern     = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	flagPattern         = regexp.MustCompile(`^--?[a-zA-Z0-9][a-zA-Z0-9._=-]{0,63}$`)
	configSubdirPattern = regexp.MustCompile(`^\.?[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
)

var reservedAgentIDs = map[string]bool{
	"shell": true, "default": true, "auto": true, "all": true,
	"system": true, "custom": true, "install": true, "uninstall": true,
	"config": true, "probe": true, "accounts": true, "login": true,
	"agent": true, "agent-login": true, "claude-login": true,
}

var forbiddenEnvVars = map[string]bool{
	"PATH": true, "LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true, "DYLD_FRAMEWORK_PATH": true,
	"BASH_ENV": true, "ENV": true, "PROMPT_COMMAND": true, "IFS": true,
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "PYTHONPATH": true, "PYTHONSTARTUP": true,
	"NODE_OPTIONS": true, "NODE_PATH": true, "RUBYOPT": true, "PERL5OPT": true,
	"GIT_SSH_COMMAND": true, "GIT_EXEC_PATH": true, "BROWSER": true, "TERM": true,
}

var forbiddenSharedNames = map[string]bool{
	".credentials.json": true, "credentials.json": true, "auth.json": true,
	"oauth_creds.json": true, "secrets.json": true, ".env": true,
	"token.json": true, "tokens.json": true, "hosts.json": true, "apps.json": true,
	"google_accounts.json": true,
}

// BuiltIn returns a fresh copy of all built-in mainstream Agent CLI specs.
func BuiltIn() []AgentSpec {
	list := make([]AgentSpec, len(builtInCatalog))
	for i, spec := range builtInCatalog {
		list[i] = spec.Clone()
	}
	return list
}

// Clone deep-copies slices and maps inside an AgentSpec.
func (s AgentSpec) Clone() AgentSpec {
	out := s
	out.FallbackBinaries = slices.Clone(s.FallbackBinaries)
	out.VersionArgs = slices.Clone(s.VersionArgs)
	out.Channels = make([]InstallChannel, len(s.Channels))
	for i, ch := range s.Channels {
		ch.Prerequisites = slices.Clone(ch.Prerequisites)
		out.Channels[i] = ch
	}
	out.Isolation.DefaultConfigPaths = slices.Clone(s.Isolation.DefaultConfigPaths)
	out.Isolation.SharedResources = slices.Clone(s.Isolation.SharedResources)
	if s.Isolation.ExtraEnv != nil {
		out.Isolation.ExtraEnv = make(map[string]string, len(s.Isolation.ExtraEnv))
		for k, v := range s.Isolation.ExtraEnv {
			out.Isolation.ExtraEnv[k] = v
		}
	}
	out.Auth.Methods = slices.Clone(s.Auth.Methods)
	out.Auth.LoginArgs = slices.Clone(s.Auth.LoginArgs)
	out.Auth.DeviceLoginArgs = slices.Clone(s.Auth.DeviceLoginArgs)
	out.Auth.StatusArgs = slices.Clone(s.Auth.StatusArgs)
	out.Auth.OAuthDomains = slices.Clone(s.Auth.OAuthDomains)
	out.Auth.APIKeyEnvVars = slices.Clone(s.Auth.APIKeyEnvVars)
	out.Auth.StripEnvOnOAuth = slices.Clone(s.Auth.StripEnvOnOAuth)
	out.Auth.CredentialFiles = slices.Clone(s.Auth.CredentialFiles)
	out.Terminal.PresetFlags = slices.Clone(s.Terminal.PresetFlags)
	if s.Terminal.EnvDefaults != nil {
		out.Terminal.EnvDefaults = make(map[string]string, len(s.Terminal.EnvDefaults))
		for k, v := range s.Terminal.EnvDefaults {
			out.Terminal.EnvDefaults[k] = v
		}
	}
	return out
}

// Lookup finds an AgentSpec by ID across built-in and custom specs.
func Lookup(id string, custom []AgentSpec) (AgentSpec, bool) {
	for _, spec := range builtInCatalog {
		if spec.ID == id {
			return spec.Clone(), true
		}
	}
	for _, spec := range custom {
		if spec.ID == id {
			return spec.Clone(), true
		}
	}
	return AgentSpec{}, false
}

// IsBuiltIn reports whether id belongs to the built-in catalog.
func IsBuiltIn(id string) bool {
	for _, spec := range builtInCatalog {
		if spec.ID == id {
			return true
		}
	}
	return false
}

// All returns built-in specs followed by validated custom specs.
func All(custom []AgentSpec) []AgentSpec {
	out := BuiltIn()
	for _, spec := range custom {
		if !IsBuiltIn(spec.ID) {
			cp := spec.Clone()
			cp.BuiltIn = false
			out = append(out, cp)
		}
	}
	return out
}

// AllOAuthDomains returns the deduplicated set of official HTTPS OAuth hosts
// across built-in and custom agents.
func AllOAuthDomains(custom []AgentSpec) []string {
	seen := map[string]bool{}
	var domains []string
	for _, spec := range All(custom) {
		for _, host := range spec.Auth.OAuthDomains {
			h := strings.ToLower(strings.TrimSpace(host))
			if h != "" && !seen[h] {
				seen[h] = true
				domains = append(domains, h)
			}
		}
	}
	return domains
}

// FindChannel returns the matching install channel (or the recommended/first one when id is empty).
func (s AgentSpec) FindChannel(channelID string) (InstallChannel, bool) {
	if len(s.Channels) == 0 {
		return InstallChannel{}, false
	}
	if channelID == "" {
		for _, ch := range s.Channels {
			if ch.Recommended {
				return ch, true
			}
		}
		return s.Channels[0], true
	}
	for _, ch := range s.Channels {
		if ch.ID == channelID {
			return ch, true
		}
	}
	return InstallChannel{}, false
}

// SupportsAuth reports whether the agent supports a given auth method.
func (s AgentSpec) SupportsAuth(method string) bool {
	return slices.Contains(s.Auth.Methods, method)
}

// ValidateEnvVarName enforces safe environment variable identifiers.
func ValidateEnvVarName(name string) error {
	if !envVarPattern.MatchString(name) {
		return errors.New("环境变量名须为大写字母、数字和下划线，且以字母开头")
	}
	if forbiddenEnvVars[name] || strings.HasPrefix(name, "CAGE_") {
		return errors.New("不允许覆盖受保护的系统环境变量: " + name)
	}
	return nil
}

// ValidateOAuthDomain ensures an OAuth host is a safe public DNS hostname.
func ValidateOAuthDomain(host string) error {
	h := strings.ToLower(strings.TrimSpace(host))
	if len(h) > 253 || !domainPattern.MatchString(h) {
		return errors.New("OAuth 域名格式无效: " + host)
	}
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") || net.ParseIP(h) != nil {
		return errors.New("OAuth 域名不能为本机地址、内网后缀或 IP: " + host)
	}
	return nil
}

// ValidateLaunchCommand enforces single-line, bounded launch commands.
func ValidateLaunchCommand(cmd string) error {
	if len(cmd) > 4096 || strings.ContainsAny(cmd, "\x00\r\n") {
		return errors.New("终端启动命令须为单行，且不超过 4096 字节")
	}
	return nil
}

// ValidateInstallChannel checks that an install channel cannot inject arbitrary shell commands.
func ValidateInstallChannel(ch InstallChannel) error {
	if strings.TrimSpace(ch.ID) == "" || !agentIDPattern.MatchString(ch.ID) {
		return errors.New("安装渠道 ID 无效（仅支持小写字母、数字和连字符）")
	}
	if strings.TrimSpace(ch.Label) == "" || utf8.RuneCountInString(ch.Label) > 64 || strings.IndexFunc(ch.Label, unicode.IsControl) >= 0 {
		return errors.New("安装渠道名称无效")
	}
	switch ch.Kind {
	case "builtin", "existing":
		return nil
	case "npm":
		pkg := strings.TrimSpace(ch.Package)
		if len(pkg) == 0 || len(pkg) > 128 || !npmPackagePattern.MatchString(pkg) {
			return errors.New("npm 包名格式无效")
		}
		return nil
	case "uv", "pip":
		pkg := strings.TrimSpace(ch.Package)
		if len(pkg) == 0 || len(pkg) > 128 || !pyPackagePattern.MatchString(pkg) {
			return errors.New("Python 包名格式无效")
		}
		return nil
	case "script":
		raw := strings.TrimSpace(ch.ScriptURL)
		if len(raw) == 0 || len(raw) > 1024 || strings.ContainsAny(raw, "\x00\r\n\t '\"") {
			return errors.New("安装脚本地址格式无效")
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return errors.New("官方安装脚本必须为有效的 HTTPS 地址且不含用户名密码")
		}
		if err := ValidateOAuthDomain(u.Hostname()); err != nil {
			return errors.New("安装脚本域名无效")
		}
		return nil
	default:
		return errors.New("不支持的安装渠道类型: " + ch.Kind)
	}
}

// ValidateCustomSpec validates and normalizes a user-created custom AgentSpec.
func ValidateCustomSpec(in AgentSpec) (AgentSpec, error) {
	spec := in.Clone()
	spec.ID = strings.ToLower(strings.TrimSpace(spec.ID))
	spec.Name = strings.TrimSpace(spec.Name)
	spec.ShortName = strings.TrimSpace(spec.ShortName)
	spec.Vendor = strings.TrimSpace(spec.Vendor)
	spec.Description = strings.TrimSpace(spec.Description)
	spec.Binary = strings.TrimSpace(spec.Binary)
	spec.DefaultLaunchCommand = strings.TrimSpace(spec.DefaultLaunchCommand)
	spec.BuiltIn = false

	if !agentIDPattern.MatchString(spec.ID) || reservedAgentIDs[spec.ID] || IsBuiltIn(spec.ID) {
		return spec, errors.New("自定义 Agent ID 须为 2-32 位小写字母/数字/连字符，且不能与内置或保留名称冲突")
	}
	if spec.Name == "" || utf8.RuneCountInString(spec.Name) > 48 || strings.IndexFunc(spec.Name, unicode.IsControl) >= 0 {
		return spec, errors.New("Agent 名称须为 1-48 个有效字符")
	}
	if spec.ShortName == "" {
		spec.ShortName = spec.Name
	}
	if utf8.RuneCountInString(spec.ShortName) > 24 || strings.IndexFunc(spec.ShortName, unicode.IsControl) >= 0 {
		return spec, errors.New("Agent 简称须为 1-24 个有效字符")
	}
	if spec.Vendor == "" {
		spec.Vendor = "自定义"
	}
	if utf8.RuneCountInString(spec.Vendor) > 48 || strings.IndexFunc(spec.Vendor, unicode.IsControl) >= 0 {
		return spec, errors.New("供应商名称过长或含非法字符")
	}
	if utf8.RuneCountInString(spec.Description) > 200 || strings.IndexFunc(spec.Description, unicode.IsControl) >= 0 {
		return spec, errors.New("描述须不超过 200 个字符")
	}
	if spec.Category == "" {
		spec.Category = "custom"
	}
	if spec.AccentColor == "" {
		spec.AccentColor = "#6366f1"
	}
	if !hexColorPattern.MatchString(spec.AccentColor) {
		return spec, errors.New("主题强调色须为 #RRGGBB 十六进制格式")
	}
	if spec.Glyph == "" {
		spec.Glyph = strings.ToUpper(spec.ID[:min(2, len(spec.ID))])
	}
	if utf8.RuneCountInString(spec.Glyph) > 4 {
		return spec, errors.New("图标标识最多 4 个字符")
	}
	if !binaryPattern.MatchString(spec.Binary) {
		return spec, errors.New("可执行文件名无效（仅支持字母、数字、点、下划线、连字符）")
	}
	for _, fb := range spec.FallbackBinaries {
		if !binaryPattern.MatchString(fb) {
			return spec, errors.New("备用可执行文件名无效: " + fb)
		}
	}
	if len(spec.VersionArgs) == 0 {
		spec.VersionArgs = []string{"--version"}
	}
	for _, arg := range spec.VersionArgs {
		if len(arg) == 0 || len(arg) > 64 || strings.ContainsAny(arg, "\x00\r\n") {
			return spec, errors.New("版本检查参数无效")
		}
	}
	if spec.DefaultLaunchCommand == "" {
		spec.DefaultLaunchCommand = spec.Binary
	}
	if err := ValidateLaunchCommand(spec.DefaultLaunchCommand); err != nil {
		return spec, err
	}
	if len(spec.Channels) == 0 {
		spec.Channels = []InstallChannel{{ID: "existing", Kind: "existing", Label: "环境已有命令", Recommended: true}}
	}
	if len(spec.Channels) > 6 {
		return spec, errors.New("最多配置 6 个安装渠道")
	}
	seenCh := map[string]bool{}
	for _, ch := range spec.Channels {
		if ch.Kind == "builtin" {
			return spec, errors.New("自定义 Agent 不能使用 builtin 渠道")
		}
		if seenCh[ch.ID] {
			return spec, errors.New("安装渠道 ID 重复: " + ch.ID)
		}
		seenCh[ch.ID] = true
		if err := ValidateInstallChannel(ch); err != nil {
			return spec, err
		}
	}
	if err := validateIsolation(&spec.Isolation); err != nil {
		return spec, err
	}
	if err := validateAuth(&spec.Auth); err != nil {
		return spec, err
	}
	if spec.Terminal.ShiftEnterMode == "" {
		spec.Terminal.ShiftEnterMode = "bracketed_newline"
	}
	if !slices.Contains([]string{"bracketed_newline", "csi_u", "plain"}, spec.Terminal.ShiftEnterMode) {
		return spec, errors.New("无效的 Shift+Enter 换行模式")
	}
	spec.Terminal.AddDirFlag = strings.TrimSpace(spec.Terminal.AddDirFlag)
	if spec.Terminal.AddDirFlag != "" && !flagPattern.MatchString(spec.Terminal.AddDirFlag) {
		return spec, errors.New("附加目录命令行参数无效（如 --add-dir）")
	}
	for k, v := range spec.Terminal.EnvDefaults {
		if err := ValidateEnvVarName(k); err != nil {
			return spec, err
		}
		if len(v) > 512 || strings.ContainsAny(v, "\x00\r\n") {
			return spec, errors.New("默认环境变量值无效: " + k)
		}
	}
	if spec.Homepage != "" {
		u, err := url.Parse(spec.Homepage)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return spec, errors.New("官网地址须为有效的 HTTPS URL")
		}
	}
	return spec, nil
}

func validateIsolation(iso *IsolationSpec) error {
	if iso.Mode == "" {
		iso.Mode = "xdg"
	}
	switch iso.Mode {
	case "env_dir":
		if err := ValidateEnvVarName(iso.EnvVar); err != nil {
			return errors.New("env_dir 隔离模式需要有效的配置目录环境变量: " + err.Error())
		}
	case "env_home":
		if err := ValidateEnvVarName(iso.EnvVar); err != nil {
			return errors.New("env_home 隔离模式需要有效的主目录环境变量: " + err.Error())
		}
		if !configSubdirPattern.MatchString(iso.ConfigSubdir) {
			return errors.New("env_home 隔离模式需要有效的配置子目录名（如 .gemini）")
		}
	case "flag_dir":
		if !flagPattern.MatchString(iso.ConfigFlag) {
			return errors.New("flag_dir 隔离模式需要有效的命令行参数名（如 --config-dir）")
		}
	case "xdg", "api_key_only":
		if iso.EnvVar != "" {
			if err := ValidateEnvVarName(iso.EnvVar); err != nil {
				return err
			}
		}
	default:
		return errors.New("不支持的配置隔离模式: " + iso.Mode)
	}
	for k, v := range iso.ExtraEnv {
		if err := ValidateEnvVarName(k); err != nil {
			return err
		}
		if len(v) > 512 || strings.ContainsAny(v, "\x00\r\n") {
			return errors.New("额外隔离环境变量值无效: " + k)
		}
	}
	for _, p := range iso.DefaultConfigPaths {
		if len(p) == 0 || len(p) > 256 || strings.ContainsAny(p, "\x00\r\n") || !strings.HasPrefix(p, "~/") || strings.Contains(p, "..") {
			return errors.New("默认配置路径须以 ~/ 开头且不含 ..")
		}
	}
	for _, res := range iso.SharedResources {
		if len(res) == 0 || len(res) > 128 || strings.HasPrefix(res, "/") || strings.Contains(res, "..") || strings.ContainsAny(res, "\\\x00\r\n") {
			return errors.New("共享资源路径须为安全相对路径: " + res)
		}
		base := res
		if idx := strings.LastIndex(res, "/"); idx >= 0 {
			base = res[idx+1:]
		}
		if forbiddenSharedNames[strings.ToLower(base)] {
			return errors.New("出于安全原因，凭据文件不能设为跨账号共享资源: " + res)
		}
	}
	return nil
}

func validateAuth(auth *AuthSpec) error {
	if len(auth.Methods) == 0 {
		auth.Methods = []string{"api_key"}
	}
	for _, m := range auth.Methods {
		if m != "oauth" && m != "device_code" && m != "api_key" {
			return errors.New("不支持的认证方式: " + m)
		}
	}
	for _, arg := range append(slices.Clone(auth.LoginArgs), auth.DeviceLoginArgs...) {
		if len(arg) == 0 || len(arg) > 128 || strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("登录命令参数无效")
		}
	}
	for _, host := range auth.OAuthDomains {
		if err := ValidateOAuthDomain(host); err != nil {
			return err
		}
	}
	for _, v := range auth.APIKeyEnvVars {
		if err := ValidateEnvVarName(v); err != nil {
			return err
		}
	}
	if auth.BaseURLEnvVar != "" {
		if err := ValidateEnvVarName(auth.BaseURLEnvVar); err != nil {
			return err
		}
	}
	if auth.ModelEnvVar != "" {
		if err := ValidateEnvVarName(auth.ModelEnvVar); err != nil {
			return err
		}
	}
	for _, v := range auth.StripEnvOnOAuth {
		if err := ValidateEnvVarName(v); err != nil {
			return err
		}
	}
	for _, f := range auth.CredentialFiles {
		if len(f) == 0 || len(f) > 128 || strings.HasPrefix(f, "/") || strings.Contains(f, "..") || strings.ContainsAny(f, "\\\x00\r\n") {
			return errors.New("凭据检测路径无效: " + f)
		}
	}
	return nil
}

var builtInCatalog = []AgentSpec{
	{
		ID:                   "claude",
		Name:                 "Claude Code",
		ShortName:            "Claude",
		Vendor:               "Anthropic",
		Category:             "first-party",
		Description:          "Anthropic 官方终端编码 Agent，深度集成多账号额度监控、手机远程会话与跨账号无缝接续。",
		Homepage:             "https://docs.anthropic.com/en/docs/claude-code",
		QuickWebURL:          "https://claude.ai/",
		AccentColor:          "#cd745b",
		Glyph:                "CL",
		BuiltIn:              true,
		Binary:               "claude",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "claude",
		Channels: []InstallChannel{
			{
				ID:            "official-cage",
				Kind:          "builtin",
				Label:         "官方受管渠道（完整性校验）",
				Package:       "@anthropic-ai/claude-code",
				Prerequisites: []string{"node>=18"},
				CommandHint:   "npm install -g @anthropic-ai/claude-code",
				Recommended:   true,
			},
		},
		Isolation: IsolationSpec{
			Mode:               "claude_native",
			EnvVar:             "CLAUDE_CONFIG_DIR",
			DefaultConfigPaths: []string{"~/.claude"},
			SharedResources:    []string{"CLAUDE.md", "AGENTS.md", "commands", "agents", "rules", "agent-memory", "skills"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth"},
			LoginArgs:       []string{"auth", "login", "--claudeai"},
			StatusArgs:      []string{"auth", "status"},
			OAuthDomains:    []string{"claude.ai", "claude.com", "console.anthropic.com", "platform.claude.com", "platform.anthropic.com"},
			APIKeyEnvVars:   []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
			BaseURLEnvVar:   "ANTHROPIC_BASE_URL",
			StripEnvOnOAuth: []string{
				"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN",
				"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_GATEWAY",
			},
			CredentialFiles: []string{".credentials.json"},
			LoginHint:       "通过 Cage 独立授权浏览器登录 claude.ai 订阅账号，支持 5h/7d 实时额度查询。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
			PresetFlags:        []string{"--continue", "--resume"},
		},
	},
	{
		ID:                   "codex",
		Name:                 "OpenAI Codex",
		ShortName:            "Codex",
		Vendor:               "OpenAI",
		Category:             "first-party",
		Description:          "OpenAI 官方编码 Agent CLI，支持 ChatGPT Plus/Pro OAuth 订阅登录、设备码登录与 API Key 隔离调度。",
		Homepage:             "https://github.com/openai/codex",
		QuickWebURL:          "https://chatgpt.com/",
		AccentColor:          "#10a37f",
		Glyph:                "CX",
		BuiltIn:              true,
		Binary:               "codex",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "codex",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@openai/codex）",
				Package:       "@openai/codex",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g @openai/codex@latest",
				Recommended:   true,
			},
		},
		Isolation: IsolationSpec{
			Mode:               "env_dir",
			EnvVar:             "CODEX_HOME",
			DefaultConfigPaths: []string{"~/.codex"},
			SharedResources:    []string{"AGENTS.md", "instructions.md", "skills", "prompts"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "device_code", "api_key"},
			LoginArgs:       []string{"login"},
			DeviceLoginArgs: []string{"login", "--device-auth"},
			StatusArgs:      []string{"login", "status"},
			OAuthDomains:    []string{"auth.openai.com", "chatgpt.com", "platform.openai.com"},
			APIKeyEnvVars:   []string{"OPENAI_API_KEY"},
			BaseURLEnvVar:   "OPENAI_BASE_URL",
			ModelEnvVar:     "OPENAI_MODEL",
			StripEnvOnOAuth: []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY"},
			CredentialFiles: []string{"auth.json"},
			LoginHint:       "支持 ChatGPT 订阅（浏览器 OAuth 或设备码）与 OpenAI API Key 独立多账号调度。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
			AddDirFlag:         "--add-dir",
			PresetFlags:        []string{"--full-auto", "--search", "--sandbox read-only"},
		},
	},
	{
		ID:                   "antigravity",
		Name:                 "Antigravity CLI",
		ShortName:            "Antigravity",
		Vendor:               "Google",
		Category:             "first-party",
		Description:          "Google 官方 Antigravity 终端编码 Agent（agy），支持 Google 账号 OAuth、Gemini Enterprise 与 Gemini API Key 隔离调度。",
		Homepage:             "https://antigravity.google",
		QuickWebURL:          "https://antigravity.google/",
		AccentColor:          "#4285f4",
		Glyph:                "AG",
		BuiltIn:              true,
		Binary:               "agy",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "agy",
		Channels: []InstallChannel{
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（antigravity.google）",
				ScriptURL:     "https://antigravity.google/cli/install.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://antigravity.google/cli/install.sh | bash",
				Recommended:   true,
			},
		},
		Isolation: IsolationSpec{
			Mode:               "xdg",
			DefaultConfigPaths: []string{"~/.gemini/antigravity-cli", "~/.gemini/config"},
			SharedResources:    []string{"AGENTS.md", "skills", "rules"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{},
			OAuthDomains:    []string{"accounts.google.com", "oauth2.googleapis.com", "antigravity.google", "aistudio.google.com"},
			APIKeyEnvVars:   []string{"GEMINI_API_KEY"},
			BaseURLEnvVar:   "GOOGLE_GEMINI_BASE_URL",
			StripEnvOnOAuth: []string{
				"GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH",
				"GOOGLE_CLOUD_PROJECT", "GOOGLE_APPLICATION_CREDENTIALS",
			},
			CredentialFiles: []string{"oauth_creds.json", "google_accounts.json", "jetski_state.pbtxt", "credentials.json"},
			LoginHint:       "首次启动 agy 自动通过 Cage 隔离浏览器完成 Google OAuth 登录；也可配置 modelProvider=gemini 与 GEMINI_API_KEY。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
			AddDirFlag:         "--add-dir",
			PresetFlags:        []string{"--continue", "--mode auto", "--sandbox"},
		},
	},
	{
		ID:                   "opencode",
		Name:                 "OpenCode",
		ShortName:            "OpenCode",
		Vendor:               "Anomaly",
		Category:             "multi-provider",
		Description:          "高性能开源多模型终端编码 Agent，支持 Anthropic、OpenAI、Google、OpenRouter 等多供应商凭据隔离管理。",
		Homepage:             "https://opencode.ai",
		QuickWebURL:          "https://opencode.ai/",
		AccentColor:          "#e11d48",
		Glyph:                "OC",
		BuiltIn:              true,
		Binary:               "opencode",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "opencode",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（opencode-ai）",
				Package:       "opencode-ai",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g opencode-ai@latest",
				Recommended:   true,
			},
			{
				ID:            "npm-scoped",
				Kind:          "npm",
				Label:         "npm Scoped（@opencode/cli）",
				Package:       "@opencode/cli",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g @opencode/cli@latest",
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（opencode.ai）",
				ScriptURL:     "https://opencode.ai/v2/install",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://opencode.ai/v2/install | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode:   "xdg",
			EnvVar: "OPENCODE_CONFIG_DIR",
			ExtraEnv: map[string]string{
				"OPENCODE_CONFIG_DIR": "{config}",
			},
			DefaultConfigPaths: []string{"~/.config/opencode", "~/.local/share/opencode"},
			SharedResources:    []string{"AGENTS.md", "commands", "agents"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{"auth", "login"},
			OAuthDomains:    []string{"opencode.ai", "auth.openai.com", "claude.ai", "github.com", "accounts.google.com", "openrouter.ai"},
			APIKeyEnvVars:   []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
			BaseURLEnvVar:   "OPENAI_BASE_URL",
			StripEnvOnOAuth: []string{},
			CredentialFiles: []string{"opencode/auth.json", "auth.json"},
			LoginHint:       "运行 opencode auth login 授权多个模型供应商，或配置 OpenRouter/OpenAI/Anthropic API Key。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
			PresetFlags:        []string{"--continue"},
		},
	},
	{
		ID:                   "copilot",
		Name:                 "GitHub Copilot CLI",
		ShortName:            "Copilot",
		Vendor:               "GitHub",
		Category:             "first-party",
		Description:          "GitHub 官方 Copilot 终端编码 Agent，支持 GitHub 设备码 OAuth 授权与多账号隔离切换。",
		Homepage:             "https://github.com/features/copilot/cli",
		QuickWebURL:          "https://github.com/login/device",
		AccentColor:          "#8b5cf6",
		Glyph:                "CP",
		BuiltIn:              true,
		Binary:               "copilot",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "copilot",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@github/copilot）",
				Package:       "@github/copilot",
				Prerequisites: []string{"node>=22"},
				CommandHint:   "npm install -g @github/copilot@latest",
				Recommended:   true,
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（gh.io）",
				ScriptURL:     "https://gh.io/copilot-install",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://gh.io/copilot-install | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode:   "env_dir",
			EnvVar: "COPILOT_HOME",
			ExtraEnv: map[string]string{
				"XDG_CONFIG_HOME": "{config}",
			},
			DefaultConfigPaths: []string{"~/.copilot"},
			SharedResources:    []string{"copilot-instructions.md", "AGENTS.md", "skills"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "device_code", "api_key"},
			LoginArgs:       []string{"login"},
			DeviceLoginArgs: []string{"login", "--device-code"},
			OAuthDomains:    []string{"github.com", "auth.githubcopilot.com"},
			APIKeyEnvVars:   []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
			StripEnvOnOAuth: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
			CredentialFiles: []string{"github-copilot/hosts.json", "github-copilot/apps.json", "hosts.json", "apps.json"},
			LoginHint:       "通过 GitHub 设备码在隔离浏览器完成 Copilot 订阅授权，或配置细粒度 GitHub Token。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
			PresetFlags:        []string{"--allow-all-tools"},
		},
	},
	{
		ID:                   "qwen",
		Name:                 "Qwen Code",
		ShortName:            "Qwen",
		Vendor:               "Alibaba Qwen",
		Category:             "first-party",
		Description:          "通义千问官方终端编码 Agent，支持 Qwen OAuth 账号与 DashScope/兼容 OpenAI API 调度。",
		Homepage:             "https://github.com/QwenLM/qwen-code",
		QuickWebURL:          "https://chat.qwen.ai/",
		AccentColor:          "#6366f1",
		Glyph:                "QW",
		BuiltIn:              true,
		Binary:               "qwen",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "qwen",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@qwen-code/qwen-code）",
				Package:       "@qwen-code/qwen-code",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g @qwen-code/qwen-code@latest",
				Recommended:   true,
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方独立安装脚本",
				ScriptURL:     "https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode:               "env_dir",
			EnvVar:             "QWEN_HOME",
			DefaultConfigPaths: []string{"~/.qwen"},
			SharedResources:    []string{"QWEN.md", "AGENTS.md", "commands", "skills"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{},
			OAuthDomains:    []string{"chat.qwen.ai", "qwen.ai", "account.aliyun.com", "dashscope.console.aliyun.com"},
			APIKeyEnvVars:   []string{"OPENAI_API_KEY", "DASHSCOPE_API_KEY", "QWEN_API_KEY"},
			BaseURLEnvVar:   "OPENAI_BASE_URL",
			ModelEnvVar:     "OPENAI_MODEL",
			StripEnvOnOAuth: []string{"OPENAI_API_KEY", "DASHSCOPE_API_KEY", "QWEN_API_KEY", "OPENAI_BASE_URL"},
			CredentialFiles: []string{"oauth_creds.json", "settings.json"},
			LoginHint:       "启动后运行 /auth 使用 Qwen OAuth 登录，或直接绑定 DashScope API Key。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
			PresetFlags:        []string{"--yolo"},
		},
	},
	{
		ID:                   "amp",
		Name:                 "Amp",
		ShortName:            "Amp",
		Vendor:               "Sourcegraph",
		Category:             "first-party",
		Description:          "Sourcegraph 官方 Agentic 编程终端，支持浏览器 OAuth 登录与 AMP_API_KEY 账号池调度。",
		Homepage:             "https://ampcode.com",
		QuickWebURL:          "https://ampcode.com/settings",
		AccentColor:          "#f97316",
		Glyph:                "AM",
		BuiltIn:              true,
		Binary:               "amp",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "amp",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@sourcegraph/amp）",
				Package:       "@sourcegraph/amp",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g @sourcegraph/amp@latest",
				Recommended:   true,
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（ampcode.com）",
				ScriptURL:     "https://ampcode.com/install.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://ampcode.com/install.sh | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode: "xdg",
			ExtraEnv: map[string]string{
				"AMP_SETTINGS_FILE": "{config}/settings.json",
			},
			DefaultConfigPaths: []string{"~/.config/amp", "~/.local/share/amp"},
			SharedResources:    []string{"AGENTS.md", "AGENT.md"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{"login"},
			OAuthDomains:    []string{"ampcode.com", "sourcegraph.com"},
			APIKeyEnvVars:   []string{"AMP_API_KEY"},
			StripEnvOnOAuth: []string{"AMP_API_KEY"},
			CredentialFiles: []string{"amp/secrets.json", "secrets.json"},
			LoginHint:       "使用 amp login 浏览器授权，或从 ampcode.com/settings 获取 AMP_API_KEY。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "cline",
		Name:                 "Cline CLI",
		ShortName:            "Cline",
		Vendor:               "Cline",
		Category:             "multi-provider",
		Description:          "Cline 官方终端 Agent，支持 Cline 账号 OAuth 及多模型供应商 API Key 独立隔离。",
		Homepage:             "https://cline.bot",
		QuickWebURL:          "https://app.cline.bot/",
		AccentColor:          "#0ea5e9",
		Glyph:                "CN",
		BuiltIn:              true,
		Binary:               "cline",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "cline",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（cline）",
				Package:       "cline",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g cline@latest",
				Recommended:   true,
			},
		},
		Isolation: IsolationSpec{
			Mode:               "env_dir",
			EnvVar:             "CLINE_DIR",
			DefaultConfigPaths: []string{"~/.cline"},
			SharedResources:    []string{"rules", "AGENTS.md"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{"auth"},
			OAuthDomains:    []string{"app.cline.bot", "cline.bot", "auth.cline.bot"},
			APIKeyEnvVars:   []string{"CLINE_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"},
			BaseURLEnvVar:   "OPENAI_BASE_URL",
			StripEnvOnOAuth: []string{"CLINE_API_KEY"},
			CredentialFiles: []string{"data/secrets.json", "secrets.json"},
			LoginHint:       "运行 cline auth 启动官方登录向导，或配置自有 API Key。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "cursor",
		Name:                 "Cursor Agent",
		ShortName:            "Cursor",
		Vendor:               "Cursor",
		Category:             "first-party",
		Description:          "Cursor 官方命令行 Agent，支持 Cursor 账号浏览器登录与 CURSOR_API_KEY 密钥调度。",
		Homepage:             "https://cursor.com/cli",
		QuickWebURL:          "https://cursor.com/dashboard",
		AccentColor:          "#14b8a6",
		Glyph:                "CR",
		BuiltIn:              true,
		Binary:               "cursor-agent",
		FallbackBinaries:     []string{"agent"},
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "cursor-agent",
		Channels: []InstallChannel{
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（cursor.com）",
				ScriptURL:     "https://cursor.com/install",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl https://cursor.com/install -fsS | bash",
				Recommended:   true,
			},
		},
		Isolation: IsolationSpec{
			Mode:   "env_dir",
			EnvVar: "CURSOR_CONFIG_DIR",
			ExtraEnv: map[string]string{
				"XDG_CONFIG_HOME": "{config}",
			},
			DefaultConfigPaths: []string{"~/.cursor"},
			SharedResources:    []string{"rules", "AGENTS.md"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{"login"},
			StatusArgs:      []string{"status"},
			OAuthDomains:    []string{"cursor.com", "authenticator.cursor.sh"},
			APIKeyEnvVars:   []string{"CURSOR_API_KEY"},
			StripEnvOnOAuth: []string{"CURSOR_API_KEY"},
			CredentialFiles: []string{"auth.json"},
			LoginHint:       "使用 cursor-agent login 浏览器授权，或绑定 Cursor Dashboard 生成的 CURSOR_API_KEY。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "vibe",
		Name:                 "Mistral Vibe",
		ShortName:            "Vibe",
		Vendor:               "Mistral AI",
		Category:             "first-party",
		Description:          "Mistral AI 官方命令行编码 Agent，通过 VIBE_HOME 实现完整配置与多账号隔离。",
		Homepage:             "https://github.com/mistralai/mistral-vibe",
		QuickWebURL:          "https://console.mistral.ai/",
		AccentColor:          "#f59e0b",
		Glyph:                "VB",
		BuiltIn:              true,
		Binary:               "vibe",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "vibe",
		Channels: []InstallChannel{
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（mistral.ai）",
				ScriptURL:     "https://mistral.ai/vibe/install.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -LsSf https://mistral.ai/vibe/install.sh | bash",
				Recommended:   true,
			},
			{
				ID:            "uv-official",
				Kind:          "uv",
				Label:         "uv tool（mistral-vibe）",
				Package:       "mistral-vibe",
				Prerequisites: []string{"python3"},
				CommandHint:   "uv tool install mistral-vibe",
			},
		},
		Isolation: IsolationSpec{
			Mode:               "env_dir",
			EnvVar:             "VIBE_HOME",
			DefaultConfigPaths: []string{"~/.vibe"},
			SharedResources:    []string{"prompts", "skills", "AGENTS.md"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{},
			OAuthDomains:    []string{"console.mistral.ai", "mistral.ai"},
			APIKeyEnvVars:   []string{"MISTRAL_API_KEY"},
			ModelEnvVar:     "VIBE_ACTIVE_MODEL",
			StripEnvOnOAuth: []string{"MISTRAL_API_KEY"},
			CredentialFiles: []string{".env", "auth.json"},
			LoginHint:       "首次运行通过向导登录 Mistral 账号，或配置 MISTRAL_API_KEY 进行多账号轮转。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: false,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "kimi",
		Name:                 "Kimi Code",
		ShortName:            "Kimi",
		Vendor:               "Moonshot AI",
		Category:             "first-party",
		Description:          "月之暗面官方 Kimi Code 终端 Agent，支持 Kimi OAuth 订阅登录与 Moonshot API Key 多账号调度。",
		Homepage:             "https://code.kimi.com",
		QuickWebURL:          "https://code.kimi.com/",
		AccentColor:          "#3b82f6",
		Glyph:                "KM",
		BuiltIn:              true,
		Binary:               "kimi",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "kimi",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@moonshot-ai/kimi-code）",
				Package:       "@moonshot-ai/kimi-code",
				Prerequisites: []string{"node>=22"},
				CommandHint:   "npm install -g @moonshot-ai/kimi-code@latest",
				Recommended:   true,
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（code.kimi.com）",
				ScriptURL:     "https://code.kimi.com/kimi-code/install.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode:   "env_dir",
			EnvVar: "KIMI_HOME",
			ExtraEnv: map[string]string{
				"KIMI_SHARE_DIR":  "{data}",
				"KIMI_CODE_HOME":  "{config}",
				"XDG_CONFIG_HOME": "{config}",
			},
			DefaultConfigPaths: []string{"~/.kimi", "~/.kimi-code"},
			SharedResources:    []string{"AGENTS.md"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{},
			OAuthDomains:    []string{"code.kimi.com", "kimi.com", "platform.moonshot.cn", "platform.moonshot.ai"},
			APIKeyEnvVars:   []string{"KIMI_API_KEY", "MOONSHOT_API_KEY"},
			BaseURLEnvVar:   "OPENAI_BASE_URL",
			StripEnvOnOAuth: []string{"KIMI_API_KEY", "MOONSHOT_API_KEY"},
			CredentialFiles: []string{"credentials.json", "auth.json"},
			LoginHint:       "启动后输入 /login 进行 Kimi Code OAuth 授权，或添加 Moonshot 开放平台 API Key。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "aider",
		Name:                 "Aider",
		ShortName:            "Aider",
		Vendor:               "Aider",
		Category:             "open-source",
		Description:          "经典开源 Git 结对编程终端 Agent，支持任意 OpenAI/Anthropic/Gemini/DeepSeek/OpenRouter API Key 池调度。",
		Homepage:             "https://aider.chat",
		QuickWebURL:          "https://aider.chat/docs/",
		AccentColor:          "#22c55e",
		Glyph:                "AD",
		BuiltIn:              true,
		Binary:               "aider",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "aider",
		Channels: []InstallChannel{
			{
				ID:            "uv-official",
				Kind:          "uv",
				Label:         "uv tool（aider-chat）",
				Package:       "aider-chat",
				Prerequisites: []string{"python3"},
				CommandHint:   "uv tool install aider-chat",
				Recommended:   true,
			},
			{
				ID:            "pip-official",
				Kind:          "pip",
				Label:         "pip（aider-chat）",
				Package:       "aider-chat",
				Prerequisites: []string{"python3"},
				CommandHint:   "python3 -m pip install --user aider-chat",
			},
		},
		Isolation: IsolationSpec{
			Mode:               "xdg",
			DefaultConfigPaths: []string{"~/.config/aider"},
			SharedResources:    []string{"CONVENTIONS.md", "AGENTS.md"},
		},
		Auth: AuthSpec{
			Methods:       []string{"api_key"},
			APIKeyEnvVars: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "DEEPSEEK_API_KEY", "OPENROUTER_API_KEY"},
			BaseURLEnvVar: "OPENAI_API_BASE",
			ModelEnvVar:   "AIDER_MODEL",
			LoginHint:     "配置任意模型供应商 API Key 及可选自定义网关地址，即刻获得独立多账号智能调度。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: false,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      false,
			PresetFlags:        []string{"--watch-files", "--auto-commits", "--architect"},
		},
	},
	{
		ID:                   "droid",
		Name:                 "Factory Droid",
		ShortName:            "Droid",
		Vendor:               "Factory",
		Category:             "first-party",
		Description:          "Factory 官方终端编码 Agent，支持浏览器 OAuth 登录与 FACTORY_API_KEY 多账号隔离调度。",
		Homepage:             "https://factory.ai",
		QuickWebURL:          "https://app.factory.ai/",
		AccentColor:          "#f43f5e",
		Glyph:                "DR",
		BuiltIn:              true,
		Binary:               "droid",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "droid",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@factory/cli）",
				Package:       "@factory/cli",
				Prerequisites: []string{"node>=20"},
				CommandHint:   "npm install -g @factory/cli@latest",
				Recommended:   true,
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（factory.ai）",
				ScriptURL:     "https://app.factory.ai/cli",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://app.factory.ai/cli | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode:   "env_dir",
			EnvVar: "FACTORY_HOME",
			ExtraEnv: map[string]string{
				"XDG_CONFIG_HOME": "{config}",
			},
			DefaultConfigPaths: []string{"~/.factory"},
			SharedResources:    []string{"AGENTS.md", "skills"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			LoginArgs:       []string{"login"},
			OAuthDomains:    []string{"app.factory.ai", "auth.factory.ai", "factory.ai"},
			APIKeyEnvVars:   []string{"FACTORY_API_KEY"},
			StripEnvOnOAuth: []string{"FACTORY_API_KEY"},
			CredentialFiles: []string{"auth.json", "credentials.json"},
			LoginHint:       "使用 droid login 浏览器授权，或绑定 Factory 平台生成的 FACTORY_API_KEY。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "goose",
		Name:                 "Goose",
		ShortName:            "Goose",
		Vendor:               "Block",
		Category:             "open-source",
		Description:          "Block 开源终端编码 Agent，支持多模型供应商 API Key 池调度与 MCP 工具链扩展。",
		Homepage:             "https://block.github.io/goose",
		QuickWebURL:          "https://block.github.io/goose/",
		AccentColor:          "#06b6d4",
		Glyph:                "GS",
		BuiltIn:              true,
		Binary:               "goose",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "goose",
		Channels: []InstallChannel{
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（GitHub）",
				ScriptURL:     "https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh | bash",
				Recommended:   true,
			},
		},
		Isolation: IsolationSpec{
			Mode:               "xdg",
			DefaultConfigPaths: []string{"~/.config/goose", "~/.local/share/goose"},
			SharedResources:    []string{".goosehints", "AGENTS.md"},
		},
		Auth: AuthSpec{
			Methods:       []string{"api_key"},
			APIKeyEnvVars: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY"},
			BaseURLEnvVar: "OPENAI_HOST",
			ModelEnvVar:   "GOOSE_MODEL",
			LoginHint:     "配置 OpenAI/Anthropic/Google/OpenRouter API Key 即可启动隔离会话与多账号调度。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: false,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
	{
		ID:                   "pi",
		Name:                 "Pi Agent",
		ShortName:            "Pi",
		Vendor:               "Pi",
		Category:             "multi-provider",
		Description:          "轻量级多供应商终端编码 Agent，支持多模型 OAuth 与 API Key 独立目录隔离调度。",
		Homepage:             "https://pi.dev",
		QuickWebURL:          "https://pi.dev/",
		AccentColor:          "#a855f7",
		Glyph:                "PI",
		BuiltIn:              true,
		Binary:               "pi",
		VersionArgs:          []string{"--version"},
		DefaultLaunchCommand: "pi",
		Channels: []InstallChannel{
			{
				ID:            "npm-official",
				Kind:          "npm",
				Label:         "npm 官方源（@earendil-works/pi-coding-agent）",
				Package:       "@earendil-works/pi-coding-agent",
				Prerequisites: []string{"node>=22"},
				CommandHint:   "npm install -g @earendil-works/pi-coding-agent@latest",
				Recommended:   true,
			},
			{
				ID:            "official-script",
				Kind:          "script",
				Label:         "官方安装脚本（pi.dev）",
				ScriptURL:     "https://pi.dev/install.sh",
				Prerequisites: []string{"curl"},
				CommandHint:   "curl -fsSL https://pi.dev/install.sh | bash",
			},
		},
		Isolation: IsolationSpec{
			Mode:               "env_dir",
			EnvVar:             "PI_CODING_AGENT_DIR",
			DefaultConfigPaths: []string{"~/.pi/agent"},
			SharedResources:    []string{"AGENTS.md", "skills"},
		},
		Auth: AuthSpec{
			Methods:         []string{"oauth", "api_key"},
			OAuthDomains:    []string{"pi.dev", "auth.openai.com", "claude.ai", "accounts.google.com", "github.com"},
			APIKeyEnvVars:   []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"},
			CredentialFiles: []string{"auth.json", "credentials.json"},
			LoginHint:       "支持 OAuth 订阅授权与多供应商 API Key 独立多账号调度。",
		},
		Terminal: TerminalSpec{
			ImagePasteShortcut: true,
			ShiftEnterMode:     "bracketed_newline",
			MouseCopyHint:      true,
		},
	},
}
