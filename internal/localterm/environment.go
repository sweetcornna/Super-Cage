package localterm

import "strings"

// LaunchEnvironment is what a launched helper inherits from Super Cage: only
// the owner's session basics. Nothing of Cage's own process (its
// configuration, start ids, proxies) belongs in a window's shell.
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
