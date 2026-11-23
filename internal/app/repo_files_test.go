package app

import (
	"os"
	"path"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// A test reads a file outside this package by its path from the checkout's
// root, through reporoot. go test starts the binary in the package directory
// and the test cache keys a pass on the files the test opens there, so a
// path resolved against any other directory leaves a later edit of the file
// out of the key, and the cache answers with a pass recorded before it.

// The binary runs in the package directory, as go test starts it: TestMain
// does not move it, so the test cache records each file a test opens as
// the file it is.
func TestTheTestsRunInThePackageDirectory(t *testing.T) {
	t.Parallel()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := reporoot.Path(t, "internal/app"); dir != want {
		t.Fatalf("the tests run in %s, want the package directory %s", dir, want)
	}
}

// wireCapture reads a provider wire capture committed under
// internal/provider/wirecaptures, named by its provider directory and file name
// (claude/x.ndjson).
func wireCapture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(reporoot.Path(t, path.Join("internal/provider/wirecaptures", name)))
	if err != nil {
		t.Fatalf("read the %s wire capture: %v", name, err)
	}
	return data
}
