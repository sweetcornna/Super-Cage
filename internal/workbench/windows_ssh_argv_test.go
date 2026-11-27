package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

// windowsCommandLineUTF16Len computes the exact UTF-16 code unit length that
// Go's os/exec (syscall.makeCmdLine / syscall.EscapeArg) passes to Windows
// CreateProcessW, which rejects command lines >= 32,767 UTF-16 code units.
func windowsCommandLineUTF16Len(args []string) int {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(windowsEscapeArg(arg))
	}
	return len(utf16.Encode([]rune(b.String())))
}

func windowsEscapeArg(s string) string {
	if len(s) == 0 {
		return `""`
	}
	needsQuote := strings.ContainsAny(s, " \t\"")
	if !needsQuote {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			slashes++
		case '"':
			for range slashes*2 + 1 {
				b.WriteByte('\\')
			}
			b.WriteByte('"')
			slashes = 0
		default:
			for range slashes {
				b.WriteByte('\\')
			}
			slashes = 0
			b.WriteByte(s[i])
		}
	}
	for range slashes * 2 {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

func totalArgvBytes(args []string) int {
	total := 0
	for _, arg := range args {
		total += len(arg)
	}
	return total
}

func assertShortSSHArgv(t *testing.T, label string, args []string, limit int) {
	t.Helper()
	raw := totalArgvBytes(args)
	winUTF16 := windowsCommandLineUTF16Len(args)
	t.Logf("%s: argv_bytes=%d win_utf16=%d", label, raw, winUTF16)
	if raw >= limit {
		t.Errorf("%s: total argv length %d bytes >= %d limit (win_utf16=%d)", label, raw, limit, winUTF16)
	}
	if winUTF16 >= limit {
		t.Errorf("%s: Windows CreateProcess command line %d UTF-16 units >= %d limit", label, winUTF16, limit)
	}
}

func TestAgentRuntimeSSHArgvIsShortAndConstant(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()

	// All three primary call sites in agent_manager.go plus the other
	// agent-runtime.py callers (magpie, t3code, herdr).
	for _, subcommand := range []string{
		"probe-all",        // runAgentRuntime
		"install",          // installAgentHTTP
		"uninstall",        // uninstallAgentHTTP
		"magpie-install",   // magpieInstallHTTP
		"magpie-uninstall", // magpieUninstallHTTP
		"t3code-install",   // t3codeInstallHTTP
		"herdr-install",    // herdrInstallHTTP
		"herdr-uninstall",  // herdrUninstallHTTP
	} {
		cmd := s.agentRuntimeSSHCommand(ctx, subcommand)
		assertShortSSHArgv(t, "agent-runtime/"+subcommand, cmd.Args, 4096)
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, " -I ") {
			t.Errorf("agent-runtime/%s lost python -I isolated mode: %s", subcommand, joined)
		}
		if !strings.Contains(joined, `d="$HOME/.local/share/cage/agent-runtime"`) ||
			!strings.Contains(joined, `exec python3 -I "$d/`+agentRuntimeHash+`.py" `+subcommand) {
			t.Errorf("agent-runtime/%s lost hash-pinned path: %s", subcommand, joined)
		}
	}

	// One-time installer command must also stay short and constant.
	install1x := s.guestScriptInstallSSHCommand(ctx, agentRuntimeSpec, s.guestPythonBinary())
	assertShortSSHArgv(t, "agent-runtime/ensure-install-1x", install1x.Args, 4096)

	// Scale the payload of each file while keeping the same companion paths.
	// The command checks those paths, so comparing bundles with different
	// members would also measure the length of the extra file checks.
	var files []guestScriptFile
	hash := sha256.New()
	for _, file := range agentRuntimeSpec.files {
		content := bytes.Repeat(file.content, 10)
		files = append(files, guestScriptFile{suffix: file.suffix, content: content})
		hash.Write(content)
	}
	bigSpec := newGuestScriptBundle(agentRuntimeSpec.relDir, fmt.Sprintf("%x", hash.Sum(nil)), agentRuntimeSpec.mode, agentRuntimeSpec.entry, files)
	exec1x := s.sshCommand(ctx, "-T", s.sshTarget(), "--", s.guestShellScript(agentRuntimeSpec.guardedShellBody(s.guestPythonBinary(), "install")))
	exec10x := s.sshCommand(ctx, "-T", s.sshTarget(), "--", s.guestShellScript(bigSpec.guardedShellBody(s.guestPythonBinary(), "install")))
	install10x := s.guestScriptInstallSSHCommand(ctx, bigSpec, s.guestPythonBinary())
	assertShortSSHArgv(t, "agent-runtime/exec-10x", exec10x.Args, 4096)
	assertShortSSHArgv(t, "agent-runtime/ensure-install-10x", install10x.Args, 4096)
	if totalArgvBytes(exec1x.Args) != totalArgvBytes(exec10x.Args) {
		t.Errorf("exec argv grew with 10x runtime: 1x=%d 10x=%d", totalArgvBytes(exec1x.Args), totalArgvBytes(exec10x.Args))
	}
	if totalArgvBytes(install1x.Args) != totalArgvBytes(install10x.Args) {
		t.Errorf("install argv grew with 10x runtime: 1x=%d 10x=%d", totalArgvBytes(install1x.Args), totalArgvBytes(install10x.Args))
	}

	// Interactive PTY sites that invoke agent-runtime.py.
	spec, _, ok := s.lookupAgentSpec("codex")
	if !ok {
		t.Fatal("missing built-in codex spec")
	}
	loginScript := managedAgentTerminalScript("/proj", spec, "codex", "0123456789abcdef01234567", "", true, false)
	assertShortSSHArgv(t, "agent-accounts/login", s.sshCommand(ctx, "-tt", s.sshTarget(), "--", loginScript).Args, 4096)

	prepScript := managedAgentTerminalScript("/proj", spec, "codex", "0123456789abcdef01234567", "", false, false)
	assertShortSSHArgv(t, "agent-accounts/prepare-session", s.sshCommand(ctx, "-tt", s.sshTarget(), "--", prepScript).Args, 4096)

	herdrWrapped := s.herdrAttachScript(herdrAttach{
		Mode:      "create",
		Name:      "cage-0123456789abcdef",
		Workspace: "/proj",
		Project:   "proj",
		Title:     "Codex 1",
		Kind:      "agent",
		Inner:     prepScript,
	})
	assertShortSSHArgv(t, "herdr-attach/wrapping-prepare-session", s.sshCommand(ctx, "-tt", s.sshTarget(), "--", herdrWrapped).Args, 4096)

	// macOS guest path stays short and preserves bundled python + zsh.
	s.macosGuest = &guestos.MacOSProfile{User: "cage"}
	macCmd := s.agentRuntimeSSHCommand(ctx, "install")
	s.macosGuest = nil
	if macCmd.Err == nil {
		assertShortSSHArgv(t, "agent-runtime/macos-install", macCmd.Args, 4096)
	}
}

