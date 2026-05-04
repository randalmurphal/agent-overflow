package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// mustCreateThreadForTurn creates a thread owned by defaultTestProjectID
// so the turns.thread_id FK is satisfied. Mirrors the pattern used by
// mustCreateThreadForCheckpoint in checkpoints_test.go.
func mustCreateThreadForTurn(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CreateThread(makeThread(id, "claude")); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

// makeInflightTurn builds a turn with completed_at=NULL, matching the
// shape triage writes at turn-start.
func makeInflightTurn(turnID, threadID string, turnIndex int, startedAt int64) Turn {
	return Turn{
		TurnID:    turnID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	}
}

func TestInsertTurnPersistsRowWithNullCompletedAt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	turn := makeInflightTurn("turn-1", "t1", 0, 1000)
	if err := s.InsertTurn(turn); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	got, ok, err := s.GetTurn("turn-1")
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if !ok {
		t.Fatal("expected inserted turn to be found")
	}
	if got.TurnID != "turn-1" {
		t.Errorf("turn_id = %q, want turn-1", got.TurnID)
	}
	if got.ThreadID != "t1" {
		t.Errorf("thread_id = %q, want t1", got.ThreadID)
	}
	if got.TurnIndex != 0 {
		t.Errorf("turn_index = %d, want 0", got.TurnIndex)
	}
	if got.StartedAt != 1000 {
		t.Errorf("started_at = %d, want 1000", got.StartedAt)
	}
	if got.CompletedAt != nil {
		t.Errorf("completed_at = %v, want nil (in-flight)", *got.CompletedAt)
	}
	if got.StopReason != "" {
		t.Errorf("stop_reason = %q, want empty", got.StopReason)
	}
	if got.AssistantMessageID != "" {
		t.Errorf("assistant_message_id = %q, want empty", got.AssistantMessageID)
	}
	if got.TokenUsageJSON != "" {
		t.Errorf("token_usage_json = %q, want empty", got.TokenUsageJSON)
	}
	if got.ErrorMessage != "" {
		t.Errorf("error_message = %q, want empty", got.ErrorMessage)
	}
}

func TestInsertTurnAcceptsZeroTurnIndex(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-0", "t1", 0, 1)); err != nil {
		t.Fatalf("insert turn at turn_index=0: %v", err)
	}

	got, ok, err := s.GetTurn("turn-0")
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}
	if got.TurnIndex != 0 {
		t.Errorf("turn_index = %d, want 0", got.TurnIndex)
	}
}

func TestInsertTurnRejectsNegativeTurnIndex(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	err := s.InsertTurn(makeInflightTurn("turn-neg", "t1", -1, 1))
	if err == nil {
		t.Fatal("expected CHECK constraint violation for turn_index = -1")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
	}
}

func TestInsertTurnRejectsDuplicateThreadTurnIndex(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-a", "t1", 0, 1)); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Same (thread_id, turn_index) with a different turn_id must hit
	// the UNIQUE(thread_id, turn_index) constraint — triage relies on
	// monotonicity.
	err := s.InsertTurn(makeInflightTurn("turn-b", "t1", 0, 2))
	if err == nil {
		t.Fatal("expected UNIQUE constraint violation on (thread_id, turn_index)")
	}
	if !strings.Contains(err.Error(), "UNIQUE") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want UNIQUE constraint violation", err)
	}
}

func TestInsertTurnRejectsDuplicateTurnID(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")
	mustCreateThreadForTurn(t, s, "t2")

	if err := s.InsertTurn(makeInflightTurn("turn-x", "t1", 0, 1)); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// turn_id is the primary key — duplicate turn_id across threads is
	// also an error. In practice providers never emit the same turn_id
	// for two threads, but this guards the contract.
	err := s.InsertTurn(makeInflightTurn("turn-x", "t2", 0, 1))
	if err == nil {
		t.Fatal("expected primary key violation for duplicate turn_id")
	}
}

func TestInsertTurnRejectsEmptyTurnID(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("", "t1", 0, 1)); err == nil {
		t.Fatal("expected error for empty turn_id")
	}
}

func TestInsertTurnRejectsUnknownThread(t *testing.T) {
	s := newTestStore(t)
	// No CreateThread call — FK should fail.

	err := s.InsertTurn(makeInflightTurn("turn-orphan", "missing-thread", 0, 1))
	if err == nil {
		t.Fatal("expected FK violation for missing thread_id")
	}
}

