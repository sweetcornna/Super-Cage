package localterm

import (
	"regexp"
	"strings"
)

// LaunchEnvironment is what a launched helper inherits from Super Cage: only
// the owner's session basics. Nothing of Cage's own process (its
// configuration, start ids, proxies) belongs in a window's shell; see
// SessionEnvironment for applications started directly.
func LaunchEnvironment(environ []string) []string {
	kept := make([]string, 0, 16)
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch {
		case name == "HOME", name == "USER", name == "LOGNAME", name == "SHELL", name == "PATH", name == "TMPDIR",
			name == "LANG", strings.HasPrefix(name, "LC_"), name == "__CF_USER_TEXT_ENCODING":
			kept = append(kept, entry)
		}
	}
	return kept
}

// launchdEnviron is the owner's login session environment, as launchd gives
// it to an application opened from the Dock or Finder. Tests replace it.
var launchdEnviron = sessionFromLaunchd

// SessionEnvironment is what an application started directly inherits (kitty
// and WezTerm, which may become the owner's running application): the
// session basics of LaunchEnvironment, then the owner's login session
// variables, which win. That is what the same application started from the
// Dock has, an SSH agent and what the owner set with launchctl setenv
// included, and nothing of Super Cage's own.
func SessionEnvironment(environ, session []string) []string {
	merged := LaunchEnvironment(environ)
	index := make(map[string]int, len(merged))
	for i, entry := range merged {
		name, _, _ := strings.Cut(entry, "=")
		index[name] = i
	}
	for _, entry := range session {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		if i, seen := index[name]; seen {
			merged[i] = entry
		} else {
			index[name] = len(merged)
			merged = append(merged, entry)
		}
	}
	return merged
}

var launchdName = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")

// launchdNames reads the variable names of the environment block of
// launchctl print gui/<uid>: one "NAME => value" line each.
func launchdNames(printed string) []string {
	var names []string
	inside := false
	for _, line := range strings.Split(printed, "\n") {
		switch {
		case line == "\tenvironment = {":
			inside = true
		case inside && line == "\t}":
			return names
		case inside && strings.HasPrefix(line, "\t\t"):
			name, _, ok := strings.Cut(strings.TrimPrefix(line, "\t\t"), " => ")
			if ok && launchdName.MatchString(name) {
				names = append(names, name)
			}
		}
	}
	return names
}
