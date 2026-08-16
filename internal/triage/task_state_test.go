package triage

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

	snapshot, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatalf("no persisted todo list")
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

	snapshot, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatalf("no persisted todo list")
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

	snapshot, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatalf("no persisted todo list")
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

	if _, ok := storedTodo(t, st, "t1"); ok {
		t.Errorf("persisted list should be cleared after deleting the only task")
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

	if _, ok := storedTodo(t, st, "t1"); ok {
		t.Errorf("persisted list should be cleared after deleting all tasks")
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
	if _, ok := storedTodo(t, st, "t1"); ok {
		t.Errorf("t1 list should be cleared")
	}
	s2, ok := storedTodo(t, st, "t2")
	if !ok || len(s2.Steps) != 1 || s2.Steps[0].Step != "t2 task" {
		t.Errorf("t2 list must be untouched by t1's clear; got %+v (ok=%v)", s2.Steps, ok)
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

	snapshot, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatalf("no persisted todo list")
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

	// The Task* correlation map is session state and dies with the session;
	// the projected list it produced is durable and must not.
	if _, ok := storedTodo(t, st, "t1"); !ok {
		t.Errorf("persisted todo list should survive CleanupThread")
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

	s1, ok1 := storedTodo(t, st, "t1")
	s2, ok2 := storedTodo(t, st, "t2")
	if !ok1 || !ok2 {
		t.Fatalf("expected persisted lists for both threads")
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

	snapshot, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatal("expected a list to survive — task state lives on the Router, not the Parser")
	}
	if len(snapshot.Steps) != 1 || snapshot.Steps[0].ID != "1" || snapshot.Steps[0].Status != "completed" {
		t.Fatalf("snapshot after simulated resume: %+v", snapshot.Steps)
	}
}

// The P1 the durable column introduced: the Task* correlation map dies with
// the session while the column — and the PROVIDER's own task list, which a
// plain resume keeps — survive it. A resumed session updates ids minted
// before the restart; a cold map must seed from the column or those events
// apply to nothing and the durable list freezes wrong forever.
func TestTaskUpdateOnColdRouterSeedsFromPersistedTodo(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	_ = router.Handle(taskCreateEvent("t1", "1", "first"))
	_ = router.Handle(taskCreateEvent("t1", "2", "second"))
	_ = router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "inProgress", Owner: "helper",
	}))

	// A fresh router over the same store is what an app restart leaves
	// behind (the provider session resumes with its task list intact).
	cold := NewRouter(st, func(eventName string, data any) {
		emissions.add(emitted{eventName, data})
	})
	if err := cold.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "completed",
	})); err != nil {
		t.Fatalf("cold task update: %v", err)
	}

	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 2 {
		t.Fatalf("stored = %+v (ok=%v), want the seeded 2-step list", stored, ok)
	}
	if stored.Steps[0].ID != "1" || stored.Steps[0].Status != "completed" || stored.Steps[0].Owner != "helper" {
		t.Fatalf("Steps[0] = %+v, want id 1 completed with seeded owner", stored.Steps[0])
	}
	if stored.Steps[1].ID != "2" || stored.Steps[1].Step != "second" || stored.Steps[1].Status != "pending" {
		t.Fatalf("Steps[1] = %+v, want seeded task 2 preserved in order", stored.Steps[1])
	}
	last := lastTodoEmission(emissions)
	if last == nil || len(last.Steps) != 2 {
		t.Fatalf("cold update must emit the full seeded list; got %+v", last)
	}
}

