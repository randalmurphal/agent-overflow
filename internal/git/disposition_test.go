package git

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestMergeBranchFastForwardAndMergeCommit(t *testing.T) {
	t.Run("fast-forward", func(t *testing.T) {
		repo := testutil.InitGitRepo(t)
		testutil.RunGit(t, repo, "checkout", "-b", "item")
		writeDispositionFixture(t, repo, "item.txt", "item\n")
		testutil.RunGit(t, repo, "add", "item.txt")
		testutil.RunGit(t, repo, "commit", "-m", "item")
		testutil.RunGit(t, repo, "checkout", "main")

		result, err := NewCore().MergeBranch(repo, "main", "item")
		if err != nil {
			t.Fatalf("MergeBranch: %v", err)
		}
		if result.Mode != "ff" || result.SHA == "" {
			t.Fatalf("result = %+v, want ff receipt", result)
		}
	})

	t.Run("merge commit", func(t *testing.T) {
		repo := testutil.InitGitRepo(t)
		testutil.RunGit(t, repo, "checkout", "-b", "item")
		writeDispositionFixture(t, repo, "item.txt", "item\n")
		testutil.RunGit(t, repo, "add", "item.txt")
		testutil.RunGit(t, repo, "commit", "-m", "item")
		testutil.RunGit(t, repo, "checkout", "main")
		writeDispositionFixture(t, repo, "base.txt", "base\n")
		testutil.RunGit(t, repo, "add", "base.txt")
		testutil.RunGit(t, repo, "commit", "-m", "base")

		result, err := NewCore().MergeBranch(repo, "main", "item")
		if err != nil {
			t.Fatalf("MergeBranch: %v", err)
		}
		if result.Mode != "merge" || result.SHA == "" {
			t.Fatalf("result = %+v, want merge receipt", result)
		}
	})
}

func TestMergeBranchRefusesDirtyBaseAndConflictWithoutMutation(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		repo := testutil.InitGitRepo(t)
		testutil.RunGit(t, repo, "branch", "item")
		core := NewCore()
		before, err := core.HeadSHA(repo)
		if err != nil {
			t.Fatal(err)
		}
		writeDispositionFixture(t, repo, "dirty.txt", "dirty\n")
		if _, err := core.MergeBranch(repo, "main", "item"); err == nil {
			t.Fatal("dirty merge unexpectedly succeeded")
		}
		after, err := core.HeadSHA(repo)
		if err != nil {
			t.Fatal(err)
		}
		if after != before || core.CurrentBranch(repo) != "main" {
			t.Fatalf("dirty refusal mutated repository: before=%s after=%s branch=%s", before, after, core.CurrentBranch(repo))
		}
		if content, err := os.ReadFile(filepath.Join(repo, "dirty.txt")); err != nil || string(content) != "dirty\n" {
			t.Fatalf("dirty refusal changed untracked file: content=%q err=%v", content, err)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		repo := testutil.InitGitRepo(t)
		testutil.RunGit(t, repo, "checkout", "-b", "item")
		writeDispositionFixture(t, repo, "README.txt", "item\n")
		testutil.RunGit(t, repo, "add", "README.txt")
		testutil.RunGit(t, repo, "commit", "-m", "item")
		testutil.RunGit(t, repo, "checkout", "main")
		writeDispositionFixture(t, repo, "README.txt", "base\n")
		testutil.RunGit(t, repo, "add", "README.txt")
		testutil.RunGit(t, repo, "commit", "-m", "base")
		core := NewCore()
		before, err := core.HeadSHA(repo)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.MergeBranch(repo, "main", "item"); err == nil {
			t.Fatal("conflicted merge unexpectedly succeeded")
		}
		after, err := core.HeadSHA(repo)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("conflict changed HEAD from %s to %s", before, after)
		}
		if count, err := core.CountWorkingTreeChanges(repo); err != nil || count != 0 {
			t.Fatalf("conflict left working tree count=%d err=%v", count, err)
		}
	})
}

func TestMergeBranchRejectsRevisionExpressionsAsBranches(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "item")
	core := NewCore()
	before, err := core.HeadSHA(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.MergeBranch(repo, "main~0", "item"); err == nil {
		t.Fatal("revision expression was accepted as a base branch")
	}
	after, err := core.HeadSHA(repo)
	if err != nil {
		t.Fatal(err)
	}
	if after != before || core.CurrentBranch(repo) != "main" {
		t.Fatalf("revision refusal mutated repository: before=%s after=%s branch=%s", before, after, core.CurrentBranch(repo))
	}
}

func writeDispositionFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
