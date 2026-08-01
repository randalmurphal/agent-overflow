package workspacefiles

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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
		"README.md":                          "# Readme",
		"src/components/App.tsx":             "app",
		"src/components/Button.ts":           "button",
		"src/utils/helper.ts":                "helper",
		"frontend/lib/index.ts":              "index",
		".git/objects/abc":                   "ignored",
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

func TestSearchEvictsExpiredSiblingIndices(t *testing.T) {
	rootA := setupWorkspace(t)
	rootB := setupWorkspace(t)
	searcher := NewSearcher(Config{TTL: 10 * time.Millisecond})

	if _, _, err := searcher.Search(rootA, "App", 10); err != nil {
		t.Fatalf("Search rootA: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	// A store for another root sweeps expired siblings.
	if _, _, err := searcher.Search(rootB, "App", 10); err != nil {
		t.Fatalf("Search rootB: %v", err)
	}

	searcher.mu.Lock()
	_, aResident := searcher.indices[rootA]
	_, bResident := searcher.indices[rootB]
	searcher.mu.Unlock()
	if aResident {
		t.Fatal("expected rootA's expired index to be evicted")
	}
	if !bResident {
		t.Fatal("expected rootB's fresh index to stay resident")
	}

	// Re-searching the evicted root rebuilds transparently.
	results, _, err := searcher.Search(rootA, "App", 10)
	if err != nil {
		t.Fatalf("Search rootA after eviction: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results after transparent rebuild")
	}
}

// rankEntriesReference is the pre-top-k implementation (full stable sort +
// truncate). The bounded selection must return byte-identical output.
func rankEntriesReference(entries []searchableEntry, query string, limit int) []rankedEntry {
	var ranked []rankedEntry
	for _, entry := range entries {
		score := scoreEntry(entry, query)
		if score < 0 {
			continue
		}
		ranked = append(ranked, rankedEntry{entry: entry, score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		return ranked[i].entry.Path < ranked[j].entry.Path
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

func TestRankEntriesMatchesFullSortRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	segments := []string{"src", "lib", "app", "cmp", "util", "btn", "idx", "main", "test"}
	queries := []string{"", "a", "src", "btn", "main.ts", "sb", "zzz", "srcmain"}

	for trial := 0; trial < 200; trial++ {
		// Unique paths (an index never holds duplicates), heavy score ties:
		// short segment alphabet means many entries share prefixes, and the
		// empty query scores every entry 0 or 1.
		n := rng.Intn(400)
		entries := make([]searchableEntry, 0, n)
		seen := make(map[string]struct{}, n)
		for len(entries) < n {
			depth := 1 + rng.Intn(3)
			parts := make([]string, depth)
			for i := range parts {
				parts[i] = segments[rng.Intn(len(segments))]
			}
			path := strings.Join(parts, "/") + fmt.Sprintf("%d.ts", rng.Intn(50))
			if _, dup := seen[path]; dup {
				continue
			}
			seen[path] = struct{}{}
			kind := "file"
			if rng.Intn(4) == 0 {
				kind = "directory"
			}
			entries = append(entries, makeEntry(path, kind))
		}

		query := normalizeQuery(queries[rng.Intn(len(queries))])
		limit := 1 + rng.Intn(60)

		got := rankEntries(entries, query, limit)
		want := rankEntriesReference(entries, query, limit)
		if len(got) != len(want) {
			t.Fatalf("trial %d (query=%q limit=%d): len %d, want %d", trial, query, limit, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("trial %d (query=%q limit=%d): position %d = %+v, want %+v",
					trial, query, limit, i, got[i], want[i])
			}
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
