package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestParseBranchList(t *testing.T) {
	// Inputs cover every behavior the parser is responsible for:
	//   - "main"            local, current, default, ahead 3 behind 2
	//   - "feature/demo"    local with no remote counterpart
	//   - "origin"          bare remote name (origin/HEAD symref collapsed
	//                       by refname:short); MUST be dropped
	//   - "origin/main"     remote shadow of a local branch; MUST be dropped
	//   - "origin/feature/demo"  remote shadow of a local branch; MUST be dropped
	//   - "origin/orphan"   remote-only branch; MUST appear as "orphan"
	//   - "origin/HEAD"     standard symref form (some git versions); MUST be dropped
	branches := parseBranchList(
		"main|*|/tmp/repo|ahead 3, behind 2\n"+
			"feature/demo| ||\n"+
			"origin| ||\n"+
			"origin/main| ||\n"+
			"origin/feature/demo| ||\n"+
			"origin/orphan| ||\n"+
			"origin/HEAD| ||\n",
		"main",
		[]string{"origin"},
	)

	var names []string
	byName := make(map[string]GitBranch)
	for _, b := range branches {
		names = append(names, b.Name)
		byName[b.Name] = b
	}

	want := []string{"main", "feature/demo", "orphan"}
	if len(names) != len(want) {
		t.Fatalf("branches = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("branches[%d] = %q, want %q (full list: %v)", i, names[i], n, names)
		}
	}

	if !byName["main"].IsCurrent {
		t.Fatal("expected main to be current")
	}
	if !byName["main"].IsDefault {
		t.Fatal("expected main to be default")
	}
	if byName["main"].AheadCount != 3 || byName["main"].BehindCount != 2 {
		t.Fatalf("main ahead/behind = %d/%d, want 3/2", byName["main"].AheadCount, byName["main"].BehindCount)
	}
	if byName["feature/demo"].IsDefault {
		t.Fatal("expected feature/demo not to be default")
	}
	if byName["feature/demo"].IsCurrent {
		t.Fatal("expected feature/demo not to be current")
	}
	if byName["feature/demo"].AheadCount != 0 || byName["feature/demo"].BehindCount != 0 {
		t.Fatalf("feature/demo ahead/behind = %d/%d, want 0/0 (no upstream configured)", byName["feature/demo"].AheadCount, byName["feature/demo"].BehindCount)
	}
	if byName["orphan"].IsDefault {
		t.Fatal("expected orphan not to be default")
	}
	if byName["orphan"].AheadCount != 0 || byName["orphan"].BehindCount != 0 {
		t.Fatalf("orphan ahead/behind = %d/%d, want 0/0", byName["orphan"].AheadCount, byName["orphan"].BehindCount)
	}
}

func TestParseBranchListPreservesLocalNamedLikeBranch(t *testing.T) {
	// A local branch literally named "feature/HEAD" should pass through
	// (only remote-namespaced HEAD symrefs are dropped).
	branches := parseBranchList("feature/HEAD| ||\nfeature/regular| ||\n", "main", []string{"origin"})
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d: %+v", len(branches), branches)
	}
}

func TestParseUpstreamTrack(t *testing.T) {
	tests := []struct {
		in     string
		ahead  int
		behind int
	}{
		{"", 0, 0},
		{"gone", 0, 0},
		{"ahead 3", 3, 0},
		{"behind 2", 0, 2},
		{"ahead 3, behind 2", 3, 2},
		{"behind 2, ahead 3", 3, 2},
		{"  ahead 7  ", 7, 0},
		{"ahead notanumber", 0, 0},
		// Defensive — truncated / extended output forms must not panic
		// or produce garbage counts.
		{"ahead", 0, 0},
		{"ahead 3 extra", 0, 0},
		{",", 0, 0},
	}
	for _, tt := range tests {
		ahead, behind := parseUpstreamTrack(tt.in)
		if ahead != tt.ahead || behind != tt.behind {
			t.Errorf("parseUpstreamTrack(%q) = %d/%d, want %d/%d", tt.in, ahead, behind, tt.ahead, tt.behind)
		}
	}
}

