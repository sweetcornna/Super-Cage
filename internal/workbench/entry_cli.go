package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// entryCall makes one request to the environment's controller with the
// launcher record, as cage agent does, and decodes a JSON answer.
func entryCall(ctx context.Context, configPath, method, path string, input, output any) error {
	c, err := agentConnect(ctx, configPath)
	if err != nil {
		return err
	}
	defer c.close()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(data))
	}
	request, err := http.NewRequestWithContext(ctx, method, c.origin+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Origin", c.origin)
	request.Header.Set(connectionHeader, "local")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	c.client.Timeout = time.Minute
	response, err := c.client.Do(request)
	if err != nil {
		return &entryRefusal{0, "connection", "与 Super Cage 的连接中断：请查看状态后再重试"}
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		refusal := entryHTTPRefusal(response.StatusCode, data)
		if refusal.Code == "failed" && len(data) > 0 && len(data) < 512 {
			refusal.Message = strings.TrimSpace(string(data))
		}
		return refusal
	}
	if output != nil {
		return json.Unmarshal(data, output)
	}
	return nil
}

const entryUsage = `用法：
  cage entry on [环境]             在这台电脑上开启这个环境的本机终端
  cage entry off [环境]            关闭，并立即断开所有本机连接
  cage entry status [环境] [--json]
  cage entry devices [环境]
  cage entry revoke <设备> [环境]   撤销一台设备，立即断开它的连接
  cage entry rename <设备> <名称> [环境]
  cage entry include [环境]         确认后在 ~/.ssh/config 顶部加入一行 Include
  cage entry proxy <环境>           ssh 的 ProxyCommand，不用手动运行
  cage ssh <环境> [ssh 参数…]       不改 ~/.ssh/config 也能连接
环境：default（默认环境）或本机虚拟机的标识。
带终端的会话（ssh、ssh -t）是一个 Super Cage 终端，环境的输出经过和窗口里一样的过滤。
不带终端的命令（ssh 主机 命令、ssh -T）、scp、sftp 和 rsync 原样传送环境的字节，不经过滤：
环境里的程序可以借这些输出向你的终端发送控制字符。
`

// EntryMain runs cage entry …; defaultConfig is the default profile's
// configuration path.
func EntryMain(args []string, defaultConfig string, stdin *os.File, stdout, stderr io.Writer) int {
	ctx := context.Background()
	if len(args) == 0 {
		fmt.Fprint(stderr, entryUsage)
		return 2
	}
	if args[0] == "proxy" {
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Super Cage：用法 cage entry proxy <环境>")
			return entryProxyFailure
		}
		return EntryProxy(ctx, defaultConfig, args[1], stdin, stdout, stderr)
	}
	jsonOutput := false
	var rest []string
	for _, arg := range args[1:] {
		if arg == "--json" {
			jsonOutput = true
		} else {
			rest = append(rest, arg)
		}
	}
	environment := defaultLocalVMID
	positional := map[string]int{"on": 0, "off": 0, "status": 0, "devices": 0, "include": 0, "revoke": 1, "rename": 2}
	need, known := positional[args[0]]
	if !known {
		fmt.Fprint(stderr, entryUsage)
		return 2
	}
	if len(rest) < need || len(rest) > need+1 {
		fmt.Fprint(stderr, entryUsage)
		return 2
	}
	if len(rest) == need+1 {
		environment = rest[need]
	}
	configPath, err := entryProfileConfig(defaultConfig, environment)
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+err.Error())
		return 1
	}
	var view entryStatusView
	switch args[0] {
	case "on":
		err = entryCall(ctx, configPath, "POST", "/api/entry/on", map[string]any{}, &view)
	case "off":
		err = entryCall(ctx, configPath, "POST", "/api/entry/off", map[string]any{}, &view)
	case "status", "devices":
		err = entryCall(ctx, configPath, "GET", "/api/entry", nil, &view)
	case "revoke":
		err = entryCall(ctx, configPath, "POST", "/api/entry/devices/"+url.PathEscape(rest[0])+"/revoke", map[string]any{}, &view)
	case "rename":
		err = entryCall(ctx, configPath, "POST", "/api/entry/devices/"+url.PathEscape(rest[0])+"/rename", map[string]string{"name": rest[1]}, &view)
	case "include":
		return entryIncludeCommand(ctx, configPath, environment, stdin, stdout, stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+entryClientError(err))
		return 1
	}
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(view)
		return 0
	}
	if args[0] == "devices" {
		entryPrintDevices(stdout, view)
		return 0
	}
	entryPrintStatus(stdout, view)
	if args[0] == "on" && view.Include != nil && !view.Include.Present {
		return entryIncludeCommand(ctx, configPath, environment, stdin, stdout, stderr)
	}
	return 0
}

