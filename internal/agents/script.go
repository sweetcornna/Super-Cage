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

// Job is what one 其他客户端 job does with an entry.
type Job struct {
	Action Action
	Arch   Arch
	// RemoveData also deletes, on Remove, the data directories
	// RemovableData returns. The caller never names them.
	RemoveData bool
	// Hidden lets a developer build install or update a hidden entry. It is
	// an explicit flag: release builds never set it, so a hidden entry can
	// only be removed there.
	Hidden bool
}

// Script is the input of internal/cage/agent-install.sh for one job. Only
// ScriptInput makes one, so no caller can splice its own text into a job.
type Script struct{ text string }

// String returns the AGENT_* assignments.
func (s Script) String() string { return s.text }

// ScriptInput returns the input of internal/cage/agent-install.sh for one job
// (cage.AgentInstallScript): one AGENT_* shell assignment per line, every
// value quoted. Each value comes from e, which is checked here against every
// catalog rule again, or from a closed set (action, arch, flags); nothing else
// reaches the shell.
//
// Install and Update need an artifact for the architecture, refuse a
// withdrawn entry, and refuse a hidden one unless job.Hidden. Remove needs
// none of that, so any entry stays removable; with job.RemoveData its data
// directories are RemovableData's.
func ScriptInput(e Entry, job Job) (Script, error) {
	action, arch := job.Action, job.Arch
	if !e.Optional() {
		return Script{}, fmt.Errorf("agent %s comes with the environment: nothing installs or removes it", e.ID)
	}
	if err := (Catalog{Schema: SchemaVersion, Entries: []Entry{e}}).Validate(); err != nil {
		return Script{}, err
	}
	if arch != ARM64 && arch != AMD64 {
		return Script{}, fmt.Errorf("agent %s: unknown architecture %q", e.ID, arch)
	}
	if e.Kind != Terminal {
		return Script{}, fmt.Errorf("agent %s: only terminal clients install through this script", e.ID)
	}
	var a Artifact
	var removeData []string
	switch action {
	case Install, Update:
		if e.Status == Withdrawn {
			return Script{}, fmt.Errorf("agent %s is withdrawn: it can only be removed", e.ID)
		}
		if e.Status == Hidden && !job.Hidden {
			return Script{}, fmt.Errorf("agent %s is hidden: only a developer build installs it, and only when asked to", e.ID)
		}
		var ok bool
		if a, ok = e.Artifact(arch); !ok {
			return Script{}, fmt.Errorf("agent %s has no %s artifact", e.ID, arch)
		}
		if a.Format != FormatTarGz {
			return Script{}, fmt.Errorf("agent %s: %s artifacts are not installed by this script", e.ID, a.Format)
		}
		if !scriptMemberPattern.MatchString(a.Entry) || !scriptURLPattern.MatchString(a.URL) {
			return Script{}, fmt.Errorf("agent %s: the %s url or entry has characters the installer does not take", e.ID, arch)
		}
		if job.RemoveData {
			return Script{}, errors.New("remove data is only for a remove")
		}
	case Remove:
		// The digest, so that a partial download is deleted too, and the
		// entry, so that the installer knows its own launcher. No URL.
		if pinned, ok := e.Artifact(arch); ok && pinned.Format == FormatTarGz {
			a = Artifact{SHA256: pinned.SHA256, SHA512: pinned.SHA512}
			if scriptMemberPattern.MatchString(pinned.Entry) {
				a.Entry = pinned.Entry
			}
		}
		if job.RemoveData {
			var err error
			if removeData, err = RemovableData(e); err != nil {
				return Script{}, err
			}
		}
	default:
		return Script{}, fmt.Errorf("unknown action %q", action)
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
		return Script{}, err
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
	return Script{text: b.String()}, nil
}

// RemovableData returns the data directories a remove with data deletes:
// the entry's own, without any it marks shared (another client, in the
// catalog or not, may keep its data there) and without any that is, holds or
// lies inside a directory another catalog entry declares.
func RemovableData(e Entry) ([]string, error) {
	entries, err := All()
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, d := range e.DataDirs {
		if d.Shared {
			continue
		}
		claimed := slices.ContainsFunc(entries, func(other Entry) bool {
			return other.ID != e.ID && slices.ContainsFunc(other.DataDirs, func(o DataDir) bool {
				return within(d.Path, o.Path) || within(o.Path, d.Path)
			})
		})
		if !claimed {
			dirs = append(dirs, d.Path)
		}
	}
	return dirs, nil
}
