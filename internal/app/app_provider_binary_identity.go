package app

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// providerBinaryIdentity is "the bytes we last looked at", cheap enough to
// re-derive on demand. Path is included because a version manager switch
// can repoint the symlink at an equally-old file with an equally-old mtime.
// Comparable by ==, which is the whole point of the upgrade tick; mtime is
// carried as unix nanos rather than a time.Time so that comparison is a plain
// value compare and not time.Time's wall/monotonic/location equality.
//
// Two owners share it: the provider-binary watcher, which compares it tick to
// tick to notice an upgrade, and the persisted Claude model catalog, which
// stamps it on the record so a restart can tell whether the binary that
// produced that answer is still the one on disk.
type providerBinaryIdentity struct {
	path        string
	size        int64
	modUnixNano int64
}

// resolveProviderBinaryIdentity stats the binary a provider would spawn.
// Stat only — a caller that finds nothing changed must cost no subprocess.
//
// A path that does not resolve is not an error here: an uninstalled provider
// is a normal state the startup detect probe already reports through the
// banner, and neither the upgrade watch nor the catalog seed has anything to
// say about it.
func (a *App) resolveProviderBinaryIdentity(providerName string) (providerBinaryIdentity, bool) {
	configured := a.providerBinaryPath(providerName)
	if configured == "" {
		return providerBinaryIdentity{}, false
	}
	resolved, err := exec.LookPath(configured)
	if err != nil {
		return providerBinaryIdentity{}, false
	}
	// A version manager (nvm, volta, mise) puts a symlink chain in front of
	// the real file, and an upgrade repoints the chain without touching the
	// shim. Follow it so size/mtime describe the file that will actually run.
	if target, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = target
	}
	info, err := os.Stat(resolved)
	if err != nil {
		log.Printf("provider binary: stat %s binary %s: %v", providerName, resolved, err)
		return providerBinaryIdentity{}, false
	}
	return providerBinaryIdentity{
		path:        resolved,
		size:        info.Size(),
		modUnixNano: info.ModTime().UnixNano(),
	}, true
}
