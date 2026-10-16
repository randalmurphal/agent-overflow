package agenthome

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The cache reads a home's instructions once while the file stands, reads
// it again when it changes, and keeps nothing for a file that is gone or
// cannot be read.
func TestRoleCacheReadsOnlyAChangedFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	file := filepath.Join(home, InstructionsFile)
	write := func(text string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Unix(1_700_000_000, 0)
	// The clock is well past every write, so no entry is racy.
	cache := RoleCache{now: func() time.Time { return at.Add(time.Hour) }}
	role := func(want string) {
		t.Helper()
		got, err := cache.Role(home)
		if err != nil || got != want {
			t.Fatalf("Role = %q, %v; want %q", got, err, want)
		}
	}
	write("Reviewer of the build\n", at)
	for range 3 {
		role("Reviewer of the build")
	}
	if reads := cache.reads.Load(); reads != 1 {
		t.Fatalf("read the file %d times for three calls, want 1", reads)
	}
	// Same size, later time: read again.
	write("Releaser of the build\n", at.Add(time.Second))
	role("Releaser of the build")
	// Same time, other size: read again.
	write("Releaser\n", at.Add(time.Second))
	role("Releaser")
	if reads := cache.reads.Load(); reads != 3 {
		t.Fatalf("read the file %d times, want once per change", reads)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	role("")
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Role(home); err == nil {
		t.Fatal("a directory in place of the instructions was read as a role")
	}
	cache.mu.Lock()
	_, kept := cache.entries[home]
	cache.mu.Unlock()
	if kept {
		t.Fatal("an unreadable file left its old role cached")
	}
}

// A file written within roleRacyWindow of the read is read again on every
// call: a second write of the same length in the same tick leaves the same
// key. Once the clock is past the window, one read caches it.
func TestRoleCacheRereadsARacilyCleanFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	file := filepath.Join(home, InstructionsFile)
	at := time.Unix(1_700_000_000, 0)
	clock := at
	cache := RoleCache{now: func() time.Time { return clock }}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	role := func(want string) {
		t.Helper()
		got, err := cache.Role(home)
		if err != nil || got != want {
			t.Fatalf("Role = %q, %v; want %q", got, err, want)
		}
	}
	write("Reviewer\n")
	role("Reviewer")
	// Same length, same time: only the racy rule tells them apart.
	write("Releaser\n")
	role("Releaser")
	clock = at.Add(roleRacyWindow + time.Second)
	role("Releaser")
	before := cache.reads.Load()
	role("Releaser")
	role("Releaser")
	if reads := cache.reads.Load() - before; reads != 0 {
		t.Fatalf("read a file past the window %d more times, want it cached", reads)
	}
}

// Forget drops a home's entry.
func TestRoleCacheForgetsADeletedHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, InstructionsFile), []byte("Reviewer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cache RoleCache
	if _, err := cache.Role(home); err != nil {
		t.Fatal(err)
	}
	cache.Forget(home)
	cache.mu.Lock()
	_, kept := cache.entries[home]
	cache.mu.Unlock()
	if kept {
		t.Fatal("a forgotten home kept its entry")
	}
}
