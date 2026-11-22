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
	osuser "os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/attach"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/entryproxy"
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
	entryDeviceKey    = entryproxy.DeviceKey
	entryDeviceRecord = entryproxy.DeviceRecord
	entryHostKey      = "host_ed25519"
	entryClientDir    = entryproxy.ClientDir
	entryRevokedFile  = "revoked.json"
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
	// SystemConfig is the system-wide ssh client configuration, read (never
	// written) to keep a generated alias off a name it already uses; empty
	// in tests.
	SystemConfig string
}

func defaultEntryPaths(profile, executable string) (entryPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return entryPaths{}, errors.New("无法确定用户目录")
	}
	// The proxy finds the device key in the same place (entryproxy).
	device, err := entryproxy.DeviceDir()
	if err != nil {
		return entryPaths{}, err
	}
	system := "/etc/ssh/ssh_config"
	if runtime.GOOS == "windows" {
		system = filepath.Join(os.Getenv("ProgramData"), "ssh", "ssh_config")
	}
	return entryPaths{
		SystemConfig: system,
		SSHDir:       filepath.Join(home, ".ssh"),
		DeviceDir:    device,
		ComputerDir:  entryComputerProfile(profile),
		HomeSSH:      true,
		Proxy: func(environment string) string {
			return entryProxyCommand(executable, environment)
		},
		Cage: executable,
	}, nil
}

