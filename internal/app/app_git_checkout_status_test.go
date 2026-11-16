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

// GetGitStatus and a checkout answer with the status the workspace's
// stream publishes: sequenced after the subscribe snapshot, in the order
// they were read, and the content git:status last broadcast, at the same
// seq or an earlier one.
func TestGitStatusAnswersAreTheStreamsPublishedStatus(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	core := f.app.gitCore()
	// The snapshot is not the repository's status, so every answer below
	// changes the published status and is broadcast.
	f.app.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn: core.Status,
		FastStatusFn: func(context.Context, string) (gitops.GitStatus, error) {
			return gitops.GitStatus{IsRepo: true, Branch: "elsewhere"}, nil
		},
	})
	t.Cleanup(f.app.gitWatch.Close)
	events, mu := captureGitStatusEmissions(f.app)
	res, err := f.app.GitStatusSubscribe(context.Background(), f.ref)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		if err := f.app.GitStatusUnsubscribe(context.Background(), res.ID); err != nil {
			t.Errorf("unsubscribe: %v", err)
		}
	}()
	// The watcher's own refresh can read a change before the method that
	// made it does. It broadcasts that content first, and the answer then
	// takes the newer seq without a second broadcast.
	broadcast := func(label string, answer gitops.GitStatus) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			seen := gitStatusEventsFor(events, mu, res.Cwd)
			if len(seen) > 0 {
				if latest := seen[len(seen)-1].Status; latest.Equal(answer) && latest.Seq <= answer.Seq {
					return
				}
			}
			select {
			case <-deadline:
				t.Fatalf("%s: answer %+v is not the latest of the broadcasts %+v", label, answer, seen)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	status, err := f.app.GetGitStatus(t.Context(), f.ref)
	if err != nil {
		t.Fatalf("GetGitStatus: %v", err)
	}
	if status.Branch != "main" || status.Seq <= res.Status.Seq {
		t.Fatalf("GetGitStatus = %+v, want branch main at a seq above the snapshot's %d", status, res.Status.Seq)
	}
	broadcast("GetGitStatus", status)

	state, err := f.app.GitCheckout(f.ref, "refs/tags/v1")
	if err != nil {
		t.Fatalf("GitCheckout(tag): %v", err)
	}
	if state.Status == nil || state.Status.Tag != "v1" || state.Status.Seq <= status.Seq {
		t.Fatalf("GitCheckout status = %+v, want tag v1 at a seq above %d", state.Status, status.Seq)
	}
	broadcast("GitCheckout", *state.Status)

	created, err := f.app.GitCreateBranchFrom(f.ref, "rescue", "", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom: %v", err)
	}
	if created.Status == nil || created.Status.Branch != "rescue" || created.Status.Seq <= state.Status.Seq {
		t.Fatalf("GitCreateBranchFrom status = %+v, want branch rescue at a seq above %d", created.Status, state.Status.Seq)
	}
	broadcast("GitCreateBranchFrom", *created.Status)
}

// A checkout while the workspace's status is held off the wire cannot be
// published, so it answers with its own read: the branch it checked out,
// sequenced after the held status, and that branch on the thread rows.
func TestCheckoutWhileStatusIsHeldAnswersItsOwnRead(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	core := f.app.gitCore()
	f.app.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn:     core.Status,
		FastStatusFn: func(_ context.Context, cwd string) (gitops.GitStatus, error) { return core.StatusFast(cwd) },
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
	if res.Status.Branch != "main" {
		t.Fatalf("snapshot branch = %q, want main", res.Status.Branch)
	}
	resume := f.app.gitApplication().SuppressStatus(f.repo)
	defer resume()

	check := func(label string, state GitWorkspaceState, want string, after uint64) {
		t.Helper()
		if state.Status == nil || state.Status.Branch != want || state.Status.Seq <= after {
			t.Errorf("%s: status %+v, want its own read of %q at a seq above %d", label, state.Status, want, after)
		}
		if state.Branch != want {
			t.Errorf("%s: answered branch %q, want %q", label, state.Branch, want)
		}
		if got := f.storedBranch(t, f.thread.ID); got != want {
			t.Errorf("%s: thread row branch %q, want %q", label, got, want)
		}
	}
	state, err := f.app.GitCheckout(f.ref, "dup")
	if err != nil {
		t.Fatalf("GitCheckout: %v", err)
	}
	check("GitCheckout", state, "dup", res.Status.Seq)
	if state.Status == nil {
		t.FailNow()
	}
	created, err := f.app.GitCreateBranchFrom(f.ref, "rescue", "", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom: %v", err)
	}
	check("GitCreateBranchFrom", created, "rescue", state.Status.Seq)
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
