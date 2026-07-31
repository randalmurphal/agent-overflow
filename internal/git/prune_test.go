package git

import (
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestListPruneCandidatesClassifiesGoneBranches(t *testing.T) {
	repo := testutil.GonePruneRepo(t)
	core := NewCore()

	candidates, err := core.ListPruneCandidates(repo)
	if err != nil {
		t.Fatalf("ListPruneCandidates: %v", err)
	}

	byName := make(map[string]PruneCandidate, len(candidates))
	for _, c := range candidates {
		byName[c.Name] = c
	}
	if len(candidates) != 2 {
		t.Fatalf("expected exactly merged-gone + squashed-gone, got %+v", candidates)
	}
	merged, ok := byName["merged-gone"]
	if !ok || !merged.MergedIntoDefault {
		t.Fatalf("merged-gone should be a merged candidate, got %+v", byName)
	}
	if merged.Tip == "" || merged.Subject != "work on merged-gone" {
		t.Fatalf("expected tip+subject populated, got %+v", merged)
	}
	squashed, ok := byName["squashed-gone"]
	if !ok || squashed.MergedIntoDefault {
		t.Fatalf("squashed-gone should be an unmerged candidate, got %+v", byName)
	}
	if squashed.Subject != "squash | pipes kept" {
		t.Fatalf("subject with embedded '|' must round-trip intact, got %q", squashed.Subject)
	}
	if _, ok := byName["local-only"]; ok {
		t.Fatal("local-only has no upstream and must never be a prune candidate")
	}
	if _, ok := byName["main"]; ok {
		t.Fatal("default branch must never be a prune candidate")
	}
}

func TestListPruneCandidatesFallsBackToLocalMainWithoutOriginHead(t *testing.T) {
	repo := testutil.GonePruneRepo(t)
	// origin/HEAD unset (manual remote add, partial clones) — the merged
	// check must still run against the conventional local main.
	testutil.RunGit(t, repo, "remote", "set-head", "origin", "--delete")

	core := NewCore()
	candidates, err := core.ListPruneCandidates(repo)
	if err != nil {
		t.Fatalf("ListPruneCandidates: %v", err)
	}
	byName := make(map[string]PruneCandidate, len(candidates))
	for _, c := range candidates {
		byName[c.Name] = c
	}
	if len(candidates) != 2 {
		t.Fatalf("expected exactly merged-gone + squashed-gone, got %+v", candidates)
	}
	if merged := byName["merged-gone"]; !merged.MergedIntoDefault {
		t.Fatalf("merged-gone must classify merged via the local-main fallback, got %+v", merged)
	}
	if squashed := byName["squashed-gone"]; squashed.MergedIntoDefault {
		t.Fatalf("squashed-gone must stay unmerged, got %+v", squashed)
	}
}

func TestListPruneCandidatesExcludesWorktreeCheckouts(t *testing.T) {
	repo := testutil.GonePruneRepo(t)
	worktree := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "worktree", "add", worktree, "squashed-gone")

	core := NewCore()
	candidates, err := core.ListPruneCandidates(repo)
	if err != nil {
		t.Fatalf("ListPruneCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.Name == "squashed-gone" {
			t.Fatalf("branch checked out in a worktree must be excluded, got %+v", candidates)
		}
	}
}

func TestDeleteLocalBranch(t *testing.T) {
	repo := testutil.GonePruneRepo(t)
	core := NewCore()

	if err := core.DeleteLocalBranch(repo, "squashed-gone"); err != nil {
		t.Fatalf("DeleteLocalBranch: %v", err)
	}
	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	for _, b := range branches {
		if b.Name == "squashed-gone" {
			t.Fatal("branch should be gone after DeleteLocalBranch")
		}
	}

	if err := core.DeleteLocalBranch(repo, "main"); err == nil {
		t.Fatal("deleting the checked-out branch must fail")
	}
	if err := core.DeleteLocalBranch(repo, " "); err == nil {
		t.Fatal("empty branch name must fail")
	}
}
