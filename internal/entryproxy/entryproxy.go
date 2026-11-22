// Package entryproxy is cage entry proxy, the ProxyCommand of the ssh Host
// blocks Super Cage writes for its local entry (internal/workbench,
// entry_*.go). It reaches the environment's local controller the way cage
// attach does (the private launcher record, the controller's proof, the
// launcher session), opens the entry's WebSocket and relays ssh's bytes.
// Any refusal is printed in Chinese on stderr and it exits 255, as ssh does
// for a connection that failed.
//
// Every ssh connection keeps one proxy running for as long as it lasts, so
// the slim cage-attach links this package: it uses only
// internal/controlclient and internal/controlwire, never net/http, TLS,
// x509 or x/crypto/ssh (cmd/cage-attach/main_test.go). The full cage runs
// the same code for cage entry proxy when no cage-attach is beside it.
//
// A cloud environment (cloud-<vm id>) is not this package's: its proxy is
// internal/cloudproxy, a program of its own (cmd/cage-cloud-entry), because
// TLS would load macOS's Security and CoreFoundation frameworks into every
// attached window.
package entryproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/controlclient"
	"github.com/sylvinus/agent-vm/internal/controlwire"
)

// Failure is the exit status of a proxy that could not connect, as ssh's
// own for a connection that failed.
const Failure = 255

// What the entry keeps on this computer, as internal/workbench writes it
// (its tests compare these with its own names).
const (
	// DefaultEnvironment names the default profile's environment.
	DefaultEnvironment = "default"
	// LocalVMDir is internal/cage's: a local VM's profile is
	// <default profile>/vms/<id>.
	LocalVMDir = "vms"
	// ClientDir is Super Cage's directory in ~/.ssh.
	ClientDir = "super-cage"
	// DeviceKey and DeviceRecord are this computer's device key and record
	// in its device directory.
	DeviceKey    = "device_ed25519"
	DeviceRecord = "device.json"
	// HostKeyHeader carries the entry's host key, an authorized_keys line,
	// in the answer to the WebSocket handshake.
	HostKeyHeader = "Cage-Entry-Host-Key"
)

// Refusal is why the entry refuses a dial or cut a connection, in the
// owner's words. The controller answers it as JSON {code, message}.
type Refusal struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Refusal) Error() string { return e.Message }

// The refusals the proxy names itself when an answer has no words of its
// own; the controller uses the same ones.
var (
	ErrLocalOnly = &Refusal{Status: 403, Code: "local_only", Message: "本机终端只能在这台电脑上开启和连接"}
	ErrLocked    = &Refusal{Status: 423, Code: "locked", Message: "Super Cage 已锁定，请在应用中解锁"}
	ErrClosing   = &Refusal{Status: 503, Code: "closing", Message: "Super Cage 正在退出"}
)

// ErrHostKeyChanged is the proxy's answer to a host key that is not the one
// this computer pinned when the entry was turned on.
const ErrHostKeyChanged = "这个环境的主机钥匙和这台电脑记录的不一致：在 Super Cage 里关闭再开启本机终端即可更新"

// HTTPRefusal reads a refusal the controller answered with status and body.
func HTTPRefusal(status int, body []byte) *Refusal {
	var refusal Refusal
	if json.Unmarshal(body, &refusal) == nil && refusal.Message != "" {
		refusal.Status = status
		return &refusal
	}
	switch status {
	case 423:
		return ErrLocked
	case 404:
		return &Refusal{Status: status, Code: "outdated", Message: "需要更新 Super Cage：正在运行的版本还没有本机终端"}
	case 401:
		return &Refusal{Status: status, Code: "authentication", Message: "无法向 Super Cage 认证：重新打开 Super Cage 后再试"}
	case 403:
		return ErrLocalOnly
	case 503:
		return ErrClosing
	}
	return &Refusal{Status: status, Code: "failed", Message: fmt.Sprintf("无法连接本机终端（HTTP %d）", status)}
}

// ClientError turns what the controller connection or the dial said into
// the owner's words.
func ClientError(err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Message
	}
	var agentErr *controlclient.AgentError
	if errors.As(err, &agentErr) {
		switch agentErr.Code {
		case "service_unavailable", "service_start_failed":
			return "Super Cage 没有在运行：打开 Super Cage 后再连接"
		case "controller_mismatch", "config_path_mismatch":
			switch agentErr.Message {
			case controlclient.ErrControllerUnproven.Error(), controlclient.ErrControllerOutdated.Error(), controlclient.ErrClientOutdated.Error():
				return agentErr.Message
			}
			return "需要更新 Super Cage：这个 cage 命令和正在运行的 Super Cage 版本不同"
		case "authentication_failed":
			return "无法向 Super Cage 认证：重新打开 Super Cage 后再试"
		case "account_locked":
			return ErrLocked.Message
		}
	}
	return "无法连接 Super Cage"
}

