// Package agents is the catalog of agent clients for 设置 → 其他客户端.
//
// The catalog is compiled in (catalog.json) and never fetched: new entries,
// new pins and withdrawals arrive only with a Super Cage release. It is
// parsed on first use, so a session that only uses Claude never pays for it.
// Claude Code's entry only describes how the environment already provides
// it; every other entry is optional and pins one version and one digest per
// architecture. Nothing here downloads, installs or runs anything. How an
// entry is reviewed and added: docs/agent-catalog.md.
package agents

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// SchemaVersion is the catalog format this package reads.
const SchemaVersion = 1

// LauncherDir holds the launcher an install job writes for an optional entry.
// It is on every guest PATH, so no profile file is edited.
const LauncherDir = "/usr/local/bin"

// Status is whether an entry is offered.
type Status string

const (
	// Available entries are offered in 设置 → 其他客户端.
	Available Status = "available"
	// Hidden entries are compiled in and never shown in a release build: they
	// wait for their real-guest acceptance.
	Hidden Status = "hidden"
	// Withdrawn entries cannot be installed; an installed copy is shown as
	// 不再受支持 with 移除.
	Withdrawn Status = "withdrawn"
)

// Kind is how a client is used.
type Kind string

const (
	Terminal Kind = "terminal"
	App      Kind = "app"
)

// Session is the terminal machinery a client runs in.
type Session string

const (
	// SessionClaude and SessionCodex are the built kinds, with their sessions,
	// accounts and phone turns.
	SessionClaude Session = "existing:claude"
	SessionCodex  Session = "existing:codex"
	// SessionGeneric is the kind agent:<id>.
	SessionGeneric Session = "generic"
)

// Arch is a guest architecture.
type Arch string

const (
	ARM64 Arch = "arm64"
	AMD64 Arch = "amd64"
)

// ParseArch reads a Go or uname -m architecture name.
func ParseArch(s string) (Arch, bool) {
	switch s {
	case "arm64", "aarch64":
		return ARM64, true
	case "amd64", "x86_64":
		return AMD64, true
	}
	return "", false
}

// Format is how an artifact is installed. The set is closed: each format is
// installer code reviewed once, and an entry cannot name anything else.
type Format string

const (
	// FormatTarGz is a gzip tarball unpacked by Cage's installer.
	FormatTarGz Format = "tar.gz"
	// FormatBuiltin marks a client that comes with the environment through
	// its existing recipe; nothing installs through the catalog.
	FormatBuiltin Format = "builtin"
)

// Catalog is the whole file.
type Catalog struct {
	Schema  int     `json:"schema"`
	Entries []Entry `json:"entries"`
}

// Entry is one client. The rules for every field are in validate.go.
type Entry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Vendor    string `json:"vendor"`
	SummaryZH string `json:"summary_zh"`
	// WithEnvironment is true only for a client that setup and the cloud
	// image install today (Claude Code); its entry describes that path.
	WithEnvironment bool `json:"with_environment"`
	// PersonOnly is true when the vendor's terms forbid driving the client
	// from another program: only a person opens its terminal.
	PersonOnly bool    `json:"person_only"`
	Kind       Kind    `json:"kind"`
	Session    Session `json:"session"`
	Status     Status  `json:"status"`
	// Version is the pinned version; empty for a builtin entry.
	Version      string            `json:"version,omitempty"`
	Artifacts    map[Arch]Artifact `json:"artifacts"`
	Libc         string            `json:"libc,omitempty"`
	MinGlibc     string            `json:"min_glibc,omitempty"`
	Requires     Requires          `json:"requires"`
	Env          map[string]string `json:"env,omitempty"`
	Settings     []Setting         `json:"settings,omitempty"`
	Updater      Updater           `json:"updater"`
	Telemetry    Telemetry         `json:"telemetry"`
	Hosts        Hosts             `json:"hosts"`
	Signin       Signin            `json:"signin"`
	DataDirs     []DataDir         `json:"data_dirs,omitempty"`
	ProjectFiles []string          `json:"project_files,omitempty"`
	Launch       Launch            `json:"launch"`
	Mark         Mark              `json:"mark"`
	Regions      Regions           `json:"regions"`
	Terms        Terms             `json:"terms"`
	// Measured is nil until the entry's real-guest acceptance; an available
	// optional entry cannot do without it.
	Measured *Measured `json:"measured"`
	Review   Review    `json:"review"`
}

