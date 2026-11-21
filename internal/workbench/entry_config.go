package workbench

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
)

// The local SSH entry ("本机终端") keeps three kinds of state:
//   - per environment (this controller's profile): whether the entry is on,
//     the devices enrolled for it and the endpoint's host key, under
//     <profile>/entry;
//   - per computer: the device key, created by Super Cage, readable only by
//     the user (design 5.3), and the owner's local terminal choices
//     (local-terminal.json, shared with cage attach);
//   - the owner's ssh client configuration: Super Cage's own files under
//     ~/.ssh/super-cage and, only after the owner confirmed it, one Include
//     line at the top of ~/.ssh/config.
// Nothing is written while the entry stays off; reading a status never
// creates a key or a directory.

const (
	entryStateVersion = 1
	entryDeviceKey    = "device_ed25519"
	entryDeviceRecord = "device.json"
	entryHostKey      = "host_ed25519"
	entryClientDir    = "super-cage"
	entryBackupSuffix = ".super-cage-backup"
	// entrySettingsFile is cage attach's per-computer settings file
	// (internal/localterm on branch local-terminal-attach). The entry only
	// reads it; replace entryLoadLocalSettings with localterm.LoadSettings
	// once that package is merged.
	entrySettingsFile = "local-terminal.json"
)

// entryPaths are the places on this computer the entry reads and writes.
// Production derives them from the user's directories; tests and experiments
// inject scratch paths, so they never touch the real ~/.ssh.
type entryPaths struct {
	SSHDir      string // the user's ~/.ssh
	DeviceDir   string // this computer's device key
	ComputerDir string // the default profile, where local-terminal.json lives
	// HomeSSH: SSHDir is the user's own ~/.ssh, so the Include line may name
	// it as ~ and stay valid in a dotfiles repository shared between computers.
	HomeSSH bool
	// Proxy is the ProxyCommand of an environment's Host block.
	Proxy func(environment string) string
	// Cage is the cage command shown in the commands to copy.
	Cage string
}

func defaultEntryPaths(profile, executable string) (entryPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return entryPaths{}, errors.New("无法确定用户目录")
	}
	var device string
	switch runtime.GOOS {
	case "darwin":
		config, err := os.UserConfigDir()
		if err != nil {
			return entryPaths{}, err
		}
		device = filepath.Join(config, "Super Cage", "entry")
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return entryPaths{}, errors.New("无法确定 LOCALAPPDATA")
		}
		device = filepath.Join(local, "Super Cage", "entry")
	default:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" || !filepath.IsAbs(data) {
			data = filepath.Join(home, ".local", "share")
		}
		device = filepath.Join(data, "super-cage", "entry")
	}
	return entryPaths{
		SSHDir:      filepath.Join(home, ".ssh"),
		DeviceDir:   device,
		ComputerDir: entryComputerProfile(profile),
		HomeSSH:     true,
		Proxy: func(environment string) string {
			return entryQuote(executable) + " entry proxy " + environment
		},
		Cage: executable,
	}, nil
}

// entryComputerProfile is the default profile of a local VM's profile
// (<default>/vms/<id>), or the profile itself.
func entryComputerProfile(profile string) string {
	parent := filepath.Dir(profile)
	if filepath.Base(parent) == cage.LocalVMDir && validLocalVMID(filepath.Base(profile)) {
		return filepath.Dir(parent)
	}
	return profile
}

// entryEnvironmentID names this controller's environment the way the
// ProxyCommand and cage entry address it: "default" or a local VM's id.
func entryEnvironmentID(profile string) string {
	if entryComputerProfile(profile) != profile {
		return filepath.Base(profile)
	}
	return defaultLocalVMID
}

func entryEnvironmentName(profile string) string {
	id := entryEnvironmentID(profile)
	if id == defaultLocalVMID {
		return "默认环境"
	}
	var manifest localVMManifestRecord
	if data, err := os.ReadFile(filepath.Join(profile, localVMManifest)); err == nil && json.Unmarshal(data, &manifest) == nil && validLocalVMName(manifest.Name) == nil {
		return manifest.Name
	}
	return "虚拟机 " + id[:8]
}

// entryProfileConfig resolves an environment id to its configuration path,
// from the default profile's configuration path.
func entryProfileConfig(defaultConfig, environment string) (string, error) {
	if environment == defaultLocalVMID {
		return defaultConfig, nil
	}
	if !validLocalVMID(environment) {
		return "", errors.New("环境标识无效")
	}
	return filepath.Join(filepath.Dir(defaultConfig), cage.LocalVMDir, environment, "config.json"), nil
}

