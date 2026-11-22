package cage

// Findings of the security review of the installer (review 1.2, F1-F12):
// each test failed before its fix.

import (
	"regexp"
	"slices"
	"strings"
	"testing"

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
