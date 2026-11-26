package localorca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

const DownloadURL = "https://orca.so"

// Installation describes the host-local Orca Desktop installation.
type Installation struct {
	Installed bool   `json:"installed"`
	AppPath   string `json:"app_path,omitempty"`
	CLIPath   string `json:"cli_path,omitempty"`
	Version   string `json:"version,omitempty"`
}

type EnvironmentEndpoint struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Endpoint string `json:"endpoint"`
}

type Environment struct {
	ID                  string                `json:"id"`
	Name                string                `json:"name"`
	PairedDeviceID      string                `json:"pairedDeviceId,omitempty"`
	RuntimeID           string                `json:"runtimeId,omitempty"`
	PreferredEndpointID string                `json:"preferredEndpointId,omitempty"`
	Endpoints           []EnvironmentEndpoint `json:"endpoints,omitempty"`
}

var (
	validEnvNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ ()-]{0,63}$`)
	validSelectorRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ ()-]{0,127}$`)
	validVersionRe  = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
)

// Detect locates the host-local Orca Desktop app and its official CLI without
// spawning subprocesses or triggering OS automation prompts.
func Detect() Installation {
	return detectLocal()
}

func readBuildVersion(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return ""
	}
	var meta struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	v := strings.TrimPrefix(strings.TrimSpace(meta.Version), "v")
	if !validVersionRe.MatchString(v) {
		return ""
	}
	return v
}

// Client wraps the host-local Orca CLI (`orca environment ...`, `orca open`)
// using strict argv execution (never a shell) with an injectable runner for tests.
type Client struct {
	Install Installation
	Run     func(ctx context.Context, binary string, args ...string) ([]byte, error)
}

func NewClient(install Installation) *Client {
	return &Client{Install: install}
}

func (c *Client) execCLI(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	if c == nil || c.Install.CLIPath == "" {
		return nil, errors.New("本机未检测到 Orca 桌面客户端 CLI")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if c.Run != nil {
		return c.Run(runCtx, c.Install.CLIPath, args...)
	}
	cmd := exec.CommandContext(runCtx, c.Install.CLIPath, args...)
	host.HideCommandWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if len(msg) > 240 {
			msg = msg[:240]
		}
		if msg != "" {
			return stdout.Bytes(), fmt.Errorf("orca CLI 执行失败: %s", msg)
		}
		return stdout.Bytes(), fmt.Errorf("orca CLI 执行失败: %w", err)
	}
	return stdout.Bytes(), nil
}

func parseEnvelope(raw []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("orca CLI 返回空输出")
	}
	var env struct {
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &env); err == nil {
		if env.OK != nil && !*env.OK {
			return nil, fmt.Errorf("orca CLI 操作失败: %v", env.Error)
		}
		if len(env.Result) > 0 {
			return env.Result, nil
		}
	}
	return trimmed, nil
}

func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	raw, err := c.execCLI(ctx, 8*time.Second, "environment", "list", "--json")
	if err != nil {
		return nil, err
	}
	result, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Environments []Environment `json:"environments"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("无法解析本机 Orca 环境列表: %w", err)
	}
	if payload.Environments == nil {
		return []Environment{}, nil
	}
	return payload.Environments, nil
}

func normalizeEndpoint(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	hostName := strings.ToLower(u.Hostname())
	if hostName == "localhost" {
		hostName = "127.0.0.1"
	}
	port := u.Port()
	if port != "" {
		return strings.ToLower(u.Scheme) + "://" + hostName + ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + hostName
}

func (c *Client) FindEnvironmentByEndpoint(ctx context.Context, endpoint string) (*Environment, error) {
	want := normalizeEndpoint(endpoint)
	envs, err := c.ListEnvironments(ctx)
	if err != nil {
		return nil, err
	}
	for i := range envs {
		for _, ep := range envs[i].Endpoints {
			if normalizeEndpoint(ep.Endpoint) == want {
				cp := envs[i]
				return &cp, nil
			}
		}
	}
	return nil, nil
}

// UsedLoopbackPorts returns every loopback port already registered by any saved
// remote environment in host Orca Desktop so Super Cage never collides with them.
func (c *Client) UsedLoopbackPorts(ctx context.Context) map[int]bool {
	used := map[int]bool{}
	envs, err := c.ListEnvironments(ctx)
	if err != nil {
		return used
	}
	for _, env := range envs {
		for _, ep := range env.Endpoints {
			u, err := url.Parse(strings.TrimSpace(ep.Endpoint))
			if err != nil {
				continue
			}
			h := strings.ToLower(u.Hostname())
			if h != "127.0.0.1" && h != "localhost" && h != "::1" {
				continue
			}
			if p, err := strconv.Atoi(u.Port()); err == nil && p >= 1024 && p <= 65535 {
				used[p] = true
			}
		}
	}
	return used
}

func ValidateEnvironmentName(name string) (string, error) {
	clean := strings.TrimSpace(name)
	if !validEnvNameRe.MatchString(clean) {
		return "", errors.New("Orca 服务器名称须以字母或数字开头，长度 1-64 个字符（支持字母、数字、空格、点、横线和括号）")
	}
	return clean, nil
}

func ValidatePairingCode(code string) (string, error) {
	clean := strings.TrimSpace(code)
	if !strings.HasPrefix(clean, "orca://pair?") || len(clean) > 4096 || strings.ContainsAny(clean, "\x00\r\n\t ") {
		return "", errors.New("Orca 配对链接格式无效")
	}
	return clean, nil
}

func (c *Client) AddEnvironment(ctx context.Context, name, pairingCode string) (Environment, error) {
	cleanName, err := ValidateEnvironmentName(name)
	if err != nil {
		return Environment{}, err
	}
	cleanCode, err := ValidatePairingCode(pairingCode)
	if err != nil {
		return Environment{}, err
	}
	raw, err := c.execCLI(ctx, 20*time.Second, "environment", "add", "--name", cleanName, "--pairing-code", cleanCode, "--json")
	if err != nil {
		return Environment{}, err
	}
	result, err := parseEnvelope(raw)
	if err != nil {
		return Environment{}, err
	}
	var wrapped struct {
		Environment *Environment `json:"environment"`
	}
	if json.Unmarshal(result, &wrapped) == nil && wrapped.Environment != nil && wrapped.Environment.ID != "" {
		return *wrapped.Environment, nil
	}
	var direct Environment
	if json.Unmarshal(result, &direct) == nil && direct.ID != "" {
		return direct, nil
	}
	return Environment{Name: cleanName}, nil
}

func (c *Client) RemoveEnvironment(ctx context.Context, selector string) error {
	clean := strings.TrimSpace(selector)
	if !validSelectorRe.MatchString(clean) {
		return errors.New("无效的 Orca 环境标识")
	}
	_, err := c.execCLI(ctx, 10*time.Second, "environment", "rm", "--environment", clean, "--json")
	return err
}

func (c *Client) OpenDesktop(ctx context.Context) error {
	if c != nil && c.Install.CLIPath != "" {
		if _, err := c.execCLI(ctx, 15*time.Second, "open", "--json"); err == nil {
			return nil
		}
	}
	if runtime.GOOS == "darwin" && c != nil && c.Install.AppPath != "" && c.Run == nil {
		cmd := exec.CommandContext(ctx, "/usr/bin/open", "-a", c.Install.AppPath)
		return cmd.Run()
	}
	return errors.New("无法打开本机 Orca 客户端，请确认 Orca 已安装")
}
