package store

import (
	"fmt"
	"testing"

	"agent-overflow/internal/itemmeta"
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

// seedForkSource creates src+dst threads and inserts rows onto src,
// stamping the thread id so a fixture only spells out the shape.
func seedForkSource(t *testing.T, s *Store, src, dst string, rows []Item) {
	t.Helper()
	now := int64(1)
	for _, id := range []string{src, dst} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}
	for _, it := range rows {
		it.ThreadID = src
		it.CreatedAt, it.UpdatedAt = now, now
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}
}

// clonedBySummary indexes a cloned thread's rows by summary — cloned ids
// are freshly minted, so the summary is the only stable handle.
func clonedBySummary(t *testing.T, s *Store, threadID string) map[string]Item {
	t.Helper()
	rows, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	bySummary := make(map[string]Item, len(rows))
	for _, it := range rows {
		if _, dup := bySummary[it.Summary]; dup {
			t.Fatalf("fixture summaries must be unique, %q repeats", it.Summary)
		}
		bySummary[it.Summary] = it
	}
	return bySummary
}

// assertClonedLinksResolve pins the clone's result invariant: every
// non-empty parent_id / completion_of on a cloned row names another
// cloned row. Anything else is a reference into the source thread —
// invisible to every window read and permanent. `exempt` lists ids the
// SOURCE already carried dangling (pre-existing corruption, copied
// verbatim by contract).
func assertClonedLinksResolve(t *testing.T, s *Store, threadID string, exempt ...string) {
	t.Helper()
	rows, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	known := make(map[string]bool, len(rows)+len(exempt))
	for _, it := range rows {
		known[it.ID] = true
	}
	for _, id := range exempt {
		known[id] = true
	}
	for _, it := range rows {
		for _, link := range []struct{ field, id string }{
			{"parent_id", it.ParentID},
			{"completion_of", it.CompletionOf},
		} {
			if link.id != "" && !known[link.id] {
				t.Errorf("cloned row %q has dangling %s = %q", it.Summary, link.field, link.id)
			}
		}
	}
}

// TestCloneThreadItemsSkipsSubtreeOfRunningBackgroundLaunch pins the
// transitive half of the background-running skip: dropping the launch
// without dropping what hangs off it left every descendant carrying the
// SOURCE thread's parent id verbatim. Covers both link kinds — nested
// children at two depths and the completion row that settles the
// skipped launch.
func TestCloneThreadItemsSkipsSubtreeOfRunningBackgroundLaunch(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "t-bgtree-src", "t-bgtree-dst", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "go audit"},
		{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
		{ID: "bg-child", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "bg-launch", ToolName: "Read", Summary: "Agent > Read: a.go"},
		{ID: "bg-grandchild", TurnIndex: 1, ItemIndex: 3, Kind: "tool_completion", Role: "assistant", Status: "completed", ParentID: "bg-child", CompletionOf: "bg-child", ToolName: "Read", Summary: "Agent > Read: a.go -> done"},
		{ID: "bg-launch-done", TurnIndex: 1, ItemIndex: 4, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "bg-launch", ToolName: "Agent", Summary: "Agent: audit -> done"},
		{ID: "asst-tail", TurnIndex: 1, ItemIndex: 5, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "meanwhile"},
	})

	if _, err := s.CloneThreadItems("t-bgtree-src", "t-bgtree-dst", nil); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	dst := clonedBySummary(t, s, "t-bgtree-dst")
	for _, gone := range []string{"Agent: audit", "Agent > Read: a.go", "Agent > Read: a.go -> done", "Agent: audit -> done"} {
		if _, ok := dst[gone]; ok {
			t.Errorf("row %q cloned, want the whole skipped launch subtree absent", gone)
		}
	}
	if len(dst) != 2 {
		t.Errorf("cloned rows = %d, want 2 (user-0 + asst-tail)", len(dst))
	}
	assertClonedLinksResolve(t, s, "t-bgtree-dst")
}

