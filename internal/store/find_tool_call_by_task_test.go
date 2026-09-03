package store

import (
	"testing"
	"time"
)

// TestFindToolCallItemByTaskIDResolvesIndexedMeta pins the v17 indexed
// lookup. Seeds a thread with several items — one carrying a matching
// task_id in meta, one carrying a different task_id, and a plain
// text item — then asserts the lookup returns exactly the right row
// without scanning the others.
func TestFindToolCallItemByTaskIDResolvesIndexedMeta(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID:            "t-ft",
		ProjectID:     defaultTestProjectID,
		Title:         "T",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Plain text item with no task_id — must not match.
	if err := s.InsertItem(Item{
		ID: "text-1", ThreadID: "t-ft", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Summary: "hello", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert text: %v", err)
	}

	// Tool call with a different task_id — must not match.
	if err := s.InsertItem(Item{
		ID: "tool-a", ThreadID: "t-ft", TurnIndex: 0, ItemIndex: 1,
		Kind: "tool_call", Role: "assistant", Summary: "other",
		Meta:      `{"task_id":"task-other"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert tool-a: %v", err)
	}

	// Tool call with the matching task_id — must match.
	if err := s.InsertItem(Item{
		ID: "tool-b", ThreadID: "t-ft", TurnIndex: 0, ItemIndex: 2,
		Kind: "tool_call", Role: "assistant", Summary: "match",
		Meta:      `{"task_id":"task-target","other":"value"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert tool-b: %v", err)
	}

	item, ok, err := s.FindToolCallItemByTaskID("t-ft", "task-target")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ok {
		t.Fatal("expected match, got none")
	}
	if item.ID != "tool-b" {
		t.Errorf("match id = %q, want tool-b", item.ID)
	}

	// Unknown task id must return (_, false, nil).
	_, ok, err = s.FindToolCallItemByTaskID("t-ft", "task-missing")
	if err != nil {
		t.Fatalf("find missing: %v", err)
	}
	if ok {
		t.Error("expected no match for unknown task id")
	}

	// Empty task id short-circuits without a DB call.
	_, ok, err = s.FindToolCallItemByTaskID("t-ft", "")
	if err != nil {
		t.Fatalf("find empty: %v", err)
	}
	if ok {
		t.Error("expected no match for empty task id")
	}

	// Different thread must not match even with the right task id —
	// the index is thread-scoped.
	if err := s.CreateThread(Thread{
		ID: "t-other", ProjectID: defaultTestProjectID, Title: "O",
		Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread other: %v", err)
	}
	_, ok, err = s.FindToolCallItemByTaskID("t-other", "task-target")
	if err != nil {
		t.Fatalf("find other thread: %v", err)
	}
	if ok {
		t.Error("expected no match on different thread")
	}
}

// TestFindOriginalAgentLaunchByTaskID pins the resume-carrier
// original-launch resolution: a §E6 resume stamps the SAME task_id onto
// the carrier's own row, so the lookup must return the OLDEST row
// carrying the task_id with the carrier itself excluded — the first
// binding, which is the original Agent launch — even across repeated
// resumes whose carriers all share the id.
func TestFindOriginalAgentLaunchByTaskID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-orig", ProjectID: defaultTestProjectID, Title: "T",
		Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// The original launch: oldest row with the task_id. Its updated_at
	// is NEWEST (round-2 Subn stamps touch the launch row), which is
	// exactly why the pick orders by created_at, not updated_at.
	if err := s.InsertItem(Item{
		ID: "agent-launch", ThreadID: "t-orig", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", ToolName: "Agent",
		Summary: "Agent: original", Meta: `{"task_id":"task-r","subagent_model":"claude-opus-4-7"}`,
		CreatedAt: now, UpdatedAt: now + 500,
	}); err != nil {
		t.Fatalf("insert launch: %v", err)
	}
	// A first-resume carrier, younger, same task_id.
	if err := s.InsertItem(Item{
		ID: "carrier-1", ThreadID: "t-orig", TurnIndex: 1, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", ToolName: "SendMessage",
		Summary: "Agent: original", Meta: `{"task_id":"task-r"}`,
		CreatedAt: now + 100, UpdatedAt: now + 100,
	}); err != nil {
		t.Fatalf("insert carrier-1: %v", err)
	}
	// The second-resume carrier doing the lookup.
	if err := s.InsertItem(Item{
		ID: "carrier-2", ThreadID: "t-orig", TurnIndex: 2, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", ToolName: "SendMessage",
		Summary: "Agent: original", Meta: `{"task_id":"task-r"}`,
		CreatedAt: now + 200, UpdatedAt: now + 200,
	}); err != nil {
		t.Fatalf("insert carrier-2: %v", err)
	}

	item, ok, err := s.FindOriginalAgentLaunchByTaskID("t-orig", "task-r", "carrier-2")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ok || item.ID != "agent-launch" {
		t.Fatalf("resolved %q (ok=%v), want agent-launch", item.ID, ok)
	}

	// Excluding the launch itself falls to the next-oldest row — the
	// exclusion is by id, never by tool name.
	item, ok, err = s.FindOriginalAgentLaunchByTaskID("t-orig", "task-r", "agent-launch")
	if err != nil {
		t.Fatalf("find excluding launch: %v", err)
	}
	if !ok || item.ID != "carrier-1" {
		t.Fatalf("resolved %q (ok=%v), want carrier-1", item.ID, ok)
	}

	// Empty task id short-circuits without a DB call.
	if _, ok, err := s.FindOriginalAgentLaunchByTaskID("t-orig", "", "carrier-2"); err != nil || ok {
		t.Fatalf("empty task id: ok=%v err=%v, want no match", ok, err)
	}

	// Unknown task id returns (_, false, nil).
	if _, ok, err := s.FindOriginalAgentLaunchByTaskID("t-orig", "task-missing", "carrier-2"); err != nil || ok {
		t.Fatalf("unknown task id: ok=%v err=%v, want no match", ok, err)
	}
}

// TestFindProvisionalSubagentPrompt pins the §E6 resume-prompt
// reconciliation lookup: the row minted from the rebind
// `system/task_started` (which has no provider uuid to give) is found by
// (parent, exact summary) so the terminal transcript can bind its uuid
// onto it in place. A row that is already bound, one under another
// parent, and one with different text are all misses — each would bind
// the transcript's copy onto the wrong row.
func TestFindProvisionalSubagentPrompt(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-p", ProjectID: defaultTestProjectID, Title: "T",
		Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seed := func(id, parentID, summary, meta string, itemIndex int) {
		t.Helper()
		if err := s.InsertItem(Item{
			ID: id, ThreadID: "t-p", TurnIndex: 0, ItemIndex: itemIndex,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: summary, ParentID: parentID, Meta: meta,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	seed("root", "", "top level prompt", `{"subagent_prompt_provisional":true}`, 0)
	// Already bound: its uuid arrived, so it is nobody's reconciliation target.
	seed("bound", "agent-1", "keep going",
		`{"subagent_prompt_provisional":true,"provider_item_id":"uuid-1"}`, 1)
	seed("other-parent", "agent-2", "keep going", `{"subagent_prompt_provisional":true}`, 2)
	seed("wanted", "agent-1", "keep going", `{"subagent_prompt_provisional":true}`, 3)

	item, ok, err := s.FindProvisionalSubagentPrompt("t-p", "agent-1", "keep going")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ok || item.ID != "wanted" {
		t.Fatalf("resolved %q (ok=%v), want wanted", item.ID, ok)
	}

	if _, ok, err := s.FindProvisionalSubagentPrompt("t-p", "agent-1", "different text"); err != nil || ok {
		t.Fatalf("different text matched: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.FindProvisionalSubagentPrompt("t-p", "agent-3", "keep going"); err != nil || ok {
		t.Fatalf("unknown parent matched: ok=%v err=%v", ok, err)
	}
	// A top-level row is never a scoped prompt: the empty parent
	// short-circuits before the query, which is also what keeps the
	// partial idx_items_parent predicate honest.
	if _, ok, err := s.FindProvisionalSubagentPrompt("t-p", "", "top level prompt"); err != nil || ok {
		t.Fatalf("empty parent matched: ok=%v err=%v", ok, err)
	}
}