// entryDevice is one computer enrolled for this environment's entry.
type entryDevice struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	PublicKey string     `json:"public_key"`
	Created   time.Time  `json:"created"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	Revoked   *time.Time `json:"revoked,omitempty"`
}

type entryState struct {
	Version int           `json:"version"`
	Enabled bool          `json:"enabled"`
	Devices []entryDevice `json:"devices"`
}

func (st *entryState) device(id string) *entryDevice {
	for i := range st.Devices {
		if st.Devices[i].ID == id {
			return &st.Devices[i]
		}
	}
	return nil
}

func entryDir(profile string) string { return filepath.Join(profile, "entry") }

// entryLoadState reads the environment's entry state; a missing file is the
// default: off, no devices.
func entryLoadState(profile string) (entryState, error) {
	var st entryState
	data, err := os.ReadFile(filepath.Join(entryDir(profile), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return entryState{Version: entryStateVersion}, nil
	}
	if err != nil {
		return st, err
	}
	if json.Unmarshal(data, &st) != nil || st.Version != entryStateVersion {
		return entryState{Version: entryStateVersion}, errors.New("本机终端的记录无法读取")
	}
	return st, nil
}

func entrySaveState(profile string, st entryState) error {
	st.Version = entryStateVersion
	return writePrivate(filepath.Join(entryDir(profile), "state.json"), st)
}

// entryComputer is this computer's identity for the entry.
type entryComputer struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	// Public is the authorized_keys form of the device key; not stored.
	Public string `json:"-"`
}

// entryReadComputer reads this computer's device record and public key
// without creating anything. ok is false when the entry was never turned on
// here.
func entryReadComputer(paths entryPaths) (entryComputer, bool, error) {
	var computer entryComputer
	data, err := os.ReadFile(filepath.Join(paths.DeviceDir, entryDeviceRecord))
	if errors.Is(err, os.ErrNotExist) {
		return computer, false, nil
	}
	if err != nil {
		return computer, false, err
	}
	if json.Unmarshal(data, &computer) != nil || !validEntryID(computer.ID) {
		return computer, false, errors.New("这台电脑的钥匙记录无法读取")
	}
	public, err := os.ReadFile(filepath.Join(paths.DeviceDir, entryDeviceKey+".pub"))
	if err != nil {
		return computer, false, err
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(public)
	if err != nil || key.Type() != ssh.KeyAlgoED25519 {
		return computer, false, errors.New("这台电脑的钥匙无法读取")
	}
	computer.Public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	return computer, true, nil
}

func validEntryID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// entryEnsureComputer creates this computer's device key the first time the
// entry is turned on, or a new one when rotate is set (after a revocation).
// The private half is written 0600 into a 0700 directory.
func entryEnsureComputer(paths entryPaths, rotate bool) (entryComputer, error) {
	computer, ok, err := entryReadComputer(paths)
	if err != nil && !rotate {
		return computer, err
	}
	if ok && !rotate {
		return computer, nil
	}
	if err := host.EnsurePrivateDir(paths.DeviceDir); err != nil {
		return computer, err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return computer, err
	}
	block, err := ssh.MarshalPrivateKey(private, "super-cage-entry")
	if err != nil {
		return computer, err
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		return computer, err
	}
	if !ok {
		computer = entryComputer{Version: 1, ID: randomToken()[:32], Name: entryComputerName(), Created: time.Now().UTC()}
	}
	if err := entryWriteFile(filepath.Join(paths.DeviceDir, entryDeviceKey), pem.EncodeToMemory(block), 0o600); err != nil {
		return computer, err
	}
	authorized := ssh.MarshalAuthorizedKey(key)
	if err := entryWriteFile(filepath.Join(paths.DeviceDir, entryDeviceKey+".pub"), authorized, 0o600); err != nil {
		return computer, err
	}
	if err := writePrivate(filepath.Join(paths.DeviceDir, entryDeviceRecord), computer); err != nil {
		return computer, err
	}
	computer.Public = strings.TrimSpace(string(authorized))
	return computer, nil
}

// entryComputerName is the default device name; the owner can rename it.
// It stays on this computer for a local environment.
func entryComputerName() string {
	name, err := os.Hostname()
	if err != nil {
		return "这台电脑"
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, "."), ".local")
	if name == "" || !utf8.ValidString(name) || len(name) > 64 {
		return "这台电脑"
	}
	return name
}

// entryHostSigner loads the endpoint's host key, creating it when create is
// set. One key per environment, kept in the controller's profile.
func entryHostSigner(profile string, create bool) (ssh.Signer, error) {
	path := filepath.Join(entryDir(profile), entryHostKey)
	data, err := os.ReadFile(path)
	if err == nil {
		return ssh.ParsePrivateKey(data)
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, "super-cage-entry-host")
	if err != nil {
		return nil, err
	}
	data = pem.EncodeToMemory(block)
	if err := host.EnsurePrivateDir(entryDir(profile)); err != nil {
		return nil, err
	}
	if err := entryWriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

func entryFingerprint(authorized string) string {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

// entryWriteFile replaces path atomically with data at mode.
func entryWriteFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cage-entry-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil && mode&0o077 == 0 {
		err = host.ProtectFile(f.Name())
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// entryLocalSettings are the owner's per-computer choices that the entry's
// terminal sessions follow. The defaults are what Cage's web terminal does:
// image paste and drop upload, and OSC 52 clipboard writes (never reads).
type entryLocalSettings struct {
	ImageUpload    bool `json:"image_upload"`
	ClipboardWrite bool `json:"clipboard_write"`
}

func entryLoadLocalSettings(paths entryPaths) entryLocalSettings {
	settings := entryLocalSettings{ImageUpload: true, ClipboardWrite: true}
	data, err := os.ReadFile(filepath.Join(paths.ComputerDir, entrySettingsFile))
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &settings) != nil {
		return entryLocalSettings{ImageUpload: true, ClipboardWrite: true}
	}
	return settings
}

// --- the owner's ssh client configuration ---

func (p entryPaths) clientDir() string  { return filepath.Join(p.SSHDir, entryClientDir) }
func (p entryPaths) hostsDir() string   { return filepath.Join(p.clientDir(), "hosts") }
func (p entryPaths) clientFile() string { return filepath.Join(p.clientDir(), "config") }
func (p entryPaths) userConfig() string { return filepath.Join(p.SSHDir, "config") }
func (p entryPaths) hostFile(environment string) string {
	return filepath.Join(p.hostsDir(), environment+".conf")
}
func (p entryPaths) knownHosts(environment string) string {
	return filepath.Join(p.hostsDir(), environment+".known_hosts")
}
func (p entryPaths) deviceKey() string { return filepath.Join(p.DeviceDir, entryDeviceKey) }

// includeLine is the one line Super Cage may add to ~/.ssh/config.
func (p entryPaths) includeLine() string {
	if p.HomeSSH {
		return "Include ~/.ssh/" + entryClientDir + "/config"
	}
	return "Include " + entryQuote(p.clientFile())
}

// entryQuote quotes a path or command word for ssh_config and for the shell
// ssh runs a ProxyCommand with.
func entryQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\"'\\$`;&|<>()*?[]#~%") {
		return value
	}
	return "\"" + value + "\""
}

