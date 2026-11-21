package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// cage entry proxy <environment> is the ProxyCommand of a generated Host
// block. It reaches the environment's local controller the way cage agent
// does (the launcher record readable only by this user), opens the entry's
// WebSocket and relays ssh's bytes. Any refusal is printed in Chinese on
// stderr and it exits 255, as ssh does for a connection that failed.

const entryProxyFailure = 255

// entryClientError turns what the controller connection or the dial said
// into the owner's words.
func entryClientError(err error) string {
	var refusal *entryRefusal
	if errors.As(err, &refusal) {
		return refusal.Message
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) {
		switch agentErr.Code {
		case "service_unavailable", "service_start_failed":
			return "Super Cage 没有在运行：打开 Super Cage 后再连接"
		case "controller_mismatch", "config_path_mismatch":
			return "需要更新 Super Cage：这个 cage 命令和正在运行的 Super Cage 版本不同"
		case "authentication_failed":
			return "无法向 Super Cage 认证：重新打开 Super Cage 后再试"
		case "account_locked":
			return errEntryLocked.Message
		}
	}
	return "无法连接 Super Cage"
}

// entryHTTPRefusal reads a refusal answered by the controller.
func entryHTTPRefusal(status int, body []byte) *entryRefusal {
	var refusal entryRefusal
	if json.Unmarshal(body, &refusal) == nil && refusal.Message != "" {
		refusal.Status = status
		return &refusal
	}
	switch status {
	case http.StatusLocked:
		return errEntryLocked
	case http.StatusNotFound:
		return &entryRefusal{status, "outdated", "需要更新 Super Cage：正在运行的版本还没有本机终端"}
	case http.StatusUnauthorized:
		return &entryRefusal{status, "authentication", "无法向 Super Cage 认证：重新打开 Super Cage 后再试"}
	case http.StatusForbidden:
		return errEntryLocalOnly
	case http.StatusServiceUnavailable:
		return errEntryClosing
	}
	return &entryRefusal{status, "failed", fmt.Sprintf("无法连接本机终端（HTTP %d）", status)}
}

// EntryProxy runs cage entry proxy for an environment of the default profile
// whose configuration path is defaultConfig.
func EntryProxy(ctx context.Context, defaultConfig, environment string, stdin io.Reader, stdout, stderr io.Writer) int {
	configPath, err := entryProfileConfig(defaultConfig, environment)
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+err.Error())
		return entryProxyFailure
	}
	executable, _ := os.Executable()
	paths, err := defaultEntryPaths(filepath.Dir(configPath), executable)
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+err.Error())
		return entryProxyFailure
	}
	return entryProxy(ctx, configPath, environment, paths, stdin, stdout, stderr)
}

func entryProxy(ctx context.Context, configPath, environment string, paths entryPaths, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(message string) int {
		fmt.Fprintln(stderr, "Super Cage："+message)
		return entryProxyFailure
	}
	computer, ok, err := entryReadComputer(paths)
	if err != nil || !ok {
		return fail("这台电脑还没有开启本机终端：在 Super Cage 里开启")
	}
	ws, hostKey, err := entryDial(ctx, configPath, computer.ID)
	if err != nil {
		return fail(entryClientError(err))
	}
	defer ws.Close()
	// Local: the controller runs on this computer, so the key it presents
	// is pinned before ssh reads known_hosts, and a re-created environment
	// never shows "REMOTE HOST IDENTIFICATION HAS CHANGED".
	if hostKey != nil {
		if err := entryPinHostKey(paths, environment, hostKey); err != nil {
			return fail("无法更新这个环境的主机钥匙：" + err.Error())
		}
	}
	entryRelay(ctx, ws, stdin, stdout)
	return 0
}

// entryDial opens the entry's WebSocket on the environment's controller.
func entryDial(ctx context.Context, configPath, device string) (*websocket.Conn, ssh.PublicKey, error) {
	c, err := agentConnect(ctx, configPath)
	if err != nil {
		return nil, nil, err
	}
	defer c.close()
	origin, _ := url.Parse(c.origin)
	headers := http.Header{"Origin": {c.origin}, connectionHeader: {"local"}}
	for _, cookie := range c.client.Jar.Cookies(origin) {
		headers.Add("Cookie", cookie.String())
	}
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 15 * time.Second, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}
	target := "ws" + strings.TrimPrefix(c.origin, "http") + "/api/entry/ssh?device=" + url.QueryEscape(device)
	ws, response, err := dialer.DialContext(ctx, target, headers)
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
			return nil, nil, entryHTTPRefusal(response.StatusCode, body)
		}
		return nil, nil, &entryRefusal{0, "connection", "无法连接 Super Cage 的本机终端"}
	}
	var hostKey ssh.PublicKey
	if line := response.Header.Get("Cage-Entry-Host-Key"); line != "" {
		hostKey, _, _, _, err = ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			ws.Close()
			return nil, nil, &entryRefusal{0, "host_key", "Super Cage 返回了无效的主机钥匙"}
		}
	}
	return ws, hostKey, nil
}

// entryRelay copies ssh's stdin into binary messages and the messages to
// ssh's stdout until either side ends.
func entryRelay(ctx context.Context, ws *websocket.Conn, stdin io.Reader, stdout io.Writer) {
	ws.SetReadLimit(1 << 20)
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		buffer := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buffer)
			if n > 0 && ws.WriteMessage(websocket.BinaryMessage, buffer[:n]) != nil {
				break
			}
			if err != nil {
				break
			}
		}
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	}()
	writer := bufio.NewWriterSize(stdout, 32<<10)
	for {
		kind, reader, err := ws.NextReader()
		if err != nil {
			break
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		if _, err := io.Copy(writer, reader); err != nil {
			break
		}
		if writer.Flush() != nil {
			break
		}
	}
	ws.Close()
	if closer, ok := stdin.(io.Closer); ok && closer != os.Stdin {
		_ = closer.Close()
	}
}

// --- cage entry and cage ssh ---

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
	fmt.Fprintf(stdout, "让 ssh 认识这个环境：将在 %s 顶部加入这一行\n\n  %s\n\n写入吗？[y/N] ", preview.DisplayPath, preview.Line)
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
