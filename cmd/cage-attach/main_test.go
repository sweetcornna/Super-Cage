package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The program exists to stay small: one stray import of any of these
// brings back the memory of the whole cage into every attached window.
var forbidden = []string{
	"github.com/sylvinus/agent-vm/internal/workbench",
	"github.com/sylvinus/agent-vm/internal/cage",
	"net/http",
	"crypto/tls",
	"github.com/gorilla/websocket",
	"runtime/cgo",
}

func TestImportGraphStaysSlim(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		cmd := exec.Command("go", "list", "-deps", ".")
		cmd.Env = append(cmd.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list for %s: %v\n%s", goos, err, out)
		}
		deps := map[string]bool{}
		for _, dep := range strings.Fields(string(out)) {
			deps[dep] = true
		}
		if !deps["github.com/sylvinus/agent-vm/internal/attach"] {
			t.Fatalf("%s: the dependency list does not look like cage-attach's:\n%s", goos, out)
		}
		for _, name := range forbidden {
			if deps[name] {
				t.Errorf("%s: cage-attach depends on %s; see the package comment", goos, name)
			}
		}
	}
}
