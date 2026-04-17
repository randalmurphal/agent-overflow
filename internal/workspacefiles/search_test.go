package workspacefiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	dirs := []string{
		"src/components",
		"src/utils",
		"frontend/lib",
		".git/objects",
		"node_modules/fake-pkg",
		"dist/bundle",
		"build/output",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	files := map[string]string{
		"README.md":                "# Readme",
		"src/components/App.tsx":   "app",
		"src/components/Button.ts": "button",
		"src/utils/helper.ts":      "helper",
		"frontend/lib/index.ts":    "index",
		".git/objects/abc":         "ignored",
		"node_modules/fake-pkg/package.json": "ignored",
		"dist/bundle/app.js":                 "ignored",
		"build/output/main.bin":              "ignored",
	}
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

func TestSearchIgnoresBuildDirs(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "", 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		first := strings.SplitN(r.Path, "/", 2)[0]
		if _, ignored := IgnoredDirs[first]; ignored {
			t.Fatalf("result from ignored dir slipped through: %s", r.Path)
		}
	}
}

func TestSearchFuzzyMatchesFilename(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "Button", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected Button match")
	}
	// The best match should be the exact filename, ranked first.
	if !strings.HasSuffix(results[0].Path, "Button.ts") {
		t.Fatalf("expected Button.ts first, got %q", results[0].Path)
	}
}

func TestSearchPrefixScoresHigherThanSubstring(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "app", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected a match for 'app'")
	}
	if !strings.HasSuffix(results[0].Path, "App.tsx") {
		t.Fatalf("expected App.tsx ranked first, got %q", results[0].Path)
	}
}

func TestSearchReturnsDirectoriesToo(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "components", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	foundDir := false
	for _, r := range results {
		if r.Path == "src/components" && r.Kind == "directory" {
			foundDir = true
			break
		}
	}
	if !foundDir {
		t.Fatalf("expected components directory in results, got %+v", results)
	}
}

func TestSearchIgnoresAtPrefixInQuery(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "@README", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected @README to match README.md")
	}
	if results[0].Path != "README.md" {
		t.Fatalf("expected README.md first, got %q", results[0].Path)
	}
}

func TestSearchLimitCapsResults(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{})

	results, _, err := searcher.Search(root, "", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected cap of 2, got %d", len(results))
	}
}

func TestSearchCachesResults(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{TTL: time.Hour})

	_, _, err := searcher.Search(root, "App", 10)
	if err != nil {
		t.Fatalf("first Search: %v", err)
	}

	// Delete the file: a fresh scan would miss it, but we should still see
	// the cached result because TTL is 1 hour.
	if err := os.Remove(filepath.Join(root, "src/components/App.tsx")); err != nil {
		t.Fatalf("remove App.tsx: %v", err)
	}

	results, _, err := searcher.Search(root, "App", 10)
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected cached App match after delete")
	}
}

func TestSearchInvalidateForcesRescan(t *testing.T) {
	root := setupWorkspace(t)
	searcher := NewSearcher(Config{TTL: time.Hour})

	if _, _, err := searcher.Search(root, "App", 10); err != nil {
		t.Fatalf("first Search: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "src/components/App.tsx")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	searcher.Invalidate(root)

	results, _, err := searcher.Search(root, "App", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if strings.HasSuffix(r.Path, "App.tsx") {
			t.Fatalf("stale cache: %s should be gone", r.Path)
		}
	}
}

func TestSearchMissingRootErrors(t *testing.T) {
	searcher := NewSearcher(Config{})
	_, _, err := searcher.Search("/no/such/path/for/test", "", 10)
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestSearchRequiresRoot(t *testing.T) {
	searcher := NewSearcher(Config{})
	_, _, err := searcher.Search("", "foo", 10)
	if err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestSubsequenceScore(t *testing.T) {
	if s := subsequenceScore("src/components/button.ts", "scbtn"); s < 0 {
		t.Fatalf("expected subsequence match, got %d", s)
	}
	if s := subsequenceScore("helper.ts", "zzz"); s >= 0 {
		t.Fatalf("expected no match, got %d", s)
	}
}

func TestBuildIndexRespectsMaxEntries(t *testing.T) {
	root := setupWorkspace(t)
	idx, err := buildIndex(root, 3)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if len(idx.entries) != 3 {
		t.Fatalf("expected exactly 3 entries, got %d", len(idx.entries))
	}
	if !idx.truncated {
		t.Fatal("expected truncated flag")
	}
}
