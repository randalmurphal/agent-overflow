package provideraccounts

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Claude Code's own OAuth refresh runs under proper-lockfile locks, and Agent
// Overflow has to hold them for any write to the canonical credential.
//
// The CLI re-reads `.credentials.json` from disk under its lock immediately
// before every refresh, so an account switch cannot make a running process
// refresh the outgoing account's token. The one real race is narrower and
// worse: if AO's atomic rename lands INSIDE a CLI's token POST round trip —
// after its locked read, before its write — the CLI's compare-and-swap sees a
// different refresh token on disk, adopts the disk value, and drops the
// rotation it has already paid for. That retires the outgoing account's
// refresh token server-side with nothing holding the replacement, which is a
// login the user can only fix by signing in again.
//
// Locks observed in the 2.1.257 binary (mkdir-based proper-lockfile
// directories), in the order the CLI takes them:
//
//   - `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock` — held across read → POST →
//     write, stale after 60s, mtime touched every 5s by its holder.
//   - `${realpath(configDir)}.lock` — the legacy lock, taken second, covering
//     the same section.
//   - `$CLAUDE_CONFIG_DIR/.storage-write.lock` — the secureStorage mutation
//     lock, taken innermost, inside the refresh section. It wraps EVERY
//     secureStorage write: the credential read-modify-write and logout, not
//     just refreshes, so it is the only lock a sign-in or a sign-out takes.
//     Its own settings are 10 retries at 100ms and stale after 15s, and
//     proper-lockfile is called with `realpath: false`, so it is named from
//     the unresolved config home. Its base is
//     `$CLAUDE_SECURESTORAGE_CONFIG_DIR` when set; AO clears that variable on
//     every canonical-home spawn, so the config home is the base here.
//
// The macOS credential lives in the Keychain rather than in the config home,
// but all three locks are still scoped to the config home, so they apply
// there unchanged.
const (
	claudeRefreshLockName      = ".oauth_refresh.lock"
	claudeStorageWriteLockName = ".storage-write.lock"
	// claudeRefreshLockStale mirrors the CLI's own threshold: a lock whose
	// mtime has not moved within it is abandoned (a crashed CLI), and
	// proper-lockfile reclaims it rather than waiting forever. Each lock
	// carries the threshold the CLI configured for it; reclaiming a live
	// lock early is the same collision the lock exists to prevent.
	claudeRefreshLockStale      = 60 * time.Second
	claudeStorageWriteLockStale = 15 * time.Second
	// claudeRefreshLockPoll is proper-lockfile's retry interval.
	claudeRefreshLockPoll = 100 * time.Millisecond
	// claudeRefreshLockWait is AO's own budget, not a CLI-observed value. The
	// section it waits on is one token round trip, and the caller is a
	// user-visible credential transaction whose RPC gives up at 60s; waiting
	// longer than the work being protected would trade one failure for a
	// worse one.
	claudeRefreshLockWait = 10 * time.Second
)

// ErrCredentialLockBusy reports that one of Claude's credential write locks
// stayed held for the whole budget. The write is REFUSED rather than forced:
// landing inside a refresh window is how a single-use refresh token gets
// retired with no replacement saved, so failing the operation is the
// recoverable outcome and overwriting is not.
var ErrCredentialLockBusy = errors.New("provideraccounts: the Claude CLI is writing its credentials; try again")

// lockCanonicalCredentialWrite takes the provider's own write locks for
// configHome, in the CLI's order, and returns the release. Only Claude has
// them; every other provider gets a no-op release so callers stay uniform.
//
// The locks are held across the credential write and nothing else — never a
// subprocess AO spawns and never a network call — because a lock this process
// holds while waiting on a CLI is a lock the CLI needs to make progress.
// (The darwin write is a `security(1)` call, which is the write itself and is
// bounded by it.)
func (c *Credentials) lockCanonicalCredentialWrite(providerName, configHome string) (func(), error) {
	if providerName != "claude" {
		return func() {}, nil
	}
	var held []string
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			if err := os.Remove(held[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				// A lock AO cannot release is one the CLI blocks on until
				// its staleness threshold passes. Nothing above can repair
				// it, so it is reported here.
				log.Printf("provideraccounts: release claude credential lock %s: %v", held[i], err)
			}
		}
	}
	for _, lock := range claudeCredentialLockPaths(configHome) {
		if err := acquireLockDirectory(lock.path, claudeRefreshLockWait, lock.stale); err != nil {
			release()
			return nil, err
		}
		held = append(held, lock.path)
	}
	return release, nil
}

// claudeCredentialLock is one of the CLI's lock directories with the staleness
// threshold that CLI configured for it.
type claudeCredentialLock struct {
	path  string
	stale time.Duration
}

// claudeCredentialLockPaths lists the CLI's lock directories in acquisition
// order. The legacy lock is named from the RESOLVED config home, because
// proper-lockfile resolves the target's realpath before appending `.lock`; a
// home reached through a symlink would otherwise take a different lock than
// the CLI does and protect nothing. The secureStorage lock is the opposite
// case: the CLI passes `realpath: false` for it, so it is named from the home
// as given.
func claudeCredentialLockPaths(configHome string) []claudeCredentialLock {
	home := filepath.Clean(configHome)
	resolved := home
	if real, err := filepath.EvalSymlinks(home); err == nil {
		resolved = real
	}
	return []claudeCredentialLock{
		{path: filepath.Join(home, claudeRefreshLockName), stale: claudeRefreshLockStale},
		{path: resolved + ".lock", stale: claudeRefreshLockStale},
		{path: filepath.Join(home, claudeStorageWriteLockName), stale: claudeStorageWriteLockStale},
	}
}

// acquireLockDirectory implements proper-lockfile's acquisition: the lock IS
// the directory, so `mkdir` is the atomic test-and-set, and a directory whose
// mtime has not been touched within stale belongs to a process that died
// holding it.
//
// The caller's parent directory is created first when it is missing: the
// canonical home is created by the write this guards, and a lock that cannot
// be taken because the home does not exist yet would refuse every first write.
func acquireLockDirectory(path string, wait, stale time.Duration) error {
	deadline := time.Now().Add(wait)
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !time.Now().Before(deadline) {
			return fmt.Errorf("%w (%s)", ErrCredentialLockBusy, path)
		}
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
				return fmt.Errorf("provideraccounts: prepare credential lock %s: %w", path, mkErr)
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("provideraccounts: take credential lock %s: %w", path, err)
		}
		info, statErr := os.Lstat(path)
		switch {
		case errors.Is(statErr, fs.ErrNotExist):
			// Released between the mkdir and the stat. Retry immediately.
		case statErr != nil:
			return fmt.Errorf("provideraccounts: inspect credential lock %s: %w", path, statErr)
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			return fmt.Errorf("provideraccounts: credential lock %s is not a directory", path)
		case time.Since(info.ModTime()) > stale:
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				return fmt.Errorf("provideraccounts: reclaim abandoned credential lock %s: %w", path, rmErr)
			}
		default:
			time.Sleep(claudeRefreshLockPoll)
		}
	}
}
