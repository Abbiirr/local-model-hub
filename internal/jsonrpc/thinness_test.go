package jsonrpc

// thinness_test.go is the structural thin-transport guard (gate #p63 / design §5 "structural
// thinness check"): the JSON-RPC surface must import ONLY the single service core + the standard
// library. A future edit that reaches for a db/engine/residency/instances package here — i.e.
// re-implements business logic in the transport — fails this test, not just code review.
//
// It parses the package's own (non-test) source files and asserts every non-stdlib import is
// exactly internal/core. Stdlib imports (no domain in the path, or the localhub module prefix
// absent) are allowed.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPackageImportsOnlyCoreAndStdlib(t *testing.T) {
	const modulePrefix = "github.com/localhub/localhub/internal/"
	const allowed = modulePrefix + "core"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue // guard only the production surface; tests may import config etc.
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(path, modulePrefix) {
				continue // stdlib (or external) — the domain guard is about localhub internal pkgs
			}
			if path != allowed {
				t.Errorf("%s imports %q; the thin JSON-RPC surface may import ONLY %q (design §5, gate #p63). "+
					"Business logic belongs in core.Service, not here.", name, path, allowed)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no production .go files found to check; the guard would be vacuous")
	}
}
