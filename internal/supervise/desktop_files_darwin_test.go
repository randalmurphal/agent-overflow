//go:build darwin

package supervise

import (
	"os"
	"path/filepath"
	"testing"
)

// A staged or set-aside version is a bundle or a bare executable. Either is
// kept while a process has it, or a file in it, open, and only then.
func TestLsofInUseJudgesBundlesAndExecutables(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	executable := filepath.Join(dir, ".agent-overflow-update-0123456789abcdef")
	if err := os.WriteFile(executable, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "Agent Overflow.app")
	inBundle := filepath.Join(bundle, "Contents", "MacOS", "agent-overflow")
	if err := os.MkdirAll(filepath.Dir(inBundle), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inBundle, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, path, open string }{
		{"executable", executable, executable},
		{"bundle", bundle, inBundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if inUse, err := lsofInUse(tc.path); err != nil || inUse {
				t.Fatalf("an unused %s: lsofInUse = %v, %v; want false", tc.name, inUse, err)
			}
			file, err := os.Open(tc.open)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if inUse, err := lsofInUse(tc.path); err != nil || !inUse {
				t.Fatalf("an open %s: lsofInUse = %v, %v; want true", tc.name, inUse, err)
			}
		})
	}

	if _, err := lsofInUse(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing path was judged without an error")
	}
}