// TestCloneThreadItemsSkipsNestedRunningLaunchUnderClonedParent pins the
// other direction: a background-running launch nested UNDER a normal
// launch takes only its own subtree with it. The surviving parent and
// siblings still clone, with parent_id / completion_of remapped onto the
// freshly minted ids (the regression guard for the remap itself).
func TestCloneThreadItemsSkipsNestedRunningLaunchUnderClonedParent(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "t-nested-src", "t-nested-dst", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "review"},
		{ID: "ok-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Agent", Summary: "Agent: review"},
		{ID: "ok-child", TurnIndex: 1, ItemIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "ok-launch", Summary: "Agent > thinking"},
		{ID: "nested-bg", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ParentID: "ok-launch", ToolName: "Agent", Summary: "Agent > Agent: deep"},
		{ID: "nested-bg-child", TurnIndex: 1, ItemIndex: 4, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "nested-bg", Summary: "Agent > Agent > thinking"},
		{ID: "ok-tool", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "ok-launch", ToolName: "Read", Summary: "Agent > Read: b.go"},
		{ID: "ok-tool-done", TurnIndex: 1, ItemIndex: 6, Kind: "tool_completion", Role: "assistant", Status: "completed", ParentID: "ok-launch", CompletionOf: "ok-tool", ToolName: "Read", Summary: "Agent > Read: b.go -> done"},
		{ID: "ok-launch-done", TurnIndex: 1, ItemIndex: 7, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "ok-launch", ToolName: "Agent", Summary: "Agent: review -> done"},
	})

	if _, err := s.CloneThreadItems("t-nested-src", "t-nested-dst", nil); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	dst := clonedBySummary(t, s, "t-nested-dst")
	for _, gone := range []string{"Agent > Agent: deep", "Agent > Agent > thinking"} {
		if _, ok := dst[gone]; ok {
			t.Errorf("row %q cloned, want the nested running launch's subtree absent", gone)
		}
	}
	if len(dst) != 6 {
		t.Errorf("cloned rows = %d, want 6 (everything but the nested subtree)", len(dst))
	}
	launch, ok := dst["Agent: review"]
	if !ok {
		t.Fatal("the normal launch must still clone")
	}
	if launch.ID == "ok-launch" {
		t.Error("clone leaked the source item id")
	}
	for _, child := range []string{"Agent > thinking", "Agent > Read: b.go", "Agent > Read: b.go -> done"} {
		if got := dst[child].ParentID; got != launch.ID {
			t.Errorf("cloned %q parent_id = %q, want remapped launch id %q", child, got, launch.ID)
		}
	}
	if got, want := dst["Agent > Read: b.go -> done"].CompletionOf, dst["Agent > Read: b.go"].ID; got != want {
		t.Errorf("cloned completion_of = %q, want remapped tool id %q", got, want)
	}
	if got := dst["Agent: review -> done"].CompletionOf; got != launch.ID {
		t.Errorf("cloned launch completion_of = %q, want remapped launch id %q", got, launch.ID)
	}
	assertClonedLinksResolve(t, s, "t-nested-dst")
}

// TestCloneThreadItemsPassesThroughUnknownParentReference pins the
// carve-out: a source row referencing an id that is not in the source
// thread at all is pre-existing corruption, not something the clone
// dropped, so it copies verbatim rather than taking the row (and its own
// descendants) out of the fork.
func TestCloneThreadItemsPassesThroughUnknownParentReference(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "t-ghost-src", "t-ghost-dst", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi"},
		{ID: "orphan", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "ghost-parent", Summary: "orphaned child"},
		{ID: "orphan-completion", TurnIndex: 1, ItemIndex: 2, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "ghost-call", ToolName: "Read", Summary: "orphaned completion"},
	})

	if _, err := s.CloneThreadItems("t-ghost-src", "t-ghost-dst", nil); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	dst := clonedBySummary(t, s, "t-ghost-dst")
	if len(dst) != 3 {
		t.Fatalf("cloned rows = %d, want 3 (unknown references must not drop rows)", len(dst))
	}
	if got := dst["orphaned child"].ParentID; got != "ghost-parent" {
		t.Errorf("cloned parent_id = %q, want the source's unknown reference preserved", got)
	}
	if got := dst["orphaned completion"].CompletionOf; got != "ghost-call" {
		t.Errorf("cloned completion_of = %q, want the source's unknown reference preserved", got)
	}
	assertClonedLinksResolve(t, s, "t-ghost-dst", "ghost-parent", "ghost-call")
}

