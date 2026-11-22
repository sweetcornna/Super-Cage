package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// IdleCPUBudget is the most an available client may use at its prompt, in
// percent of one core over ten minutes (design 9.2).
const IdleCPUBudget = 0.5

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	commandPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// Three numbers, as the installer's version check reads them, and an
	// optional pre-release; no build suffix, which would not order.
	versionPattern  = regexp.MustCompile(`^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(-[0-9A-Za-z][0-9A-Za-z.-]{0,31})?$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sha512Pattern   = regexp.MustCompile(`^[0-9a-f]{128}$`)
	minGlibcPattern = regexp.MustCompile(`^2\.[0-9]{1,2}$`)
	envKeyPattern   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	envValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.,:/@%+=-]{0,256}$`)
	settingKey      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}(\.[A-Za-z][A-Za-z0-9_-]{0,63}){0,3}$`)
	hostPattern     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	regionPattern   = regexp.MustCompile(`^[A-Z]{2}$`)
)

// maintenanceHosts is today's download channel: maintenanceOrigins and the
// CONNECT-only hosts of maintenanceDestination in
// internal/workbench/maintenance.go. Artifacts are https, so CONNECT is
// enough. TestMaintenanceHostsMatchWorkbench keeps this copy honest.
var maintenanceHosts = map[string]bool{
	"deb.debian.org": true, "security.debian.org": true,
	"archive.ubuntu.com": true, "security.ubuntu.com": true, "ports.ubuntu.com": true,
	"github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true,
	"codeload.github.com": true, "registry.npmjs.org": true, "cdn.npmmirror.com": true,
	"downloads.claude.ai": true, "deb.nodesource.com": true, "storage.googleapis.com": true,
	"mirrors.nju.edu.cn": true, "mirrors.ustc.edu.cn": true,
}

// extraInstallHosts lists, per entry id, download hosts outside today's
// channel. Adding one is a code change with a security review, and the
// workbench must allow it for that entry's jobs (design 3.2, 1.3).
var extraInstallHosts = map[string][]string{}

// reviewed is what a security review allowed each optional entry beyond the
// general rules: its launcher name, the vendor prefixes of the environment
// variables its launcher may set, and data directories that lie one level
// below the home or inside one of Cage's own folders. A new entry, or a new
// name for one, is a code change with a review (docs/agent-catalog.md);
// internal/cage/agent-install.sh holds the same commands and prefixes.
var reviewed = map[string]allowance{
	"codex": {command: "codex", envPrefixes: []string{"CODEX_"},
		dataDirs: []string{"~/.codex", "~/.local/share/cage/codex-accounts", "~/.local/share/cage/codex-sessions"}},
	"antigravity-cli": {command: "agy", envPrefixes: []string{"AGY_"}},
}

type allowance struct {
	command     string
	envPrefixes []string
	dataDirs    []string
}

// allowedPackages are the Debian packages an entry may ask the job to install.
var allowedPackages = map[string]bool{"bubblewrap": true}

// systemSettings are the files outside the home an entry may write, and the
// entry that may write each.
var systemSettings = map[string]string{"/etc/codex/config.toml": "codex"}

// reservedCommands are launcher names an entry cannot take: what Cage
// installs in /usr/local/bin (plus every cage-* name), and programs a
// launcher earlier on PATH would shadow.
// It backs up the reviewed list, which alone decides.
var reservedCommands = map[string]bool{
	"cage": true, "claude": true, "ssh": true, "xdg-open": true,
	"sh": true, "bash": true, "dash": true, "zsh": true, "sudo": true, "su": true, "env": true,
	"login": true, "passwd": true, "scp": true, "sftp": true, "git": true, "python": true, "python3": true,
	"node": true, "npm": true, "npx": true, "curl": true, "wget": true, "tar": true,
	"apt": true, "apt-get": true, "dpkg": true, "systemctl": true,
	"ls": true, "rg": true, "bwrap": true, "gh": true, "mise": true, "code-server": true, "chromium": true,
	"timeout": true, "flock": true, "pip": true, "pip3": true, "jq": true, "make": true, "unshare": true,
	"uv": true, "uvx": true, "gpg": true, "tmux": true, "vim": true, "vi": true, "nano": true,
}

// homeRoots are shared directories no entry may claim as a whole.
var homeRoots = map[string]bool{
	".config": true, ".local": true, ".local/bin": true, ".local/share": true, ".local/state": true,
	".cache": true, ".local/share/cage": true,
}

// homeProtected are trees no entry may claim or write into: logins and
// credential stores, places that start programs, and Claude's own.
var homeProtected = []string{
	".ssh", ".gnupg", ".pki", ".claude", ".aws", ".kube", ".docker", ".netrc", ".gitconfig", ".git-credentials",
	".password-store", ".config/gh", ".config/gcloud", ".config/git", ".config/systemd", ".config/autostart",
	".local/share/keyrings", ".mozilla", ".config/chromium",
}

// cageOwn are Cage's folders in the home: an entry claims a directory in
// one only when reviewed.
var cageOwn = []string{".local/share/cage", ".cache/cage", ".claude-cage", ".config/cage"}

var signinMethods = map[string]bool{
	"browser-code": true, "browser-loopback": true, "device-code": true, "api-key": true, "adc": true,
}

type checker struct{ problems []string }

func (v *checker) add(id, field, format string, args ...any) {
	if id == "" {
		id = "catalog"
	}
	v.problems = append(v.problems, id+": "+field+": "+fmt.Sprintf(format, args...))
}

// Validate checks every rule (docs/agent-catalog.md) and reports all
// violations together.
func (c Catalog) Validate() error {
	var v checker
	if c.Schema != SchemaVersion {
		v.add("", "schema", "is %d, want %d", c.Schema, SchemaVersion)
	}
	if len(c.Entries) == 0 {
		v.add("", "entries", "the catalog is empty")
	}
	ids := map[string]bool{}
	commands := map[string]string{}
	sessions := map[Session]string{}
	for _, e := range c.Entries {
		v.entry(e)
		if ids[e.ID] {
			v.add(e.ID, "id", "used by two entries")
		}
		ids[e.ID] = true
		if command := e.Launch.Command; command != "" {
			if other, ok := commands[command]; ok {
				v.add(e.ID, "launch.command", "%q is also %s's", command, other)
			}
			commands[command] = e.ID
		}
		if e.Session == SessionClaude || e.Session == SessionCodex {
			if other, ok := sessions[e.Session]; ok {
				v.add(e.ID, "session", "%s is also %s's", e.Session, other)
			}
			sessions[e.Session] = e.ID
		}
	}
	v.overlaps(c.Entries)
	if len(v.problems) == 0 {
		return nil
	}
	return errors.New("agent catalog:\n  " + strings.Join(v.problems, "\n  "))
}

func (v *checker) entry(e Entry) {
	id := e.ID
	if !idPattern.MatchString(id) {
		v.add(id, "id", "must match %s", idPattern)
	}
	if !text(e.Name, 64) {
		v.add(id, "name", "needs 1 to 64 characters")
	}
	if !text(e.Vendor, 64) {
		v.add(id, "vendor", "needs 1 to 64 characters")
	}
	if !text(e.SummaryZH, 40) {
		v.add(id, "summary_zh", "needs 1 to 40 characters")
	}
	if e.Kind != Terminal && e.Kind != App {
		v.add(id, "kind", "%q is not terminal or app", e.Kind)
	}
	switch e.Session {
	case SessionClaude, SessionCodex, SessionGeneric:
	default:
		v.add(id, "session", "%q is not existing:claude, existing:codex or generic", e.Session)
	}
	if e.Kind == App && e.Session != SessionGeneric {
		v.add(id, "session", "an app has no built terminal kind; use generic")
	}
	if e.PersonOnly && e.Session != SessionGeneric {
		v.add(id, "person_only", "needs session generic: the built kinds take phone turns and agent-opened terminals")
	}
	if e.Status != Available && e.Status != Hidden && e.Status != Withdrawn {
		v.add(id, "status", "%q is not available, hidden or withdrawn", e.Status)
	}
	v.artifacts(e)
	if e.WithEnvironment {
		v.builtin(e)
	} else {
		v.optional(e)
	}
	v.hosts(e)
	v.dataDirs(e)
	for _, p := range e.ProjectFiles {
		if !cleanRelative(p) {
			v.add(id, "project_files", "%q must be a clean relative path", p)
		}
	}
	v.mark(e)
	for _, code := range e.Regions.Unavailable {
		if !regionPattern.MatchString(code) {
			v.add(id, "regions.unavailable", "%q is not a two-letter region code", code)
		}
	}
	optionalText(v, id, "regions.note_zh", e.Regions.NoteZH, 120)
	if e.Terms.URL != "" && !httpsURL(e.Terms.URL) {
		v.add(id, "terms.url", "must be https")
	}
	optionalText(v, id, "terms.note_zh", e.Terms.NoteZH, 120)
	if e.Measured != nil {
		v.measured(id, *e.Measured)
	}
	v.review(e)
}

func (v *checker) artifacts(e Entry) {
	id := e.ID
	builtin := 0
	for _, arch := range slices.Sorted(maps.Keys(e.Artifacts)) {
		a, field := e.Artifacts[arch], "artifacts."+string(arch)
		if arch != ARM64 && arch != AMD64 {
			v.add(id, field, "unknown architecture (arm64 or amd64)")
			continue
		}
		switch a.Format {
		case FormatBuiltin:
			builtin++
			if a != (Artifact{Format: FormatBuiltin}) {
				v.add(id, field, "a builtin artifact has no url, digest, size or entry")
			}
		case FormatTarGz:
			v.download(e, field, a)
		default:
			v.add(id, field+".format", "%q is not an install method Cage implements (tar.gz, builtin)", a.Format)
		}
	}
	if e.WithEnvironment && (len(e.Artifacts) == 0 || builtin != len(e.Artifacts)) {
		v.add(id, "with_environment", "needs every artifact to be builtin")
	}
	if !e.WithEnvironment && builtin > 0 {
		v.add(id, "artifacts", "builtin is only for a client that comes with the environment")
	}
	if !e.WithEnvironment && e.Status == Available && len(e.Artifacts) == 0 {
		v.add(id, "artifacts", "an available entry needs at least one architecture")
	}
}

func (v *checker) download(e Entry, field string, a Artifact) {
	id := e.ID
	if u, err := url.Parse(a.URL); err != nil || !httpsURL(a.URL) || u.RawQuery != "" || u.ForceQuery || !plainURLPath(u) {
		v.add(id, field+".url", "must be https on port 443 without credentials, query, fragment, percent-encoding or dot segments")
	} else {
		if !slices.Contains(e.Hosts.Install, u.Hostname()) {
			v.add(id, field+".url", "host %s is not in hosts.install", u.Hostname())
		}
		if e.Version != "" && !strings.Contains(u.Path, e.Version) {
			v.add(id, field+".url", "does not name version %s", e.Version)
		}
	}
	if a.SHA256 == "" && a.SHA512 == "" {
		v.add(id, field, "needs sha256 or sha512")
	}
	if a.SHA256 != "" && !sha256Pattern.MatchString(a.SHA256) {
		v.add(id, field+".sha256", "must be 64 lowercase hex digits")
	}
	if a.SHA512 != "" && !sha512Pattern.MatchString(a.SHA512) {
		v.add(id, field+".sha512", "must be 128 lowercase hex digits")
	}
	if a.Size <= 0 || a.Unpacked <= 0 {
		v.add(id, field, "needs size and unpacked in bytes")
	}
	if !cleanRelative(a.Entry) {
		v.add(id, field+".entry", "%q must be a clean relative path inside the archive", a.Entry)
	}
}

// builtin checks that an entry that comes with the environment only
// describes today's mechanism: nothing installs through the catalog.
func (v *checker) builtin(e Entry) {
	if e.Version != "" || e.Libc != "" || e.MinGlibc != "" || len(e.Requires.Packages) > 0 || len(e.Env) > 0 ||
		len(e.Settings) > 0 || e.Updater != (Updater{}) || e.Telemetry != (Telemetry{}) || len(e.DataDirs) > 0 ||
		e.Launch.Command != "" || len(e.Launch.Argv) > 0 || e.Launch.CWD != "" || e.Launch.Title != "" || e.Measured != nil {
		v.add(e.ID, "with_environment", "an entry that comes with the environment only describes it: no version, libc, packages, env, settings, updater, telemetry, data_dirs, launch or measured")
	}
	if len(e.Review.Sources) == 0 && len(e.Review.Notes) == 0 {
		v.add(e.ID, "review", "say where today's mechanism lives (notes or sources)")
	}
}

func (v *checker) optional(e Entry) {
	id := e.ID
	if !versionPattern.MatchString(e.Version) {
		v.add(id, "version", "%q is not a version", e.Version)
	}
	if e.Libc != "glibc" && e.Libc != "musl" && e.Libc != "any" {
		v.add(id, "libc", "%q is not glibc, musl or any", e.Libc)
	}
	if e.MinGlibc != "" && (e.Libc != "glibc" || !minGlibcPattern.MatchString(e.MinGlibc)) {
		v.add(id, "min_glibc", "%q needs libc glibc and the form 2.N", e.MinGlibc)
	}
	seen := map[string]bool{}
	for _, p := range e.Requires.Packages {
		if !allowedPackages[p] || seen[p] {
			v.add(id, "requires.packages", "%q is not on the package allow-list or is listed twice", p)
		}
		seen[p] = true
	}
	for _, key := range slices.Sorted(maps.Keys(e.Env)) {
		vendor := slices.ContainsFunc(reviewed[id].envPrefixes, func(p string) bool { return strings.HasPrefix(key, p) })
		if !envKeyPattern.MatchString(key) || deniedEnv(key) || !vendor {
			v.add(id, "env."+key, "name is not allowed")
		}
		if !envValuePattern.MatchString(e.Env[key]) {
			v.add(id, "env."+key, "value must be plain (letters, digits and ._,:/@%%+=-) and at most 256 bytes")
		}
	}
	for i, s := range e.Settings {
		v.setting(e, fmt.Sprintf("settings[%d]", i), s)
	}
	v.updater(e)
	if !text(e.Telemetry.How, 200) {
		v.add(id, "telemetry.how", "say how telemetry is turned off, or that it cannot be")
	}
	if !e.Telemetry.DefaultOff && e.Telemetry.NoteZH == "" {
		v.add(id, "telemetry.note_zh", "telemetry that stays on must be said before install")
	}
	optionalText(v, id, "telemetry.note_zh", e.Telemetry.NoteZH, 120)
	if len(e.Signin.Methods) == 0 {
		v.add(id, "signin.methods", "needs at least one method")
	}
	if len(e.DataDirs) == 0 {
		v.add(id, "data_dirs", "needs the client's own data directory")
	}
	v.launch(e)
	if len(e.Review.Sources) == 0 {
		v.add(id, "review.sources", "an optional entry needs its sources")
	}
	if e.Status != Available {
		return
	}
	if e.Measured == nil {
		v.add(id, "measured", "an available entry needs its real-guest measurements")
	} else if e.Measured.IdleCPUPct > IdleCPUBudget {
		v.add(id, "measured.idle_cpu_pct", "%.2f is over the %.1f %% budget", e.Measured.IdleCPUPct, IdleCPUBudget)
	}
	if e.Review.By == "" || e.Review.Date == "" {
		v.add(id, "review", "an available entry needs by and date")
	}
}

func (v *checker) setting(e Entry, field string, s Setting) {
	id := e.ID
	if s.Format != "json" && s.Format != "toml" {
		v.add(id, field+".format", "%q is not json or toml", s.Format)
	} else if path.Ext(s.File) != "."+s.Format {
		v.add(id, field+".file", "%q does not end in .%s", s.File, s.Format)
	}
	if strings.HasPrefix(s.File, "/") {
		if systemSettings[s.File] != id {
			v.add(id, field+".file", "%s is not a system file this entry may write", s.File)
		}
	} else if rest, ok := homePath(s.File); !ok || !strings.Contains(rest, "/") || protected(rest) || inCageOwn(rest, true) || !ownsFile(e, s.File) {
		v.add(id, field+".file", "%q must lie inside one of the entry's data_dirs", s.File)
	}
	if len(s.Set) == 0 {
		v.add(id, field+".set", "sets nothing")
	}
	for _, key := range slices.Sorted(maps.Keys(s.Set)) {
		if !settingKey.MatchString(key) {
			v.add(id, field+".set", "key %q is not a dotted name", key)
		}
		switch value := s.Set[key].(type) {
		case bool:
		case string:
			if len(value) > 256 || strings.ContainsFunc(value, unicode.IsControl) {
				v.add(id, field+".set."+key, "strings are plain and at most 256 bytes")
			}
		case json.Number:
			if _, err := value.Int64(); err != nil {
				v.add(id, field+".set."+key, "numbers are integers")
			}
		default:
			v.add(id, field+".set."+key, "values are bool, string or integer")
		}
	}
}

func (v *checker) updater(e Entry) {
	u, id := e.Updater, e.ID
	switch u.OffBy {
	case "env":
		if len(e.Env) == 0 {
			v.add(id, "updater.off_by", "env, but the entry sets no variable")
		}
	case "setting":
		if len(e.Settings) == 0 {
			v.add(id, "updater.off_by", "setting, but the entry sets no file")
		}
	case "none":
		if u.SelfUpdate && u.NoteZH == "" {
			v.add(id, "updater.note_zh", "a self-updating client Cage cannot switch off needs a note")
		}
	default:
		v.add(id, "updater.off_by", "%q is not env, setting or none", u.OffBy)
	}
	optionalText(v, id, "updater.note_zh", u.NoteZH, 120)
}

func (v *checker) hosts(e Entry) {
	id := e.ID
	for _, list := range []struct {
		field string
		hosts []string
	}{{"install", e.Hosts.Install}, {"signin", e.Hosts.Signin}, {"run", e.Hosts.Run}, {"telemetry", e.Hosts.Telemetry}, {"signin.login_link_hosts", e.Signin.LoginLinkHosts}} {
		for _, h := range list.hosts {
			if !hostname(h) {
				v.add(id, "hosts."+list.field, "%q is not a hostname", h)
			}
		}
	}
	for _, h := range e.Hosts.Install {
		if !maintenanceHosts[h] && !slices.Contains(extraInstallHosts[id], h) {
			v.add(id, "hosts.install", "%s is outside today's download channel and not in extraInstallHosts (a code change with a security review)", h)
		}
	}
	for _, h := range e.Signin.LoginLinkHosts {
		if !slices.Contains(e.Hosts.Signin, h) {
			v.add(id, "signin.login_link_hosts", "%s is not in hosts.signin", h)
		}
	}
	for _, m := range e.Signin.Methods {
		if !signinMethods[m] {
			v.add(id, "signin.methods", "%q is not a known method", m)
		}
	}
	optionalText(v, id, "hosts.note_zh", e.Hosts.NoteZH, 120)
}

// dataDirs: a dot folder under the home, two levels deep, outside the
// protected trees and Cage's folders, unless reviewed for this entry; never a
// shared root or a protected tree.
func (v *checker) dataDirs(e Entry) {
	for i, d := range e.DataDirs {
		rest, ok := homePath(d.Path)
		ok = ok && strings.HasPrefix(rest, ".") && !homeRoots[rest] && !protected(rest) && !inCageOwn(rest, false)
		if ok && (!strings.Contains(rest, "/") || inCageOwn(rest, true)) && (!e.Optional() || !slices.Contains(reviewed[e.ID].dataDirs, d.Path)) {
			ok = false
		}
		if !ok {
			v.add(e.ID, "data_dirs", "%q must be a dot folder under ~/ two levels deep (or reviewed), not a shared root, a credential store or one of Cage's folders", d.Path)
			continue
		}
		for _, earlier := range e.DataDirs[:i] {
			if within(d.Path, earlier.Path) || within(earlier.Path, d.Path) {
				v.add(e.ID, "data_dirs", "%s and %s are the same or nested", earlier.Path, d.Path)
			}
		}
	}
}

// overlaps refuses two entries claiming the same or nested data directories,
// unless both mark them shared: removing one client's data must not remove
// another's.
func (v *checker) overlaps(entries []Entry) {
	for i, a := range entries {
		for _, b := range entries[i+1:] {
			for _, da := range a.DataDirs {
				for _, db := range b.DataDirs {
					if !(da.Shared && db.Shared) && (within(da.Path, db.Path) || within(db.Path, da.Path)) {
						v.add(b.ID, "data_dirs", "%s overlaps %s of %s; only directories both mark shared may", db.Path, da.Path, a.ID)
					}
				}
			}
		}
	}
}

func (v *checker) launch(e Entry) {
	l, id := e.Launch, e.ID
	if !commandPattern.MatchString(l.Command) || reservedCommands[l.Command] || strings.HasPrefix(l.Command, "cage-") || reviewed[id].command != l.Command {
		v.add(id, "launch.command", "%q is not allowed (pattern, Cage's own names, a system program it would shadow, or not the reviewed name)", l.Command)
	}
	if len(l.Argv) == 0 || l.Argv[0] != path.Join(LauncherDir, l.Command) {
		v.add(id, "launch.argv", "must start with %s/<command>", LauncherDir)
	}
	for _, arg := range l.Argv {
		if arg == "" || len(arg) > 256 || strings.ContainsFunc(arg, unicode.IsControl) {
			v.add(id, "launch.argv", "arguments are plain and at most 256 bytes")
		}
	}
	if l.CWD != "project" {
		v.add(id, "launch.cwd", "must be project")
	}
	if !text(l.Title, 32) {
		v.add(id, "launch.title", "needs 1 to 32 characters")
	}
}

func (v *checker) mark(e Entry) {
	m, id := e.Mark, e.ID
	switch {
	case m.Builtin != "" && m.Monogram != "":
		v.add(id, "mark", "builtin or monogram, not both")
	case m.Builtin == "claude" && e.Session == SessionClaude, m.Builtin == "codex" && e.Session == SessionCodex:
	case m.Builtin != "":
		v.add(id, "mark.builtin", "%q is not the drawn mark of this entry's session", m.Builtin)
	case m.Monogram != "":
		if n := utf8.RuneCountInString(m.Monogram); n > 2 || strings.ContainsFunc(m.Monogram, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			v.add(id, "mark.monogram", "%q must be one or two letters or digits", m.Monogram)
		}
	default:
		v.add(id, "mark", "needs builtin or monogram")
	}
}

func (v *checker) measured(id string, m Measured) {
	if m.DiskMB <= 0 || m.IdleRSSMB <= 0 || m.InstallS <= 0 || m.IdleCPUPct < 0 || m.IdleCPUPct > 100 {
		v.add(id, "measured", "disk_mb, idle_rss_mb and install_s must be positive and idle_cpu_pct 0 to 100")
	}
	if !text(m.Guest, 64) || !date(m.Date) {
		v.add(id, "measured", "needs the guest and the date (YYYY-MM-DD)")
	}
}

func (v *checker) review(e Entry) {
	r, id := e.Review, e.ID
	optionalText(v, id, "review.by", r.By, 64)
	if r.Date != "" && !date(r.Date) {
		v.add(id, "review.date", "%q is not YYYY-MM-DD", r.Date)
	}
	for _, s := range r.Sources {
		if !httpsURL(s.URL) || !date(s.Read) {
			v.add(id, "review.sources", "%q needs an https URL and the day it was read (YYYY-MM-DD)", s.URL)
		}
	}
	for _, note := range r.Notes {
		if !text(note, 600) {
			v.add(id, "review.notes", "notes are 1 to 600 characters")
		}
	}
}

// deniedEnv backs up the vendor prefixes: never a variable a loader, a shell
// or Cage reads, and never one that names a path, a file, a command, options
// or a proxy. internal/cage/agent-install.sh refuses the same.
func deniedEnv(key string) bool {
	return slices.Contains(deniedEnvNames, key) ||
		slices.ContainsFunc(deniedEnvPrefixes, func(p string) bool { return strings.HasPrefix(key, p) }) ||
		slices.ContainsFunc(deniedEnvSuffixes, func(s string) bool { return strings.HasSuffix(key, s) })
}

var (
	deniedEnvNames    = []string{"PATH", "HOME", "SHELL", "BROWSER", "TZ", "LANG", "NODE_OPTIONS", "ENV", "BASH_ENV", "IFS", "SHELLOPTS"}
	deniedEnvPrefixes = []string{"LD_", "LC_", "PYTHON", "CAGE_"}
	deniedEnvSuffixes = []string{"_PROXY", "_HOME", "_PATH", "_DIR", "_FILE", "_COMMAND", "_OPTIONS", "_SOCK", "_CERTS"}
)

// plainURLPath reports whether the path is what curl fetches: no percent
// encoding and no empty, "." or ".." segment.
func plainURLPath(u *url.URL) bool {
	if u.RawPath != "" || strings.Contains(u.EscapedPath(), "%") || !strings.HasPrefix(u.Path, "/") {
		return false
	}
	return !slices.ContainsFunc(strings.Split(u.Path[1:], "/"), func(s string) bool { return s == "" || s == "." || s == ".." })
}

// httpsURL reports whether raw is an absolute https URL to a hostname on the
// default port, without credentials or fragment.
func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Opaque == "" && u.User == nil && u.Fragment == "" &&
		(u.Port() == "" || u.Port() == "443") && hostname(u.Hostname()) &&
		!strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

func hostname(h string) bool { return len(h) <= 253 && hostPattern.MatchString(h) }

// cleanRelative reports whether p is a clean relative slash path that stays
// where it is put.
func cleanRelative(p string) bool {
	return p != "" && len(p) <= 255 && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." &&
		!slices.Contains(strings.Split(p, "/"), "..") && !strings.ContainsFunc(p, func(r rune) bool { return r == '\\' || unicode.IsControl(r) })
}

// homePath returns the part of p after "~/" when p is a clean path below the
// home.
func homePath(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, "~/")
	return rest, ok && cleanRelative(rest)
}

func protected(rest string) bool {
	return slices.ContainsFunc(homeProtected, func(root string) bool { return within(rest, root) })
}

// inCageOwn reports whether rest is one of Cage's folders (exact) or lies
// inside one (inside).
func inCageOwn(rest string, inside bool) bool {
	return slices.ContainsFunc(cageOwn, func(root string) bool {
		if inside {
			return strings.HasPrefix(rest, root+"/")
		}
		return rest == root
	})
}

// ownsFile reports whether a home file lies inside one of e's data dirs.
func ownsFile(e Entry, file string) bool {
	return slices.ContainsFunc(e.DataDirs, func(d DataDir) bool { return strings.HasPrefix(file, d.Path+"/") })
}

func within(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

func date(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// text reports whether s is 1 to max characters of valid UTF-8 without
// control or format characters (bidi overrides, zero-width marks).
func text(s string, max int) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, unsafeRune)
}

func unsafeRune(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

func optionalText(v *checker, id, field, s string, max int) {
	if s != "" && !text(s, max) {
		v.add(id, field, "needs at most %d characters without control characters", max)
	}
}
