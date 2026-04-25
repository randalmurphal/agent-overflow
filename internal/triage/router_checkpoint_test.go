package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/diffsummary"
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

func (f *fakeCheckpointStore) DiffRefToRef(_ context.Context, _ string, _, _ string) ([]byte, error) {
	return nil, nil
}

func (f *fakeCheckpointStore) DiffRefToRefSummary(_ context.Context, _ string, _, _ string) ([]diffsummary.File, error) {
	return nil, nil
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

func TestHandleTurnStartOnlyCapturesInitialBaseline(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	for _, turnIndex := range []int{0, 1} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  "t1",
			TurnIndex: turnIndex,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle turn %d: %v", turnIndex, err)
		}
	}

	if len(fake.capturedCalls) != 1 {
		t.Fatalf("capture calls = %d, want initial baseline only", len(fake.capturedCalls))
	}
	if fake.capturedCalls[0].TurnIndex != 0 {
		t.Fatalf("first checkpoint turn = %d, want 0", fake.capturedCalls[0].TurnIndex)
	}

	rows, err := st.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("checkpoint rows = %d, want 1", len(rows))
	}
	if rows[0].CheckpointTurnCount != 0 {
		t.Fatalf("checkpoint turn = %d, want 0", rows[0].CheckpointTurnCount)
	}
}

func TestHandleTurnCompleteCapturesErroredTurnCheckpoint(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		TurnIndex: 0,
		Meta:      json.RawMessage(`{"error":"tool failed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	if len(fake.capturedCalls) != 2 {
		t.Fatalf("capture calls = %d, want baseline + completed checkpoint", len(fake.capturedCalls))
	}
	if fake.capturedCalls[1].TurnIndex != 1 {
		t.Fatalf("completed checkpoint turn = %d, want 1", fake.capturedCalls[1].TurnIndex)
	}
	got, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing: ok=%v err=%v", ok, err)
	}
	if got.Status != "error" {
		t.Fatalf("checkpoint status = %q, want error", got.Status)
	}
}

func TestHandleTurnCompleteCapturesInterruptedTurnCheckpoint(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		TurnIndex: 0,
		Meta:      json.RawMessage(`{"turn_status":"interrupted"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	got, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing: ok=%v err=%v", ok, err)
	}
	if got.Status != "interrupted" {
		t.Fatalf("checkpoint status = %q, want interrupted", got.Status)
	}
}

func TestHandleTurnStartDoesNotReplaceCompletedCheckpoint(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start turn 0: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		TurnID:    "turn-0",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete turn 0: %v", err)
	}
	before, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing before next start: ok=%v err=%v", ok, err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start turn 1: %v", err)
	}

	after, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing after next start: ok=%v err=%v", ok, err)
	}
	if after.RefName != before.RefName || after.CapturedAt != before.CapturedAt {
		t.Fatalf("checkpoint turn 1 changed after next turn start: before=%+v after=%+v", before, after)
	}
	if len(fake.capturedCalls) != 2 {
		t.Fatalf("capture calls = %d, want baseline + completed checkpoint only", len(fake.capturedCalls))
	}
}

func TestHandleTurnCompleteCapturesCheckpointWhenPreviousMissing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)
	router.setOpenTurn("t1", 1)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	if len(fake.capturedCalls) != 1 {
		t.Fatalf("capture calls = %+v, want completed checkpoint even when prior checkpoint is missing", fake.capturedCalls)
	}
	if fake.capturedCalls[0].TurnIndex != 2 {
		t.Fatalf("captured checkpoint turn = %d, want 2", fake.capturedCalls[0].TurnIndex)
	}
	if _, ok, err := st.GetCheckpointByTurnCount("t1", 2); err != nil || !ok {
		t.Fatalf("checkpoint turn 2 ok=%v err=%v, want present", ok, err)
	}
	if got := len(filterEmissions(*emissions, "checkpoint:error")); got != 0 {
		t.Fatalf("checkpoint:error emissions = %d, want 0 when capture succeeds", got)
	}
}

func TestHandleTurnStartCapturesWorkspacePathWhenWorktreeMetadataIsStale(t *testing.T) {
	// WorkspacePath is the provider cwd. WorktreePath is retained as metadata
	// for owned worktrees, so checkpoint capture must follow WorkspacePath.
	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t-wt",
		ProjectID:     triageTestProjectID,
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
	if fake.capturedCalls[0].Workspace != "/tmp/project" {
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

	// After cleanup, later turn starts must route normally but must not
	// synthesize a checkpoint. Checkpoints after turn 0 are captured on
	// completion, where the previous checkpoint boundary is known.
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
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "next",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle second turn: %v", err)
	}
	if len(fake.capturedCalls) != 1 {
		t.Errorf("expected only the initial baseline capture, got %d", len(fake.capturedCalls))
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

func (f *ddlBombCheckpointStore) DiffRefToRef(_ context.Context, _ string, _, _ string) ([]byte, error) {
	return nil, nil
}

func (f *ddlBombCheckpointStore) DiffRefToRefSummary(_ context.Context, _ string, _, _ string) ([]diffsummary.File, error) {
	return nil, nil
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
