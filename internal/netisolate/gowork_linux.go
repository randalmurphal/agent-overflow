//go:build linux

package netisolate

import (
	"bytes"
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
const (
	// goWorkPrefix names the per-run directories in diskParent (ownedtemp).
	goWorkPrefix = "ao-gowork-"
	// goWorkMount and goEnvName are the fixed names, in TempDir, of the
	// mounted work directory and the Go environment file naming it.
	goWorkMount = ".ao-go-work"
	goEnvName   = ".ao-go-env"
)

func goWorkPath() string { return filepath.Join(TempDir(), goWorkMount) }

func goEnvPath() string { return filepath.Join(TempDir(), goEnvName) }

// newGoWorkDir creates the run's Go work directory on disk. It returns nil
// when GOTMPDIR is set already, in the environment or the developer's Go
// environment file; that one stays.
func newGoWorkDir() (*ownedtemp.Shared, error) {
	if os.Getenv("GOTMPDIR") != "" {
		return nil, nil
	}
	userEnv, err := goenvfile.Read()
	if err != nil {
		return nil, err
	}
	if goenvfile.Lookup(userEnv, "GOTMPDIR") != "" {
		return nil, nil
	}
	dir, err := ownedtemp.MakeShared(diskParent(), goWorkPrefix)
	if err != nil {
		return nil, fmt.Errorf("create the Go work directory: %w", err)
	}
	return dir, nil
}

// sweepRunDirs removes the directories on disk of earlier runs whose
// processes have all exited (ownedtemp). What it cannot do goes to stderr.
func sweepRunDirs(stderr io.Writer) {
	parent := diskParent()
	for _, prefix := range []string{goWorkPrefix, diskTempPrefix} {
		if err := ownedtemp.Sweep(parent, prefix); err != nil {
			fmt.Fprintln(stderr, "netisolate: remove an earlier run's directories:", err)
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

// mountGoWork mounts dir at goWorkPath and writes the Go environment file
// that names it, after the developer's userEnv. Runs in the helper, after
// the private temp directory is mounted.
func mountGoWork(dir string, userEnv []byte) error {
	target := goWorkPath()
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if err := unix.Mount(dir, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount %s on %s: %w", dir, target, err)
	}
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
