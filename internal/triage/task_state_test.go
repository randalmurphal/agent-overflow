package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
)

func taskCreateEvent(threadID, taskID, subject string) provider.ProviderEvent {
	meta, _ := json.Marshal(provider.TaskCreateMeta{TaskID: taskID, Subject: subject})
	return provider.ProviderEvent{
		Kind:      provider.EventTaskCreate,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func taskUpdateEvent(threadID string, meta provider.TaskUpdateMeta) provider.ProviderEvent {
	raw, _ := json.Marshal(meta)
	return provider.ProviderEvent{
		Kind:      provider.EventTaskUpdate,
		ThreadID:  threadID,
		Meta:      raw,
		Timestamp: time.Now(),
	}
}

func lastTodoEmission(emissions *emissionLog) *TodoUpdateEvent {
	snap := emissions.snapshot()
	for i := len(snap) - 1; i >= 0; i-- {
		e := snap[i]
		if e.eventName == "provider:todo_update" {
			if update, ok := e.data.(TodoUpdateEvent); ok {
				return &update
			}
		}
	}
	return nil
}

func todoEmissionCount(emissions *emissionLog) int {
	n := 0
	for _, e := range emissions.snapshot() {
		if e.eventName == "provider:todo_update" {
			n++
		}
	}
	return n
}

func TestHandleTaskCreateEmitsSnapshot(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(taskCreateEvent("t1", "1", "first task")); err != nil {
		t.Fatalf("handle: %v", err)
	}

	update := lastTodoEmission(emissions)
	if update == nil {
		t.Fatalf("no provider:todo_update emission")
	}
	if len(update.Steps) != 1 {
		t.Fatalf("steps: got %d, want 1", len(update.Steps))
	}
	if update.Steps[0].ID != "1" || update.Steps[0].Step != "first task" || update.Steps[0].Status != "pending" {
		t.Errorf("step: got %+v, want {ID:1 Step:first task Status:pending}", update.Steps[0])
	}
}

func TestHandleTaskCreateSequentialPreservesOrder(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	for _, tc := range []struct{ id, subject string }{
		{"1", "first"}, {"2", "second"}, {"3", "third"},
	} {
		if err := router.Handle(taskCreateEvent("t1", tc.id, tc.subject)); err != nil {
			t.Fatalf("create %s: %v", tc.id, err)
		}
	}

	update := lastTodoEmission(emissions)
	if update == nil {
		t.Fatalf("no emission")
	}
	if len(update.Steps) != 3 {
		t.Fatalf("steps: got %d, want 3", len(update.Steps))
	}
	for i, want := range []string{"1", "2", "3"} {
		if update.Steps[i].ID != want {
			t.Errorf("step[%d].ID: got %q, want %q", i, update.Steps[i].ID, want)
		}
	}
}

func TestHandleTaskUpdateUnknownIDNoop(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "99", Status: "completed",
	})); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := lastTodoEmission(emissions); got != nil {
		t.Errorf("expected no emission for unknown id, got %+v", got)
	}
}

func TestHandleTaskUpdateOwnerPreservesStatus(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "task"))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "inProgress",
	}))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Owner: "helper",
	}))

	snapshot, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatalf("no live snapshot")
	}
	if snapshot.Steps[0].Status != "inProgress" {
		t.Errorf("status: got %q, want inProgress (preserved)", snapshot.Steps[0].Status)
	}
	if snapshot.Steps[0].Owner != "helper" {
		t.Errorf("owner: got %q, want helper", snapshot.Steps[0].Owner)
	}
}

func TestHandleTaskUpdateNoOpPreservesAllFields(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "task"))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "completed", Owner: "claimant",
	}))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1",
	}))

	snapshot, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatalf("no live snapshot")
	}
	s := snapshot.Steps[0]
	if s.Step != "task" || s.Status != "completed" || s.Owner != "claimant" {
		t.Errorf("step: got %+v, want {task completed claimant}", s)
	}
}