func TestParseBranchListProjectsRemoteOnlyDefault(t *testing.T) {
	// When the default branch only exists on the remote (fresh clone with
	// no local checkout yet of main), the projected "main" entry must
	// still be flagged as default so the picker keeps the badge.
	branches := parseBranchList(
		"feature| ||\norigin/main| ||\n",
		"main",
		[]string{"origin"},
	)
	if len(branches) != 2 {
		t.Fatalf("len(branches) = %d, want 2", len(branches))
	}
	var main GitBranch
	for _, b := range branches {
		if b.Name == "main" {
			main = b
		}
	}
	if main.Name != "main" {
		t.Fatalf("expected projected main branch, got names: %+v", branches)
	}
	if !main.IsDefault {
		t.Fatal("expected projected main to be default")
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

func TestListBranchesOnRepository(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected at least one branch")
	}
	if branches[0].Name == "" {
		t.Fatal("expected branch name to be populated")
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

// seedForgeCacheGitHub is a test-only helper that populates the forge
// classification cache so Core.lookupOpenPR's dispatch resolves to the
// github forge without requiring the test to set up a real origin URL.
// The Core.forgeFor call would otherwise return nullForge for a bare
// t.TempDir() (no origin remote) and short-circuit gh invocation.
func seedForgeCacheGitHub(t *testing.T, core *Core, cwd string) {
	t.Helper()
	core.storeForgeCache(cwd, "github", core.nowFn())
}

func TestLookupOpenPRUsesGHWhenAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PATH override in short mode")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho '[{\"url\":\"https://example.com/pr/7\",\"number\":7,\"title\":\"Demo PR\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	url, number := core.lookupOpenPR(cwd, "main")
	if url != "https://example.com/pr/7" {
		t.Fatalf("url = %q, want https://example.com/pr/7", url)
	}
	if number != 7 {
		t.Fatalf("number = %d, want 7", number)
	}
}

// TestLookupOpenPRCachesResults pins the perf optimisation that
// repeated lookups on the same (cwd, branch) inside the TTL window do
// NOT shell out again. Without the cache, gitwatch's hot path would
// translate every fs-event-debounce into a `gh pr list` round-trip.
func TestLookupOpenPRCachesResults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "calls")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\nprintf x >> " + counterFile + "\necho '[{\"url\":\"https://example.com/pr/9\",\"number\":9,\"title\":\"x\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	// First call: cold cache → shell out.
	if url, _ := core.lookupOpenPR(cwd, "feat-a"); url == "" {
		t.Fatalf("cold lookup returned empty url")
	}
	// Subsequent calls within TTL: warm cache → no shell out.
	for i := 0; i < 5; i++ {
		if url, _ := core.lookupOpenPR(cwd, "feat-a"); url == "" {
			t.Fatalf("warm lookup #%d returned empty url", i)
		}
	}
	calls, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got := len(calls); got != 1 {
		t.Fatalf("gh invocations = %d, want 1 (cache should absorb 5 follow-up calls)", got)
	}

	// Different branch → different cache key → fresh shell out.
	if url, _ := core.lookupOpenPR(cwd, "feat-b"); url == "" {
		t.Fatalf("different-branch lookup returned empty url")
	}
	calls, _ = os.ReadFile(counterFile)
	if got := len(calls); got != 2 {
		t.Fatalf("after different-branch lookup: gh invocations = %d, want 2", got)
	}

	// TTL expiry → fresh shell out. Drive nowFn forward past the TTL.
	core.nowFn = func() time.Time { return time.Now().Add(prLookupTTL + time.Second) }
	if url, _ := core.lookupOpenPR(cwd, "feat-a"); url == "" {
		t.Fatalf("post-TTL lookup returned empty url")
	}
	calls, _ = os.ReadFile(counterFile)
	if got := len(calls); got != 3 {
		t.Fatalf("after TTL expiry: gh invocations = %d, want 3", got)
	}
}

