package goroutinedump

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreatesDirAndDumpsStacks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")

	path, err := Write(dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("dump landed at %s, want it under %s", path, dir)
	}
	if !strings.HasPrefix(filepath.Base(path), FilePrefix) {
		t.Fatalf("dump basename = %q, want the %q prefix", filepath.Base(path), FilePrefix)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	// debug=2 is the panic-traceback form — per-goroutine stacks with the
	// wait reason in brackets — not debug=1's aggregated counts. The wait
	// reason is the whole point when the question is "what is this wedged
	// on", so assert on the shape, not just on "a file exists".
	if !strings.Contains(string(body), "goroutine ") || !strings.Contains(string(body), "[running]:") {
		t.Fatalf("dump is not debug=2 per-goroutine stacks:\n%s", body)
	}
	if !strings.Contains(string(body), "TestWriteCreatesDirAndDumpsStacks") {
		t.Fatalf("dump does not contain the calling goroutine's stack:\n%s", body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if perm := info.Mode().Perm(); perm != sensitiveFilePerm {
		t.Fatalf("dump perms = %v, want %v — dumps name process internals", perm, sensitiveFilePerm)
	}
}

// MkdirAll is a no-op on a directory that already exists, so a dir minted under
// a looser umask (or by an earlier build) would keep those permissions forever
// while this package wrote 0600 dumps into it. The mode is REPAIRED on every
// write, which is the same heal internal/logging applies to the same directory.
func TestWriteRepairsALooseDirectoryMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed loose dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod loose dir: %v", err)
	}

	if _, err := Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != privateDirPerm {
		t.Fatalf("dir perms = %v, want %v — dumps name process internals", perm, privateDirPerm)
	}
}

func TestWriteRefusesEmptyDir(t *testing.T) {
	if _, err := Write(""); err == nil {
		t.Fatal("Write(\"\") succeeded; an unresolvable log dir must be reported")
	}
}

func TestWriteDoesNotOverwriteWithinTheSameMillisecond(t *testing.T) {
	dir := t.TempDir()
	first, err := Write(dir)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// Same-millisecond collisions are possible; O_EXCL means the second dump
	// fails loudly rather than silently replacing the first one's evidence.
	// Either outcome is acceptable — a clobber is not.
	second, err := Write(dir)
	if err == nil && second == first {
		t.Fatalf("second Write reused path %s", first)
	}
}