// entryProxyCommand is the ProxyCommand of a Host block: the slim
// cage-attach beside cage when there is one, which keeps each ssh
// connection's resident proxy small, else cage. ssh takes the rest of the
// line as it is, expands its % tokens and hands it to the user's shell (on
// Windows it starts the command line directly), so the executable is quoted
// for that and its % doubled.
func entryProxyCommand(executable, environment string) string {
	if slim := attach.Beside(executable); slim != "" {
		executable = slim
	}
	quoted := shellescape.Quote(executable)
	if runtime.GOOS == "windows" {
		quoted = "\"" + executable + "\""
	}
	return strings.ReplaceAll(quoted, "%", "%%") + " entry proxy " + environment
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
// from the default profile's configuration path, as the proxy does.
func entryProfileConfig(defaultConfig, environment string) (string, error) {
	return entryproxy.ProfileConfig(defaultConfig, environment)
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
// ssh_config reads a backslash before a quote or another backslash as an
// escape, so Windows paths are written with forward slashes, which Windows'
// OpenSSH accepts.
func entryConfigPath(path string) string {
	if runtime.GOOS == "windows" {
		path = filepath.ToSlash(path)
	}
	return entryQuote(strings.ReplaceAll(path, "%", "%%"))
}

func validEntryConfigValue(value string) bool {
	return !strings.ContainsAny(value, "\"\r\n\x00")
}

// entryHostAlias is "cage-<slug>" from the environment's name, unique among
// the Host blocks already written on this computer and never a name the
// owner's own ssh configuration already uses (F6 of the review): Super
// Cage's Include comes first, so its block would take that host over.
func entryHostAlias(paths entryPaths, environment, name string) (string, error) {
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
	suffix := environment
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	taken := entryAliasesInUse(paths, environment)
	owner := entryReadOwner(paths)
	candidates := []string{"cage-" + base, "cage-" + base + "-" + suffix, "super-cage-" + base}
	if owner.unknown {
		// N1 of the second review: names the owner's configuration may use
		// cannot be read, so only the form with the environment's id.
		candidates = nil
	}
	candidates = append(candidates, "super-cage-"+base+"-"+environment)
	for n := 2; n <= 9; n++ {
		candidates = append(candidates, fmt.Sprintf("super-cage-%s-%s-%d", base, environment, n))
	}
	for _, alias := range candidates {
		if !slices.Contains(taken, alias) && !entryClaimed(owner.rules, alias) {
			return alias, nil
		}
	}
	return "", fmt.Errorf("~/.ssh/config 里的主机名规则（Host 或 Match）已经包括 %s 这样的名字：请改一改那条规则，或换一个环境名称", candidates[0])
}

// entryChooseAlias keeps the environment's alias unless the owner's own
// configuration has come to use it since, and picks a new one then.
func entryChooseAlias(paths entryPaths, environment, name string) (string, error) {
	if alias := entryReadAlias(paths.hostFile(environment)); alias != "" && entryAliasSafe(entryReadOwner(paths), alias, environment) {
		return alias, nil
	}
	return entryHostAlias(paths, environment, name)
}

// entryAliasSafe: the owner's configuration does not use alias, or, when
// what it uses cannot be read, alias has the form with the environment's
// id ("super-cage-<name>-<id>", "-<n>" after it).
func entryAliasSafe(owner entryOwnerConfig, alias, environment string) bool {
	if entryClaimed(owner.rules, alias) {
		return false
	}
	if !owner.unknown {
		return true
	}
	trimmed := alias
	if n := len(trimmed); n > 2 && trimmed[n-2] == '-' && trimmed[n-1] >= '2' && trimmed[n-1] <= '9' {
		trimmed = trimmed[:n-2]
	}
	return strings.HasPrefix(alias, "super-cage-") && strings.HasSuffix(trimmed, "-"+environment)
}

// entryOwnerConfig is what Super Cage reads of the owner's own ssh client
// configuration: every Host line and every "Match host" or "Match
// originalhost" list in ~/.ssh/config, the system-wide file and the files
// they include, leaving out Super Cage's own files. unknown: an Include
// names files Super Cage cannot tell with certainty (see
// entryIncludePattern), or a file it cannot read safely; the owner's names
// are then unknown.
type entryOwnerConfig struct {
	rules   [][]string
	unknown bool
}

// entryReadOwner reads the owner's configuration. It never writes, runs
// nothing and opens only regular files.
func entryReadOwner(paths entryPaths) entryOwnerConfig {
	home := filepath.Dir(paths.SSHDir)
	var owner entryOwnerConfig
	seen := map[string]bool{}
	var read func(path string, user bool, depth int)
	read = func(path string, user bool, depth int) {
		if seen[path] || entryWithin(paths.clientDir(), path) {
			return
		}
		if depth > 16 {
			owner.unknown = true
			return
		}
		seen[path] = true
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) || err == nil && info.IsDir() {
			return
		}
		// A FIFO or a device would block or never end; ssh would read it.
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			owner.unknown = true
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			owner.unknown = true
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			keyword, args := entryConfigLine(line)
			switch strings.ToLower(keyword) {
			case "host":
				owner.rules = append(owner.rules, args)
			case "match":
				owner.rules = append(owner.rules, entryMatchHosts(args)...)
			case "include":
				base := paths.SSHDir
				if !user {
					base = filepath.Dir(paths.SystemConfig)
				}
				for _, arg := range args {
					pattern, ok := entryIncludePattern(arg, home, base, user)
					if !ok {
						owner.unknown = true
						continue
					}
					matches, err := filepath.Glob(pattern)
					if err != nil {
						owner.unknown = true
					}
					for _, match := range matches {
						read(match, user, depth+1)
					}
				}
			}
		}
	}
	read(paths.userConfig(), true, 0)
	if paths.SystemConfig != "" {
		read(paths.SystemConfig, false, 0)
	}
	return owner
}

