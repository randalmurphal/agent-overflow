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

// TestHandleTurnStartCapturesPriorTurnCheckpoint pins the user-send-time
// capture pattern (matches Claude Code's QueryEngine.ts:641-655 behavior:
// fileHistoryMakeSnapshot fires at user-prompt-submit, not at turn end).
//
// Turn 0 start → baseline (checkpoint #0).
// Turn 0 events stream + complete → NO capture at turn end.
// Turn 1 start → captures the PRIOR turn's completion checkpoint (#1)
//
//	with the working tree state at the moment of user-send.
//
// Two captures total: baseline + prior-turn-completion.
func TestHandleTurnStartCapturesPriorTurnCheckpoint(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnIndex: 0,
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn 0 complete: %v", err)
	}

	// Mid-stream snapshot: only the baseline checkpoint exists. Capture
	// for turn 0 is deliberately deferred until the user submits the
	// next prompt.
	if len(fake.capturedCalls) != 1 {
		t.Fatalf("after turn 0 complete: capture calls = %d, want 1 (baseline only)", len(fake.capturedCalls))
	}
	if rows, err := st.ListCheckpoints("t1"); err != nil {
		t.Fatalf("list checkpoints after turn 0 complete: %v", err)
	} else if len(rows) != 1 || rows[0].CheckpointTurnCount != 0 {
		t.Fatalf("checkpoints after turn 0 complete = %+v, want only baseline", rows)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn 1 start: %v", err)
	}

	if len(fake.capturedCalls) != 2 {
		t.Fatalf("after turn 1 start: capture calls = %d, want 2 (baseline + prior-turn capture)", len(fake.capturedCalls))
	}
	if fake.capturedCalls[1].TurnIndex != 1 {
		t.Fatalf("prior-turn checkpoint turn count = %d, want 1", fake.capturedCalls[1].TurnIndex)
	}
	rows, err := st.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("checkpoint rows = %d, want 2", len(rows))
	}
}

// Errored turn metadata is preserved
// in the turns row at turn-end; capture happens at the NEXT turn-start
// and reads the prior turn's row to derive the checkpoint status. The
// "error" status flows through end-to-end.
func TestErroredTurnCheckpointAtNextTurnStart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0,
		TurnComplete: &provider.WireTurnCompleteMeta{
			ErrorMessage: "tool failed",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start (triggers prior-turn capture): %v", err)
	}

	if len(fake.capturedCalls) != 2 {
		t.Fatalf("capture calls = %d, want baseline + prior-turn capture", len(fake.capturedCalls))
	}
	if fake.capturedCalls[1].TurnIndex != 1 {
		t.Fatalf("prior-turn checkpoint turn = %d, want 1", fake.capturedCalls[1].TurnIndex)
	}
	got, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing: ok=%v err=%v", ok, err)
	}
	if got.Status != "error" {
		t.Fatalf("checkpoint status = %q, want error", got.Status)
	}
}

// Interrupted turn metadata is preserved on the turns row and surfaced in the
// next-turn-start capture. The Aborted flag derives from
// turn.StopReason == "interrupted" in capturePriorTurnCheckpoint.
func TestInterruptedTurnCheckpointAtNextTurnStart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0,
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason: "interrupted",
			Aborted:    true,
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start (triggers prior-turn capture): %v", err)
	}

	got, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing: ok=%v err=%v", ok, err)
	}
	if got.Status != "interrupted" {
		t.Fatalf("checkpoint status = %q, want interrupted", got.Status)
	}
}