// TestCloneThreadHistoryBeforeItemSkipsDescendantsOfExcludedRows pins
// that the keep predicate's exclusions are transitive too. A promoted
// anchor's cut is the one that can split a subtree: it drops the turn's
// TOP-LEVEL user successors while keeping the content rows below them,
// so a child parented to a dropped queued message is a row keep() says
// yes to and whose parent stayed in the source thread.
func TestCloneThreadHistoryBeforeItemSkipsDescendantsOfExcludedRows(t *testing.T) {
	s := newTestStore(t)
	cloneHistoryFixture(t, s, "t-hist-dsrc", "t-hist-ddst", true)
	// A child of `queued2` — a top-level user row the promoted cut drops
	// while keeping its own (later, assistant) position.
	if err := s.InsertItem(Item{
		ID: "queued2-child", ThreadID: "t-hist-dsrc", TurnIndex: 1, ItemIndex: 5,
		Kind: "assistant_text", Role: "assistant", ParentID: "queued2", CreatedAt: 1_015,
	}); err != nil {
		t.Fatalf("insert queued2-child: %v", err)
	}

	idMap, err := s.CloneThreadHistoryBeforeItem("t-hist-dsrc", "t-hist-ddst", "anchor")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, ok := idMap["queued2"]; ok {
		t.Fatal("fixture broken: the promoted cut must drop the queued top-level user row")
	}
	if _, ok := idMap["queued2-child"]; ok {
		t.Error("child of an excluded row cloned; its parent stayed in the source thread")
	}
	assertClonedLinksResolve(t, s, "t-hist-ddst")
}

