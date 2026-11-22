package agents

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The installer re-checks its input at least as strictly as the validator
// (review F4): its fixed lists are the validator's.
func TestInstallerListsMatchTheValidator(t *testing.T) {
	data, err := os.ReadFile("../cage/agent-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	list := func(name string) []string {
		t.Helper()
		m := regexp.MustCompile("\n" + name + "='([^']*)'\n").FindStringSubmatch(script)
		if m == nil {
			t.Fatalf("the installer has no %s", name)
		}
		return strings.Fields(m[1])
	}
	same := func(name string, got, want []string) {
		t.Helper()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: installer %v, validator %v", name, got, want)
		}
	}
	var commands, env, dirs []string
	for id, a := range reviewed {
		if id == "alpha" || id == "beta" { // validate_test.go fixtures
			continue
		}
		commands = append(commands, id+":"+a.command)
		for _, p := range a.envPrefixes {
			env = append(env, id+":"+p)
		}
		for _, d := range a.dataDirs {
			dirs = append(dirs, id+":"+d)
		}
	}
	same("agent_reviewed_commands", list("agent_reviewed_commands"), commands)
	same("agent_reviewed_env", list("agent_reviewed_env"), env)
	same("agent_reviewed_data", list("agent_reviewed_data"), dirs)
	var system []string
	for file, id := range systemSettings {
		system = append(system, id+":"+file)
	}
	same("agent_system_settings", list("agent_system_settings"), system)
	same("agent_packages_allowed", list("agent_packages_allowed"), slices.Collect(maps.Keys(allowedPackages)))
	same("agent_reserved_commands", list("agent_reserved_commands"), slices.Collect(maps.Keys(reservedCommands)))
	same("agent_env_denied_names", list("agent_env_denied_names"), slices.Clone(deniedEnvNames))
	same("agent_env_denied_prefixes", list("agent_env_denied_prefixes"), slices.Clone(deniedEnvPrefixes))
	same("agent_env_denied_suffixes", list("agent_env_denied_suffixes"), slices.Clone(deniedEnvSuffixes))
	same("agent_home_roots", list("agent_home_roots"), slices.Collect(maps.Keys(homeRoots)))
	same("agent_protected", list("agent_protected"), slices.Clone(homeProtected))
	same("agent_cage_own", list("agent_cage_own"), slices.Clone(cageOwn))
	for _, pattern := range regexp.MustCompile("re_version='([^']*)'").FindAllStringSubmatch(script, -1) {
		if pattern[1] != versionPattern.String() {
			t.Errorf("installer version pattern %s, validator %s", pattern[1], versionPattern)
		}
	}
}
