package reporoot

import (
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestGoFilesListsEveryPackageRoot(t *testing.T) {
	files := GoFiles(t)
	for _, want := range []string{"main.go", "cmd/ao-gotest/main.go", "internal/testutil/reporoot/reporoot.go", "internal/testutil/reporoot/reporoot_test.go"} {
		if !slices.Contains(files, want) {
			t.Errorf("GoFiles lacks %s", want)
		}
	}
	for _, file := range files {
		if strings.Contains("/"+file, "/.") {
			t.Errorf("GoFiles lists %s, under a dot-directory", file)
		}
	}
}

func TestFilesMatchesNamesInOneDirectory(t *testing.T) {
	got := Files(t, "internal/testutil/reporoot", "*_test.go")
	if !slices.Equal(got, []string{"internal/testutil/reporoot/reporoot_test.go"}) {
		t.Fatalf("Files = %v, want this test file alone", got)
	}
	fset := token.NewFileSet()
	file, err := ParseGoFile(t, fset, got[0], 0)
	if err != nil {
		t.Fatal(err)
	}
	if name := fset.Position(file.Package).Filename; name != got[0] {
		t.Fatalf("positions name %s, want %s", name, got[0])
	}
}
