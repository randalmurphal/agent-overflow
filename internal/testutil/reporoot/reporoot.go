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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Observe puts the files at rels, slash paths from the checkout's root, in
// the calling test's cache key, for a test whose child process reads them,
// as make reads the Makefile. The test cache keys a result on the files the
// test process itself opens, so a pass recorded before such a file changed
// would otherwise be reused.
func Observe(t testing.TB, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		f, err := os.Open(Path(t, rel))
		if err != nil {
			t.Fatalf("reporoot: observe %s: %v", rel, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("reporoot: observe %s: %v", rel, err)
		}
	}
}

// goSourceRoots are the directories under the checkout whose trees hold Go
// packages, beside the root package itself, read from the Makefile's
// GO_PACKAGE_ROOTS: the one declaration of where the build's packages are.
var goSourceRoots = sync.OnceValues(func() ([]string, error) {
	if rootErr != nil {
		return nil, rootErr
	}
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, fmt.Errorf("reporoot: read GO_PACKAGE_ROOTS: %w", err)
	}
	return packageRoots(makefile)
})

// packageRoots reads the line `GO_PACKAGE_ROOTS := . ./dir/... ...` from a
// Makefile, the root package and each directory whose tree holds packages,
// and returns those directories. It fails on any other form, which GoFiles
// and ObserveGoSources would read wrongly.
func packageRoots(makefile []byte) ([]string, error) {
	const prefix = "GO_PACKAGE_ROOTS := "
	for line := range strings.Lines(string(makefile)) {
		value, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var dirs []string
		rootPackage := false
		for _, field := range strings.Fields(value) {
			dir, tree := strings.CutSuffix(strings.TrimPrefix(field, "./"), "/...")
			switch {
			case field == ".":
				rootPackage = true
			case tree && strings.HasPrefix(field, "./") && fs.ValidPath(dir) && dir != "." && !strings.Contains(dir, "$"):
				dirs = append(dirs, dir)
			default:
				return nil, fmt.Errorf("reporoot: GO_PACKAGE_ROOTS names %q; a root is . or ./dir/...", field)
			}
		}
		if !rootPackage {
			return nil, fmt.Errorf("reporoot: GO_PACKAGE_ROOTS (%s) leaves out the root package, which GoFiles lists", strings.TrimSpace(value))
		}
		return dirs, nil
	}
	return nil, fmt.Errorf("reporoot: the Makefile has no %q line", strings.TrimSpace(prefix))
}

// sourceRoots is goSourceRoots for a test.
func sourceRoots(t testing.TB) []string {
	t.Helper()
	dirs, err := goSourceRoots()
	if err != nil {
		t.Fatal(err)
	}
	return dirs
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
	dir := Dir(t)
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatalf("reporoot: read %s: %v", dir, err)
	}
	for _, sourceRoot := range sourceRoots(t) {
		err := filepath.WalkDir(filepath.Join(dir, sourceRoot), func(_ string, _ fs.DirEntry, err error) error {
			return err
		})
		if err != nil {
			t.Fatalf("reporoot: walk %s: %v", sourceRoot, err)
		}
	}
}