// TestThirdTurnStartDoesNotReplacePriorCheckpoint pins that re-issuing
// EventTurnStart for an already-captured prior turn does NOT trigger a
// duplicate capture. Specifically: if turn 1 already started (which
// captured turn 0's checkpoint) and turn 2 starts (which captures turn
// 1's checkpoint), the turn 0 checkpoint is left alone — only the
// turn 1 capture is freshly fired.
func TestThirdTurnStartDoesNotReplacePriorCheckpoint(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	for _, idx := range []int{0, 1, 2} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: "t1",
			TurnID: fmt.Sprintf("turn-%d", idx), TurnIndex: idx,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn %d start: %v", idx, err)
		}
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: "t1",
			TurnID: fmt.Sprintf("turn-%d", idx), TurnIndex: idx,
			TurnComplete: normalTurnCompleteMeta(),
			Timestamp:    time.Now(),
		}); err != nil {
			t.Fatalf("turn %d complete: %v", idx, err)
		}
	}

	beforeCount := len(fake.capturedCalls)
	beforeTurn1, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing: ok=%v err=%v", ok, err)
	}

	// One more TurnStart (turn 3) — should re-capture for turn 2 only,
	// not turn 1 again.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-3", TurnIndex: 3, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 3 start: %v", err)
	}

	if got := len(fake.capturedCalls); got != beforeCount+1 {
		t.Fatalf("capture calls = %d, want %d (one prior-turn capture per turn-start)", got, beforeCount+1)
	}
	afterTurn1, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint turn 1 missing after turn 3 start: ok=%v err=%v", ok, err)
	}
	if afterTurn1.RefName != beforeTurn1.RefName || afterTurn1.CapturedAt != beforeTurn1.CapturedAt {
		t.Fatalf("checkpoint turn 1 mutated when turn 3 started: before=%+v after=%+v", beforeTurn1, afterTurn1)
	}
}

// TestPriorTurnCheckpointAtNextStartWhenBaselineMissing exercises the
// crash-recovery shape: a thread is resumed on turn 1 with no
// pre-existing baseline (turn 0's start was never observed). The
// captureCompletedTurnCheckpoint code path must still fire at the next
// turn-start, even though the previous (baseline) checkpoint is
// missing — the diff-against-previous step degrades gracefully to
// `files=nil` rather than aborting the capture.
func TestPriorTurnCheckpointAtNextStartWhenBaselineMissing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	// Seed a turns row at index 1 directly (skip turn 0 entirely so the
	// baseline checkpoint is missing). Then drive turn 2 start, which
	// must capture for the prior turn 1.
	if err := st.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  "t1",
		TurnIndex: 1,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed turn 1 row: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-2", TurnIndex: 2, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn 2 start: %v", err)
	}

	if len(fake.capturedCalls) != 1 {
		t.Fatalf("capture calls = %+v, want prior-turn capture even when baseline is missing", fake.capturedCalls)
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
	// First TurnStart fires the baseline capture (turn 0). Second
	// TurnStart resolves to turn_index=1 (LastTurnIndex from the
	// inserted item) and fires capturePriorTurnCheckpoint(0), which
	// looks up the turns row inserted by the FIRST TurnStart and
	// captures checkpoint #1. Two captures total — that's the
	// expected behavior under the user-send-time capture model.
	if len(fake.capturedCalls) != 2 {
		t.Errorf("expected baseline + prior-turn capture (2), got %d", len(fake.capturedCalls))
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
		ID:                  "stale-row",
		ThreadID:            "t1",
		CheckpointTurnCount: 0,
		RefName:             staleRef,
		CapturedAt:          1000,
		WorkspacePath:       "/tmp",
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

// Drives the full stage→commit→drain pipeline across two consecutive
// turns: tool start in turn 0 with file_path=a.txt, tool complete with
// is_error=false, turn complete; then tool start in turn 1 with
// file_path=b.txt, tool complete, turn complete. Both checkpoint rows
// must record the per-turn path. Provider parsers don't populate
// `evt.TurnIndex` on tool events (only TurnID is set), so this test
// guards against the regression of keying the per-turn set by the
// always-zero `evt.TurnIndex` rather than the router-tracked open turn.
func TestToolPathsRecordedPerTurnAcrossMultipleTurns(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	turn := func(idx int, itemID, filePath string) {
		t.Helper()
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: "t1",
			TurnID: fmt.Sprintf("turn-%d", idx), TurnIndex: idx,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn %d start: %v", idx, err)
		}
		toolMeta, _ := json.Marshal(map[string]any{
			"toolName": "Edit",
			"input":    map[string]any{"file_path": filePath},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1",
			ItemID: itemID, ItemType: "Edit",
			Meta:      toolMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn %d tool start: %v", idx, err)
		}
		completeMeta, _ := json.Marshal(map[string]any{"is_error": false})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: "t1",
			ItemID: itemID, ItemType: "Edit",
			Meta:      completeMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn %d tool complete: %v", idx, err)
		}
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: "t1",
			TurnID: fmt.Sprintf("turn-%d", idx), TurnIndex: idx,
			TurnComplete: normalTurnCompleteMeta(),
			Timestamp:    time.Now(),
		}); err != nil {
			t.Fatalf("turn %d complete: %v", idx, err)
		}
	}

	turn(0, "tool-0", "a.txt")
	turn(1, "tool-1", "b.txt")

	// Capture for each prior turn now happens at the NEXT TurnStart
	// (Claude Code parity). To exercise both checkpoints in this test,
	// fire one more TurnStart so turn 1's checkpoint is captured.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-2", TurnIndex: 2, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 2 start (triggers capture for turn 1): %v", err)
	}

	// Turn count = TurnIndex + 1; the per-turn checkpoints land at
	// counts 1 and 2.
	c1, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint 1 missing: ok=%v err=%v", ok, err)
	}
	if len(c1.ToolPaths) != 1 || c1.ToolPaths[0] != "a.txt" {
		t.Errorf("checkpoint 1 tool paths = %v, want [a.txt]", c1.ToolPaths)
	}
	c2, ok, err := st.GetCheckpointByTurnCount("t1", 2)
	if err != nil || !ok {
		t.Fatalf("checkpoint 2 missing: ok=%v err=%v", ok, err)
	}
	if len(c2.ToolPaths) != 1 || c2.ToolPaths[0] != "b.txt" {
		t.Errorf("checkpoint 2 tool paths = %v, want [b.txt]", c2.ToolPaths)
	}
}