// DefaultConfig is the default profile's configuration path: fromEnv (the
// configuration variable cage reads) or ~/.claude-cage/config.json.
func DefaultConfig(fromEnv string) (string, error) {
	path := fromEnv
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errors.New("无法确定配置位置")
		}
		path = filepath.Join(home, ".claude-cage", "config.json")
	}
	return filepath.Abs(path)
}

// ValidEnvironment accepts a local VM's id, as internal/workbench does:
// 8 to 64 lower-case hexadecimal digits.
func ValidEnvironment(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// ProfileConfig resolves an environment id to its configuration path, from
// the default profile's configuration path.
func ProfileConfig(defaultConfig, environment string) (string, error) {
	if environment == DefaultEnvironment {
		return defaultConfig, nil
	}
	if !ValidEnvironment(environment) {
		return "", errors.New("环境标识无效")
	}
	return filepath.Join(filepath.Dir(defaultConfig), LocalVMDir, environment, "config.json"), nil
}

// DeviceDir is where this computer's device key lives: outside ~/.ssh, in
// the user's application data.
func DeviceDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("无法确定用户目录")
	}
	switch runtime.GOOS {
	case "darwin":
		config, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(config, "Super Cage", "entry"), nil
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return "", errors.New("无法确定 LOCALAPPDATA")
		}
		return filepath.Join(local, "Super Cage", "entry"), nil
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" || !filepath.IsAbs(data) {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "super-cage", "entry"), nil
}

// Paths are the places the proxy reads; it never writes anything.
type Paths struct {
	SSHDir    string // the user's ~/.ssh
	DeviceDir string // this computer's device key and record
}

// DefaultPaths are the user's own.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return Paths{}, errors.New("无法确定用户目录")
	}
	device, err := DeviceDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{SSHDir: filepath.Join(home, ".ssh"), DeviceDir: device}, nil
}

// KnownHosts is the environment's pinned host key, written only when the
// entry is turned on.
func (p Paths) KnownHosts(environment string) string {
	return filepath.Join(p.SSHDir, ClientDir, "hosts", environment+".known_hosts")
}

// Device reads this computer's device id; ok is false when the entry was
// never turned on here. The key itself is ssh's to read.
func Device(paths Paths) (id string, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(paths.DeviceDir, DeviceRecord))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var record struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &record) != nil || len(record.ID) != 32 {
		return "", false, errors.New("这台电脑的钥匙记录无法读取")
	}
	if _, err := hex.DecodeString(record.ID); err != nil {
		return "", false, errors.New("这台电脑的钥匙记录无法读取")
	}
	public, err := os.ReadFile(filepath.Join(paths.DeviceDir, DeviceKey+".pub"))
	if err != nil {
		return "", false, err
	}
	if kind, _, ok := parseKey(strings.Fields(string(public))); !ok || kind != "ssh-ed25519" {
		return "", false, errors.New("这台电脑的钥匙无法读取")
	}
	return record.ID, true, nil
}