// entryIncludePattern is the glob pattern ssh makes of an Include argument
// (ssh_config(5), Include, TOKENS and ENVIRONMENT VARIABLES; OpenSSH 9.9
// added the expansion, older releases take the argument as it is): ${HOME}
// and the tokens that do not depend on the host being connected to, then
// a relative path in ~/.ssh (/etc/ssh for the system file), then ~ and
// ~/ as the user's home. ok is false for anything that cannot be expanded
// with certainty: any other variable (ssh reads it from the environment of
// the shell it runs in, which a controller started from the Dock or at
// login does not share); %C, %h, %j, %k, %n, %p and %r name the
// destination, %L and %l the local host name, which can change with the
// network; another user's home; %i and %u on Windows; ~ in the system
// file, which ssh refuses.
func entryIncludePattern(arg, home, base string, user bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(arg); i++ {
		switch c := arg[i]; {
		case c == '$' && i+1 < len(arg) && arg[i+1] == '{':
			end := strings.IndexByte(arg[i+2:], '}')
			if end < 0 {
				return "", false
			}
			value, ok := os.LookupEnv(arg[i+2 : i+2+end])
			if !ok || arg[i+2:i+2+end] != "HOME" {
				return "", false
			}
			b.WriteString(value)
			i += 2 + end
		case c == '%':
			if i+1 == len(arg) {
				return "", false
			}
			i++
			switch arg[i] {
			case '%':
				b.WriteByte('%')
			case 'd':
				b.WriteString(home)
			case 'i':
				if runtime.GOOS == "windows" {
					return "", false
				}
				b.WriteString(strconv.Itoa(os.Getuid()))
			case 'u':
				current, err := osuser.Current()
				if runtime.GOOS == "windows" || err != nil {
					return "", false
				}
				b.WriteString(current.Username)
			default:
				return "", false
			}
		default:
			b.WriteByte(c)
		}
	}
	pattern := b.String()
	if !strings.HasPrefix(pattern, "~") && !filepath.IsAbs(pattern) {
		pattern = filepath.Join(base, pattern)
	}
	if strings.HasPrefix(pattern, "~") {
		if !user {
			return "", false
		}
		switch {
		case pattern == "~":
			pattern = home
		case strings.HasPrefix(pattern, "~/"):
			pattern = filepath.Join(home, pattern[2:])
		default:
			return "", false
		}
	}
	return pattern, true
}

func entryWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// entryConfigLine splits an ssh_config line into its keyword and arguments
// the way ssh reads it: "Keyword args", "Keyword=args", double quotes
// grouping, a # starting a word ending the line.
func entryConfigLine(line string) (string, []string) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
	if line == "" || line[0] == '#' {
		return "", nil
	}
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		return line, nil
	}
	keyword, rest := line[:end], strings.TrimLeft(line[end:], " \t")
	rest = strings.TrimLeft(strings.TrimPrefix(rest, "="), " \t")
	var args []string
	for rest != "" {
		if rest[0] == '#' {
			break
		}
		var word string
		if rest[0] == '"' {
			closing := strings.IndexByte(rest[1:], '"')
			if closing < 0 {
				return keyword, nil
			}
			word, rest = rest[1:1+closing], rest[2+closing:]
		} else if space := strings.IndexAny(rest, " \t"); space >= 0 {
			word, rest = rest[:space], rest[space:]
		} else {
			word, rest = rest, ""
		}
		args = append(args, word)
		rest = strings.TrimLeft(rest, " \t")
	}
	return keyword, args
}

// entryMatchHosts are the pattern lists of a Match line's host and
// originalhost criteria; a negated criterion claims nothing.
func entryMatchHosts(args []string) [][]string {
	var rules [][]string
	for i := 0; i < len(args); i++ {
		switch strings.ToLower(args[i]) {
		case "all", "canonical", "final":
		case "host", "originalhost":
			if i+1 < len(args) {
				rules = append(rules, strings.Split(args[i+1], ","))
			}
			i++
		default:
			i++ // every other criterion takes one argument
		}
	}
	return rules
}

// entryClaimed reports whether the owner's configuration names alias: a
// Host or Match host list with a pattern that matches it and has a
// character of its own (so "Host *" claims nothing), and no negated
// pattern that matches it.
func entryClaimed(rules [][]string, alias string) bool {
	for _, patterns := range rules {
		claims, excluded := false, false
		for _, pattern := range patterns {
			negated := strings.HasPrefix(pattern, "!")
			pattern = strings.TrimPrefix(pattern, "!")
			if !entryWildcard(strings.ToLower(pattern), strings.ToLower(alias)) {
				continue
			}
			if negated {
				excluded = true
			} else if strings.Trim(pattern, "*?") != "" {
				claims = true
			}
		}
		if claims && !excluded {
			return true
		}
	}
	return false
}

