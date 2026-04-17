package triage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// fakeCheckpointStore records capture calls so tests can assert on the
// workspace + thread arguments the router passed through.
type fakeCheckpointStore struct {
	isRepo        bool
	captureErr    error
	capturedCalls []fakeCaptureCall
}

type fakeCaptureCall struct {
	Workspace string
	ThreadID  string
	TurnIndex int
}

func (f *fakeCheckpointStore) IsGitRepository(_ context.Context, _ string) bool {
	return f.isRepo
}

func (f *fakeCheckpointStore) CaptureBaseline(_ context.Context, workspace, threadID string, turnIndex int) (string, error) {
	f.capturedCalls = append(f.capturedCalls, fakeCaptureCall{
		Workspace: workspace,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
	})
	if f.captureErr != nil {
		return "", f.captureErr
	}
	// Mimic the real store: ref is unique per (thread, turn).
	return fmt.Sprintf("refs/agent-overflow/checkpoints/%s/turn/%d", threadID, turnIndex), nil
}

func TestHandleTurnStartCapturesBaselineWhenRepo(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(fake.capturedCalls) != 1 {
		t.Fatalf("expected 1 capture call, got %d", len(fake.capturedCalls))
	}
	if fake.capturedCalls[0].ThreadID != "t1" {
		t.Errorf("thread id: got %q, want t1", fake.capturedCalls[0].ThreadID)
	}
	if fake.capturedCalls[0].Workspace != "/tmp" {
		t.Errorf("workspace: got %q, want /tmp", fake.capturedCalls[0].Workspace)
	}

	// A checkpoint row must exist now.
	checkpoints, err := st.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint row, got %d", len(checkpoints))
	}
	if checkpoints[0].RefName == "" {
		t.Errorf("expected non-empty ref name")
	}

	// Existing behavior preserved: turn-start event still emitted inline.
	sawTurnStart := false
	sawCaptured := false
	for _, e := range *emissions {
		if e.eventName == "provider:event" {
			sawTurnStart = true
		}
		if e.eventName == "checkpoint:captured" {
			sawCaptured = true
		}
	}
	if !sawTurnStart {
		t.Errorf("expected provider:event emission for turn_start")
	}
	if !sawCaptured {
		t.Errorf("expected checkpoint:captured emission")
	}
}

func TestHandleTurnStartSkipsWhenNotGitRepo(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: false}
	router.SetCheckpointStore(fake)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(fake.capturedCalls) != 0 {
		t.Errorf("expected no capture calls when not a repo, got %d", len(fake.capturedCalls))
	}
	// No checkpoint row.
	rows, _ := st.ListCheckpoints("t1")
	if len(rows) != 0 {
		t.Errorf("expected no checkpoint row, got %d", len(rows))
	}

	// Activity event so the UI can display "checkpoints unavailable".
	sawUnavailable := false
	for _, e := range *emissions {
		if e.eventName == "checkpoint:unavailable" {
			sawUnavailable = true
		}
	}
	if !sawUnavailable {
		t.Errorf("expected checkpoint:unavailable activity emission")
	}
}

func TestHandleTurnStartNoOpWhenCheckpointStoreNil(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	// Deliberately do NOT call SetCheckpointStore — the router must still
	// handle turn_start without panicking.

	evt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Errorf("handle should not error when checkpoint store unset: %v", err)
	}
}

func TestHandleTurnStartCaptureErrorDoesNotAbortTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true, captureErr: errors.New("boom")}
	router.SetCheckpointStore(fake)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Errorf("turn-start must not return error even when capture fails: %v", err)
	}

	rows, _ := st.ListCheckpoints("t1")
	if len(rows) != 0 {
		t.Errorf("no checkpoint row should be persisted when capture errored, got %d", len(rows))
	}

	sawErr := false
	for _, e := range *emissions {
		if e.eventName == "checkpoint:error" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("expected checkpoint:error activity emission")
	}
}

func TestHandleTurnStartDoubleFiresAreDedupedPerTurn(t *testing.T) {
	// Some providers re-emit turn_start on interrupt/resume. We must not
	// capture twice for the same (thread, turn_index).
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	for i := 0; i < 3; i++ {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  "t1",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}

	if len(fake.capturedCalls) != 1 {
		t.Errorf("expected 1 capture across repeated turn_start events, got %d", len(fake.capturedCalls))
	}
	rows, _ := st.ListCheckpoints("t1")
	if len(rows) != 1 {
		t.Errorf("expected 1 checkpoint row, got %d", len(rows))
	}
}

func TestHandleTurnStartCapturesWorktreePathWhenSet(t *testing.T) {
	// When a thread has a worktree_path (git worktree), capture should
	// target that directory rather than the project root.
	router, st, _ := newTestRouter(t)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t-wt",
		Title:         "WT thread",
		Provider:      "claude",
		WorkspacePath: "/tmp/project",
		WorktreePath:  "/tmp/project/.worktrees/feature",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t-wt",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(fake.capturedCalls) != 1 {
		t.Fatalf("expected 1 capture, got %d", len(fake.capturedCalls))
	}
	if fake.capturedCalls[0].Workspace != "/tmp/project/.worktrees/feature" {
		t.Errorf("workspace mismatch: got %q", fake.capturedCalls[0].Workspace)
	}
}

func TestCleanupThreadClearsCheckpointTrackingState(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	router.CleanupThread("t1")

	// After cleanup, a second turn_start must be allowed to re-capture. We
	// need another item in the store to advance the turn index — simulate it.
	if _, err := st.GetThread("t1"); err != nil {
		t.Fatalf("thread gone: %v", err)
	}
	// Append a text item so LastTurnIndex > 0.
	if err := st.InsertItem(store.Item{
		ID:        "item-1",
		ThreadID:  "t1",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		Summary:   "next",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle second turn: %v", err)
	}
	if len(fake.capturedCalls) != 2 {
		t.Errorf("expected 2 captures across turns, got %d", len(fake.capturedCalls))
	}
}
