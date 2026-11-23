package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

// A pump whose loop panics while serving a person's refresh stops on its
// way out: the refresh is refused as an unknown subscription instead of
// waiting until its caller gives up. Each test bounds the refresh with a
// deadline, which a refresh left waiting would return instead.

func refreshDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPRCIRefreshReturnsWhenThePumpLoopPanics(t *testing.T) {
	t.Parallel()
	app := NewApp()
	stubPRCIFetch(app)
	app.prUpdates.interval = time.Hour
	app.prUpdates.fetchFn = func(_ context.Context, got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a", BaseRefSHA: "base-a"}}, nil
	}
	var explode atomic.Bool
	firstPoll := make(chan struct{})
	var firstPolled sync.Once
	app.prUpdates.ciFetchFn = func(context.Context, gitops.PRReference, *gitops.CIPipeline, []string) (gitops.CIPipeline, error) {
		if explode.Load() {
			panic("test: the CI poll panicked")
		}
		defer firstPolled.Do(func() { close(firstPoll) })
		return gitops.CIPipeline{}, nil
	}
	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() {
		app.unsubscribePRUpdates(sub.ID)
		app.prUpdates.wg.Wait()
	})
	// The loop's first CI poll ran, so the refresh is served by the loop
	// rather than refused by a pump that already died on that poll.
	select {
	case <-firstPoll:
	case <-time.After(10 * time.Second):
		t.Fatal("the pump's first CI poll never ran")
	}
	explode.Store(true)
	if err := app.refreshPRCI(refreshDeadline(t), sub.ID); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Fatalf("refresh against a pump whose loop panicked = %v, want the unknown subscription", err)
	}
}

func TestPipelineRefreshReturnsWhenThePumpLoopPanics(t *testing.T) {
	t.Parallel()
	app := NewApp()
	stubPRCIFetch(app)
	app.prUpdates.interval = time.Hour
	var explode atomic.Bool
	app.prUpdates.pipelineFetchFn = func(context.Context, gitops.RepoReference, string, *gitops.CIPipeline, []string) (gitops.CIPipeline, error) {
		if explode.Load() {
			panic("test: the pipeline read panicked")
		}
		return pipelineWith(gitops.CIStatusSuccess), nil
	}
	sub, err := app.SubscribePipeline(context.Background(), testPR.Repository(), "42")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() {
		app.unsubscribePipeline(sub.ID)
		app.prUpdates.wg.Wait()
	})
	explode.Store(true)
	if err := app.RefreshPipeline(refreshDeadline(t), sub.ID); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Fatalf("refresh against a pump whose loop panicked = %v, want the unknown subscription", err)
	}
}

func TestPRListRefreshReturnsWhenThePumpLoopPanics(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	awaitPRListRead(t, events)
	fake.set(func(f *prListFake) {
		f.projectsGate = func(context.Context) { panic("test: the projects read panicked") }
	})
	if err := app.RefreshPRList(refreshDeadline(t), sub.ID); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Fatalf("refresh against a pump whose loop panicked = %v, want the unknown subscription", err)
	}
}
