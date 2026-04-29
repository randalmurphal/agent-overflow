package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestTimelineNotificationsUseTurnWideIDs(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventPlanUpdate,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"completed"}]}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("plan update: %v", err)
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
	if notifications[0].ToolName != "plan_update" || notifications[1].ToolName != "warning" {
		t.Fatalf("tool names = %q, %q; want plan_update, warning", notifications[0].ToolName, notifications[1].ToolName)
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