// entryWildcard is ssh's pattern match: * any run of characters, ? one.
func entryWildcard(pattern, name string) bool {
	p, n, star, mark := 0, 0, -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case star >= 0:
			p, mark = star+1, mark+1
			n = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
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
	options := [][2]string{
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
		// The owner's Host * defaults must not reach this host either (F8 of
		// the review): no shared master connection, the device key and
		// nothing else, no other known_hosts, no forking or session type
		// the owner set for other hosts. The command line still wins.
		{"ControlMaster", "no"},
		{"ControlPath", "none"},
		{"ControlPersist", "no"},
		{"PubkeyAuthentication", "yes"},
		{"PreferredAuthentications", "publickey"},
		{"PasswordAuthentication", "no"},
		{"KbdInteractiveAuthentication", "no"},
		{"ProxyUseFdpass", "no"},
		{"GlobalKnownHostsFile", entryConfigPath(os.DevNull)},
		{"CanonicalizeHostname", "no"},
		{"AddKeysToAgent", "no"},
		{"ServerAliveInterval", "15"},
		{"ServerAliveCountMax", "4"},
		// A refused dial prints one Chinese line from the proxy; ssh's own
		// "Connection closed by UNKNOWN port 65535" after it only confuses.
		// ssh -v still raises the level, since the command line wins.
		{"LogLevel", "ERROR"},
		// The proxy reaches the controller, not a host DNS could vouch for.
		{"VerifyHostKeyDNS", "no"},
	}
	for _, option := range options {
		fmt.Fprintf(&b, "  %s %s\n", option[0], option[1])
	}
	return b.String()
}

// entryBaselineOptions are the only options a file the owner's ssh reads
// carries (N2 of the second review): an option that ssh does not know makes
// every ssh the owner runs fail, and Super Cage cannot see every ssh on
// the computer (Git for Windows', MacPorts', Nix's, a GUI-started
// controller's PATH). The baseline is OpenSSH 7.6 (2017, Ubuntu 18.04's;
// Windows 10 ships 7.7 and 8.1): every name is in the keyword table of
// readconf.c at V_7_6_P1, and each value it is written with is one 7.6
// accepts (ControlPath and RemoteCommand none clear the option in 7.6,
// IdentityAgent none is read by ssh.c). The value is the release whose
// notes add the option, or "≤7.6" for one older than the notes say.
var entryBaselineOptions = map[string]string{
	"HostName": "≤7.6", "User": "≤7.6", "ProxyCommand": "≤7.6", "HostKeyAlias": "2.5",
	"UserKnownHostsFile": "≤7.6", "StrictHostKeyChecking": "≤7.6", "UpdateHostKeys": "6.8",
	"IdentityFile": "≤7.6", "IdentitiesOnly": "3.9", "IdentityAgent": "7.3",
	"ForwardAgent": "≤7.6", "ForwardX11": "≤7.6", "ForwardX11Trusted": "3.8",
	"ClearAllForwardings": "3.0", "Tunnel": "≤7.6", "PermitLocalCommand": "≤7.6",
	"RemoteCommand": "7.6", "RequestTTY": "5.9", "ControlMaster": "3.9", "ControlPath": "3.9",
	"ControlPersist": "5.6", "PubkeyAuthentication": "2.5", "PreferredAuthentications": "2.5",
	"PasswordAuthentication": "≤7.6", "KbdInteractiveAuthentication": "≤7.6",
	"ProxyUseFdpass": "6.5", "GlobalKnownHostsFile": "≤7.6", "CanonicalizeHostname": "≤7.6",
	"AddKeysToAgent": "7.2", "ServerAliveInterval": "3.8", "ServerAliveCountMax": "≤7.6",
	"LogLevel": "≤7.6", "VerifyHostKeyDNS": "≤7.6",
}

// entryNewerOptions are the pins OpenSSH 7.6 does not know, with the
// release that added them. Only cage ssh applies them, through its own -F
// file (entryCageSSHConfig), which ssh reads instead of ~/.ssh/config and
// the system file, so its IgnoreUnknown replaces nobody's. A file the
// owner's ssh reads never carries them: IgnoreUnknown would hide them from
// an older ssh, but the first IgnoreUnknown ssh reads wins, and Super
// Cage's Include comes first, so it would silently replace one the owner
// adds later (nothing of Super Cage runs when the owner edits the file),
// and an option of theirs that their ssh does not know would then break
// every ssh again.
var entryNewerOptions = map[string]string{
	"KnownHostsCommand":       "none",    // 8.5
	"SessionType":             "default", // 8.7
	"StdinNull":               "no",      // 8.7
	"ForkAfterAuthentication": "no",      // 8.7
	"ObscureKeystrokeTiming":  "no",      // 9.5
}

