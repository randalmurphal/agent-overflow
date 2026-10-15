package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// prCloneFixture builds an "origin" repo carrying refs/pull/5/head with
// two commits beyond main, clones it, and registers the clone as a project —
// the shape ListPRCommits/OpenPRCommitDiff resolve through prCloneWorkspace.
func prCloneFixture(t *testing.T, app *App) (ref WorkspaceRef, clone string, prSHAs []string) {
	t.Helper()

	origin := initMainGitRepo(t)
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
	return gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 5}
}

// prRev is the revision the forge reports for prCloneFixture's PR: its head
// and the base branch's tip on origin.
func prRev(t *testing.T, clone string, head string) gitops.PRRevision {
	t.Helper()
	base, _, err := gitops.NewCore().Execute(clone, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	return gitops.PRRevision{BaseRef: "main", BaseSHA: strings.TrimSpace(base), HeadSHA: head}
}

func TestListPRCommitsFromLocalClone(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	ref, clone, prSHAs := prCloneFixture(t, app)

	commits, err := app.ListPRCommits(ref, prRef(), prRev(t, clone, prSHAs[0]))
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

func TestListPRCommitsWithoutCloneErrors(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	noClone := testWorkspaceRef(t, app, t.TempDir()) // not a git repo
	sha := strings.Repeat("a", 40)

	_, err := app.ListPRCommits(noClone, prRef(), gitops.PRRevision{BaseRef: "main", BaseSHA: sha, HeadSHA: sha})
	if err == nil || !strings.Contains(err.Error(), "workspace is not a git repository") {
		t.Fatalf("ListPRCommits() error = %v, want the not-a-repository refusal", err)
	}
}

// Every PR read whose commits are already in the clone answers without
// origin: the remote is broken, so any fetch would fail them.
func TestPRReadsSkipTheNetworkWhenCommitsAreLocal(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, clone, prSHAs := prCloneFixture(t, app)
	rev := prRev(t, clone, prSHAs[0])
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	commits, err := app.ListPRCommits(ref, prRef(), rev)
	if err != nil {
		t.Fatalf("ListPRCommits() error = %v", err)
	}
	if len(commits) != 2 || commits[0].SHA != prSHAs[0] {
		t.Fatalf("expected the local listing, got %+v", commits)
	}
	patch, err := openPRCommitPatch(t, app, ref, rev, prSHAs[0])
	if err != nil {
		t.Fatalf("OpenPRCommitDiff() error = %v", err)
	}
	if !strings.Contains(patch, "+second.txt content") {
		t.Fatalf("expected the commit diff from local objects, got:\n%s", patch)
	}
	ctx, _ := reviewDiffConn(t)
	if _, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false); err != nil {
		t.Fatalf("OpenPRDiff() error = %v", err)
	}
}

func TestPRReadsRefuseAnInvalidRevision(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, clone, prSHAs := prCloneFixture(t, app)
	valid := prRev(t, clone, prSHAs[0])
	ctx, _ := reviewDiffConn(t)
	for name, rev := range map[string]gitops.PRRevision{
		"empty base ref": {BaseRef: "  ", BaseSHA: valid.BaseSHA, HeadSHA: valid.HeadSHA},
		"no base commit": {BaseRef: "main", HeadSHA: valid.HeadSHA},
		"short head":     {BaseRef: "main", BaseSHA: valid.BaseSHA, HeadSHA: prSHAs[0][:12]},
	} {
		if _, err := app.ListPRCommits(ref, prRef(), rev); err == nil {
			t.Errorf("%s: ListPRCommits accepted %+v", name, rev)
		}
		if _, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false); err == nil {
			t.Errorf("%s: OpenPRDiff accepted %+v", name, rev)
		}
	}
}

func TestOpenPRCommitDiffFromLocalClone(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, clone, prSHAs := prCloneFixture(t, app)

	patch, err := openPRCommitPatch(t, app, ref, prRev(t, clone, prSHAs[0]), prSHAs[0])
	if err != nil {
		t.Fatalf("OpenPRCommitDiff() error = %v", err)
	}
	if !strings.Contains(patch, "+second.txt content") {
		t.Fatalf("expected the newest commit's own addition, got:\n%s", patch)
	}
	if strings.Contains(patch, "first.txt content") {
		t.Fatalf("single-commit diff must not include the earlier commit:\n%s", patch)
	}
}

func TestOpenPRCommitDiffWithoutCloneErrors(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	noClone := testWorkspaceRef(t, app, t.TempDir())
	sha := strings.Repeat("a", 40)

	_, err := openPRCommitPatch(t, app, noClone, gitops.PRRevision{BaseRef: "main", BaseSHA: sha, HeadSHA: sha}, sha)
	if err == nil || !strings.Contains(err.Error(), "workspace is not a git repository") {
		t.Fatalf("OpenPRCommitDiff() error = %v, want the not-a-repository refusal", err)
	}
}

// A PR read that could not fetch its commits has the PR's pump emit its
// next successful poll even though the snapshot is unchanged: that frame is
// the pane's cue to retry. One frame per failure, then dedup resumes.
func TestFailedPRReadMakesTheNextUnchangedPollEmit(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	stubPRCIFetch(app)
	app.prUpdates.interval = time.Hour
	app.prUpdates.fetchFn = func(_ context.Context, got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a"}}, nil
	}
	sub, err := app.SubscribePRUpdates(context.Background(), prRef())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = app.UnsubscribePRUpdates(context.Background(), sub.ID) })
	app.prUpdates.mu.Lock()
	pump := app.prUpdates.pumps[sub.PRKey]
	app.prUpdates.mu.Unlock()
	if _, changed := app.pollPRUpdate(pump); changed {
		t.Fatal("an unchanged poll emitted before any read failed")
	}

	ref, clone, prSHAs := prCloneFixture(t, app)
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	missing := gitops.PRRevision{BaseRef: "main", BaseSHA: strings.Repeat("d", 40), HeadSHA: prSHAs[0]}
	if _, err := app.ListPRCommits(ref, prRef(), missing); err == nil {
		t.Fatal("ListPRCommits succeeded without its base commit")
	}

	evt, changed := app.pollPRUpdate(pump)
	if !changed || evt.Error != "" || evt.Detail.HeadSHA != "head-a" || evt.Seq <= sub.Seq {
		t.Fatalf("poll after the failed read = %+v changed=%v, want the unchanged snapshot under a new seq", evt, changed)
	}
	if _, changed := app.pollPRUpdate(pump); changed {
		t.Fatal("a second unchanged poll emitted; the failure should cue one frame")
	}
}