// Same shape as the multi-turn test above, but the tool completes with
// is_error=true. Failed tools must NOT contribute to the per-turn path
// set (the file may or may not have been written, and the user may
// later edit the path manually — restoring it would silently overwrite).
func TestToolPathsDroppedOnFailedToolCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	toolMeta, _ := json.Marshal(map[string]any{
		"toolName": "Edit",
		"input":    map[string]any{"file_path": "rejected.txt"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1",
		ItemID: "tool-0", ItemType: "Edit", Meta: toolMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	failedMeta, _ := json.Marshal(map[string]any{"is_error": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1",
		ItemID: "tool-0", ItemType: "Edit", Meta: failedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	// Trigger the prior-turn capture by firing the next TurnStart.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start (triggers capture for turn 0): %v", err)
	}

	c, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint missing: ok=%v err=%v", ok, err)
	}
	if len(c.ToolPaths) != 0 {
		t.Errorf("failed tool should not record paths, got %v", c.ToolPaths)
	}
}

// TestMultiResultCheckpointCumulativeToolPaths pins the multi-result
// fix end-to-end at the checkpoint layer: two `result` envelopes for
// one logical user prompt produce ONE checkpoint at the next user-send
// containing the cumulative tool paths from both halves.
//
// The wire pattern (Claude task_notification → CLI synthesizes a
// `type:"user"` envelope → second `result`) results in:
//  1. First half: tool completes write a.txt → committedToolPaths[t1|0]=[a.txt].
//  2. First EventTurnComplete fires → handleTurnComplete runs but does
//     NOT capture (capture-at-next-user-send model). claimTurnSettlement
//     marks (t1, 0) as settled. clearOpenTurn clears open-turn pointer.
//  3. Second half: the wire keeps emitting tool events. settleToolPaths
//     falls back to LastTurnIndex (B5 fix) so paths attribute to turn
//     0, not a stale or wrong turn. committedToolPaths[t1|0]=[a.txt,b.txt].
//  4. Second EventTurnComplete fires → handleTurnComplete sees
//     settledTurns[t1|0] already set → returns early. Still no capture.
//  5. User sends turn 1 → handleTurnStart for turn 1 →
//     capturePriorTurnCheckpoint(t1, 0) → drains
//     committedToolPaths[t1|0]=[a.txt,b.txt] → captures checkpoint #1
//     with cumulative tool paths.
//
// Without the architectural fix, (a) the first capture would land at
// turn-end with only first-half paths, (b) second-half tool completes
// would drop their paths because openTurns was cleared, and (c) the
// final checkpoint would be missing b.txt — breaking path-scoped revert.
func TestMultiResultCheckpointCumulativeToolPaths(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	fake := &fakeCheckpointStore{isRepo: true}
	router.SetCheckpointStore(fake)

	editMeta := func(filePath string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{
			"toolName": "Edit",
			"input":    map[string]any{"file_path": filePath},
		})
		if err != nil {
			t.Fatalf("marshal edit meta: %v", err)
		}
		return raw
	}
	completeMeta := func() json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"is_error": false})
		return raw
	}

	// Turn 0 starts.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}

	// First half: Edit a.txt.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1",
		ItemID: "tool-a", ItemType: "Edit", Meta: editMeta("a.txt"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool a start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1",
		ItemID: "tool-a", ItemType: "Edit", Meta: completeMeta(),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool a complete: %v", err)
	}

	// First `result`. clearOpenTurn fires; settledTurns marks (t1,0).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first turn complete: %v", err)
	}

	// Second half — same logical turn, no fresh EventTurnStart.
	// settleToolPaths must fall back to LastTurnIndex so b.txt
	// attributes to turn 0 even though openTurns is empty.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1",
		ItemID: "tool-b", ItemType: "Edit", Meta: editMeta("b.txt"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool b start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1",
		ItemID: "tool-b", ItemType: "Edit", Meta: completeMeta(),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool b complete: %v", err)
	}

	// Second `result` for the same turn — idempotent guard returns early.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnID: "turn-0", TurnIndex: 0, TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second turn complete: %v", err)
	}

	// Up to this point NO completion checkpoint should exist — only the
	// baseline. This is the user-send-time capture model.
	if _, ok, err := st.GetCheckpointByTurnCount("t1", 1); err != nil {
		t.Fatalf("get checkpoint #1 mid-stream: %v", err)
	} else if ok {
		t.Fatal("checkpoint #1 should not exist before next user-send")
	}

	// User sends turn 1. capturePriorTurnCheckpoint drains the
	// cumulative paths and captures checkpoint #1.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1",
		TurnID: "turn-1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start (triggers prior-turn capture): %v", err)
	}

	c1, ok, err := st.GetCheckpointByTurnCount("t1", 1)
	if err != nil || !ok {
		t.Fatalf("checkpoint #1 missing after next user-send: ok=%v err=%v", ok, err)
	}
	if len(c1.ToolPaths) != 2 {
		t.Fatalf("checkpoint #1 tool paths = %v, want 2 entries (a.txt + b.txt cumulative)", c1.ToolPaths)
	}
	gotPaths := map[string]bool{}
	for _, p := range c1.ToolPaths {
		gotPaths[p] = true
	}
	if !gotPaths["a.txt"] {
		t.Errorf("checkpoint #1 missing a.txt: got %v", c1.ToolPaths)
	}
	if !gotPaths["b.txt"] {
		t.Errorf("checkpoint #1 missing b.txt (second-half write): got %v", c1.ToolPaths)
	}
}

