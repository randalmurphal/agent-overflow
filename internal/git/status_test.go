package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

func TestParseStatusOutput(t *testing.T) {
	// headNumstat is the combined `git diff HEAD --numstat` output (worktree
	// vs HEAD), so staged and unstaged churn arrive as one stream.
	status := parseStatusOutput(
		`# branch.oid abcdef
# branch.head feature/demo
# branch.upstream origin/feature/demo
# branch.ab +2 -1
1 .M N... 100644 100644 100644 abcdef abcdef tracked.txt
? untracked.txt`,
		"3\t1\ttracked.txt\n1\t0\tstaged.txt\n",
	)

	if status.Branch != "feature/demo" {
		t.Fatalf("Branch = %q, want feature/demo", status.Branch)
	}
	if !status.HasUpstream {
		t.Fatal("expected HasUpstream=true")
	}
	if status.AheadCount != 2 || status.BehindCount != 1 {
		t.Fatalf("ahead/behind = %d/%d, want 2/1", status.AheadCount, status.BehindCount)
	}
	if !status.HasChanges {
		t.Fatal("expected HasChanges=true")
	}
	// FileCount here counts only tracked/staged paths (tracked.txt, staged.txt);
	// the untracked '?' entry is excluded — baseStatus counts untracked files
	// individually via ls-files (git status collapses untracked dirs).
	if status.FileCount != 2 {
		t.Fatalf("FileCount = %d, want 2 (tracked+staged only, untracked excluded)", status.FileCount)
	}
	if status.Insertions != 4 {
		t.Fatalf("Insertions = %d, want 4", status.Insertions)
	}
	if status.Deletions != 1 {
		t.Fatalf("Deletions = %d, want 1", status.Deletions)
	}
}

func TestStatusReturnsNotRepoForNonGitDirectory(t *testing.T) {
	core := NewCore()

	status, err := core.Status(t.TempDir())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.IsRepo {
		t.Fatal("expected IsRepo=false")
	}
}

// repoWithOrigin creates a bare repo + a working clone with that bare
// repo set as origin and an initial main push, returning (workingRepo,
// barePath).
func repoWithOrigin(t *testing.T) (string, string) {
	t.Helper()
	bare := t.TempDir()
	if err := testutil.RunGitAllowError(bare, "init", "--bare", "-b", "main"); err != nil {
		testutil.RunGit(t, bare, "init", "--bare")
	}
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", bare)
	testutil.RunGit(t, repo, "push", "-u", "origin", "main")
	return repo, bare
}

func TestMaybeFetchRemotesRespectsStaleWindow(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	start := time.Unix(10_000, 0)
	now := start
	core.nowFn = func() time.Time { return now }

	fetched, err := core.MaybeFetchRemotes(repo)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if !fetched {
		t.Fatal("first call should fetch (no cached timestamp)")
	}

	// Cached at `start`; staleness gate is now.Sub(last) < FetchStaleWindow.
	// Just under the window — still fresh, skip.
	now = start.Add(FetchStaleWindow - time.Nanosecond)
	fetched, err = core.MaybeFetchRemotes(repo)
	if err != nil {
		t.Fatalf("just-under-window fetch: %v", err)
	}
	if fetched {
		t.Fatal("expected cache to skip just under the stale window")
	}

	// Exactly at the window — Sub == FetchStaleWindow, not less than,
	// so we must refetch. Boundary regression guard.
	now = start.Add(FetchStaleWindow)
	fetched, err = core.MaybeFetchRemotes(repo)
	if err != nil {
		t.Fatalf("at-window fetch: %v", err)
	}
	if !fetched {
		t.Fatal("expected cache to expire AT the boundary (Sub == FetchStaleWindow is not < FetchStaleWindow)")
	}
}

func TestPruneRemotesUpdatesFetchCache(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	now := time.Unix(50_000, 0)
	core.nowFn = func() time.Time { return now }

	if err := core.PruneRemotes(repo); err != nil {
		t.Fatalf("PruneRemotes: %v", err)
	}

	// Prune must refresh the cache clock so the picker's next
	// background fetch sees a fresh window and skips.
	fetched, err := core.MaybeFetchRemotes(repo)
	if err != nil {
		t.Fatalf("MaybeFetchRemotes after prune: %v", err)
	}
	if fetched {
		t.Fatal("expected prune to reset the stale window")
	}
}

