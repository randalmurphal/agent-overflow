package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

func TestMapSubmitPRReviewResult(t *testing.T) {
	cases := []struct {
		name    string
		result  gitops.SubmitReviewResult
		err     error
		want    SubmitPRReviewResult
		wantErr bool
	}{
		{
			name:   "success",
			result: gitops.SubmitReviewResult{PostedReview: true, PostedFileComments: 2},
			want:   SubmitPRReviewResult{PostedReview: true, PostedFileComments: 2},
		},
		{
			name: "partial",
			err: &gitops.PartialSubmitError{
				PostedReview:       true,
				PostedFileComments: 1,
				FailedPath:         "README.md",
				Err:                errors.New("denied"),
			},
			want: SubmitPRReviewResult{
				PostedReview:       true,
				PostedFileComments: 1,
				PartialFailurePath: "README.md",
				PartialFailure:     "denied",
			},
		},
		{
			name:    "hard error",
			err:     errors.New("no auth"),
			wantErr: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapSubmitPRReviewResult(tt.result, tt.err)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}
}

func TestPRUpdatePollingEmitsOnlyOnSnapshotChange(t *testing.T) {
	app := NewApp()
	app.prUpdateInterval = 5 * time.Millisecond
	pr := gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9}
	calls := 0
	app.prUpdateFetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls++
		head := "head-a"
		if calls >= 2 {
			head = "head-b"
		}
		return prUpdateSnapshot{
			Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head, Mergeability: gitops.MergeabilityChecking},
			Threads: []gitops.ReviewThread{{
				ID:   "thread-1",
				Path: "main.go",
				Side: "right",
			}},
		}, nil
	}
	events := make(chan PRUpdatedEvent, 4)
	app.testEmitHook = func(name string, data any) {
		if name != "pr:updated" {
			return
		}
		evt, ok := data.(PRUpdatedEvent)
		if !ok {
			t.Fatalf("event payload type = %T", data)
		}
		events <- evt
	}

	sub, err := app.SubscribePRUpdates(context.Background(), "thread-123", pr)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdatePumpWG.Wait()
	}()

	select {
	case evt := <-events:
		if evt.SubscriptionID != sub.ID || evt.HeadSHA != "head-b" {
			t.Fatalf("event = %+v", evt)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for changed snapshot emit")
	}

	select {
	case evt := <-events:
		t.Fatalf("unexpected unchanged snapshot emit: %+v", evt)
	case <-time.After(40 * time.Millisecond):
	}
}

func TestPRUpdatePollingPausesWhileInactiveAndCatchesUpOnResume(t *testing.T) {
	app := NewApp()
	app.prUpdateInterval = 5 * time.Millisecond
	pr := gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9}
	var calls atomic.Int32
	var changed atomic.Bool
	app.prUpdateFetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls.Add(1)
		head := "head-a"
		if changed.Load() {
			head = "head-b"
		}
		return prUpdateSnapshot{
			Detail: gitops.PRDetail{Number: got.Number, HeadSHA: head, Mergeability: gitops.MergeabilityChecking},
		}, nil
	}
	events := make(chan PRUpdatedEvent, 4)
	app.testEmitHook = func(name string, data any) {
		if name != "pr:updated" {
			return
		}
		evt, ok := data.(PRUpdatedEvent)
		if !ok {
			t.Errorf("event payload type = %T", data)
			return
		}
		events <- evt
	}

	sub, err := app.SubscribePRUpdates(context.Background(), "thread-123", pr)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdatePumpWG.Wait()
	}()

	if err := app.SetPRUpdatesActive(sub.ID, false); err != nil {
		t.Fatalf("SetPRUpdatesActive(false): %v", err)
	}
	// A tick already past the paused check may still complete one fetch;
	// let it drain, then require the count to hold across many intervals.
	time.Sleep(15 * time.Millisecond)
	before := calls.Load()
	time.Sleep(60 * time.Millisecond)
	if after := calls.Load(); after != before {
		t.Fatalf("paused pump kept polling: calls %d -> %d", before, after)
	}

	changed.Store(true)
	if err := app.SetPRUpdatesActive(sub.ID, true); err != nil {
		t.Fatalf("SetPRUpdatesActive(true): %v", err)
	}
	select {
	case evt := <-events:
		if evt.SubscriptionID != sub.ID || evt.HeadSHA != "head-b" {
			t.Fatalf("event = %+v", evt)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("no catch-up poll after resume")
	}
}

func TestPRUpdateResumeWithoutMissedTickDoesNotPoll(t *testing.T) {
	app := NewApp()
	app.prUpdateInterval = 300 * time.Millisecond
	pr := gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9}
	var calls atomic.Int32
	app.prUpdateFetchFn = func(got gitops.PRReference) (prUpdateSnapshot, error) {
		calls.Add(1)
		return prUpdateSnapshot{
			Detail: gitops.PRDetail{Number: got.Number, HeadSHA: "head-a", Mergeability: gitops.MergeabilityChecking},
		}, nil
	}

	sub, err := app.SubscribePRUpdates(context.Background(), "thread-123", pr)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	defer func() {
		if err := app.UnsubscribePRUpdates(sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		app.prUpdatePumpWG.Wait()
	}()

	// Quick hide/show flip well inside one interval: no tick was missed,
	// so resume must not spend a fetch.
	if err := app.SetPRUpdatesActive(sub.ID, false); err != nil {
		t.Fatalf("SetPRUpdatesActive(false): %v", err)
	}
	if err := app.SetPRUpdatesActive(sub.ID, true); err != nil {
		t.Fatalf("SetPRUpdatesActive(true): %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("resume without missed tick polled: calls = %d (want 1, the subscribe fetch)", got)
	}
}

func TestSetPRUpdatesActiveUnknownIDIsNoOp(t *testing.T) {
	app := NewApp()
	if err := app.SetPRUpdatesActive("nope", true); err != nil {
		t.Fatalf("SetPRUpdatesActive: %v", err)
	}
}
