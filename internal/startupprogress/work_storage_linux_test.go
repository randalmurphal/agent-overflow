package startupprogress

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// storageDir is a directory whose writes reach a block device, which
// /proc/self/io's write_bytes counts. t.TempDir is on a tmpfs inside
// ao-netns and on hosts that mount /tmp as one; /var/tmp is then the
// storage. Not the package directory: a directory made and removed there
// changes that directory's entry in the listing of internal/, which every
// test that reads the source tree records, so those tests would rerun in
// each fresh checkout.
func storageDir(t *testing.T) string {
	t.Helper()
	if dir := t.TempDir(); onStorage(t, dir) {
		return dir
	}
	const varTmp = "/var/tmp"
	if !onStorage(t, varTmp) {
		t.Fatalf("both the test's temp directory and %s are in memory; this test needs a directory on a block device", varTmp)
	}
	dir, err := os.MkdirTemp(varTmp, "ao-startupprogress-storage-")
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

// onStorage reports whether dir is on a filesystem other than tmpfs or ramfs.
func onStorage(t *testing.T, dir string) bool {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	return fs.Type != unix.TMPFS_MAGIC && fs.Type != unix.RAMFS_MAGIC
}
