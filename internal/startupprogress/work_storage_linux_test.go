package startupprogress

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/netisolate"
)

// storageDir is a directory whose writes reach a block device, which
// /proc/self/io's write_bytes counts. t.TempDir is on a tmpfs inside
// ao-netns whenever the host can spare the memory, and on hosts that mount
// /tmp as one; the run's disk directory, which the namespace owns and
// removes, is then the storage (netisolate.DiskDir). Not the package
// directory: a directory made and removed there changes that directory's
// entry in the listing of internal/, which every test that reads the
// source tree records, so those tests would rerun in each fresh checkout.
func storageDir(t *testing.T) string {
	t.Helper()
	if dir := t.TempDir(); onStorage(t, dir) {
		return dir
	}
	disk := netisolate.DiskDir()
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("the test's temp directory is in memory and there is no run disk directory (%v); run the test through make go-test, which provides one", err)
	}
	if !onStorage(t, disk) {
		t.Fatalf("the run's disk directory %s is in memory", disk)
	}
	dir, err := os.MkdirTemp(disk, "startupprogress-")
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