// TestInvalidatePRCacheClearsCwdEntries verifies that a successful
// CreatePR can drop the stale "no PR" cached value so the next status
// refresh sees the freshly-opened PR rather than waiting up to 30s.
func TestInvalidatePRCacheClearsCwdEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "calls")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\nprintf x >> " + counterFile + "\necho '[]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	seedForgeCacheGitHub(t, core, cwdA)
	seedForgeCacheGitHub(t, core, cwdB)

	// Seed the cache with a "no PR" answer for two cwds.
	core.lookupOpenPR(cwdA, "main")
	core.lookupOpenPR(cwdB, "main")

	// Invalidate cwdA only — cwdB's cache must be untouched.
	core.InvalidatePRCache(cwdA)

	core.lookupOpenPR(cwdA, "main") // miss → shell out
	core.lookupOpenPR(cwdB, "main") // hit → no shell out

	calls, _ := os.ReadFile(counterFile)
	if got := len(calls); got != 3 {
		t.Fatalf("gh invocations = %d, want 3 (2 seeds + 1 post-invalidate refetch)", got)
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

func TestListBranchesIncludesNewBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/test")

	core := NewCore()
	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}

	found := false
	for _, b := range branches {
		if b.Name == "feature/test" {
			found = true
			if b.IsCurrent {
				t.Fatal("feature/test should not be current")
			}
		}
	}
	if !found {
		t.Fatal("expected feature/test in branch list")
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

func TestIsDefaultBranchNameEdgeCases(t *testing.T) {
	tests := []struct {
		branch string
		dflt   string
		want   bool
	}{
		{"", "main", false},
		{"main", "", true},
		{"master", "", true},
		{"origin/main", "", true},
		{"origin/master", "", true},
		{"develop", "", false},
		{"develop", "develop", true},
		{"origin/develop", "develop", true},
		{"feature/develop", "develop", true}, // HasSuffix("/develop") matches remote-like patterns
	}

	for _, tt := range tests {
		t.Run(tt.branch+"/"+tt.dflt, func(t *testing.T) {
			got := isDefaultBranchName(tt.branch, tt.dflt)
			if got != tt.want {
				t.Fatalf("isDefaultBranchName(%q, %q) = %v, want %v", tt.branch, tt.dflt, got, tt.want)
			}
		})
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

// Bug C3 regression: GetGitStatus must surface in-progress multi-step
// operations (merge/rebase/bisect) so the Ship Changes wizard can disable
// commit and tell the user why.
func TestPendingOperationClean(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "" {
		t.Fatalf("PendingOperation = %q, want empty for idle repo", status.PendingOperation)
	}
}

func TestPendingOperationMerge(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	// Create two diverging branches that touch the same line to force a
	// conflict when we merge them back together.
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("feature side\n"), 0o644); err != nil {
		t.Fatalf("write feature README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "feature change")

	testutil.RunGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("main side\n"), 0o644); err != nil {
		t.Fatalf("write main README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "main change")

	// Merge attempts to combine both sides; the conflict leaves MERGE_HEAD
	// in place until the user resolves or aborts. Expect a non-zero exit
	// code but don't fail the test — we specifically want the mid-merge
	// state.
	_ = testutil.RunGitAllowError(repo, "merge", "--no-commit", "--no-ff", "feature")

	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "merge" {
		t.Fatalf("PendingOperation = %q, want 'merge'", status.PendingOperation)
	}
}

func TestPendingOperationRebase(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	// Set up a conflicting commit on main, then start a rebase of a
	// divergent branch onto main to trigger an unresolved rebase state.
	testutil.RunGit(t, repo, "checkout", "-b", "topic")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("topic side\n"), 0o644); err != nil {
		t.Fatalf("write topic README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "topic change")

	testutil.RunGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("main side\n"), 0o644); err != nil {
		t.Fatalf("write main README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "main change")

	testutil.RunGit(t, repo, "checkout", "topic")
	// Expect the rebase to stop on the conflict — that's what we want.
	_ = testutil.RunGitAllowError(repo, "rebase", "main")

	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "rebase" {
		t.Fatalf("PendingOperation = %q, want 'rebase'", status.PendingOperation)
	}
}

func TestPendingOperationBisect(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	// Create a short history so there's something to bisect.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("file-%d.txt", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		testutil.RunGit(t, repo, "add", name)
		testutil.RunGit(t, repo, "commit", "-m", fmt.Sprintf("add %s", name))
	}

	testutil.RunGit(t, repo, "bisect", "start")
	testutil.RunGit(t, repo, "bisect", "bad")
	// Provide a known-good commit to activate the bisect session.
	testutil.RunGit(t, repo, "bisect", "good", "HEAD~2")

	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "bisect" {
		t.Fatalf("PendingOperation = %q, want 'bisect'", status.PendingOperation)
	}
}

func TestPendingOperationNonRepoReturnsEmpty(t *testing.T) {
	// A non-repo directory must yield an empty pendingOperation — never a
	// false positive, since Status already reports IsRepo=false.
	dir := t.TempDir()
	status, err := NewCore().Status(dir)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.IsRepo {
		t.Fatal("expected IsRepo=false for non-repo dir")
	}
	if status.PendingOperation != "" {
		t.Fatalf("PendingOperation = %q, want empty for non-repo dir", status.PendingOperation)
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

func TestUpstreamForReturnsFalseWhenNoUpstream(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if _, ok := core.upstreamFor(repo, "main"); ok {
		t.Fatal("expected upstreamFor to return ok=false on a repo with no remote")
	}
}

// TestCountAddedLines pins countAddedLines to git's numstat accounting for a
// file added whole versus /dev/null: a trailing-newline-aware line count, with
// git's binary heuristic (a NUL inside the first 8000 bytes) zeroing the count.
// The two boundary cases at exactly 8000 bytes are the load-bearing ones.
func TestCountAddedLines(t *testing.T) {
	// NUL at index 7999 sits *inside* git's 8000-byte probe window → binary → 0.
	nulInProbe := append(bytes.Repeat([]byte("x"), 7999), 0)
	// NUL at index 8000 sits *past* the probe window → treated as text; the
	// 8000 x's plus the unterminated NUL byte are a single line.
	nulPastProbe := append(bytes.Repeat([]byte("x"), 8000), 0)

	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"empty", []byte(""), 0},
		{"trailing newline", []byte("a\nb\nc\n"), 3},
		{"no trailing newline", []byte("a\nb\nc"), 3},
		{"single line no newline", []byte("abc"), 1},
		{"lone newline", []byte("\n"), 1},
		{"nul in content", []byte("a\x00b\n"), 0},
		{"nul inside probe window", nulInProbe, 0},
		{"nul past probe window", nulPastProbe, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countAddedLines(tc.in); got != tc.want {
				t.Fatalf("countAddedLines(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestCountUntrackedFileLines covers the per-file counter that backs the
// untracked-insertion tally: regular files (with/without trailing newline,
// binary), the scan-budget cap, a file that vanished mid-scan, and the
// security-critical symlink case (counted as its link text, never followed).
// The FIFO no-hang guard lives in status_fifo_test.go (needs syscall.Mkfifo).
func TestCountUntrackedFileLines(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	// Regular text file: every newline is a line; bytesRead is the full size.
	if ins, read := countUntrackedFileLines(write("text.txt", "a\nb\nc\n"), maxUntrackedScanBytes); ins != 3 || read != 6 {
		t.Fatalf("text file: got (%d, %d), want (3, 6)", ins, read)
	}
	// No trailing newline still counts the final partial line.
	if ins, read := countUntrackedFileLines(write("tail.txt", "x\ny"), maxUntrackedScanBytes); ins != 2 || read != 3 {
		t.Fatalf("no-trailing-newline: got (%d, %d), want (2, 3)", ins, read)
	}
	// Binary content (NUL in the probe window) → 0 lines, but bytes are read.
	if ins, read := countUntrackedFileLines(write("blob.bin", "\x00\x01\x02"), maxUntrackedScanBytes); ins != 0 || read != 3 {
		t.Fatalf("binary file: got (%d, %d), want (0, 3)", ins, read)
	}
	// A file that vanished mid-scan (Lstat fails) degrades to (0, 0).
	if ins, read := countUntrackedFileLines(filepath.Join(dir, "gone.txt"), maxUntrackedScanBytes); ins != 0 || read != 0 {
		t.Fatalf("missing file: got (%d, %d), want (0, 0)", ins, read)
	}
	// Budget cap: only `budget` bytes are read, so trailing lines beyond it are
	// not tallied and bytesRead never exceeds the budget. "a\n" → 1 line, 2 bytes.
	if ins, read := countUntrackedFileLines(write("capped.txt", "a\nb\nc\n"), 2); ins != 1 || read != 2 {
		t.Fatalf("budget cap: got (%d, %d), want (1, 2)", ins, read)
	}

	// A symlink is counted as its target *text* (one line), never followed —
	// the security/parity fix. "text.txt" is an 8-char link with no newline.
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("text.txt", link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if ins, read := countUntrackedFileLines(link, maxUntrackedScanBytes); ins != 1 || read != 0 {
		t.Fatalf("symlink: got (%d, %d), want (1, 0) — link text counted, target not opened", ins, read)
	}
}

// TestUntrackedStatsBudgetExhaustion proves the whole-scan byte budget caps the
// line tally without ever capping the file count: once budget is spent, every
// remaining untracked file still increments FileCount but contributes zero
// insertions. That is the contract keeping a single huge un-ignored file (a
// forgotten build/data dir, a multi-GB log) from zeroing the count the user
// sees. The const's generous default makes this impractical to hit with a real
// fixture, which is why untrackedStats takes budget as a parameter.
func TestUntrackedStatsBudgetExhaustion(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// Three identical untracked files, 3 lines / 6 bytes each. Identical content
	// makes the result independent of ls-files iteration order.
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeRepoFile(t, repo, name, "1\n2\n3\n")
	}

	// Full budget: every file is read and tallied — 3 files x 3 lines.
	if ins, files := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 9 || files != 3 {
		t.Fatalf("full budget: got (%d insertions, %d files), want (9, 3)", ins, files)
	}

	// Tiny budget: the first file's 1-byte read spends the whole budget, so files
	// 2 and 3 hit the budget<=0 short-circuit — counted but not tallied. The file
	// count must survive; only the line tally stops.
	if ins, files := core.untrackedStats(repo, 1); ins != 1 || files != 3 {
		t.Fatalf("budget exhausted: got (%d insertions, %d files), want (1, 3) — "+
			"file count must survive budget exhaustion", ins, files)
	}
}

// TestCountAddedLinesMatchesPanelParse pins countAddedLines (the badge's
// untracked counter) to the diff panel's per-file count: the frontend's parse
// (countPatchAddsDels, mirroring patchFiles.ts) of git's own `--no-index --patch`
// output. numstat agreement is not enough — the panel counts the patch, not
// numstat — so this is the test that proves the badge matches what the user sees
// for each shape of untracked file. A content line starting with a single '+' is
// included because the patch prefixes it to "++…", and both sides must still
// count it (only the '+++' header is excluded).
func TestCountAddedLinesMatchesPanelParse(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	cases := []struct{ name, content string }{
		{"plain", "a\nb\nc\n"},
		{"no_trailing_newline", "x\ny"},
		{"binary", "\x00\x01\x02binary"},
		{"single_line", "only\n"},
		{"content_line_starting_with_plus", "+added\nplain\n"},
		{"empty", ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := fmt.Sprintf("f%d.txt", i)
			writeRepoFile(t, repo, rel, tc.content)
			panelIns, _ := countPatchAddsDels(gitStdout(t, repo, "diff", "--no-index",
				"--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv",
				"--", os.DevNull, rel))
			if got := countAddedLines([]byte(tc.content)); got != panelIns {
				t.Fatalf("%s: countAddedLines = %d, panel parse = %d — badge and panel diverge for %q",
					tc.name, got, panelIns, tc.content)
			}
		})
	}
}

// TestStatusUntrackedSymlinkNotFollowed guards the security/parity fix: an
// untracked symlink must be counted as git counts it — its 1-line target text —
// NOT by following the link and counting the target file's contents (which would
// read outside the workspace, could hang on a FIFO target, and diverge from the
// diff panel).
func TestStatusUntrackedSymlinkNotFollowed(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// 5-line target; if the scan followed the link it would count these 5 for
	// the link too.
	writeRepoFile(t, repo, "target.txt", "1\n2\n3\n4\n5\n")
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	// target.txt (5 lines) + link.txt (1 line of target-path text) = 6.
	// Following the link would instead give 5 + 5 = 10.
	if status.Insertions != 6 {
		t.Fatalf("Status.Insertions = %d, want 6 (target 5 + symlink-as-link-text 1); "+
			"a value of 10 means the symlink was followed", status.Insertions)
	}
	// Version-robust backstop: match git itself, whatever it reports per entry.
	if want := untrackedOracle(t, repo); status.Insertions != want {
		t.Fatalf("Status.Insertions = %d, want %d (git oracle)", status.Insertions, want)
	}
}

// TestStatusInsertionsNoHeadRepo covers the fresh-repo path: `git diff HEAD`
// has no HEAD to diff against (exits non-zero, empty stdout) so tracked churn is
// zero — like the panel — while untracked files are still counted, and Status
// must not error.
func TestStatusInsertionsNoHeadRepo(t *testing.T) {
	repo := t.TempDir()
	if err := testutil.RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		testutil.RunGit(t, repo, "init")
	}
	testutil.RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")
	testutil.RunGit(t, repo, "config", "user.name", "Agent Overflow")
	core := NewCore()

	writeRepoFile(t, repo, "a.txt", "one\ntwo\n") // 2 lines
	writeRepoFile(t, repo, "b.txt", "x\ny\nz\n")  // 3 lines

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status on no-HEAD repo returned error: %v", err)
	}
	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	want := untrackedOracle(t, repo)
	if want == 0 {
		t.Fatal("fixture produced no untracked insertions")
	}
	if status.Insertions != want {
		t.Fatalf("Status.Insertions = %d, want %d (untracked-only, no tracked churn without HEAD)",
			status.Insertions, want)
	}
	if status.Deletions != 0 {
		t.Fatalf("Status.Deletions = %d, want 0", status.Deletions)
	}
	// No HEAD → no tracked changes; the two untracked files are the whole count.
	if status.FileCount != 2 {
		t.Fatalf("Status.FileCount = %d, want 2 (a.txt + b.txt, untracked)", status.FileCount)
	}
}

// TestStatusInsertionsIncludeUntracked is the regression guard for the badge /
// panel divergence: Status.Insertions must equal the number the diff panel
// computes (tracked churn vs HEAD plus every untracked file counted as
// all-insertions), proven against git itself as an independent oracle on a
// fixture that exercises binary, no-trailing-newline, and nested-dir files.
func TestStatusInsertionsIncludeUntracked(t *testing.T) {
	repo := testutil.InitGitRepo(t) // README.txt = "hello\n", committed
	core := NewCore()

	// Tracked edit: rewrite the committed file so there is real churn (and a
	// deletion) to fold in alongside the untracked content.
	writeRepoFile(t, repo, "README.txt", "hello world\nsecond\nthird\n")

	// Untracked files of every shape the counter has to get right. sub/ holds
	// two files and is wholly untracked, so `git status` collapses it to one
	// porcelain entry — the dir-collapse case that the FileCount fix must not
	// undercount (ls-files enumerates both).
	writeRepoFile(t, repo, "new.txt", "a\nb\nc\n")                          // 3 lines
	writeRepoFile(t, repo, "tail.txt", "x\ny")                              // 2 lines, no trailing newline
	writeRepoFile(t, repo, "blob.bin", "\x00\x01\x02binary")                // binary → 0 lines
	writeRepoFile(t, repo, filepath.Join("sub", "nested.txt"), "n1\nn2\n")  // 2 lines, nested dir
	writeRepoFile(t, repo, filepath.Join("sub", "nested2.txt"), "m1\nm2\n") // 2 lines, same wholly-untracked dir

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	// Oracle: the same numbers the panel derives, straight from git.
	wantTrackedIns, wantTrackedDel := numstatTotals(gitStdout(t, repo,
		"diff", "--numstat", "--minimal", "--no-ext-diff", "--no-textconv", "HEAD", "--"))
	wantUntrackedIns := untrackedOracle(t, repo)

	if wantUntrackedIns == 0 {
		t.Fatal("fixture produced no untracked insertions; the new path is not being exercised")
	}
	wantIns := wantTrackedIns + wantUntrackedIns
	if status.Insertions != wantIns {
		t.Fatalf("Status.Insertions = %d, want %d (tracked %d + untracked %d)",
			status.Insertions, wantIns, wantTrackedIns, wantUntrackedIns)
	}
	if status.Deletions != wantTrackedDel {
		t.Fatalf("Status.Deletions = %d, want %d", status.Deletions, wantTrackedDel)
	}
	// The original bug was Status.Insertions == tracked-only; assert it now
	// genuinely exceeds that because untracked content is included.
	if status.Insertions <= wantTrackedIns {
		t.Fatalf("Status.Insertions = %d did not include untracked content (tracked = %d)",
			status.Insertions, wantTrackedIns)
	}

	// FileCount must match the panel too: tracked-changed files plus every
	// individual untracked file. The oracle counts untracked files via ls-files
	// (which enumerates sub/nested.txt and sub/nested2.txt separately) — the
	// previous porcelain-based count collapsed sub/ to one entry and undercounted.
	wantFiles := countLines(gitStdout(t, repo, "diff", "HEAD", "--name-only")) +
		len(splitNUL(gitStdout(t, repo, "ls-files", "--others", "--exclude-standard", "-z")))
	if wantFiles != 6 {
		t.Fatalf("fixture sanity: oracle wantFiles = %d, want 6 (1 tracked README + 5 untracked, sub/ counting as 2)", wantFiles)
	}
	if status.FileCount != wantFiles {
		t.Fatalf("Status.FileCount = %d, want %d (tracked-changed + individual untracked files)", status.FileCount, wantFiles)
	}

	// The definitive check for the original complaint (badge ≠ panel): the badge
	// must equal what the panel actually shows. panelWorkspaceTotal parses the
	// same patch DiffWorkspaceVsHead produces using the frontend's algorithm — a
	// separate code path from the badge's numstat + countAddedLines — so this
	// closes the transitivity gap a numstat-only oracle would leave. The fixture
	// avoids content lines beginning with '+++'/'---' (the one case where the
	// frontend prefix parser undercounts and the badge is the more accurate).
	panelIns, panelDel := panelWorkspaceTotal(t, repo)
	if status.Insertions != panelIns || status.Deletions != panelDel {
		t.Fatalf("badge (+%d -%d) != panel (+%d -%d) — the badge must match the diff panel",
			status.Insertions, status.Deletions, panelIns, panelDel)
	}
}

func writeRepoFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
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
