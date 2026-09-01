package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// headCommit reads the current HEAD sha so a test can assert against the real
// hash rather than a fixture value.
func headCommit(t *testing.T, repo string) string {
	t.Helper()
	result, err := NewCore().run(repo, "rev-parse", "HEAD")
	if err != nil || result.exitCode != 0 {
		t.Fatalf("rev-parse HEAD in %s: %v (exit %d)", repo, err, result.exitCode)
	}
	return strings.TrimSpace(result.stdout)
}

func TestRepoIdentityReadsOriginAndTheSingleRoot(t *testing.T) {
	repo, bare := repoWithOrigin(t)
	root := headCommit(t, repo)

	remoteURL, rootCommit := NewCore().RepoIdentity(repo)
	if remoteURL != bare {
		t.Errorf("remoteURL = %q, want the origin %q", remoteURL, bare)
	}
	if rootCommit != root {
		t.Errorf("rootCommit = %q, want the initial commit %q", rootCommit, root)
	}
}

// A remoteless repository still has an identity: the root commit is what lets
// two clones of a never-published repo be recognised as one project.
func TestRepoIdentityAnswersRootWithoutAnOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	root := headCommit(t, repo)

	remoteURL, rootCommit := NewCore().RepoIdentity(repo)
	if remoteURL != "" {
		t.Errorf("remoteURL = %q, want empty for a repo with no origin", remoteURL)
	}
	if rootCommit != root {
		t.Errorf("rootCommit = %q, want %q", rootCommit, root)
	}
}

// `rev-list --max-parents=0 HEAD` lists every root in traversal order, which
// depends on which branch is checked out. Sorting is what makes two machines
// holding the same repository answer the same string.
func TestRepoIdentityPicksTheSmallestOfSeveralRoots(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	first := headCommit(t, repo)

	testutil.RunGit(t, repo, "checkout", "--orphan", "second-root")
	// The orphan branch inherits the index and working tree; clear both so
	// the later checkout back to main is not blocked by untracked leftovers.
	testutil.RunGit(t, repo, "rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatalf("write other.txt: %v", err)
	}
	testutil.RunGit(t, repo, "add", "other.txt")
	testutil.RunGit(t, repo, "commit", "-m", "second root")
	second := headCommit(t, repo)

	testutil.RunGit(t, repo, "checkout", "main")
	testutil.RunGit(t, repo, "merge", "--allow-unrelated-histories", "--no-edit", "second-root")

	want := first
	if second < want {
		want = second
	}
	if _, rootCommit := NewCore().RepoIdentity(repo); rootCommit != want {
		t.Fatalf("rootCommit = %q, want the smaller of %q and %q", rootCommit, first, second)
	}
}

func TestRepoIdentityIsEmptyOutsideARepository(t *testing.T) {
	remoteURL, rootCommit := NewCore().RepoIdentity(t.TempDir())
	if remoteURL != "" || rootCommit != "" {
		t.Fatalf("RepoIdentity outside a repo = (%q, %q), want both empty", remoteURL, rootCommit)
	}
}

// An unborn HEAD is a normal state (`git init`, nothing committed yet), not a
// failure: both halves answer "not known" and the caller stores empty.
func TestRepoIdentityIsEmptyForAnUnbornHead(t *testing.T) {
	repo := t.TempDir()
	if err := testutil.RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		testutil.RunGit(t, repo, "init")
	}

	remoteURL, rootCommit := NewCore().RepoIdentity(repo)
	if remoteURL != "" || rootCommit != "" {
		t.Fatalf("RepoIdentity on an empty repo = (%q, %q), want both empty", remoteURL, rootCommit)
	}
}

func TestRepoIdentityIsEmptyForAnEmptyPath(t *testing.T) {
	if remoteURL, rootCommit := NewCore().RepoIdentity(""); remoteURL != "" || rootCommit != "" {
		t.Fatalf("RepoIdentity(\"\") = (%q, %q), want both empty", remoteURL, rootCommit)
	}
}
