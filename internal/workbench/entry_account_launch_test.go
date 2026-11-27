//go:build !windows

package workbench

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"golang.org/x/crypto/ssh"
)

func TestEntryExecUsesManagedCodexAndRefusesUnavailablePool(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for the guest account helper")
	}
	f := newEntryFixture(t, true)
	home := filepath.Join(f.guest.dir, "home")
	install := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	// This test inspects stderr from the child; use a locale available on every
	// test host so bash doesn't inject a locale warning into that stream.
	install(filepath.Join(f.guest.dir, "environment"), "TZ=UTC\nLANG=C\nLC_ALL=C\n", 0o600)
	sshdConfigPath := filepath.Join(f.guest.dir, "sshd_config")
	sshdConfig, err := os.ReadFile(sshdConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	install(sshdConfigPath, strings.ReplaceAll(string(sshdConfig), "ja_JP.UTF-8", "C"), 0o600)

	helper := filepath.Join(home, "tools", "cage-codex-account")
	source, err := os.ReadFile(filepath.Join("..", "cage", "codex-account.py"))
	if err != nil {
		t.Fatal(err)
	}
	install(helper, string(source), 0o700)
	install(filepath.Join(filepath.Dir(helper), "client_runtime.py"), cage.ClientRuntimeHelper, 0o600)
	install(filepath.Join(home, ".local", "bin", "codex"), "#!"+python+`
import json, os, sys
if sys.argv[-2:] == ["login", "status"]:
    print("Logged in using ChatGPT")
    sys.exit(0)
if sys.argv[-1:] == ["--version"]:
    print("codex-cli 0.172.0")
    sys.exit(0)
if sys.argv[-2:] == ["resume", "--all"]:
    print("opened native picker in " + os.environ["CODEX_HOME"])
    sys.exit(0)
if "--fixture-hold" in sys.argv[1:]:
    print(os.environ["CODEX_HOME"], flush=True)
    sys.stdin.read()
    sys.exit(37)
print(json.dumps([os.environ.get("CODEX_HOME", ""), sys.argv[1:], sys.stdin.read()]))
print("child-stderr", file=sys.stderr)
sys.exit(37)
`, 0o700)

	// Generate the same executable shim as publication, pointing only its
	// helper location at the isolated guest's test install.
	makeShim := exec.Command(python, "-I", "-c", `
import runpy, sys
runtime = runpy.run_path(sys.argv[1])
make_shim = runtime["_write_codex_auto_shim"]
make_shim.__globals__["_codex_account_helper"] = lambda: sys.argv[2]
runtime["AUTO_SHIMS_DIR"].mkdir(parents=True, exist_ok=True)
make_shim(True, False)
runtime["_ensure_shell_shims_hook"]()
`, filepath.Join("..", "cage", "agent-runtime.py"), helper)
	makeShim.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if output, err := makeShim.CombinedOutput(); err != nil {
		t.Fatalf("install guest shim: %s (%v)", output, err)
	}

	first, second := strings.Repeat("a", 24), strings.Repeat("b", 24)
	root := filepath.Join(home, ".local", "share", "cage", "codex-accounts")
	profile := filepath.Join(root, second, "home")
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"managed@example.test"}`))
	auth := `{"tokens":{"id_token":"fixture.` + claims + `.fixture"}}`
	install(filepath.Join(root, first, "home", "auth.json"), auth, 0o600)
	install(filepath.Join(profile, "auth.json"), auth, 0o600)
	var history []cliHistoryAccount
	mode, selected, revision := "sequential", "", ""
	var active map[string]int
	writePolicy := func(managed bool, candidates ...claudeLaunchCandidate) {
		t.Helper()
		if candidates == nil {
			candidates = []claudeLaunchCandidate{}
		}
		data, err := json.Marshal(claudeLaunchSchedule{
			Mode: mode, Managed: managed, Accounts: candidates, HistoryAccounts: history, Active: active,
			SelectedAccountID: selected, SelectedAccountRevision: revision, AccountOrder: []string{first, second},
		})
		if err != nil {
			t.Fatal(err)
		}
		install(filepath.Join(root, "launch.json"), string(data), 0o600)
	}
	stale := claudeLaunchCandidate{ID: first, Subject: strings.Repeat("f", 64)}
	good := claudeLaunchCandidate{ID: second, Subject: claudeIdentityHash("managed@example.test", "")}
	firstProfile := filepath.Join(root, first, "home")
	firstReady := claudeLaunchCandidate{ID: first, Subject: good.Subject}
	writePolicy(true, stale, good)

	f.on()
	client := f.client()
	launch := func(args []string) (out, stderr string, status int) {
		return entryRun(t, client, shellescape.QuoteCommand(args), []byte("stdin from SSH\n"), nil)
	}
	args := []string{"codex", "-c", "disable_paste_burst=true", "--", "with space", "quote's $HOME", ""}
	out, stderr, status := launch(args)
	expected, _ := json.Marshal([]any{profile, args[1:], "stdin from SSH\n"})
	// Python's JSON emitter inserts spaces and the child may see a resolved
	// guest path. Check decoded values through actual SSH streams.
	var got []any
	if status != 37 || strings.TrimSpace(stderr) != "child-stderr" || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("managed launch = status %d stdout %q stderr %q", status, out, stderr)
	}
	decoded, _ := json.Marshal(got)
	if string(decoded) != string(expected) {
		t.Fatalf("child input = %s, want %s", decoded, expected)
	}

	if zsh, err := exec.LookPath("zsh"); err == nil {
		// The raw SSH hop does not run entry-run.sh. A noninteractive zsh
		// resolves the generated shim from its guest shell startup file.
		command := "exec " + shellescape.QuoteCommand([]string{zsh, "-c", shellescape.QuoteCommand(args)})
		raw := f.s.sshCommand(t.Context(), "-T", f.s.sshTarget(), "--", command)
		var stdout, stderr bytes.Buffer
		raw.Stdin, raw.Stdout, raw.Stderr = strings.NewReader("stdin from SSH\n"), &stdout, &stderr
		var exit *exec.ExitError
		if err := raw.Run(); !errors.As(err, &exit) || exit.ExitCode() != 37 || stdout.String() != out || stderr.String() != "child-stderr\n" {
			t.Fatalf("raw SSH = %v stdout %q stderr %q", err, stdout.String(), stderr.String())
		}
	}

	held, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	heldInput, _ := held.StdinPipe()
	heldOutput, _ := held.StdoutPipe()
	if err := held.Start("codex --fixture-hold"); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(heldOutput).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case path := <-ready:
		if path != profile {
			t.Fatalf("held process used %q instead of %q", path, profile)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held Codex did not start")
	}
	quota := shellescape.QuoteCommand([]string{helper, "quotas", second})
	out, stderr, status = entryRun(t, client, quota, nil, nil)
	var load map[string]codexGuestQuota
	if status != 0 || json.Unmarshal([]byte(out), &load) != nil || load[second].PlainLaunches != 1 {
		t.Fatalf("active lease = status %d stdout %q stderr %q", status, out, stderr)
	}
	mode = "smart"
	writePolicy(true, good, firstReady)
	out, stderr, status = launch(args)
	if status != 37 || !strings.Contains(out, firstProfile) || strings.TrimSpace(stderr) != "child-stderr" {
		t.Fatalf("smart launch with active lease = status %d stdout %q stderr %q", status, out, stderr)
	}
	_ = heldInput.Close()
	if err := held.Wait(); err == nil {
		t.Fatal("held Codex lost its nonzero exit status")
	}
	out, stderr, status = entryRun(t, client, quota, nil, nil)
	if status != 0 || strings.TrimSpace(out) != "{}" {
		t.Fatalf("settled lease = status %d stdout %q stderr %q", status, out, stderr)
	}

	wantProfile := func(want string) {
		t.Helper()
		out, stderr, status := launch(args)
		if status != 37 || !strings.Contains(out, want) || strings.TrimSpace(stderr) != "child-stderr" {
			t.Fatalf("want profile %q, got status %d stdout %q stderr %q", want, status, out, stderr)
		}
	}
	mode, selected = "manual", first
	writePolicy(true, firstReady, good)
	wantProfile(firstProfile)
	writePolicy(true, stale, good)
	out, stderr, status = launch(args)
	if status != 78 || out != "" || !strings.Contains(stderr, "手动选择") {
		t.Fatalf("manual launch with wrong identity = status %d stdout %q stderr %q", status, out, stderr)
	}

	mode, revision = "exhaust", "first-selection"
	writePolicy(true, firstReady, good)
	wantProfile(firstProfile)
	writeUsage := func(used int) {
		t.Helper()
		now := time.Now().Unix()
		data, err := json.Marshal(map[string]any{
			"identity": good.Subject, "observed_at": now,
			"primary": map[string]any{
				"used_percentage": used, "resets_at": now + 3600, "window_minutes": 300, "observed_at": now,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		install(filepath.Join(root, first, "quota.json"), string(data), 0o600)
	}
	writeUsage(100)
	wantProfile(profile)
	writeUsage(15)
	wantProfile(profile)
	revision = "explicitly-reselected"
	writePolicy(true, firstReady, good)
	wantProfile(firstProfile)

	mode, active = "smart", map[string]int{first: 1}
	writePolicy(true, firstReady, good)
	wantProfile(profile)
	mode, selected, revision, active = "manual", first, "", nil

	thread := "3e590d2c-3f9f-4b4d-aa93-14a9b5e726f7"
	install(filepath.Join(profile, "sessions", "2026", "11", "26", "rollout-2026-11-26T12-00-00-"+thread+".jsonl"),
		`{"type":"session_meta","payload":{"id":"`+thread+`","cwd":"/old/project"}}`+"\n", 0o600)
	install(filepath.Join(root, first, "home", "sessions", "2026", "11", "25", "rollout-2026-11-25T12-00-00-c1a464d9-f992-4408-b233-ee3b6e23c518.jsonl"),
		`{"type":"session_meta","payload":{"id":"c1a464d9-f992-4408-b233-ee3b6e23c518","cwd":"/other/project"}}`+"\n", 0o600)
	history = []cliHistoryAccount{{first, good.Subject, "first profile"}, {second, good.Subject, "original profile"}}
	writePolicy(true)
	orca := []string{"codex", "-c", "disable_paste_burst=true", "--dangerously-bypass-approvals-and-sandbox"}
	resume := append(append([]string{}, orca...), "resume", thread)
	out, stderr, status = launch(resume)
	if status != 37 || !strings.Contains(out, profile) || !strings.Contains(out, thread) {
		t.Fatalf("resume = status %d stdout %q stderr %q", status, out, stderr)
	}
	t.Run("interactive resume chooses the original profile", func(t *testing.T) {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := session.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		input, err := session.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := session.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		timeout := time.AfterFunc(10*time.Second, func() { _ = session.Close() })
		defer timeout.Stop()
		if err := session.Start(shellescape.QuoteCommand(append(append([]string{}, orca...), "resume", "--all"))); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(output)
		var seen strings.Builder
		for scanner.Scan() {
			seen.WriteString(scanner.Text())
			if strings.Contains(scanner.Text(), "2. original profile") {
				_, _ = input.Write([]byte("2\n"))
			}
		}
		if err := session.Wait(); err != nil || !strings.Contains(seen.String(), "opened native picker in "+profile) {
			t.Fatalf("original picker: %v, output %q", err, seen.String())
		}
	})
	mode, selected = "sequential", ""
	for _, candidates := range [][]claudeLaunchCandidate{{stale}, {}} {
		writePolicy(true, candidates...)
		out, stderr, status = launch(args)
		if status != 78 || out != "" || !strings.Contains(stderr, "暂无可启动") {
			t.Fatalf("unavailable launch = status %d stdout %q stderr %q", status, out, stderr)
		}
	}
	install(filepath.Join(root, "launch.json"), "{broken", 0o600)
	out, stderr, status = launch(args)
	if status != 78 || out != "" || !strings.Contains(stderr, "无法读取") {
		t.Fatalf("corrupt pool launch = status %d stdout %q stderr %q", status, out, stderr)
	}
	out, stderr, status = launch(append(append([]string{}, orca...), "login", "status"))
	if status != 0 || strings.TrimSpace(out) != "Logged in using ChatGPT" || stderr != "" {
		t.Fatalf("login status = status %d stdout %q stderr %q", status, out, stderr)
	}
	out, stderr, status = launch(append(append([]string{}, orca...), "--version"))
	if status != 0 || strings.TrimSpace(out) != "codex-cli 0.172.0" || stderr != "" {
		t.Fatalf("version = status %d stdout %q stderr %q", status, out, stderr)
	}
	writePolicy(false)
	out, stderr, status = launch(args)
	if status != 37 || !strings.HasPrefix(out, `["",`) || strings.TrimSpace(stderr) != "child-stderr" {
		t.Fatalf("empty pool launch = status %d stdout %q stderr %q", status, out, stderr)
	}
}
