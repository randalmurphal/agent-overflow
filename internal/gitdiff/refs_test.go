package gitdiff

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// unpushedMainClone builds the shape the remote-tracking base rule exists
// for: a clone whose local `main` carries a commit origin has never seen,
// and a `feature` branch cut from that local tip.
//
//	origin/main   C0
//	main          C0 - C1        ("local main work", never pushed)
//	feature       C0 - C1 - C2   ("feature work")
func unpushedMainClone(t *testing.T) string {
	t.Helper()
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	commitFile(t, repo, "main-only.txt", "local main work\n", "local main work")
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "feature.txt", "feature work\n", "feature work")
	return repo
}

func TestResolveBaseRefPrefersTheRemoteTrackingRef(t *testing.T) {
	repo, _ := testutil.InitGitRepoWithOrigin(t)

	got, err := resolveBaseRef(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	if got != "origin/main" {
		t.Fatalf("resolveBaseRef(main) = %q, want origin/main", got)
	}
}

func TestResolveBaseRefFallsBackToTheLocalBranch(t *testing.T) {
	// No remote at all, and a remote-less repo must behave exactly as it
	// did before the preference existed.
	repo := testutil.InitGitRepo(t)
	got, err := resolveBaseRef(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	if got != "main" {
		t.Fatalf("resolveBaseRef(main) = %q, want main", got)
	}

	// A remote exists but has never seen this branch — same answer.
	withOrigin, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, withOrigin, "branch", "local-only")
	got, err = resolveBaseRef(context.Background(), withOrigin, "local-only")
	if err != nil {
		t.Fatalf("resolveBaseRef(local-only): %v", err)
	}
	if got != "local-only" {
		t.Fatalf("resolveBaseRef(local-only) = %q, want local-only", got)
	}
}

func TestResolveBaseRefHonorsAConfiguredUpstreamOverOrigin(t *testing.T) {
	// A fork: `origin` is the user's copy, `upstream` is what PRs target,
	// and the local main tracks upstream.
	fork, _ := testutil.InitGitRepoWithOrigin(t)
	upstreamBare := t.TempDir()
	testutil.RunGit(t, upstreamBare, "init", "--bare", "-b", "main")
	testutil.RunGit(t, fork, "remote", "add", "upstream", upstreamBare)
	testutil.RunGit(t, fork, "push", "upstream", "main")
	testutil.RunGit(t, fork, "fetch", "upstream")
	testutil.RunGit(t, fork, "branch", "--set-upstream-to=upstream/main", "main")

	got, err := resolveBaseRef(context.Background(), fork, "main")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	if got != "upstream/main" {
		t.Fatalf("resolveBaseRef(main) = %q, want upstream/main", got)
	}
}

func TestResolveBaseRefIgnoresALocalBranchUpstream(t *testing.T) {
	// `branch.<name>.remote = .` makes the upstream another LOCAL branch.
	// Treating that as a remote-tracking ref would re-point the diff base
	// at a branch that has nothing to do with the remote.
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "branch", "dev")
	testutil.RunGit(t, repo, "config", "branch.dev.remote", ".")
	testutil.RunGit(t, repo, "config", "branch.dev.merge", "refs/heads/main")

	got, err := resolveBaseRef(context.Background(), repo, "dev")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	if got != "dev" {
		t.Fatalf("resolveBaseRef(dev) = %q, want dev (the local-branch upstream must be ignored)", got)
	}
}

func TestResolveBaseRefKeepsAnExplicitRemoteRef(t *testing.T) {
	// app_forge_review.go passes "origin/<base>" already resolved.
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	got, err := resolveBaseRef(context.Background(), repo, "origin/main")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	if got != "origin/main" {
		t.Fatalf("resolveBaseRef(origin/main) = %q, want origin/main", got)
	}
}

