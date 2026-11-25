package testutil

import (
	"os"
	"runtime"
	"testing"
)

// UnsyncableDir makes dir refuse to be opened until the test ends, so a
// sync of it (atomicfile.SyncDir) fails, while a file in it can still be
// made, renamed and removed: write and search permission without read.
// It skips the test where that cannot make the sync fail: on Windows,
// where a directory is not synced, and for a user that opens a directory
// it may not read (root).
func UnsyncableDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a directory is not synced on Windows")
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.Open(dir); err == nil {
		_ = f.Close()
		t.Skip("this user opens a directory it may not read, so its sync cannot be made to fail")
	}
}
