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
	for _, e := range *emissions {
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

	for _, e := range *emissions {
		if e.eventName == "provider:todo_update" {
			t.Fatalf("empty todos must not emit; got %+v", e)
		}
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
