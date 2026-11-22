package agents

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
