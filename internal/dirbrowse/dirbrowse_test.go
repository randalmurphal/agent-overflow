package dirbrowse

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// seedEntry describes one filesystem artefact for seedDir. It
// deliberately covers only what the Browse tests need: plain dirs,
// plain files, and symlinks. name is always a basename (no path
// separators).
type seedEntry struct {
	name      string
	isDir     bool
	isSymlink bool
	target    string // for symlinks: absolute or relative target path
	content   string // for files: body written verbatim
}

// seedDir creates root if needed and materialises each entry under it.
// Symlinks whose target is relative are evaluated relative to root so
// tests can point at sibling entries without knowing the temp path.
func seedDir(t *testing.T, root string, entries []seedEntry) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root %s: %v", root, err)
	}
	for _, e := range entries {
		path := filepath.Join(root, e.name)
		switch {
		case e.isSymlink:
			target := e.target
			if !filepath.IsAbs(target) {
				target = filepath.Join(root, target)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("symlink %s -> %s: %v", path, target, err)
			}
		case e.isDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
		default:
			if err := os.WriteFile(path, []byte(e.content), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}
}

// findEntry returns a pointer to the first entry matching name, or
// nil. Tests use it as `if got := findEntry(listing.Entries, "x");
// got == nil {...}`.
func findEntry(entries []Entry, name string) *Entry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

func TestBrowseHappyPath(t *testing.T) {
	root := t.TempDir()
	seedDir(t, root, []seedEntry{
		{name: "zeta", isDir: true},
		{name: "alpha", isDir: true},
		{name: "readme.md", content: "hi"},
		{name: ".hiddenfile", content: "secret"},
	})

	listing, err := Browse(root)
	if err != nil {
		t.Fatalf("Browse() error = %v, want nil", err)
	}

	if listing.Path != filepath.Clean(root) {
		t.Errorf("Path = %q, want %q", listing.Path, filepath.Clean(root))
	}
	if listing.Truncated {
		t.Errorf("Truncated = true, want false")
	}
	if !listing.Exists {
		t.Errorf("Exists = false, want true for a real directory")
	}

	// Expected order: dirs first (alphabetical) then files (alphabetical).
	wantOrder := []string{"alpha", "zeta", ".hiddenfile", "readme.md"}
	if len(listing.Entries) != len(wantOrder) {
		t.Fatalf("len(Entries) = %d, want %d (got: %+v)", len(listing.Entries), len(wantOrder), listing.Entries)
	}
	for i, name := range wantOrder {
		if listing.Entries[i].Name != name {
			t.Errorf("Entries[%d].Name = %q, want %q", i, listing.Entries[i].Name, name)
		}
	}

	if got := findEntry(listing.Entries, "alpha"); got == nil || !got.IsDir {
		t.Errorf("expected alpha IsDir=true, got %+v", got)
	}
	if got := findEntry(listing.Entries, "readme.md"); got == nil || got.IsDir {
		t.Errorf("expected readme.md IsDir=false, got %+v", got)
	}
	if got := findEntry(listing.Entries, ".hiddenfile"); got == nil || !got.Hidden {
		t.Errorf("expected .hiddenfile Hidden=true, got %+v", got)
	}
	if got := findEntry(listing.Entries, "readme.md"); got != nil && got.Hidden {
		t.Errorf("expected readme.md Hidden=false, got %+v", got)
	}
}

func TestBrowseEmptyDirectory(t *testing.T) {
	root := t.TempDir()

	listing, err := Browse(root)
	if err != nil {
		t.Fatalf("Browse() error = %v, want nil", err)
	}
	if len(listing.Entries) != 0 {
		t.Errorf("len(Entries) = %d, want 0", len(listing.Entries))
	}
	if listing.Truncated {
		t.Errorf("Truncated = true, want false")
	}
}

func TestBrowseNonExistentPath(t *testing.T) {
	// A typed-but-not-yet-valid path is an expected UI state (the modal
	// calls Browse on every keystroke). It must return a zero listing
	// with Exists=false and no error — logging a server ERR for every
	// incomplete keystroke would flood the logs.
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	listing, err := Browse(missing)
	if err != nil {
		t.Fatalf("Browse(missing) error = %v, want nil", err)
	}
	if listing.Exists {
		t.Errorf("Exists = true, want false for a missing path")
	}
	if len(listing.Entries) != 0 {
		t.Errorf("len(Entries) = %d, want 0", len(listing.Entries))
	}
	if listing.Path != filepath.Clean(missing) {
		t.Errorf("Path = %q, want %q (cleaned absolute path echoed back)", listing.Path, filepath.Clean(missing))
	}
}

func TestBrowsePathIsFile(t *testing.T) {
	// Same contract as missing-path: the UI treats "path points at a
	// file" as an empty-listing state, not a hard error.
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	listing, err := Browse(file)
	if err != nil {
		t.Fatalf("Browse(file) error = %v, want nil", err)
	}
	if listing.Exists {
		t.Errorf("Exists = true, want false for a file path")
	}
	if len(listing.Entries) != 0 {
		t.Errorf("len(Entries) = %d, want 0", len(listing.Entries))
	}
}

func TestBrowseDetectsRepo(t *testing.T) {
	root := t.TempDir()
	// Repo with `.git` as a directory (normal checkout).
	repoDir := filepath.Join(root, "repo-dir")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo-dir/.git: %v", err)
	}
	// Repo with `.git` as a file (git worktree pattern).
	repoFile := filepath.Join(root, "repo-file")
	if err := os.MkdirAll(repoFile, 0o755); err != nil {
		t.Fatalf("mkdir repo-file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoFile, ".git"), []byte("gitdir: ../.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("write repo-file/.git: %v", err)
	}
	// A regular sibling dir with no .git.
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatalf("mkdir plain: %v", err)
	}
	// A file in root — files must always report IsRepo=false.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	listing, err := Browse(root)
	if err != nil {
		t.Fatalf("Browse() error = %v", err)
	}

	for _, name := range []string{"repo-dir", "repo-file"} {
		got := findEntry(listing.Entries, name)
		if got == nil {
			t.Fatalf("missing entry %q", name)
		}
		if !got.IsRepo {
			t.Errorf("%s IsRepo = false, want true", name)
		}
	}
	if got := findEntry(listing.Entries, "plain"); got == nil || got.IsRepo {
		t.Errorf("plain IsRepo = true, want false (got %+v)", got)
	}
	if got := findEntry(listing.Entries, "notes.txt"); got == nil || got.IsRepo {
		t.Errorf("notes.txt IsRepo = true, want false (files never report IsRepo)")
	}
}

