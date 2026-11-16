package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// A PR read stops at 1000 conversation comments on either forge and the
// pane's snapshot says so; one with exactly 1000 says nothing.
func TestPRUpdateSnapshotSaysTheThreadsAreBounded(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gitlab", "github"} {
		for _, n := range []int{1000, 1001} {
			t.Run(fmt.Sprintf("%s %d", forge, n), func(t *testing.T) {
				t.Parallel()
				app, _ := newTestAppWithStorePath(t)
				engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
				comments := make([]forgefake.Comment, n)
				for i := range comments {
					comments[i] = forgefake.Comment{Body: fmt.Sprintf("comment %d", i)}
				}
				if _, err := engine.Seed(forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{Forge: forge, Project: "acme/widgets", Pulls: []forgefake.Pull{
					{Number: 9, Title: "Widgets", Author: "octo", HeadRef: "feature", HeadSHA: reviewTestHead, State: "open", Comments: comments},
				}}}}); err != nil {
					t.Fatal(err)
				}
				useForgefake(t, app, engine)
				pr := gitops.PRReference{Forge: forge, Host: map[string]string{"gitlab": "gitlab.com", "github": "github.com"}[forge], Namespace: "acme", Repo: "widgets", Number: 9}
				snapshot, err := app.fetchPRUpdateSnapshot(context.Background(), pr)
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if n > 1000 {
					want = "Only the first 1000 threads and comments of this conversation are shown; the rest are on " + map[string]string{"gitlab": "GitLab", "github": "GitHub"}[forge] + "."
				}
				if len(snapshot.Threads) != 1000 || snapshot.ThreadsTruncated != want {
					t.Fatalf("%d threads, truncated %q; want 1000 and %q", len(snapshot.Threads), snapshot.ThreadsTruncated, want)
				}
			})
		}
	}
}

// The bounded-read sentence reaches the subscriber and its frames, and a
// change of it alone is a frame.
func TestThreadsTruncatedReachesThePane(t *testing.T) {
	t.Parallel()
	app := NewApp()
	stubPRCIFetch(app)
	app.prUpdates.interval = 5 * time.Millisecond
	const notice = "Only the first 1000 threads and comments of this conversation are shown; the rest are on GitHub."
	var sentence atomic.Value
	sentence.Store(notice)
	app.prUpdates.fetchFn = func(_ context.Context, pr gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: pr.Number, HeadSHA: "head"}, ThreadsTruncated: sentence.Load().(string)}, nil
	}
	events := capturePRUpdates(t, app)
	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	t.Cleanup(func() {
		_ = app.UnsubscribePRUpdates(context.Background(), sub.ID)
		app.prUpdates.wg.Wait()
	})
	if sub.ThreadsTruncated != notice {
		t.Fatalf("subscribe ThreadsTruncated = %q", sub.ThreadsTruncated)
	}
	expectNoPRUpdate(t, events, "an unchanged snapshot")
	sentence.Store("")
	if frame := awaitPRUpdate(t, events, "the sentence cleared"); frame.ThreadsTruncated != "" {
		t.Fatalf("frame ThreadsTruncated = %q", frame.ThreadsTruncated)
	}
	sentence.Store(notice)
	if frame := awaitPRUpdate(t, events, "the sentence back"); frame.ThreadsTruncated != notice {
		t.Fatalf("frame ThreadsTruncated = %q", frame.ThreadsTruncated)
	}
}
