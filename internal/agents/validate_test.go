package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func builtinEntry() Entry {
	return Entry{
		ID: "claude-code", Name: "Claude Code", Vendor: "Anthropic", SummaryZH: "随环境提供。",
		WithEnvironment: true, Kind: Terminal, Session: SessionClaude, Status: Available,
		Artifacts: map[Arch]Artifact{ARM64: {Format: FormatBuiltin}, AMD64: {Format: FormatBuiltin}},
		Hosts:     Hosts{Install: []string{"registry.npmjs.org"}},
		Mark:      Mark{Builtin: "claude"},
		Review:    Review{Notes: []string{"today's recipe in guest-tools.sh"}},
	}
}

func availableEntry() Entry {
	return Entry{
		ID: "alpha", Name: "Alpha", Vendor: "Vendor", SummaryZH: "示例客户端。",
		Kind: Terminal, Session: SessionCodex, Status: Available, Version: "1.2.3", Libc: "any",
		Artifacts: map[Arch]Artifact{
			ARM64: {Format: FormatTarGz, URL: "https://github.com/v/alpha/releases/download/v1.2.3/alpha-arm64.tar.gz", SHA256: strings.Repeat("a", 64), Size: 10, Unpacked: 20, Entry: "alpha"},
			AMD64: {Format: FormatTarGz, URL: "https://github.com/v/alpha/releases/download/v1.2.3/alpha-x64.tar.gz", SHA512: strings.Repeat("b", 128), Size: 10, Unpacked: 20, Entry: "bin/alpha"},
		},
		Requires:     Requires{Packages: []string{"bubblewrap"}, UserNS: true},
		Env:          map[string]string{"ALPHA_NO_UPDATE": "1"},
		Settings:     []Setting{{File: "~/.alpha/config.toml", Format: "toml", Set: map[string]any{"update.check": false, "name": "x", "limit": json.Number("3")}}},
		Updater:      Updater{SelfUpdate: true, OffBy: "env"},
		Telemetry:    Telemetry{DefaultOff: true, How: "ALPHA_NO_TELEMETRY in the launcher"},
		Hosts:        Hosts{Install: []string{"github.com", "release-assets.githubusercontent.com"}, Signin: []string{"login.example.com"}, Run: []string{"api.example.com"}},
		Signin:       Signin{Methods: []string{"device-code"}, LoginLinkHosts: []string{"login.example.com"}},
		DataDirs:     []DataDir{{Path: "~/.alpha"}, {Path: "~/.shared/tool", Shared: true}},
		ProjectFiles: []string{".alpha/hooks.json"},
		Launch:       Launch{Command: "alpha", Argv: []string{"/usr/local/bin/alpha", "--tui"}, CWD: "project", Title: "Alpha"},
		Mark:         Mark{Builtin: "codex"},
		Regions:      Regions{Unavailable: []string{"CN"}, NoteZH: "暂不提供服务。"},
		Terms:        Terms{URL: "https://example.com/terms"},
		Measured:     &Measured{DiskMB: 300, IdleRSSMB: 120, IdleCPUPct: 0.1, InstallS: 30, Guest: "debian-13 arm64", Date: "2026-11-22"},
		Review:       Review{By: "reviewer", Date: "2026-11-22", Sources: []Source{{URL: "https://example.com/release", Read: "2026-11-22"}}},
	}
}

// hiddenEntry waits for its acceptance: no artifacts and no measurements yet.
func hiddenEntry() Entry {
	return Entry{
		ID: "beta", Name: "Beta", Vendor: "Vendor", SummaryZH: "另一个客户端。",
		PersonOnly: true, Kind: Terminal, Session: SessionGeneric, Status: Hidden, Version: "2.0.0", Libc: "glibc", MinGlibc: "2.28",
		Updater:   Updater{SelfUpdate: true, OffBy: "none", NoteZH: "无法关闭自动更新。"},
		Telemetry: Telemetry{How: "account toggle only", NoteZH: "遥测无法预先关闭。"},
		Signin:    Signin{Methods: []string{"browser-code"}},
		DataDirs:  []DataDir{{Path: "~/.beta"}, {Path: "~/.shared/tool", Shared: true}},
		Launch:    Launch{Command: "beta", Argv: []string{"/usr/local/bin/beta"}, CWD: "project", Title: "Beta"},
		Mark:      Mark{Monogram: "Be"},
		Review:    Review{Sources: []Source{{URL: "https://example.com/beta", Read: "2026-11-22"}}},
	}
}

