// Package cloudproxy is cage entry proxy for a cloud environment
// (cloud-<vm id>, design 5.2): it opens wss://vm-<id>.<portal>/_cage/entry
// itself, with the portal's normal TLS certificate, and signs the admission
// with this computer's device key, the same key ssh then uses inside. SSH
// runs end to end between the owner's ssh and the workspace's workbench; the
// gateway only splices.
//
// It is a program of its own, cage-cloud-entry (cmd/cage-cloud-entry), and
// never part of cage-attach: crypto/x509 makes macOS load the Security and
// CoreFoundation frameworks at launch, about 4.5 MB more in every attached
// window and local ssh connection. It links no net/http either.
package cloudproxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/controlwire"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
)

// ProgramName is the file name of the cloud entry's proxy program.
func ProgramName() string {
	if runtime.GOOS == "windows" {
		return "cage-cloud-entry.exe"
	}
	return "cage-cloud-entry"
}

// Beside returns the cage-cloud-entry installed in the same directory as
// exe, after resolving symbolic links, or "" when there is none; cage itself
// is the proxy then (cage entry proxy).
func Beside(exe string) string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	path := filepath.Join(filepath.Dir(exe), ProgramName())
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// CloudDir is where the desktop records, per cloud environment, the portal,
// the VM and this computer's device id at that portal; the proxy only
// reads it.
const CloudDir = "cloud"

// CloudRecord is one cloud environment on this computer.
type CloudRecord struct {
	Version int    `json:"version"`
	Portal  string `json:"portal"` // the portal's host, with its port unless 443
	VM      string `json:"vm"`
	Device  string `json:"device"` // this computer's id at that portal
}

// RecordPath is the record of environment in paths.
func RecordPath(paths entryproxy.Paths, environment string) string {
	return filepath.Join(paths.DeviceDir, CloudDir, environment+".json")
}

// ErrCloudHostKeyChanged: the endpoint's key is not the one pinned when the
// entry was turned on. The proxy never rewrites a pinned cloud key.
const ErrCloudHostKeyChanged = "这个工作区的主机钥匙变了（可能被重建）：在 Super Cage 里确认后再连接"

// ReadCloudRecord reads and checks the record of environment.
func ReadCloudRecord(paths entryproxy.Paths, environment string) (CloudRecord, error) {
	vm, ok := cloudentry.VMOf(environment)
	if !ok {
		return CloudRecord{}, errors.New("环境标识无效")
	}
	data, err := os.ReadFile(RecordPath(paths, environment))
	if errors.Is(err, os.ErrNotExist) {
		return CloudRecord{}, errors.New("这台电脑还没有开启这个云端工作区的本机终端：在 Super Cage 里开启")
	}
	if err != nil {
		return CloudRecord{}, err
	}
	var record CloudRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.VM != vm || !cloudentry.ValidDevice(record.Device) || !validPortalHost(record.Portal) {
		return CloudRecord{}, errors.New("这个云端工作区的记录无法读取：在 Super Cage 里关闭再开启本机终端")
	}
	return record, nil
}

// validPortalHost accepts a DNS name with an optional port, lower case.
func validPortalHost(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name, port = host, ""
	}
	if len(name) > 217 || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return false
		}
	}
	if port != "" {
		if len(port) > 5 || strings.Trim(port, "0123456789") != "" || port[0] == '0' || port == "443" {
			return false
		}
	}
	return true
}

// DeviceSigner reads this computer's device key: the unencrypted OpenSSH
// Ed25519 key Super Cage wrote (ssh.MarshalPrivateKey), parsed here without
// x/crypto/ssh.
func DeviceSigner(paths entryproxy.Paths) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(filepath.Join(paths.DeviceDir, entryproxy.DeviceKey))
	if err != nil {
		return nil, err
	}
	key, ok := parseOpenSSHEd25519(data)
	if !ok {
		return nil, errors.New("这台电脑的钥匙无法读取")
	}
	return key, nil
}

