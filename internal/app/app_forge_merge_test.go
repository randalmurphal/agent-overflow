package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

// A merge action that changed the PR polls the live pump's snapshot: a
// success, and a merge whose branch delete then failed. A refusal changed
// nothing and does not.
func TestSettlePRActionRefreshesWhenThePRChanged(t *testing.T) {
	t.Parallel()
	app := NewApp()
	stubPRCIFetch(app)
	app.prUpdates.interval = time.Hour
	var fetches atomic.Int32
	app.prUpdates.fetchFn = func(_ context.Context, got gitops.PRReference) (prUpdateSnapshot, error) {
		fetches.Add(1)
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head", Mergeability: gitops.MergeabilityClean}}, nil
	}
	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(context.Background(), sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdates.wg.Wait()
	}()
	base := fetches.Load()

	if err := app.settlePRAction(context.Background(), testPR, gitops.ActionMerge, nil); err != nil || fetches.Load() != base+1 {
		t.Fatalf("success = %v, fetches %d after %d", err, fetches.Load(), base)
	}

	refused := errors.New("refused")
	err = app.settlePRAction(context.Background(), testPR, gitops.ActionMerge, refused)
	if explained, ok := errors.AsType[*gitops.ForgeActionError](err); !ok || !errors.Is(err, refused) || fetches.Load() != base+1 {
		t.Fatalf("refusal = %v (%+v), fetches %d", err, explained, fetches.Load())
	}

	kept := &gitops.BranchKeptError{Outcome: gitops.MergeOutcome{Merged: true, SHA: "m"}, Branch: "feature", Err: errors.New("delete refused")}
	err = app.settlePRAction(context.Background(), testPR, gitops.ActionMerge, kept)
	explained, ok := errors.AsType[*gitops.ForgeActionError](err)
	if !ok || explained.Message != "Merged owner/repo#9, but GitHub kept the branch feature: delete refused" || fetches.Load() != base+2 {
		t.Fatalf("merged, branch kept = %v, fetches %d", err, fetches.Load())
	}
}
