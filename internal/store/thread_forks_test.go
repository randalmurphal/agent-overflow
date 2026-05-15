package store

import (
	"testing"
)

// TestCloneThreadItemsRespectsThroughTurnIndex pins the fork-at-point
// store-level contract: only items whose turn_index <= *throughTurnIndex
// are copied. nil clones every turn (matches existing fork-at-tail).
func TestCloneThreadItemsRespectsThroughTurnIndex(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-slice-src", "t-slice-dst", "t-slice-dst-full"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}

	// 3 turns, 2 items per turn (user + assistant).
	items := []Item{
		{ID: "u0", ThreadID: "t-slice-src", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "t0", CreatedAt: now, UpdatedAt: now},
		{ID: "a0", ThreadID: "t-slice-src", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "r0", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "u1", ThreadID: "t-slice-src", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "t1", CreatedAt: now, UpdatedAt: now},
		{ID: "a1", ThreadID: "t-slice-src", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "r1", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "u2", ThreadID: "t-slice-src", TurnIndex: 2, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "t2", CreatedAt: now, UpdatedAt: now},
		{ID: "a2", ThreadID: "t-slice-src", TurnIndex: 2, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "r2", Status: "completed", CreatedAt: now, UpdatedAt: now},
	}
	for _, it := range items {
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	// Slice through turn 1 — should get items from turns 0 and 1 only (4 rows).
	throughOne := 1
	if _, err := s.CloneThreadItems("t-slice-src", "t-slice-dst", &throughOne); err != nil {
		t.Fatalf("CloneThreadItems sliced: %v", err)
	}
	dst, err := s.ListItems("t-slice-dst")
	if err != nil {
		t.Fatalf("ListItems sliced dst: %v", err)
	}
	if got, want := len(dst), 4; got != want {
		t.Errorf("sliced dst items = %d, want %d (turns 0+1 only)", got, want)
	}
	for _, it := range dst {
		if it.TurnIndex > throughOne {
			t.Errorf("sliced dst leaked turn_index %d (cap was %d)", it.TurnIndex, throughOne)
		}
	}

	// nil throughTurnIndex clones every turn (full clone fallback).
	if _, err := s.CloneThreadItems("t-slice-src", "t-slice-dst-full", nil); err != nil {
		t.Fatalf("CloneThreadItems full: %v", err)
	}
	dstFull, err := s.ListItems("t-slice-dst-full")
	if err != nil {
		t.Fatalf("ListItems full dst: %v", err)
	}
	if got, want := len(dstFull), len(items); got != want {
		t.Errorf("full dst items = %d, want %d", got, want)
	}
}

// TestCloneThreadItemsExcludesRunningBackgroundRows pins Phase-4's
// fork-exclusion contract at the store level: the clone skips rows
// whose `IsBackground && status='running'` combination implicates the
// source thread's subprocess. Any other row — completed backgrounded,
// non-background running, non-tool_call text — copies normally.
//
// The parent thread is untouched; the filter is applied in the clone
// path alone.
func TestCloneThreadItemsExcludesRunningBackgroundRows(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-fork-src", "t-fork-dst"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}

	// Mixed seed on the source thread.
	items := []Item{
		{ID: "user-0", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now, UpdatedAt: now},
		{ID: "asst-1", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "hello", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "bg-run", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, Summary: "Bash: sleep 60", ToolName: "Bash", CreatedAt: now, UpdatedAt: now},
		{ID: "bg-done", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed", IsBackground: true, Summary: "Bash: echo done", ToolName: "Bash", CreatedAt: now, UpdatedAt: now},
		{ID: "inline-run", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 4, Kind: "tool_call", Role: "assistant", Status: "running", Summary: "Read: /tmp/x", ToolName: "Read", CreatedAt: now, UpdatedAt: now},
	}
	for _, it := range items {
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	if _, err := s.CloneThreadItems("t-fork-src", "t-fork-dst", nil); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	// Destination should carry everything EXCEPT `bg-run`.
	dst, err := s.ListItems("t-fork-dst")
	if err != nil {
		t.Fatalf("ListItems(dst): %v", err)
	}
	if len(dst) != 4 {
		t.Fatalf("dst items = %d, want 4 (bg-run excluded)", len(dst))
	}
	for _, it := range dst {
		if it.Summary == "Bash: sleep 60" {
			t.Errorf("running backgrounded row leaked into clone: %+v", it)
		}
		if it.IsBackground && it.Status == "running" {
			t.Errorf("clone carries a running backgrounded row: id=%s summary=%q", it.ID, it.Summary)
		}
	}

	// Source thread is untouched: the running bg row is still there.
	src, err := s.ListItems("t-fork-src")
	if err != nil {
		t.Fatalf("ListItems(src): %v", err)
	}
	found := false
	for _, it := range src {
		if it.ID == "bg-run" {
			found = true
			if it.Status != "running" {
				t.Errorf("source bg-run status mutated: %q", it.Status)
			}
		}
	}
	if !found {
		t.Error("source bg-run row vanished (clone must not mutate source)")
	}
}

// TestCloneThreadItemsNoBackgroundRowsCopiesEverything guards the
// no-op branch of Phase-4's fork filter: a thread with ZERO background
// rows must clone every row verbatim. Phase 4's filter is a narrow
// WHERE-negation; a regression that broadened the predicate (e.g. to
// "any running row") would silently drop legitimate inline tool_calls
// during the fork. Keeping a dedicated test for the empty-bg case
// means a future fork-filter change gets caught here before it reaches
// callers.
func TestCloneThreadItemsNoBackgroundRowsCopiesEverything(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-fork-nobg-src", "t-fork-nobg-dst"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}

	// Mix spanning every non-background-running shape — user turn,
	// assistant text, inline running tool_call, completed tool_call,
	// and a tool_completion sibling. No IsBackground anywhere.
	items := []Item{
		{ID: "user-0", ThreadID: "t-fork-nobg-src", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now, UpdatedAt: now},
		{ID: "asst-1", ThreadID: "t-fork-nobg-src", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "hello", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "tool-run", ThreadID: "t-fork-nobg-src", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "running", Summary: "Edit: foo.ts", ToolName: "Edit", CreatedAt: now, UpdatedAt: now},
		{ID: "tool-done", ThreadID: "t-fork-nobg-src", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "Read: bar.ts", ToolName: "Read", CreatedAt: now, UpdatedAt: now},
		{ID: "sibling", ThreadID: "t-fork-nobg-src", TurnIndex: 1, ItemIndex: 4, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "tool-done", Summary: "Read: bar.ts -> done", ToolName: "Read", CreatedAt: now, UpdatedAt: now},
	}
	for _, it := range items {
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	if _, err := s.CloneThreadItems("t-fork-nobg-src", "t-fork-nobg-dst", nil); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	dst, err := s.ListItems("t-fork-nobg-dst")
	if err != nil {
		t.Fatalf("ListItems(dst): %v", err)
	}
	if len(dst) != len(items) {
		t.Fatalf("dst items = %d, want %d (filter broadened beyond bg-running?)", len(dst), len(items))
	}
	// CloneThreadItems reassigns ids to avoid FK collisions on the
	// destination thread; match seeded rows by (kind, summary) pair
	// instead. A broadened filter would drop at least one pair.
	want := make(map[string]bool)
	for _, it := range items {
		want[it.Kind+"|"+it.Summary] = true
	}
	for _, it := range dst {
		key := it.Kind + "|" + it.Summary
		if !want[key] {
			t.Errorf("unexpected cloned row %s (summary=%q)", it.Kind, it.Summary)
			continue
		}
		delete(want, key)
	}
	var clonedToolID, clonedSiblingCompletionOf string
	for _, it := range dst {
		if it.ID == "tool-done" || it.ID == "sibling" {
			t.Errorf("clone leaked source item id %q", it.ID)
		}
		if it.Kind == "tool_call" && it.Summary == "Read: bar.ts" {
			clonedToolID = it.ID
		}
		if it.Kind == "tool_completion" {
			clonedSiblingCompletionOf = it.CompletionOf
		}
	}
	if clonedToolID == "" || clonedSiblingCompletionOf != clonedToolID {
		t.Errorf("completion_of not rewritten: sibling=%q cloned tool=%q", clonedSiblingCompletionOf, clonedToolID)
	}
	for key := range want {
		t.Errorf("seeded row %q missing from clone (fork filter may be over-eager)", key)
	}
}

// TestCloneThreadItemsPreservesInputPayloadID pins that v44's
// items.input_payload_id propagates through fork-at-point. Without
// this, expanding the input on a forked Edit row would fail authz
// because the cloned item lost its FK to the tool_call_input payload.
// The payload row itself is shared (not duplicated); the
// trg_items_gc_input_payload trigger's NOT EXISTS guard keeps it from
// being swept while either thread still references it.
func TestCloneThreadItemsPreservesInputPayloadID(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-input-src", "t-input-dst"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}
	if err := s.InsertPayload(Payload{
		ID: "p-edit-input", Kind: "tool_call_input", Meta: "{}",
		Data: []byte(`{"old_string":"a","new_string":"b"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "edit-src", ThreadID: "t-input-src", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "Edit foo.go",
		ToolName: "Edit", InputPayloadID: "p-edit-input",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert source item: %v", err)
	}

	if _, err := s.CloneThreadItems("t-input-src", "t-input-dst", nil); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	dst, err := s.ListItems("t-input-dst")
	if err != nil {
		t.Fatalf("ListItems(dst): %v", err)
	}
	if len(dst) != 1 {
		t.Fatalf("dst items = %d, want 1", len(dst))
	}
	cloned := dst[0]
	if cloned.InputPayloadID != "p-edit-input" {
		t.Errorf("cloned input_payload_id = %q, want p-edit-input", cloned.InputPayloadID)
	}
	if cloned.ID == "edit-src" {
		t.Error("clone should reassign item id, but the source id leaked through")
	}
	// Authz lookup from the destination thread succeeds (covers the
	// frontend lazy-load path).
	got, found, err := s.GetThreadItemByPayloadID("t-input-dst", "p-edit-input")
	if err != nil {
		t.Fatalf("lookup on cloned thread: %v", err)
	}
	if !found || got.ID != cloned.ID {
		t.Errorf("forked thread lookup returned id=%q found=%v, want cloned id=%q", got.ID, found, cloned.ID)
	}
}

// TestBuildForkedThreadCopiesEverythingButSessionState pins the
// fork-row builder contract: every model/runtime field carries over so
// the fork starts with the same provider posture, the lineage column
// is set, IDs/timestamps are fresh, and the session-state fields are
// left empty for the app-side saga to populate.
func TestBuildForkedThreadCopiesEverythingButSessionState(t *testing.T) {
	source := Thread{
		ID:                         "source-id",
		ProjectID:                  "p-1",
		Title:                      "Build feature",
		Provider:                   "claude",
		WorkspacePath:              "/tmp/workspace",
		Model:                      "claude-opus-4-7",
		WorktreePath:               "/tmp/wt",
		Branch:                     "feature/login",
		Mode:                       "plan",
		ReasoningEffort:            "xhigh",
		FastMode:                   true,
		ContextWindow:              1000000,
		AutoCompactStandardPercent: 80,
		AutoCompactExtendedPercent: 70,
		RuntimeMode:                "full-access",
		SessionRef:                 "live-session",
		PendingForkRef:             "pending-fork",
		LastTokenUsage:             `{"usedTokens":98765,"maxTokens":1000000,"contextPercent":9.876}`,
		ForkedFromThreadID:         "previous-fork",
		CreatedAt:                  1,
		UpdatedAt:                  2,
	}

	fork := BuildForkedThread(source)

	if fork.ID == "" || fork.ID == source.ID {
		t.Errorf("fork.ID = %q, want fresh non-empty value", fork.ID)
	}
	if fork.Title != "Build feature (fork)" {
		t.Errorf("fork.Title = %q, want %q", fork.Title, "Build feature (fork)")
	}
	if fork.ForkedFromThreadID != source.ID {
		t.Errorf("ForkedFromThreadID = %q, want %q", fork.ForkedFromThreadID, source.ID)
	}
	if fork.ProjectID != source.ProjectID {
		t.Errorf("ProjectID = %q, want %q", fork.ProjectID, source.ProjectID)
	}
	if fork.Provider != source.Provider || fork.Model != source.Model {
		t.Errorf("provider/model not copied: %+v", fork)
	}
	if fork.WorkspacePath != source.WorkspacePath || fork.WorktreePath != source.WorktreePath || fork.Branch != source.Branch {
		t.Errorf("workspace fields not copied: %+v", fork)
	}
	if fork.Mode != source.Mode || fork.RuntimeMode != source.RuntimeMode {
		t.Errorf("mode fields not copied: %+v", fork)
	}
	if fork.ReasoningEffort != source.ReasoningEffort || fork.FastMode != source.FastMode || fork.ContextWindow != source.ContextWindow {
		t.Errorf("model-config fields not copied: %+v", fork)
	}
	// Session-state fields MUST be empty — the fork hasn't run yet, so
	// it has no resume reference. The app-side saga sets these once
	// the provider-specific resume is resolved.
	if fork.SessionRef != "" {
		t.Errorf("SessionRef = %q, want empty (fork has not run)", fork.SessionRef)
	}
	if fork.PendingForkRef != "" {
		t.Errorf("PendingForkRef = %q, want empty (fork has not run)", fork.PendingForkRef)
	}
	// AutoCompact percent fields are intentionally NOT copied so the
	// fork picks up the live Settings default on first spawn.
	if fork.AutoCompactStandardPercent != 0 || fork.AutoCompactExtendedPercent != 0 {
		t.Errorf("AutoCompact percents leaked: std=%d ext=%d, want 0 0",
			fork.AutoCompactStandardPercent, fork.AutoCompactExtendedPercent)
	}
	// LastTokenUsage IS copied so the meter renders inherited usage from
	// frame 0 instead of falsely showing 0% until the new resumed
	// session emits its first reading.
	if fork.LastTokenUsage != source.LastTokenUsage {
		t.Errorf("LastTokenUsage = %q, want copied source value %q",
			fork.LastTokenUsage, source.LastTokenUsage)
	}
	if fork.CreatedAt == 0 || fork.CreatedAt != fork.UpdatedAt {
		t.Errorf("CreatedAt/UpdatedAt = (%d, %d), want non-zero and equal", fork.CreatedAt, fork.UpdatedAt)
	}
}