// entryConfigPath escapes ssh's % tokens in a path option and quotes it.
func entryConfigPath(path string) string {
	return entryQuote(strings.ReplaceAll(path, "%", "%%"))
}

func validEntryConfigValue(value string) bool {
	return !strings.ContainsAny(value, "\"\r\n\x00")
}

// entryHostAlias is "cage-<slug>" from the environment's name, unique among
// the Host blocks already written on this computer.
func entryHostAlias(paths entryPaths, environment, name string) string {
	var slug []byte
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug = append(slug, byte(r))
			dash = false
		} else if len(slug) > 0 && !dash {
			slug = append(slug, '-')
			dash = true
		}
	}
	base := strings.Trim(string(slug), "-")
	if len(base) > 32 {
		base = strings.Trim(base[:32], "-")
	}
	if base == "" {
		base = environment
		if environment != defaultLocalVMID {
			base = environment[:8]
		}
	}
	alias := "cage-" + base
	taken := entryAliasesInUse(paths, environment)
	if slices.Contains(taken, alias) {
		suffix := environment
		if len(suffix) > 6 {
			suffix = suffix[:6]
		}
		alias = "cage-" + base + "-" + suffix
	}
	return alias
}

// entryAliasesInUse lists the Host aliases of the other environments' blocks.
func entryAliasesInUse(paths entryPaths, except string) []string {
	matches, _ := filepath.Glob(filepath.Join(paths.hostsDir(), "*.conf"))
	var aliases []string
	for _, match := range matches {
		if strings.TrimSuffix(filepath.Base(match), ".conf") == except {
			continue
		}
		if alias := entryReadAlias(match); alias != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

func entryReadAlias(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Host" {
			return fields[1]
		}
	}
	return ""
}