func TestBrowseTruncation(t *testing.T) {
	root := t.TempDir()
	total := EntryLimit + 1 // 501 — enough to trigger the cap.
	for i := 0; i < total; i++ {
		// Zero-pad so ReadDir's alphabetical scan ordering is stable
		// and the test doesn't depend on iteration order.
		name := filepath.Join(root, fmt.Sprintf("%04d", i))
		if err := os.MkdirAll(name, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}

	listing, err := Browse(root)
	if err != nil {
		t.Fatalf("Browse() error = %v", err)
	}
	if len(listing.Entries) != EntryLimit {
		t.Errorf("len(Entries) = %d, want %d", len(listing.Entries), EntryLimit)
	}
	if !listing.Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestBrowseHomeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	wantPath := filepath.Clean(home)

	for _, input := range []string{"", "~", "~/"} {
		t.Run("input="+input, func(t *testing.T) {
			listing, err := Browse(input)
			if err != nil {
				t.Fatalf("Browse(%q) error = %v", input, err)
			}
			if listing.Path != wantPath {
				t.Errorf("Path = %q, want %q", listing.Path, wantPath)
			}
		})
	}
}

func TestBrowseParentComputation(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	listing, err := Browse(sub)
	if err != nil {
		t.Fatalf("Browse(sub) error = %v", err)
	}
	wantParent := filepath.Clean(root)
	if listing.Parent != wantParent {
		t.Errorf("Parent = %q, want %q", listing.Parent, wantParent)
	}

	// Filesystem-root check. On unix this is `/`; on Windows we'd need
	// to find a drive root, which varies across CI setups — skip there.
	if runtime.GOOS == "windows" {
		t.Skip("filesystem-root parent check skipped on windows (drive root varies)")
	}
	rootListing, err := Browse("/")
	if err != nil {
		t.Fatalf("Browse(/) error = %v", err)
	}
	if rootListing.Parent != "" {
		t.Errorf("Parent at / = %q, want \"\"", rootListing.Parent)
	}
}

func TestBrowseSeparator(t *testing.T) {
	root := t.TempDir()
	listing, err := Browse(root)
	if err != nil {
		t.Fatalf("Browse() error = %v", err)
	}
	want := string(os.PathSeparator)
	if listing.Separator != want {
		t.Errorf("Separator = %q, want %q", listing.Separator, want)
	}
}

func TestBrowseSymlinkIsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Creating symlinks on Windows without elevated privilege fails
		// on most shells; symlink semantics are already covered on
		// unix and the non-symlink paths stay exercised on Windows CI.
		t.Skip("symlink creation requires elevated shell on windows")
	}
	root := t.TempDir()
	seedDir(t, root, []seedEntry{
		{name: "real", isDir: true},
		{name: "link", isSymlink: true, target: "real"},
	})

	listing, err := Browse(root)
	if err != nil {
		t.Fatalf("Browse() error = %v", err)
	}
	got := findEntry(listing.Entries, "link")
	if got == nil {
		t.Fatalf("missing symlink entry 'link'")
	}
	if !got.IsDir {
		t.Errorf("link IsDir = false, want true (symlink should be stat'd, not lstat'd)")
	}
}