var entryStateLines = map[string]string{
	"off":         "未开启",
	"idle":        "已开启 · 没有连接",
	"in_use":      "已开启 · 正在使用",
	"paused":      "网络已暂停 · ssh 已断开，恢复网络后可重新连接",
	"stopped":     "环境已停止 · 启动环境后可连接",
	"starting":    "环境正在启动 · 稍后可连接",
	"unsupported": "这个环境暂不支持本机终端",
}

func entryPrintStatus(out io.Writer, view entryStatusView) {
	line := entryStateLines[view.State]
	if view.State == "in_use" {
		line = fmt.Sprintf("已开启 · %d 个连接（终端 %d，命令 %d，文件 %d）", view.Connections.Total, view.Connections.Shell, view.Connections.Exec, view.Connections.SFTP)
	}
	fmt.Fprintf(out, "本机终端（%s）：%s\n", view.Name, line)
	if view.Revoked {
		fmt.Fprintln(out, "这台电脑的钥匙已撤销。重新开启即可生成新钥匙。")
	}
	if view.Commands != nil {
		fmt.Fprintf(out, "  任何终端：%s\n", view.Commands.SSH)
		fmt.Fprintf(out, "  不改 ~/.ssh/config：%s\n", view.Commands.CageSSH)
	}
}

func entryPrintDevices(out io.Writer, view entryStatusView) {
	if len(view.Devices) == 0 {
		fmt.Fprintln(out, "还没有设备")
		return
	}
	for _, device := range view.Devices {
		state := ""
		if device.Current {
			state += " · 这台电脑"
		}
		if device.Revoked {
			state += " · 已撤销"
		}
		used := "从未使用"
		if device.LastUsed != nil {
			used = "最近使用 " + device.LastUsed.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(out, "%s  %s  %s  %s%s\n", device.ID[:8], device.Name, device.Fingerprint, used, state)
	}
}

// entryIncludeCommand shows the exact line and asks before it is written.
// Without a terminal to ask on it writes nothing.
func entryIncludeCommand(ctx context.Context, configPath, environment string, stdin *os.File, stdout, stderr io.Writer) int {
	var preview entryInclude
	if err := entryCall(ctx, configPath, "GET", "/api/entry/include", nil, &preview); err != nil {
		fmt.Fprintln(stderr, "Super Cage："+entryClientError(err))
		return 1
	}
	if preview.Error != "" {
		fmt.Fprintln(stderr, "Super Cage："+preview.Error)
		return 1
	}
	if preview.Present {
		fmt.Fprintln(stdout, preview.DisplayPath+" 已经包含 Super Cage 的配置。")
		return 0
	}
	hint := "未修改 " + preview.DisplayPath + "。不加这一行也能用：cage ssh " + environment
	if stdin == nil || !term.IsTerminal(int(stdin.Fd())) {
		fmt.Fprintln(stdout, hint)
		return 0
	}
	fmt.Fprint(stdout, entryIncludePrompt(preview))
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	if strings.TrimSpace(strings.ToLower(answer)) != "y" {
		fmt.Fprintln(stdout, hint)
		return 0
	}
	var applied entryInclude
	if err := entryCall(ctx, configPath, "POST", "/api/entry/include", map[string]string{"token": preview.Token}, &applied); err != nil {
		fmt.Fprintln(stderr, "Super Cage："+entryClientError(err))
		return 1
	}
	fmt.Fprintf(stdout, "已写入。原文件备份在 %s\n", applied.Backup)
	return 0
}

// entryIncludePrompt is the confirmation cage entry include asks: the line,
// the host name it brings in and the whole block behind it (F6).
func entryIncludePrompt(preview entryInclude) string {
	var b strings.Builder
	fmt.Fprintf(&b, "让 ssh 认识这个环境：将在 %s 顶部加入这一行\n\n  %s\n\n", preview.DisplayPath, preview.Line)
	if preview.Alias != "" {
		fmt.Fprintf(&b, "之后用 ssh %s 连接。这一行引入的配置：\n\n", preview.Alias)
		for _, line := range strings.Split(strings.TrimRight(preview.Block, "\n"), "\n") {
			b.WriteString("    " + line + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("写入吗？[y/N] ")
	return b.String()
}

// SSHCommand is the argument list of cage ssh <environment> [args…]:
// ssh with Super Cage's own configuration file, for owners who did not add
// the Include line.
func SSHCommand(defaultConfig, environment string, args []string) ([]string, error) {
	configPath, err := entryProfileConfig(defaultConfig, environment)
	if err != nil {
		return nil, err
	}
	executable, _ := os.Executable()
	paths, err := defaultEntryPaths(filepath.Dir(configPath), executable)
	if err != nil {
		return nil, err
	}
	return entrySSHCommand(paths, environment, args)
}

func entrySSHCommand(paths entryPaths, environment string, args []string) ([]string, error) {
	alias := entryReadAlias(paths.hostFile(environment))
	if alias == "" {
		return nil, errEntryOff
	}
	return append([]string{"ssh", "-F", paths.clientFile(), alias}, args...), nil
}
