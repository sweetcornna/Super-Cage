package workbench

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The server's gate (scienceHostVerified) and the Windows window's
// (SCIENCE_HOST_VERIFIED, ScienceFrame.HostVerified) are lifted together:
// by build-windows.ps1's one parameter for the real-window run, or by one
// reviewed commit that changes both defaults. A server that offers the
// switch to a window that never adds its switches, or a window with the
// switches under a server that never offers them, fails here.
func TestScienceHostGatesMoveTogether(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	script := read("scripts/build-windows.ps1")
	project := read("desktop/windows/Cage.Windows.csproj")
	frame := read("desktop/windows/ScienceFrame.cs")

	// One parameter, off unless given.
	if !regexp.MustCompile(`(?m)^\s*\[switch\]\$ScienceHostVerified,\s*$`).MatchString(script) {
		t.Fatal("build-windows.ps1 has no [switch]$ScienceHostVerified parameter")
	}
	// A release never carries the lifted gate (a reviewed commit lifts it
	// in the sources).
	if !regexp.MustCompile(`(?m)^if \(\$Release -and \$ScienceHostVerified\) \{ throw '[^']+' \}$`).MatchString(script) {
		t.Fatal("build-windows.ps1 does not refuse -Release with -ScienceHostVerified")
	}
	// Every build of the window passes the property explicitly (true or
	// false), so an environment variable cannot lift the window alone.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "Cage.Windows.csproj") && !strings.Contains(line, "'publish'") {
			t.Fatalf("build-windows.ps1 builds the window other than through the one publish: %s", line)
		}
	}
	// It alone decides both: the server's ldflag and the window's property.
	const lift = "-X github.com/sylvinus/agent-vm/internal/workbench.scienceHostVerified=windows"
	for _, want := range []string{
		"$scienceGo = if ($ScienceHostVerified) { ' " + lift + "' } else { '' }",
		"$scienceShell = if ($ScienceHostVerified) { 'true' } else { 'false' }",
		`"-p:ScienceHostVerified=$scienceShell"`,
	} {
		if strings.Count(script, want) != 1 {
			t.Fatalf("build-windows.ps1: want once: %s", want)
		}
	}
	if strings.Count(script, lift) != 1 || strings.Count(strings.ToLower(script), "sciencehostverified=") != 2 {
		t.Fatal("build-windows.ps1 lifts a gate outside $ScienceHostVerified")
	}
	ldflags := regexp.MustCompile(`(?m)^\s*\$ldflags = ".*"$`).FindString(script)
	if !strings.HasSuffix(ldflags, `$scienceGo"`) {
		t.Fatalf("cage.exe's ldflags do not carry the server's gate: %s", ldflags)
	}
	if !regexp.MustCompile(`(?s)'publish', 'desktop/windows/Cage\.Windows\.csproj'.*?"-p:ScienceHostVerified=\$scienceShell"`).MatchString(script) {
		t.Fatal("Cage.exe's publish does not carry the window's gate")
	}

	// The window's property maps to its symbol, and the symbol to HostVerified.
	if !strings.Contains(project, `<DefineConstants Condition="'$(ScienceHostVerified)' == 'true'">$(DefineConstants);SCIENCE_HOST_VERIFIED</DefineConstants>`) {
		t.Fatal("Cage.Windows.csproj does not map ScienceHostVerified to SCIENCE_HOST_VERIFIED")
	}
	if !strings.Contains(frame, "#if SCIENCE_HOST_VERIFIED\n    public const bool HostVerified = true;\n#else\n    public const bool HostVerified = false;\n#endif") {
		t.Fatal("ScienceFrame.HostVerified does not follow SCIENCE_HOST_VERIFIED")
	}
	// The defaults agree (a reviewed commit lifts both).
	defaults := regexp.MustCompile(`<ScienceHostVerified Condition="'\$\(ScienceHostVerified\)' == ''">(true|false)</ScienceHostVerified>`).FindStringSubmatch(project)
	if defaults == nil {
		t.Fatal("Cage.Windows.csproj has no default for ScienceHostVerified")
	}
	if (defaults[1] == "true") != scienceHostRenderVerified("windows") {
		t.Fatalf("the window's default (%s) and the server's (%q) differ", defaults[1], scienceHostVerified)
	}
}
