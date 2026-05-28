package workspacefiles

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// nestedWorkspace writes a small file tree with three directory levels under
// root so prefix/substring/subsequence matchers have something to chew on.
func nestedWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"a/b/c/deep.go":          "deep",
		"a/b/middle.go":          "middle",
		"a/top.go":               "top",
		"src/components/App.tsx": "App",
		"src/utils/helper.ts":    "helper",
		"README.md":              "readme",
	}
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// TestIntegration_SearchFindsFilesInNestedDirs drops a file at depth 3 and
// asserts the walker reaches it.
func TestIntegration_SearchFindsFilesInNestedDirs(t *testing.T) {
	root := nestedWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "", 500)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	paths := map[string]bool{}
	for _, r := range results {
		paths[r.Path] = true
	}
	for _, expected := range []string{"a/b/c/deep.go", "a/b/middle.go", "a/top.go"} {
		if !paths[expected] {
			t.Fatalf("expected %q in results, got %v", expected, paths)
		}
	}
}

// TestIntegration_SearchPrefixMatchRanksHighest is the core ranking
// guarantee: a prefix match should beat a substring match, which in turn
// beats a subsequence match. The three crafted files each take one tier.
func TestIntegration_SearchPrefixMatchRanksHighest(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"foobar.go", "barfoo.go", "bfooz.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	searcher := NewSearcher(Config{})
	results, _, err := searcher.Search(root, "foo", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 3 {
		t.Fatalf("expected all three files back, got %d (%+v)", len(results), results)
	}
	if results[0].Path != "foobar.go" {
		t.Fatalf("first result should be foobar.go, got %q", results[0].Path)
	}
	// barfoo (substring) should beat bfooz (subsequence of "foo" through name).
	var idxBar, idxBfooz int = -1, -1
	for i, r := range results {
		if r.Path == "barfoo.go" {
			idxBar = i
		}
		if r.Path == "bfooz.go" {
			idxBfooz = i
		}
	}
	if idxBar < 0 || idxBfooz < 0 {
		t.Fatalf("expected both barfoo.go and bfooz.go in results: %+v", results)
	}
	if idxBar >= idxBfooz {
		t.Fatalf("barfoo.go (substring) should rank above bfooz.go (subsequence); got indices %d vs %d", idxBar, idxBfooz)
	}
}

// TestIntegration_SearchSubstringOverSubsequence isolates the substring vs
// subsequence rule. "xyz.go" contains the literal substring "xyz"; while
// "xaybzc.go" only matches as a subsequence. The former must rank first.
func TestIntegration_SearchSubstringOverSubsequence(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"xyz.go", "xaybzc.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	searcher := NewSearcher(Config{})
	results, _, err := searcher.Search(root, "xyz", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 matches, got %d", len(results))
	}
	if results[0].Path != "xyz.go" {
		t.Fatalf("substring match should rank first; got %q", results[0].Path)
	}
}

// TestIntegration_SearchCacheRespectsWorkspaceFilesystemChanges documents
// the 15s TTL contract. Within the TTL the cache is authoritative: a
// newly-added file will NOT appear in subsequent searches until the TTL
// expires or Invalidate is called. This test sets a small TTL, adds a file,
// verifies it's absent before TTL expiry, then verifies it appears after.
func TestIntegration_SearchCacheRespectsWorkspaceFilesystemChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stable.go"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stable: %v", err)
	}
	// Small TTL so the test is fast but non-zero so the first + second
	// search hit the same cache snapshot. 50ms is enough on any machine to
	// avoid racing the sleep below.
	searcher := NewSearcher(Config{TTL: 50 * time.Millisecond})

	// Prime the cache.
	if _, _, err := searcher.Search(root, "", 50); err != nil {
		t.Fatalf("prime Search: %v", err)
	}

	// Add a new file; it should be invisible while TTL is valid.
	if err := os.WriteFile(filepath.Join(root, "brand-new.go"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write brand-new: %v", err)
	}

	results, _, err := searcher.Search(root, "", 50)
	if err != nil {
		t.Fatalf("cached Search: %v", err)
	}
	seenInCache := false
	for _, r := range results {
		if r.Path == "brand-new.go" {
			seenInCache = true
			break
		}
	}
	if seenInCache {
		t.Fatal("expected brand-new.go to be hidden by TTL cache")
	}

	// Wait past the TTL so the next call rebuilds the index.
	time.Sleep(120 * time.Millisecond)

	results, _, err = searcher.Search(root, "", 50)
	if err != nil {
		t.Fatalf("post-TTL Search: %v", err)
	}
	seenAfterTTL := false
	for _, r := range results {
		if r.Path == "brand-new.go" {
			seenAfterTTL = true
			break
		}
	}
	if !seenAfterTTL {
		t.Fatalf("expected brand-new.go after TTL expiry; got %+v", results)
	}
}

