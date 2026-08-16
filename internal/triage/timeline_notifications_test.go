package triage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestTimelineNotificationsUseTurnWideIDs(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Two distinct notification kinds (review_status + warning) so the
	// turn-wide ID counter can be exercised. EventTodoUpdate used to
	// land here too; it now bypasses persistence — see
	// TestTodoUpdateEmitsWithoutPersistence.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  "t1",
		Content:   "Code review started",
		Meta:      json.RawMessage(`{"kind":"review_status","title":"Code review started"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("review status: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  "t1",
		Content:   "Config warning",
		Meta:      json.RawMessage(`{"kind":"warning","title":"Config warning"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("warning: %v", err)
	}

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 2 {
		t.Fatalf("notifications = %d, want 2: %+v", len(notifications), notifications)
	}
	if notifications[0].ID != "notification:0:0" || notifications[1].ID != "notification:0:1" {
		t.Fatalf("notification IDs = %q, %q; want notification:0:0, notification:0:1", notifications[0].ID, notifications[1].ID)
	}
	if notifications[0].ToolName != "review_status" || notifications[1].ToolName != "warning" {
		t.Fatalf("tool names = %q, %q; want review_status, warning", notifications[0].ToolName, notifications[1].ToolName)
	}
}

// TestTodoUpdateEmitsWithoutTimelineRow pins the contract: EventTodoUpdate
// produces a provider:todo_update emission for the frontend live panel and
// writes no timeline row — a todo list is state about the conversation, not
// an entry in it. It IS persisted, on the thread row.
func TestTodoUpdateEmitsWithoutTimelineRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"inProgress"},{"step":"two","status":"pending"}]}`),
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("plan update: %v", err)
	}

	if notifications := findItemsByKind(t, st, "t1", itemKindNotification); len(notifications) != 0 {
		t.Fatalf("todo_update must not persist a notification row; got %d", len(notifications))
	}

	var todoEmits int
	for _, e := range emissions.snapshot() {
		if e.eventName != "provider:todo_update" {
			continue
		}
		todoEmits++
		payload, ok := e.data.(TodoUpdateEvent)
		if !ok {
			t.Fatalf("provider:todo_update payload type = %T, want TodoUpdateEvent", e.data)
		}
		if payload.ThreadID != "t1" {
			t.Errorf("ThreadID = %q, want t1", payload.ThreadID)
		}
		want := []TodoStep{
			{Step: "one", Status: "inProgress"},
			{Step: "two", Status: "pending"},
		}
		if !reflect.DeepEqual(payload.Steps, want) {
			t.Errorf("Steps = %+v, want %+v", payload.Steps, want)
		}
	}
	if todoEmits != 1 {
		t.Fatalf("expected exactly 1 provider:todo_update emit, got %d", todoEmits)
	}

	stored, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatalf("expected a persisted todo list")
	}
	wantStored := []store.ThreadLiveTodoStep{
		{Step: "one", Status: "inProgress"},
		{Step: "two", Status: "pending"},
	}
	if stored.UpdatedAt != 1_700_000_000_000 || !reflect.DeepEqual(stored.Steps, wantStored) {
		t.Fatalf("stored todo = %+v, want updatedAt=1700000000000 steps=%+v", stored, wantStored)
	}
}

// TestTodoUpdateEmptyDropsSilently guards the defensive empty check —
// a malformed or empty wire payload must not produce a no-op emission
// the frontend would render as "no todos".
func TestTodoUpdateEmptyDropsSilently(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	for _, raw := range []string{
		`{"plan":[]}`,
		`{}`,
		``,
		`{"plan":[{"step":"","status":"pending"}]}`,
	} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTodoUpdate,
			ThreadID:  "t1",
			Meta:      json.RawMessage(raw),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("todo update %q: %v", raw, err)
		}
	}

	for _, e := range emissions.snapshot() {
		if e.eventName == "provider:todo_update" {
			t.Fatalf("empty todos must not emit; got %+v", e)
		}
	}
	if stored, ok := storedTodo(t, st, "t1"); ok {
		t.Fatalf("empty todos must not persist a list; got %+v", stored)
	}
}

func TestTodoUpdateEmptyClearsPersistedTodo(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"inProgress"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	if _, ok := storedTodo(t, st, "t1"); !ok {
		t.Fatalf("expected a persisted todo list before the clear")
	}
	countBefore := todoEmissionCount(emissions)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("empty todo update: %v", err)
	}
	if stored, ok := storedTodo(t, st, "t1"); ok {
		t.Fatalf("empty todo update did not clear the persisted list: %+v", stored)
	}
	// Clearing a prior live snapshot must reach live panes too, not just the
	// backend refresh copy — an in-memory pane only re-reads the backend on
	// refresh.
	if got := todoEmissionCount(emissions) - countBefore; got != 1 {
		t.Fatalf("clearing a prior snapshot: got %d new emissions, want 1 (the live clear)", got)
	}
	if last := lastTodoEmission(emissions); last == nil || len(last.Steps) != 0 {
		t.Fatalf("clear emission must carry empty steps; got %+v", last)
	}
}

// Triage stores what the provider reported and nothing else: the age filter
// that hides a finished list belongs to the reader (app_live_state.go), so a
// stale all-completed list must still be on the row. Persisting it is what
// lets the reader decide — and what makes a list the provider merely finished
// distinguishable from one it cleared.
func TestTodoUpdateStoresCompletedListVerbatim(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	reportedAt := time.Now().Add(-10 * time.Second)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"done","status":"completed"}]}`),
		Timestamp: reportedAt,
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	stored, ok := storedTodo(t, st, "t1")
	if !ok {
		t.Fatalf("a completed list must still be stored; triage does not age lists out")
	}
	if stored.UpdatedAt != reportedAt.UnixMilli() {
		t.Fatalf("UpdatedAt = %d, want the reporting event's time %d", stored.UpdatedAt, reportedAt.UnixMilli())
	}
}

