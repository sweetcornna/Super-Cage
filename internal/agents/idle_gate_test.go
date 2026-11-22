package agents

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Guarantee G (design 9.1, "Host work"): nothing here runs on its own. The
// catalog is data read when asked; the package starts no goroutine and arms no
// timer, so an environment that never adds another client pays nothing for it.
// A grep gate over the package's own source, not a runtime measurement.
func TestPackageStartsNoGoroutineAndArmsNoTimer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files found: %v", err)
	}
	timers := map[string]bool{"NewTicker": true, "NewTimer": true, "AfterFunc": true, "Tick": true, "After": true}
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				t.Errorf("%s starts a goroutine: the catalog package must not run anything by itself", file)
			case *ast.CallExpr:
				if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "time" && timers[selector.Sel.Name] {
						t.Errorf("%s calls time.%s: the catalog package must not arm a timer", file, selector.Sel.Name)
					}
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no non-test source file was checked")
	}
}

// Guarantee G: only two places can parse the catalog at all, the routes of the
// optional clients and the guest script of their jobs. Nothing the Claude path
// runs (start, attach, a terminal) imports it. An allowed importer that goes
// away or a new one fails here, so the list is looked at when it changes.
func TestOnlyTheOptionalClientCodeImportsTheCatalog(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	skipped := []string{".git", ".claude", "node_modules", "ui", "output", "dist"}
	var importers []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if slices.Contains(skipped, entry.Name()) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		relative := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(relative, "internal/agents/") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // a file that does not parse here cannot be told apart; the build will say
		}
		for _, spec := range parsed.Imports {
			if value, err := strconv.Unquote(spec.Path.Value); err == nil && strings.HasSuffix(value, "/internal/agents") {
				importers = append(importers, relative)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(importers)
	want := []string{"internal/cage/agent_install.go", "internal/workbench/agents.go"}
	if !slices.Equal(importers, want) {
		t.Fatalf("the importers of the catalog are %v, want %v: another importer may parse it on the Claude path (design 9.1)", importers, want)
	}
}
