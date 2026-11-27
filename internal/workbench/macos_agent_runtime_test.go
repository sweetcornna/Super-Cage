//go:build darwin

package workbench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/agentcatalog"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"golang.org/x/crypto/ssh"
)

func TestNativeMacAgentRuntimeColdInstall(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	home := t.TempDir()
	const pythonPath = "/usr/local/libexec/cage-cli/bin/python3"
	guest := newMacWorkspaceGuest(t, func(channel ssh.Channel, command string) (uint32, bool) {
		if !strings.HasPrefix(command, "exec /bin/zsh -c ") || !strings.Contains(command, pythonPath) {
			return 1, true
		}
		// Use the host interpreter in place of the bundled interpreter, with
		// the actual installer and helper running under an empty guest home.
		cmd := exec.Command("/bin/sh", "-c", strings.ReplaceAll(command, pythonPath, python))
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
		if err := cmd.Run(); err != nil {
			return uint32(max(1, exitCodeOf(err))), true
		}
		return 0, true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var result agentProbeSnapshot
	if err := guest.s.runAgentRuntime(ctx, "probe-all", map[string]any{"specs": []any{}}, &result); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, agentRuntimeSpec.entryRelPath())
	data, err := os.ReadFile(installed)
	if err != nil || string(data) != cage.AgentRuntimeHelper || result.Agents == nil || result.Prerequisites == nil {
		t.Fatalf("bundled Python did not install and run the expected helper: %v", err)
	}
}

func TestNativeMacClaudeAliasUsesTheNativeLauncherRules(t *testing.T) {
	s := testServer(t)
	s.macosGuest = &guestos.MacOSProfile{}
	s.preferences.ClaudeTerminalCommand = "claude --verbose"
	if _, err := s.createTerminal(terminalRequest{Kind: "agent", AgentID: "claude"}); err == nil || !strings.Contains(err.Error(), "macOS 客机支持") {
		t.Fatalf("Agent Claude alias bypassed launcher validation: %v", err)
	}
}

func TestNativeMacAgentLoginRequiresBundledBrowserHelper(t *testing.T) {
	home := t.TempDir()
	python := filepath.Join(home, "python")
	if err := os.WriteFile(python, []byte("#!/bin/sh\nprintf '%s' \"$CAGE_CLI_LOGIN_JSON\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	opener := filepath.Join(home, "cage-open")
	spec, ok := agentcatalog.Lookup("codex", nil)
	if !ok {
		t.Fatal("missing fixture agent spec")
	}
	id := strings.Repeat("a", 24)
	script := managedAgentTerminalScript(home, spec, "codex", id, "oauth", true, true)
	script = strings.ReplaceAll(script, "/usr/local/libexec/cage-cli/bin/cage-open", shellescape.Quote(opener))
	script = strings.ReplaceAll(script, "/usr/local/libexec/cage-cli/bin/python3", shellescape.Quote(python))
	run := func() ([]byte, error) {
		cmd := exec.Command("/bin/zsh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		return cmd.CombinedOutput()
	}
	if out, err := run(); err == nil || strings.Contains(string(out), id) || !strings.Contains(string(out), "浏览器辅助程序缺失") {
		t.Fatalf("missing opener reached the login helper: %v, %s", err, out)
	}
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err != nil || !strings.Contains(string(out), `"account_id":"`+id+`"`) {
		t.Fatalf("installed helper did not receive the account: %v, %s", err, out)
	}
}
