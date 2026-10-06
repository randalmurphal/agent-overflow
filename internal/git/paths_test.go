package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalPathResolvesSymlinks(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	dir := t.TempDir()
	dirty := filepath.Join(dir, "a", "..", "b", ".")
	got := CanonicalPath(dirty)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	want := filepath.Join(resolved, "b")
	if got != want {
		t.Fatalf("CanonicalPath(%q) = %q, want %q", dirty, got, want)
	}
}

func TestCanonicalPathResolvesMissingPathThroughExistingAncestor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nonexistent := filepath.Join(dir, "does", "not", "..", "not", "exist")
	got := CanonicalPath(nonexistent)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	want := filepath.Join(resolved, "does", "not", "exist")
	if got != want {
		t.Fatalf("CanonicalPath(nonexistent) = %q, want %q", got, want)
	}
}

// A directory deleted below a symlinked parent still compares equal to its
// other spelling: git records the resolved path, a thread row may hold the
// link's.
func TestSameFilesystemPathForDeletedDirectoryBelowSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "worktree"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	viaLink := filepath.Join(link, "worktree")
	viaReal := filepath.Join(real, "worktree")
	if err := os.Remove(viaReal); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if !SameFilesystemPath(viaLink, viaReal) {
		t.Fatalf("SameFilesystemPath(%q, %q) = false after deletion, want true", viaLink, viaReal)
	}
	if SameFilesystemPath(viaLink, filepath.Join(real, "other")) {
		t.Fatalf("SameFilesystemPath matched a different deleted directory")
	}
}

func TestCanonicalPathOfEmptyAndRelativePaths(t *testing.T) {
	t.Parallel()
	if got := CanonicalPath(""); got != "." {
		t.Fatalf("CanonicalPath(\"\") = %q, want \".\"", got)
	}
	missing := filepath.Join("does-not-exist-here", "child")
	if got := CanonicalPath(missing); got != missing {
		t.Fatalf("CanonicalPath(%q) = %q, want it unchanged", missing, got)
	}
}

func TestSameFilesystemPathThroughSymlink(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	dir := t.TempDir()
	if !SameFilesystemPath(dir, dir) {
		t.Fatalf("SameFilesystemPath(%q, %q) = false, want true", dir, dir)
	}
}
