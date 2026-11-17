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
	"testing"
)

var root, rootErr = find()

func find() (string, error) {
	start, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("reporoot: working directory: %w", err)
	}
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
