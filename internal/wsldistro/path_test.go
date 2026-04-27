//go:build !windows

package wsldistro

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWSLConfigDir_EmptyEnvReturnsFalse(t *testing.T) {
	t.Setenv(AppDataEnv, "")
	dir, ok := WSLConfigDir()
	if ok {
		t.Errorf("ok = true, want false; dir=%q", dir)
	}
	if dir != "" {
		t.Errorf("dir = %q, want \"\"", dir)
	}
}

func TestWSLConfigDir_NonExistentPathReturnsFalse(t *testing.T) {
	t.Setenv(AppDataEnv, "/this/path/should/not/exist/anywhere")
	if dir, ok := WSLConfigDir(); ok {
		t.Errorf("ok = true on missing dir; dir=%q", dir)
	}
}

func TestWSLConfigDir_ResolvesToAgentOverflowSubdir(t *testing.T) {
	parent := t.TempDir() // exists
	t.Setenv(AppDataEnv, parent)
	dir, ok := WSLConfigDir()
	if !ok {
		t.Fatal("ok = false; want true for resolvable env")
	}
	want := filepath.Join(parent, "agent-overflow")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

// TestWSLConfigDir_RejectsRelativePath pins the absolute-path
// requirement. A relative env value would resolve relative to the
// backend's CWD — that's a different trust surface from %APPDATA% and
// the launcher never produces it.
func TestWSLConfigDir_RejectsRelativePath(t *testing.T) {
	t.Setenv(AppDataEnv, "tmp/relative")
	if dir, ok := WSLConfigDir(); ok {
		t.Errorf("ok = true on relative path; dir=%q", dir)
	}
}

// TestWSLConfigDir_RejectsTraversal pins the no-".." rule. After
// filepath.Join, "/tmp/foo/../etc/agent-overflow" would collapse to
// "/etc/agent-overflow", which is escape-via-traversal — reject before
// the join.
func TestWSLConfigDir_RejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	bad := filepath.Join(parent, "..", "evil")
	t.Setenv(AppDataEnv, bad)
	if dir, ok := WSLConfigDir(); ok {
		t.Errorf("ok = true on traversal path %q; dir=%q", bad, dir)
	}
}

// TestWSLConfigDir_RejectsRegularFile pins the IsDir check. An env var
// pointing at a regular file (corrupted state, attacker-prepared
// artifact) must not be accepted — Save would otherwise try to create
// a child path under a non-directory and fail mid-write.
func TestWSLConfigDir_RejectsRegularFile(t *testing.T) {
	parent := t.TempDir()
	regular := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(regular, []byte("hello"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	t.Setenv(AppDataEnv, regular)
	if dir, ok := WSLConfigDir(); ok {
		t.Errorf("ok = true on regular-file env; dir=%q", dir)
	}
}
