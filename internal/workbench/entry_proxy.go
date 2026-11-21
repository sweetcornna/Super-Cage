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
)

// The proxy mode below is to become the second mode of the slim program
// (cmd/cage-attach on branch local-terminal-attach), so that ssh's
// ProxyCommand does not load the whole cage binary. This file therefore
// keeps to what moves with it, and the cage entry and cage ssh commands live
// in entry_cli.go. Once that program lands, what remains to move or replace:
//   - entryDial's connection: agentConnect (proven record, /api/session,
//     trusted-view cookie) becomes internal/controlclient's connect, whose
//     record needs Auth and ProofKey and whose dial (internal/controlwire,
//     already tcp4 127.0.0.1) needs the proof exchange of control_proof.go;
//     the WebSocket and entryRelay move onto controlwire's frames, without
//     gorilla/websocket;
//   - entryRefusal and its messages (entry_registry.go) and entryClientError,
//     whose AgentError cases become controlclient's errors;
//   - from entry_config.go: entryPaths, defaultEntryPaths,
//     entryProfileConfig, entryReadComputer, entryPinnedHostKey and
//     entrySweepOrphans with entryRemoveClient and the Include helpers; they
//     use only internal/cage's LocalVMDir, internal/host, x/crypto/ssh and
//     validLocalVMID.

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
			switch agentErr.Message {
			case errControllerUnproven.Error(), errControllerOutdated.Error(), errClientOutdated.Error():
				return agentErr.Message
			}
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
	if _, err := os.Stat(filepath.Dir(configPath)); errors.Is(err, os.ErrNotExist) && environment != defaultLocalVMID {
		// A deleted local VM: its block goes, and the Include line with
		// the last one.
		_ = entrySweepOrphans(paths)
		return fail("这个环境已经删除，Super Cage 已移除它的 ssh 配置")
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
	// The pin is written when the entry is turned on, from the controller's
	// own key, and only then (F2 of the review): the proxy compares and
	// never rewrites it, so whatever answers on the controller's port
	// cannot make ssh trust another key. A key that changed (an
	// environment created again) is refused here with the fix, before ssh
	// shows "REMOTE HOST IDENTIFICATION HAS CHANGED".
	if hostKey != nil {
		if pinned, err := entryPinnedHostKey(paths, environment); err != nil || !entryKeysEqual(pinned, hostKey) {
			return fail(errEntryHostKeyChanged)
		}
	}
	entryRelay(ctx, ws, stdin, stdout)
	return 0
}

const errEntryHostKeyChanged = "这个环境的主机钥匙和这台电脑记录的不一致：在 Super Cage 里关闭再开启本机终端即可更新"

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
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 15 * time.Second, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10, NetDialContext: c.dial}
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
