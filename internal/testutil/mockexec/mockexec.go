// Package mockexec writes mock executables for tests without paying macOS's
// first-exec assessment for each one.
//
// macOS assesses every newly written executable on its first exec, which costs
// from 150ms to over a second and serializes across concurrently running test
// binaries. Write instead links a stable, already-assessed wrapper at the
// requested path and stores the script beside it; the wrapper runs the script
// with the interpreter named by its shebang (/bin/sh without one). The process
// ID is the wrapper's, so signals, process groups and exit codes behave as if
// the script were executed directly.
//
// The package depends only on the standard library so any test package can use
// it, including the packages internal/testutil itself imports.
package mockexec

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Write installs script as an executable at path and returns path. An existing
// file or link at path is replaced, never written through, so a test may call
// Write again to swap the script. Callers must not write to or chmod path
// directly: it is a link to the shared wrapper.
func Write(t testing.TB, path, script string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mockexec: create dir: %v", err)
	}
	if err := os.WriteFile(path+".payload", []byte(script), 0o644); err != nil {
		t.Fatalf("mockexec: write payload: %v", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mockexec: replace %s: %v", path, err)
	}
	wrapper := wrapperPath(t)
	// A hard link keeps path a regular file, so code that resolves symlinks
	// still finds the payload beside it. Fall back to a symlink across
	// filesystems.
	if err := os.Link(wrapper, path); err != nil {
		if err := os.Symlink(wrapper, path); err != nil {
			t.Fatalf("mockexec: link wrapper: %v", err)
		}
	}
	return path
}

// WriteIn is Write for a script named name in dir.
func WriteIn(t testing.TB, dir, name, script string) string {
	t.Helper()
	return Write(t, filepath.Join(dir, name), script)
}

func wrapperPath(t testing.TB) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("mockexec: resolve wrapper: runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(source), "testdata", "wrapper.sh")
}