func parseOpenSSHEd25519(data []byte) (ed25519.PrivateKey, bool) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return nil, false
	}
	const magic = "openssh-key-v1\x00"
	rest, ok := bytes.CutPrefix(block.Bytes, []byte(magic))
	if !ok {
		return nil, false
	}
	read := func() ([]byte, bool) {
		if len(rest) < 4 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(rest)
		if uint64(n) > uint64(len(rest)-4) {
			return nil, false
		}
		field := rest[4 : 4+n]
		rest = rest[4+n:]
		return field, true
	}
	cipher, ok1 := read()
	kdf, ok2 := read()
	options, ok3 := read()
	if !ok1 || !ok2 || !ok3 || string(cipher) != "none" || string(kdf) != "none" || len(options) != 0 || len(rest) < 4 || binary.BigEndian.Uint32(rest) != 1 {
		return nil, false
	}
	rest = rest[4:]
	public, ok1 := read()
	private, ok2 := read()
	if !ok1 || !ok2 || len(rest) != 0 {
		return nil, false
	}
	rest = private
	if len(rest) < 8 || binary.BigEndian.Uint32(rest) != binary.BigEndian.Uint32(rest[4:]) {
		return nil, false
	}
	rest = rest[8:]
	kind, ok1 := read()
	pub, ok2 := read()
	priv, ok3 := read()
	if !ok1 || !ok2 || !ok3 || string(kind) != "ssh-ed25519" || len(pub) != ed25519.PublicKeySize || len(priv) != ed25519.PrivateKeySize || !bytes.Equal(priv[32:], pub) {
		return nil, false
	}
	if _, blob, ok := cloudentry.ParseAuthorizedKey("ssh-ed25519 " + base64.StdEncoding.EncodeToString(public)); !ok || !bytes.Equal(blob[len(blob)-32:], pub) {
		return nil, false
	}
	key := ed25519.NewKeyFromSeed(priv[:32])
	if !bytes.Equal(key.Public().(ed25519.PublicKey), pub) {
		return nil, false
	}
	return key, true
}

// cloudDial is how cloud mode reaches the portal; tests replace it with one
// that trusts their own certificate. Production uses the system's roots.
var cloudDial = func(ctx context.Context, address, serverName string) (net.Conn, error) {
	return tlsDial(ctx, address, serverName, nil)
}

func tlsDial(ctx context.Context, address, serverName string, roots *x509.CertPool) (net.Conn, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		Config:    &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, RootCAs: roots},
	}
	return dialer.DialContext(ctx, "tcp", address)
}

// cloudStartWait bounds how long an interactive dial waits for a workspace
// it woke from stopped; cloudRetry is the pause between its tries.
var (
	cloudStartWait = 3 * time.Minute
	cloudRetry     = 3 * time.Second
)

// interactive: ssh's stderr is a terminal, so a person typed the command.
// Herdr and scripts run ssh with stderr on a pipe, and their dials never wake
// a cooled workspace.
var interactive = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }

// DialCloud opens the cloud entry of record and returns its socket and the
// host key it answered with.
func DialCloud(ctx context.Context, record CloudRecord, key ed25519.PrivateKey, wake bool, stderr io.Writer) (*controlwire.Conn, string, error) {
	host := "vm-" + record.VM + "." + record.Portal
	address := host
	serverName := host
	if name, _, err := net.SplitHostPort(host); err == nil {
		serverName = name
	} else {
		address = net.JoinHostPort(host, "443")
	}
	deadline := time.Now().Add(cloudStartWait)
	announced := false
	for {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, "", err
		}
		auth := cloudentry.Sign(key, record.Device, host, record.VM, time.Now(), nonce)
		headers := [][2]string{{"Authorization", auth.String()}, {"User-Agent", "Super-Cage-entry"}}
		if wake {
			headers = append(headers, [2]string{cloudentry.HeaderWake, "1"})
		}
		conn, err := cloudDial(ctx, address, serverName)
		if err != nil {
			var unknown x509.UnknownAuthorityError
			var invalid x509.CertificateInvalidError
			var hostname x509.HostnameError
			if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) {
				return nil, "", &entryproxy.Refusal{Code: "tls", Message: "门户的 TLS 证书无法验证：确认网络没有被拦截"}
			}
			return nil, "", &entryproxy.Refusal{Code: "connection", Message: "无法连接门户：检查网络后再试"}
		}
		ws, answer, err := controlwire.UpgradeConn(ctx, conn, host, cloudentry.Path, headers)
		if err == nil {
			return ws, answer.Header[strings.ToLower(cloudentry.HostKeyHeader)], nil
		}
		if answer.Status == 0 || answer.Status == 101 {
			return nil, "", &entryproxy.Refusal{Code: "connection", Message: "无法连接门户：检查网络后再试"}
		}
		refusal := cloudRefusal(answer.Status, answer.Body)
		if wake && refusal.Code == "starting" && time.Now().Before(deadline) {
			if !announced {
				fmt.Fprintln(stderr, "Super Cage：工作区正在启动…")
				announced = true
			}
			timer := time.NewTimer(cloudRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, "", refusal
			case <-timer.C:
			}
			continue
		}
		return nil, "", refusal
	}
}

