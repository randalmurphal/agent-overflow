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
// WriteFile and CopyFile are for an executable whose bytes must be the file
// itself. They install it through procutil.InstallExecutable, as every
// executable test code writes must be:
// TestEveryTestExecutableIsWrittenWithForksHeldOff fails test code that
// writes one any other way.
//
// The package depends only on the standard library, procutil and reporoot,
// none of which imports another package of this module, so any test package
// but procutil's can use it.
package mockexec

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/testutil/reporoot"
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

// WriteFile installs data as the file at path with mode perm, which may be
// executable, and returns path. Use it where the file must hold data
// itself: a copied binary, a file whose bytes, size or mode the test reads,
// or a script that finds files beside it through $0. Use Write for any other
// mock script. A file or link already at path is replaced, never written
// through, and process starts are held off while the new file is open for
// writing (procutil.InstallExecutable).
func WriteFile(t testing.TB, path string, data []byte, perm fs.FileMode) string {
	t.Helper()
	return CopyFile(t, path, bytes.NewReader(data), perm)
}

// CopyFile is WriteFile with the bytes src yields, streamed rather than held
// in memory, for a copy of a binary.
func CopyFile(t testing.TB, path string, src io.Reader, perm fs.FileMode) string {
	t.Helper()
	if err := procutil.InstallExecutable(path, src, perm); err != nil {
		t.Fatalf("mockexec: %v", err)
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
	return reporoot.Path(t, "internal/testutil/mockexec/testdata/wrapper.sh")
}
