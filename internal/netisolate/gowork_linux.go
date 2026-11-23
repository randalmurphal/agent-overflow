//go:build linux

package netisolate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agent-overflow/internal/goenvfile"
	"agent-overflow/internal/ownedtemp"

	"golang.org/x/sys/unix"
)

// The go command keeps its work directory, test binaries included, under
// GOTMPDIR or else the temp directory: up to several GB at once for a
// whole-tree run, too much for the namespace's RAM temp directory. A run's
// go commands get a directory on disk instead.
//
// It cannot be named in the run's environment. t.TempDir creates its
// directories under GOTMPDIR when that is set, which would move every
// test's files back to disk. And the test cache keys a result by the
// GOTMPDIR (and GOENV) the go command resolves for a test that reads it,
// t.TempDir included, so a per-run value would make every result miss.
// So the launcher creates a directory per run on disk (newGoWorkDir), the
// helper mounts it at a fixed path inside the private temp directory
// (goWorkMount), and the go commands find it through a Go environment file
// at a fixed path (goEnvPath) that copies the developer's and adds
// GOTMPDIR. Tests do not see GOTMPDIR in their environment, and every value
// the cache records is the same on every run.
//
// The same directory is the run's one directory on disk for tests too: a
// tmpfs temp directory leaves a test whose writes must reach a block device,
// such as one that measures the I/O a process does, nowhere to put them.
// DiskDir is a directory inside it, so the run makes, mounts and sweeps one
// directory for both. It is made even when the developer's GOTMPDIR stays.
const (
	// goWorkPrefix names the per-run directories in diskParent (ownedtemp).
	goWorkPrefix = "ao-gowork-"
	// goWorkMount and goEnvName are the fixed names, in TempDir, of the
	// mounted work directory and the Go environment file naming it.
	goWorkMount = ".ao-go-work"
	goEnvName   = ".ao-go-env"
	// diskDirName is DiskDir's name in the Go work directory.
	diskDirName = "ao-disk"
)

func goWorkPath() string { return filepath.Join(TempDir(), goWorkMount) }

func goEnvPath() string { return filepath.Join(TempDir(), goEnvName) }

// DiskDir is, inside a namespace Command built, a directory on disk that
// the run owns and that goes with it. It is the same path on every run, as
// TempDir is, so a cached test result that read it holds on the next run.
// A test makes its own directory inside, with os.MkdirTemp. Outside such a
// namespace nothing is mounted there.
func DiskDir() string { return filepath.Join(goWorkPath(), diskDirName) }

// newGoWorkDir creates the run's Go work directory on disk, with DiskDir's
// directory in it. goTmpDir reports whether the go command is to use it:
// not when it has a GOTMPDIR already, from the environment or the
// developer's Go environment file; that one stays.
func newGoWorkDir() (dir *ownedtemp.Shared, goTmpDir bool, err error) {
	given, err := goenvfile.Getenv("GOTMPDIR")
	if err != nil {
		return nil, false, err
	}
	dir, err = ownedtemp.MakeShared(diskParent(), goWorkPrefix)
	if err != nil {
		return nil, false, fmt.Errorf("create the Go work directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir.Dir, diskDirName), 0o700); err != nil {
		return nil, false, errors.Join(fmt.Errorf("create the run's disk directory: %w", err), dir.Release())
	}
	return dir, given == "", nil
}

// sweepRunDirs removes the directories on disk of earlier runs whose
// processes have all exited (ownedtemp). What it cannot do goes to stderr.
func sweepRunDirs(stderr io.Writer) {
	parent := diskParent()
	for _, prefix := range []string{goWorkPrefix, diskTempPrefix} {
		if err := ownedtemp.Sweep(parent, prefix); err != nil {
			warn(stderr, "netisolate: remove an earlier run's directories:", err)
		}
	}
}

// hostTempDir is the temp directory outside any namespace: a namespace's
// own would be hidden by the private one the new namespace mounts.
func hostTempDir() string {
	dir := os.TempDir()
	if dir != TempDir() {
		return dir
	}
	if host := os.Getenv(EnvHostTempDir); host != "" {
		return host
	}
	return "/tmp"
}

// diskParent is where a run's directories on disk go: the host's temp
// directory, or /var/tmp where that is a tmpfs, as /tmp is on Fedora, Arch
// and Ubuntu 25.10, since the point of these directories is to keep their
// gigabytes out of memory.
func diskParent() string {
	dir := hostTempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err == nil && stat.Type == unix.TMPFS_MAGIC {
		return "/var/tmp"
	}
	return dir
}

// mountGoWork mounts dir at goWorkPath. Runs in the helper, after the
// private temp directory is mounted.
func mountGoWork(dir string) error {
	target := goWorkPath()
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if err := unix.Mount(dir, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount %s on %s: %w", dir, target, err)
	}
	return nil
}

// writeGoEnv writes the Go environment file that names the mounted Go work
// directory as GOTMPDIR, after the developer's userEnv.
func writeGoEnv(userEnv []byte) error {
	target := goWorkPath()
	var content bytes.Buffer
	content.Write(userEnv)
	if len(userEnv) > 0 && !bytes.HasSuffix(userEnv, []byte("\n")) {
		content.WriteByte('\n')
	}
	fmt.Fprintf(&content, "GOTMPDIR=%s\n", target)
	if err := os.WriteFile(goEnvPath(), content.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write the run's Go environment file: %w", err)
	}
	return nil
}