// entryHostBlock is the generated Host block of design 5.3.
func entryHostBlock(paths entryPaths, environment, name, alias string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Super Cage 本机终端 — generated, do not edit. Environment: %s\n", strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name))
	fmt.Fprintf(&b, "Host %s\n", alias)
	for _, option := range [][2]string{
		{"IgnoreUnknown", "ObscureKeystrokeTiming"},
		{"HostName", alias + ".invalid"},
		{"User", "cage"},
		{"ProxyCommand", paths.Proxy(environment)},
		{"HostKeyAlias", "cage-" + environment},
		{"UserKnownHostsFile", entryConfigPath(paths.knownHosts(environment))},
		{"StrictHostKeyChecking", "yes"},
		{"UpdateHostKeys", "no"},
		{"IdentityFile", entryConfigPath(paths.deviceKey())},
		{"IdentitiesOnly", "yes"},
		{"IdentityAgent", "none"},
		{"ForwardAgent", "no"},
		{"ForwardX11", "no"},
		{"ForwardX11Trusted", "no"},
		{"ClearAllForwardings", "yes"},
		{"Tunnel", "no"},
		{"PermitLocalCommand", "no"},
		{"RemoteCommand", "none"},
		{"RequestTTY", "auto"},
		{"ObscureKeystrokeTiming", "no"},
		{"ServerAliveInterval", "15"},
		{"ServerAliveCountMax", "4"},
	} {
		fmt.Fprintf(&b, "  %s %s\n", option[0], option[1])
	}
	return b.String()
}

// entryWriteClient writes this environment's Host block and pinned host key,
// and Super Cage's own config file that includes every block. Each
// environment owns its own two files, so two controllers never write the
// same one.
func entryWriteClient(paths entryPaths, environment, name string, hostKey ssh.PublicKey) (string, error) {
	for _, value := range []string{paths.clientFile(), paths.deviceKey(), paths.Proxy(environment)} {
		if !validEntryConfigValue(value) {
			return "", errors.New("路径里有 ssh 配置无法表示的字符")
		}
	}
	if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
		return "", err
	}
	if err := host.EnsurePrivateDir(paths.clientDir()); err != nil {
		return "", err
	}
	if err := host.EnsurePrivateDir(paths.hostsDir()); err != nil {
		return "", err
	}
	include := "# Super Cage 本机终端 — generated, do not edit.\nInclude " + entryQuote(filepath.Join(paths.hostsDir(), "*.conf")) + "\n"
	if err := entryWriteFile(paths.clientFile(), []byte(include), 0o600); err != nil {
		return "", err
	}
	alias := entryReadAlias(paths.hostFile(environment))
	if alias == "" {
		alias = entryHostAlias(paths, environment, name)
	}
	if err := entryPinHostKey(paths, environment, hostKey); err != nil {
		return "", err
	}
	return alias, entryWriteFile(paths.hostFile(environment), []byte(entryHostBlock(paths, environment, name, alias)), 0o600)
}

// entryPinHostKey writes the environment's known_hosts line. Local only: the
// key comes from this computer's own controller.
func entryPinHostKey(paths entryPaths, environment string, key ssh.PublicKey) error {
	line := "cage-" + environment + " " + string(ssh.MarshalAuthorizedKey(key))
	if current, err := os.ReadFile(paths.knownHosts(environment)); err == nil && string(current) == line {
		return nil
	}
	return entryWriteFile(paths.knownHosts(environment), []byte(line), 0o600)
}