func TestHandleTaskUpdateDeletedRemovesMiddleEntry(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "first"))
	_ = router.Handle(taskCreateEvent("t1", "2", "second"))
	_ = router.Handle(taskCreateEvent("t1", "3", "third"))

	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "2", Deleted: true,
	}))

	snapshot, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatalf("no live snapshot")
	}
	if len(snapshot.Steps) != 2 {
		t.Fatalf("steps: got %d, want 2", len(snapshot.Steps))
	}
	if snapshot.Steps[0].ID != "1" || snapshot.Steps[1].ID != "3" {
		t.Errorf("order: got [%s, %s], want [1, 3]", snapshot.Steps[0].ID, snapshot.Steps[1].ID)
	}
}

// TestHandleTaskDeleteOnlyEmitsLiveClear pins the fix for the 2026-06-14
// todo-not-clearing incident: deleting the last task must EMIT an empty
// provider:todo_update so a live pane drops the list — not merely clear the
// backend refresh snapshot. A live pane holds the last non-empty snapshot in
// memory and only re-reads the backend copy on refresh/reconnect, so without
// the emit the cleared list lingers on screen. The backend snapshot is still
// cleared for the refresh path.
func TestHandleTaskDeleteOnlyEmitsLiveClear(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "only task"))
	countBefore := todoEmissionCount(emissions)

	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Deleted: true,
	}))

	if _, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Errorf("live snapshot should be cleared after deleting the only task")
	}
	if got := todoEmissionCount(emissions) - countBefore; got != 1 {
		t.Fatalf("delete-to-empty: got %d new emissions, want 1 (the live clear)", got)
	}
	last := lastTodoEmission(emissions)
	if last == nil {
		t.Fatalf("no provider:todo_update emission for the clear")
	}
	if last.ThreadID != "t1" {
		t.Errorf("clear emission threadID: got %q, want t1", last.ThreadID)
	}
	if len(last.Steps) != 0 {
		t.Errorf("clear emission must carry empty steps; got %+v", last.Steps)
	}
}

// TestHandleTaskDeleteAllEmitsFinalClear reproduces the 2026-06-14 screenshot:
// three tasks created then deleted one at a time. Each intermediate delete
// emits the shrinking list; the final delete (list → empty) must emit an
// empty-steps clear so the activity-rail Todos widget empties instead of
// freezing on the last surviving item ("Write tests" in the report).
func TestHandleTaskDeleteAllEmitsFinalClear(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "Draft the API schema"))
	_ = router.Handle(taskCreateEvent("t1", "2", "Implement the handler"))
	_ = router.Handle(taskCreateEvent("t1", "3", "Write tests"))

	for _, id := range []string{"1", "2", "3"} {
		if err := router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
			TaskID: id, Deleted: true,
		})); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
	}

	if _, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Errorf("backend snapshot should be cleared after deleting all tasks")
	}
	last := lastTodoEmission(emissions)
	if last == nil {
		t.Fatalf("no provider:todo_update emission")
	}
	if last.ThreadID != "t1" {
		t.Errorf("final clear emission threadID: got %q, want t1", last.ThreadID)
	}
	if len(last.Steps) != 0 {
		t.Errorf("final emission after deleting all tasks must be an empty clear; got %+v", last.Steps)
	}
}

// TestHandleTaskDeleteClearIsThreadScoped pins that draining one thread's list
// to empty clears only that thread — the incident was a per-pane targeting
// failure, so the clear must carry the right ThreadID and must not disturb a
// concurrent thread's live snapshot.
func TestHandleTaskDeleteClearIsThreadScoped(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")
	seedOpenTurn(t, router, st, "t1", 0)
	seedOpenTurn(t, router, st, "t2", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "t1 task"))
	_ = router.Handle(taskCreateEvent("t2", "1", "t2 task"))

	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{TaskID: "1", Deleted: true}))

	last := lastTodoEmission(emissions)
	if last == nil || last.ThreadID != "t1" || len(last.Steps) != 0 {
		t.Fatalf("expected an empty clear for t1; got %+v", last)
	}
	if _, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Errorf("t1 snapshot should be cleared")
	}
	s2, ok := router.LiveTodoSnapshot("t2")
	if !ok || len(s2.Steps) != 1 || s2.Steps[0].Step != "t2 task" {
		t.Errorf("t2 snapshot must be untouched by t1's clear; got %+v (ok=%v)", s2.Steps, ok)
	}
}

