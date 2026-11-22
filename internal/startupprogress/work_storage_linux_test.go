package startupprogress

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// storageDir is a directory whose writes reach a block device, which
// /proc/self/io's write_bytes counts. t.TempDir is on a tmpfs inside
// ao-netns and on hosts that mount /tmp as one; the package directory is
// then the nearest storage.
func storageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	if fs.Type != unix.TMPFS_MAGIC && fs.Type != unix.RAMFS_MAGIC {
		return dir
	}
	dir, err := os.MkdirTemp(".", ".storage-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	return dir
}