func TestAuditedSSHCallSitesStayUnderWindowsArgvLimit(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()

	// 1. codex_sessions.go (runCodexSessions) & codex_terminal.go (tui-launch)
	codexCmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", guestLoginScript(codexSessionsSpec.guardedShellBody("python3", "discover", "-")))
	assertShortSSHArgv(t, "codex-sessions/discover", codexCmd.Args, 4096)

	payload, _ := json.Marshal(map[string]any{
		"connection":             "conn-1",
		"lease":                  "lease-1",
		"thread":                 "00000000-0000-0000-0000-000000000001",
		"account":                "default",
		"cwd":                    "/mnt/host/project",
		"additional_directories": []string{"/mnt/host/dep1", "/mnt/host/dep2"},
	})
	codexTUI := s.sshCommand(ctx, "-tt", s.sshTarget(), "--", codexSessionTerminalScript("/mnt/host/project", string(payload)))
	assertShortSSHArgv(t, "codex-terminal/tui-launch", codexTUI.Args, 4096)

	// 2. cache_maintenance.go with a large 110-item clean request (~12 KB JSON + 24 KB script).
	items := make([]map[string]string, 110)
	for i := range items {
		items[i] = map[string]string{
			"id":          "cache-item-" + strings.Repeat("x", 20),
			"fingerprint": strings.Repeat("a", 64),
		}
	}
	cleanReq, _ := json.Marshal(map[string]any{"action": "clean", "scope": "linux-guest", "items": items})
	cacheCmd, err := s.cacheMaintenanceSSHCommand(ctx, cleanReq)
	if err != nil {
		t.Fatal(err)
	}
	assertShortSSHArgv(t, "cache-maintenance/clean-110", cacheCmd.Args, 4096)

	// 3. mobile_cli.go (mobileRunnerSSHCommand) with 10 additional directories.
	extraDirs := make([]string, 0, 20)
	for range 10 {
		extraDirs = append(extraDirs, "--add-dir", "/mnt/host/workspace/"+strings.Repeat("nested/", 10)+"repo")
	}
	mobileCmd := s.mobileRunnerSSHCommand(ctx, "/mnt/host/project", []string{"claude"}, append([]string{"--print", "--output-format", "stream-json"}, extraDirs...))
	assertShortSSHArgv(t, "mobile-cli/runner", mobileCmd.Args, 4096)

	// 4. mobile_cli_adopt.go (promoteMobileVerifyCommand).
	adoptCmd := s.promoteMobileVerifyCommand(ctx, "/mnt/host/project", []string{"claude"}, "00000000-0000-0000-0000-000000000001")
	assertShortSSHArgv(t, "mobile-cli/adopt-verify", adoptCmd.Args, 4096)
}

func TestStdinPythonBootstrapFraming(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	script := `import json, sys
if __name__ != "__main__":
    raise SystemExit(9)
data = json.loads(sys.stdin.buffer.read())
print(json.dumps({"echo": data["items"], "count": len(data["items"])}))
`
	input := []byte(`{"items":["a","b","c"]}`)
	cmd := exec.Command(python, "-I", "-c", stdinPythonBootstrap, "test_framed.py")
	cmd.Stdin = strings.NewReader(string(stdinPythonPayload(script, input)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stdinPythonBootstrap failed: %v\n%s", err, out)
	}
	var got struct {
		Echo  []string `json:"echo"`
		Count int      `json:"count"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.Count != 3 {
		t.Fatalf("unexpected framed output: %q (%v)", out, err)
	}
}