func TestPruneRemotesErrorLeavesCacheUntouched(t *testing.T) {
	// A non-git directory fails at RepositoryRoot — the early error
	// path must NOT stamp the fetch cache, otherwise a subsequent open
	// against a real repo would inherit the stale entry.
	notARepo := t.TempDir()

	core := NewCore()
	now := time.Unix(70_000, 0)
	core.nowFn = func() time.Time { return now }

	if err := core.PruneRemotes(notARepo); err == nil {
		t.Fatal("expected PruneRemotes on a non-git dir to error")
	}

	core.fetchCacheMu.RLock()
	defer core.fetchCacheMu.RUnlock()
	if len(core.fetchCache) != 0 {
		t.Fatalf("expected empty fetch cache after failed prune, got %d entries", len(core.fetchCache))
	}
}

func TestInvalidateFetchCacheForcesRefetch(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	now := time.Unix(80_000, 0)
	core.nowFn = func() time.Time { return now }

	if _, err := core.MaybeFetchRemotes(repo); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	core.InvalidateFetchCache(repo)
	fetched, err := core.MaybeFetchRemotes(repo)
	if err != nil {
		t.Fatalf("MaybeFetchRemotes after invalidate: %v", err)
	}
	if !fetched {
		t.Fatal("expected invalidation to force a refetch")
	}
}

// advanceOriginMain pushes a new commit to the bare repo's main branch
// via a transient sibling clone, simulating an external collaborator.
// Returns once the bare has the new tip.
func advanceOriginMain(t *testing.T, bare string) {
	t.Helper()
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", bare, ".")
	if err := os.WriteFile(filepath.Join(sibling, "outside.txt"), []byte("upstream"), 0o644); err != nil {
		t.Fatalf("write outside.txt: %v", err)
	}
	testutil.RunGit(t, sibling, "add", "outside.txt")
	testutil.RunGit(t, sibling, "-c", "user.email=outside@example.com", "-c", "user.name=Outside",
		"commit", "-m", "upstream commit")
	testutil.RunGit(t, sibling, "push", "origin", "main")
}

func TestSyncBranchPullsCurrentBranch(t *testing.T) {
	repo, bare := repoWithOrigin(t)
	advanceOriginMain(t, bare)

	core := NewCore()
	// Update remote-tracking refs so the FF check has something to chase.
	if _, _, err := core.Execute(repo, "fetch", "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Behind > 0 before sync.
	beforeBranches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches before: %v", err)
	}
	var beforeBehind int
	for _, b := range beforeBranches {
		if b.Name == "main" {
			beforeBehind = b.BehindCount
		}
	}
	if beforeBehind == 0 {
		t.Fatal("expected main to be behind upstream before sync")
	}

	if err := core.SyncBranch(repo, "main"); err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}

	afterBranches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches after: %v", err)
	}
	for _, b := range afterBranches {
		if b.Name == "main" && b.BehindCount != 0 {
			t.Fatalf("expected main to be in sync after SyncBranch, behind=%d", b.BehindCount)
		}
	}

	// Working tree picked up the file pushed by advanceOriginMain.
	if _, err := os.Stat(filepath.Join(repo, "outside.txt")); err != nil {
		t.Fatalf("expected outside.txt to be checked out after pull-style sync: %v", err)
	}
}

