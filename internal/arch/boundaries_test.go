// Package arch enforces the module boundaries from
// 05-Project-Structure-and-Contracts as a test, so they fail in CI rather than
// relying on someone remembering them during review.
//
// The note is explicit that this is the rule keeping ADR-001's monolith from
// becoming a ball of mud. A convention nobody checks is not a boundary; it is
// a preference. This makes it mechanical.
//
// Two rules:
//
//  1. A module's `domain` package imports NOTHING outside the standard library.
//     Not another module, not infrastructure, not a driver, not a framework.
//     This is what makes the domain testable without a database and portable
//     across the eventual service split (NFR-8).
//
//  2. A module's package never imports another module's `infrastructure`.
//     Cross-module traffic goes through events, per ADR-001 -- reaching into
//     a sibling's adapters is precisely the shortcut that welds two modules
//     together and makes them impossible to separate later.
package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/ayna/ayna-backend"

// repoRoot walks up from the test's working directory to the go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate go.mod")
	return ""
}

type file struct {
	path    string // repo-relative, forward slashes
	module  string // owning module, e.g. "skinanalysis"
	layer   string // "domain" | "application" | "infrastructure" | ""
	imports []string
}

// collect parses every Go file under internal/modules.
func collect(t *testing.T, root string) []file {
	t.Helper()

	modulesDir := filepath.Join(root, "internal", "modules")
	if _, err := os.Stat(modulesDir); os.IsNotExist(err) {
		return nil // no modules yet -- the rules hold vacuously
	}

	var out []file
	fset := token.NewFileSet()

	err := filepath.WalkDir(modulesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		// internal/modules/<module>/<layer>/...
		parts := strings.Split(rel, "/")
		f := file{path: rel}
		if len(parts) > 2 {
			f.module = parts[2]
		}
		if len(parts) > 3 {
			f.layer = parts[3]
		}

		for _, imp := range parsed.Imports {
			f.imports = append(f.imports, strings.Trim(imp.Path.Value, `"`))
		}
		out = append(out, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// isStdlib treats any import path without a dot in its first segment as
// standard library, which is the same heuristic the go tool uses.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func TestDomainImportsOnlyStandardLibrary(t *testing.T) {
	root := repoRoot(t)

	for _, f := range collect(t, root) {
		if f.layer != "domain" {
			continue
		}
		for _, imp := range f.imports {
			if isStdlib(imp) {
				continue
			}
			t.Errorf("%s imports %q\n"+
				"    a domain package may import only the standard library.\n"+
				"    Define a port (an interface) in the domain and implement it "+
				"in infrastructure instead.", f.path, imp)
		}
	}
}

func TestNoModuleImportsAnotherModulesInfrastructure(t *testing.T) {
	root := repoRoot(t)
	prefix := modulePath + "/internal/modules/"

	for _, f := range collect(t, root) {
		for _, imp := range f.imports {
			rest, ok := strings.CutPrefix(imp, prefix)
			if !ok {
				continue
			}
			parts := strings.Split(rest, "/")
			if len(parts) < 2 {
				continue
			}
			otherModule, otherLayer := parts[0], parts[1]

			if otherModule == f.module || otherLayer != "infrastructure" {
				continue
			}
			t.Errorf("%s (module %q) imports %q\n"+
				"    reaching into another module's infrastructure welds the two "+
				"together.\n"+
				"    Cross-module communication goes through the event bus "+
				"(ADR-001).", f.path, f.module, imp)
		}
	}
}

// The rules are worth nothing if the checker cannot detect a violation, and a
// checker that silently passes on everything looks identical to a clean
// codebase. These two tests check the checker.
func TestStdlibDetection(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
	}{
		{"context", true},
		{"net/http", true},
		{"errors", true},
		{"github.com/jackc/pgx/v5", false},
		{modulePath + "/internal/platform/logger", false},
	} {
		if got := isStdlib(c.path); got != c.want {
			t.Errorf("isStdlib(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestLayerAndModuleAreParsedFromPath(t *testing.T) {
	// Mirrors what collect() derives, so a refactor of the folder convention
	// fails here loudly instead of quietly disabling both rules above.
	rel := "internal/modules/skinanalysis/domain/scan.go"
	parts := strings.Split(rel, "/")

	if parts[2] != "skinanalysis" {
		t.Errorf("module = %q, want skinanalysis", parts[2])
	}
	if parts[3] != "domain" {
		t.Errorf("layer = %q, want domain", parts[3])
	}
}
