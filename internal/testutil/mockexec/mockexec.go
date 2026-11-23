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
// itself. They hold process starts in this process off while the file is
// open for writing, as every executable test code writes needs:
// TestEveryTestExecutableIsWrittenWithForksHeldOff fails test code that
// writes one any other way.
//
// The package depends only on the standard library, procutil and reporoot,
// none of which imports another package of this module, so any test package
// but procutil's can use it.
package mockexec

import (
	"errors"
	"fmt"
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

// WriteFile writes data to path with perm, which may be executable, and
// returns path. Use it where the file must hold data itself: a copied
// binary, a file whose bytes, size or mode the test reads, or a script that
// finds files beside it through $0. Use Write for any other mock script.
//
// Process starts in this process are held off while the file is open for
// writing (procutil.HoldForks): a child forked then keeps the write
// descriptor until its own exec, and running the file meanwhile fails with
// "text file busy". It holds them off for the whole write rather than
// waiting after it (procutil.WriteExecutable), because a start that asks for
// a new user namespace, as internal/netisolate's do, releases the fork lock
// before its child execs.
func WriteFile(t testing.TB, path string, data []byte, perm fs.FileMode) string {
	t.Helper()
	if err := writeFile(path, data, perm); err != nil {
		t.Fatalf("mockexec: %v", err)
	}
	return path
}

func writeFile(path string, data []byte, perm fs.FileMode) error {
	release := procutil.HoldForks()
	defer release()
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// CopyFile is WriteFile with the bytes of the file at src, streamed rather
// than read into memory, for a copy of a binary. path must not exist.
func CopyFile(t testing.TB, path, src string, perm fs.FileMode) string {
	t.Helper()
	if err := copyFile(path, src, perm); err != nil {
		t.Fatalf("mockexec: %v", err)
	}
	return path
}

func copyFile(path, src string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", src, closeErr))
		}
	}()
	release := procutil.HoldForks()
	defer release()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, path, err)
	}
	return nil
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