// TestTaskStepsTruncatesOversizedFields pins the defense-in-depth cap on the
// Task* projection: Subject/Owner are model-controlled and only TrimSpace'd
// upstream, so taskStepsLocked must truncate them to the same maxTodo*Runes
// bounds the legacy TodoWrite path enforces — otherwise an oversized field
// reaches the WS payload and the in-memory pane snapshot unbounded.
func TestTaskStepsTruncatesOversizedFields(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	longSubject := strings.Repeat("x", maxTodoStepRunes+50)
	longOwner := strings.Repeat("y", maxTodoOwnerRunes+50)

	_ = router.Handle(taskCreateEvent("t1", "1", longSubject))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{TaskID: "1", Owner: longOwner}))

	update := lastTodoEmission(emissions)
	if update == nil || len(update.Steps) != 1 {
		t.Fatalf("expected one projected step; got %+v", update)
	}
	step := update.Steps[0]
	if got := utf8.RuneCountInString(step.Step); got > maxTodoStepRunes+len("...") {
		t.Errorf("subject not truncated: %d runes (cap %d + ellipsis)", got, maxTodoStepRunes)
	}
	if got := utf8.RuneCountInString(step.Owner); got > maxTodoOwnerRunes+len("...") {
		t.Errorf("owner not truncated: %d runes (cap %d + ellipsis)", got, maxTodoOwnerRunes)
	}
}

func TestHandleTaskCreateDuplicateIDPreservesStatus(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "original"))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "inProgress",
	}))
	_ = router.Handle(taskCreateEvent("t1", "1", "replayed subject"))

	snapshot, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatalf("no live snapshot")
	}
	if snapshot.Steps[0].Status != "inProgress" {
		t.Errorf("status: got %q, want inProgress (preserved)", snapshot.Steps[0].Status)
	}
	if snapshot.Steps[0].Step != "replayed subject" {
		t.Errorf("subject: got %q, want replayed subject", snapshot.Steps[0].Step)
	}
}

func TestTasksByThreadClearedOnCleanupThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "task"))
	router.CleanupThread("t1")

	if _, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Errorf("live snapshot should be cleared after CleanupThread")
	}
	router.mu.Lock()
	_, exists := router.tasksByThread["t1"]
	router.mu.Unlock()
	if exists {
		t.Errorf("tasksByThread should be cleared after CleanupThread")
	}
}

func TestTasksByThreadIsolatedPerThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")
	seedOpenTurn(t, router, st, "t1", 0)
	seedOpenTurn(t, router, st, "t2", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "t1 only"))
	_ = router.Handle(taskCreateEvent("t2", "1", "t2 only"))

	s1, ok1 := router.LiveTodoSnapshot("t1")
	s2, ok2 := router.LiveTodoSnapshot("t2")
	if !ok1 || !ok2 {
		t.Fatalf("expected snapshots for both threads")
	}
	if s1.Steps[0].Step != "t1 only" || s2.Steps[0].Step != "t2 only" {
		t.Errorf("thread isolation violated: t1=%+v t2=%+v", s1.Steps, s2.Steps)
	}
}

// TestTaskUpdateSurvivesParserRecreation is the load-bearing regression
// test for the session-resume gap. Task state lives on the Router (not
// on per-Session Parser instances), so a TaskUpdate against an id
// created before session resume produces a correct snapshot. Before
// the seam migration this failed because the fresh Parser had no
// tasksByID entry for the pre-resume id.
func TestTaskUpdateSurvivesParserRecreation(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	_ = router.Handle(taskCreateEvent("t1", "1", "pre-resume task"))

	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "completed",
	}))

	snapshot, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatal("expected snapshot to survive — task state lives on the Router, not the Parser")
	}
	if len(snapshot.Steps) != 1 || snapshot.Steps[0].ID != "1" || snapshot.Steps[0].Status != "completed" {
		t.Fatalf("snapshot after simulated resume: %+v", snapshot.Steps)
	}
}