// A persistence failure must not cost the user the live update they are
// looking at — but it must be reported. The failure is forced with a thread
// that has no row, which is what a deletion racing the write looks like.
func TestTodoUpdateStillEmitsWhenThePersistFails(t *testing.T) {
	router, _, emissions := newTestRouter(t)

	err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "no-such-thread",
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"pending"}]}`),
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatalf("a failed todo persist must be reported, got nil error")
	}
	last := lastTodoEmission(emissions)
	if last == nil || last.ThreadID != "no-such-thread" || len(last.Steps) != 1 {
		t.Fatalf("the live update must still be emitted after a persist failure; got %+v", last)
	}
}

// Surviving teardown is the point of v65: a session ending is not the user
// finishing their list.
func TestCleanupThreadPreservesPersistedTodo(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"inProgress"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	router.CleanupThread("t1")
	stored, ok := storedTodo(t, st, "t1")
	if !ok || len(stored.Steps) != 1 || stored.Steps[0].Step != "one" {
		t.Fatalf("todo list did not survive CleanupThread: found=%v stored=%+v", ok, stored)
	}
}

// TestDecodeTodoStepsTruncatesIDAndOwner pins the wire-input bounds
// on the optional Task* fields. The owner and id originate in model
// output and the rail/widget receive whatever the parser emits, so the
// rune caps are the defense against a runaway provider stuffing a
// multi-MB string into a single snapshot field. Step bounds are
// covered indirectly elsewhere; this is the direct boundary check.
func TestDecodeTodoStepsTruncatesIDAndOwner(t *testing.T) {
	// Build owner/id strings that straddle the cap exactly (64 runes
	// passes through; 65 runes truncates).
	longOwner := strings.Repeat("o", 65)
	longID := strings.Repeat("i", 65)
	exactOwner := strings.Repeat("p", 64)
	exactID := strings.Repeat("j", 64)
	raw := json.RawMessage(`{"plan":[
		{"step":"first","status":"pending","id":"` + exactID + `","owner":"` + exactOwner + `"},
		{"step":"second","status":"pending","id":"` + longID + `","owner":"` + longOwner + `"}
	]}`)
	steps := decodeTodoSteps(raw)
	if len(steps) != 2 {
		t.Fatalf("steps: got %d, want 2", len(steps))
	}
	if steps[0].ID != exactID || steps[0].Owner != exactOwner {
		t.Errorf("step[0]: 64-rune values must pass through; got id=%q owner=%q", steps[0].ID, steps[0].Owner)
	}
	if !strings.HasSuffix(steps[1].ID, "...") || len([]rune(steps[1].ID)) != 67 {
		t.Errorf("step[1].id: expected truncation to 64 runes + \"...\"; got %q", steps[1].ID)
	}
	if !strings.HasSuffix(steps[1].Owner, "...") || len([]rune(steps[1].Owner)) != 67 {
		t.Errorf("step[1].owner: expected truncation to 64 runes + \"...\"; got %q", steps[1].Owner)
	}
}

func TestTimelineNotificationMetaIsAllowlisted(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventNotification,
		ThreadID: "t1",
		Content:  "pre-commit hook (completed)",
		Meta: json.RawMessage(`{
			"kind":"hook",
			"title":"pre-commit hook (completed)",
			"secret":"do-not-store",
			"run":{
				"eventName":"pre-commit",
				"status":"completed",
				"entries":[
					{"kind":"stdout","text":"visible hook output"},
					{"kind":"stderr","text":"another line"}
				]
			}
		}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("hook notification: %v", err)
	}

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifications))
	}
	meta := notifications[0].Meta
	if strings.Contains(meta, "do-not-store") {
		t.Fatalf("meta retained raw secret field: %s", meta)
	}
	if !strings.Contains(meta, "visible hook output") {
		t.Fatalf("meta dropped rendered hook entry: %s", meta)
	}
}

// The clear's emit gate answers "is a pane showing something", and the
// store's "was anything stored" is only a proxy for it — a proxy a failed
// call cannot supply. A SET that failed just before may have left a pane
// showing a list over an empty column; a clear gated on the store would then
// never take it down. So a failed clear emits: the eager-clear residue heals
// on the next refresh, the gated-clear residue never does.
func TestTodoClearStillEmitsWhenTheClearFails(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"pending"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	// Closing under the fixture's own t.Cleanup(st.Close) is deliberate and
	// benign: sql.DB.Close is idempotent, and the cleanup's second close only
	// logs its failed WAL checkpoint. The closed store is what forces the
	// clear to fail.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	countBefore := todoEmissionCount(emissions)
	err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[]}`),
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatalf("a failed clear must be reported, got nil error")
	}
	if got := todoEmissionCount(emissions) - countBefore; got != 1 {
		t.Fatalf("a failed clear must still emit the live clear; got %d new emissions", got)
	}
	last := lastTodoEmission(emissions)
	if last == nil || last.ThreadID != "t1" || len(last.Steps) != 0 {
		t.Fatalf("the failed clear's frame must be the empty list; got %+v", last)
	}
}