// parseKey reads "<type> <base64> [comment]": the type and the key blob,
// when the blob is a key of that type.
func parseKey(fields []string) (string, []byte, bool) {
	if len(fields) < 2 {
		return "", nil, false
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 4 {
		return "", nil, false
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || string(blob[4:4+n]) != fields[0] {
		return "", nil, false
	}
	return fields[0], blob, true
}

// pinnedKey reads the key the entry pinned for environment, from the line
// "<host> <type> <base64>" it wrote.
func pinnedKey(paths Paths, environment string) (string, []byte, bool) {
	data, err := os.ReadFile(paths.KnownHosts(environment))
	if err != nil {
		return "", nil, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 3 {
			return "", nil, false
		}
		return parseKey(fields[1:])
	}
	return "", nil, false
}

// HostKeyPinned reports whether offered, an authorized_keys line, is the
// key pinned for environment: the same type and key blob.
func HostKeyPinned(paths Paths, environment, offered string) bool {
	kind, blob, ok := parseKey(strings.Fields(offered))
	if !ok {
		return false
	}
	pinnedKind, pinned, ok := pinnedKey(paths, environment)
	return ok && pinnedKind == kind && bytes.Equal(pinned, blob)
}

// Dial opens the entry's WebSocket on the controller of configPath and
// returns it with the host key line the controller answered with.
func Dial(ctx context.Context, configPath, device string, check controlclient.Check) (*controlwire.Conn, string, error) {
	c, err := controlclient.AgentConnect(ctx, configPath, check, controlwire.ConnectConn)
	if err != nil {
		return nil, "", err
	}
	defer c.Close()
	client, ok := c.(*controlwire.Client)
	if !ok {
		return nil, "", &Refusal{Code: "connection", Message: "无法连接 Super Cage 的本机终端"}
	}
	ws, answer, err := client.DialHandshake(ctx, controlclient.AgentTarget{Scope: "local"}, "/api/entry/ssh?device="+url.QueryEscape(device))
	if err != nil {
		if answer.Status != 0 && answer.Status != 101 {
			return nil, "", HTTPRefusal(answer.Status, answer.Body)
		}
		if identity := controlclient.IdentityAgentError(err); identity != nil {
			return nil, "", identity
		}
		return nil, "", &Refusal{Code: "connection", Message: "无法连接 Super Cage 的本机终端"}
	}
	return ws, answer.Header[strings.ToLower(HostKeyHeader)], nil
}

// Relay copies ssh's stdin into binary messages and the messages to ssh's
// stdout until either side ends. Each direction holds one 32 KiB buffer.
func Relay(ctx context.Context, ws *controlwire.Conn, stdin io.Reader, stdout io.Writer) {
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buffer)
			if n > 0 && ws.WriteMessage(controlwire.BinaryMessage, buffer[:n]) != nil {
				break
			}
			if err != nil {
				break
			}
		}
		_ = ws.WriteClose()
	}()
	writer := bufio.NewWriterSize(stdout, 32<<10)
	for {
		kind, reader, err := ws.NextReader()
		if err != nil {
			break
		}
		if kind != controlwire.BinaryMessage {
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

// Config is one proxy run.
type Config struct {
	ConfigPath  string // the environment's controller configuration
	Environment string // "default" or a local VM's id
	Paths       Paths
	// Check accepts the controller: the very build for cage, the
	// companion it verified for cage-attach.
	Check controlclient.Check
}

// Run is cage entry proxy for one environment.
func Run(ctx context.Context, cfg Config, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(message string) int {
		fmt.Fprintln(stderr, "Super Cage："+message)
		return Failure
	}
	if _, err := os.Stat(filepath.Dir(cfg.ConfigPath)); errors.Is(err, os.ErrNotExist) && cfg.Environment != DefaultEnvironment {
		// A deleted local VM. The proxy changes nothing in ~/.ssh: the
		// controller removes the block the next time the entry is turned
		// on or off.
		return fail("这个环境已经删除：在 Super Cage 里开启或关闭任一环境的本机终端时，会移除它的 ssh 配置")
	}
	device, ok, err := Device(cfg.Paths)
	if err != nil || !ok {
		return fail("这台电脑还没有开启本机终端：在 Super Cage 里开启")
	}
	ws, hostKey, err := Dial(ctx, cfg.ConfigPath, device, cfg.Check)
	if err != nil {
		return fail(ClientError(err))
	}
	defer ws.Close()
	// The pin is written when the entry is turned on, from the controller's
	// own key, and only then: the proxy compares and never rewrites it, so
	// whatever answers on the controller's port cannot make ssh trust
	// another key. A key that changed (an environment created again) is
	// refused here with the fix, before ssh shows "REMOTE HOST
	// IDENTIFICATION HAS CHANGED".
	if hostKey != "" {
		kind, offered, ok := parseKey(strings.Fields(hostKey))
		if !ok {
			return fail("Super Cage 返回了无效的主机钥匙")
		}
		pinnedKind, pinned, ok := pinnedKey(cfg.Paths, cfg.Environment)
		if !ok || pinnedKind != kind || !bytes.Equal(pinned, offered) {
			return fail(ErrHostKeyChanged)
		}
	}
	Relay(ctx, ws, stdin, stdout)
	return 0
}

// Main runs "proxy <environment>", the words after cage entry, for the
// default profile whose configuration path is defaultConfig.
func Main(ctx context.Context, args []string, defaultConfig string, check controlclient.Check, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "proxy" {
		fmt.Fprintln(stderr, "Super Cage：用法 cage entry proxy <环境>")
		return Failure
	}
	if strings.HasPrefix(args[1], cloudentry.EnvironmentPrefix) {
		// Without TLS: cage entry proxy and cage-cloud-entry connect cloud
		// environments, never this program.
		message := cloudentry.ClosedMessage
		if cloudentry.Open() {
			message = "云端工作区由 cage-cloud-entry 连接：用 Super Cage 生成的 ssh 配置或 cage ssh 连接"
		}
		fmt.Fprintln(stderr, "Super Cage："+message)
		return Failure
	}
	configPath, err := ProfileConfig(defaultConfig, args[1])
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+err.Error())
		return Failure
	}
	paths, err := DefaultPaths()
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+err.Error())
		return Failure
	}
	return Run(ctx, Config{ConfigPath: configPath, Environment: args[1], Paths: paths, Check: check}, stdin, stdout, stderr)
}
