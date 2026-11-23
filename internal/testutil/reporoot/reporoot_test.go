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

func TestFindStopsAtTheNearestGoMod(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "module")
	start := filepath.Join(inner, "internal", "pkg")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{outer, inner} {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := find(start); err != nil || got != inner {
		t.Fatalf("find(%s) = %s, %v; want %s", start, got, err, inner)
	}
}

func TestFindFailsWithNoGoModAbove(t *testing.T) {
	volumeRoot := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if dir, err := find(volumeRoot); err == nil {
		t.Fatalf("find(%s) = %s, want an error naming the missing go.mod", volumeRoot, dir)
	}
}
