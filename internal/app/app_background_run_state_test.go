package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// The tray reads a Claude background agent's run state from the live
// list, the one place it is served: a parked agent's row says parked and
// how many commands it waits on, and the stored launch row holds none of
// it.
func TestListLiveBackgroundTasks_ServesAParkedClaudeAgentsRunState(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread, err := createTestThread(t, app, "claude", "/tmp/w-park", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	handle := func(evt provider.ProviderEvent, fields map[string]any) {
		t.Helper()
		if fields != nil {
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("marshal meta: %v", err)
			}
			evt.Meta = raw
		}
		evt.ThreadID, evt.Timestamp = thread.ID, time.Now()
		if err := app.triage.Handle(evt); err != nil {
			t.Fatalf("handle %s %s: %v", evt.Kind, evt.ItemID, err)
		}
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: thread.ID + ":turn"}, nil)
	handle(provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "agent", ItemType: "Agent"},
		map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Spike", "prompt": "do the thing"}})
	handle(provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "agent"},
		map[string]any{"task_id": "task-agent", "task_type": "local_agent"})
	handle(provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "agent", Content: "Async agent launched successfully."},
		map[string]any{"is_background": true})
	handle(provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "shell", ParentToolUseID: "agent"},
		map[string]any{"task_id": "task-shell", "task_type": "local_bash", "parent_tool_use_id": "agent"})
	handle(provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "shell", ItemType: "Bash", ParentToolUseID: "agent"},
		map[string]any{"toolName": "Bash", "is_background": true, "input": map[string]any{"command": "sleep 60", "run_in_background": true}})
	handle(provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: "agent"},
		map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "completed", "source": "task_updated"})
	handle(provider.ProviderEvent{Kind: provider.EventBackgroundTaskNotification, ItemID: "agent", Content: "WAITING"},
		map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "completed", "uuid": "u1"})

	tasks, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks: %v", err)
	}
	var served map[string]any
	for _, task := range tasks {
		if task.ID == "agent" {
			if err := json.Unmarshal([]byte(task.Meta), &served); err != nil {
				t.Fatalf("decode served meta: %v", err)
			}
		}
	}
	if served == nil {
		t.Fatalf("the parked agent is missing from the live list: %+v", tasks)
	}
	if served["subagentRunState"] != "parked" || served["subagentParkedCommands"] != float64(1) {
		t.Errorf("served run state = %v / %v, want parked on 1", served["subagentRunState"], served["subagentParkedCommands"])
	}
	stored, found, err := app.store.GetThreadItemForWrite(thread.ID, "agent")
	if err != nil || !found {
		t.Fatalf("stored launch: found=%v err=%v", found, err)
	}
	if strings.Contains(stored.Meta, "subagentRunState") {
		t.Errorf("the launch row stores its run state: %s", stored.Meta)
	}
}
