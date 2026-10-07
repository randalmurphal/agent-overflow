package git

import (
	"context"
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

func readIdentity(t *testing.T, core *Core, cwd, knownRoot string) RepoIdentity {
	t.Helper()
	identity, err := core.ReadRepoIdentity(context.Background(), cwd, knownRoot)
	if err != nil {
		t.Fatalf("ReadRepoIdentity(%s): %v", cwd, err)
	}
	return identity
}

func TestRepoIdentityReadsOriginAndTheSingleRoot(t *testing.T) {
	t.Parallel()
	repo, bare := repoWithOrigin(t)
	root := headCommit(t, repo)

	identity := readIdentity(t, NewCore(), repo, "")
	if !identity.Repository {
		t.Error("Repository = false for a checkout")
	}
	if identity.RemoteURL != bare {
		t.Errorf("RemoteURL = %q, want the origin %q", identity.RemoteURL, bare)
	}
	if identity.RootCommit != root {
		t.Errorf("RootCommit = %q, want the initial commit %q", identity.RootCommit, root)
	}
}

// The origin is read fresh, not from the status cache: a persisted identity
// must not record a remote that was just reconfigured as its old value.
func TestRepoIdentityReadsAChangedOriginImmediately(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	_ = core.OriginRemoteURL(repo) // warm the status cache
	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://example.com/moved.git")

	if got := readIdentity(t, core, repo, "").RemoteURL; got != "https://example.com/moved.git" {
		t.Fatalf("RemoteURL = %q, want the reconfigured origin", got)
	}
}

// A remoteless repository still has an identity: the root commit is what lets
// two clones of a never-published repo be recognised as one project.
func TestRepoIdentityAnswersRootWithoutAnOrigin(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	root := headCommit(t, repo)

	identity := readIdentity(t, NewCore(), repo, "")
	if !identity.Repository || identity.RemoteURL != "" || identity.RootCommit != root {
		t.Fatalf("identity = %+v, want a repository with no remote and root %q", identity, root)
	}
}

// `rev-list --max-parents=0 HEAD` lists every root in traversal order, which
// depends on which branch is checked out. Sorting is what makes two machines
// holding the same repository answer the same string.
func TestRepoIdentityPicksTheSmallestOfSeveralRoots(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
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
	if got := readIdentity(t, NewCore(), repo, "").RootCommit; got != want {
		t.Fatalf("RootCommit = %q, want the smaller of %q and %q", got, first, second)
	}
}

// A known root the repository still contains is kept without walking history;
// one it does not contain (the path now holds another repository) is read
// fresh, and a value that is not an object name never reaches git.
func TestRepoIdentityKeepsAKnownRootOnlyWhenTheRepositoryHasIt(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	root := headCommit(t, repo)
	core := NewCore()

	// A second commit is a commit the repository has, so it stands in for
	// a stored root that rev-list would not return: kept means not re-read.
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "second")
	second := headCommit(t, repo)
	if got := readIdentity(t, core, repo, second).RootCommit; got != second {
		t.Fatalf("RootCommit = %q, want the known %q kept", got, second)
	}
	missing := strings.Repeat("a", 40)
	if got := readIdentity(t, core, repo, missing).RootCommit; got != root {
		t.Fatalf("RootCommit with a foreign known root = %q, want the real root %q", got, root)
	}
	if got := readIdentity(t, core, repo, "--all").RootCommit; got != root {
		t.Fatalf("RootCommit with an option as known root = %q, want %q", got, root)
	}
}

func TestRepoIdentityIsNotARepositoryOutsideOne(t *testing.T) {
	t.Parallel()
	core := NewCore()
	for name, path := range map[string]string{
		"plain directory": t.TempDir(),
		"missing path":    filepath.Join(t.TempDir(), "gone"),
		"empty path":      "",
	} {
		identity, err := core.ReadRepoIdentity(context.Background(), path, "")
		if err != nil {
			t.Errorf("%s: err = %v, want none", name, err)
		}
		if identity != (RepoIdentity{}) {
			t.Errorf("%s: identity = %+v, want zero", name, identity)
		}
	}
}

// An unborn HEAD is a normal state (`git init`, nothing committed yet), not a
// failure: a repository with an empty root commit.
func TestRepoIdentityHasNoRootForAnUnbornHead(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := testutil.RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		testutil.RunGit(t, repo, "init")
	}

	identity := readIdentity(t, NewCore(), repo, "")
	if identity != (RepoIdentity{Repository: true}) {
		t.Fatalf("identity on an empty repo = %+v, want a repository with nothing else", identity)
	}
}

// A repository git refuses to read is a failure with git's reason, never a
// plain folder: reporting it as "not a repository" is what kept such a
// checkout from ever matching its clones elsewhere.
func TestRepoIdentityReportsARepositoryGitRefuses(t *testing.T) {
	repo := initGitRepo(t)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	identity, err := NewCore().ReadRepoIdentity(context.Background(), repo, "")
	if err == nil {
		t.Fatalf("ReadRepoIdentity = %+v, want the ownership refusal", identity)
	}
	if !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("err = %v, want git's dubious-ownership message", err)
	}
}

func TestRepoIdentityReportsABrokenRepository(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core\n"), 0o644); err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	if _, err := NewCore().ReadRepoIdentity(context.Background(), repo, ""); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("err = %v, want git's bad-config failure", err)
	}
}

// A read cut short by its ctx is an error, never "not a repository" or an
// empty identity, so a caller can tell it apart and skip recording it.
func TestRepoIdentityStopsWithItsContext(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	identity, err := NewCore().ReadRepoIdentity(ctx, repo, "")
	if err == nil {
		t.Fatalf("ReadRepoIdentity with an ended ctx = %+v, want an error", identity)
	}
}