var entryNewerOptionOrder = []string{"KnownHostsCommand", "SessionType", "StdinNull", "ForkAfterAuthentication", "ObscureKeystrokeTiming"}

// cageSSHFile is the configuration cage ssh gives ssh with -F.
func (p entryPaths) cageSSHFile() string { return filepath.Join(p.clientDir(), "cage-ssh.conf") }

// entryCageSSHConfig is cage ssh's own configuration: the environments'
// blocks and the newer pins, hidden from an ssh that does not know them.
// The command line (-o) could not carry them: ssh refuses -N after
// -o SessionType.
func entryCageSSHConfig(paths entryPaths) string {
	var b strings.Builder
	b.WriteString("# Super Cage 本机终端（cage ssh）— generated, do not edit.\n")
	fmt.Fprintf(&b, "IgnoreUnknown %s\n", strings.Join(entryNewerOptionOrder, ","))
	fmt.Fprintf(&b, "Include %s\n", entryQuote(filepath.Join(paths.hostsDir(), "*.conf")))
	b.WriteString("Host *\n")
	for _, option := range entryNewerOptionOrder {
		fmt.Fprintf(&b, "  %s %s\n", option, entryNewerOptions[option])
	}
	return b.String()
}

// entryWriteClient writes this environment's Host block and pinned host key,
// and Super Cage's own config file that includes every block. Each
// environment owns its own two files, so two controllers never write the
// same one.
func entryWriteClient(paths entryPaths, environment, name string, hostKey ssh.PublicKey) (string, error) {
	for _, value := range []string{paths.clientFile(), paths.deviceKey(), paths.knownHosts(environment)} {
		if !validEntryConfigValue(value) {
			return "", errors.New("路径里有 ssh 配置无法表示的字符")
		}
	}
	if strings.ContainsAny(paths.Proxy(environment), "\r\n\x00") || strings.ContainsAny(paths.Cage, "\"\r\n\x00") {
		return "", errors.New("cage 命令的路径里有 ssh 配置无法表示的字符")
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
	if err := entryWriteFile(paths.cageSSHFile(), []byte(entryCageSSHConfig(paths)), 0o600); err != nil {
		return "", err
	}
	alias, err := entryChooseAlias(paths, environment, name)
	if err != nil {
		return "", err
	}
	if err := entryPinHostKey(paths, environment, hostKey); err != nil {
		return "", err
	}
	return alias, entryWriteFile(paths.hostFile(environment), []byte(entryHostBlock(paths, environment, name, alias)), 0o600)
}

// entryPinHostKey writes the environment's known_hosts line. Only turning
// the entry on writes it, from the controller's own key (F2 of the review):
// the proxy compares and never rewrites it.
// entryPinnedHostKey reads the key entryPinHostKey wrote.
func entryPinnedHostKey(paths entryPaths, environment string) (ssh.PublicKey, error) {
	data, err := os.ReadFile(paths.knownHosts(environment))
	if err != nil {
		return nil, err
	}
	_, _, key, _, _, err := ssh.ParseKnownHosts(data)
	return key, err
}

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
	for _, path := range []string{paths.clientFile(), paths.cageSSHFile(), paths.hostsDir(), paths.clientDir()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirNotEmpty(err) {
			return err
		}
	}
	return nil
}

// entryBackupPaths are the backups of ~/.ssh/config: the first one, from
// before Super Cage changed it, and the latest one.
func entryBackupPaths(paths entryPaths) (first, latest string) {
	dir := filepath.Join(paths.DeviceDir, "ssh-config-backups")
	return filepath.Join(dir, "config.first"), filepath.Join(dir, "config.latest")
}

