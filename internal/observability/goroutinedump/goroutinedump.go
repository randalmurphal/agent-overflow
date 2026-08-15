// Package goroutinedump writes a full goroutine dump of the running
// process on demand, triggered by a signal an operator can send to a
// process that is already wedged.
//
// It exists because the shipped binary is stripped: when a send wedged
// under a per-thread lock in production (incident 2026-08-15) there was
// no way to see WHERE the goroutines were parked — delve could not
// attach usefully, and the opt-in pprof listener
// (internal/observability/pprofserve) is no help when nobody set
// AGENT_OVERFLOW_PPROF before the process that is now stuck started.
// A signal handler is always armed, costs one parked goroutine, and
// answers the only question a wedge asks.
//
// Install is unix-only (SIGUSR1); the Windows build is a no-op.
package goroutinedump

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

const (
	privateDirPerm    os.FileMode = 0o700
	sensitiveFilePerm os.FileMode = 0o600
)

// FilePrefix is the basename prefix of every dump this package writes.
const FilePrefix = "goroutines-"

// Write dumps every goroutine's stack at pprof debug level 2 (the
// human-readable form, including the wait reason and how long each
// goroutine has been blocked — which is the whole point for a wedge)
// into a timestamped file under dir, and returns the path written.
//
// dir is created if missing. Dumps name process internals, so the file
// is written 0600 under a 0700 directory, matching internal/logging.
func Write(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("goroutinedump: no output directory")
	}
	if err := ensurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("goroutinedump: create %s: %w", dir, err)
	}
	// Colons are illegal in Windows filenames and awkward everywhere else,
	// so the timestamp is RFC3339-shaped with '-' separators in the time.
	path := filepath.Join(dir, FilePrefix+time.Now().Format("2006-01-02T15-04-05.000")+".txt")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, sensitiveFilePerm)
	if err != nil {
		return "", fmt.Errorf("goroutinedump: create %s: %w", path, err)
	}
	writeErr := pprof.Lookup("goroutine").WriteTo(f, 2)
	closeErr := f.Close()
	if writeErr != nil {
		return "", fmt.Errorf("goroutinedump: write %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("goroutinedump: close %s: %w", path, closeErr)
	}
	return path, nil
}

// ensurePrivateDir creates the log directory and REPAIRS its mode, the
// same heal internal/logging applies to the same directory. MkdirAll is
// a no-op on a directory that already exists, so a dir created earlier
// under a looser umask — or one this package's own MkdirAll created
// before the mode was tightened — would keep those permissions forever,
// and dumps name process internals.
func ensurePrivateDir(dir string) error {
	// Same-uid hygiene rather than a trust boundary, but the rule matches the
	// app's own path repairs (`repairAppOwnedTreeIfExists`): never chmod through
	// a symlink — a link planted at the logs path would redirect the mode change
	// (and the dumps) somewhere this package does not own.
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("goroutinedump: %s is a symlink; refusing to write dumps through it", dir)
	}
	if err := os.MkdirAll(dir, privateDirPerm); err != nil {
		return err
	}
	return os.Chmod(dir, privateDirPerm)
}
