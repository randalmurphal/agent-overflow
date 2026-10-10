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

func readIdentity(t *testing.T, core *Core, cwd string) RepoIdentity {
	t.Helper()
	identity, err := core.ReadRepoIdentity(context.Background(), cwd)
	if err != nil {
		t.Fatalf("ReadRepoIdentity(%s): %v", cwd, err)
	}
	return identity
}

func TestRepoIdentityReadsTheTransientOrigin(t *testing.T) {
	t.Parallel()
	repo, bare := repoWithOrigin(t)

	identity := readIdentity(t, NewCore(), repo)
	if !identity.Repository {
		t.Error("Repository = false for a checkout")
	}
	if identity.RemoteURL != bare {
		t.Errorf("RemoteURL = %q, want the origin %q", identity.RemoteURL, bare)
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

	if got := readIdentity(t, core, repo).RemoteURL; got != "https://example.com/moved.git" {
		t.Fatalf("RemoteURL = %q, want the reconfigured origin", got)
	}
}

// A remoteless repository is still Git, but has no cross-computer identity.
func TestRepoIdentityRecognizesARepositoryWithoutAnOrigin(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)

	identity := readIdentity(t, NewCore(), repo)
	if !identity.Repository || identity.RemoteURL != "" {
		t.Fatalf("identity = %+v, want a repository with no remote", identity)
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
		identity, err := core.ReadRepoIdentity(context.Background(), path)
		if err != nil {
			t.Errorf("%s: err = %v, want none", name, err)
		}
		if identity != (RepoIdentity{}) {
			t.Errorf("%s: identity = %+v, want zero", name, identity)
		}
	}
}

// An unborn HEAD is a normal state (`git init`, nothing committed yet), not a
// failure: it is still a repository.
func TestRepoIdentityRecognizesAnUnbornHead(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := testutil.RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		testutil.RunGit(t, repo, "init")
	}

	identity := readIdentity(t, NewCore(), repo)
	if identity != (RepoIdentity{Repository: true}) {
		t.Fatalf("identity on an empty repo = %+v, want a repository with nothing else", identity)
	}
}

// A repository git refuses to read is a failure with git's reason, never a
// plain folder: reporting it as "not a repository" is what kept such a
// checkout from ever matching its clones elsewhere.
func TestRepoIdentityReportsARepositoryGitRefuses(t *testing.T) {
	repo := initGitRepo(t)
	// The host may allow every directory through safe.directory (GitHub's
	// runner image writes `[safe] directory = *` to /etc/gitconfig), which
	// skips the ownership check this test needs git to perform.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	identity, err := NewCore().ReadRepoIdentity(context.Background(), repo)
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

	if _, err := NewCore().ReadRepoIdentity(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "config") {
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
	identity, err := NewCore().ReadRepoIdentity(ctx, repo)
	if err == nil {
		t.Fatalf("ReadRepoIdentity with an ended ctx = %+v, want an error", identity)
	}
}