// entryBackup keeps a copy of ~/.ssh/config before Super Cage changes it,
// in Super Cage's own directory (never beside the file a symlink points
// to): the first one is kept for good, the latest one replaced each time.
func entryBackup(paths entryPaths, content []byte) (string, error) {
	first, latest := entryBackupPaths(paths)
	if err := host.EnsurePrivateDir(paths.DeviceDir); err != nil {
		return "", err
	}
	if err := host.EnsurePrivateDir(filepath.Dir(first)); err != nil {
		return "", err
	}
	if _, err := os.Lstat(first); errors.Is(err, os.ErrNotExist) {
		if err := entryWriteFile(first, content, 0o600); err != nil {
			return "", err
		}
	}
	return latest, entryWriteFile(latest, content, 0o600)
}

// entryIncludePreviewFor is the preview with this environment's alias and
// the whole Host block the line brings in, as the confirmation shows them.
func entryIncludePreviewFor(paths entryPaths, environment, name string) entryInclude {
	view := entryIncludePreview(paths)
	alias, err := entryChooseAlias(paths, environment, name)
	if err != nil {
		if view.Error == "" {
			view.Error = err.Error()
		}
		return view
	}
	view.Alias = alias
	view.Block = entryHostBlock(paths, environment, name, view.Alias)
	if data, err := os.ReadFile(paths.hostFile(environment)); err == nil && entryReadAlias(paths.hostFile(environment)) == view.Alias {
		view.Block = string(data)
	}
	return view
}

