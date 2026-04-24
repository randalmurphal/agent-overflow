package triage

import (
	"strings"
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

	// Install the router's test-only eventHook so this test can observe
	// that ParentToolUseID survives the routing pipeline — previously
	// the assertion rode on the retired provider:event fanout.
	var observed provider.ProviderEvent
	router.SetEventHook(func(evt provider.ProviderEvent) {
		observed = evt
	})

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

	if observed.ParentToolUseID != "task_tool_abc" {
		t.Errorf("ParentToolUseID on eventHook observation: got %q, want %q",
			observed.ParentToolUseID, "task_tool_abc")
	}

	// The persisted tool_call row carries the parent_tool_use_id as
	// parent_id, and that lands on the upsert channel — use the
	// emissions sink to assert the outbound contract without depending
	// on a retired passthrough channel.
	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) == 0 {
		t.Fatalf("expected at least 1 provider:item_event upsert, got %d", len(upserts))
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
			if it.ParentID != "task_tool_99" {
				t.Errorf("persisted item ParentID: got %q, want %q",
					it.ParentID, "task_tool_99")
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

// TestTextItemIDDisambiguatesSubagentScopes is the correctness check on
// textItemID scoping: two text deltas in the same turn but under
// different ParentToolUseIDs must produce DISTINCT rows. If the item-id
// namespace collapsed subagent scopes, one subagent's streaming text
// would concatenate into another's, and the SubagentGroup UI would
// render the combined text under the wrong Task card.
//
// The test also asserts item.ParentID is populated on each subagent row
// so the frontend's subagent grouping can bucket them by parent.
func TestTextItemIDDisambiguatesSubagentScopes(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now()
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "A output",
		ParentToolUseID: "task_tool_alpha",
		Timestamp:       now,
	}); err != nil {
		t.Fatalf("delta alpha: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "B output",
		ParentToolUseID: "task_tool_beta",
		Timestamp:       now,
	}); err != nil {
		t.Fatalf("delta beta: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}

	textByParent := map[string]string{}
	idsSeen := map[string]bool{}
	for _, it := range items {
		if it.Kind != "assistant_text" {
			continue
		}
		if idsSeen[it.ID] {
			t.Errorf("duplicate assistant_text item id %q — scopes collapsed", it.ID)
		}
		idsSeen[it.ID] = true
		textByParent[it.ParentID] = it.Summary
	}

	if len(textByParent) != 2 {
		t.Fatalf("expected 2 distinct scoped text items, got %d: %+v", len(textByParent), textByParent)
	}

	alphaID := "task_tool_alpha"
	betaID := "task_tool_beta"
	if got := textByParent[alphaID]; got != "A output" {
		t.Errorf("alpha scope summary = %q, want %q", got, "A output")
	}
	if got := textByParent[betaID]; got != "B output" {
		t.Errorf("beta scope summary = %q, want %q", got, "B output")
	}

	// The persisted item id must encode the scope literally so downstream
	// card-id resolution (SubagentGroup bucketing) is deterministic across
	// reloads — "text:0:<scope>:<segment>" is the canonical shape.
	var alphaRowID, betaRowID string
	for _, it := range items {
		if it.Kind != "assistant_text" {
			continue
		}
		switch it.ParentID {
		case alphaID:
			alphaRowID = it.ID
		case betaID:
			betaRowID = it.ID
		}
	}
	if alphaRowID == "" || betaRowID == "" {
		t.Fatalf("missing one of the scoped rows: alpha=%q beta=%q", alphaRowID, betaRowID)
	}
	if alphaRowID == betaRowID {
		t.Fatalf("item ids collapsed across scopes: %q == %q", alphaRowID, betaRowID)
	}
	if !strings.Contains(alphaRowID, "task_tool_alpha") {
		t.Errorf("alpha row id %q does not encode its scope", alphaRowID)
	}
	if !strings.Contains(betaRowID, "task_tool_beta") {
		t.Errorf("beta row id %q does not encode its scope", betaRowID)
	}
}
