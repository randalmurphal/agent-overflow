package git

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// setOriginHead points refs/remotes/origin/HEAD at refs/remotes/origin/<branch>,
// creating the remote-tracking ref first so the symref resolves.
func setOriginHead(t *testing.T, repo, branch string) {
	t.Helper()
	testutil.RunGit(t, repo, "update-ref", "refs/remotes/origin/"+branch, "HEAD")
	testutil.RunGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

func TestDefaultBranchNameCachesPositiveAnswerUntilTTL(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	setOriginHead(t, repo, "trunk")

	core := NewCore()
	now := time.Unix(10_000, 0)
	core.nowFn = func() time.Time { return now }

	branch, err := core.defaultBranchName(repo)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if branch != "trunk" {
		t.Fatalf("first read = %q, want trunk", branch)
	}

	// Repoint origin/HEAD out of band; within the TTL the cached answer
	// must be served without a subprocess seeing the change.
	setOriginHead(t, repo, "develop")
	branch, err = core.defaultBranchName(repo)
	if err != nil {
		t.Fatalf("cached read: %v", err)
	}
	if branch != "trunk" {
		t.Fatalf("cached read = %q, want the pre-change trunk", branch)
	}

	now = now.Add(repoMetaTTL + time.Second)
	branch, err = core.defaultBranchName(repo)
	if err != nil {
		t.Fatalf("post-TTL read: %v", err)
	}
	if branch != "develop" {
		t.Fatalf("post-TTL read = %q, want develop", branch)
	}
}

func TestDefaultBranchNameDoesNotCacheMissingSymref(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	branch, err := core.defaultBranchName(repo)
	if err != nil {
		t.Fatalf("read without origin/HEAD: %v", err)
	}
	if branch != "" {
		t.Fatalf("read without origin/HEAD = %q, want empty", branch)
	}

	// The symref materializing (clone, `git remote set-head`, fetch on
	// newer git) must be visible immediately — "" is never cached.
	setOriginHead(t, repo, "main")
	branch, err = core.defaultBranchName(repo)
	if err != nil {
		t.Fatalf("read after set-head: %v", err)
	}
	if branch != "main" {
		t.Fatalf("read after set-head = %q, want main", branch)
	}
}

func TestOriginRemoteCachesKnownIdentityUntilTTL(t *testing.T) {
	repo, bare := repoWithOrigin(t)

	core := NewCore()
	now := time.Unix(10_000, 0)
	core.nowFn = func() time.Time { return now }

	origin := core.originRemote(repo)
	if !origin.known || origin.url != bare {
		t.Fatalf("first read = %+v, want known url %q", origin, bare)
	}

	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://example.com/retargeted.git")
	if origin = core.originRemote(repo); origin.url != bare {
		t.Fatalf("cached read = %q, want the pre-change %q", origin.url, bare)
	}

	now = now.Add(repoMetaTTL + time.Second)
	if origin = core.originRemote(repo); origin.url != "https://example.com/retargeted.git" {
		t.Fatalf("post-TTL read = %q, want the retargeted url", origin.url)
	}
}

func TestInvalidateForgeCacheDropsCachedOriginAndDefaultBranch(t *testing.T) {
	repo, bare := repoWithOrigin(t)
	setOriginHead(t, repo, "trunk")

	core := NewCore()
	if origin := core.originRemote(repo); origin.url != bare {
		t.Fatalf("warm read = %q, want %q", origin.url, bare)
	}
	if branch, _ := core.defaultBranchName(repo); branch != "trunk" {
		t.Fatalf("warm read = %q, want trunk", branch)
	}

	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://example.com/retargeted.git")
	setOriginHead(t, repo, "develop")
	core.InvalidateForgeCache(repo)

	if origin := core.originRemote(repo); origin.url != "https://example.com/retargeted.git" {
		t.Fatalf("post-invalidate read = %q, want the retargeted url", origin.url)
	}
	if branch, _ := core.defaultBranchName(repo); branch != "develop" {
		t.Fatalf("post-invalidate read = %q, want develop", branch)
	}
}

func TestOriginRemoteDoesNotCacheUnknown(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	core := NewCore()
	if origin := core.originRemote(repo); origin.known {
		t.Fatalf("repo without origin read as known: %+v", origin)
	}

	// Gaining an origin must be visible immediately — unknown covers
	// both "no remote" and "read failed" and neither is cached.
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://example.com/new.git")
	origin := core.originRemote(repo)
	if !origin.known || origin.url != "https://example.com/new.git" {
		t.Fatalf("read after remote add = %+v, want the new url", origin)
	}
}

func TestRepoMetaCacheIsSharedAcrossWorktrees(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	setOriginHead(t, repo, "trunk")
	testutil.RunGit(t, repo, "branch", "feature/meta-cache")
	worktree := filepath.Join(t.TempDir(), "meta-cache")
	testutil.RunGit(t, repo, "worktree", "add", worktree, "feature/meta-cache")

	core := NewCore()
	if branch, _ := core.defaultBranchName(repo); branch != "trunk" {
		t.Fatalf("primary checkout read = %q, want trunk", branch)
	}

	// The worktree is a different cwd but the same repository; keying on
	// anything other than the common dir would miss here and re-probe,
	// observing the out-of-band change.
	setOriginHead(t, repo, "develop")
	if branch, _ := core.defaultBranchName(worktree); branch != "trunk" {
		t.Fatalf("worktree read = %q, want the shared cached trunk", branch)
	}
}