// Deletes are the sharpest edge of the cold-map defect: without the seed no
// event could EVER clear the column, because a delete against a nil map is a
// no-op and only Task* events touch this thread's list.
func TestTaskDeleteOnColdRouterClearsPersistedTodo(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	_ = router.Handle(taskCreateEvent("t1", "1", "only task"))

	cold := NewRouter(st, func(eventName string, data any) {
		emissions.add(emitted{eventName, data})
	})
	if err := cold.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Deleted: true,
	})); err != nil {
		t.Fatalf("cold task delete: %v", err)
	}

	if stored, ok := storedTodo(t, st, "t1"); ok {
		t.Fatalf("cold delete must clear the persisted list; got %+v", stored)
	}
	last := lastTodoEmission(emissions)
	if last == nil || last.ThreadID != "t1" || len(last.Steps) != 0 {
		t.Fatalf("cold delete-to-empty must emit the live clear; got %+v", last)
	}
}

// A TodoWrite/update_plan list carries no ids, so there is nothing for a
// Task* event to correlate against: the seed installs an empty map, an update
// finds nothing, and a create starts a fresh list — wholesale replacement,
// exactly as over a never-seeded thread. Note the "exactly as": this test's
// observable outcome is identical with the seed deleted, so it pins the
// id-less CONTRACT (a wrong seed that admitted id-less steps would fail the
// final assertion) rather than the seed's existence — the cold-router tests
// above are the ones that fail without it.
func TestColdSeedIgnoresStepsWithoutIDs(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"a","status":"pending"},{"step":"b","status":"pending"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}

	cold := NewRouter(st, func(string, any) {})
	if err := cold.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "completed",
	})); err != nil {
		t.Fatalf("cold task update: %v", err)
	}
	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 2 || stored.Steps[0].Step != "a" {
		t.Fatalf("an id-less list must be untouched by a Task* update; got %+v (ok=%v)", stored, ok)
	}

	if err := cold.Handle(taskCreateEvent("t1", "1", "fresh start")); err != nil {
		t.Fatalf("cold task create: %v", err)
	}
	stored, ok = storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 1 || stored.Steps[0].Step != "fresh start" {
		t.Fatalf("a create over an id-less list must replace it wholesale; got %+v (ok=%v)", stored, ok)
	}
}

// A resumed session appending a NEW task onto its carried-forward list: the
// create must land after the seeded prefix, not replace it — this is the
// transition where a wrong seed (or none) silently halves the list.
func TestTaskCreateOnColdRouterAppendsToSeededList(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	_ = router.Handle(taskCreateEvent("t1", "1", "first"))
	_ = router.Handle(taskCreateEvent("t1", "2", "second"))

	cold := NewRouter(st, func(string, any) {})
	if err := cold.Handle(taskCreateEvent("t1", "3", "added after the restart")); err != nil {
		t.Fatalf("cold task create: %v", err)
	}

	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 3 {
		t.Fatalf("stored = %+v (ok=%v), want the seeded pair plus the new task", stored, ok)
	}
	for i, want := range []string{"first", "second", "added after the restart"} {
		if stored.Steps[i].Step != want {
			t.Fatalf("Steps[%d].Step = %q, want %q (seeded order then append)", i, stored.Steps[i].Step, want)
		}
	}
}

// An all-completed stored list must NOT seed: the CLI (≥2.1.233) deletes a
// fully-completed list's task files 5s after the last completion while its
// high-water mark keeps later ids monotonic — but a FRESH list after a
// rollback-free restart still starts at "1" only when the old list is gone,
// and either way the old steps name tasks the provider discarded. Without the
// skip, the cold create below would find the old id in the seeded map and
// merge the finished list into the new one.
func TestColdSeedSkipsAnAllCompletedList(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	_ = router.Handle(taskCreateEvent("t1", "1", "old work"))
	_ = router.Handle(taskCreateEvent("t1", "2", "more old work"))
	for _, id := range []string{"1", "2"} {
		if err := router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
			TaskID: id, Status: "completed",
		})); err != nil {
			t.Fatalf("complete task %s: %v", id, err)
		}
	}

	cold := NewRouter(st, func(string, any) {})
	if err := cold.Handle(taskCreateEvent("t1", "1", "next list")); err != nil {
		t.Fatalf("cold task create: %v", err)
	}

	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 1 {
		t.Fatalf("stored = %+v (ok=%v), want ONLY the fresh list — completed steps must not resurrect", stored, ok)
	}
	if stored.Steps[0].ID != "1" || stored.Steps[0].Step != "next list" || stored.Steps[0].Status != "pending" {
		t.Fatalf("Steps[0] = %+v, want the new pending task, not the finished one it shares an id with", stored.Steps[0])
	}
}

