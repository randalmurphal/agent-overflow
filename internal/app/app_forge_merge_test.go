package app

import (
	"context"
	"errors"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

// A merge action's success asks the PR's live pump to poll at once: the
// next frame carries the snapshot the poll read, without waiting out the
// interval.
func TestPRActionRefreshPollsTheLivePump(t *testing.T) {
	t.Parallel()
	app, script, events, sub := newScriptedPump(t, ok)
	// Park the ticker: a 5ms interval would poll on its own and hide
	// whether the refresh did.
	app.prUpdates.mu.Lock()
	pump := app.livePRUpdatePumpLocked(sub.PRKey)
	app.prUpdates.mu.Unlock()
	if pump == nil {
		t.Fatal("no live pump after subscribe")
	}
	pump.paused.Store(true)
	before := len(script.calls())
	// The scripted fetch answers head-a every time; the refresh's own
	// poll is told apart by its count, and the frame by a changed head.
	script.mu.Lock()
	script.steps = []func() error{func() error { return nil }}
	script.mu.Unlock()
	app.prUpdates.fetchFn = func(_ context.Context, pr gitops.PRReference) (prUpdateSnapshot, error) {
		script.mu.Lock()
		script.at = append(script.at, time.Now())
		script.mu.Unlock()
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: pr.Number, HeadSHA: "head-b"}, Events: []gitops.ReviewEvent{{ID: "e1", Kind: gitops.EventApproved}}}, nil
	}
	if err := app.settlePRAction(context.Background(), testPR, gitops.ActionApprove, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	frame := awaitPRUpdate(t, events, "refresh frame")
	if frame.HeadSHA != "head-b" || len(frame.Events) != 1 || frame.Error != "" {
		t.Fatalf("refresh frame = %+v, want head-b with one event", frame)
	}
	if got := len(script.calls()) - before; got != 1 {
		t.Fatalf("polls after the action = %d, want 1", got)
	}
}

// A failed action reaches the caller as the one sentence under its kind,
// and polls nothing.
func TestPRActionFailureIsExplained(t *testing.T) {
	t.Parallel()
	app, script, _, _ := newScriptedPump(t, ok)
	before := len(script.calls())
	err := app.settlePRAction(context.Background(), testPR, gitops.ActionMerge, errors.New("forge PUT merge: HTTP 418"))
	var explained *gitops.ForgeActionError
	if !errors.As(err, &explained) || explained.Kind != gitops.ForgeErrorForge {
		t.Fatalf("settle = %T %v, want a forge ForgeActionError", err, err)
	}
	time.Sleep(20 * time.Millisecond)
	if polls := len(script.calls()) - before; polls > 4 {
		t.Fatalf("a failure should not add a poll beyond the ticker's; got %d", polls)
	}
}

// Without a live pump (no pane on the PR) a success refreshes nothing and
// still succeeds.
func TestPRActionWithoutPumpSucceeds(t *testing.T) {
	t.Parallel()
	app := NewApp()
	if err := app.settlePRAction(context.Background(), testPR, gitops.ActionApprove, nil); err != nil {
		t.Fatalf("settle without pump: %v", err)
	}
}

// The bound methods validate before touching a forge.
func TestPRActionsValidateTheReference(t *testing.T) {
	t.Parallel()
	app := NewApp()
	ctx := context.Background()
	bad := gitops.PRReference{Forge: "github"}
	if err := app.ApprovePR(ctx, bad, "abc"); err == nil {
		t.Fatal("ApprovePR accepted an empty reference")
	}
	if _, err := app.MergePR(ctx, testPR, gitops.MergeRequest{Method: "rebase-merge", HeadSHA: "abcdef1"}); err == nil {
		t.Fatal("MergePR accepted an unknown method")
	}
	if _, err := app.MergePR(ctx, testPR, gitops.MergeRequest{Method: gitops.MergeMethodSquash, HeadSHA: "not a sha"}); err == nil {
		t.Fatal("MergePR accepted a malformed head")
	}
}
