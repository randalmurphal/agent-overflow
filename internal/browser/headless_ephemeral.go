package browser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// The temp directories an EPHEMERAL browser profile lives in, and the one
// thing that can still be true after this process is gone: the directory
// outliving it.
//
// browserPersistSiteData=false is an ephemeral SESSION, not a suppressed
// write. Chromium has no in-memory profile mode, so the directory is the
// promise and Dispose removing it is what keeps it. A backend that was
// killed, crashed, or lost power runs no Dispose, and what it leaves in
// the temp directory is a full Chromium profile: cookies, tokens, local
// storage, for as long as the machine keeps its temp files. A serve host
// that is never at a desk is exactly where nobody notices.
//
// So each root records the pid that made it, and a later start sweeps the
// ones whose owner is gone. The marker is what makes the sweep safe: two
// backends share a temp directory, and a sweep that deleted by NAME would
// delete the other one's live session.

const (
	// ephemeralDirPrefix is the one name an ephemeral root is created
	// under, and the only thing the sweep considers at all.
	ephemeralDirPrefix = "ao-browser-ephemeral-"

	// ephemeralOwnerFile holds the decimal pid that created the root. A
	// root with no readable marker is left alone: this process cannot say
	// whose it is, and "cannot say" must never resolve to delete.
	ephemeralOwnerFile = "owner.pid"
)

// writeEphemeralOwner stamps a fresh root with this process's pid.
//
// A failure is reported to the caller's log rather than failing the
// profile: the marker decides whether a LATER run can reclaim the
// directory, and refusing to browse over a bookkeeping file would trade a
// working feature for a tidier temp directory. The unmarked root is then
// simply one the sweep leaves behind.
func writeEphemeralOwner(root string) error {
	return os.WriteFile(filepath.Join(root, ephemeralOwnerFile), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// readEphemeralOwner reads a root's owner pid back. Anything unreadable,
// unparseable or out of range answers false, which the sweep treats as
// "leave it alone".
func readEphemeralOwner(root string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(root, ephemeralOwnerFile))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// sweepEphemeralRoots removes every ephemeral profile root under tempRoot
// whose owner process is gone.
//
// alive is a parameter so the sweep is testable without pids: the rule
// worth pinning is WHICH directories it will touch, and a test that had to
// arrange a real dead process would be pinning the operating system
// instead.
//
// An empty tempRoot sweeps nothing. An engine built without one is a unit
// test's engine, and a sweep of the machine's real temp directory is not
// something a `go test` run may do.
func sweepEphemeralRoots(tempRoot string, alive func(pid int) bool, logf func(string, ...any)) {
	if tempRoot == "" {
		return
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		logf("browser: list %s for abandoned ephemeral profiles: %v", tempRoot, err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ephemeralDirPrefix) {
			continue
		}
		root := filepath.Join(tempRoot, entry.Name())
		pid, marked := readEphemeralOwner(root)
		if !marked || alive(pid) {
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			logf("browser: remove abandoned ephemeral profile %s: %v", root, err)
			continue
		}
		logf("browser: removed the ephemeral profile left by pid %d", pid)
	}
}

// ownerAlive reports whether a pid still names a running process, erring
// towards ALIVE in every case it cannot tell. The cost of a wrong "alive"
// is one directory that survives until the next start, and the cost of a
// wrong "dead" is deleting the live session of a backend that is using it.
func ownerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Windows opens a handle here and fails when there is no such
		// process, which makes this the whole answer there.
		return false
	}
	if runtime.GOOS == "windows" {
		// Signal is not: Windows supports only Kill, so asking would
		// report every live process as dead.
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	// EPERM is a process that exists and is somebody else's, which is
	// alive and is also a directory this user could not remove anyway.
	return err == nil || errors.Is(err, syscall.EPERM)
}
