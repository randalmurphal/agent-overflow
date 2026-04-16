package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalPathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := CanonicalPath(link)
	want := CanonicalPath(real)
	if got != want {
		t.Fatalf("CanonicalPath(symlink) = %q, want %q", got, want)
	}
}

func TestCanonicalPathCleansRedundantSegments(t *testing.T) {
	dir := t.TempDir()
	dirty := filepath.Join(dir, "a", "..", "b", ".")
	got := CanonicalPath(dirty)
	want := filepath.Join(dir, "b")
	if got != want {
		t.Fatalf("CanonicalPath(%q) = %q, want %q", dirty, got, want)
	}
}

func TestCanonicalPathFallsBackOnMissingPath(t *testing.T) {
	// A non-existent path cannot be resolved through EvalSymlinks, so
	// CanonicalPath should fall back to filepath.Clean.
	nonexistent := filepath.Join(t.TempDir(), "does", "not", "exist")
	got := CanonicalPath(nonexistent)
	want := filepath.Clean(nonexistent)
	if got != want {
		t.Fatalf("CanonicalPath(nonexistent) = %q, want %q", got, want)
	}
}

func TestSameFilesystemPathThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if !SameFilesystemPath(real, link) {
		t.Fatalf("SameFilesystemPath(%q, %q) = false, want true", real, link)
	}
}

func TestSameFilesystemPathDifferentPaths(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.Mkdir(b, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}

	if SameFilesystemPath(a, b) {
		t.Fatalf("SameFilesystemPath(%q, %q) = true, want false", a, b)
	}
}

func TestSameFilesystemPathIdenticalPath(t *testing.T) {
	dir := t.TempDir()
	if !SameFilesystemPath(dir, dir) {
		t.Fatalf("SameFilesystemPath(%q, %q) = false, want true", dir, dir)
	}
}