// TestIntegration_SearchRespectsGitIgnore verifies that a git-backed workspace
// honours the user's .gitignore, while a non-git workspace falls back to the
// tight IgnoredDirs whitelist walk.
func TestIntegration_SearchRespectsGitIgnore(t *testing.T) {
	t.Run("git repo honours .gitignore", func(t *testing.T) {
		root := initGitRepoForWorkspacefiles(t)
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret/\nnode_modules/\n"), 0o644); err != nil {
			t.Fatalf("write .gitignore: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, "secret"), 0o755); err != nil {
			t.Fatalf("mkdir secret: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "secret", "token.txt"), []byte("shh"), 0o644); err != nil {
			t.Fatalf("write secret/token.txt: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, "node_modules", "fake"), 0o755); err != nil {
			t.Fatalf("mkdir node_modules: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "node_modules", "fake", "index.js"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write node_modules file: %v", err)
		}
		// A tracked file so the index is not empty.
		if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
			t.Fatalf("write README.md: %v", err)
		}

		searcher := NewSearcher(Config{})
		results, _, err := searcher.Search(root, "", 500)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		seenSecret, seenNodeModules, seenReadme := false, false, false
		for _, r := range results {
			if strings.HasPrefix(r.Path, "secret/") || r.Path == "secret" {
				seenSecret = true
			}
			if strings.HasPrefix(r.Path, "node_modules/") || r.Path == "node_modules" {
				seenNodeModules = true
			}
			if r.Path == "README.md" {
				seenReadme = true
			}
		}
		if seenSecret {
			t.Fatal("secret/ appeared despite being listed in .gitignore")
		}
		if seenNodeModules {
			t.Fatal("node_modules should always be skipped regardless of .gitignore")
		}
		if !seenReadme {
			t.Fatalf("README.md missing from index: %+v", results)
		}
	})

	t.Run("non-git workspace uses IgnoredDirs whitelist", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret/\n"), 0o644); err != nil {
			t.Fatalf("write .gitignore: %v", err)
		}
		// secret/ is in the .gitignore but NOT in IgnoredDirs; without git we
		// have no way to honour it, so it surfaces. node_modules still gets
		// filtered because it's in IgnoredDirs.
		if err := os.MkdirAll(filepath.Join(root, "secret"), 0o755); err != nil {
			t.Fatalf("mkdir secret: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "secret", "token.txt"), []byte("shh"), 0o644); err != nil {
			t.Fatalf("write secret/token.txt: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, "node_modules", "fake"), 0o755); err != nil {
			t.Fatalf("mkdir node_modules: %v", err)
		}

		searcher := NewSearcher(Config{})
		results, _, err := searcher.Search(root, "", 500)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		seenSecret, seenNodeModules := false, false
		for _, r := range results {
			if strings.HasPrefix(r.Path, "secret/") || r.Path == "secret" {
				seenSecret = true
			}
			if strings.HasPrefix(r.Path, "node_modules/") || r.Path == "node_modules" {
				seenNodeModules = true
			}
		}
		if !seenSecret {
			t.Fatal("non-git fallback should surface .gitignore-listed paths (they aren't in IgnoredDirs)")
		}
		if seenNodeModules {
			t.Fatal("node_modules should always be skipped (it is in IgnoredDirs)")
		}
	})
}

// initGitRepoForWorkspacefiles runs `git init -q` in a temp dir so buildIndex
// takes the git-backed branch. Tests that need .gitignore honoured use this.
func initGitRepoForWorkspacefiles(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Write a minimal .git directory so isGitRepo picks it up without needing
	// a real `git init`. The buildIndexFromGit path will shell out to the real
	// `git` binary, which resolves the repo via the .git directory we make.
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init failed (skipping git-backed test): %v: %s", err, string(out))
	}
	return root
}