func TestSyncBranchUpdatesNonCurrentBranchViaFetchRefspec(t *testing.T) {
	repo, bare := repoWithOrigin(t)

	core := NewCore()

	// Create a `feature` branch on the bare via a sibling, with a unique
	// commit so we can verify the working repo's local `feature` advances.
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", bare, ".")
	testutil.RunGit(t, sibling, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(sibling, "feature.txt"), []byte("feat"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	testutil.RunGit(t, sibling, "add", "feature.txt")
	testutil.RunGit(t, sibling, "-c", "user.email=sib@example.com", "-c", "user.name=Sibling",
		"commit", "-m", "feature commit")
	testutil.RunGit(t, sibling, "push", "-u", "origin", "feature")

	// Working repo now learns about origin/feature and creates a tracking
	// local branch — but stays on main, so feature is non-current.
	if _, _, err := core.Execute(repo, "fetch", "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, _, err := core.Execute(repo, "branch", "--track", "feature", "origin/feature"); err != nil {
		t.Fatalf("create tracking feature: %v", err)
	}

	// Get the sibling's feature tip — that's what the working repo's
	// feature ref should match after sync.
	wantTip, _, err := core.Execute(sibling, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse sibling feature: %v", err)
	}
	wantTip = strings.TrimSpace(wantTip)

	// Push another commit so the working repo's feature is behind.
	if err := os.WriteFile(filepath.Join(sibling, "feature2.txt"), []byte("feat2"), 0o644); err != nil {
		t.Fatalf("write feature2.txt: %v", err)
	}
	testutil.RunGit(t, sibling, "add", "feature2.txt")
	testutil.RunGit(t, sibling, "-c", "user.email=sib@example.com", "-c", "user.name=Sibling",
		"commit", "-m", "feature commit 2")
	testutil.RunGit(t, sibling, "push", "origin", "feature")

	newTip, _, err := core.Execute(sibling, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse sibling feature 2: %v", err)
	}
	newTip = strings.TrimSpace(newTip)
	if newTip == wantTip {
		t.Fatal("setup: sibling feature did not advance")
	}

	// Pre-condition: working repo's feature is still at the old tip.
	got, _, err := core.Execute(repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse working feature: %v", err)
	}
	if strings.TrimSpace(got) != wantTip {
		t.Fatalf("setup: expected working feature at %s, got %s", wantTip, strings.TrimSpace(got))
	}

	// Sync the non-current branch — must update refs/heads/feature
	// without touching the working tree (which is still on main).
	if err := core.SyncBranch(repo, "feature"); err != nil {
		t.Fatalf("SyncBranch feature: %v", err)
	}

	got, _, err = core.Execute(repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse working feature after sync: %v", err)
	}
	if strings.TrimSpace(got) != newTip {
		t.Fatalf("expected feature to advance to %s after sync, got %s", newTip, strings.TrimSpace(got))
	}

	// Working tree still on main — feature.txt would only exist if we'd
	// accidentally checked out feature.
	headBranch, _, err := core.Execute(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if strings.TrimSpace(headBranch) != "main" {
		t.Fatalf("expected HEAD to stay on main, got %s", strings.TrimSpace(headBranch))
	}
}

func TestSyncBranchRefusesDiverged(t *testing.T) {
	repo, bare := repoWithOrigin(t)
	advanceOriginMain(t, bare)

	core := NewCore()
	if _, _, err := core.Execute(repo, "fetch", "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Add a local commit on main so it diverges from origin/main.
	if err := os.WriteFile(filepath.Join(repo, "local.txt"), []byte("local"), 0o644); err != nil {
		t.Fatalf("write local.txt: %v", err)
	}
	testutil.RunGit(t, repo, "add", "local.txt")
	testutil.RunGit(t, repo, "-c", "user.email=local@example.com", "-c", "user.name=Local",
		"commit", "-m", "local commit")

	// Now main is ahead 1, behind 1 → FF-only pull must fail.
	if err := core.SyncBranch(repo, "main"); err == nil {
		t.Fatal("expected SyncBranch to fail on diverged main")
	}
}

func TestSyncBranchRequiresUpstream(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if _, _, err := core.Execute(repo, "checkout", "-b", "no-upstream"); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	err := core.SyncBranch(repo, "no-upstream")
	if err == nil {
		t.Fatal("expected SyncBranch on branch without upstream to fail")
	}
	if !strings.Contains(err.Error(), "no upstream") {
		t.Fatalf("expected error to mention missing upstream, got: %v", err)
	}
}

func TestSyncBranchValidatesName(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.SyncBranch(repo, "")
	if err == nil || !strings.Contains(err.Error(), "branch name is required") {
		t.Fatalf("expected empty branch to fail validation, got: %v", err)
	}
	err = core.SyncBranch(repo, "-flag")
	if err == nil || !strings.Contains(err.Error(), "must not start with -") {
		t.Fatalf("expected flag-shaped branch to fail validation, got: %v", err)
	}
}

func TestSplitUpstreamRef(t *testing.T) {
	remotes := []string{"origin"}

	remote, branch, ok := splitUpstreamRef("origin/main", remotes)
	if !ok || remote != "origin" || branch != "main" {
		t.Fatalf("expected ('origin', 'main', true), got (%q, %q, %v)", remote, branch, ok)
	}

	// Multi-segment branch under origin.
	remote, branch, ok = splitUpstreamRef("origin/feature/foo", remotes)
	if !ok || remote != "origin" || branch != "feature/foo" {
		t.Fatalf("expected ('origin', 'feature/foo', true), got (%q, %q, %v)", remote, branch, ok)
	}

	// Unknown remote prefix.
	if _, _, ok := splitUpstreamRef("other/main", remotes); ok {
		t.Fatal("expected unknown-remote ref to fail")
	}

	// Bare remote name (no branch) — invalid.
	if _, _, ok := splitUpstreamRef("origin/", remotes); ok {
		t.Fatal("expected origin/ to fail (empty branch part)")
	}

	// Slash-in-remote-name tie-break: first match in listRemoteNames
	// order wins. Document the chosen behavior so future config-shape
	// changes don't quietly drift it.
	overlapping := []string{"origin", "origin/sub"}
	remote, branch, ok = splitUpstreamRef("origin/sub/main", overlapping)
	if !ok || remote != "origin" || branch != "sub/main" {
		t.Fatalf("expected first-match win ('origin', 'sub/main', true), got (%q, %q, %v)", remote, branch, ok)
	}
}

func TestRepositoryRootReturnsGitTopLevel(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	workspace := filepath.Join(repo, "nested", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	root, err := NewCore().RepositoryRoot(workspace)
	if err != nil {
		t.Fatalf("RepositoryRoot() error = %v", err)
	}
	if testutil.CanonicalPath(t, root) != testutil.CanonicalPath(t, repo) {
		t.Fatalf("root = %q, want %q", root, repo)
	}
}

func TestRepositoryRootReturnsErrorForNonRepo(t *testing.T) {
	root, err := NewCore().RepositoryRoot(t.TempDir())
	if err == nil {
		t.Fatalf("RepositoryRoot() = %q, want error", root)
	}
	if !strings.Contains(err.Error(), "git rev-parse --show-toplevel failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStatusOnRepositoryWithOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, t.TempDir(), "init", "--bare", remote)
	testutil.RunGit(t, repo, "remote", "add", "origin", remote)
	testutil.RunGit(t, repo, "push", "-u", "origin", "main")

	readmePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readmePath, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("update README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	core := NewCore()
	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	if status.Branch != "main" {
		t.Fatalf("Branch = %q, want main", status.Branch)
	}
	if !status.IsDefaultBranch {
		t.Fatal("expected default branch")
	}
	if !status.HasOriginRemote {
		t.Fatal("expected HasOriginRemote=true")
	}
	if !status.HasUpstream {
		t.Fatal("expected HasUpstream=true after push -u")
	}
	if !status.HasChanges {
		t.Fatal("expected HasChanges=true")
	}
	if status.FileCount < 2 {
		t.Fatalf("FileCount = %d, want at least 2", status.FileCount)
	}
	if status.Insertions == 0 {
		t.Fatal("expected insertions to be counted")
	}
}

// TestStatusReportsForge verifies Status populates GitStatus.Forge
// from the origin URL classification. Covers the three v1 cases:
// github (recognised), gitlab (recognised), self-hosted (unsupported).
func TestStatusReportsForge(t *testing.T) {
	cases := []struct {
		name      string
		originURL string
		want      string
	}{
		{"github https", "https://github.com/owner/repo.git", "github"},
		{"github ssh alias", "git@github.com:owner/repo.git", "github"},
		{"gitlab https", "https://gitlab.com/group/repo.git", "gitlab"},
		{"gitlab ssh alias", "git@gitlab.com:group/repo.git", "gitlab"},
		{"self-hosted", "https://git.example.com/owner/repo.git", ""},
		{"no remote", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.InitGitRepo(t)
			if tc.originURL != "" {
				testutil.RunGit(t, repo, "remote", "add", "origin", tc.originURL)
			}

			core := NewCore()
			status, err := core.Status(repo)
			if err != nil {
				t.Fatalf("Status returned error: %v", err)
			}
			if status.Forge != tc.want {
				t.Errorf("Forge = %q, want %q", status.Forge, tc.want)
			}
			if (tc.originURL != "") != status.HasOriginRemote {
				t.Errorf("HasOriginRemote = %v, want %v", status.HasOriginRemote, tc.originURL != "")
			}
		})
	}
}

func TestStatusSkipsPRLookupWithoutSupportedForge(t *testing.T) {
	cases := []struct {
		name      string
		originURL string
	}{
		{"unsupported remote", "https://git.example.com/owner/repo.git"},
		{"no remote", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.InitGitRepo(t)
			if tc.originURL != "" {
				testutil.RunGit(t, repo, "remote", "add", "origin", tc.originURL)
			}

			status, err := NewCore().Status(repo)
			if err != nil {
				t.Fatalf("Status returned error: %v", err)
			}
			if status.Forge != "" {
				t.Fatalf("Forge = %q, want empty", status.Forge)
			}
			if status.OpenPRLookupError != "" {
				t.Fatalf("OpenPRLookupError = %q, want empty", status.OpenPRLookupError)
			}
			if status.OpenPRURL != "" || status.OpenPRNumber != 0 {
				t.Fatalf("open PR = (%q, %d), want empty", status.OpenPRURL, status.OpenPRNumber)
			}
		})
	}
}

func TestWorkingTreeDiff(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	readmePath := filepath.Join(repo, "README.txt")

	if err := os.WriteFile(readmePath, []byte("hello\nstaged\n"), 0o644); err != nil {
		t.Fatalf("write staged version: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	if err := os.WriteFile(readmePath, []byte("hello\nstaged\nunstaged\n"), 0o644); err != nil {
		t.Fatalf("write unstaged version: %v", err)
	}

	core := NewCore()
	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if !strings.Contains(diff, "README.txt") {
		t.Fatalf("expected diff to mention README.txt, got %q", diff)
	}
}

func TestWorkingTreeDiffReturnsEmptyForCleanRepo(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff for clean repo, got %q", diff)
	}
}

func TestWorkingTreeDiffCachedOnly(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	readmePath := filepath.Join(repo, "README.txt")

	if err := os.WriteFile(readmePath, []byte("hello\nstaged change\n"), 0o644); err != nil {
		t.Fatalf("write staged version: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")

	core := NewCore()
	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if !strings.Contains(diff, "staged change") {
		t.Fatalf("expected diff to contain staged content, got %q", diff)
	}
}

func TestWorkingTreeDiffOnNonRepo(t *testing.T) {
	core := NewCore()

	_, err := core.WorkingTreeDiff(t.TempDir())
	if err == nil {
		t.Fatal("expected error for non-repo directory")
	}
}

func TestCombineDiffsEdgeCases(t *testing.T) {
	if got := combineDiffs("", "cached"); got != "cached" {
		t.Fatalf("combineDiffs empty+cached = %q, want cached", got)
	}
	if got := combineDiffs("head", ""); got != "head" {
		t.Fatalf("combineDiffs head+empty = %q, want head", got)
	}
	if got := combineDiffs("", ""); got != "" {
		t.Fatalf("combineDiffs empty+empty = %q, want empty", got)
	}
}

func TestParseAheadBehindMalformed(t *testing.T) {
	ahead, behind := parseAheadBehind("invalid")
	if ahead != 0 || behind != 0 {
		t.Fatalf("parseAheadBehind(invalid) = %d/%d, want 0/0", ahead, behind)
	}
}

func TestParseNumstatCountMalformed(t *testing.T) {
	if got := parseNumstatCount("abc"); got != 0 {
		t.Fatalf("parseNumstatCount(abc) = %d, want 0", got)
	}
}

func TestParsePorcelainPathIgnoredPrefix(t *testing.T) {
	if got := parsePorcelainPath("! ignored.txt"); got != "ignored.txt" {
		t.Fatalf("parsePorcelainPath for ignored = %q, want ignored.txt", got)
	}
}

func TestParsePorcelainPathEmptyFields(t *testing.T) {
	if got := parsePorcelainPath(""); got != "" {
		t.Fatalf("parsePorcelainPath for empty = %q, want empty", got)
	}
}

func TestParseStatusOutputDetachedHead(t *testing.T) {
	status := parseStatusOutput(
		"# branch.oid abcdef\n# branch.head (HEAD detached)\n",
		"",
	)
	if status.Branch != "" {
		t.Fatalf("Branch = %q, want empty for detached HEAD", status.Branch)
	}
}

func TestParseNumstatSkipsShortLines(t *testing.T) {
	entries := parseNumstat("incomplete\ttwo\n")
	if len(entries) != 0 {
		t.Fatalf("expected no entries for incomplete numstat line, got %d", len(entries))
	}
}

func TestStatusOnCleanRepository(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	if status.HasChanges {
		t.Fatal("expected HasChanges=false for clean repo")
	}
	if status.FileCount != 0 {
		t.Fatalf("FileCount = %d, want 0", status.FileCount)
	}
}

func TestHelperFunctions(t *testing.T) {
	if got := normalizeNumstatPath("old/name.txt => new/name.txt"); got != "new/name.txt" {
		t.Fatalf("normalizeNumstatPath returned %q", got)
	}
	if got := parsePorcelainPath("? notes.txt"); got != "notes.txt" {
		t.Fatalf("parsePorcelainPath for untracked returned %q", got)
	}
	if got := parsePorcelainPath("2 R. N... 100644 100644 100644 abc abc R100\tnew.txt\told.txt"); got != "new.txt" {
		t.Fatalf("parsePorcelainPath for rename returned %q", got)
	}
	if got := combineDiffs("one", "two"); got != "one\ntwo" {
		t.Fatalf("combineDiffs returned %q", got)
	}
	if got := parseNumstatCount("-"); got != 0 {
		t.Fatalf("parseNumstatCount('-') = %d, want 0", got)
	}
	if !isDefaultBranchName("origin/main", "") {
		t.Fatal("expected origin/main to be treated as default when origin HEAD is unavailable")
	}
}

func TestCountWorkingTreeChangesCountsStagedUnstagedAndUntracked(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if count, err := core.CountWorkingTreeChanges(repo); err != nil {
		t.Fatalf("clean count returned err: %v", err)
	} else if count != 0 {
		t.Fatalf("clean tree count = %d, want 0", count)
	}

	// Tracked modification (unstaged), staged add, and untracked file.
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("modify tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	testutil.RunGit(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	count, err := core.CountWorkingTreeChanges(repo)
	if err != nil {
		t.Fatalf("dirty count returned err: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3 (1 unstaged + 1 staged + 1 untracked)", count)
	}
}

func TestCountUnpushedCommitsReportsNoUpstreamWhenUnconfigured(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	count, hasUpstream, err := core.CountUnpushedCommits(repo, "main")
	if err != nil {
		t.Fatalf("CountUnpushedCommits: %v", err)
	}
	if hasUpstream {
		t.Errorf("hasUpstream = true on a brand-new repo with no remote; want false")
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 when no upstream is configured", count)
	}
}

func TestCountUnpushedCommitsRejectsFlagShapedBranch(t *testing.T) {
	core := NewCore()

	_, _, err := core.CountUnpushedCommits(t.TempDir(), "--objects")
	if err == nil {
		t.Fatal("expected flag-shaped branch name to be rejected")
	}
	if !strings.Contains(err.Error(), "must not start with -") {
		t.Fatalf("error = %q, want branch validation failure", err)
	}
}

func TestCountUnpushedCommitsCountsCommitsAhead(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// Build a bare remote so the test doesn't need network. Configure
	// origin and push main so it has a tracked upstream.
	bare := t.TempDir()
	testutil.RunGit(t, bare, "init", "--bare", "-b", "main")
	testutil.RunGit(t, repo, "remote", "add", "origin", bare)
	testutil.RunGit(t, repo, "push", "-u", "origin", "main")

	// Establish baseline: 0 ahead.
	count, hasUpstream, err := core.CountUnpushedCommits(repo, "main")
	if err != nil {
		t.Fatalf("baseline count: %v", err)
	}
	if !hasUpstream {
		t.Fatal("expected hasUpstream=true after push -u")
	}
	if count != 0 {
		t.Fatalf("baseline count = %d, want 0", count)
	}

	// Add two unpushed commits.
	for i := range 2 {
		path := filepath.Join(repo, fmt.Sprintf("ahead-%d.txt", i))
		if err := os.WriteFile(path, []byte("ahead\n"), 0o644); err != nil {
			t.Fatalf("write ahead file: %v", err)
		}
		testutil.RunGit(t, repo, "add", filepath.Base(path))
		testutil.RunGit(t, repo, "commit", "-m", fmt.Sprintf("ahead %d", i))
	}

	count, hasUpstream, err = core.CountUnpushedCommits(repo, "main")
	if err != nil {
		t.Fatalf("ahead count: %v", err)
	}
	if !hasUpstream {
		t.Fatal("expected hasUpstream=true with origin still configured")
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 unpushed commits", count)
	}
}

func TestCountUnpushedCommitsSeparatesBranchNameFromPath(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	core := NewCore()

	const branch = "spike/claude-mitm"
	testutil.RunGit(t, repo, "checkout", "-b", branch)

	collidingPath := filepath.Join(repo, filepath.FromSlash(branch))
	if err := os.MkdirAll(collidingPath, 0o755); err != nil {
		t.Fatalf("create colliding path: %v", err)
	}
	readmePathspec := branch + "/README.md"
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(readmePathspec)), []byte("path with the same name as the branch\n"), 0o644); err != nil {
		t.Fatalf("write colliding path file: %v", err)
	}
	testutil.RunGit(t, repo, "add", readmePathspec)
	testutil.RunGit(t, repo, "commit", "-m", "add branch path collision")
	testutil.RunGit(t, repo, "push", "-u", "origin", branch)

	unpushedPathspec := branch + "/unpushed.md"
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(unpushedPathspec)), []byte("unpushed\n"), 0o644); err != nil {
		t.Fatalf("write unpushed path file: %v", err)
	}
	testutil.RunGit(t, repo, "add", unpushedPathspec)
	testutil.RunGit(t, repo, "commit", "-m", "add unpushed collision file")

	count, hasUpstream, err := core.CountUnpushedCommits(repo, branch)
	if err != nil {
		t.Fatalf("CountUnpushedCommits: %v", err)
	}
	if !hasUpstream {
		t.Fatal("expected hasUpstream=true with pushed tracking branch")
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 unpushed commit", count)
	}
}

func TestUpstreamForReturnsFalseWhenNoUpstream(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if _, ok := core.upstreamFor(repo, "main"); ok {
		t.Fatal("expected upstreamFor to return ok=false on a repo with no remote")
	}
}

// gitStdout runs git in repo and returns stdout, tolerating the exit code 1
// that `git diff --no-index` uses to mean "the inputs differ".
func gitStdout(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}
	return string(out)
}

func numstatTotals(numstat string) (insertions, deletions int) {
	for _, e := range parseNumstat(numstat) {
		insertions += e.Insertions
		deletions += e.Deletions
	}
	return insertions, deletions
}

func splitNUL(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// countLines returns the number of non-empty newline-separated lines in s,
// used to count files in `git ... --name-only` output for FileCount oracles.
func countLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			n++
		}
	}
	return n
}