// TestCloneThreadItemsRefusesOutOfOrderDroppedReference pins the loud
// failure for the one shape the forward pass cannot repair: a row whose
// reference names a dropped id that appears LATER in source order. The
// live write path cannot produce it (parents precede children per
// invariants 10/11), so surviving to the remap means some writer broke
// that ordering — the clone refuses rather than minting the invisible
// cross-thread reference, before anything is inserted.
func TestCloneThreadItemsRefusesOutOfOrderDroppedReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []Item
	}{
		{name: "parent", rows: []Item{
			{ID: "early-child", TurnIndex: 1, ItemIndex: 0, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "bg-launch", Summary: "child before its launch"},
			{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
		}},
		{name: "completion", rows: []Item{
			{ID: "early-done", TurnIndex: 1, ItemIndex: 0, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "bg-launch", ToolName: "Agent", Summary: "done before its launch"},
			{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			src, dst := "t-ooo-src-"+tc.name, "t-ooo-dst-"+tc.name
			seedForkSource(t, s, src, dst, tc.rows)

			if _, err := s.CloneThreadItems(src, dst, nil); err == nil {
				t.Fatal("clone succeeded, want a refusal for the out-of-order dropped reference")
			}
			rows, err := s.ListItems(dst)
			if err != nil {
				t.Fatalf("ListItems(%s): %v", dst, err)
			}
			if len(rows) != 0 {
				t.Fatalf("target thread has %d rows after refused clone, want none", len(rows))
			}
		})
	}
}

// TestCloneThreadItemsPreservesInputPayloadID pins that v44's
// items.input_payload_id propagates through fork-at-point. Without
// this, expanding the input on a forked Edit row would fail authz
// because the cloned item lost its FK to the tool_call_input payload.
// The payload graph is duplicated into the destination scope. Later source
// writes must not rewrite the fork's history.
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
	if err := seedPayloadRow(s, "t-input-src", Payload{
		ID: "p-edit-input", Kind: "tool_call_input", Meta: "{}",
		Data: []byte(`{"old_string":"a",`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}
	if err := s.AppendPayloadData(
		"t-input-src", "p-edit-input", []byte(`"new_string":"b"}`), "{}", now,
	); err != nil {
		t.Fatalf("append source payload chunk: %v", err)
	}
	if err := s.PutEditFileSnapshot("t-input-src", "p-edit-input", "foo.go", "source at fork", now); err != nil {
		t.Fatalf("put source snapshot: %v", err)
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

	destinationData, err := s.GetPayloadData("t-input-dst", "p-edit-input")
	if err != nil {
		t.Fatalf("read cloned payload: %v", err)
	}
	if string(destinationData) != `{"old_string":"a","new_string":"b"}` {
		t.Fatalf("cloned payload = %q", destinationData)
	}
	destinationSnapshot, found, err := s.GetEditFileSnapshot("t-input-dst", "p-edit-input", "foo.go")
	if err != nil || !found || destinationSnapshot != "source at fork" {
		t.Fatalf("cloned snapshot = %q found=%v err=%v", destinationSnapshot, found, err)
	}

	if err := s.ReplacePayloadData("t-input-src", "p-edit-input", []byte("source changed"), "{}", now+1); err != nil {
		t.Fatalf("replace source payload: %v", err)
	}
	if err := s.PutEditFileSnapshot("t-input-src", "p-edit-input", "foo.go", "source changed", now+1); err != nil {
		t.Fatalf("replace source snapshot: %v", err)
	}
	destinationData, err = s.GetPayloadData("t-input-dst", "p-edit-input")
	if err != nil || string(destinationData) != `{"old_string":"a","new_string":"b"}` {
		t.Fatalf("source mutation leaked into fork payload: data=%q err=%v", destinationData, err)
	}
	destinationSnapshot, found, err = s.GetEditFileSnapshot("t-input-dst", "p-edit-input", "foo.go")
	if err != nil || !found || destinationSnapshot != "source at fork" {
		t.Fatalf("source mutation leaked into fork snapshot: snapshot=%q found=%v err=%v", destinationSnapshot, found, err)
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

// TestCloneThreadTurnsSynthesizesIDsAndPreservesProviderTurnID pins the
// turns-clone contract: cloned rows get `<targetThreadID>:<turn_index>`
// PKs (turn_id is globally unique; a fork needs its own thread-scoped row
// identity even though Codex keeps the source's wire turn id) while
// provider_turn_id carries the wire id across — that's what lets a
// revert inside the fork resolve its lastTurnId anchor after the
// source thread is gone.
func TestCloneThreadTurnsSynthesizesIDsAndPreservesProviderTurnID(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-turns-src", "t-turns-dst", "t-turns-dst-full"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "codex", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}

	for i, wireID := range []string{"wire-turn-0", "wire-turn-1", "wire-turn-2"} {
		if err := s.InsertTurn(Turn{
			TurnID:         wireID,
			ProviderTurnID: wireID,
			ThreadID:       "t-turns-src",
			TurnIndex:      i,
			StartedAt:      int64(i + 1),
		}); err != nil {
			t.Fatalf("InsertTurn %s: %v", wireID, err)
		}
		if err := s.UpdateTurnCompleted(wireID, int64(i+2), "end_turn", "", "", ""); err != nil {
			t.Fatalf("UpdateTurnCompleted %s: %v", wireID, err)
		}
	}

	// Slice through turn 1.
	throughOne := 1
	if err := s.CloneThreadTurns("t-turns-src", "t-turns-dst", &throughOne); err != nil {
		t.Fatalf("CloneThreadTurns sliced: %v", err)
	}
	for i, wantWire := range []string{"wire-turn-0", "wire-turn-1"} {
		turn, found, err := s.GetTurnByThreadIndex("t-turns-dst", i)
		if err != nil {
			t.Fatalf("GetTurnByThreadIndex(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("cloned turn %d missing", i)
		}
		if wantID := fmt.Sprintf("t-turns-dst:%d", i); turn.TurnID != wantID {
			t.Errorf("cloned turn %d id = %q, want synthesized %q", i, turn.TurnID, wantID)
		}
		if turn.ProviderTurnID != wantWire {
			t.Errorf("cloned turn %d provider_turn_id = %q, want %q", i, turn.ProviderTurnID, wantWire)
		}
	}
	if _, found, err := s.GetTurnByThreadIndex("t-turns-dst", 2); err != nil {
		t.Fatalf("GetTurnByThreadIndex(2): %v", err)
	} else if found {
		t.Error("sliced clone leaked turn_index 2 (cap was 1)")
	}

	// nil throughTurnIndex clones every turn.
	if err := s.CloneThreadTurns("t-turns-src", "t-turns-dst-full", nil); err != nil {
		t.Fatalf("CloneThreadTurns full: %v", err)
	}
	last, found, err := s.GetTurnByThreadIndex("t-turns-dst-full", 2)
	if err != nil || !found {
		t.Fatalf("full clone turn 2: found=%v err=%v", found, err)
	}
	if last.ProviderTurnID != "wire-turn-2" {
		t.Errorf("full clone turn 2 provider_turn_id = %q, want wire-turn-2", last.ProviderTurnID)
	}

	// The source is untouched.
	src, found, err := s.GetTurnByThreadIndex("t-turns-src", 0)
	if err != nil || !found {
		t.Fatalf("source turn 0: found=%v err=%v", found, err)
	}
	if src.TurnID != "wire-turn-0" {
		t.Errorf("source turn 0 id = %q, want wire-turn-0", src.TurnID)
	}
}

// cloneHistoryFixture seeds a source thread with three settled turns whose
// middle turn mixes a prompt, a partial reply, two queued flush user rows,
// and the interrupted round's persisted tail — the layout behind the
// item-granular fork tests below.
func cloneHistoryFixture(t *testing.T, s *Store, src, dst string, promotedAnchors bool) {
	t.Helper()
	now := int64(1_000)
	for _, id := range []string{src, dst} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}
	anchorMeta := ""
	if promotedAnchors {
		var err error
		if anchorMeta, err = itemmeta.MarkPromotedAtInterrupt(""); err != nil {
			t.Fatalf("mark promoted: %v", err)
		}
	}
	rows := []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", CreatedAt: now},
		{ID: "a0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 1},
		{ID: "prompt", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", CreatedAt: now + 10},
		{ID: "pre", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 11},
		{ID: "anchor", TurnIndex: 1, ItemIndex: 2, Kind: "user_text", Role: "user", Meta: anchorMeta, CreatedAt: now + 12},
		{ID: "queued2", TurnIndex: 1, ItemIndex: 3, Kind: "user_text", Role: "user", Meta: anchorMeta, CreatedAt: now + 13},
		{ID: "tail", TurnIndex: 1, ItemIndex: 4, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 14},
		{ID: "u2", TurnIndex: 2, ItemIndex: 0, Kind: "user_text", Role: "user", CreatedAt: now + 20},
		{ID: "a2", TurnIndex: 2, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 21},
	}
	for _, it := range rows {
		it.ThreadID = src
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}
	for turn := 0; turn <= 2; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID: fmt.Sprintf("%s:%d", src, turn), ThreadID: src,
			TurnIndex: turn, StartedAt: now + int64(turn*10),
		}); err != nil {
			t.Fatalf("insert turn %d: %v", turn, err)
		}
		if err := s.UpdateTurnCompleted(
			fmt.Sprintf("%s:%d", src, turn), now+int64(turn*10)+100,
			"end_turn", fmt.Sprintf("am-%d", turn), `{"in":5}`, "",
		); err != nil {
			t.Fatalf("settle turn %d: %v", turn, err)
		}
	}
}

// TestCloneThreadHistoryBeforeItemPlainAnchorKeepsPrefix — a plain anchor's
// clone keeps earlier turns plus the anchor turn's strict prefix, clones
// turn rows only where items survive, and trims the anchor turn's cloned
// settle metadata (completed_at back to the anchor, assistant_message_id
// cleared, token usage and stop reason kept). The source is untouched.
func TestCloneThreadHistoryBeforeItemPlainAnchorKeepsPrefix(t *testing.T) {
	s := newTestStore(t)
	cloneHistoryFixture(t, s, "t-hist-src", "t-hist-dst", false)

	idMap, err := s.CloneThreadHistoryBeforeItem("t-hist-src", "t-hist-dst", "anchor")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	dst, err := s.ListItems("t-hist-dst")
	if err != nil {
		t.Fatalf("list dst: %v", err)
	}
	var got []string
	for _, it := range dst {
		got = append(got, fmt.Sprintf("%d:%d", it.TurnIndex, it.ItemIndex))
	}
	want := []string{"0:0", "0:1", "1:0", "1:1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cloned positions = %v, want %v", got, want)
	}
	if len(idMap) != 4 {
		t.Errorf("idMap size = %d, want 4", len(idMap))
	}
	if _, ok := idMap["anchor"]; ok {
		t.Error("anchor itself must not be cloned")
	}

	assertTurnsRemaining(t, s, "t-hist-dst", []int{0, 1})
	turn1, ok, err := s.GetTurnByThreadIndex("t-hist-dst", 1)
	if err != nil || !ok {
		t.Fatalf("dst turn 1: ok=%v err=%v", ok, err)
	}
	if turn1.CompletedAt == nil || *turn1.CompletedAt != 1_011 {
		t.Errorf("dst turn 1 completed_at = %v, want last cloned row's created_at 1011", turn1.CompletedAt)
	}
	if turn1.AssistantMessageID != "" {
		t.Errorf("dst turn 1 assistant_message_id = %q, want cleared", turn1.AssistantMessageID)
	}
	if turn1.TokenUsageJSON != `{"in":5}` || turn1.StopReason != "end_turn" {
		t.Errorf("dst turn 1 usage/stop rewritten: %+v", turn1)
	}
	turn0, ok, err := s.GetTurnByThreadIndex("t-hist-dst", 0)
	if err != nil || !ok {
		t.Fatalf("dst turn 0: ok=%v err=%v", ok, err)
	}
	if turn0.CompletedAt == nil || *turn0.CompletedAt != 1_100 || turn0.AssistantMessageID != "am-0" {
		t.Errorf("dst turn 0 settle metadata should copy verbatim: %+v", turn0)
	}

	srcTurn1, ok, err := s.GetTurnByThreadIndex("t-hist-src", 1)
	if err != nil || !ok {
		t.Fatalf("src turn 1: ok=%v err=%v", ok, err)
	}
	if srcTurn1.CompletedAt == nil || *srcTurn1.CompletedAt != 1_110 || srcTurn1.AssistantMessageID != "am-1" {
		t.Errorf("source turn 1 must stay untouched: %+v", srcTurn1)
	}
	srcItems, err := s.ListItems("t-hist-src")
	if err != nil {
		t.Fatalf("list src: %v", err)
	}
	if len(srcItems) != 9 {
		t.Errorf("source items = %d, want 9 untouched", len(srcItems))
	}
}

// TestCloneThreadHistoryBeforeItemPromotedAnchorKeepsTail — an
// interrupt-promoted anchor's clone keeps the anchor turn's non-user
// successors (the interrupted round's tail, which precedes the promoted
// message in the provider transcript), drops same-turn user successors,
// and leaves the anchor turn's cloned settle metadata alone.
func TestCloneThreadHistoryBeforeItemPromotedAnchorKeepsTail(t *testing.T) {
	s := newTestStore(t)
	cloneHistoryFixture(t, s, "t-hist-psrc", "t-hist-pdst", true)

	if _, err := s.CloneThreadHistoryBeforeItem("t-hist-psrc", "t-hist-pdst", "anchor"); err != nil {
		t.Fatalf("clone: %v", err)
	}

	dst, err := s.ListItems("t-hist-pdst")
	if err != nil {
		t.Fatalf("list dst: %v", err)
	}
	var got []string
	for _, it := range dst {
		got = append(got, fmt.Sprintf("%d:%d:%s", it.TurnIndex, it.ItemIndex, it.Role))
	}
	want := []string{"0:0:user", "0:1:assistant", "1:0:user", "1:1:assistant", "1:4:assistant"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cloned rows = %v, want %v", got, want)
	}

	assertTurnsRemaining(t, s, "t-hist-pdst", []int{0, 1})
	turn1, ok, err := s.GetTurnByThreadIndex("t-hist-pdst", 1)
	if err != nil || !ok {
		t.Fatalf("dst turn 1: ok=%v err=%v", ok, err)
	}
	if turn1.CompletedAt == nil || *turn1.CompletedAt != 1_110 || turn1.AssistantMessageID != "am-1" {
		t.Errorf("promoted clone must copy turn 1 settle metadata verbatim: %+v", turn1)
	}
}

// TestCloneThreadHistoryBeforeItemPromotedBoundaryCutsResponse — a promoted
// anchor whose echo stamped a provider-order boundary keeps the interrupted
// tail (non-user successors at or below the boundary) but excludes the
// response rows past it, and — because same-turn content was excluded —
// trims the cloned turn's settle metadata to the last cloned row.
func TestCloneThreadHistoryBeforeItemPromotedBoundaryCutsResponse(t *testing.T) {
	s := newTestStore(t)
	cloneHistoryFixture(t, s, "t-hist-bsrc", "t-hist-bdst", true)

	// Echo consumed the anchor mid-loop after the tail (idx 4) and a
	// parented subagent prompt (idx 5, user-role but nested content):
	// stamp the boundary there and add the response that streamed below.
	if _, _, err := s.UpdateItemMetaMerge("t-hist-bsrc", "anchor", func(raw string) (string, error) {
		return itemmeta.MarkPromotedEchoBoundary(raw, 5)
	}, 2_000); err != nil {
		t.Fatalf("stamp boundary: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "subprompt", ThreadID: "t-hist-bsrc", TurnIndex: 1, ItemIndex: 5,
		Kind: "user_text", Role: "user", ParentID: "pre", CreatedAt: 1_015,
	}); err != nil {
		t.Fatalf("insert subprompt: %v", err)
	}
	for i, id := range []string{"resp1", "resp2"} {
		if err := s.InsertItem(Item{
			ID: id, ThreadID: "t-hist-bsrc", TurnIndex: 1, ItemIndex: 6 + i,
			Kind: "assistant_text", Role: "assistant", CreatedAt: 1_016 + int64(i),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	if _, err := s.CloneThreadHistoryBeforeItem("t-hist-bsrc", "t-hist-bdst", "anchor"); err != nil {
		t.Fatalf("clone: %v", err)
	}

	dst, err := s.ListItems("t-hist-bdst")
	if err != nil {
		t.Fatalf("list dst: %v", err)
	}
	var got []string
	for _, it := range dst {
		got = append(got, fmt.Sprintf("%d:%d:%s", it.TurnIndex, it.ItemIndex, it.Role))
	}
	want := []string{"0:0:user", "0:1:assistant", "1:0:user", "1:1:assistant", "1:4:assistant", "1:5:user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cloned rows = %v, want %v (parented subprompt kept, response past boundary excluded)", got, want)
	}

	turn1, ok, err := s.GetTurnByThreadIndex("t-hist-bdst", 1)
	if err != nil || !ok {
		t.Fatalf("dst turn 1: ok=%v err=%v", ok, err)
	}
	if turn1.CompletedAt == nil || *turn1.CompletedAt != 1_015 {
		t.Errorf("dst turn 1 completed_at = %v, want last cloned row's created_at 1015", turn1.CompletedAt)
	}
	if turn1.AssistantMessageID != "" {
		t.Errorf("dst turn 1 assistant_message_id = %q, want cleared (response excluded)", turn1.AssistantMessageID)
	}
}

// TestCloneThreadHistoryBeforeItemTurnOpeningAnchor — an anchor that opens
// turn 0 keeps nothing; a missing anchor errors instead of silently cloning
// everything.
func TestCloneThreadHistoryBeforeItemTurnOpeningAnchor(t *testing.T) {
	s := newTestStore(t)
	cloneHistoryFixture(t, s, "t-hist-zsrc", "t-hist-zdst", false)

	idMap, err := s.CloneThreadHistoryBeforeItem("t-hist-zsrc", "t-hist-zdst", "u0")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if len(idMap) != 0 {
		t.Errorf("turn-0-opening anchor cloned %d items, want 0", len(idMap))
	}
	assertTurnsRemaining(t, s, "t-hist-zdst", nil)

	if _, err := s.CloneThreadHistoryBeforeItem("t-hist-zsrc", "t-hist-zdst", "no-such-item"); err == nil {
		t.Fatal("expected error for missing anchor")
	}
}
