// SPDX-License-Identifier: Apache-2.0

package collector_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedRootLiterals are the files permitted to name an absolute /proc or
// /sys path: fsroot is where those constants are defined, and tests assert on
// the paths they produce.
var allowedRootLiterals = map[string]bool{
	"internal/fsroot/fsroot.go": true,
}

// TestNoHardcodedRoots enforces SPEC.md §Hard constraints #3: all filesystem
// access goes through internal/fsroot, so tests can point the exporter at a
// fixture tree.
//
// A hardcoded "/proc/stat" would not fail any parser test — it would read the
// developer's own machine and quietly pass, then disagree with the golden file
// on someone else's. This is the check that makes that impossible.
func TestNoHardcodedRoots(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if allowedRootLiterals[filepath.ToSlash(rel)] {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, forbidden := range []string{"/proc/", "/sys/"} {
				if strings.HasPrefix(value, forbidden) || value == strings.TrimSuffix(forbidden, "/") {
					t.Errorf("%s:%d: hardcoded root %q — build the path through fsroot.Roots",
						rel, fset.Position(lit.Pos()).Line, value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// repoRoot walks up from this package's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