// untrackedOracle independently computes the untracked-insertion total the diff
// panel would show, straight from git: each untracked, non-ignored file diffed
// against /dev/null (git counts a symlink's link text, and a binary file as 0).
func untrackedOracle(t *testing.T, repo string) int {
	t.Helper()
	total := 0
	for _, rel := range splitNUL(gitStdout(t, repo, "ls-files", "--others", "--exclude-standard", "-z")) {
		ins, _ := numstatTotals(gitStdout(t, repo,
			"diff", "--no-index", "--numstat", "--minimal", "--no-ext-diff", "--no-textconv", "--", os.DevNull, rel))
		total += ins
	}
	return total
}

// countPatchAddsDels mirrors the frontend patch parser
// (frontend/src/lib/utils/patchFiles.ts): a '+'/'-' line that is not a
// '+++'/'---' file header is an addition/deletion. This is how the diff panel
// turns DiffWorkspaceVsHead's unified patch into the +/- totals it displays.
func countPatchAddsDels(patch string) (insertions, deletions int) {
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			insertions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			deletions++
		}
	}
	return insertions, deletions
}

// panelWorkspaceTotal computes the insertions/deletions the diff panel displays
// for the workspace by replicating DiffWorkspaceVsHead's exact git commands
// (tracked patch vs HEAD + a per-untracked-file --no-index patch) and parsing
// them the way the frontend does (countPatchAddsDels). It is a deliberately
// different code path from the badge (which uses --numstat + countAddedLines),
// so asserting badge == panelWorkspaceTotal closes the badge↔panel gap that a
// numstat-only oracle leaves open. A no-HEAD repo's `git diff HEAD` exits
// non-zero with empty stdout → zero tracked churn, mirroring the badge.
func panelWorkspaceTotal(t *testing.T, repo string) (insertions, deletions int) {
	t.Helper()
	insertions, deletions = countPatchAddsDels(gitStdout(t, repo, "diff", "--patch",
		"--minimal", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD", "--"))
	for _, rel := range splitNUL(gitStdout(t, repo, "ls-files", "--others", "--exclude-standard", "-z")) {
		ui, ud := countPatchAddsDels(gitStdout(t, repo, "diff", "--no-index", "--patch",
			"--minimal", "--no-color", "--no-ext-diff", "--no-textconv", "--", os.DevNull, rel))
		insertions += ui
		deletions += ud
	}
	return insertions, deletions
}

// The discard loss preview names files, so the parser has to survive the paths
// git would otherwise quote and escape, and must not count a rename twice.
func TestWorkingTreeChangesNamesAwkwardPathsAndCountsRenamesOnce(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if paths, total, err := core.WorkingTreeChanges(repo, 5); err != nil {
		t.Fatalf("clean tree: %v", err)
	} else if total != 0 || len(paths) != 0 {
		t.Fatalf("clean tree = %d changes %v, want none", total, paths)
	}

	const spaced = "a file with spaces.txt"
	const unicode = "réponse.txt"
	for _, name := range []string{spaced, unicode} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("seed\n"), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-m", "awkward names")
	// One rename (staged, two porcelain fields) and one untracked add.
	testutil.RunGit(t, repo, "mv", spaced, "renamed file.txt")
	if err := os.WriteFile(filepath.Join(repo, unicode), []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("modify unicode file: %v", err)
	}

	paths, total, err := core.WorkingTreeChanges(repo, 10)
	if err != nil {
		t.Fatalf("WorkingTreeChanges: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d (%v), want 2: a rename is one change, not two", total, paths)
	}
	joined := strings.Join(paths, "|")
	for _, want := range []string{"renamed file.txt", unicode} {
		if !strings.Contains(joined, want) {
			t.Fatalf("paths %v are missing %q; git quoting leaked into the preview", paths, want)
		}
	}
	if strings.Contains(joined, `\`) || strings.Contains(joined, `"`) {
		t.Fatalf("paths %v are quoted; a preview must show the real filename", paths)
	}
	// The count stays exact when the sample is capped, and a zero limit is the
	// count-only form CountWorkingTreeChanges is built on.
	if capped, cappedTotal, err := core.WorkingTreeChanges(repo, 1); err != nil {
		t.Fatalf("capped: %v", err)
	} else if len(capped) != 1 || cappedTotal != 2 {
		t.Fatalf("capped = %d names of %d, want 1 of 2", len(capped), cappedTotal)
	}
	if none, noneTotal, err := core.WorkingTreeChanges(repo, 0); err != nil {
		t.Fatalf("count only: %v", err)
	} else if len(none) != 0 || noneTotal != 2 {
		t.Fatalf("count-only = %d names of %d, want 0 of 2", len(none), noneTotal)
	}
	if count, err := core.CountWorkingTreeChanges(repo); err != nil || count != 2 {
		t.Fatalf("CountWorkingTreeChanges = %d, %v; want 2", count, err)
	}
}

func TestDeleteBranchForcesUnmergedAndIsIdempotent(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	testutil.RunGit(t, repo, "checkout", "-b", "unlanded")
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("unlanded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "add", "work.txt")
	testutil.RunGit(t, repo, "commit", "-m", "unlanded work")
	testutil.RunGit(t, repo, "checkout", "main")

	// -d refuses a branch whose commits never landed, which is every branch a
	// discard is asked to delete.
	if err := core.DeleteBranch(repo, "unlanded", false); err == nil {
		t.Fatal("DeleteBranch without force accepted an unmerged branch")
	}
	if err := core.DeleteBranch(repo, "unlanded", true); err != nil {
		t.Fatalf("DeleteBranch force: %v", err)
	}
	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range branches {
		if branch.Name == "unlanded" {
			t.Fatal("forced delete left the branch behind")
		}
	}
	// Deleting again is a no-op: a tree whose branch was already released must
	// not fail the discard that covers it.
	if err := core.DeleteBranch(repo, "unlanded", true); err != nil {
		t.Fatalf("DeleteBranch on an absent branch: %v", err)
	}
	if err := core.DeleteBranch(repo, "  ", true); err == nil {
		t.Fatal("DeleteBranch accepted an empty branch name")
	}
	if err := core.DeleteBranch(repo, "--upload-pack=evil", true); err == nil {
		t.Fatal("DeleteBranch accepted a flag-shaped branch name")
	}
}
