package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// retryEvent builds a minimal EventAPIRetry with the given attempt
// counts; the wire metadata uses the same shape both providers
// normalise to (see internal/provider/claude/parse_system.go and
// internal/provider/codex/protocol.go).
func retryEvent(threadID string, attempt, max int, reason string) provider.ProviderEvent {
	meta, _ := json.Marshal(map[string]any{
		"attempt":     attempt,
		"max_retries": max,
		"error":       reason,
	})
	return provider.ProviderEvent{
		Kind:      provider.EventAPIRetry,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

// openTurn opens turn 0 on the router so handleAPIRetry can resolve
// `currentTurnIndex` and allocate a deterministic id.
func openTurn(t *testing.T, router *Router, threadID string) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  threadID,
		TurnID:    "turn-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn-start: %v", err)
	}
}

// TestAPIRetryHidesAttemptsBelowFour mirrors Claude Code's
// SystemAPIErrorMessage.tsx hidden-when-retryAttempt<4 behaviour: we
// stay silent for the first three attempts so the timeline isn't
// polluted by transient retries that almost always succeed.
func TestAPIRetryHidesAttemptsBelowFour(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTurn(t, router, "t1")

	for attempt := 1; attempt <= 3; attempt++ {
		if err := router.Handle(retryEvent("t1", attempt, 10, "rate_limit")); err != nil {
			t.Fatalf("api_retry %d: %v", attempt, err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.Kind == itemKindAPIRetry {
			t.Fatalf("expected no api_retry rows below attempt 4, got %+v", it)
		}
	}
}

// TestAPIRetryUpsertsRowFromFourthAttempt — the row appears at attempt
// 4 and updates in place on subsequent attempts (deterministic id
// `retry:<turnIndex>`). Each new attempt refreshes summary +
// updatedAt without orphaning a prior row.
func TestAPIRetryUpsertsRowFromFourthAttempt(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTurn(t, router, "t1")

	if err := router.Handle(retryEvent("t1", 4, 10, "server_error")); err != nil {
		t.Fatalf("api_retry attempt 4: %v", err)
	}
	if err := router.Handle(retryEvent("t1", 5, 10, "server_error")); err != nil {
		t.Fatalf("api_retry attempt 5: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var rows []store.Item
	for _, it := range items {
		if it.Kind == itemKindAPIRetry {
			rows = append(rows, it)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one api_retry row, got %d (%+v)", len(rows), rows)
	}
	if rows[0].ID != "retry:0" {
		t.Fatalf("expected deterministic id retry:0, got %q", rows[0].ID)
	}
	if rows[0].Status != statusRunning {
		t.Fatalf("status: got %q, want %q", rows[0].Status, statusRunning)
	}
	// Summary must reflect the latest attempt (5/10), not the first (4/10).
	if !strings.Contains(rows[0].Summary, "5/10") {
		t.Fatalf("summary should reflect attempt 5/10, got %q", rows[0].Summary)
	}
}

// TestAPIRetryClosesOnForwardProgress — once a forward-progress event
// arrives for the thread (text delta, tool start, tool complete, turn
// complete, etc) the api_retry row flips from running to completed
// so the renderer reads it as historical context rather than a live
// indicator.
func TestAPIRetryClosesOnForwardProgress(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTurn(t, router, "t1")

	if err := router.Handle(retryEvent("t1", 4, 10, "rate_limit")); err != nil {
		t.Fatalf("api_retry: %v", err)
	}

	// Forward-progress event: a text delta on the open turn proves the
	// retry succeeded, so the row should flip to completed.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		ItemID:    "msg-1",
		Content:   "ok",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", "retry:0")
	if err != nil {
		t.Fatalf("get retry row: %v", err)
	}
	if !found {
		t.Fatalf("expected api_retry row to remain after forward progress")
	}
	if row.Status != statusCompleted {
		t.Fatalf("status: got %q, want %q (forward progress should flip to completed)", row.Status, statusCompleted)
	}
}

// TestAPIRetryStaysCompletedOnLateAttempt — once the row has flipped
// to completed, an out-of-order retry observation arriving late in the
// turn must NOT reopen the row. The historical record should stay
// closed so it doesn't read as "still retrying" after work continued.
// The upsert still runs (refreshing the summary in place) — only the
// status preservation kicks in.
func TestAPIRetryStaysCompletedOnLateAttempt(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTurn(t, router, "t1")

	if err := router.Handle(retryEvent("t1", 4, 10, "rate_limit")); err != nil {
		t.Fatalf("api_retry initial: %v", err)
	}
	// Forward-progress flips the row to completed.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		ItemID:    "msg-1",
		Content:   "ok",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	// Late retry observation arrives after the turn moved on.
	if err := router.Handle(retryEvent("t1", 5, 10, "rate_limit")); err != nil {
		t.Fatalf("late api_retry: %v", err)
	}

	row, _, err := st.GetThreadItem("t1", "retry:0")
	if err != nil {
		t.Fatalf("get retry row: %v", err)
	}
	if row.Status != statusCompleted {
		t.Fatalf("status: got %q, want %q (late retry should not reopen row)", row.Status, statusCompleted)
	}
	// The upsert path still runs and refreshes the summary in place;
	// only the status preservation differs from a not-yet-completed
	// re-attempt. A change that skipped the upsert entirely on
	// completed rows would silently break this assertion.
	if !strings.Contains(row.Summary, "5/10") {
		t.Fatalf("late attempt should refresh summary to 5/10, got %q", row.Summary)
	}
}
