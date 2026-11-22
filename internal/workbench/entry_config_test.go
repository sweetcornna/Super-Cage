//go:build !windows

package workbench

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func entryTestPaths(t *testing.T) entryPaths {
	t.Helper()
	entryRealHomeGuard(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// "Application Support" style spaces and a % that ssh would expand.
	return entryPaths{
		SSHDir:      filepath.Join(dir, "home", ".ssh"),
		DeviceDir:   filepath.Join(dir, "Library", "Application Support", "Super Cage 100%", "entry"),
		ComputerDir: filepath.Join(dir, "profile"),
		Proxy: func(environment string) string {
			return entryProxyCommand("/Applications/Super Cage.app/cage", environment)
		},
		Cage: "/Applications/Super Cage.app/cage",
	}
}

func entryMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// sshG is what the system ssh makes of a configuration for a host.
func sshG(t *testing.T, config, host string) map[string]string {
	t.Helper()
	cmd := exec.Command(sshExecutable(), "-G", "-F", config, host)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Dir(filepath.Dir(config))}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v %s", err, out)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if name, value, ok := strings.Cut(line, " "); ok {
			if values[name] != "" {
				values[name] += "\n"
			}
			values[name] += value
		}
	}
	return values
}

// Design 7.1 row 18: the device key is the computer's alone, every file the
// entry writes is private, and the system ssh reads the generated block as
// meant, also from a path with spaces and %.
func TestEntryClientFilesModesAndBlock(t *testing.T) {
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH ssh is required")
	}
	paths := entryTestPaths(t)
	if _, ok, err := entryReadComputer(paths); ok || err != nil {
		t.Fatal("reading created a device", err)
	}
	if _, err := os.Stat(paths.DeviceDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reading created the device directory")
	}
	computer, err := entryEnsureComputer(paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := entryEnsureComputer(paths, false); err != nil || again.Public != computer.Public || again.ID != computer.ID {
		t.Fatal("a second ensure changed the key", err)
	}
	for path, mode := range map[string]os.FileMode{paths.DeviceDir: 0o700, paths.deviceKey(): 0o600, paths.deviceKey() + ".pub": 0o600, filepath.Join(paths.DeviceDir, entryDeviceRecord): 0o600} {
		if got := entryMode(t, path); got != mode {
			t.Fatalf("%s: %v, want %v", path, got, mode)
		}
	}
	data, _ := os.ReadFile(paths.deviceKey())
	key, err := ssh.ParsePrivateKey(data)
	if err != nil || key.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatal("device key", err)
	}
	profile := paths.ComputerDir
	host, err := entryHostSigner(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryMode(t, filepath.Join(entryDir(profile), entryHostKey)); got != 0o600 {
		t.Fatalf("host key mode %v", got)
	}
	if err := entrySaveState(profile, entryState{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := entryMode(t, filepath.Join(entryDir(profile), "state.json")); got != 0o600 {
		t.Fatalf("state mode %v", got)
	}
	alias, err := entryWriteClient(paths, defaultLocalVMID, "默认环境", host.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if alias != "cage-default" {
		t.Fatalf("alias %q", alias)
	}
	for path, mode := range map[string]os.FileMode{paths.clientDir(): 0o700, paths.hostsDir(): 0o700, paths.clientFile(): 0o600, paths.hostFile(defaultLocalVMID): 0o600, paths.knownHosts(defaultLocalVMID): 0o600} {
		if got := entryMode(t, path); got != mode {
			t.Fatalf("%s: %v, want %v", path, got, mode)
		}
	}
	if _, err := os.Stat(paths.userConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("turning on wrote ~/.ssh/config")
	}
	got := sshG(t, paths.clientFile(), "cage-default")
	for name, want := range map[string]string{
		"hostname":              "cage-default.invalid",
		"user":                  "cage",
		"proxycommand":          "'/Applications/Super Cage.app/cage' entry proxy default",
		"hostkeyalias":          "cage-default",
		"userknownhostsfile":    paths.knownHosts(defaultLocalVMID),
		"stricthostkeychecking": "true",
		"updatehostkeys":        "false",
		"identityfile":          strings.ReplaceAll(paths.deviceKey(), "%", "%%"), // ssh expands it when it connects
		"identitiesonly":        "yes",
		"identityagent":         "none",
		"forwardagent":          "no",
		"forwardx11":            "no",
		"forwardx11trusted":     "no",
		"clearallforwardings":   "yes",
		"tunnel":                "false",
		"permitlocalcommand":    "no",
		"requesttty":            "auto",
		"verifyhostkeydns":      "false",
		"serveraliveinterval":   "15",
		"serveralivecountmax":   "4",
		"loglevel":              "ERROR",
	} {
		if got[name] != want {
			t.Fatalf("ssh -G %s = %q, want %q", name, got[name], want)
		}
	}
	if got["sendenv"] != "" || got["setenv"] != "" || got["remotecommand"] != "" {
		t.Fatalf("env or command: %q %q %q", got["sendenv"], got["setenv"], got["remotecommand"])
	}
	known, _ := os.ReadFile(paths.knownHosts(defaultLocalVMID))
	if string(known) != "cage-default "+string(ssh.MarshalAuthorizedKey(host.PublicKey())) {
		t.Fatalf("known_hosts %q", known)
	}
	// Rewriting keeps the alias the owner already uses.
	if again, err := entryWriteClient(paths, defaultLocalVMID, "renamed", host.PublicKey()); err != nil || again != "cage-default" {
		t.Fatal("alias changed", again, err)
	}
	// A new key replaces the old one; the private half stays user-only.
	rotated, err := entryEnsureComputer(paths, true)
	if err != nil || rotated.Public == computer.Public || rotated.ID != computer.ID || entryMode(t, paths.deviceKey()) != 0o600 {
		t.Fatal("rotation", err)
	}
}

func TestEntryHostAliases(t *testing.T) {
	paths := entryTestPaths(t)
	if got, _ := entryHostAlias(paths, defaultLocalVMID, "默认环境"); got != "cage-default" {
		t.Fatal(got)
	}
	if got, _ := entryHostAlias(paths, "0123456789abcdef0123456789abcdef", "My Box!  (GPU)"); got != "cage-my-box-gpu" {
		t.Fatal(got)
	}
	if got, _ := entryHostAlias(paths, "0123456789abcdef0123456789abcdef", "研究机"); got != "cage-01234567" {
		t.Fatal(got)
	}
	host, _ := entryHostSigner(paths.ComputerDir, true)
	if _, err := entryWriteClient(paths, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "work", host.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if got, _ := entryHostAlias(paths, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "Work"); got != "cage-work-bbbbbb" {
		t.Fatal(got)
	}
	if command, err := entrySSHCommand(paths, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"-t", "ls"}); err != nil || strings.Join(command, " ") != "ssh -F "+paths.cageSSHFile()+" cage-work -t ls" {
		t.Fatal(command, err)
	}
	if _, err := entrySSHCommand(paths, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil); err == nil {
		t.Fatal("cage ssh for an environment whose entry is off")
	}
}

// Design 5.3: the Include line goes in only with the confirmed preview's
// token, at the top, with a backup, keeping mode, line endings and a
// symlinked file; it is written once and removed byte for byte.
func TestEntryIncludeConfirmApplyRemove(t *testing.T) {
	for _, home := range []bool{false, true} {
		paths := entryTestPaths(t)
		paths.HomeSSH = home
		if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
			t.Fatal(err)
		}
		// ~/.ssh/config is a symlink into a dotfiles checkout, with CRLF.
		dotfiles := filepath.Join(filepath.Dir(paths.SSHDir), "dotfiles", "ssh_config")
		_ = os.MkdirAll(filepath.Dir(dotfiles), 0o700)
		original := []byte("Host *\r\n  ForwardAgent yes\r\n  SendEnv LANG\r\n")
		if err := os.WriteFile(dotfiles, original, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dotfiles, paths.userConfig()); err != nil {
			t.Fatal(err)
		}
		preview := entryIncludePreview(paths)
		wantLine := "Include " + paths.clientFile()
		if home {
			wantLine = "Include ~/.ssh/super-cage/config"
		}
		if preview.Present || preview.Token == "" || preview.Line != wantLine || preview.Path != dotfiles || preview.Error != "" {
			t.Fatalf("preview %+v", preview)
		}
		if b, _ := os.ReadFile(dotfiles); !bytes.Equal(b, original) {
			t.Fatal("the preview wrote")
		}
		if _, err := entryIncludeApply(paths, "not-the-token"); !errors.Is(err, errEntryIncludeChanged) {
			t.Fatal("applied without the confirmed token", err)
		}
		// The file changes between the preview and the confirmation.
		changed := append(append([]byte{}, original...), "Host extra\r\n"...)
		_ = os.WriteFile(dotfiles, changed, 0o640)
		if _, err := entryIncludeApply(paths, preview.Token); !errors.Is(err, errEntryIncludeChanged) {
			t.Fatal("applied over a change the owner did not see", err)
		}
		original = changed
		preview = entryIncludePreview(paths)
		applied, err := entryIncludeApply(paths, preview.Token)
		if _, latest := entryBackupPaths(paths); err != nil || !applied.Present || applied.Backup != latest {
			t.Fatalf("apply %+v %v", applied, err)
		}
		after, _ := os.ReadFile(dotfiles)
		if string(after) != wantLine+"\r\n"+string(original) {
			t.Fatalf("after %q", after)
		}
		if info, _ := os.Lstat(paths.userConfig()); info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the symlink was replaced")
		}
		if entryMode(t, dotfiles) != 0o640 || entryMode(t, applied.Backup) != 0o600 {
			t.Fatal("modes", entryMode(t, dotfiles), entryMode(t, applied.Backup))
		}
		if backup, _ := os.ReadFile(applied.Backup); !bytes.Equal(backup, original) {
			t.Fatal("backup")
		}
		if again, err := entryIncludeApply(paths, ""); err != nil || !again.Present {
			t.Fatal("not idempotent", err)
		}
		if again, _ := os.ReadFile(dotfiles); !bytes.Equal(again, after) {
			t.Fatal("a second apply changed the file")
		}
		// The owner's Host * comes after the Include, so the generated
		// block's values win.
		host, _ := entryHostSigner(paths.ComputerDir, true)
		if _, err := entryWriteClient(paths, defaultLocalVMID, "默认环境", host.PublicKey()); err != nil {
			t.Fatal(err)
		}
		if !home {
			if got := sshG(t, paths.userConfig(), "cage-default"); got["forwardagent"] != "no" || got["proxycommand"] == "" {
				t.Fatalf("with the owner's defaults: forwardagent=%q proxycommand=%q", got["forwardagent"], got["proxycommand"])
			}
		}
		// Off everywhere: the line goes, the rest stays byte for byte.
		if err := entryRemoveClient(paths, defaultLocalVMID); err != nil {
			t.Fatal(err)
		}
		if final, _ := os.ReadFile(dotfiles); !bytes.Equal(final, original) {
			t.Fatalf("after removal %q", final)
		}
		if _, err := os.Stat(paths.clientDir()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Super Cage's ssh directory stayed")
		}
	}
}

func TestEntryIncludeStaysWhileAnotherEnvironmentUsesIt(t *testing.T) {
	paths := entryTestPaths(t)
	host, _ := entryHostSigner(paths.ComputerDir, true)
	for _, environment := range []string{defaultLocalVMID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if _, err := entryWriteClient(paths, environment, environment, host.PublicKey()); err != nil {
			t.Fatal(err)
		}
	}
	preview := entryIncludePreview(paths)
	if _, err := entryIncludeApply(paths, preview.Token); err != nil {
		t.Fatal(err)
	}
	if err := entryRemoveClient(paths, defaultLocalVMID); err != nil {
		t.Fatal(err)
	}
	if !entryIncludePreview(paths).Present {
		t.Fatal("the Include went while another environment's block remains")
	}
	if err := entryRemoveClient(paths, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(paths.userConfig()); len(b) != 0 {
		t.Fatalf("left %q", b)
	}
}