// cloudRefusal reads the gateway's answer; a portal without the cloud entry
// answers 404.
func cloudRefusal(status int, body []byte) *entryproxy.Refusal {
	var refusal entryproxy.Refusal
	if json.Unmarshal(body, &refusal) == nil && refusal.Message != "" && refusal.Code != "" {
		refusal.Status = status
		return &refusal
	}
	switch status {
	case 404:
		return &entryproxy.Refusal{Status: status, Code: "unavailable", Message: "这个门户还没有开放本机终端"}
	case 429:
		return &entryproxy.Refusal{Status: status, Code: "rate_limited", Message: "连接过于频繁，稍后再试"}
	case 503:
		return &entryproxy.Refusal{Status: status, Code: "unavailable", Message: "门户或工作区暂时不可用，稍后再试"}
	}
	return &entryproxy.Refusal{Status: status, Code: "failed", Message: fmt.Sprintf("无法连接云端工作区（HTTP %d）", status)}
}

// Run is cage entry proxy for a cloud environment.
func Run(ctx context.Context, paths entryproxy.Paths, environment string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(message string) int {
		fmt.Fprintln(stderr, "Super Cage："+message)
		return entryproxy.Failure
	}
	if !cloudentry.Open() {
		return fail(cloudentry.ClosedMessage)
	}
	record, err := ReadCloudRecord(paths, environment)
	if err != nil {
		return fail(err.Error())
	}
	key, err := DeviceSigner(paths)
	if err != nil {
		return fail("这台电脑还没有开启本机终端：在 Super Cage 里开启")
	}
	ws, hostKey, err := DialCloud(ctx, record, key, interactive(), stderr)
	if err != nil {
		return fail(entryproxy.ClientError(err))
	}
	defer ws.Close()
	// The pin is written when the entry is turned on and never here: a
	// gateway that turned hostile cannot make ssh trust another key.
	if !entryproxy.HostKeyPinned(paths, environment, hostKey) {
		return fail(ErrCloudHostKeyChanged)
	}
	entryproxy.Relay(ctx, ws, stdin, stdout)
	return 0
}

// Main runs "entry proxy <environment>" (or "proxy <environment>"), the words
// a Host block's ProxyCommand gives cage-cloud-entry and cage.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 3 && args[0] == "entry" {
		args = args[1:]
	}
	if len(args) != 2 || args[0] != "proxy" {
		fmt.Fprintln(stderr, "Super Cage：用法 cage-cloud-entry entry proxy cloud-<工作区>")
		return entryproxy.Failure
	}
	if !cloudentry.Open() {
		fmt.Fprintln(stderr, "Super Cage："+cloudentry.ClosedMessage)
		return entryproxy.Failure
	}
	paths, err := entryproxy.DefaultPaths()
	if err != nil {
		fmt.Fprintln(stderr, "Super Cage："+err.Error())
		return entryproxy.Failure
	}
	return Run(ctx, paths, args[1], stdin, stdout, stderr)
}