func TestResolveNamedRefPrefersTheLocalBranch(t *testing.T) {
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "pushed.txt", "pushed\n", "pushed commit")
	testutil.RunGit(t, repo, "push", "-u", "origin", "feature")
	commitFile(t, repo, "unpushed.txt", "unpushed\n", "unpushed commit")

	// The base side follows the remote…
	base, err := resolveBaseRef(context.Background(), repo, "feature")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	if base != "origin/feature" {
		t.Fatalf("resolveBaseRef(feature) = %q, want origin/feature", base)
	}
	// …and the described side does not, or unpushed commits vanish.
	named, err := resolveNamedRef(context.Background(), repo, "feature")
	if err != nil {
		t.Fatalf("resolveNamedRef: %v", err)
	}
	if named != "feature" {
		t.Fatalf("resolveNamedRef(feature) = %q, want feature", named)
	}
}

func TestResolveNamedRefResolvesRemoteOnlyBranches(t *testing.T) {
	clone := cloneWithRemoteOnlyBranch(t)
	got, err := resolveNamedRef(context.Background(), clone, "release")
	if err != nil {
		t.Fatalf("resolveNamedRef: %v", err)
	}
	if got != "origin/release" {
		t.Fatalf("resolveNamedRef(release) = %q, want origin/release", got)
	}
}

func TestResolveRefsRejectBadNames(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	for _, name := range []string{"", "  ", "--all"} {
		if _, err := resolveBaseRef(context.Background(), repo, name); err == nil {
			t.Fatalf("resolveBaseRef(%q) accepted a bad ref", name)
		}
		if _, err := resolveNamedRef(context.Background(), repo, name); err == nil {
			t.Fatalf("resolveNamedRef(%q) accepted a bad ref", name)
		}
	}
}

func TestListCommitsMeasuresAgainstTheRemoteBase(t *testing.T) {
	repo := unpushedMainClone(t)

	commits, err := ListCommits(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	subjects := make([]string, 0, len(commits))
	for _, commit := range commits {
		subjects = append(subjects, commit.Subject)
	}
	// Against origin/main, both commits are unlanded work — which is what
	// a PR onto main would carry, and what the forge would show.
	want := []string{"feature work", "local main work"}
	if strings.Join(subjects, "|") != strings.Join(want, "|") {
		t.Fatalf("commits = %v, want %v (base must be origin/main, not the local main)", subjects, want)
	}
}

func TestListCommitsWithoutARemoteUsesTheLocalBase(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "main-only.txt", "local main work\n", "local main work")
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "feature.txt", "feature work\n", "feature work")

	commits, err := ListCommits(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 1 || commits[0].Subject != "feature work" {
		t.Fatalf("commits = %+v, want only the feature commit", commits)
	}
}

func TestDiffBranchBaseToWorktreeMeasuresAgainstTheRemoteBase(t *testing.T) {
	repo := unpushedMainClone(t)
	writeFile(t, repo, "dirty.txt", "uncommitted\n")

	patch, err := DiffBranchBaseToWorktree(context.Background(), repo, "main", Options{})
	if err != nil {
		t.Fatalf("DiffBranchBaseToWorktree: %v", err)
	}
	text := string(patch)
	for _, want := range []string{"main-only.txt", "feature.txt", "dirty.txt"} {
		if !strings.Contains(text, want) {
			t.Fatalf("patch is missing %s; base did not resolve to origin/main:\n%s", want, text)
		}
	}
}

func TestListBranchCommitsCountsUnpushedCommitsOnTheDescribedBranch(t *testing.T) {
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "pushed.txt", "pushed\n", "pushed commit")
	testutil.RunGit(t, repo, "push", "-u", "origin", "feature")
	commitFile(t, repo, "unpushed.txt", "unpushed\n", "unpushed commit")

	commits, err := ListBranchCommits(context.Background(), repo, "main", "feature")
	if err != nil {
		t.Fatalf("ListBranchCommits: %v", err)
	}
	// Deleting this branch would lose BOTH commits. Resolving the
	// described branch to origin/feature would have reported one.
	if len(commits) != 2 {
		t.Fatalf("commits = %+v, want both the pushed and unpushed commit", commits)
	}
	if commits[0].Subject != "unpushed commit" {
		t.Fatalf("newest commit = %q, want the unpushed one", commits[0].Subject)
	}
}
