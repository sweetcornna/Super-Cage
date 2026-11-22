//go:build !windows

package workbench

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/termfilter"
)

// Tests for the findings of the security review of the local SSH entry
// (review3-ssh). Each names its finding.

// text is everything a pty session received so far.
func (s *entryShell) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.String()
}

// entryShellExit waits for a session to end and returns its exit status.
func entryShellExit(t *testing.T, s *entryShell) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.session.Wait() }()
	select {
	case err := <-done:
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			return exit.ExitStatus()
		}
		if err != nil {
			return -1
		}
		return 0
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
	}
	return -1
}

// F1, now that the escape filter is in: a session with a pty is a Super
// Cage terminal whose output goes through cage attach's filter. A clipboard
// read and the guest's own focus-report switch never reach the client; a
// title does. The system ssh with -tt gets the command's status.
func TestEntryTerminalSessionsAreFiltered(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	shell := entryOpenShell(t, f.client(), 100, 30, "printf 'A\\033]52;c;?\\007B\\033[?1004hC\\033]2;entry-title\\007D\\n'; exit 3")
	if status := entryShellExit(t, shell); status != 3 {
		t.Fatalf("status %d", status)
	}
	text := shell.text()
	if !strings.Contains(text, "ABC\x1b]2;entry-title\x07D\r\n") || strings.Contains(text, "\x1b]52;c;?") || strings.Count(text, entryFocusOn) != 1 {
		t.Fatalf("output %q", text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, stderr, code := entryCombined(f.systemSSH(ctx, f.paths.clientFile(), "-tt", "cage-default", "printf 'RAN=%s\\033]52;c;?\\007\\n' yes; exit 4"))
	if code != 4 || !strings.Contains(out, "RAN=yes") || strings.Contains(out, "\x1b]52;c;?") {
		t.Fatalf("ssh -tt: %d %q %q", code, out, stderr)
	}
}

// Design 5.7 at the end of a session: the title is saved before the first
// output and the local terminal is reset before the exit status, the way
// cage attach leaves a window.
func TestEntryTerminalSavesTitleAndResetsAtTheEnd(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	shell := entryOpenShell(t, f.client(), 100, 30, "printf '\\033[?1049h\\033[?1000hDONE\\n'; exit 3")
	if status := entryShellExit(t, shell); status != 3 {
		t.Fatalf("status %d", status)
	}
	text := shell.text()
	if !strings.HasPrefix(text, termfilter.TitlePush+entryFocusOn) || !strings.HasSuffix(text, entryExitReset) || !strings.Contains(text, "DONE") {
		t.Fatalf("output %q", text)
	}
}

// F4: a lock cuts the connection but keeps the terminal. The dial while
// locked says so; after unlock, a new interactive ssh -t offers to re-join.
func TestEntryLockKeepsTheTerminalAndOffersToRejoin(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	client, err := f.dial(f.device)
	if err != nil {
		t.Fatal(err)
	}
	shell := entryOpenShell(t, client, 100, 30, "")
	shell.send("export MARK=kept42; printf 'R=%s\\n' ready\n")
	shell.expect("R=")
	ended := make(chan struct{})
	go func() { _ = client.Wait(); close(ended) }()
	f.s.account.lock()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the lock did not cut the connection")
	}
	if text := shell.text(); !strings.Contains(text, entryExitReset) || !strings.Contains(text, "已锁定") {
		t.Fatalf("no reset or message before the cut: %q", text[max(0, len(text)-400):])
	}
	listed := entrySSHTerminals(f.s)
	if len(listed) != 1 || listed[0]["closed"] == true || listed[0]["origin"].(map[string]any)["kept"] != true {
		t.Fatalf("after the lock: %v", listed)
	}
	if _, err := f.dial(f.device); err == nil || entryClientError(err) != "Super Cage 已锁定，请在应用中解锁" {
		t.Fatalf("dial while locked: %v", err)
	}
	f.s.account.setKey(make([]byte, 32))
	again := entryOpenShell(t, f.client(), 100, 30, "")
	again.expect("输入编号重新接入")
	again.send("1\r")
	again.send("printf 'M=%s\\n' \"$MARK\"\n")
	if got := again.expect("M="); got != "kept42" {
		t.Fatalf("re-joined a different terminal: M=%q", got)
	}
	if listed := entrySSHTerminals(f.s); len(listed) != 1 {
		t.Fatalf("re-join opened another terminal: %v", listed)
	}
	// The first terminal has a viewer now, so nothing is offered and a new
	// terminal opens.
	fresh := entryOpenShell(t, f.client(), 100, 30, "")
	fresh.send("printf 'N=%s.\\n' \"$MARK\"\n")
	if got := fresh.expect("N="); got != "." {
		t.Fatalf("a second shell joined the first terminal: N=%q", got)
	}
}

// The reset reaches a terminal that never stops writing, on a lock and on
// quitting: the session stops reading output as soon as it is cut.
func TestEntryCutResetsABusyTerminal(t *testing.T) {
	for _, event := range []string{"lock", "quit"} {
		t.Run(event, func(t *testing.T) {
			f := newEntryFixture(t, true)
			f.on()
			client, err := f.dial(f.device)
			if err != nil {
				t.Fatal(err)
			}
			shell := entryOpenShell(t, client, 100, 30, "")
			shell.send("exec yes 0123456789abcdef0123456789abcdef\n")
			entryAwait(t, 5*time.Second, "steady output", func() bool { return strings.Count(shell.text(), "0123456789abcdef0123456789abcdef\r\n") > 200 })
			ended := make(chan struct{})
			go func() { _ = client.Wait(); close(ended) }()
			if event == "lock" {
				f.s.account.lock()
			} else {
				go func() { _ = f.s.closeService() }()
			}
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s did not end the connection", event)
			}
			entryAwait(t, 2*time.Second, "the reset", func() bool { return strings.Contains(shell.text(), entryExitReset) })
		})
	}
}