// TestIntegration_SearchRespectsMaxCap generates enough files to exceed the
// configured MaxEntries cap, then asserts the index is truncated and the
// truncated flag is propagated up through Search.
func TestIntegration_SearchRespectsMaxCap(t *testing.T) {
	// 30k files would blow out the temp dir on CI; assert the cap behaviour
	// with a smaller, equivalent knob. The production path uses the same
	// code with MaxEntries = 25_000 (DefaultMaxEntries). Verifying at 200
	// exercises the boundary logic identically.
	const capFiles = 200
	root := t.TempDir()
	for i := 0; i < capFiles+50; i++ {
		name := fmt.Sprintf("file-%04d.go", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	searcher := NewSearcher(Config{MaxEntries: capFiles})
	// Limit = capFiles so the rank step won't truncate; only the walker cap
	// should trigger truncation.
	results, truncated, err := searcher.Search(root, "", capFiles)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true when cap is hit")
	}
	if len(results) != capFiles {
		t.Fatalf("results=%d, want %d", len(results), capFiles)
	}
}

// TestIntegration_SearchCaseInsensitive verifies the documented contract:
// normalizeQuery lowercases both the query and the file path/name before
// scoring, so a query of "FOO" must match "foo.go".
func TestIntegration_SearchCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "foo.go"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write foo.go: %v", err)
	}
	searcher := NewSearcher(Config{})
	results, _, err := searcher.Search(root, "FOO", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].Path != "foo.go" {
		t.Fatalf("expected foo.go for case-insensitive FOO, got %+v", results)
	}
}

// TestIntegration_SearchEmptyQueryReturnsAllOrNothing documents the
// contract: an empty/blank query returns every indexed file, sorted with
// directories ahead of files (directories score 0, files score 1).
func TestIntegration_SearchEmptyQueryReturnsAllOrNothing(t *testing.T) {
	root := nestedWorkspace(t)
	searcher := NewSearcher(Config{})
	results, _, err := searcher.Search(root, "", 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("empty query should return all indexed files, got 0")
	}
	// Directories come first per scoreEntry — verify at least one directory
	// appears at the top of the sorted results.
	if len(results) > 0 && results[0].Kind != "directory" {
		t.Fatalf("expected a directory at position 0 for empty query, got kind=%q (%q)", results[0].Kind, results[0].Path)
	}
}

// TestIntegration_SearchConcurrentOnSameWorkspace fires 10 goroutines at
// the same Searcher against the same workspace and asserts they all
// return identical result sets with no data races. Run with -race.
func TestIntegration_SearchConcurrentOnSameWorkspace(t *testing.T) {
	root := nestedWorkspace(t)
	searcher := NewSearcher(Config{})

	const goroutines = 10
	var wg sync.WaitGroup
	resultsCh := make(chan []string, goroutines)
	errsCh := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			files, _, err := searcher.Search(root, "App", 50)
			if err != nil {
				errsCh <- err
				return
			}
			paths := make([]string, 0, len(files))
			for _, f := range files {
				paths = append(paths, f.Path)
			}
			resultsCh <- paths
		}()
	}
	wg.Wait()
	close(resultsCh)
	close(errsCh)
	for err := range errsCh {
		t.Fatalf("goroutine Search: %v", err)
	}
	var baseline []string
	first := true
	for paths := range resultsCh {
		if first {
			baseline = paths
			first = false
			continue
		}
		if len(paths) != len(baseline) {
			t.Fatalf("inconsistent result length across goroutines: baseline=%d current=%d", len(baseline), len(paths))
		}
		for i := range paths {
			if paths[i] != baseline[i] {
				t.Fatalf("inconsistent ordering at index %d: %q vs %q", i, paths[i], baseline[i])
			}
		}
	}
}