// entrySweepOrphans removes the blocks of local VMs whose profile is gone
// (F10 of the review), and with the last block Super Cage's own files and
// the Include line. The app has no way to delete a local VM yet; this runs
// when the entry is turned on or off and when the proxy meets one, so a VM
// deleted by hand or by a later version does not leave a host behind.
func entrySweepOrphans(paths entryPaths) error {
	matches, _ := filepath.Glob(filepath.Join(paths.hostsDir(), "*.conf"))
	var failures []error
	for _, match := range matches {
		environment := strings.TrimSuffix(filepath.Base(match), ".conf")
		if environment == defaultLocalVMID || !validLocalVMID(environment) {
			continue
		}
		if _, err := os.Stat(filepath.Join(paths.ComputerDir, cage.LocalVMDir, environment)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		failures = append(failures, entryRemoveClient(paths, environment))
	}
	return errors.Join(failures...)
}

// entryRevoked is the computer-wide list of revoked device keys: a key
// revoked in one environment is refused in every environment (F10).
type entryRevoked struct {
	Version int      `json:"version"`
	Keys    []string `json:"keys"`
}

// entryLoadRevoked reads revoked.json. One that exists but cannot be read
// (or names a version this release does not know) is an error: the caller
// refuses every key until it can be read, and never writes it again from
// an empty list (N4 of the second review).
func entryLoadRevoked(paths entryPaths) (entryRevoked, error) {
	path := filepath.Join(paths.DeviceDir, entryRevokedFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return entryRevoked{Version: 1}, nil
	}
	var revoked entryRevoked
	if err != nil || json.Unmarshal(data, &revoked) != nil || revoked.Version != 1 {
		return entryRevoked{}, errEntryRevocationsUnreadable
	}
	return revoked, nil
}

// entryKeyRevoked is true for a revoked key, and for every key while
// revoked.json cannot be read.
func entryKeyRevoked(paths entryPaths, key ssh.PublicKey) bool {
	revoked, err := entryLoadRevoked(paths)
	return err != nil || slices.Contains(revoked.Keys, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
}

// entryRevokeKey adds a device key, in authorized_keys form, to the list.
func entryRevokeKey(paths entryPaths, authorized string) error {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return err
	}
	revoked, err := entryLoadRevoked(paths)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if slices.Contains(revoked.Keys, line) {
		return nil
	}
	revoked.Keys = append(revoked.Keys, line)
	if err := host.EnsurePrivateDir(paths.DeviceDir); err != nil {
		return err
	}
	return writePrivate(filepath.Join(paths.DeviceDir, entryRevokedFile), revoked)
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
	// Alias and Block are this environment's generated Host block, shown in
	// the confirmation with the line that includes it.
	Alias string `json:"alias,omitempty"`
	Block string `json:"block,omitempty"`
	// Warning is something the owner should know before confirming.
	Warning string `json:"warning,omitempty"`
}

// entryCurrentUID is the user that may own ~/.ssh/config; a variable for
// tests.
var entryCurrentUID = os.Getuid

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
	for _, line := range strings.Split(strings.TrimPrefix(string(content), entryBOM), "\n") {
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
	if info, err := os.Stat(target); err == nil && !view.Present {
		if refusal := entryFileRefusal(target, info); refusal != nil {
			view.Error = fmt.Sprintf("%s：要直接用 ssh 连接，请自己在它的第一行加入 %s", refusal.Error(), view.Line)
			return view
		}
	}
	if bytes.HasPrefix(content, []byte(entryBOM)) {
		view.Warning = "~/.ssh/config 以 BOM（字节顺序标记）开头，多数 OpenSSH 会把带 BOM 的第一行读成未知选项而报错；Super Cage 的这一行会写在 BOM 之后。建议先把文件另存为不带 BOM 的 UTF-8。"
	}
	owner := entryReadOwner(paths)
	blocks, _ := filepath.Glob(filepath.Join(paths.hostsDir(), "*.conf"))
	for _, block := range blocks {
		alias := entryReadAlias(block)
		if alias == "" || entryAliasSafe(owner, alias, strings.TrimSuffix(filepath.Base(block), ".conf")) {
			continue
		}
		if entryClaimed(owner.rules, alias) {
			view.Error = fmt.Sprintf("~/.ssh/config 里已经有主机 %s：在 Super Cage 里关闭再开启本机终端，换一个名字后再加入", alias)
		} else {
			view.Error = fmt.Sprintf("~/.ssh/config 引入的文件有 Super Cage 无法确定的路径，可能已经用了主机名 %s：在 Super Cage 里关闭再开启本机终端，换成带环境标识的名字后再加入", alias)
		}
		return view
	}
	if !view.Present {
		view.Token = entryIncludeToken(content, view.Line)
	}
	return view
}

// errEntryConfigReadOnly and errEntryConfigNotOwned: a ~/.ssh/config (or
// the file it links to, a Nix or dotfiles store, say) that this user cannot
// write or does not own is never replaced, though its directory would allow
// it (entryFileRefusal).
var (
	errEntryConfigReadOnly = errors.New("~/.ssh/config 不可写（只读、带不可更改标记或在只读的文件系统上），Super Cage 不会修改它")
	errEntryConfigNotOwned = errors.New("~/.ssh/config 属于另一个用户，Super Cage 不会修改它")
)

// entryBOM is the UTF-8 byte order mark some editors write; it stays the
// first thing in the file.
const entryBOM = "\ufeff"

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
		if view.Backup, err = entryBackup(paths, content); err != nil {
			return view, err
		}
	} else if err := os.MkdirAll(filepath.Dir(view.Path), 0o700); err != nil {
		return view, err
	}
	newline := "\n"
	if bytes.Contains(content, []byte("\r\n")) {
		newline = "\r\n"
	}
	bom := ""
	if bytes.HasPrefix(content, []byte(entryBOM)) {
		bom, content = entryBOM, content[len(entryBOM):]
	}
	updated := append([]byte(bom+view.Line+newline), content...)
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
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if refusal := entryFileRefusal(target, info); refusal != nil {
		return fmt.Errorf("%w：请自己删掉其中的 %s", refusal, paths.includeLine())
	}
	var kept []byte
	if bytes.HasPrefix(content, []byte(entryBOM)) {
		kept = []byte(entryBOM)
	}
	for _, line := range bytes.SplitAfter(bytes.TrimPrefix(content, []byte(entryBOM)), []byte("\n")) {
		if slices.Contains(lines, strings.TrimSpace(string(line))) {
			continue
		}
		kept = append(kept, line...)
	}
	if _, err := entryBackup(paths, content); err != nil {
		return err
	}
	return entryWriteFile(target, kept, info.Mode().Perm())
}