// entryRemoveClient removes this environment's block. When no block is left,
// Super Cage's own files and the Include line go too.
func entryRemoveClient(paths entryPaths, environment string) error {
	for _, path := range []string{paths.hostFile(environment), paths.knownHosts(environment)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	left, _ := filepath.Glob(filepath.Join(paths.hostsDir(), "*.conf"))
	if len(left) > 0 {
		return nil
	}
	if err := entryIncludeRemove(paths); err != nil {
		return err
	}
	for _, path := range []string{paths.clientFile(), paths.hostsDir(), paths.clientDir()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirNotEmpty(err) {
			return err
		}
	}
	return nil
}

func isDirNotEmpty(err error) bool {
	var pathErr *os.PathError
	return errors.As(err, &pathErr) && (strings.Contains(pathErr.Err.Error(), "not empty") || strings.Contains(pathErr.Err.Error(), "exists"))
}

// --- the one Include line in ~/.ssh/config ---

// entryInclude is what the confirmation step shows: the file that changes,
// the exact line, and whether it is already there. Token binds the apply
// call to the content the owner saw.
type entryInclude struct {
	Path        string `json:"path"`
	DisplayPath string `json:"display_path"`
	Line        string `json:"line"`
	Present     bool   `json:"present"`
	Token       string `json:"token,omitempty"`
	Backup      string `json:"backup,omitempty"`
	Error       string `json:"error,omitempty"`
}

// entryUserConfigTarget is ~/.ssh/config, or the file it links to, so a
// dotfiles link keeps pointing at the owner's file.
func entryUserConfigTarget(paths entryPaths) (string, error) {
	path := paths.userConfig()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return filepath.EvalSymlinks(path)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("~/.ssh/config 不是普通文件")
	}
	return path, nil
}

func entryIncludeToken(content []byte, line string) string {
	sum := sha256.Sum256(append(append([]byte(line), 0), content...))
	return hex.EncodeToString(sum[:])
}

// entryIncludeLines are the lines entryIncludeRemove takes out: the one
// shown in the preview, and the other form of the same Include.
func entryIncludeLines(paths entryPaths) []string {
	home := paths
	home.HomeSSH = true
	absolute := paths
	absolute.HomeSSH = false
	return []string{paths.includeLine(), home.includeLine(), absolute.includeLine()}
}

func entryHasInclude(content []byte, lines []string) bool {
	for _, line := range strings.Split(string(content), "\n") {
		if slices.Contains(lines, strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// entryIncludePreview reads ~/.ssh/config and never writes.
func entryIncludePreview(paths entryPaths) entryInclude {
	view := entryInclude{Path: paths.userConfig(), DisplayPath: "~/.ssh/config", Line: paths.includeLine()}
	if !paths.HomeSSH {
		view.DisplayPath = view.Path
	}
	target, err := entryUserConfigTarget(paths)
	if err != nil {
		view.Error = "无法读取 ~/.ssh/config：" + err.Error()
		return view
	}
	view.Path = target
	content, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		view.Error = "无法读取 ~/.ssh/config：" + err.Error()
		return view
	}
	view.Present = entryHasInclude(content, entryIncludeLines(paths))
	if !view.Present {
		view.Token = entryIncludeToken(content, view.Line)
	}
	return view
}

var errEntryIncludeChanged = errors.New("~/.ssh/config 在确认后又有变化：请重新确认")

// entryIncludeApply adds the line at the top of ~/.ssh/config, so Super
// Cage's values win over the owner's Host * defaults. It needs the token of
// the preview the owner confirmed, keeps a backup of the previous content,
// keeps the file's mode and line endings, and does nothing when the line is
// already there.
func entryIncludeApply(paths entryPaths, token string) (entryInclude, error) {
	view := entryIncludePreview(paths)
	if view.Error != "" {
		return view, errors.New(view.Error)
	}
	if view.Present {
		return view, nil
	}
	if token == "" || token != view.Token {
		return view, errEntryIncludeChanged
	}
	content, err := os.ReadFile(view.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return view, err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(view.Path); err == nil {
		mode = info.Mode().Perm()
		view.Backup = view.Path + entryBackupSuffix
		if err := entryWriteFile(view.Backup, content, 0o600); err != nil {
			return view, err
		}
	} else if err := os.MkdirAll(filepath.Dir(view.Path), 0o700); err != nil {
		return view, err
	}
	newline := "\n"
	if bytes.Contains(content, []byte("\r\n")) {
		newline = "\r\n"
	}
	updated := append([]byte(view.Line+newline), content...)
	if err := entryWriteFile(view.Path, updated, mode); err != nil {
		return view, err
	}
	view.Present, view.Token = true, ""
	return view, nil
}

// entryIncludeRemove takes Super Cage's Include line out again, keeping a
// backup; the rest of the file stays byte for byte.
func entryIncludeRemove(paths entryPaths) error {
	target, err := entryUserConfigTarget(paths)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := entryIncludeLines(paths)
	if !entryHasInclude(content, lines) {
		return nil
	}
	var kept []byte
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		if slices.Contains(lines, strings.TrimSpace(string(line))) {
			continue
		}
		kept = append(kept, line...)
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if err := entryWriteFile(target+entryBackupSuffix, content, 0o600); err != nil {
		return err
	}
	return entryWriteFile(target, kept, info.Mode().Perm())
}
