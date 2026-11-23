package reporoot

import (
	"go/token"
	"slices"
	"strings"
	"testing"
)

func TestGoFilesListsEveryPackageRoot(t *testing.T) {
	files := GoFiles(t)
	for _, want := range []string{"main.go", "cmd/ao-gotest/main.go", "internal/testutil/reporoot/files.go", "internal/testutil/reporoot/files_test.go"} {
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
	got := Files(t, "internal/testutil/reporoot", "files*.go")
	if want := []string{"internal/testutil/reporoot/files.go", "internal/testutil/reporoot/files_test.go"}; !slices.Equal(got, want) {
		t.Fatalf("Files = %v, want %v", got, want)
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
