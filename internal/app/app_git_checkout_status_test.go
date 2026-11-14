package app

import (
	"context"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
)

// A checkout answers with the git status it produced, so a client has the
// new checkout's facts without a status call that waits on the forge.
func TestCheckoutAnswersWithTheStatusItProduced(t *testing.T) {
	t.Parallel()
	repo, _ := initGitRepoWithOrigin(t)
	f := newTagFixture(t, repo)
	check := func(label string, state GitWorkspaceState, want gitops.GitStatus) {
		t.Helper()
		got := state.Status
		if got == nil {
			t.Fatalf("%s: no status in %+v", label, state)
		}
		if !got.IsRepo || got.Branch != want.Branch || got.Tag != want.Tag || got.HeadCommit != want.HeadCommit ||
			got.HasUpstream != want.HasUpstream || got.AheadCount != want.AheadCount || got.BehindCount != 0 {
			t.Errorf("%s: status = branch %q tag %q head %q upstream %v ahead %d behind %d; want branch %q tag %q head %q upstream %v ahead %d",
				label, got.Branch, got.Tag, got.HeadCommit, got.HasUpstream, got.AheadCount, got.BehindCount,
				want.Branch, want.Tag, want.HeadCommit, want.HasUpstream, want.AheadCount)
		}
	}

	state, err := f.app.GitCheckout(f.ref, "refs/tags/v1")
	if err != nil {
		t.Fatalf("GitCheckout(tag): %v", err)
	}
	check("tag checkout", state, gitops.GitStatus{Tag: "v1", HeadCommit: f.v1})

	// main tracks origin/main and is two commits ahead of it.
	if state, err = f.app.GitCheckout(f.ref, "main"); err != nil {
		t.Fatalf("GitCheckout(main): %v", err)
	}
	check("branch checkout", state, gitops.GitStatus{Branch: "main", HasUpstream: true, AheadCount: 2})

	if _, err := f.app.GitCheckout(f.ref, "refs/tags/v1"); err != nil {
		t.Fatal(err)
	}
	if state, err = f.app.GitCreateBranchFrom(f.ref, "rescue", "", false); err != nil {
		t.Fatalf("GitCreateBranchFrom(detached): %v", err)
	}
	check("branch from a detached HEAD", state, gitops.GitStatus{Branch: "rescue"})
}

// A checkout pushes the new status to every client watching the workspace.
func TestCheckoutRefreshesTheWorkspaceStatusStream(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	// Only the workspace's top level is watched, not .git, so the checkout's
	// own ref writes trigger nothing and the refresh must be asked for.
	elsewhere := t.TempDir()
	f.app.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn: stub.fn(),
		WatchRootsFn: func(string) ([]gitops.WatchRoot, error) {
			return []gitops.WatchRoot{{Path: elsewhere}}, nil
		},
	})
	t.Cleanup(f.app.gitWatch.Close)
	res, err := f.app.GitStatusSubscribe(context.Background(), f.ref)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		if err := f.app.GitStatusUnsubscribe(context.Background(), res.ID); err != nil {
			t.Errorf("unsubscribe: %v", err)
		}
	}()
	events, mu := captureGitStatusEmissions(f.app)
	stub.setStatus(gitops.GitStatus{IsRepo: true, Tag: "v1"})

	if _, err := f.app.GitCheckout(f.ref, "refs/tags/v1"); err != nil {
		t.Fatalf("GitCheckout(tag): %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		matches := gitStatusEventsFor(events, mu, res.Cwd)
		if len(matches) > 0 && matches[len(matches)-1].Status.Tag == "v1" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("GitCheckout did not push a refresh to the workspace's subscribers")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
