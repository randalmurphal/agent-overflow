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
	var cache RoleCache
	role := func(want string) {
		t.Helper()
		got, err := cache.Role(home)
		if err != nil || got != want {
			t.Fatalf("Role = %q, %v; want %q", got, err, want)
		}
	}
	at := time.Unix(1_700_000_000, 0)
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