// Artifact is the pinned download for one architecture.
type Artifact struct {
	Format Format `json:"format"`
	URL    string `json:"url,omitempty"`
	// SHA256 and SHA512 are lowercase hex; at least one is set, as the
	// vendor publishes it.
	SHA256 string `json:"sha256,omitempty"`
	SHA512 string `json:"sha512,omitempty"`
	// Size is the download in bytes, Unpacked the bytes it unpacks to.
	Size     int64 `json:"size,omitempty"`
	Unpacked int64 `json:"unpacked,omitempty"`
	// Entry is the archive member the launcher runs.
	Entry string `json:"entry,omitempty"`
}

// Requires is what the guest needs before the download.
type Requires struct {
	// Packages are Debian package names from a fixed allow-list.
	Packages []string `json:"packages,omitempty"`
	UserNS   bool     `json:"userns"`
	Keyring  bool     `json:"keyring"`
	Display  bool     `json:"display"`
}

// Setting is a set of keys merged into a client's settings file at install;
// other keys in the file are kept.
type Setting struct {
	File   string `json:"file"`
	Format string `json:"format"`
	// Set maps a dotted key to a bool, a string or an integer (json.Number).
	Set map[string]any `json:"set"`
}

// Updater is the client's own updater and how Cage turns it off.
type Updater struct {
	SelfUpdate bool `json:"self_update"`
	// OffBy is env, setting or none.
	OffBy  string `json:"off_by"`
	NoteZH string `json:"note_zh,omitempty"`
}

// Telemetry is what the client reports and whether Cage can preset it off.
type Telemetry struct {
	DefaultOff bool   `json:"default_off"`
	How        string `json:"how"`
	NoteZH     string `json:"note_zh,omitempty"`
}

// Hosts are the client's hosts. Install hosts are enforced by the job's
// download channel; the others are shown.
type Hosts struct {
	Install   []string `json:"install"`
	Signin    []string `json:"signin"`
	Run       []string `json:"run"`
	Telemetry []string `json:"telemetry"`
	NoteZH    string   `json:"note_zh,omitempty"`
}

// Signin is how a person signs in, inside the environment.
type Signin struct {
	Methods        []string `json:"methods"`
	LoginLinkHosts []string `json:"login_link_hosts"`
}

// DataDir is a directory the client keeps in the home. Remove keeps it unless
// asked; Shared marks one another entry may also use.
type DataDir struct {
	Path   string `json:"path"`
	Shared bool   `json:"shared"`
}

// Launch is how the host starts the client. The host builds argv from here,
// never from the guest.
type Launch struct {
	Command string   `json:"command"`
	Argv    []string `json:"argv"`
	CWD     string   `json:"cwd"`
	Title   string   `json:"title"`
}

// Mark is the row's mark: a drawn one (claude, codex) or a monogram.
type Mark struct {
	Builtin  string `json:"builtin,omitempty"`
	Monogram string `json:"monogram,omitempty"`
}

// Regions says where the vendor does not offer the client.
type Regions struct {
	Unavailable []string `json:"unavailable,omitempty"`
	NoteZH      string   `json:"note_zh,omitempty"`
}

// Terms points at the vendor's terms when they shape how Cage may run it.
type Terms struct {
	URL    string `json:"url,omitempty"`
	NoteZH string `json:"note_zh,omitempty"`
}

// Measured is the entry's real-guest acceptance (scripts/measure-agent.sh).
type Measured struct {
	DiskMB     float64 `json:"disk_mb"`
	IdleRSSMB  float64 `json:"idle_rss_mb"`
	IdleCPUPct float64 `json:"idle_cpu_pct"`
	InstallS   float64 `json:"install_s"`
	Guest      string  `json:"guest"`
	Date       string  `json:"date"`
}

var measuredKeys = []string{"disk_mb", "idle_rss_mb", "idle_cpu_pct", "install_s", "guest", "date"}

// UnmarshalJSON requires every measurement. A null or missing one would
// otherwise read as zero, which is a valid idle CPU figure.
func (m *Measured) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range measuredKeys {
		if value, ok := fields[key]; !ok || string(bytes.TrimSpace(value)) == "null" {
			return fmt.Errorf("measured.%s is missing: measure the entry or set measured to null", key)
		}
	}
	type plain Measured
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(m))
}