func TestUpdateTurnCompletedFlipsFields(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	turn := makeInflightTurn("turn-1", "t1", 0, 1000)
	if err := s.InsertTurn(turn); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	err := s.UpdateTurnCompleted(
		"turn-1",
		2000,
		"end_turn",
		"msg-final",
		`{"inputTokens":100,"outputTokens":50}`,
		"",
	)
	if err != nil {
		t.Fatalf("update turn completed: %v", err)
	}

	got, ok, err := s.GetTurn("turn-1")
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}

	// started_at must be preserved — the update only touches settle fields.
	if got.StartedAt != 1000 {
		t.Errorf("started_at after update = %d, want 1000 (preserved)", got.StartedAt)
	}
	if got.CompletedAt == nil || *got.CompletedAt != 2000 {
		t.Errorf("completed_at = %v, want 2000", got.CompletedAt)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", got.StopReason)
	}
	if got.AssistantMessageID != "msg-final" {
		t.Errorf("assistant_message_id = %q, want msg-final", got.AssistantMessageID)
	}
	if got.TokenUsageJSON != `{"inputTokens":100,"outputTokens":50}` {
		t.Errorf("token_usage_json = %q, want tokens JSON", got.TokenUsageJSON)
	}
	if got.ErrorMessage != "" {
		t.Errorf("error_message = %q, want empty", got.ErrorMessage)
	}
	// turn_index is immutable across the settle — it was set at InsertTurn
	// and must not shift.
	if got.TurnIndex != 0 {
		t.Errorf("turn_index = %d, want 0 (preserved)", got.TurnIndex)
	}
}

func TestUpdateTurnCompletedWithErrorMessage(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-err", "t1", 0, 1)); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	err := s.UpdateTurnCompleted("turn-err", 100, "error", "", "", "api connection reset")
	if err != nil {
		t.Fatalf("update turn: %v", err)
	}

	got, ok, err := s.GetTurn("turn-err")
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}
	if got.StopReason != "error" {
		t.Errorf("stop_reason = %q, want error", got.StopReason)
	}
	if got.ErrorMessage != "api connection reset" {
		t.Errorf("error_message = %q, want 'api connection reset'", got.ErrorMessage)
	}
}

// TestUpdateTurnLatePayload pins the per-column semantics of the
// late-payload fold: token usage uses first-non-empty-wins (preserves
// the first settle's value); assistant_message_id uses
// last-non-empty-wins (each subsequent round overwrites so the column
// reflects the FINAL assistant message of the turn). Both columns
// share the no-op rule for empty inputs.
func TestUpdateTurnLatePayload(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "thread-1")

	// Fresh row — both columns empty after the soft settle that
	// did not include either field.
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-late",
		ThreadID:  "thread-1",
		TurnIndex: 0,
		StartedAt: 100,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-late", 200, "end_turn", "", "", ""); err != nil {
		t.Fatalf("update completed: %v", err)
	}

	// First late fold writes both columns.
	if err := s.UpdateTurnLatePayload("turn-late", `{"inputTokens":1}`, "msg_first"); err != nil {
		t.Fatalf("first late fold: %v", err)
	}
	got, ok, err := s.GetTurn("turn-late")
	if err != nil || !ok {
		t.Fatalf("get turn: ok=%v err=%v", ok, err)
	}
	if got.TokenUsageJSON != `{"inputTokens":1}` {
		t.Fatalf("token_usage_json after first fold = %q, want %q", got.TokenUsageJSON, `{"inputTokens":1}`)
	}
	if got.AssistantMessageID != "msg_first" {
		t.Fatalf("assistant_message_id after first fold = %q, want %q", got.AssistantMessageID, "msg_first")
	}

	// Second late fold (e.g. round-2 result after round-1 settle in
	// a multi-result-per-turn cascade): usage stays first-write-wins,
	// amid OVERWRITES with the later round's id.
	if err := s.UpdateTurnLatePayload("turn-late", `{"inputTokens":99}`, "msg_round2"); err != nil {
		t.Fatalf("second late fold: %v", err)
	}
	got, _, _ = s.GetTurn("turn-late")
	if got.TokenUsageJSON != `{"inputTokens":1}` {
		t.Fatalf("usage clobbered (should be first-write-wins): got %q", got.TokenUsageJSON)
	}
	if got.AssistantMessageID != "msg_round2" {
		t.Fatalf("amid not overwritten (should be last-write-wins): got %q, want %q",
			got.AssistantMessageID, "msg_round2")
	}

	// Empty amid input is a no-op for amid (preserves whatever the
	// row already has) — usage path same; both empty is a no-op
	// for the whole call.
	if err := s.UpdateTurnLatePayload("turn-late", "", ""); err != nil {
		t.Fatalf("empty fold: %v", err)
	}
	got, _, _ = s.GetTurn("turn-late")
	if got.AssistantMessageID != "msg_round2" {
		t.Fatalf("empty fold disturbed amid: got %q", got.AssistantMessageID)
	}
	if got.TokenUsageJSON != `{"inputTokens":1}` {
		t.Fatalf("empty fold disturbed usage: got %q", got.TokenUsageJSON)
	}

	// Empty amid with non-empty usage: usage path runs (and no-ops
	// because already populated), amid is preserved.
	if err := s.UpdateTurnLatePayload("turn-late", `{"inputTokens":42}`, ""); err != nil {
		t.Fatalf("usage-only fold: %v", err)
	}
	got, _, _ = s.GetTurn("turn-late")
	if got.AssistantMessageID != "msg_round2" {
		t.Fatalf("usage-only fold disturbed amid: got %q", got.AssistantMessageID)
	}

	// Non-empty amid with empty usage: amid overwrites, usage
	// preserved.
	if err := s.UpdateTurnLatePayload("turn-late", "", "msg_round3"); err != nil {
		t.Fatalf("amid-only fold: %v", err)
	}
	got, _, _ = s.GetTurn("turn-late")
	if got.AssistantMessageID != "msg_round3" {
		t.Fatalf("amid-only fold did not overwrite: got %q, want %q",
			got.AssistantMessageID, "msg_round3")
	}
	if got.TokenUsageJSON != `{"inputTokens":1}` {
		t.Fatalf("amid-only fold disturbed usage: got %q", got.TokenUsageJSON)
	}
}

