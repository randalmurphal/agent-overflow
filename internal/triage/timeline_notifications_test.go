package triage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
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

// TestTodoUpdateEmitsWithoutPersistence pins the new contract:
// EventTodoUpdate produces a provider:todo_update emission for the
// frontend live panel and writes nothing to the items table — the
// snapshot lives only in pane state.
func TestTodoUpdateEmitsWithoutPersistence(t *testing.T) {
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

	snapshot, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatalf("expected live todo snapshot")
	}
	want := []TodoStep{
		{Step: "one", Status: "inProgress"},
		{Step: "two", Status: "pending"},
	}
	if snapshot.ThreadID != "t1" || snapshot.UpdatedAt != 1_700_000_000_000 || !reflect.DeepEqual(snapshot.Steps, want) {
		t.Fatalf("live todo snapshot = %+v, want thread=t1 updatedAt=1700000000000 steps=%+v", snapshot, want)
	}

	snapshot.Steps[0].Step = "mutated"
	again, ok := router.LiveTodoSnapshot("t1")
	if !ok {
		t.Fatalf("expected live todo snapshot after copy mutation")
	}
	if again.Steps[0].Step != "one" {
		t.Fatalf("LiveTodoSnapshot returned map-owned steps slice; got %+v", again.Steps)
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
	if snapshot, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Fatalf("empty todos must not store live snapshot; got %+v", snapshot)
	}
}

func TestTodoUpdateEmptyClearsPriorLiveSnapshot(t *testing.T) {
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
	if _, ok := router.LiveTodoSnapshot("t1"); !ok {
		t.Fatalf("expected live todo snapshot before clear")
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("empty todo update: %v", err)
	}
	if snapshot, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Fatalf("empty todo update did not clear live snapshot: %+v", snapshot)
	}
}

func TestLiveTodoSnapshotExpiresCompletedSnapshot(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"done","status":"completed"}]}`),
		Timestamp: time.Now().Add(-10 * time.Second),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	if snapshot, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Fatalf("completed live todo snapshot should expire: %+v", snapshot)
	}
	if snapshot := router.LiveStateSnapshotForThread("t1"); snapshot.Todo != nil {
		t.Fatalf("live state returned expired todo snapshot: %+v", snapshot.Todo)
	}
}

func TestCleanupThreadClearsLiveTodoSnapshot(t *testing.T) {
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
	if _, ok := router.LiveTodoSnapshot("t1"); !ok {
		t.Fatalf("expected live todo snapshot before cleanup")
	}
	router.CleanupThread("t1")
	if snapshot, ok := router.LiveTodoSnapshot("t1"); ok {
		t.Fatalf("live todo snapshot leaked after cleanup: %+v", snapshot)
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
