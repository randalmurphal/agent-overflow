package provideraccounts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// lockTestCredentials builds a store whose user home is a temporary
// directory, with the Claude home already present so a fake CLI can take the
// lock before AO's first write.
func lockTestCredentials(t *testing.T) (*Credentials, string) {
	t.Helper()
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome, Policy{SignedOut: literalHuskDetector})
	if err != nil {
		t.Fatal(err)
	}
	claudeHome := filepath.Join(userHome, ".claude")
	if err := os.MkdirAll(claudeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return credentials, claudeHome
}

// takeLockLikeTheCLI creates the lock directory the way proper-lockfile does.
func takeLockLikeTheCLI(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

// lockPaths names the three CLI locks in acquisition order: the refresh lock,
// the legacy lock, then the secureStorage write lock the CLI takes innermost.
func lockPaths(t *testing.T, claudeHome string) (refresh string, legacy string, storage string) {
	t.Helper()
	locks := claudeCredentialLockPaths(claudeHome)
	if len(locks) != 3 {
		t.Fatalf("claudeCredentialLockPaths = %v, want the refresh, legacy and storage-write locks", locks)
	}
	if locks[0].stale != claudeRefreshLockStale || locks[1].stale != claudeRefreshLockStale {
		t.Fatalf("refresh locks carry stale %s/%s, want %s", locks[0].stale, locks[1].stale, claudeRefreshLockStale)
	}
	if locks[2].stale != claudeStorageWriteLockStale {
		t.Fatalf("storage-write lock carries stale %s, want the CLI's %s", locks[2].stale, claudeStorageWriteLockStale)
	}
	return locks[0].path, locks[1].path, locks[2].path
}

// The whole point of the lock: a canonical write must not land inside the
// CLI's read → POST → write section, because the CLI's compare-and-swap would
// then adopt AO's bytes and drop the rotation it already paid for.
func TestCanonicalCredentialWriteWaitsForTheClaudeRefreshLock(t *testing.T) {
	credentials, claudeHome := lockTestCredentials(t)
	refreshLock, _, _ := lockPaths(t, claudeHome)
	takeLockLikeTheCLI(t, refreshLock, 0)

	const held = 250 * time.Millisecond
	released := make(chan time.Time, 1)
	go func() {
		time.Sleep(held)
		released <- time.Now()
		_ = os.Remove(refreshLock)
	}()

	started := time.Now()
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("rotated")); err != nil {
		t.Fatalf("canonical write under a held lock: %v", err)
	}
	waited := time.Since(started)
	if waited < held {
		t.Fatalf("canonical write finished after %s, want it to wait for the lock (%s)", waited, held)
	}
	<-released

	data, err := credentials.ReadCredential("claude", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "rotated" {
		t.Fatalf("canonical credential = %s, want the write to have landed after the lock cleared", data)
	}
	assertLocksReleased(t, claudeHome)
}

// A CLI that died holding the lock must not brick every later write. The
// reclaim rule is the provider's own: a lock whose mtime has not moved within
// the stale threshold belongs to nobody.
func TestCanonicalCredentialWriteReclaimsAnAbandonedLock(t *testing.T) {
	credentials, claudeHome := lockTestCredentials(t)
	refreshLock, legacyLock, storageLock := lockPaths(t, claudeHome)
	takeLockLikeTheCLI(t, refreshLock, claudeRefreshLockStale+time.Minute)
	takeLockLikeTheCLI(t, legacyLock, claudeRefreshLockStale+time.Minute)
	takeLockLikeTheCLI(t, storageLock, claudeStorageWriteLockStale+time.Minute)

	started := time.Now()
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("rotated")); err != nil {
		t.Fatalf("canonical write against abandoned locks: %v", err)
	}
	if waited := time.Since(started); waited > claudeRefreshLockWait/2 {
		t.Fatalf("canonical write waited %s on abandoned locks, want an immediate reclaim", waited)
	}
	assertLocksReleased(t, claudeHome)
}

// A lock that stays held is a refusal, not a forced write: overwriting inside
// a refresh window retires a single-use token with no replacement saved, and
// that is unrecoverable. Exercised on the acquisition helper so the test does
// not have to sit out the production budget.
func TestLockAcquisitionGivesUpWhileTheLockIsHeld(t *testing.T) {
	_, claudeHome := lockTestCredentials(t)
	refreshLock, _, _ := lockPaths(t, claudeHome)
	takeLockLikeTheCLI(t, refreshLock, 0)

	err := acquireLockDirectory(refreshLock, 150*time.Millisecond, claudeRefreshLockStale)
	if !errors.Is(err, ErrCredentialLockBusy) {
		t.Fatalf("acquireLockDirectory on a held lock = %v, want ErrCredentialLockBusy", err)
	}
	if _, statErr := os.Lstat(refreshLock); statErr != nil {
		t.Fatalf("the holder's lock was disturbed by a failed acquisition: %v", statErr)
	}
}

