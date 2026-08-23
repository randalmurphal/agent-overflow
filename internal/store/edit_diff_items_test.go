package store

import "testing"

// ListTurnUserSummaries labels the edit-diff turn selector, so it must
// name what the READER asked for. A subagent's own prompt is a
// `user_text` row carrying the launch's turn index, and it must never
// become a turn's label — not beside the reader's prompt, and not in a
// turn that has no reader prompt of its own.
func TestListTurnUserSummariesSkipsSubagentPrompts(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	if err := s.CreateThread(Thread{
		ID: "t-1", ProjectID: defaultTestProjectID, Title: "t-1", Provider: "claude",
		WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	rows := []Item{
		{ID: "user:0", ThreadID: "t-1", TurnIndex: 0, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "refactor the parser"},
		{ID: "toolu_agent", ThreadID: "t-1", TurnIndex: 0, ItemIndex: 1,
			Kind: "tool_call", Role: "assistant", ToolName: "Agent", Summary: "Agent"},
		{ID: "user:wire:s1", ThreadID: "t-1", TurnIndex: 0, ItemIndex: 2,
			Kind: "user_text", Role: "user", Summary: "map every surface",
			ParentID: "toolu_agent", Meta: `{"provider_item_id":"s1","wire_only":true}`},
		// Turn 1 has NO reader prompt: a resumed agent's later prompt is
		// its only user row, and the turn must stay unlabelled rather
		// than be labelled with what the agent was told.
		{ID: "toolu_agent2", ThreadID: "t-1", TurnIndex: 1, ItemIndex: 0,
			Kind: "tool_call", Role: "assistant", ToolName: "Agent", Summary: "Agent"},
		{ID: "user:wire:s2", ThreadID: "t-1", TurnIndex: 1, ItemIndex: 1,
			Kind: "user_text", Role: "user", Summary: "keep going",
			ParentID: "toolu_agent2", Meta: `{"provider_item_id":"s2","wire_only":true}`},
	}
	for _, it := range rows {
		it.CreatedAt, it.UpdatedAt = now, now
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}

	got, err := s.ListTurnUserSummaries("t-1")
	if err != nil {
		t.Fatalf("ListTurnUserSummaries: %v", err)
	}
	want := []TurnUserSummary{{TurnIndex: 0, Summary: "refactor the parser"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("summaries = %+v, want %+v", got, want)
	}
}
