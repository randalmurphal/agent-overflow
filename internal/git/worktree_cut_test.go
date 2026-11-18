package git

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// UndoCut takes back a cut only as far as it is still the cut's: a
// checkout holding work stays with its branch, a branch that moved or is
// checked out elsewhere stays, a branch the cut did not make is never
// deleted, and an untouched cut goes with the branch it made.
func TestUndoCutTakesBackOnlyWhatIsStillTheCuts(t *testing.T) {
	t.Parallel()
	cut := func(t *testing.T, branch string) (repo, path, tip string) {
		t.Helper()
		repo = initGitRepo(t)
		path = filepath.Join(t.TempDir(), "cut")
		tip, err := NewCore().CreateWorktreeFromRef(repo, path, Ref{}, branch)
		if err != nil || tip == "" {
			t.Fatalf("CreateWorktreeFromRef = %q, %v", tip, err)
		}
		if head := revParse(t, repo, "HEAD"); head != tip {
			t.Fatalf("the cut reports its branch made at %s, want HEAD %s", tip, head)
		}
		return repo, path, tip
	}
	gone := func(t *testing.T, path string) bool {
		t.Helper()
		_, err := os.Stat(path)
		return errors.Is(err, fs.ErrNotExist)
	}

	t.Run("untouched", func(t *testing.T) {
		t.Parallel()
		repo, path, tip := cut(t, "ao-untouched")
		if err := NewCore().UndoCut(repo, path, "ao-untouched", tip); err != nil {
			t.Fatalf("UndoCut = %v", err)
		}
		if !gone(t, path) || branchListed(t, repo, "ao-untouched") {
			t.Fatal("an untouched cut left its checkout or its branch")
		}
	})
	t.Run("an untracked file", func(t *testing.T) {
		t.Parallel()
		repo, path, tip := cut(t, "ao-dirty")
		if err := os.WriteFile(filepath.Join(path, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var kept *CutKept
		if err := NewCore().UndoCut(repo, path, "ao-dirty", tip); !errors.As(err, &kept) ||
			kept.Path != path || kept.Branch != "ao-dirty" || !strings.Contains(kept.PathReason, "1 uncommitted or untracked file") {
			t.Fatalf("UndoCut = %v, want the checkout and its branch kept for the file in it", err)
		}
		if gone(t, filepath.Join(path, "mine.txt")) || !branchListed(t, repo, "ao-dirty") {
			t.Fatal("a checkout holding a file lost it or its branch")
		}
	})
	t.Run("a moved branch", func(t *testing.T) {
		t.Parallel()
		repo, path, tip := cut(t, "ao-moved")
		testutil.RunGit(t, path, "commit", "--allow-empty", "-m", "mine")
		var kept *CutKept
		if err := NewCore().UndoCut(repo, path, "ao-moved", tip); !errors.As(err, &kept) ||
			kept.Path != "" || kept.Branch != "ao-moved" || !strings.Contains(kept.BranchReason, "moved") {
			t.Fatalf("UndoCut = %v, want the clean checkout gone and the moved branch kept", err)
		}
		if !gone(t, path) || !branchListed(t, repo, "ao-moved") {
			t.Fatal("a moved branch was deleted, or its clean checkout kept")
		}
	})
	t.Run("a branch checked out elsewhere", func(t *testing.T) {
		t.Parallel()
		repo, path, tip := cut(t, "ao-elsewhere")
		testutil.RunGit(t, path, "switch", "--detach")
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		testutil.RunGit(t, repo, "worktree", "add", elsewhere, "ao-elsewhere")
		var kept *CutKept
		if err := NewCore().UndoCut(repo, path, "ao-elsewhere", tip); !errors.As(err, &kept) ||
			kept.Branch != "ao-elsewhere" || !strings.Contains(kept.BranchReason, "checked out at") {
			t.Fatalf("UndoCut = %v, want the branch another checkout has kept", err)
		}
		if !gone(t, path) || !branchListed(t, repo, "ao-elsewhere") {
			t.Fatal("a branch checked out elsewhere was deleted, or the cut's clean checkout kept")
		}
	})
	t.Run("a branch the cut did not make", func(t *testing.T) {
		t.Parallel()
		repo := initGitRepo(t)
		testutil.RunGit(t, repo, "branch", "ao-theirs")
		path := filepath.Join(t.TempDir(), "cut")
		madeTip, err := NewCore().AttachWorktreeAt(repo, path, BranchRef("ao-theirs"))
		if err != nil || madeTip != "" {
			t.Fatalf("AttachWorktreeAt = %q, %v; want the existing branch checked out", madeTip, err)
		}
		if err := NewCore().UndoCut(repo, path, "ao-theirs", madeTip); err != nil {
			t.Fatalf("UndoCut = %v", err)
		}
		if !gone(t, path) || !branchListed(t, repo, "ao-theirs") {
			t.Fatal("undoing an attach deleted a branch it did not make, or kept its clean checkout")
		}
	})
	t.Run("a branch made with no checkout", func(t *testing.T) {
		t.Parallel()
		repo := initGitRepo(t)
		tip := revParse(t, repo, "HEAD")
		testutil.RunGit(t, repo, "branch", "ao-fetched")
		if err := NewCore().UndoCut(repo, "", "ao-fetched", tip); err != nil || branchListed(t, repo, "ao-fetched") {
			t.Fatalf("UndoCut = %v, branch left %v; want the branch it made at its tip gone", err, branchListed(t, repo, "ao-fetched"))
		}
		testutil.RunGit(t, repo, "branch", "ao-fetched-moved")
		testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "on main")
		testutil.RunGit(t, repo, "branch", "-f", "ao-fetched-moved", "HEAD")
		var kept *CutKept
		if err := NewCore().UndoCut(repo, "", "ao-fetched-moved", tip); !errors.As(err, &kept) ||
			kept.Path != "" || kept.Branch != "ao-fetched-moved" || !strings.Contains(kept.BranchReason, "moved") {
			t.Fatalf("UndoCut = %v, want the moved branch kept", err)
		}
		if !branchListed(t, repo, "ao-fetched-moved") {
			t.Fatal("a branch that moved since it was made was deleted")
		}
	})
}

func branchListed(t *testing.T, repo, branch string) bool {
	t.Helper()
	return testutil.RunGitAllowError(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
}