func TestUpdateTurnCompletedRejectsUnknownTurn(t *testing.T) {
	s := newTestStore(t)

	err := s.UpdateTurnCompleted("nonexistent", 1, "end_turn", "", "", "")
	if err == nil {
		t.Fatal("expected error for unknown turn_id")
	}
	// requireRowsAffected wraps sql.ErrNoRows — exercising that contract
	// so callers can switch on errors.Is if they want to.
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("error chain = %v, want sql.ErrNoRows", err)
	}
}

func TestUpdateTurnCompletedRejectsEmptyTurnID(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpdateTurnCompleted("", 1, "end_turn", "", "", ""); err == nil {
		t.Fatal("expected error for empty turn_id")
	}
}

func TestGetTurnReturnsFalseForMissingRow(t *testing.T) {
	s := newTestStore(t)

	got, ok, err := s.GetTurn("missing")
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if ok {
		t.Fatal("expected found=false for missing turn")
	}
	if got.TurnID != "" {
		t.Errorf("expected zero Turn for miss, got %+v", got)
	}
}

func TestListRecentTurnsOrdersNewestFirst(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	// Insert out of order to prove the ORDER BY does the sorting.
	for _, turn := range []Turn{
		makeInflightTurn("turn-2", "t1", 2, 3000),
		makeInflightTurn("turn-0", "t1", 0, 1000),
		makeInflightTurn("turn-1", "t1", 1, 2000),
	} {
		if err := s.InsertTurn(turn); err != nil {
			t.Fatalf("insert %s: %v", turn.TurnID, err)
		}
	}

	got, err := s.ListRecentTurns("t1", 10)
	if err != nil {
		t.Fatalf("list recent turns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(got))
	}
	wantOrder := []string{"turn-2", "turn-1", "turn-0"}
	for i, want := range wantOrder {
		if got[i].TurnID != want {
			t.Errorf("turns[%d].TurnID = %q, want %q", i, got[i].TurnID, want)
		}
	}
}

func TestListRecentTurnsRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	for i := 0; i < 5; i++ {
		turn := makeInflightTurn(
			"turn-"+string(rune('0'+i)),
			"t1",
			i,
			int64(i+1)*1000,
		)
		if err := s.InsertTurn(turn); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
	}

	got, err := s.ListRecentTurns("t1", 2)
	if err != nil {
		t.Fatalf("list recent turns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 turns with limit=2, got %d", len(got))
	}
	// The two most recent are turn-4 and turn-3.
	if got[0].TurnID != "turn-4" {
		t.Errorf("got[0].TurnID = %q, want turn-4", got[0].TurnID)
	}
	if got[1].TurnID != "turn-3" {
		t.Errorf("got[1].TurnID = %q, want turn-3", got[1].TurnID)
	}
}

func TestListRecentTurnsFiltersByThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")
	mustCreateThreadForTurn(t, s, "t2")

	if err := s.InsertTurn(makeInflightTurn("t1-turn-0", "t1", 0, 1000)); err != nil {
		t.Fatalf("insert t1 turn: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("t2-turn-0", "t2", 0, 2000)); err != nil {
		t.Fatalf("insert t2 turn: %v", err)
	}

	got, err := s.ListRecentTurns("t1", 10)
	if err != nil {
		t.Fatalf("list t1 turns: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 turn for t1, got %d", len(got))
	}
	if got[0].TurnID != "t1-turn-0" {
		t.Errorf("got[0].TurnID = %q, want t1-turn-0 (scoping failed)", got[0].TurnID)
	}
}

