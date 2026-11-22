package agents

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Loading the package must not parse the catalog: a session that only
	// uses Claude never pays for it.
	if n := decodes.Load(); n != 0 {
		println("agent catalog parsed during package initialisation:", n)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestParseIsLazyAndHappensOnce(t *testing.T) {
	load := lazy(catalogJSON)
	before := decodes.Load()
	if decodes.Load() != before {
		t.Fatal("building the loader parsed the catalog")
	}
	for range 3 {
		if _, err := load(); err != nil {
			t.Fatal(err)
		}
	}
	if got := decodes.Load() - before; got != 1 {
		t.Fatalf("three loads parsed %d times, want 1", got)
	}
	if _, err := All(); err != nil {
		t.Fatal(err)
	}
	after := decodes.Load()
	for range 3 {
		if _, _, err := Lookup("codex"); err != nil {
			t.Fatal(err)
		}
	}
	if decodes.Load() != after {
		t.Fatal("lookups parsed the shipped catalog again")
	}
}

func TestShippedCatalogPassesTheValidator(t *testing.T) {
	c, err := Parse(catalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range c.Entries {
		ids = append(ids, e.ID)
	}
	if want := []string{"claude-code", "codex", "antigravity-cli"}; !slices.Equal(ids, want) {
		t.Fatalf("entries %v, want %v", ids, want)
	}
	claude := c.Entries[0]
	if !claude.WithEnvironment || claude.Optional() || claude.Status != Available || claude.Session != SessionClaude {
		t.Fatalf("claude-code must come with the environment: %+v", claude)
	}
	for _, arch := range []Arch{ARM64, AMD64} {
		if a, ok := claude.Artifact(arch); !ok || a.Format != FormatBuiltin {
			t.Fatalf("claude-code %s: %+v", arch, a)
		}
	}
	for _, e := range c.Entries[1:] {
		// Both optional entries ship hidden until their real-guest acceptance.
		if !e.Optional() || e.Status != Hidden || e.Measured != nil {
			t.Fatalf("%s must ship hidden and unmeasured: %+v", e.ID, e)
		}
		for _, arch := range []Arch{ARM64, AMD64} {
			a, ok := e.Artifact(arch)
			if !ok || a.Format != FormatTarGz || (a.SHA256 == "" && a.SHA512 == "") || !strings.Contains(a.URL, e.Version) {
				t.Fatalf("%s %s: %+v", e.ID, arch, a)
			}
		}
	}
}

func TestVisibleLeavesOutHiddenEntries(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	visible, err := Visible()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || len(visible) != 1 || visible[0].ID != "claude-code" {
		t.Fatalf("all %d, visible %+v", len(all), visible)
	}
}

func TestLookupAndByCommand(t *testing.T) {
	for _, tc := range []struct {
		id, command string
	}{{"codex", "codex"}, {"antigravity-cli", "agy"}} {
		e, ok, err := Lookup(tc.id)
		if err != nil || !ok || e.Launch.Command != tc.command {
			t.Fatalf("Lookup(%s): %v %v %+v", tc.id, ok, err, e.Launch)
		}
		e, ok, err = ByCommand(tc.command)
		if err != nil || !ok || e.ID != tc.id {
			t.Fatalf("ByCommand(%s): %v %v %s", tc.command, ok, err, e.ID)
		}
	}
	if _, ok, err := Lookup("claude-code"); !ok || err != nil {
		t.Fatal("claude-code not found")
	}
	for _, command := range []string{"claude", "", "nope"} {
		if e, ok, _ := ByCommand(command); ok {
			t.Fatalf("ByCommand(%q) found %s", command, e.ID)
		}
	}
	if _, ok, _ := Lookup("nope"); ok {
		t.Fatal("unknown id found")
	}
}

func TestEntriesAreCopies(t *testing.T) {
	e, _, err := Lookup("antigravity-cli")
	if err != nil {
		t.Fatal(err)
	}
	e.Artifacts[ARM64] = Artifact{URL: "https://evil.example/x"}
	e.Env["PATH"] = "/tmp"
	e.Settings[0].Set["showFeedbackSurvey"] = true
	e.Hosts.Install[0] = "evil.example"
	e.Launch.Argv[0] = "/tmp/agy"
	e.DataDirs[0].Path = "~"
	e.Review.Notes[0] = "changed"
	again, _, _ := Lookup("antigravity-cli")
	a, _ := again.Artifact(ARM64)
	if a.Format != FormatTarGz || again.Env["PATH"] != "" || again.Settings[0].Set["showFeedbackSurvey"] != false ||
		again.Hosts.Install[0] != "storage.googleapis.com" || again.Launch.Argv[0] != "/usr/local/bin/agy" ||
		again.DataDirs[0].Path != "~/.gemini/antigravity-cli" || again.Review.Notes[0] == "changed" {
		t.Fatalf("a caller changed the shared catalog: %+v", again)
	}
	m := Measured{DiskMB: 1}
	e.Measured = &m
	c := e.clone()
	c.Measured.DiskMB = 2
	if e.Measured.DiskMB != 1 {
		t.Fatal("clone shares measured")
	}
}

func TestParseArch(t *testing.T) {
	for in, want := range map[string]Arch{"arm64": ARM64, "aarch64": ARM64, "amd64": AMD64, "x86_64": AMD64} {
		if got, ok := ParseArch(in); !ok || got != want {
			t.Fatalf("%s: %s %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "arm", "i386", "riscv64", "ARM64"} {
		if _, ok := ParseArch(in); ok {
			t.Fatalf("%q accepted", in)
		}
	}
}

// TestMaintenanceHostsMatchWorkbench keeps the validator's copy of today's
// download channel from claiming a host the workbench does not allow.
func TestMaintenanceHostsMatchWorkbench(t *testing.T) {
	source, err := os.ReadFile("../workbench/maintenance.go")
	if err != nil {
		t.Fatal(err)
	}
	for host := range maintenanceHosts {
		if !strings.Contains(string(source), `"`+host+`"`) {
			t.Errorf("%s is not in internal/workbench/maintenance.go", host)
		}
	}
}

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(catalogJSON); err != nil {
			b.Fatal(err)
		}
	}
}
