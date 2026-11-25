package workbench

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"slices"

	"al.essio.dev/pkg/shellescape"
)

//go:embed mobile_cli_identity.py
var mobileCLIIdentity string

var mobileCLIGuestScripts = func() *guestScriptSpec {
	sum := sha256.Sum256([]byte(mobileSupervisor + "\x00" + mobileCLIIdentity))
	return newGuestScriptBundle(".local/share/cage/mobile-cli", fmt.Sprintf("%x", sum), 0o600, "-supervisor.py", []guestScriptFile{
		{suffix: "-supervisor.py", content: []byte(mobileSupervisor)},
		{suffix: "-identity.py", content: []byte(mobileCLIIdentity)},
	})
}()

// Runs after the guest login shell has established the CLI's environment.
func mobileIdentityCommand(prefix, args []string) []string {
	if len(prefix) == 5 && prefix[0] == "env" && prefix[2] == "/usr/local/bin/cage-claude-account" && prefix[3] == "run" {
		command := []string{prefix[0], prefix[1], "python3", "-I", "-c", mobileCLIIdentity, "official", prefix[2], prefix[4]}
		return append(command, args...)
	}
	if len(prefix) == 1 && prefix[0] == "claude" || len(prefix) == 2 && prefix[0] == "ccs" {
		command := []string{"python3", "-I", "-c", mobileCLIIdentity, prefix[0]}
		if prefix[0] == "ccs" {
			command = append(command, prefix[1])
		}
		return append(command, args...)
	}
	return append(append([]string(nil), prefix...), args...)
}

// mobileIdentityShellCommand builds a constant-size POSIX command that runs the
// installed hash-pinned mobile_cli_identity.py script referenced by scriptExpr
// (e.g. `"$HOME/.local/share/cage/mobile-cli/<hash>-identity.py"`).
func mobileIdentityShellCommand(scriptExpr string, prefix, args []string) string {
	if len(prefix) == 5 && prefix[0] == "env" && prefix[2] == "/usr/local/bin/cage-claude-account" && prefix[3] == "run" {
		head := shellescape.QuoteCommand([]string{prefix[0], prefix[1], "python3", "-I"})
		tail := shellescape.QuoteCommand(append([]string{"official", prefix[2], prefix[4]}, args...))
		return head + " " + scriptExpr + " " + tail
	}
	if len(prefix) == 1 && prefix[0] == "claude" || len(prefix) == 2 && prefix[0] == "ccs" {
		tail := shellescape.QuoteCommand(append(slices.Clone(prefix), args...))
		return "python3 -I " + scriptExpr + " " + tail
	}
	return shellescape.QuoteCommand(append(slices.Clone(prefix), args...))
}