// Every exit path releases. A lock AO leaves behind is a lock the CLI waits
// the full stale threshold on before it can refresh at all.
func TestCanonicalCredentialWriteReleasesTheLockOnEveryErrorPath(t *testing.T) {
	credentials, claudeHome := lockTestCredentials(t)

	// Refused inside the locked section: the size checks live in the write
	// itself, past all three acquisitions.
	if err := credentials.WriteNativeCredentialForTest("claude", nil); err == nil {
		t.Fatal("an empty canonical write succeeded, want a refusal")
	}
	assertLocksReleased(t, claudeHome)

	if err := credentials.WriteNativeCredentialForTest(
		"claude",
		make([]byte, maxCredentialBytes+1),
	); err == nil {
		t.Fatal("an oversize canonical write succeeded, want a refusal")
	}
	assertLocksReleased(t, claudeHome)

	// Refused BETWEEN acquisitions: the legacy lock path is occupied
	// by a regular file, so the first lock must still come back off.
	_, legacyLock, storageLock := lockPaths(t, claudeHome)
	if err := os.WriteFile(legacyLock, []byte("not a lock directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("rotated")); err == nil {
		t.Fatal("a canonical write succeeded with an unusable legacy lock, want a refusal")
	}
	refreshLock, _, _ := lockPaths(t, claudeHome)
	if _, err := os.Lstat(refreshLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refresh lock survived a failed legacy acquisition (%v)", err)
	}
	if err := os.Remove(legacyLock); err != nil {
		t.Fatal(err)
	}

	// Refused on the INNERMOST acquisition: both refresh locks are already
	// held by AO itself at that point, so the unwind has to give them back.
	if err := os.WriteFile(storageLock, []byte("not a lock directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("rotated")); err == nil {
		t.Fatal("a canonical write succeeded with an unusable storage-write lock, want a refusal")
	}
	for _, path := range []string{refreshLock, legacyLock} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived a failed storage-write acquisition (%v)", path, err)
		}
	}
	if err := os.Remove(storageLock); err != nil {
		t.Fatal(err)
	}

	// Signing the provider out is a write to the same file under the same
	// locks.
	if err := credentials.RemoveActive("claude"); err != nil {
		t.Fatalf("RemoveActive: %v", err)
	}
	assertLocksReleased(t, claudeHome)
}

// The secureStorage lock is the one the CLI holds for a sign-in or a sign-out,
// not only for a refresh, so a write that ignored it would collide with the
// credential read-modify-write of a `claude /login` running beside AO. It is
// the innermost of the three: AO holds the two refresh locks while it waits.
func TestCanonicalCredentialWriteWaitsForTheStorageWriteLock(t *testing.T) {
	credentials, claudeHome := lockTestCredentials(t)
	refreshLock, legacyLock, storageLock := lockPaths(t, claudeHome)
	takeLockLikeTheCLI(t, storageLock, 0)

	const held = 250 * time.Millisecond
	nested := make(chan bool, 1)
	go func() {
		time.Sleep(held)
		outer := true
		for _, path := range []string{refreshLock, legacyLock} {
			if _, err := os.Lstat(path); err != nil {
				outer = false
			}
		}
		nested <- outer
		_ = os.Remove(storageLock)
	}()

	started := time.Now()
	if err := credentials.WriteNativeCredentialForTest("claude", []byte("rotated")); err != nil {
		t.Fatalf("canonical write under a held storage-write lock: %v", err)
	}
	if waited := time.Since(started); waited < held {
		t.Fatalf("canonical write finished after %s, want it to wait for the lock (%s)", waited, held)
	}
	if !<-nested {
		t.Fatal("the refresh locks were not held while AO waited on the storage-write lock, want the CLI's nesting")
	}

	data, err := credentials.ReadCredential("claude", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "rotated" {
		t.Fatalf("canonical credential = %s, want the write to have landed after the lock cleared", data)
	}
	assertLocksReleased(t, claudeHome)
}

// The CLI configures the storage-write lock with a 15s stale threshold and the
// refresh locks with 60s. Judging one by the other's number either reclaims a
// lock a live CLI still holds or waits out the budget on one nobody holds.
func TestEachLockKeepsItsOwnStaleThreshold(t *testing.T) {
	_, claudeHome := lockTestCredentials(t)
	_, _, storageLock := lockPaths(t, claudeHome)
	age := claudeStorageWriteLockStale + 5*time.Second
	if age >= claudeRefreshLockStale {
		t.Fatalf("stale thresholds no longer differ: storage %s, refresh %s", claudeStorageWriteLockStale, claudeRefreshLockStale)
	}
	takeLockLikeTheCLI(t, storageLock, age)

	// Judged by the refresh threshold this lock is still alive, so acquisition
	// has to give up rather than steal it.
	if err := acquireLockDirectory(storageLock, 150*time.Millisecond, claudeRefreshLockStale); !errors.Is(err, ErrCredentialLockBusy) {
		t.Fatalf("acquireLockDirectory with the refresh threshold = %v, want ErrCredentialLockBusy", err)
	}

	// Judged by its own, it is abandoned and reclaimable.
	if err := acquireLockDirectory(storageLock, claudeRefreshLockWait, claudeStorageWriteLockStale); err != nil {
		t.Fatalf("acquireLockDirectory with the storage-write threshold: %v", err)
	}
	if err := os.Remove(storageLock); err != nil {
		t.Fatal(err)
	}
}

// The locks are Claude's. Codex has no refresh lock of its own, and inventing
// one in its home would leave directories nothing ever reads.
func TestCodexCredentialWritesTakeNoClaudeLock(t *testing.T) {
	credentials, _ := lockTestCredentials(t)
	if err := credentials.WriteNativeCredentialForTest("codex", []byte("codex-token")); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(credentials.userHome, ".codex")
	entries, err := os.ReadDir(codexHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".lock" {
			t.Fatalf("codex home holds %s, want no lock directories", entry.Name())
		}
	}
}

func assertLocksReleased(t *testing.T, claudeHome string) {
	t.Helper()
	for _, lock := range claudeCredentialLockPaths(claudeHome) {
		if _, err := os.Lstat(lock.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("credential lock %s was left behind (%v)", lock.path, err)
		}
	}
}