// Review is the entry's provenance.
type Review struct {
	By      string   `json:"by"`
	Date    string   `json:"date"`
	Sources []Source `json:"sources"`
	Notes   []string `json:"notes,omitempty"`
}

// Source is a page read for the review, with the day it was read.
type Source struct {
	URL  string `json:"url"`
	Read string `json:"read"`
}

// Optional reports whether the person adds the client: every entry but the
// ones that come with the environment.
func (e Entry) Optional() bool { return !e.WithEnvironment }

// Artifact returns the pinned artifact for arch.
func (e Entry) Artifact(arch Arch) (Artifact, bool) {
	a, ok := e.Artifacts[arch]
	return a, ok
}

// Parse decodes a catalog strictly (unknown fields, trailing data and a
// partly filled measurement are errors) and validates it.
func Parse(data []byte) (Catalog, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var c Catalog
	if err := decoder.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("agent catalog: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Catalog{}, errors.New("agent catalog: data after the catalog")
	}
	if err := c.Validate(); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

//go:embed catalog.json
var catalogJSON []byte

// decodes counts catalog parses; tests use it to prove the parse is lazy.
var decodes atomic.Int64

func lazy(data []byte) func() (Catalog, error) {
	return sync.OnceValues(func() (Catalog, error) {
		decodes.Add(1)
		return Parse(data)
	})
}

var shipped = lazy(catalogJSON)

// All returns every entry in catalog order, hidden and withdrawn ones
// included. The entries are copies.
func All() ([]Entry, error) {
	c, err := shipped()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, len(c.Entries))
	for i, e := range c.Entries {
		entries[i] = e.clone()
	}
	return entries, nil
}

// Visible returns the entries a release build may show: all but the hidden
// ones. A withdrawn entry is listed so that an installed copy can be shown
// and removed.
func Visible() ([]Entry, error) {
	entries, err := All()
	return slices.DeleteFunc(entries, func(e Entry) bool { return e.Status == Hidden }), err
}

// Lookup returns the entry with id, whatever its status.
func Lookup(id string) (Entry, bool, error) {
	return find(func(e Entry) bool { return e.ID == id })
}

// ByCommand returns the optional entry whose launcher is LauncherDir/command,
// whatever its status: a presence report names commands, not ids.
func ByCommand(command string) (Entry, bool, error) {
	return find(func(e Entry) bool { return e.Optional() && e.Launch.Command == command })
}

func find(match func(Entry) bool) (Entry, bool, error) {
	c, err := shipped()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range c.Entries {
		if match(e) {
			return e.clone(), true, nil
		}
	}
	return Entry{}, false, nil
}

// clone copies every map, slice and pointer, so a caller cannot change the
// shared catalog.
func (e Entry) clone() Entry {
	c := e
	c.Artifacts = maps.Clone(e.Artifacts)
	c.Requires.Packages = slices.Clone(e.Requires.Packages)
	c.Env = maps.Clone(e.Env)
	c.Settings = slices.Clone(e.Settings)
	for i := range c.Settings {
		c.Settings[i].Set = maps.Clone(e.Settings[i].Set)
	}
	c.Hosts.Install = slices.Clone(e.Hosts.Install)
	c.Hosts.Signin = slices.Clone(e.Hosts.Signin)
	c.Hosts.Run = slices.Clone(e.Hosts.Run)
	c.Hosts.Telemetry = slices.Clone(e.Hosts.Telemetry)
	c.Signin.Methods = slices.Clone(e.Signin.Methods)
	c.Signin.LoginLinkHosts = slices.Clone(e.Signin.LoginLinkHosts)
	c.DataDirs = slices.Clone(e.DataDirs)
	c.ProjectFiles = slices.Clone(e.ProjectFiles)
	c.Launch.Argv = slices.Clone(e.Launch.Argv)
	c.Regions.Unavailable = slices.Clone(e.Regions.Unavailable)
	if e.Measured != nil {
		m := *e.Measured
		c.Measured = &m
	}
	c.Review.Sources = slices.Clone(e.Review.Sources)
	c.Review.Notes = slices.Clone(e.Review.Notes)
	return c
}
