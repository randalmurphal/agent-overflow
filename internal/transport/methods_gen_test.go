package transport

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMethodsGen_InSync regenerates methods_gen.go into a tempfile and
// asserts the bytes match the committed file. A developer who adds an
// App method without running `go run ./internal/transport/methodgen`
// fails this test, and the failure message points to the fix.
//
// Skipped on Windows in CI because the methodgen tool reads the repo
// root and the relative-path math depends on POSIX-y filesystem layout.
// The CI matrix runs the test on Linux, which is sufficient.
func TestMethodsGen_InSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("methodgen integrity test runs on POSIX CI")
	}

	repoRoot := findRepoRoot(t)

	tempDir := t.TempDir()
	tempOut := filepath.Join(tempDir, "methods_gen.go")
	tempInputs := filepath.Join(tempDir, "inputs.txt")

	cmd := exec.Command("go", "run", "./internal/transport/methodgen",
		"-out", tempOut,
		"-root", repoRoot,
		"-inputs", tempInputs,
	)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run methodgen: %v", err)
	}

	observeGeneratorInputs(t, tempInputs)

	want, err := os.ReadFile(tempOut)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, "internal/transport/methods_gen.go"))
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}

	if !bytes.Equal(want, got) {
		t.Fatalf("methods_gen.go is out of sync with App methods.\n" +
			"Run `go run ./internal/transport/methodgen` and commit the result.")
	}
}

// observeGeneratorInputs opens, and immediately closes, every path the
// generator read. Nothing is done with the contents; the OPEN is the
// point.
//
// `go test` keys its result cache on the files the test PROCESS opens, and
// everything this test actually inspects lives in package transport. The
// source that decides the answer — internal/app — is opened by a
// subprocess, which the cache cannot see. Without these reads the gate
// reports a cached PASS over an internal/app it never looked at, so a
// newly exported App method stays undeclared through a green `make
// go-test` (that is exactly how RedeemPairing and RenewSession slipped
// past a full gate run on 2026-08-30). Reading the manifest's files puts
// them in the cache key.
//
// Directories are in the manifest alongside their files, and both halves
// are load-bearing: Go hashes a file by content and a directory by its
// entry list, so files alone would still miss a method declared in a file
// that did not exist on the cached run.
func observeGeneratorInputs(t *testing.T, manifest string) {
	t.Helper()
	listing, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read generator input manifest: %v", err)
	}
	paths := strings.Split(strings.TrimSpace(string(listing)), "\n")
	if len(paths) < 4 {
		t.Fatalf("the generator named %d inputs; it reads at least the skip list, the scope vocabulary, "+
			"one receiver directory, and its files", len(paths))
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open generator input %s: %v", path, err)
		}
		_ = f.Close()
	}
}

// findRepoRoot walks up from the test binary's location until it
// finds go.mod. Tests run from internal/transport/, so we expect to
// find go.mod two levels up.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no go.mod above test cwd)")
	return ""
}
