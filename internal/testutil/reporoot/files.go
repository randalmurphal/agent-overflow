package reporoot

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Files lists the entries of the directory dir, a slash path from the
// checkout's root, that are files whose names match pattern (path.Match), as
// slash paths from the root. It fails when none does, so a scan cannot pass
// by reading nothing.
func Files(t testing.TB, dir, pattern string) []string {
	t.Helper()
	entries, err := os.ReadDir(Path(t, dir))
	if err != nil {
		t.Fatalf("reporoot: read %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		matched, err := path.Match(pattern, entry.Name())
		if err != nil {
			t.Fatalf("reporoot: match %q: %v", pattern, err)
		}
		if matched && !entry.IsDir() {
			files = append(files, path.Join(dir, entry.Name()))
		}
	}
	if len(files) == 0 {
		t.Fatalf("reporoot: no file in %s matches %s", dir, pattern)
	}
	return files
}

// ParseGoFile parses the Go file at rel, a slash path from the checkout's
// root, naming its positions by rel.
func ParseGoFile(t testing.TB, fset *token.FileSet, rel string, mode parser.Mode) (*ast.File, error) {
	t.Helper()
	src, err := os.ReadFile(Path(t, rel))
	if err != nil {
		return nil, err
	}
	return parser.ParseFile(fset, rel, src, mode)
}

// GoFiles lists every Go file the Makefile's package roots hold, tests and
// testdata included, as slash paths from the checkout's root: the root
// package's, and those Walk visits under the package roots outside
// dot-directories.
func GoFiles(t testing.TB) []string {
	t.Helper()
	root := Dir(t)
	files, err := fs.Glob(os.DirFS(root), "*.go")
	if err != nil {
		t.Fatalf("reporoot: list the root package: %v", err)
	}
	for _, sourceRoot := range sourceRoots(t) {
		top := Path(t, sourceRoot)
		err := Walk(t, sourceRoot, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name != top && strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(name, ".go") {
				rel, err := filepath.Rel(root, name)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("reporoot: walk %s: %v", sourceRoot, err)
		}
	}
	return files
}
