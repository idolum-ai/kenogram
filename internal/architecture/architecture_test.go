package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPurePackagesDoNotImportStatefulLayers(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, dir := range []string{"internal/decl", "internal/plan"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, path := range files {
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range file.Imports {
				imp := strings.Trim(spec.Path.Value, `"`)
				for _, forbidden := range []string{"/internal/app", "/internal/backend", "/internal/proxy", "/internal/worldfs"} {
					if strings.HasSuffix(imp, forbidden) {
						t.Fatalf("%s imports stateful layer %s", path, imp)
					}
				}
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("root not found")
		}
		wd = parent
	}
}