// TestPriorTurnCheckpointFailureUnmarksDedup pins the rollback path
// of the capturePriorTurnCheckpoint dedup guard: when the underlying
// capture fails (CaptureBaseline returns error, SaveCheckpoint fails,
// etc.), the markTurnCaptured entry must be removed so a subsequent
// re-fired EventTurnStart can retry. Without the rollback the failed
// checkpoint would never be retried — silent permanent loss.
func TestPriorTurnCheckpointFailureUnmarksDedup(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Fake that fails CaptureBaseline. The first capturePriorTurnCheckpoint
	// call should fail, unmark the dedup entry, and a second call should
	// retry rather than no-op.
	fake := &fakeCheckpointStore{isRepo: true, captureErr: errors.New("boom")}
	router.SetCheckpointStore(fake)

	// Drive turn 0 start + complete so a turns row exists for turn 0.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnIndex: 0,
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
	baselineCalls := len(fake.capturedCalls)

	// First turn 1 start: prior-turn capture for turn 0 fails. The
	// dedup mark for (t1, checkpointTurnCount=1) should be rolled back.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		TurnID: "turn-1-a", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first turn 1 start: %v", err)
	}
	if got := len(fake.capturedCalls); got != baselineCalls+1 {
		t.Fatalf("first turn 1 start: capture calls = %d, want %d (one failed attempt)", got, baselineCalls+1)
	}

	router.mu.Lock()
	_, hasMark := router.capturedTurns["t1|1"]
	router.mu.Unlock()
	if hasMark {
		t.Errorf("capturedTurns[t1|1] still set after failure; rollback regressed — re-fired EventTurnStart will silently skip the retry")
	}

	// Drive another turn 1 start (re-fired EventTurnStart, e.g. Claude
	// system.init resend). The capture should retry — not no-op on the
	// stale dedup mark.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		TurnID: "turn-1-b", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second turn 1 start: %v", err)
	}
	if got := len(fake.capturedCalls); got != baselineCalls+2 {
		t.Errorf("second turn 1 start: capture calls = %d, want %d (retry after failure unmark)", got, baselineCalls+2)
	}
}
