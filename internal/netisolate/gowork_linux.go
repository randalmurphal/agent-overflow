//go:build linux

package netisolate

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	// goWorkPrefix names the per-run directories under the host's temp
	// directory (ownedtemp).
	goWorkPrefix = "ao-gowork-"
	// goWorkMount and goEnvName are the fixed names, in TempDir, of the
	// mounted work directory and the Go environment file naming it.
	goWorkMount = ".ao-go-work"
	goEnvName   = ".ao-go-env"
)

func goWorkPath() string { return filepath.Join(TempDir(), goWorkMount) }

func goEnvPath() string { return filepath.Join(TempDir(), goEnvName) }

// newGoWorkDir creates the run's Go work directory on disk and removes those
// of earlier runs that ended without their release. It returns "" when
// GOTMPDIR is set already, in the environment or the developer's Go
// environment file; that one stays. release removes the directory; a run
// killed first leaves it to the next run's sweep, whose failures are
// written to stderr.
func newGoWorkDir(stderr io.Writer) (dir string, release func() error, err error) {
	noop := func() error { return nil }
	if os.Getenv("GOTMPDIR") != "" {
		return "", noop, nil
	}
	userEnv, err := readGoEnvFile()
	if err != nil {
		return "", noop, err
	}
	if goEnvFileSets(userEnv, "GOTMPDIR") {
		return "", noop, nil
	}
	parent := hostTempDir()
	if err := ownedtemp.Sweep(parent, goWorkPrefix); err != nil {
		fmt.Fprintln(stderr, "netisolate: remove leftover Go work directories:", err)
	}
	dir, release, err = ownedtemp.Make(parent, goWorkPrefix)
	if err != nil {
		return "", noop, fmt.Errorf("create the Go work directory: %w", err)
	}
	return dir, release, nil
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

// readGoEnvFile returns the Go environment file the go command would read
// now (GOENV, else go/env in the user config directory), or nothing when
// there is none.
func readGoEnvFile() ([]byte, error) {
	path := os.Getenv("GOENV")
	if path == "off" {
		return nil, nil
	}
	if path == "" {
		config, err := os.UserConfigDir()
		if err != nil {
			return nil, nil // the go command then reads no file either
		}
		path = filepath.Join(config, "go", "env")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the Go environment file: %w", err)
	}
	return data, nil
}

func goEnvFileSets(data []byte, name string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if ok && strings.TrimSpace(key) == name && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}
