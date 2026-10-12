package deviceclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A release that cannot unlock or close reports it: the lock is the only
// thing standing between two processes writing one profile file, so a
// release failure is the caller's error, not a silent one.
func TestReleaseProfileReportsAFailedUnlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.lock")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = releaseProfile(f)
	if err == nil {
		t.Fatal("releasing a closed descriptor reported no error")
	}
	if !strings.Contains(err.Error(), "unlock "+path) || !strings.Contains(err.Error(), "close "+path) {
		t.Fatalf("error does not name both failures: %v", err)
	}
}