// The seed runs only while the map is nil; once warm, the MAP is the truth
// and later column writes by the other producer family do not re-enter it.
// Observable end to end: a TodoWrite list overwrites the column, and the next
// Task* update still applies against the warm map and re-projects it.
func TestWarmTaskMapIsTruthOverLaterColumnWrites(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	_ = router.Handle(taskCreateEvent("t1", "1", "task one"))
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"todowrite list","status":"pending"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}

	if err := router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "1", Status: "completed",
	})); err != nil {
		t.Fatalf("task update: %v", err)
	}
	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 1 || stored.Steps[0].ID != "1" || stored.Steps[0].Status != "completed" {
		t.Fatalf("stored = %+v (ok=%v), want the warm map's task list re-projected", stored, ok)
	}
}

// A stored blob this build cannot read must not wedge the rail: an update
// over the failed seed applies to nothing (the provider's state is untouched,
// a later event retries), while a create starts a fresh list and OVERWRITES
// the unreadable blob — the overwrite is the heal, and the error is still
// reported. Blocking creates on a seed error would leave the blob in place
// for the thread's lifetime.
func TestTaskCreateHealsAnUnreadableStoredList(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "triage.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	router := NewRouter(st, func(string, any) {})
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// json_valid passes the CHECK; the strict decoder refuses the unknown
	// field. Written through a second handle because no accessor can produce
	// this — which is the point.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.Exec(
		`UPDATE threads SET live_todo = ? WHERE id = 't1'`,
		`{"steps":[{"step":"drifted","status":"pending","id":"9"}],"drifted":true}`,
	); err != nil {
		t.Fatalf("seed drifted blob: %v", err)
	}

	if err := router.Handle(taskUpdateEvent("t1", provider.TaskUpdateMeta{
		TaskID: "9", Status: "completed",
	})); err == nil {
		t.Fatalf("a failed seed must be reported on the update path")
	}

	err = router.Handle(taskCreateEvent("t1", "1", "fresh list"))
	if err == nil {
		t.Fatalf("a failed seed must be reported on the create path too")
	}
	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 1 || stored.Steps[0].Step != "fresh list" {
		t.Fatalf("the create must overwrite the unreadable blob; stored = %+v (ok=%v)", stored, ok)
	}
}

// ResetThreadTodo is the app-side contract seedTasksFromStoredTodo leans on:
// a from-scratch restart of the SAME thread row (rollback, provider switch)
// must drop both halves — the correlation map and the column — and push the
// live clear so a pane drops the dead list too.
func TestResetThreadTodoDropsMapAndColumnAndEmits(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	_ = router.Handle(taskCreateEvent("t1", "1", "doomed"))

	countBefore := todoEmissionCount(emissions)
	if err := router.ResetThreadTodo("t1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, ok := storedTodo(t, st, "t1"); ok {
		t.Fatalf("reset must clear the column")
	}
	router.mu.Lock()
	_, warm := router.tasksByThread["t1"]
	router.mu.Unlock()
	if warm {
		t.Fatalf("reset must drop the correlation map")
	}
	last := lastTodoEmission(emissions)
	if got := todoEmissionCount(emissions) - countBefore; got != 1 || last == nil || len(last.Steps) != 0 {
		t.Fatalf("reset over a stored list must emit exactly the live clear; got %d new, last=%+v", got, last)
	}
	// Idempotent: a second reset has nothing to clear and stays silent.
	if err := router.ResetThreadTodo("t1"); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if got := todoEmissionCount(emissions) - countBefore; got != 1 {
		t.Fatalf("a reset over nothing must not emit; got %d new emissions", got)
	}
}
