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

func TestPackageRootsAreTheMakefilesDeclaration(t *testing.T) {
	for _, tc := range []struct {
		makefile string
		want     []string
		fails    bool
	}{
		{"X := 1\nGO_PACKAGE_ROOTS := . ./cmd/... ./internal/...\n", []string{"cmd", "internal"}, false},
		{"GO_PACKAGE_ROOTS := . ./internal/app/...\n", []string{"internal/app"}, false},
		{"GO_PACKAGE_ROOTS := ./cmd/...\n", nil, true},
		{"GO_PACKAGE_ROOTS := . ./cmd\n", nil, true},
		{"GO_PACKAGE_ROOTS := . ../x/...\n", nil, true},
		{"GO_PACKAGE_ROOTS := . ./$(DIR)/...\n", nil, true},
		{"GO_PACKAGE_ROOTS ?= . ./cmd/...\n", nil, true},
	} {
		got, err := packageRoots([]byte(tc.makefile))
		if (err != nil) != tc.fails || !slices.Equal(got, tc.want) {
			t.Errorf("packageRoots(%q) = %v, %v; want %v, failure %v", tc.makefile, got, err, tc.want, tc.fails)
		}
	}
}
