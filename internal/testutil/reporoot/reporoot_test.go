package reporoot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirIsTheCheckoutEvenAfterTheTestChangesDirectory(t *testing.T) {
	want, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	if got := Dir(t); got != want {
		t.Fatalf("Dir = %s, want %s", got, want)
	}
	self := Path(t, "internal/testutil/reporoot/reporoot.go")
	if _, err := os.Stat(self); err != nil {
		t.Fatalf("Path does not reach this package's source: %v", err)
	}
}

func TestFindFailsOutsideACheckout(t *testing.T) {
	outside := t.TempDir()
	t.Chdir(outside)

	if dir, err := find(); err == nil {
		t.Fatalf("find from %s = %s, want an error naming the missing go.mod", outside, dir)
	}
}
