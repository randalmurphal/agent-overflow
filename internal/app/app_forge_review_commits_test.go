package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// prCloneFixture builds an "origin" repo carrying refs/pull/5/head with
// two commits beyond main, clones it, and registers the clone as a project —
// the shape ListPRCommits/GetPRCommitDiff resolve through localCloneWorkspace.
func prCloneFixture(t *testing.T, app *App) (ref WorkspaceRef, clone string, prSHAs []string) {
	t.Helper()

	origin := testutil.InitGitRepo(t)
	testutil.RunGit(t, origin, "checkout", "-b", "feature")
	for _, name := range []string{"first.txt", "second.txt"} {
		path := filepath.Join(origin, name)
		if err := os.WriteFile(path, []byte(name+" content\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		testutil.RunGit(t, origin, "add", name)
		testutil.RunGit(t, origin, "commit", "-m", "pr commit "+name)
	}
	shaOut, _, err := gitops.NewCore().Execute(origin, "rev-list", "main..feature")
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	prSHAs = strings.Fields(shaOut) // newest first
	testutil.RunGit(t, origin, "update-ref", "refs/pull/5/head", prSHAs[0])
	testutil.RunGit(t, origin, "checkout", "main")

	clone = filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, origin, "clone", origin, clone)

	return testWorkspaceRef(t, app, clone), clone, prSHAs
}

func prRef() gitops.PRReference {
	return gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 5}
}

func TestListPRCommitsFromLocalClone(t *testing.T) {
	app := newTestAppWithStore(t)
	ref, _, prSHAs := prCloneFixture(t, app)

	commits, err := app.ListPRCommits(ref, prRef(), "main", "")
	if err != nil {
		t.Fatalf("ListPRCommits() error = %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 PR commits, got %d: %+v", len(commits), commits)
	}
	if commits[0].SHA != prSHAs[0] || commits[1].SHA != prSHAs[1] {
		t.Fatalf("expected newest-first %v, got %+v", prSHAs, commits)
	}
	if commits[0].Subject != "pr commit second.txt" {
		t.Fatalf("newest subject = %q", commits[0].Subject)
	}
}

func TestListPRCommitsWithoutCloneReturnsEmpty(t *testing.T) {
	app := newTestAppWithStore(t)
	noClone := testWorkspaceRef(t, app, t.TempDir()) // not a git repo

	commits, err := app.ListPRCommits(noClone, prRef(), "main", "")
	if err != nil {
		t.Fatalf("ListPRCommits() error = %v", err)
	}
	if commits == nil || len(commits) != 0 {
		t.Fatalf("expected empty non-nil list without a clone, got %#v", commits)
	}
}

func TestListPRCommitsWithKnownHeadSkipsFetch(t *testing.T) {
	app := newTestAppWithStore(t)
	ref, clone, prSHAs := prCloneFixture(t, app)
	// Break the remote: any fetch now fails, so a passing listing proves
	// the known-head fast path skipped the network entirely.
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	commits, err := app.ListPRCommits(ref, prRef(), "main", prSHAs[0])
	if err != nil {
		t.Fatalf("ListPRCommits() with known head error = %v", err)
	}
	if len(commits) != 2 || commits[0].SHA != prSHAs[0] {
		t.Fatalf("expected the local listing from the known head, got %+v", commits)
	}

	// GetPRCommitDiff takes the same no-fetch fast path when the commit
	// is already local.
	patch, err := app.GetPRCommitDiff(ref, prRef(), prSHAs[0], false)
	if err != nil {
		t.Fatalf("GetPRCommitDiff() with local commit error = %v", err)
	}
	if !strings.Contains(patch, "+second.txt content") {
		t.Fatalf("expected the commit diff from local objects, got:\n%s", patch)
	}
}

func TestListPRCommitsRequiresBaseRef(t *testing.T) {
	app := newTestAppWithStore(t)
	ref, _, _ := prCloneFixture(t, app)

	if _, err := app.ListPRCommits(ref, prRef(), "  ", ""); err == nil {
		t.Fatal("expected error for an empty base ref")
	}
}

func TestGetPRCommitDiffFromLocalClone(t *testing.T) {
	app := newTestAppWithStore(t)
	ref, _, prSHAs := prCloneFixture(t, app)

	patch, err := app.GetPRCommitDiff(ref, prRef(), prSHAs[0], false)
	if err != nil {
		t.Fatalf("GetPRCommitDiff() error = %v", err)
	}
	if !strings.Contains(patch, "+second.txt content") {
		t.Fatalf("expected the newest commit's own addition, got:\n%s", patch)
	}
	if strings.Contains(patch, "first.txt content") {
		t.Fatalf("single-commit diff must not include the earlier commit:\n%s", patch)
	}
}

func TestGetPRCommitDiffWithoutCloneErrors(t *testing.T) {
	app := newTestAppWithStore(t)
	noClone := testWorkspaceRef(t, app, t.TempDir())

	if _, err := app.GetPRCommitDiff(noClone, prRef(), strings.Repeat("a", 40), false); err == nil {
		t.Fatal("expected error without a local clone")
	}
}
