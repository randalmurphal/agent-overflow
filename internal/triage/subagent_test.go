package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// --- Gap 3: ParentToolUseID propagation through triage ---
//
// Triage preserves ParentToolUseID on every outbound emission so the
// frontend and SQLite persistence layer can group subagent work by Task
// parent. Also persists the field on the timeline item when the event
// results in a store insert.

func TestParentToolUseIDFlowsThroughInlineEmit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        "t1",
		ItemID:          "tool-1",
		ItemType:        "Bash",
		ParentToolUseID: "task_tool_abc",
		Timestamp:       time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	inline := filterEmissions(*emissions, "provider:event")
	if len(inline) != 1 {
		t.Fatalf("expected 1 provider:event emission, got %d", len(inline))
	}

	emitted, ok := inline[0].data.(provider.ProviderEvent)
	if !ok {
		t.Fatalf("emitted payload not a ProviderEvent: %T", inline[0].data)
	}
	if emitted.ParentToolUseID != "task_tool_abc" {
		t.Errorf("ParentToolUseID on emission: got %q, want %q",
			emitted.ParentToolUseID, "task_tool_abc")
	}
}

func TestParentToolUseIDPersistsOnTurnText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Simulate a streaming subagent text delta followed by turn complete.
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "subagent result",
		ParentToolUseID: "task_tool_99",
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTurnComplete,
		ThreadID:        "t1",
		ParentToolUseID: "task_tool_99",
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.Summary == "subagent result" {
			if it.ParentID != providerScopedItemID("task_tool_99") {
				t.Errorf("persisted item ParentID: got %q, want %q",
					it.ParentID, providerScopedItemID("task_tool_99"))
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected item 'subagent result', items=%+v", items)
	}
}

func TestParentToolUseIDEmptyWhenAbsent(t *testing.T) {
	// Backward compat: events without a ParentToolUseID emit an empty field
	// and persist items with an empty column value.
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "top-level text",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	if len(*emissions) < 1 {
		t.Fatalf("expected at least 1 emission, got %d", len(*emissions))
	}
	for i, e := range *emissions {
		emitted, ok := e.data.(provider.ProviderEvent)
		if !ok {
			continue
		}
		if emitted.ParentToolUseID != "" {
			t.Errorf("emission[%d] ParentToolUseID: got %q, want empty",
				i, emitted.ParentToolUseID)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.ParentID != "" {
			t.Errorf("persisted item %q ParentID: got %q, want empty",
				it.ID, it.ParentID)
		}
	}
}