// F2: the proxy never rewrites the pinned host key. A key that differs from
// the pin is refused with a message that names the fix, before ssh runs into
// "REMOTE HOST IDENTIFICATION HAS CHANGED".
func TestEntryProxyRefusesAChangedHostKey(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	pinned, err := os.ReadFile(f.paths.knownHosts(defaultLocalVMID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(entryDir(f.s.profile), entryHostKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := entryHostSigner(f.s.profile, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, stderr, code := entryCombined(f.systemSSH(ctx, f.paths.clientFile(), "cage-default", "true"))
	if code != 255 || !strings.Contains(stderr, "主机钥匙") || !strings.Contains(stderr, "关闭再开启") {
		t.Fatalf("changed host key: %d %q", code, stderr)
	}
	if now, _ := os.ReadFile(f.paths.knownHosts(defaultLocalVMID)); !bytes.Equal(now, pinned) {
		t.Fatal("the proxy rewrote the pinned host key")
	}
	// Turning the entry on again pins the controller's new key.
	f.on()
	if out, stderr, code := entryCombined(f.systemSSH(ctx, f.paths.clientFile(), "cage-default", "echo fine")); code != 0 || out != "fine\n" {
		t.Fatalf("after on: %d %q %q", code, out, stderr)
	}
}

// F3 and F10: keeping a terminal answers only the trusted local view, and
// the entry's routes refuse the agent transport on the server too.
func TestEntryKeepAndEntryRoutesRefuseRemoteViewsAndAgents(t *testing.T) {
	s := testServer(t)
	call := func(method, path string, trusted, agent bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, s.origin+path, strings.NewReader("{}"))
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		if trusted {
			r.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		}
		if agent {
			r.Header.Set(agentRequestHeader, "1")
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/api/terminals/missing/keep", false, false); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "local_only") {
		t.Fatalf("keep without the trusted view: %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/terminals/missing/keep", true, false); w.Code != http.StatusNotFound {
		t.Fatalf("keep from the window: %d %s", w.Code, w.Body)
	}
	for _, route := range [][2]string{{"GET", "/api/entry"}, {"POST", "/api/entry/on"}, {"GET", "/api/entry/include"}, {"POST", "/api/entry/include"}, {"POST", "/api/entry/off"}, {"POST", "/api/entry/devices/x/revoke"}, {"GET", "/api/entry/ssh"}, {"POST", "/api/terminals/missing/keep"}} {
		if w := call(route[0], route[1], true, true); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "window_only") {
			t.Fatalf("%v from the agent transport: %d %s", route, w.Code, w.Body)
		}
	}
}

// F5: off and revoke never wait on work in the guest. An install of the run
// script that hangs in the guest holds up neither.
func TestEntryOffAndRevokeNeverWaitForAHungInstall(t *testing.T) {
	previous := entryWrapperTimeout
	entryWrapperTimeout = 3 * time.Second
	t.Cleanup(func() { entryWrapperTimeout = previous })
	f := newEntryFixture(t, true)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cancelled := make(chan time.Time, 4)
	f.s.entry.install = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			cancelled <- time.Now()
			return ctx.Err()
		case <-release:
			return errors.New("released")
		}
	}
	for _, action := range []string{"revoke", "off"} {
		f.on()
		client, err := f.dial(f.device)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			go func() {
				if session, err := client.NewSession(); err == nil {
					_ = session.Run("true")
				}
			}()
		}
		entryAwait(t, 5*time.Second, "three commands waiting for the install", func() bool { return f.s.entry.registry.counts().Exec >= 3 })
		ended := make(chan time.Time, 1)
		go func() { _ = client.Wait(); ended <- time.Now() }()
		path := "/api/entry/off"
		if action == "revoke" {
			path = "/api/entry/devices/" + f.computer.ID + "/revoke"
		}
		start := time.Now()
		if err := f.call("POST", path, map[string]any{}, nil); err != nil {
			t.Fatal(err)
		}
		answered := time.Since(start)
		select {
		case at := <-ended:
			t.Logf("%s with a hung install: answered after %v, connection cut after %v", action, answered, at.Sub(start))
			if answered > time.Second || at.Sub(start) > time.Second {
				t.Fatalf("%s waited for the install: %v / %v", action, answered, at.Sub(start))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s left the connection open", action)
		}
		if action == "off" {
			// Turning the entry off also stops the install in the guest.
			select {
			case at := <-cancelled:
				t.Logf("off stopped the install %v after the request", at.Sub(start))
			case <-time.After(time.Second):
				t.Fatal("off left the install running")
			}
		}
	}
}

// entrySecondEnvironment is a local VM's controller on the same computer as
// a: its profile is <default>/vms/<id> and it shares a's entry paths.
func entrySecondEnvironment(t *testing.T, a *entryFixture, wrap ...func(http.Handler) http.Handler) *entryFixture {
	t.Helper()
	id := "0123456789abcdef"
	dir := filepath.Join(a.s.profile, cage.LocalVMDir, id)
	if err := host.EnsurePrivateDir(filepath.Join(a.s.profile, cage.LocalVMDir)); err != nil {
		t.Fatal(err)
	}
	if err := host.EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	config, err := os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	s.profile, s.configPath = dir, filepath.Join(dir, "config.json")
	identity, err := currentController()
	if err != nil {
		t.Fatal(err)
	}
	s.identity, s.proofKey = identity, randomToken()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	port := server.Listener.Addr().(*net.TCPAddr).Port
	s.origin = "http://localhost:" + strconv.Itoa(port)
	server.Config.Handler = s.handler()
	for _, w := range wrap {
		server.Config.Handler = w(server.Config.Handler)
	}
	server.Start()
	t.Cleanup(server.Close)
	holdControllerLock(t, dir)
	if err := writePrivate(filepath.Join(dir, "control.json"), s.controlRecord(port)); err != nil {
		t.Fatal(err)
	}
	b := &entryFixture{t: t, s: s, dir: a.dir, paths: a.paths}
	s.entry.paths = &b.paths
	s.entry.runPath = a.s.entry.runPath
	s.entry.install = func(context.Context) error { return nil }
	entrySetRunning(t, s, true)
	t.Cleanup(func() { s.entry.registry.cutAll(errEntryClosing) })
	return b
}

// F10: revoking this computer's key in one environment applies in every
// environment at once: live connections elsewhere end, and every
// environment refuses the device until its entry is turned on again. The
// first one renews the key; the others take the renewed key as it is.
func TestEntryRevocationAppliesInEveryEnvironment(t *testing.T) {
	a := newEntryFixture(t, true)
	a.on()
	b := entrySecondEnvironment(t, a)
	b.on()
	client, err := b.dial(b.device)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { _ = client.Wait(); close(ended) }()
	if err := a.call("POST", "/api/entry/devices/"+a.computer.ID+"/revoke", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the other environment kept a connection of the revoked key")
	}
	if client, err := b.dial(b.device); err == nil {
		client.Close()
		t.Fatal("the other environment accepted the revoked key")
	} else if entryClientError(err) != errEntryRevoked.Message {
		t.Fatalf("the other environment: %v", err)
	}
	old := a.device.PublicKey().Marshal()
	a.on()
	if bytes.Equal(a.device.PublicKey().Marshal(), old) {
		t.Fatal("turning the entry on again kept the revoked key")
	}
	if client, err := b.dial(a.device); err == nil {
		client.Close()
		t.Fatal("the other environment took the device back without being turned on")
	}
	b.on()
	if !bytes.Equal(b.device.PublicKey().Marshal(), a.device.PublicKey().Marshal()) {
		t.Fatal("the second environment renewed the key again")
	}
	if client, err := b.dial(b.device); err != nil {
		t.Fatalf("the renewed key in the other environment: %v", err)
	} else {
		client.Close()
	}
	// A controller that did not run when the key was revoked refuses it
	// from the computer-wide list.
	if err := a.call("POST", "/api/entry/devices/"+a.computer.ID+"/revoke", map[string]any{"propagated": true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.dial(b.device); err == nil || entryClientError(err) != errEntryRevoked.Message {
		t.Fatalf("revoked key in an environment that was not told: %v", err)
	}
}

// F10: the command hop runs the guest's run script through a bash without
// profile or rc files, like guestControl, and replaces it with exec.
func TestEntryCommandHopRunsInPlainBash(t *testing.T) {
	f := newEntryFixture(t, true)
	f.on()
	if out, stderr, status := entryRun(t, f.client(), "echo hop", nil, nil); status != 0 || out != "hop\n" {
		t.Fatalf("%q %q %d", out, stderr, status)
	}
	for _, line := range f.guest.commands() {
		if strings.Contains(line, "echo hop") {
			if !strings.HasPrefix(line, "exec /bin/bash --noprofile --norc -c ") {
				t.Fatalf("hop command %q", line)
			}
			return
		}
	}
	t.Fatalf("no hop command in %q", f.guest.commands())
}

// F10: the blocks of an environment that is gone (a deleted local VM) are
// removed, and the Include line with the last one.
func TestEntrySweepRemovesBlocksOfDeletedEnvironments(t *testing.T) {
	paths := entryTestPaths(t)
	key := entryTestHostKey(t)
	vm := "fedcba9876543210"
	if err := os.MkdirAll(filepath.Join(paths.ComputerDir, cage.LocalVMDir, vm), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{defaultLocalVMID, vm} {
		if _, err := entryWriteClient(paths, environment, "测试 "+environment, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := entryIncludeApply(paths, entryIncludePreview(paths).Token); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(paths.ComputerDir, cage.LocalVMDir, vm)); err != nil {
		t.Fatal(err)
	}
	if err := entrySweepOrphans(paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.hostFile(vm)); !os.IsNotExist(err) {
		t.Fatal("the deleted VM's block stayed")
	}
	if _, err := os.Stat(paths.hostFile(defaultLocalVMID)); err != nil || !entryIncludePreview(paths).Present {
		t.Fatal("the sweep removed what still exists")
	}
	// A computer whose only block was a deleted VM loses the Include line.
	other := entryTestPaths(t)
	if err := os.MkdirAll(filepath.Join(other.ComputerDir, cage.LocalVMDir, vm), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := entryWriteClient(other, vm, "测试", key); err != nil {
		t.Fatal(err)
	}
	if _, err := entryIncludeApply(other, entryIncludePreview(other).Token); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(other.ComputerDir, cage.LocalVMDir, vm)); err != nil {
		t.Fatal(err)
	}
	if err := entrySweepOrphans(other); err != nil {
		t.Fatal(err)
	}
	if entryIncludePreview(other).Present {
		t.Fatal("the Include line stayed after the last block went")
	}
}

func entryTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	signer, err := entryHostSigner(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// F10: ~/.ssh/config edits refuse a read-only file, keep the first backup
// (from before Super Cage) and a rolling latest one in Super Cage's own
// directory, never beside a symlink's target, and leave a BOM where it was.
func TestEntryIncludeEditsReadOnlyBackupsAndBOM(t *testing.T) {
	paths := entryTestPaths(t)
	key := entryTestHostKey(t)
	if _, err := entryWriteClient(paths, defaultLocalVMID, "默认环境", key); err != nil {
		t.Fatal(err)
	}
	config := paths.userConfig()
	original := "Host example.org\n  User me\n"
	if err := os.WriteFile(config, []byte(original), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := entryIncludeApply(paths, entryIncludePreview(paths).Token); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Fatalf("read-only apply: %v", err)
	}
	if data, _ := os.ReadFile(config); string(data) != original || entryMode(t, config) != 0o444 {
		t.Fatal("a read-only ~/.ssh/config was changed")
	}
	if err := os.Chmod(config, 0o600); err != nil {
		t.Fatal(err)
	}
	// The line already there in a read-only file: nothing to write, no error;
	// taking it out is refused with what to do.
	if _, err := entryIncludeApply(paths, entryIncludePreview(paths).Token); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0o444); err != nil {
		t.Fatal(err)
	}
	if preview := entryIncludePreview(paths); !preview.Present || preview.Error != "" {
		t.Fatalf("read-only with the line: %+v", preview)
	}
	if err := entryIncludeRemove(paths); err == nil || !strings.Contains(err.Error(), "只读") || !strings.Contains(err.Error(), paths.includeLine()) {
		t.Fatalf("removal from a read-only file: %v", err)
	}
	if err := os.Chmod(config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := entryIncludeRemove(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(paths.DeviceDir, "ssh-config-backups", "config.first")); err != nil {
		t.Fatal(err)
	}
	// Apply, remove, apply, remove: the first backup keeps the original.
	for range 2 {
		if _, err := entryIncludeApply(paths, entryIncludePreview(paths).Token); err != nil {
			t.Fatal(err)
		}
		if err := entryIncludeRemove(paths); err != nil {
			t.Fatal(err)
		}
	}
	first, latest := entryBackupPaths(paths)
	if data, err := os.ReadFile(first); err != nil || string(data) != original {
		t.Fatalf("first backup %q %v", data, err)
	}
	if data, err := os.ReadFile(latest); err != nil || !strings.HasPrefix(string(data), paths.includeLine()) {
		t.Fatalf("latest backup %q %v", data, err)
	}
	if !strings.HasPrefix(first, paths.DeviceDir) {
		t.Fatalf("backup outside Super Cage's directory: %s", first)
	}
	// A symlinked ~/.ssh/config: nothing new appears beside its target.
	dotfiles := filepath.Join(filepath.Dir(paths.SSHDir), "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "ssh_config")
	if err := os.WriteFile(target, []byte("\ufeffHost x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, config); err != nil {
		t.Fatal(err)
	}
	if _, err := entryIncludeApply(paths, entryIncludePreview(paths).Token); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dotfiles); len(entries) != 1 {
		t.Fatalf("files beside the symlink's target: %v", entries)
	}
	data, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(data), "\ufeff"+paths.includeLine()+"\n") {
		t.Fatalf("the BOM moved: %q", data)
	}
	if err := entryIncludeRemove(paths); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "\ufeffHost x\n" {
		t.Fatalf("after removal: %q", data)
	}
	if entries, _ := os.ReadDir(dotfiles); len(entries) != 1 {
		t.Fatalf("files beside the symlink's target: %v", entries)
	}
}

// F6: a generated alias never takes a name the owner's own configuration
// already matches, directly, through an Include or a pattern; the
// confirmation shows the alias and the whole block.
func TestEntryAliasSkipsNamesTheOwnerAlreadyUses(t *testing.T) {
	key := entryTestHostKey(t)
	cases := []struct {
		name, config, extra string
		bad                 func(string) bool
	}{
		{"direct", "Host cage-lab1\n  HostName lab1.example.org\n", "", func(a string) bool { return a == "cage-lab1" }},
		{"include", "Include conf.d/*.conf\n", "Host other cage-lab1\n  Port 2222\n", func(a string) bool { return a == "cage-lab1" }},
		{"pattern", "Host cage-*\n  User owner\n", "", func(a string) bool { return strings.HasPrefix(a, "cage-") }},
		{"match", "Match host cage-lab?\n  User owner\n", "", func(a string) bool { return a == "cage-lab1" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths := entryTestPaths(t)
			if err := os.MkdirAll(filepath.Join(paths.SSHDir, "conf.d"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.userConfig(), []byte(c.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if c.extra != "" {
				if err := os.WriteFile(filepath.Join(paths.SSHDir, "conf.d", "a.conf"), []byte(c.extra), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			alias, err := entryWriteClient(paths, defaultLocalVMID, "lab1", key)
			if err != nil {
				t.Fatal(err)
			}
			if c.bad(alias) {
				t.Fatalf("alias %q takes the owner's name", alias)
			}
			preview := entryIncludePreviewFor(paths, defaultLocalVMID, "lab1")
			if preview.Alias != alias || !strings.Contains(preview.Block, "Host "+alias+"\n") || !strings.Contains(preview.Block, "ProxyCommand ") {
				t.Fatalf("preview %+v", preview)
			}
			if prompt := entryIncludePrompt(preview); !strings.Contains(prompt, "ssh "+alias+" 连接") || !strings.Contains(prompt, "    Host "+alias+"\n") {
				t.Fatalf("cage entry include asks:\n%s", prompt)
			}
		})
	}
	// A rule that takes every candidate is said plainly, and nothing hangs.
	every := entryTestPaths(t)
	if err := os.MkdirAll(every.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(every.userConfig(), []byte("Host *-*\n  User owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := entryWriteClient(every, defaultLocalVMID, "lab1", key); err == nil || !strings.Contains(err.Error(), "主机名规则") {
		t.Fatalf("every name taken: %v", err)
	}
	if preview := entryIncludePreviewFor(every, defaultLocalVMID, "lab1"); !strings.Contains(preview.Error, "主机名规则") {
		t.Fatalf("preview %+v", preview)
	}
	// A "Host *" default is not a claim on any name.
	paths := entryTestPaths(t)
	if err := os.MkdirAll(paths.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.userConfig(), []byte("Host *\n  User owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if alias, err := entryWriteClient(paths, defaultLocalVMID, "lab1", key); err != nil || alias != "cage-lab1" {
		t.Fatalf("alias %q %v", alias, err)
	}
}

// sshBinaries are the OpenSSH clients on this computer the generated block
// must work with: the system one and Homebrew's when installed.
func sshBinaries() []string {
	var found []string
	for _, path := range []string{"/usr/bin/ssh", "/opt/homebrew/bin/ssh", "/usr/local/bin/ssh"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			found = append(found, path)
		}
	}
	return found
}

func sshGWith(t *testing.T, binary, config, host string) (map[string]string, string, int) {
	t.Helper()
	cmd := exec.Command(testSSHShim(binary), "-G", "-F", config, host)
	cmd.Env = []string{testSSHPath("/usr/bin:/bin"), "HOME=" + filepath.Dir(filepath.Dir(config))}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	}
	values := map[string]string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if name, value, ok := strings.Cut(line, " "); ok {
			if values[name] != "" {
				values[name] += "\n"
			}
			values[name] += value
		}
	}
	return values, stderr.String(), code
}

// F7 and F8: the reviewer's three owner configurations with hostile
// "Host *" defaults, against every OpenSSH client found here.
func TestEntryGeneratedBlockAgainstHostileOwnerConfigs(t *testing.T) {
	binaries := sshBinaries()
	if len(binaries) == 0 {
		t.Skip("no OpenSSH client")
	}
	for _, binary := range binaries {
		t.Run(strings.ReplaceAll(binary, "/", "_"), func(t *testing.T) {
			version, _ := exec.Command(binary, "-V").CombinedOutput()
			t.Logf("%s: %s", binary, strings.TrimSpace(string(version)))
			paths := entryTestPaths(t)
			alias, err := entryWriteClient(paths, defaultLocalVMID, "lab1", entryTestHostKey(t))
			if err != nil {
				t.Fatal(err)
			}
			include := "Include " + entryQuote(paths.clientFile()) + "\n"
			dir := filepath.Dir(paths.SSHDir)
			write := func(name, text string) string {
				path := filepath.Join(paths.SSHDir, name)
				if err := os.WriteFile(path, []byte(include+text), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			hostile := write("owner_config", "Host *\n  ProxyJump bastion.example.org\n  ControlMaster auto\n  ControlPath /tmp/cm-%C\n  ControlPersist 10m\n  IdentityFile "+dir+"/id_owner\n  SendEnv FOO\n  SetEnv BAR=1\n  ForwardAgent yes\n  RemoteForward 9000 localhost:22\n  PermitLocalCommand yes\n  LocalCommand echo hi\n  RequestTTY force\n  RemoteCommand tmux\n  KnownHostsCommand /bin/echo %H\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  GlobalKnownHostsFile /etc/hostile_known_hosts\n  AddKeysToAgent yes\n  SessionType none\n  StdinNull yes\n  ForkAfterAuthentication yes\n  PubkeyAuthentication no\n  PasswordAuthentication yes\n  KbdInteractiveAuthentication yes\n  CanonicalizeHostname yes\n  ProxyUseFdpass yes\n  Tunnel yes\n  BatchMode yes\n  PreferredAuthentications password\n")
			values, stderr, code := sshGWith(t, binary, hostile, alias)
			if code != 0 {
				t.Fatalf("owner_config: %d %s", code, stderr)
			}
			want := map[string]string{
				"controlmaster": "false", "controlpersist": "no", "pubkeyauthentication": "true",
				"preferredauthentications": "publickey", "passwordauthentication": "no", "kbdinteractiveauthentication": "no",
				"proxyusefdpass":       "no",
				"globalknownhostsfile": "/dev/null", "canonicalizehostname": "false", "addkeystoagent": "false",
				"forwardagent": "no", "clearallforwardings": "yes", "permitlocalcommand": "no", "requesttty": "auto",
				"stricthostkeychecking": "true", "tunnel": "false", "user": "cage",
			}
			for name, value := range want {
				if values[name] != value {
					t.Errorf("owner_config: %s = %q, want %q", name, values[name], value)
				}
			}
			// LocalCommand stays the owner's, but PermitLocalCommand no keeps
			// it from running. The pins newer than OpenSSH 7.6 (SessionType,
			// StdinNull, ForkAfterAuthentication, KnownHostsCommand) are
			// cage ssh's only (N2 of the second review); its -F file keeps
			// the owner's configuration out.
			for _, unset := range []string{"controlpath", "proxyjump", "remotecommand"} {
				if values[unset] != "" {
					t.Errorf("owner_config: %s = %q", unset, values[unset])
				}
			}
			if !strings.Contains(values["proxycommand"], "entry proxy default") || !strings.Contains(values["userknownhostsfile"], "default.known_hosts") {
				t.Errorf("owner_config: proxycommand %q, known hosts %q", values["proxycommand"], values["userknownhostsfile"])
			}
			// The owner's own IgnoreUnknown still counts (first value wins).
			ignore := write("owner_config2", "Host *\n  IgnoreUnknown FooBarOption\n  FooBarOption yes\n")
			for _, host := range []string{alias, "example.org"} {
				if _, stderr, code := sshGWith(t, binary, ignore, host); code != 0 {
					t.Fatalf("owner_config2 %s: %d %s", host, code, stderr)
				}
			}
			// The owner's globals after the Include still reach other hosts.
			globals := write("owner_config3", "ServerAliveInterval 77\nUser globaluser\nForwardAgent yes\n\nHost example.org\n  HostName 192.0.2.10\n")
			other, _, code := sshGWith(t, binary, globals, "example.org")
			cageHost, _, code2 := sshGWith(t, binary, globals, alias)
			if code != 0 || code2 != 0 || other["serveraliveinterval"] != "77" || other["forwardagent"] != "yes" || other["user"] != "globaluser" ||
				cageHost["serveraliveinterval"] != "15" || cageHost["forwardagent"] != "no" || cageHost["user"] != "cage" {
				t.Fatalf("owner_config3: %v / %v", other, cageHost)
			}
		})
	}
}

// Item 8: the help says which commands carry the environment's bytes
// unfiltered.
func TestEntryHelpSaysWhatIsUnfiltered(t *testing.T) {
	var stderr bytes.Buffer
	if code := EntryMain(nil, filepath.Join(t.TempDir(), "config.json"), nil, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "不经过滤") || !strings.Contains(stderr.String(), "ssh -T") || !strings.Contains(stderr.String(), "rsync") {
		t.Fatalf("usage:\n%s", stderr.String())
	}
}