func TestListRecentTurnsEmptyLimitReturnsNil(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")
	if err := s.InsertTurn(makeInflightTurn("turn-0", "t1", 0, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// limit=0 should short-circuit without hitting the DB; assert no rows.
	got, err := s.ListRecentTurns("t1", 0)
	if err != nil {
		t.Fatalf("list turns limit=0: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice for limit=0, got %d", len(got))
	}

	// Same for negative.
	got, err = s.ListRecentTurns("t1", -5)
	if err != nil {
		t.Fatalf("list turns limit=-5: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice for limit=-5, got %d", len(got))
	}
}

func TestListRecentTurnsEmptyThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	got, err := s.ListRecentTurns("t1", 10)
	if err != nil {
		t.Fatalf("list recent turns: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 turns for empty thread, got %d", len(got))
	}
}

func TestListRecentTurnsIncludesCompletedAtNullRows(t *testing.T) {
	// Rehydration on thread-switch calls this; if the store accidentally
	// filtered out in-flight rows, the frontend couldn't render the
	// "interrupted" marker for a crashed turn.
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-done", "t1", 0, 1)); err != nil {
		t.Fatalf("insert done: %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-done", 100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("settle turn-done: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("turn-inflight", "t1", 1, 200)); err != nil {
		t.Fatalf("insert inflight: %v", err)
	}

	got, err := s.ListRecentTurns("t1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 turns (done + inflight), got %d", len(got))
	}
	// Newest first: turn-inflight then turn-done.
	if got[0].TurnID != "turn-inflight" {
		t.Errorf("got[0].TurnID = %q, want turn-inflight", got[0].TurnID)
	}
	if got[0].CompletedAt != nil {
		t.Errorf("turn-inflight.CompletedAt = %v, want nil", *got[0].CompletedAt)
	}
	if got[1].CompletedAt == nil || *got[1].CompletedAt != 100 {
		t.Errorf("turn-done.CompletedAt = %v, want 100", got[1].CompletedAt)
	}
}

func TestGetActiveTurnReturnsOnlyInflightRow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	// Settled turn-0.
	if err := s.InsertTurn(makeInflightTurn("turn-done", "t1", 0, 1)); err != nil {
		t.Fatalf("insert done: %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-done", 100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// Active turn-1.
	if err := s.InsertTurn(makeInflightTurn("turn-active", "t1", 1, 200)); err != nil {
		t.Fatalf("insert active: %v", err)
	}

	got, ok, err := s.GetActiveTurn("t1")
	if err != nil {
		t.Fatalf("get active turn: %v", err)
	}
	if !ok {
		t.Fatal("expected active turn to be found")
	}
	if got.TurnID != "turn-active" {
		t.Errorf("turn_id = %q, want turn-active", got.TurnID)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil (the active row must be in-flight)", *got.CompletedAt)
	}
}

func TestGetActiveTurnReturnsMostRecentWhenMultipleInflight(t *testing.T) {
	// Normal triage serialises turn-start so at most one in-flight row
	// exists per thread, but a crash could leave an older in-flight row
	// alongside a newer one. The frontend wants the most recent — it's
	// what matches any live provider:turn_started push.
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-stale", "t1", 0, 1)); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("turn-newest", "t1", 2, 3)); err != nil {
		t.Fatalf("insert newest: %v", err)
	}
	if err := s.InsertTurn(makeInflightTurn("turn-mid", "t1", 1, 2)); err != nil {
		t.Fatalf("insert mid: %v", err)
	}

	got, ok, err := s.GetActiveTurn("t1")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if !ok {
		t.Fatal("expected an active turn to be found")
	}
	if got.TurnID != "turn-newest" {
		t.Errorf("turn_id = %q, want turn-newest (highest turn_index wins)", got.TurnID)
	}
}

func TestGetActiveTurnReturnsFalseWhenAllSettled(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-done", "t1", 0, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-done", 100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("settle: %v", err)
	}

	got, ok, err := s.GetActiveTurn("t1")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if ok {
		t.Fatalf("expected no active turn when all settled, got %+v", got)
	}
}

func TestGetActiveTurnReturnsFalseForEmptyThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	_, ok, err := s.GetActiveTurn("t1")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if ok {
		t.Fatal("expected false for thread with no turns")
	}
}

func TestTurnsCascadeOnThreadDelete(t *testing.T) {
	// turns.thread_id ON DELETE CASCADE mirrors every other per-thread
	// table (items, checkpoints, attachments); deleting the thread must
	// drop its turn rows rather than leaving dangling FKs.
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "t1")

	if err := s.InsertTurn(makeInflightTurn("turn-1", "t1", 0, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE thread_id = 't1'`).Scan(&count); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if count != 0 {
		t.Errorf("expected CASCADE to drop turn rows, still have %d", count)
	}
}
