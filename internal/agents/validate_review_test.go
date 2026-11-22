package agents

// Findings F7 and F11 of the installer's security review (review 1.2): each
// value below passed the validator before.

import (
	"strings"
	"testing"
)

func TestValidatorClosesReviewGaps(t *testing.T) {
	base := lookup(t, "codex")
	refused := func(label string, mutate func(e *Entry)) {
		t.Helper()
		e := base.clone()
		mutate(&e)
		if _, err := ScriptInput(e, Job{Action: Install, Arch: ARM64, Hidden: true}); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
	// Environment variables: only names under the entry's reviewed vendor
	// prefix, and never one that names a path, a command or a proxy.
	for _, key := range []string{"BASH_ENV", "ENV", "GCONV_PATH", "GLIBC_TUNABLES", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS",
		"GIT_SSH_COMMAND", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "TMPDIR", "EDITOR", "NODE_PATH", "PERL5OPT",
		"JAVA_TOOL_OPTIONS", "ELECTRON_RUN_AS_NODE", "IFS", "SHELLOPTS", "CODEX_HOME", "CODEX_SSL_CERT_FILE", "AGY_CLI_X"} {
		refused("env "+key, func(e *Entry) { e.Env = map[string]string{key: "x"} })
	}
	// Data directories: a dot folder, two levels deep unless reviewed, and
	// never a credential store or one of Cage's own folders unless reviewed.
	for _, dir := range []string{"~/project", "~/Documents", "~/.aws", "~/.kube", "~/.docker", "~/.netrc", "~/.gitconfig",
		"~/.bashrc", "~/.claude-cage", "~/.config/gh", "~/.config/gcloud", "~/.local/share/keyrings",
		"~/.local/share/cage/workspaces", "~/.cache/cage", "~/.aws/sso", "~/.alpha"} {
		refused("data_dir "+dir, func(e *Entry) { e.DataDirs = append(e.DataDirs, DataDir{Path: dir}) })
	}
	// Launcher names: the entry's reviewed command only.
	for _, command := range []string{"ls", "rg", "bwrap", "gh", "mise", "code-server", "chromium", "timeout", "flock", "pip", "jq", "make", "cage-x", "codex2"} {
		refused("command "+command, func(e *Entry) { e.Launch.Command = command; e.Launch.Argv = []string{LauncherDir + "/" + command} })
	}
	// URLs: what is reviewed is what curl fetches.
	for _, url := range []string{
		"https://github.com/openai/codex/releases/download/rust-v0.171.0/../../../x/0.171.0/codex.tar.gz",
		"https://github.com/openai/codex/releases/download/rust-v0.171.0/./codex.tar.gz",
		"https://github.com/openai/codex/releases/download/rust-v0.171.0//codex.tar.gz",
		"https://github.com/openai%2Fcodex/releases/download/rust-v0.171.0/codex.tar.gz",
		"https://github.com/openai/codex/releases/download/rust-v0.171.0/%2e%2e/codex.tar.gz",
	} {
		refused("url "+url, func(e *Entry) { a := e.Artifacts[ARM64]; a.URL = url; e.Artifacts[ARM64] = a })
	}
	// Versions: the three-part form the installer's check reads (F11).
	for _, version := range []string{"0.171", "0.171.0.1", "0.171.0+build.1", "0.171.0-"} {
		refused("version "+version, func(e *Entry) {
			for arch, a := range e.Artifacts {
				a.URL = strings.ReplaceAll(a.URL, e.Version, version)
				e.Artifacts[arch] = a
			}
			e.Version = version
		})
	}
	// Names and notes: no bidi or other format characters.
	for _, text := range []string{"Codex\u202eevil", "Co\u200bdex", "Codex\u2066x\u2069"} {
		refused("name "+text, func(e *Entry) { e.Name = text })
		refused("vendor "+text, func(e *Entry) { e.Vendor = text })
	}
	// Still accepted: the shipped entries and a pre-release pin.
	pre := base.clone()
	for arch, a := range pre.Artifacts {
		a.URL = strings.ReplaceAll(a.URL, pre.Version, "0.172.0-alpha.3")
		pre.Artifacts[arch] = a
	}
	pre.Version = "0.172.0-alpha.3"
	if _, err := ScriptInput(pre, Job{Action: Install, Arch: ARM64, Hidden: true}); err != nil {
		t.Errorf("a pre-release pin: %v", err)
	}
	withEnv := base.clone()
	withEnv.Env = map[string]string{"CODEX_TEST_SWITCH": "off"}
	if _, err := ScriptInput(withEnv, Job{Action: Install, Arch: ARM64, Hidden: true}); err != nil {
		t.Errorf("a variable under the reviewed prefix: %v", err)
	}
}
