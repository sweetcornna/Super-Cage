package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"al.essio.dev/pkg/shellescape"
)

// Action is what a 其他客户端 job does with an entry. The set is closed.
type Action string

const (
	Install Action = "install"
	Update  Action = "update"
	Remove  Action = "remove"
)

// The installer's own rules for values it puts into a path or a launcher:
// stricter than the catalog's, so that single quotes always suffice.
// internal/cage/agent-install.sh checks the same patterns again.
var (
	scriptMemberPattern = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]*(/[A-Za-z0-9_+-][A-Za-z0-9._+-]*)*$`)
	scriptURLPattern    = regexp.MustCompile(`^https://[a-z0-9.-]+/[A-Za-z0-9._~/%+-]+$`)
)

// ScriptInputNames are the variables ScriptInput assigns, in order.
var ScriptInputNames = []string{
	"AGENT_ACTION", "AGENT_ID", "AGENT_NAME", "AGENT_VENDOR", "AGENT_STATUS", "AGENT_VERSION", "AGENT_COMMAND",
	"AGENT_ARCH", "AGENT_LIBC", "AGENT_MIN_GLIBC", "AGENT_URL", "AGENT_SHA256", "AGENT_SHA512", "AGENT_SIZE",
	"AGENT_UNPACKED", "AGENT_ENTRY", "AGENT_PACKAGES", "AGENT_USERNS", "AGENT_KEYRING", "AGENT_ENV",
	"AGENT_SETTINGS", "AGENT_REMOVE_DATA",
}

// ScriptInput returns the input of internal/cage/agent-install.sh for one job
// (cage.AgentInstallScript): one AGENT_* shell assignment per line, every
// value quoted. Each value comes from e, which is checked here against every
// catalog rule again, or from a closed set (action, arch, flags); nothing else
// reaches the shell. removeData is only for Remove and names the data
// directories to delete; each must be one of e's data_dirs paths (the host
// leaves out shared ones another installed entry still uses).
//
// Visibility is the caller's decision: a hidden entry is scripted like any
// other. Install and Update need an artifact for arch and refuse a withdrawn
// entry; Remove needs neither, so a withdrawn entry stays removable.
func ScriptInput(e Entry, arch Arch, action Action, removeData []string) (string, error) {
	if !e.Optional() {
		return "", fmt.Errorf("agent %s comes with the environment: nothing installs or removes it", e.ID)
	}
	if err := (Catalog{Schema: SchemaVersion, Entries: []Entry{e}}).Validate(); err != nil {
		return "", err
	}
	if arch != ARM64 && arch != AMD64 {
		return "", fmt.Errorf("agent %s: unknown architecture %q", e.ID, arch)
	}
	if e.Kind != Terminal {
		return "", fmt.Errorf("agent %s: only terminal clients install through this script", e.ID)
	}
	var a Artifact
	switch action {
	case Install, Update:
		if e.Status == Withdrawn {
			return "", fmt.Errorf("agent %s is withdrawn: it can only be removed", e.ID)
		}
		var ok bool
		if a, ok = e.Artifact(arch); !ok {
			return "", fmt.Errorf("agent %s has no %s artifact", e.ID, arch)
		}
		if a.Format != FormatTarGz {
			return "", fmt.Errorf("agent %s: %s artifacts are not installed by this script", e.ID, a.Format)
		}
		if !scriptMemberPattern.MatchString(a.Entry) || !scriptURLPattern.MatchString(a.URL) {
			return "", fmt.Errorf("agent %s: the %s url or entry has characters the installer does not take", e.ID, arch)
		}
		if len(removeData) > 0 {
			return "", errors.New("remove data is only for a remove")
		}
	case Remove:
		// Only the digest is passed, so that a partial download is deleted too.
		if pinned, ok := e.Artifact(arch); ok {
			a = Artifact{SHA256: pinned.SHA256, SHA512: pinned.SHA512}
		}
		dirs := make([]string, len(e.DataDirs))
		for i, d := range e.DataDirs {
			dirs[i] = d.Path
		}
		for i, p := range removeData {
			if !slices.Contains(dirs, p) || slices.Contains(removeData[:i], p) {
				return "", fmt.Errorf("agent %s: %q is not one of its data directories, or is listed twice", e.ID, p)
			}
		}
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
	env := make([]string, 0, len(e.Env))
	for _, key := range slices.Sorted(maps.Keys(e.Env)) {
		env = append(env, key+"="+e.Env[key])
	}
	settings := e.Settings
	if settings == nil {
		settings = []Setting{}
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	bytes := func(n int64) string {
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	values := []string{
		string(action), e.ID, e.Name, e.Vendor, string(e.Status), e.Version, e.Launch.Command,
		string(arch), e.Libc, e.MinGlibc, a.URL, a.SHA256, a.SHA512, bytes(a.Size),
		bytes(a.Unpacked), a.Entry, strings.Join(e.Requires.Packages, " "), flag(e.Requires.UserNS), flag(e.Requires.Keyring), strings.Join(env, " "),
		string(settingsJSON), strings.Join(removeData, "\n"),
	}
	var b strings.Builder
	for i, name := range ScriptInputNames {
		b.WriteString(name + "=" + shellescape.Quote(values[i]) + "\n")
	}
	return b.String(), nil
}
