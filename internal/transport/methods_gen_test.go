package transport

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	cmd := exec.Command("go", "run", "./internal/transport/methodgen",
		"-out", tempOut,
		"-root", repoRoot,
	)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run methodgen: %v", err)
	}

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

// TestLocalOnlyMethods_AllExist guards the LocalOnlyMethods set against
// silent decay. Every name in LocalOnlyMethods MUST correspond to a
// real method in GeneratedMethods — a typo would otherwise let a
// LAN-attached caller invoke the privileged method with no enforcement
// at all (the dispatcher would never find a name match, so the LAN-
// only refusal branch wouldn't fire either).
//
// Failure here means LocalOnlyMethods drifted: rename the entry to
// match the App method, or drop it if the App method has been removed.
func TestLocalOnlyMethods_AllExist(t *testing.T) {
	known := make(map[string]bool, len(GeneratedMethods))
	for _, m := range GeneratedMethods {
		known[m.Name] = true
	}
	for name := range LocalOnlyMethods {
		if !known[name] {
			t.Errorf("LocalOnlyMethods[%q] does not match any entry in GeneratedMethods — typo or stale entry", name)
		}
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
