package cage

// Findings of the security review of the installer (review 1.2, F1-F12):
// each test failed before its fix.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/agents"
)

// dropInput removes one AGENT_* assignment from a job's input.
func dropInput(input, name string) string {
	var keep []string
	for _, line := range strings.Split(input, "\n") {
		if !strings.HasPrefix(line, name+"=") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// The installer's own list of inputs is ScriptInputNames.
func TestAgentInstallInputListMatchesScriptInput(t *testing.T) {
	match := regexp.MustCompile(`for name in (AGENT_[A-Z0-9_ ]+); do`).FindStringSubmatch(agentInstall)
	if match == nil || !slices.Equal(strings.Fields(match[1]), agents.ScriptInputNames) {
		t.Fatalf("the installer declares %v, ScriptInput assigns %v", match, agents.ScriptInputNames)
	}
}

// F5: a missing input variable is a host bug: exit 2 with a failure line,
// never a crash without one and never an install without its settings.
func TestAgentInstallEveryInputIsDeclared(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	inputs := map[string]string{"install": r.input(e, agents.Install), "remove": r.input(e, agents.Remove)}
	for _, name := range agents.ScriptInputNames {
		for action, input := range inputs {
			if !strings.Contains(input, "\n"+name+"=") && !strings.HasPrefix(input, name+"=") {
				t.Fatalf("%s input has no %s", action, name)
			}
			result := r.run(dropInput(input, name))
			if result.code != 2 || result.failed != "input" || !strings.Contains(result.out, name) {
				t.Errorf("%s without %s: exit %d failed=%q\n%s", action, name, result.code, result.failed, result.out)
			}
		}
	}
	if r.exists(r.lib) || r.exists(r.state) || r.server.downloads() != 0 {
		t.Fatal("a refused input changed the guest or downloaded")
	}
}

// hookedRig runs REVIEW_HOOK (a sh fragment) at every sudo call: a
// same-user process racing the job, as in the review.
func hookedRig(t *testing.T) *agentRig {
	r := newAgentRig(t)
	r.write(filepath.Join(r.bin, "sudo"), `#!/bin/sh
printf '%s\n' "$*" >>"$TEST_ROOT/sudo.log"
if [ -n "${REVIEW_HOOK:-}" ]; then . "$REVIEW_HOOK"; fi
case "$1" in
  chown) exit 0 ;;
  env) exec "$@" ;;
esac
for argument in "$@"; do
  case "$argument" in
    "$TEST_ROOT"/*) ;;
    /*) echo "sudo stand-in: $argument is outside the test root" >&2; exit 97 ;;
  esac
done
exec "$@"
`, 0o755)
	return r
}

// setInput replaces one AGENT_* assignment of a job's input.
func setInput(t *testing.T, input, name, value string) string {
	t.Helper()
	start := strings.Index(input, name+"=")
	if start < 0 || (start > 0 && input[start-1] != '\n') {
		t.Fatalf("no %s in the input", name)
	}
	end := start + strings.Index(input[start:], "\n")
	return input[:start] + name + "=" + shellescape.Quote(value) + input[end:]
}

// F4: the installer re-checks its input at least as strictly as the
// validator, so a host bug cannot widen what root writes or deletes.
func TestAgentInstallChecksInputAsStrictlyAsTheValidator(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	install, remove := r.input(e, agents.Install), r.input(e, agents.Remove)
	setting := func(file string) string {
		format := strings.TrimPrefix(filepath.Ext(file), ".")
		return `[{"file":"` + file + `","format":"` + format + `","set":{"a":true}}]`
	}
	for _, tc := range []struct{ input, name, value string }{
		{install, "AGENT_SETTINGS", setting("~/.claude/settings.json")},
		{install, "AGENT_SETTINGS", setting("~/.ssh/config.json")},
		{install, "AGENT_SETTINGS", setting("~/x.json")},
		{install, "AGENT_SETTINGS", setting("~/.bashrc.toml")},
		{install, "AGENT_SETTINGS", setting("/etc/sudoers.d/x.json")},
		{install, "AGENT_SETTINGS", setting("/etc/cron.d/x.json")},
		{install, "AGENT_PACKAGES", "curl"},
		{install, "AGENT_COMMAND", "ls"},
		{install, "AGENT_COMMAND", "cage-x"},
		{install, "AGENT_COMMAND", "agy"},
		{install, "AGENT_ENV", "BASH_ENV=x"},
		{install, "AGENT_ENV", "AGY_CLI_DISABLE_AUTO_UPDATE=true"},
		{install, "AGENT_ENV", "CODEX_HOME=/tmp/x"},
		{install, "AGENT_URL", "https://github.com/openai%2Fcodex/releases/download/rust-v0.171.0/codex.tar.gz"},
		{install, "AGENT_URL", "https://github.com/openai/codex/releases/download/rust-v0.171.0/../../x/codex.tar.gz"},
		{install, "AGENT_VERSION", "0.171"},
		{remove, "AGENT_REMOVE_DATA", "~/.aws"},
		{remove, "AGENT_REMOVE_DATA", "~/project"},
		{remove, "AGENT_REMOVE_DATA", "~/.ssh/x"},
		{remove, "AGENT_REMOVE_DATA", "~/.local/share/cage/workspaces"},
		{remove, "AGENT_REMOVE_DATA", "~/.config/gh"},
	} {
		result := r.run(setInput(t, tc.input, tc.name, tc.value))
		if result.code != 2 || result.failed != "input" {
			t.Errorf("%s=%s: exit %d failed=%q\n%s", tc.name, tc.value, result.code, result.failed, result.out)
		}
	}
	if r.exists(filepath.Join(r.home, ".claude")) || r.exists(filepath.Join(r.etc, "sudoers.d")) || r.server.downloads() != 0 {
		t.Fatal("a refused input changed the guest")
	}
}

// F1: root never takes a destination, a mode or content from the job's
// temporary folder, which the guest user (and a sandboxed client of that
// user) can write. Here every file in it is rewritten at every sudo call.
func TestAgentInstallRootTakesNothingFromTmp(t *testing.T) {
	r := hookedRig(t)
	hook := filepath.Join(r.root, "hook.sh")
	r.write(hook, `for tmp in "$TEST_ROOT"/tmp/agent-clients.*; do
  [ -d "$tmp" ] || continue
  mkdir -p "$tmp/settings"
  printf '0 system changed created dir-created 440 %s\n' "$TEST_ROOT/etc/sudoers.d/cage-evil" >"$tmp/settings/list.txt"
  printf 'guest ALL=(ALL) NOPASSWD: ALL\n' >"$tmp/settings/0"
  printf 'remove 0 %s\n' "$TEST_ROOT/etc/important.conf" >"$tmp/removable"
  for f in "$tmp"/* "$tmp"/*/*; do
    case "$f" in */version | */reason) continue ;; esac
    if [ -f "$f" ]; then printf 'guest ALL=(ALL) NOPASSWD: ALL\n' >"$f"; fi
  done
done
`, 0o644)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	result := r.run(r.input(e, agents.Install), "REVIEW_HOOK="+hook)
	if result.code != 0 || result.done != "0.171.0" {
		t.Fatalf("install: exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
	if r.exists(filepath.Join(r.etc, "sudoers.d")) {
		t.Fatalf("root wrote where a temporary file said:\n%s", r.read(filepath.Join(r.root, "sudo.log")))
	}
	for _, path := range []string{filepath.Join(r.etc, "codex/config.toml"), filepath.Join(r.launchers, "codex"), filepath.Join(r.state, "codex.json"), filepath.Join(r.state, "codex.stat")} {
		if strings.Contains(r.read(path), "NOPASSWD") {
			t.Fatalf("root wrote a temporary file's content to %s", path)
		}
	}
	if !strings.Contains(r.read(filepath.Join(r.etc, "codex/config.toml")), "check_for_update_on_startup = false") {
		t.Fatal("the settings were not written")
	}
	victim := filepath.Join(r.etc, "important.conf")
	r.write(victim, "keep me\n", 0o644)
	result = r.run(r.input(e, agents.Remove), "REVIEW_HOOK="+hook)
	if result.code != 0 || !result.removed || !r.exists(victim) {
		t.Fatalf("remove: exit %d removed=%v victim kept=%v\n%s", result.code, result.removed, r.exists(victim), result.out)
	}
}

// F9: a program that carries Cage's marker line but is not the launcher Cage
// wrote stays: install refuses, remove keeps it.
func TestAgentInstallKnowsItsLauncherByHash(t *testing.T) {
	r := newAgentRig(t)
	mine := "#!/bin/sh\n# super-cage-agent-launcher codex\necho mine\n"
	launcher := filepath.Join(r.launchers, "codex")
	r.write(launcher, mine, 0o755)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	if result := r.run(r.input(e, agents.Install)); result.failed != "own-install" || r.read(launcher) != mine {
		t.Fatalf("install over a marked program: exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
	if result := r.run(r.input(e, agents.Remove)); result.code != 0 || r.read(launcher) != mine {
		t.Fatalf("remove deleted a marked program: exit %d\n%s", result.code, result.out)
	}
}

// F9: a program of the same name that appears during the job (a sudo npm -g
// while the download runs) is never replaced: the check runs again right
// before the commit.
func TestAgentInstallChecksOwnInstallAgainBeforeCommit(t *testing.T) {
	r := hookedRig(t)
	hook := filepath.Join(r.root, "hook.sh")
	r.write(hook, `if [ "$1" = chown ] && [ "$2" = -hR ]; then printf '#!/bin/sh\necho npm\n' >"$TEST_ROOT/usr/local/bin/codex"; chmod 755 "$TEST_ROOT/usr/local/bin/codex"; fi
`, 0o644)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	result := r.run(r.input(e, agents.Install), "REVIEW_HOOK="+hook)
	if result.failed != "own-install" || r.read(filepath.Join(r.launchers, "codex")) != "#!/bin/sh\necho npm\n" {
		t.Fatalf("exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
}

// F10: temporary names a canceled job left behind go at the next job.
func TestAgentInstallClearsLeftoverTemporaryFiles(t *testing.T) {
	r := newAgentRig(t)
	leftovers := []string{
		filepath.Join(r.launchers, ".codex.agent-new"), filepath.Join(r.etc, "codex/.agent-settings-new"),
		filepath.Join(r.state, ".codex.json.new"), filepath.Join(r.state, ".codex.stat.new"),
	}
	plant := func() {
		for _, path := range leftovers {
			r.write(path, "left\n", 0o644)
		}
	}
	plant()
	e, _ := r.install("0.171.0")
	for _, path := range leftovers {
		if r.exists(path) {
			t.Errorf("install left %s", path)
		}
	}
	plant()
	if result := r.run(r.input(e, agents.Remove)); result.code != 0 {
		t.Fatal(result.out)
	}
	for _, path := range leftovers {
		if r.exists(path) {
			t.Errorf("remove left %s", path)
		}
	}
}

// F2: the digest is over the bytes that are unpacked: an archive swapped
// after the digest check is refused.
func TestAgentInstallDigestCoversTheBytesUnpacked(t *testing.T) {
	r := newAgentRig(t)
	evil := agentTarGz(t, tarMember{name: agentEntryName, body: "#!/bin/sh\necho EVIL\necho codex-cli 0.171.0\n", mode: 0o755})
	r.write(filepath.Join(r.root, "evil.tar.gz"), string(evil), 0o644)
	wrapper := filepath.Join(r.root, "pywrap")
	r.write(wrapper, "#!/bin/sh\nif [ \"$3\" = archive-check ]; then cp \"$TEST_ROOT/evil.tar.gz\" \"$4\"; fi\nexec '"+r.python+"' \"$@\"\n", 0o755)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	result := r.runScript(r.compose(r.input(e, agents.Install), [2]string{"agent_python=" + r.python + "\n", "agent_python=" + wrapper + "\n"}))
	if result.failed != "digest" || r.exists(filepath.Join(r.lib, "codex")) {
		t.Fatalf("a swapped archive was unpacked: exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
}

// F2: what is committed is what was unpacked: a tree swapped in the stage
// (here while the version check runs) is refused.
func TestAgentInstallCommitsTheTreeItUnpacked(t *testing.T) {
	r := newAgentRig(t)
	swap := "#!/bin/sh\nfor tree in '" + r.lib + "'/.stage-*/tree; do\n" +
		"  mv \"$tree\" \"$tree.pinned\" && mkdir \"$tree\" && printf '#!/bin/sh\\necho SWAPPED\\necho codex-cli 0.171.0\\n' >\"$tree/" + agentEntryName + "\" && chmod 755 \"$tree/" + agentEntryName + "\"\ndone\n" +
		"if [ \"$1\" = -k ]; then shift 2; fi\nshift\nexec \"$@\"\n"
	r.write(filepath.Join(r.bin, "timeout"), swap, 0o755)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	result := r.run(r.input(e, agents.Install))
	installed, _ := os.ReadFile(filepath.Join(r.lib, "codex/0.171.0", agentEntryName))
	if result.code == 0 || strings.Contains(string(installed), "SWAPPED") {
		t.Fatalf("a swapped stage was committed: exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
}

// F2: the tree belongs to root and holds no hard links. Here the owner the
// installer expects is another user's, and a hard link makes a repair.
func TestAgentInstallTreeOwnerAndLinks(t *testing.T) {
	r := newAgentRig(t)
	body := r.client("0.171.0")
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	other := strconv.Itoa(os.Getuid() + 1)
	result := r.runScript(r.compose(r.input(e, agents.Install), [2]string{"agent_root_uid=" + strconv.Itoa(os.Getuid()) + "\n", "agent_root_uid=" + other + "\n"}))
	if result.failed != "archive" || r.exists(filepath.Join(r.lib, "codex")) {
		t.Fatalf("a tree of the wrong owner was committed: exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
	e, _ = r.install("0.171.0")
	if err := os.Link(filepath.Join(r.lib, "codex/0.171.0", agentEntryName), filepath.Join(r.home, "linked")); err != nil {
		t.Fatal(err)
	}
	downloads := r.server.downloads()
	again := r.run(r.input(e, agents.Install))
	if again.code != 0 || strings.Contains(again.out, "已是最新") || r.server.downloads() == downloads {
		t.Fatalf("a hard link in the tree passed as up to date:\n%s", again.out)
	}
}

// F3: a settings file Cage created and the person then edited is theirs:
// a later repair does not claim it back, and remove keeps it.
func TestAgentInstallKeepsAnEditedSettingsFile(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	config := filepath.Join(r.etc, "codex/config.toml")
	edited := r.read(config) + "model = \"mine\"\n"
	r.write(config, edited, 0o644)
	if again := r.run(r.input(e, agents.Install)); again.code != 0 {
		t.Fatal(again.out)
	}
	if result := r.run(r.input(e, agents.Remove)); result.code != 0 || !r.exists(config) || r.read(config) != edited {
		t.Fatalf("remove deleted an edited settings file:\n%s", result.out)
	}
}

// F6: the version check runs in an empty environment from the start, in /,
// with bounded output, and does not wait for a child the client leaves.
func TestAgentInstallVerifyStepIsBounded(t *testing.T) {
	r := newAgentRig(t)
	r.write(filepath.Join(r.bin, "timeout"), "#!/bin/sh\nenv >'"+r.root+"/timeout-env'\nif [ \"$1\" = -k ]; then shift 2; fi\nshift\nexec \"$@\"\n", 0o755)
	client := "#!/bin/sh\nif [ \"$1\" = --version ]; then pwd >'" + r.root + "/verify-pwd'; (/bin/sleep 20 &); echo 'codex-cli 0.171.0'; exit 0; fi\n"
	body := agentTarGz(t, tarMember{name: agentEntryName, body: client, mode: 0o755})
	e := r.entry("0.171.0", body)
	r.server.publish(e, body)
	start := time.Now()
	result := r.run(r.input(e, agents.Install))
	if result.code != 0 || time.Since(start) > 12*time.Second {
		t.Fatalf("exit %d after %v\n%s", result.code, time.Since(start), result.out)
	}
	if env := r.read(filepath.Join(r.root, "timeout-env")); strings.Contains(env, "proxy-secret") || strings.Contains(env, "CAGE_") {
		t.Fatalf("the version check's timeout saw the job's environment:\n%s", env)
	}
	if pwd := strings.TrimSpace(r.read(filepath.Join(r.root, "verify-pwd"))); pwd != "/" {
		t.Fatalf("the version check ran in %s", pwd)
	}
	loud := agentTarGz(t, tarMember{name: agentEntryName, mode: 0o755,
		body: "#!/bin/sh\nif [ \"$1\" = --version ]; then head -c 4000000 /dev/zero; echo 'codex-cli 0.172.0'; exit 0; fi\n"})
	e = r.entry("0.172.0", loud)
	r.server.publish(e, loud)
	// The version after 4 MB of output is never read.
	if result := r.run(r.input(e, agents.Update)); result.failed != "verify" || r.current() != "0.171.0" {
		t.Fatalf("unbounded version output: exit %d failed=%q\n%s", result.code, result.failed, result.out)
	}
}

// F11: a pre-release orders below its release, as semver says (dpkg reads
// the part after "-" as a Debian revision and orders it above).
func TestAgentInstallOrdersPreReleases(t *testing.T) {
	r := newAgentRig(t)
	r.write(filepath.Join(r.bin, "dpkg"), "#!/bin/sh\n[ \"$1\" = --compare-versions ] || exit 2\nexec \"$TEST_PYTHON\" -c 'import re, sys\n"+
		"def key(v):\n    up, _, rev = v.partition(\"-\")\n    return [int(x) for x in re.findall(r\"[0-9]+\", up)], rev\n"+
		"a, op, b = sys.argv[1:4]\nsys.exit(0 if {\"gt\": key(a) > key(b), \"ge\": key(a) >= key(b)}[op] else 1)' \"$2\" \"$3\" \"$4\"\n", 0o755)
	rc, _ := r.install("1.0.0-rc.1")
	if r.current() != "1.0.0-rc.1" {
		t.Fatal(r.current())
	}
	body := r.client("1.0.0")
	release := r.entry("1.0.0", body)
	r.server.publish(release, body)
	if result := r.run(r.input(release, agents.Update)); result.done != "1.0.0" || r.current() != "1.0.0" {
		t.Fatalf("the release did not replace its pre-release: %s\n%s", r.current(), result.out)
	}
	if result := r.run(r.input(rc, agents.Install)); result.done != "1.0.0" || r.current() != "1.0.0" {
		t.Fatalf("went back to the pre-release: %s\n%s", r.current(), result.out)
	}
}

// F12: a data folder reached through a link into a protected folder is not
// deleted.
func TestAgentRemoveDataChecksTheResolvedPath(t *testing.T) {
	r := newAgentRig(t)
	e, _ := r.install("0.171.0")
	keep := filepath.Join(r.home, ".ssh/codex-accounts/keep")
	r.write(keep, "x", 0o600)
	if err := os.MkdirAll(filepath.Join(r.home, ".local/share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.home, ".ssh"), filepath.Join(r.home, ".local/share/cage")); err != nil {
		t.Fatal(err)
	}
	result := r.run(r.inputData(e))
	if result.code != 0 || !r.exists(keep) {
		t.Fatalf("deleted inside ~/.ssh through a link: exit %d\n%s", result.code, result.out)
	}
}
