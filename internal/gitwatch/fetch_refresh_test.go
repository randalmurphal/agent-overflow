package gitwatch

import (
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// TestBackgroundFetchRefUpdateReachesSubscribers is the evidence behind
// the claim in app_git_background_fetch.go that the background fetch
// needs no explicit refresh trigger.
//
// `git fetch` writes remote-tracking refs under the common dir's refs/
// (watched recursively) and FETCH_HEAD at its top level (watched
// non-recursively) — see gitMetadataRoots in internal/git/watch_roots.go
// — so the existing watcher for a subscribed workspace observes the
// update on its own and re-runs the status pipeline. If that ever stops
// being true, this test fails and the fetch cadence has to broadcast the
// refresh itself.
func TestBackgroundFetchRefUpdateReachesSubscribers(t *testing.T) {
	repo, bare := testutil.InitGitRepoWithOrigin(t)

	core := gitops.NewCore()
	mgr := NewManager(ManagerConfig{
		StatusFn:     core.Status,
		FastStatusFn: core.StatusFast,
		WatchRootsFn: core.WatchRoots,
	})
	t.Cleanup(mgr.Close)

	sub, err := mgr.Subscribe(repo)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()
	if got := sub.Initial().BehindCount; got != 0 {
		t.Fatalf("initial BehindCount = %d, want 0", got)
	}

	// A collaborator pushes. Nothing local changed, so nothing in the
	// workspace can tell the watcher — only the fetch can.
	testutil.AdvanceOriginMain(t, bare)

	fetched, err := core.FetchRemotesBackground(t.Context(), repo)
	if err != nil {
		t.Fatalf("FetchRemotesBackground: %v", err)
	}
	if !fetched {
		t.Fatal("expected the background fetch to run")
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case status, ok := <-sub.Updates():
			if !ok {
				t.Fatal("subscription closed while waiting for the post-fetch status")
			}
			if status.BehindCount == 1 {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the fs watch to notice the fetched refs; " +
				"the background fetch cadence would need to trigger the refresh explicitly")
		}
	}
}