func validCatalog() Catalog {
	return Catalog{Schema: SchemaVersion, Entries: []Entry{builtinEntry(), availableEntry(), hiddenEntry()}}
}

func TestValidCatalogPasses(t *testing.T) {
	if err := validCatalog().Validate(); err != nil {
		t.Fatal(err)
	}
}

// One case per rule (docs/agent-catalog.md). want is the start of the
// expected problem: "<id>: <field>".
func TestValidatorRules(t *testing.T) {
	alpha := func(c *Catalog) *Entry { return &c.Entries[1] }
	beta := func(c *Catalog) *Entry { return &c.Entries[2] }
	claude := func(c *Catalog) *Entry { return &c.Entries[0] }
	arm := func(c *Catalog, mutate func(*Artifact)) {
		a := alpha(c).Artifacts[ARM64]
		mutate(&a)
		alpha(c).Artifacts[ARM64] = a
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Catalog)
		want   string
	}{
		// Catalog
		{"schema version", func(c *Catalog) { c.Schema = 2 }, "catalog: schema"},
		{"empty catalog", func(c *Catalog) { c.Entries = nil }, "catalog: entries"},
		// id, texts, kind, session, status
		{"id pattern", func(c *Catalog) { alpha(c).ID = "Alpha" }, "Alpha: id"},
		{"id too short", func(c *Catalog) { alpha(c).ID = "a" }, "a: id"},
		{"id unique", func(c *Catalog) { beta(c).ID = "alpha" }, "alpha: id: used by two"},
		{"name required", func(c *Catalog) { alpha(c).Name = "" }, "alpha: name"},
		{"vendor required", func(c *Catalog) { alpha(c).Vendor = "" }, "alpha: vendor"},
		{"summary at most 40 characters", func(c *Catalog) { alpha(c).SummaryZH = strings.Repeat("字", 41) }, "alpha: summary_zh"},
		{"summary without control characters", func(c *Catalog) { alpha(c).SummaryZH = "a\nb" }, "alpha: summary_zh"},
		{"kind closed set", func(c *Catalog) { alpha(c).Kind = "web" }, "alpha: kind"},
		{"session closed set", func(c *Catalog) { alpha(c).Session = "existing:gemini" }, "alpha: session"},
		{"an app has no built kind", func(c *Catalog) { alpha(c).Kind = App }, "alpha: session: an app"},
		{"a built kind belongs to one entry", func(c *Catalog) { beta(c).Session = SessionCodex; beta(c).PersonOnly = false }, "beta: session: existing:codex is also alpha's"},
		{"person_only needs the generic kind", func(c *Catalog) { alpha(c).PersonOnly = true }, "alpha: person_only"},
		{"status closed set", func(c *Catalog) { alpha(c).Status = "beta" }, "alpha: status"},
		// with_environment and builtin
		{"with_environment needs builtin", func(c *Catalog) { claude(c).Artifacts[ARM64] = alpha(c).Artifacts[ARM64] }, "claude-code: with_environment: needs every artifact"},
		{"with_environment needs artifacts", func(c *Catalog) { claude(c).Artifacts = nil }, "claude-code: with_environment: needs every artifact"},
		{"builtin only with the environment", func(c *Catalog) { alpha(c).Artifacts[ARM64] = Artifact{Format: FormatBuiltin} }, "alpha: artifacts: builtin is only"},
		{"a builtin artifact has nothing to download", func(c *Catalog) {
			claude(c).Artifacts[ARM64] = Artifact{Format: FormatBuiltin, URL: "https://github.com/x"}
		}, "claude-code: artifacts.arm64: a builtin"},
		{"a builtin entry installs nothing: version", func(c *Catalog) { claude(c).Version = "2.1.0" }, "claude-code: with_environment: an entry"},
		{"a builtin entry installs nothing: launch", func(c *Catalog) { claude(c).Launch = alpha(c).Launch; claude(c).Launch.Command = "claude2" }, "claude-code: with_environment: an entry"},
		{"a builtin entry installs nothing: env", func(c *Catalog) { claude(c).Env = map[string]string{"X_Y": "1"} }, "claude-code: with_environment: an entry"},
		{"a builtin entry says where it lives", func(c *Catalog) { claude(c).Review = Review{} }, "claude-code: review"},
		// version, artifacts
		{"version required", func(c *Catalog) { alpha(c).Version = "" }, "alpha: version"},
		{"version form", func(c *Catalog) { alpha(c).Version = "latest" }, "alpha: version"},
		{"architecture closed set", func(c *Catalog) { alpha(c).Artifacts["riscv64"] = alpha(c).Artifacts[ARM64] }, "alpha: artifacts.riscv64"},
		{"format closed set: zip not implemented", func(c *Catalog) { arm(c, func(a *Artifact) { a.Format = "zip" }) }, "alpha: artifacts.arm64.format"},
		{"format closed set: apt not implemented", func(c *Catalog) { arm(c, func(a *Artifact) { a.Format = "apt" }) }, "alpha: artifacts.arm64.format"},
		{"format closed set: no script", func(c *Catalog) { arm(c, func(a *Artifact) { a.Format = "script" }) }, "alpha: artifacts.arm64.format"},
		{"https only", func(c *Catalog) { arm(c, func(a *Artifact) { a.URL = strings.Replace(a.URL, "https:", "http:", 1) }) }, "alpha: artifacts.arm64.url"},
		{"no credentials in the URL", func(c *Catalog) {
			arm(c, func(a *Artifact) { a.URL = strings.Replace(a.URL, "https://", "https://u:p@", 1) })
		}, "alpha: artifacts.arm64.url"},
		{"no query in the URL", func(c *Catalog) { arm(c, func(a *Artifact) { a.URL += "?token=1" }) }, "alpha: artifacts.arm64.url"},
		{"default port only", func(c *Catalog) {
			arm(c, func(a *Artifact) { a.URL = strings.Replace(a.URL, "github.com", "github.com:8443", 1) })
		}, "alpha: artifacts.arm64.url"},
		{"no address literal", func(c *Catalog) { arm(c, func(a *Artifact) { a.URL = "https://140.82.112.3/v1.2.3/a.tar.gz" }) }, "alpha: artifacts.arm64.url"},
		{"artifact host in hosts.install", func(c *Catalog) { alpha(c).Hosts.Install = []string{"release-assets.githubusercontent.com"} }, "alpha: artifacts.arm64.url: host github.com"},
		{"artifact URL names the version", func(c *Catalog) { alpha(c).Version = "1.2.4" }, "alpha: artifacts.amd64.url: does not name"},
		{"every architecture has a digest", func(c *Catalog) { arm(c, func(a *Artifact) { a.SHA256 = "" }) }, "alpha: artifacts.arm64: needs sha256 or sha512"},
		{"sha256 form", func(c *Catalog) { arm(c, func(a *Artifact) { a.SHA256 = strings.Repeat("A", 64) }) }, "alpha: artifacts.arm64.sha256"},
		{"sha512 form", func(c *Catalog) { arm(c, func(a *Artifact) { a.SHA512 = strings.Repeat("b", 127) }) }, "alpha: artifacts.arm64.sha512"},
		{"size and unpacked", func(c *Catalog) { arm(c, func(a *Artifact) { a.Unpacked = 0 }) }, "alpha: artifacts.arm64: needs size"},
		{"entry without ..", func(c *Catalog) { arm(c, func(a *Artifact) { a.Entry = "../alpha" }) }, "alpha: artifacts.arm64.entry"},
		{"entry relative", func(c *Catalog) { arm(c, func(a *Artifact) { a.Entry = "/usr/bin/alpha" }) }, "alpha: artifacts.arm64.entry"},
		{"entry clean", func(c *Catalog) { arm(c, func(a *Artifact) { a.Entry = "bin/./alpha" }) }, "alpha: artifacts.arm64.entry"},
		{"an available entry has an artifact", func(c *Catalog) { alpha(c).Artifacts = nil }, "alpha: artifacts: an available"},
		// libc, requires
		{"libc closed set", func(c *Catalog) { alpha(c).Libc = "bionic" }, "alpha: libc"},
		{"min_glibc only with glibc", func(c *Catalog) { beta(c).Libc = "musl" }, "beta: min_glibc"},
		{"min_glibc form", func(c *Catalog) { beta(c).MinGlibc = "2.28.1" }, "beta: min_glibc"},
		{"packages from the allow-list", func(c *Catalog) { alpha(c).Requires.Packages = []string{"curl"} }, "alpha: requires.packages"},
		{"packages once", func(c *Catalog) { alpha(c).Requires.Packages = []string{"bubblewrap", "bubblewrap"} }, "alpha: requires.packages"},
		// env
		{"env key form", func(c *Catalog) { alpha(c).Env["alpha_x"] = "1" }, "alpha: env.alpha_x: name"},
		{"env value plain", func(c *Catalog) { alpha(c).Env["ALPHA_NO_UPDATE"] = "$(id)" }, "alpha: env.ALPHA_NO_UPDATE: value"},
		{"env value without newline", func(c *Catalog) { alpha(c).Env["ALPHA_NO_UPDATE"] = "1\n2" }, "alpha: env.ALPHA_NO_UPDATE: value"},
		{"env value at most 256 bytes", func(c *Catalog) { alpha(c).Env["ALPHA_NO_UPDATE"] = strings.Repeat("1", 257) }, "alpha: env.ALPHA_NO_UPDATE: value"},
		// settings
		{"settings format closed set", func(c *Catalog) { alpha(c).Settings[0].Format = "yaml" }, "alpha: settings[0].format"},
		{"settings extension matches format", func(c *Catalog) { alpha(c).Settings[0].Format = "json" }, "alpha: settings[0].file"},
		{"system file only for its entry", func(c *Catalog) { alpha(c).Settings[0].File = "/etc/codex/config.toml" }, "alpha: settings[0].file"},
		{"other system files refused", func(c *Catalog) { alpha(c).Settings[0].File = "/etc/profile.d/alpha.toml" }, "alpha: settings[0].file"},
		{"no file directly in the home", func(c *Catalog) { alpha(c).Settings[0].File = "~/.bashrc.toml" }, "alpha: settings[0].file"},
		{"home file inside the entry's data_dirs", func(c *Catalog) { alpha(c).Settings[0].File = "~/.config/git/config.toml" }, "alpha: settings[0].file"},
		{"no file in a protected tree", func(c *Catalog) {
			alpha(c).DataDirs = append(alpha(c).DataDirs, DataDir{Path: "~/.ssh"})
			alpha(c).Settings[0].File = "~/.ssh/config.toml"
		}, "alpha: settings[0].file"},
		{"settings path without ..", func(c *Catalog) { alpha(c).Settings[0].File = "~/.alpha/../.profile.toml" }, "alpha: settings[0].file"},
		{"settings set something", func(c *Catalog) { alpha(c).Settings[0].Set = nil }, "alpha: settings[0].set"},
		{"settings key dotted name", func(c *Catalog) { alpha(c).Settings[0].Set["a b"] = true }, "alpha: settings[0].set: key"},
		{"settings value integer", func(c *Catalog) { alpha(c).Settings[0].Set["limit"] = json.Number("1.5") }, "alpha: settings[0].set.limit"},
		{"settings value scalar", func(c *Catalog) { alpha(c).Settings[0].Set["nested"] = map[string]any{"x": true} }, "alpha: settings[0].set.nested"},
		// updater, telemetry
		{"updater off_by closed set", func(c *Catalog) { alpha(c).Updater.OffBy = "" }, "alpha: updater.off_by"},
		{"updater off by env needs env", func(c *Catalog) { alpha(c).Env = nil }, "alpha: updater.off_by: env"},
		{"updater off by setting needs settings", func(c *Catalog) { alpha(c).Updater.OffBy = "setting"; alpha(c).Settings = nil }, "alpha: updater.off_by: setting"},
		{"updater none needs a note", func(c *Catalog) { beta(c).Updater.NoteZH = "" }, "beta: updater.note_zh"},
		{"telemetry how", func(c *Catalog) { alpha(c).Telemetry.How = "" }, "alpha: telemetry.how"},
		{"telemetry left on needs a note", func(c *Catalog) { beta(c).Telemetry.NoteZH = "" }, "beta: telemetry.note_zh"},
		// hosts, sign-in
		{"hosts are hostnames", func(c *Catalog) { alpha(c).Hosts.Telemetry = []string{"(not named in vendor docs)"} }, "alpha: hosts.telemetry"},
		{"hostnames lowercase", func(c *Catalog) { alpha(c).Hosts.Run = []string{"API.example.com"} }, "alpha: hosts.run"},
		{"install hosts inside today's channel", func(c *Catalog) { alpha(c).Hosts.Install = append(alpha(c).Hosts.Install, "downloads.example.com") }, "alpha: hosts.install: downloads.example.com is outside"},
		{"login links only to sign-in hosts", func(c *Catalog) { alpha(c).Signin.LoginLinkHosts = []string{"api.example.com"} }, "alpha: signin.login_link_hosts"},
		{"sign-in methods closed set", func(c *Catalog) { alpha(c).Signin.Methods = []string{"password"} }, "alpha: signin.methods"},
		{"sign-in method required", func(c *Catalog) { alpha(c).Signin.Methods = nil }, "alpha: signin.methods"},
		// data_dirs, project_files
		{"data_dirs required", func(c *Catalog) { alpha(c).DataDirs = nil; alpha(c).Settings = nil; alpha(c).Updater.OffBy = "env" }, "alpha: data_dirs: needs"},
		{"data_dirs under the home", func(c *Catalog) { alpha(c).DataDirs[0].Path = "/var/lib/alpha" }, "alpha: data_dirs"},
		{"data_dirs not the home", func(c *Catalog) { alpha(c).DataDirs[0].Path = "~/" }, "alpha: data_dirs"},
		{"data_dirs without ..", func(c *Catalog) { alpha(c).DataDirs[0].Path = "~/.alpha/../.." }, "alpha: data_dirs"},
		{"data_dirs not a shared root", func(c *Catalog) { alpha(c).DataDirs[0].Path = "~/.config" }, "alpha: data_dirs"},
		{"data_dirs not Cage's root", func(c *Catalog) { alpha(c).DataDirs[0].Path = "~/.local/share/cage" }, "alpha: data_dirs"},
		{"data_dirs not protected", func(c *Catalog) { alpha(c).DataDirs[0].Path = "~/.ssh/alpha" }, "alpha: data_dirs"},
		{"data_dirs not nested in one entry", func(c *Catalog) { alpha(c).DataDirs = append(alpha(c).DataDirs, DataDir{Path: "~/.alpha/cache"}) }, "alpha: data_dirs: ~/.alpha and ~/.alpha/cache"},
		{"no two entries claim one directory", func(c *Catalog) { beta(c).DataDirs[0].Path = "~/.alpha" }, "beta: data_dirs: ~/.alpha overlaps ~/.alpha of alpha"},
		{"no entry claims inside another's", func(c *Catalog) { beta(c).DataDirs[0].Path = "~/.alpha/beta" }, "beta: data_dirs: ~/.alpha/beta overlaps"},
		{"shared only when both say so", func(c *Catalog) { beta(c).DataDirs[1].Shared = false }, "beta: data_dirs: ~/.shared/tool overlaps"},
		{"project files relative", func(c *Catalog) { alpha(c).ProjectFiles = []string{"/etc/passwd"} }, "alpha: project_files"},
		{"project files without ..", func(c *Catalog) { alpha(c).ProjectFiles = []string{"../.alpha"} }, "alpha: project_files"},
		// launch
		{"command pattern", func(c *Catalog) { alpha(c).Launch.Command = "Alpha"; alpha(c).Launch.Argv[0] = "/usr/local/bin/Alpha" }, "alpha: launch.command"},
		{"command not claude", func(c *Catalog) {
			alpha(c).Launch.Command = "claude"
			alpha(c).Launch.Argv[0] = "/usr/local/bin/claude"
		}, "alpha: launch.command"},
		{"command not cage-*", func(c *Catalog) {
			alpha(c).Launch.Command = "cage-open"
			alpha(c).Launch.Argv[0] = "/usr/local/bin/cage-open"
		}, "alpha: launch.command"},
		{"command does not shadow a system program", func(c *Catalog) { alpha(c).Launch.Command = "sudo"; alpha(c).Launch.Argv[0] = "/usr/local/bin/sudo" }, "alpha: launch.command"},
		{"command unique", func(c *Catalog) { beta(c).Launch.Command = "alpha"; beta(c).Launch.Argv[0] = "/usr/local/bin/alpha" }, "beta: launch.command: \"alpha\" is also alpha's"},
		{"argv starts with the launcher", func(c *Catalog) { alpha(c).Launch.Argv[0] = "/tmp/alpha" }, "alpha: launch.argv"},
		{"argv required", func(c *Catalog) { alpha(c).Launch.Argv = nil }, "alpha: launch.argv"},
		{"argv plain", func(c *Catalog) { alpha(c).Launch.Argv = append(alpha(c).Launch.Argv, "a\nb") }, "alpha: launch.argv"},
		{"cwd is the project", func(c *Catalog) { alpha(c).Launch.CWD = "/" }, "alpha: launch.cwd"},
		{"title required", func(c *Catalog) { alpha(c).Launch.Title = "" }, "alpha: launch.title"},
		// mark, regions, terms
		{"mark required", func(c *Catalog) { beta(c).Mark = Mark{} }, "beta: mark: needs"},
		{"mark one kind", func(c *Catalog) { beta(c).Mark.Builtin = "claude" }, "beta: mark: builtin or monogram"},
		{"drawn mark of the entry's session", func(c *Catalog) { beta(c).Mark = Mark{Builtin: "claude"} }, "beta: mark.builtin"},
		{"monogram at most two characters", func(c *Catalog) { beta(c).Mark.Monogram = "Bet" }, "beta: mark.monogram"},
		{"monogram letters or digits", func(c *Catalog) { beta(c).Mark.Monogram = "<" }, "beta: mark.monogram"},
		{"region codes", func(c *Catalog) { alpha(c).Regions.Unavailable = []string{"china"} }, "alpha: regions.unavailable"},
		{"terms https", func(c *Catalog) { alpha(c).Terms.URL = "http://example.com/terms" }, "alpha: terms.url"},
		// measured: available is impossible without it
		{"available needs measurements", func(c *Catalog) { alpha(c).Measured = nil }, "alpha: measured: an available entry"},
		{"available within the idle CPU budget", func(c *Catalog) { alpha(c).Measured.IdleCPUPct = 0.6 }, "alpha: measured.idle_cpu_pct"},
		{"measurements positive", func(c *Catalog) { alpha(c).Measured.DiskMB = 0 }, "alpha: measured: disk_mb"},
		{"measurements dated", func(c *Catalog) { alpha(c).Measured.Date = "22/11/2026" }, "alpha: measured: needs the guest"},
		{"hidden measurements must be complete too", func(c *Catalog) { beta(c).Measured = &Measured{DiskMB: 1} }, "beta: measured"},
		// review
		{"optional entries cite sources", func(c *Catalog) { beta(c).Review.Sources = nil }, "beta: review.sources"},
		{"sources https", func(c *Catalog) { alpha(c).Review.Sources[0].URL = "http://example.com" }, "alpha: review.sources"},
		{"sources dated", func(c *Catalog) { alpha(c).Review.Sources[0].Read = "today" }, "alpha: review.sources"},
		{"available entries name the reviewer", func(c *Catalog) { alpha(c).Review.By = "" }, "alpha: review: an available"},
		{"review date form", func(c *Catalog) { beta(c).Review.Date = "2026-13-01" }, "beta: review.date"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validCatalog()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("accepted, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v\nwant a problem starting %q", err, tc.want)
			}
		})
	}
}

