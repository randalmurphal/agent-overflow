package main

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// The runner-side quota rules live in `internal/workflowhost`. What stays here
// is the one App-level fact they depend on: the interrupt the workflow cleanup
// issues is bookkeeping, not a person pressing stop, so it must not leave a
// "Stopped by user" row on the thread.
func TestUsageLimitCleanupDoesNotRecordAUserInterrupt(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()
	thread := testThread("usage-cleanup-interrupt")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: thread.ID, Content: "usage is exhausted",
		Meta: json.RawMessage(`{"fatal":true,"expect_turn_complete":true,"codexErrorInfo":"usageLimitExceeded"}`),
		Failure: &provider.FailureMeta{
			Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn,
			Reason: provider.FailureReasonUsageLimit, Code: "usageLimitExceeded",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if open := app.triage.OpenTurnIndex(thread.ID); open != -1 {
		t.Fatalf("usage refusal left turn %d open before workflow cleanup", open)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex), token: "usage-cleanup-token", codex: sess,
	})
	if err := app.InterruptTurn(thread.ID); err != nil {
		t.Fatal(err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == "error" && item.Summary == "Stopped by user" {
			t.Fatalf("usage-limit cleanup created user-interrupt row %+v", item)
		}
	}
}
