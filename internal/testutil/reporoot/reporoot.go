// Package reporoot locates the checkout a test binary was started in.
//
// `go test` starts a test binary in its package directory, so the checkout is
// the nearest directory at or above it that holds go.mod. The directory is
// read at package initialization, before a TestMain or a test can change it.
//
// A source path from runtime.Caller cannot locate the checkout: the Make
// targets build with -trimpath, which makes those paths module-relative.
//
// Standard library only, so any test package can import it.
package reporoot

import (
	"errors"
	"fmt"
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

var root, rootErr = fromWorkingDirectory()

func fromWorkingDirectory() (string, error) {
	start, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("reporoot: working directory: %w", err)
	}
	return find(start)
}

// find returns the nearest directory at or above start that holds go.mod.
func find(start string) (string, error) {
	for dir := start; ; {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("reporoot: look for go.mod in %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("reporoot: no go.mod at or above %s: the test binary must start inside the checkout, as `go test` starts it", start)
		}
		dir = parent
	}
}

// Dir returns the checkout's root directory.
func Dir(t testing.TB) string {
	t.Helper()
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	return root
}

// Path returns the absolute path of rel, a slash-separated path relative to
// the checkout's root.
func Path(t testing.TB, rel string) string {
	t.Helper()
	return filepath.Join(Dir(t), filepath.FromSlash(rel))
}

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

// goSourceRoots are the directories under the checkout that hold Go packages,
// beside the root package itself: the Makefile's GO_PACKAGE_ROOTS.
var goSourceRoots = []string{"cmd", "internal"}

// GoFiles lists every Go file the Makefile's package roots hold, tests and
// testdata included, as slash paths from the checkout's root: the root
// package's, and those under the package roots outside dot-directories.
func GoFiles(t testing.TB) []string {
	t.Helper()
	tree := os.DirFS(Dir(t))
	files, err := fs.Glob(tree, "*.go")
	if err != nil {
		t.Fatalf("reporoot: list the root package: %v", err)
	}
	for _, sourceRoot := range goSourceRoots {
		err := fs.WalkDir(tree, sourceRoot, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name != sourceRoot && strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(name, ".go") {
				files = append(files, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("reporoot: walk %s: %v", sourceRoot, err)
		}
	}
	return files
}

// ObserveGoSources puts the Go source tree in the calling test's cache key.
//
// The test cache keys a result on the files and directories the test process
// itself opens. A test that inspects the tree through a child process, such as
// `go list`, is otherwise answered from a pass recorded before the tree
// changed, in this checkout or in another that built the same test binary.
// Reading the root and every directory under the package roots records each
// entry's name, size and modification time.
func ObserveGoSources(t testing.TB) {
	t.Helper()
	GoFiles(t)
}