func TestEnvDenyList(t *testing.T) {
	for _, key := range []string{"PATH", "HOME", "SHELL", "BROWSER", "TZ", "LANG", "LC_ALL", "LD_PRELOAD", "LD_LIBRARY_PATH",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_OPTIONS", "PYTHONPATH", "PYTHONSTARTUP", "CAGE_TOKEN"} {
		c := validCatalog()
		c.Entries[1].Env[key] = "x"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "alpha: env."+key+": name") {
			t.Fatalf("%s: %v", key, err)
		}
	}
}

func TestAllowedVariations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Catalog)
	}{
		{"a hidden entry needs no artifact or measurement", func(c *Catalog) {}},
		{"a withdrawn entry needs no artifact", func(c *Catalog) { c.Entries[2].Status = Withdrawn }},
		{"an extra install host listed in code", func(c *Catalog) {
			c.Entries[1].Hosts.Install = append(c.Entries[1].Hosts.Install, "downloads.example.com")
			extraInstallHosts["alpha"] = []string{"downloads.example.com"}
		}},
		{"idle CPU of zero", func(c *Catalog) { c.Entries[1].Measured.IdleCPUPct = 0 }},
		{"one architecture", func(c *Catalog) { delete(c.Entries[1].Artifacts, AMD64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer delete(extraInstallHosts, "alpha")
			c := validCatalog()
			tc.mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseIsStrict(t *testing.T) {
	good, err := json.Marshal(validCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(good); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	edit := func(mutate func(map[string]any)) []byte {
		var raw map[string]any
		if err := json.Unmarshal(good, &raw); err != nil {
			t.Fatal(err)
		}
		mutate(raw)
		out, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	entry := func(raw map[string]any, i int) map[string]any { return raw["entries"].([]any)[i].(map[string]any) }
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"unknown top-level field", edit(func(r map[string]any) { r["_comment"] = "x" }), "unknown field"},
		{"misspelt digest", edit(func(r map[string]any) {
			arm := entry(r, 1)["artifacts"].(map[string]any)["arm64"].(map[string]any)
			arm["sha265"] = arm["sha256"]
		}), "unknown field"},
		{"trailing data", append(append([]byte{}, good...), []byte(" {}")...), "data after"},
		{"null measurement", edit(func(r map[string]any) { entry(r, 1)["measured"].(map[string]any)["idle_cpu_pct"] = nil }), "measured.idle_cpu_pct is missing"},
		{"missing measurement", edit(func(r map[string]any) { delete(entry(r, 1)["measured"].(map[string]any), "guest") }), "measured.guest is missing"},
		{"all-null measurement block", edit(func(r map[string]any) {
			entry(r, 2)["measured"] = map[string]any{"disk_mb": nil, "idle_rss_mb": nil, "idle_cpu_pct": nil, "install_s": nil, "guest": nil, "date": nil}
		}), "measured.disk_mb is missing"},
		{"unknown measurement", edit(func(r map[string]any) { entry(r, 1)["measured"].(map[string]any)["gpu"] = 1 }), "unknown field"},
		{"available without measured", edit(func(r map[string]any) { entry(r, 1)["measured"] = nil }), "alpha: measured: an available entry"},
		{"not json", []byte("{"), "agent catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
