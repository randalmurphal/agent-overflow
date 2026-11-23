package netisolate

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A run's temp directory is a tmpfs whenever the host can spare the
// memory, so a file whose writes must reach a block device, such as a
// test that measures the I/O a process does, has nowhere to go inside it.
// The run's disk directory is that place: a per-run directory in
// diskParent (diskDirPrefix), created, owned, removed and swept like the Go
// work directory, and mounted at DiskDir.
const (
	// diskDirPrefix names a run's disk directory in diskParent (ownedtemp).
	diskDirPrefix = "ao-disk-"
	// diskDirMount is its fixed name in TempDir.
	diskDirMount = ".ao-disk"
)

// DiskDir is, inside a namespace Command built, a directory on disk that
// the run owns and that goes with it. It is the same path on every run, as
// TempDir is, so a cached test result that read it holds on the next run.
// A test makes its own directory inside, with os.MkdirTemp. Outside such a
// namespace nothing is mounted there.
func DiskDir() string { return filepath.Join(TempDir(), diskDirMount) }

// mountDiskDir mounts dir at DiskDir. Runs in the helper, after the private
// temp directory is mounted.
func mountDiskDir(dir string) error {
	target := DiskDir()
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if err := unix.Mount(dir, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount %s on %s: %w", dir, target, err)
	}
	return nil
}
