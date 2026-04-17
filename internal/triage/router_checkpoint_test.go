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
	isRepo         bool
	captureErr     error
	deleteRefErr   error
	capturedCalls  []fakeCaptureCall
	deleteRefCalls []string
	// liveRefs tracks git refs the fake has "written", so tests can assert
	// the router cleans them up on rollback or idempotent recapture.
	liveRefs map[string]bool
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
	ref := fmt.Sprintf("refs/agent-overflow/checkpoints/%s/turn/%d", threadID, turnIndex)
	if f.liveRefs == nil {
		f.liveRefs = make(map[string]bool)
	}
	f.liveRefs[ref] = true
	return ref, nil
}

func (f *fakeCheckpointStore) DeleteRef(_ context.Context, _ string, ref string) error {
	f.deleteRefCalls = append(f.deleteRefCalls, ref)
	if f.deleteRefErr != nil {
		return f.deleteRefErr
	}
	if f.liveRefs != nil {
		delete(f.liveRefs, ref)
	}
	return nil
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
	// Simulate a fresh StartSession for the same thread. Bug B5's fix
	// requires an EventInit to clear the stopped-thread flag set by
	// CleanupThread before further events persist.
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventInit,
		ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle reinit: %v", err)
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

// TestCaptureBaselineIdempotentOnStalePair reproduces the crashed-mid-capture
// scenario: a prior capture left a DB row + git ref in place, but the in-
// memory dedupe state was lost (e.g. app restart, CleanupThread fired). A
// fresh turn-start must tear down the stale pair and rewrite both sides in
// lockstep. Without A1 the v8 UNIQUE constraint makes the second capture's
// SaveCheckpoint fail and strands the fresh git ref.
func TestCaptureBaselineIdempotentOnStalePair(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	// Seed a pre-existing (row, ref) for (t1, turn 0). This is what a
	// crash-mid-capture or reboot would leave behind.
	staleRef := "refs/agent-overflow/checkpoints/t1/turn/0-stale"
	if fake.liveRefs == nil {
		fake.liveRefs = make(map[string]bool)
	}
	fake.liveRefs[staleRef] = true
	if err := st.SaveCheckpoint(store.Checkpoint{
		ID:            "stale-row",
		ThreadID:      "t1",
		TurnIndex:     0,
		RefName:       staleRef,
		CapturedAt:    1000,
		WorkspacePath: "/tmp",
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	// Turn-start. The idempotent path must: drop the stale row, delete the
	// stale ref, capture anew, save a fresh row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	rows, err := st.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 checkpoint row after idempotent recapture, got %d", len(rows))
	}
	if rows[0].ID == "stale-row" {
		t.Errorf("expected fresh row; stale row survived")
	}
	if fake.liveRefs[staleRef] {
		t.Errorf("stale ref %q should have been deleted", staleRef)
	}
	// Exactly one live ref left — the new one.
	if live := len(fake.liveRefs); live != 1 {
		t.Errorf("expected 1 live ref, got %d", live)
	}
	sawCaptured := false
	for _, e := range *emissions {
		if e.eventName == "checkpoint:captured" {
			sawCaptured = true
		}
	}
	if !sawCaptured {
		t.Errorf("expected checkpoint:captured emission")
	}
}

// TestCaptureBaselineDeletesRefWhenThreadGoneBeforeSave drives the
// rollback-after-DB-failure path. After the router's idempotent cleanup but
// before the new SaveCheckpoint, we delete the parent thread. The FK
// constraint on thread_checkpoints.thread_id causes SaveCheckpoint to fail,
// and the router MUST delete the just-written git ref so neither side leaks.
// Without A1's rollback, the fresh git ref would dangle with no DB row.
func TestCaptureBaselineDeletesRefWhenThreadGoneBeforeSave(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// fake synchronises DB mutation with capture so we can race the FK.
	fake := &ddlBombCheckpointStore{
		isRepo: true,
		beforeSaveHook: func() {
			// Delete the thread mid-capture: the upcoming SaveCheckpoint
			// will FK-fail.
			if err := st.DeleteThread("t1"); err != nil {
				t.Logf("delete thread in hook: %v", err)
			}
		},
	}
	router.SetCheckpointStore(fake)

	// Trigger capture. Handle must not error (checkpoint failure is
	// non-fatal) but the router must have called DeleteRef on the ref it
	// just wrote.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Assertions: no DB row survived (thread was deleted so cascade cleared
	// anything), no ref is live.
	rows, _ := st.ListCheckpoints("t1")
	if len(rows) != 0 {
		t.Errorf("expected 0 checkpoint rows after FK failure, got %d", len(rows))
	}
	if live := len(fake.liveRefs); live != 0 {
		t.Errorf("expected 0 live refs after rollback, got %d", live)
	}
	if len(fake.deleteRefCalls) == 0 {
		t.Errorf("expected DeleteRef to be called on rollback")
	}
}

// ddlBombCheckpointStore extends fakeCheckpointStore with a hook that runs
// after CaptureBaseline but before the caller (router) issues SaveCheckpoint.
// Useful for simulating race conditions between git-ref write and DB write.
type ddlBombCheckpointStore struct {
	isRepo         bool
	captureErr     error
	capturedCalls  []fakeCaptureCall
	deleteRefCalls []string
	liveRefs       map[string]bool
	beforeSaveHook func()
}

func (f *ddlBombCheckpointStore) IsGitRepository(_ context.Context, _ string) bool {
	return f.isRepo
}

func (f *ddlBombCheckpointStore) CaptureBaseline(_ context.Context, workspace, threadID string, turnIndex int) (string, error) {
	f.capturedCalls = append(f.capturedCalls, fakeCaptureCall{
		Workspace: workspace,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
	})
	if f.captureErr != nil {
		return "", f.captureErr
	}
	ref := fmt.Sprintf("refs/agent-overflow/checkpoints/%s/turn/%d", threadID, turnIndex)
	if f.liveRefs == nil {
		f.liveRefs = make(map[string]bool)
	}
	f.liveRefs[ref] = true
	// Fire the bomb between git-write and DB-write.
	if f.beforeSaveHook != nil {
		f.beforeSaveHook()
	}
	return ref, nil
}

func (f *ddlBombCheckpointStore) DeleteRef(_ context.Context, _ string, ref string) error {
	f.deleteRefCalls = append(f.deleteRefCalls, ref)
	if f.liveRefs != nil {
		delete(f.liveRefs, ref)
	}
	return nil
}

// TestCaptureBaselineIdempotentAcrossReCaptures re-fires TurnStart twice on
// the same (thread, turn) with the in-memory dedupe cleared between. Both
// calls must succeed and the end state must be exactly one row + one ref —
// the old ref must be deleted.
func TestCaptureBaselineIdempotentAcrossReCaptures(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	// Clear the in-memory dedupe so the router will actually recapture the
	// same (thread, turn) — exercising the idempotent cleanup path.
	router.CleanupThread("t1")

	// Bug B5's fix marks the thread stopped on CleanupThread; a fresh
	// StartSession would emit EventInit before any further TurnStart.
	// Model that here so the recapture is routed to the live path.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle init: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle 2: %v", err)
	}

	rows, err := st.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected exactly 1 row after recapture, got %d", len(rows))
	}
	if len(fake.deleteRefCalls) == 0 {
		t.Errorf("expected DeleteRef to be called for the stale ref")
	}
	if live := len(fake.liveRefs); live != 1 {
		t.Errorf("expected exactly 1 live ref after recapture, got %d", live)
	}
}
