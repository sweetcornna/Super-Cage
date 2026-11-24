package guestos

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Lima chooses a new loopback port on each boot. Read only that number from
// its generated config; use the provisioned VM key, user and identity file.
func MacOSSSHPort(config []byte) (int, error) {
	var port int
	for _, line := range strings.Split(string(config), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "port") {
			continue
		}
		if len(fields) != 2 || port != 0 {
			return 0, errors.New("macOS 客机 SSH 端口配置无效")
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil || value < 1 || value > 65535 {
			return 0, errors.New("macOS 客机 SSH 端口配置无效")
		}
		port = value
	}
	if port == 0 {
		return 0, errors.New("macOS 客机 SSH 端口尚未就绪")
	}
	return port, nil
}

func macOSSSHBaseArgs(profile, user string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(profile, "state", "lima", "mac", "ssh.config"))
	if err != nil {
		return nil, err
	}
	port, err := MacOSSSHPort(data)
	if err != nil {
		return nil, err
	}
	// ssh drops an -i whose file is not there and then offers the default
	// keys of the home in the user database (~/.ssh/id_*), IdentitiesOnly or
	// not: ssh -G lists them. So the provisioned key must be a file first.
	key := filepath.Join(profile, "state", "lima", "_config", "user")
	if info, err := os.Lstat(key); errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("macOS 客机 SSH 密钥尚未就绪")
	} else if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("macOS 客机 SSH 密钥无效")
	}
	known := filepath.Join(profile, "macos-known-hosts")
	// ssh -o parses each option as a config line; quote paths with spaces.
	// Lima's ssh.config is not used: the port is reread each call because it
	// changes between boots.
	return []string{"-q", "-e", "none", "-F", os.DevNull, "-p", strconv.Itoa(port),
		"-l", user, "-i", key,
		"-o", "Hostname=127.0.0.1", "-o", "HostKeyAlias=cage-macos",
		"-o", "StrictHostKeyChecking=yes", "-o", "HostKeyAlgorithms=ssh-ed25519",
		"-o", "UserKnownHostsFile=" + strconv.Quote(known), "-o", "GlobalKnownHostsFile=" + os.DevNull,
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "ForwardAgent=no", "-o", "ControlPath=none",
		"-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2"}, nil
}

func MacOSSSHArgs(profile, user string) ([]string, error) {
	base, err := macOSSSHBaseArgs(profile, user)
	if err != nil {
		return nil, err
	}
	return append(base, "-o", "ClearAllForwardings=yes"), nil
}

// MacOSScreenSharingSSHArgs builds a loopback-only local forward to the guest
// Screen Sharing port. Do not reuse
// MacOSSSHArgs: that set clears all forwards.
func MacOSScreenSharingSSHArgs(profile, user string, hostPort int) ([]string, error) {
	return MacOSLocalForwardSSHArgs(profile, user, hostPort, 5900)
}

// MacOSLocalForwardSSHArgs builds a loopback-only local forward from hostPort
// to guestPort inside the macOS VM with authenticated LocalCommand readiness.
func MacOSLocalForwardSSHArgs(profile, user string, hostPort, guestPort int) ([]string, error) {
	if hostPort < 1 || hostPort > 65535 || guestPort < 1 || guestPort > 65535 {
		return nil, errors.New("本机转发端口无效")
	}
	base, err := macOSSSHBaseArgs(profile, user)
	if err != nil {
		return nil, err
	}
	return append(base,
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "PermitLocalCommand=yes", "-o", "LocalCommand=echo 1",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", hostPort, guestPort)), nil
}

// MacOSProvisionUser reads Lima's installation identity, including older VMs.
// Only these two dedicated identities may run privileged maintenance.
func MacOSProvisionUser(profile string) (string, error) {
	data, err := os.ReadFile(filepath.Join(profile, "state", "lima", "mac", "ssh.config"))
	if err != nil {
		return "", err
	}
	var user string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "User") {
			continue
		}
		if len(fields) != 2 || user != "" || (fields[1] != "cage" && fields[1] != "cageprovision") {
			return "", errors.New("macOS 客机安装账户配置无效")
		}
		user = fields[1]
	}
	if user == "" {
		return "", errors.New("macOS 客机安装账户尚未就绪")
	}
	return user, nil
}
