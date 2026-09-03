package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

// seedChildItem persists a subagent child row with an explicit summary
// and status so the aggregate-preview tests can stage the exact shapes
// pickLatestChildSummary distinguishes (active vs terminal, empty vs
// non-empty summary).
func seedChildItem(
	t *testing.T,
	s *Store,
	threadID, id string,
	turnIndex, itemIndex int,
	parentID, summary, status string,
) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    status,
		Summary:   summary,
		ParentID:  parentID,
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}); err != nil {
		t.Fatalf("seed child item %s: %v", id, err)
	}
}

func seedToolChildItem(
	t *testing.T,
	s *Store,
	threadID, id string,
	turnIndex, itemIndex int,
	parentID, summary, status string,
) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID: id, ThreadID: threadID, TurnIndex: turnIndex, ItemIndex: itemIndex,
		Kind: "tool_call", Role: "assistant", ToolName: "Bash", Status: status,
		Summary: summary, ParentID: parentID, CreatedAt: int64(turnIndex*10 + itemIndex),
	}); err != nil {
		t.Fatalf("seed tool child item %s: %v", id, err)
	}
}

// decodedSubagentMeta extracts the decoration keys from an anchor's
// merged meta JSON. ok flags report key presence so tests can assert
// "summary omitted" distinctly from "summary empty".
func decodedSubagentMeta(t *testing.T, item Item) (count float64, summary string, hasCount, hasSummary bool) {
	t.Helper()
	meta := map[string]any{}
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("unmarshal meta %q: %v", item.Meta, err)
	}
	countVal, hasCount := meta[metaKeySubagentDescendantCount]
	if hasCount {
		count, _ = countVal.(float64)
	}
	summaryVal, hasSummary := meta[metaKeySubagentLatestChildSummary]
	if hasSummary {
		summary, _ = summaryVal.(string)
	}
	return count, summary, hasCount, hasSummary
}

func itemByID(items []Item, id string) (Item, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return Item{}, false
}

func TestListSubagentDescendants_MultiLevelOrderedAndExcludedFromWindows(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// grand anchors a nested launch: grand -> parent -> child. The
	// intermediate "parent" row is itself a launch (tool_call) but
	// still a child of grand. Unrelated top-level noise sits between.
	seedAnchorItem(t, s, "t", "grand", 0, 0)
	if err := s.InsertItem(Item{
		ID: "parent", ThreadID: "t", TurnIndex: 1, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", ToolName: "Task",
		Summary: "parent", ParentID: "grand", CreatedAt: 10,
	}); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	seedItem(t, s, "t", "noise", 2, 0, "")
	seedItem(t, s, "t", "noise2", 3, 0, "")
	seedChildItem(t, s, "t", "child", 5, 0, "parent", "child", "completed")

	// The whole subtree hydrates from the root in one call, ordered by
	// timeline coordinate.
	descendants, err := s.ListSubagentDescendants("t", "grand")
	if err != nil {
		t.Fatalf("list descendants: %v", err)
	}
	if !equalStringSlice(collectIDs(descendants), []string{"parent", "child"}) {
		t.Errorf("descendants: got %v, want [parent child]", collectIDs(descendants))
	}

	// Windows carry only top-level rows; the anchor's collapsed card
	// aggregates stand in for the unloaded subtree.
	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	if !equalStringSlice(collectIDs(paged.Items), []string{"grand", "noise", "noise2"}) {
		t.Errorf("window: got %v, want [grand noise noise2]", collectIDs(paged.Items))
	}
	grand, ok := itemByID(paged.Items, "grand")
	if !ok {
		t.Fatal("grand missing from window")
	}
	count, summary, hasCount, hasSummary := decodedSubagentMeta(t, grand)
	if !hasCount || count != 2 {
		t.Errorf("descendant count: got %v (present=%v), want 2", count, hasCount)
	}
	// Prose is not an activity preview. The nested Task launch is the latest
	// observable action even though later assistant prose exists.
	if !hasSummary || summary != "parent" {
		t.Errorf("latest summary: got %q (present=%v), want \"parent\"", summary, hasSummary)
	}
}

func TestListSubagentDescendants_CrossThreadIsolation(t *testing.T) {
	// The items PK is `(thread_id, id)`, so the same item id can exist
	// on two threads. The recursive walk re-filters by thread_id at
	// every step; an id collision on another thread must neither leak
	// rows in nor pull foreign children.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("ta", "claude")); err != nil {
		t.Fatalf("create ta: %v", err)
	}
	if err := s.CreateThread(makeThread("tb", "claude")); err != nil {
		t.Fatalf("create tb: %v", err)
	}
	seedAnchorItem(t, s, "ta", "parent", 0, 0)
	seedItem(t, s, "ta", "child", 5, 0, "parent")
	// Thread B reuses the id "parent" as a plain row AND has its own
	// child pointing at that id.
	seedItem(t, s, "tb", "parent", 10, 0, "")
	seedItem(t, s, "tb", "b-child", 10, 1, "parent")

	forA, err := s.ListSubagentDescendants("ta", "parent")
	if err != nil {
		t.Fatalf("list ta: %v", err)
	}
	if !equalStringSlice(collectIDs(forA), []string{"child"}) {
		t.Errorf("ta descendants: got %v, want [child]", collectIDs(forA))
	}
	for _, item := range forA {
		if item.ThreadID != "ta" {
			t.Errorf("cross-thread leak: item %s came from %s, want ta", item.ID, item.ThreadID)
		}
	}

	forB, err := s.ListSubagentDescendants("tb", "parent")
	if err != nil {
		t.Fatalf("list tb: %v", err)
	}
	if !equalStringSlice(collectIDs(forB), []string{"b-child"}) {
		t.Errorf("tb descendants: got %v, want [b-child]", collectIDs(forB))
	}
	for _, item := range forB {
		if item.ThreadID != "tb" {
			t.Errorf("cross-thread leak: item %s came from %s, want tb", item.ID, item.ThreadID)
		}
	}
}

func TestListSubagentDescendants_TerminatesOnParentCycle(t *testing.T) {
	// A malformed thread where parent_id forms a cycle (a -> b -> a).
	// The recursive CTE uses UNION (not UNION ALL) so (root, id) pairs
	// dedup during recursion and the walk converges instead of
	// spinning. The cycle makes the root reachable from itself, so it
	// appears in its own descendant list — acceptable for corrupt data;
	// the guard is termination, not prettiness. If a future refactor
	// changes UNION → UNION ALL this test hangs, surfacing the
	// regression immediately.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedItem(t, s, "t", "a", 0, 0, "b")
	seedItem(t, s, "t", "b", 1, 0, "a")
	seedItem(t, s, "t", "child", 5, 0, "a")

	descendants, err := s.ListSubagentDescendants("t", "a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := collectIDs(descendants)
	want := []string{"a", "b", "child"}
	if !equalStringSlice(got, want) {
		t.Errorf("descendants: got %v, want %v", got, want)
	}
}

func TestListSubagentDescendants_EmptyRootID(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	descendants, err := s.ListSubagentDescendants("t", "   ")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(descendants) != 0 {
		t.Errorf("descendants: got %v, want empty", collectIDs(descendants))
	}
}

func TestListSubagentDescendants_FiltersPlanUpdateChildren(t *testing.T) {
	// plan_update notifications never render in any timeline surface;
	// they must not hydrate on expansion nor count against the
	// collapsed card's "N entries" badge.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedAnchorItem(t, s, "t", "anchor", 0, 0)
	if err := s.InsertItem(Item{
		ID: "c-plan", ThreadID: "t", TurnIndex: 0, ItemIndex: 1,
		Kind: "notification", Role: "system", ToolName: "plan_update",
		Summary: "plan", ParentID: "anchor", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed plan child: %v", err)
	}
	seedChildItem(t, s, "t", "c-real", 0, 2, "anchor", "real work", "completed")

	descendants, err := s.ListSubagentDescendants("t", "anchor")
	if err != nil {
		t.Fatalf("list descendants: %v", err)
	}
	if !equalStringSlice(collectIDs(descendants), []string{"c-real"}) {
		t.Errorf("descendants: got %v, want [c-real]", collectIDs(descendants))
	}

	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	anchor, ok := itemByID(paged.Items, "anchor")
	if !ok {
		t.Fatal("anchor missing from window")
	}
	count, _, hasCount, _ := decodedSubagentMeta(t, anchor)
	if !hasCount || count != 1 {
		t.Errorf("descendant count: got %v (present=%v), want 1 (plan_update excluded)", count, hasCount)
	}
}

func TestListSubagentDescendants_HydratesPayloadMeta(t *testing.T) {
	// Child rows carry the same payload-meta projection as window rows
	// so the expanded transcript renders identically (diff stats,
	// command exit codes) without loading payload data blobs.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedAnchorItem(t, s, "t", "anchor", 0, 0)
	if err := s.InsertItemWithPayload(Item{
		ID: "c-cmd", ThreadID: "t", TurnIndex: 0, ItemIndex: 1,
		Kind: "tool_call", Role: "assistant", ToolName: "Bash",
		Summary: "go test ./...", ParentID: "anchor",
		PayloadID: "p-cmd", CreatedAt: 1,
	}, Payload{
		ID: "p-cmd", Kind: "command_output",
		Meta: `{"exitCode":0}`, Data: []byte("ok\n"), CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed payload child: %v", err)
	}

	descendants, err := s.ListSubagentDescendants("t", "anchor")
	if err != nil {
		t.Fatalf("list descendants: %v", err)
	}
	child, ok := itemByID(descendants, "c-cmd")
	if !ok {
		t.Fatalf("c-cmd missing: got %v", collectIDs(descendants))
	}
	if child.PayloadKind != "command_output" {
		t.Errorf("payload kind: got %q, want command_output", child.PayloadKind)
	}
	if child.PayloadMeta != `{"exitCode":0}` {
		t.Errorf("payload meta: got %q, want exitCode JSON", child.PayloadMeta)
	}
}

func TestDecorateSubagentAnchors_PreviewPrefersActiveThenLatest(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Anchor A: a running child with a summary beats a later-coordinate
	// terminal child — mirrors the frontend's pickLatestChildSummary.
	seedAnchorItem(t, s, "t", "anchor-a", 0, 0)
	seedToolChildItem(t, s, "t", "a-run", 0, 1, "anchor-a", "working on auth", "running")
	seedToolChildItem(t, s, "t", "a-done", 0, 2, "anchor-a", "finished tests", "completed")

	// Anchor B: an active child with an EMPTY summary loses to a
	// terminal child that actually has text.
	seedAnchorItem(t, s, "t", "anchor-b", 1, 0)
	seedToolChildItem(t, s, "t", "b-done", 1, 1, "anchor-b", "did the thing", "completed")
	seedToolChildItem(t, s, "t", "b-run-empty", 1, 2, "anchor-b", "", "running")

	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}

	anchorA, ok := itemByID(paged.Items, "anchor-a")
	if !ok {
		t.Fatal("anchor-a missing from window")
	}
	countA, summaryA, _, _ := decodedSubagentMeta(t, anchorA)
	if countA != 2 {
		t.Errorf("anchor-a count: got %v, want 2", countA)
	}
	if summaryA != "working on auth" {
		t.Errorf("anchor-a summary: got %q, want running child's summary", summaryA)
	}

	anchorB, ok := itemByID(paged.Items, "anchor-b")
	if !ok {
		t.Fatal("anchor-b missing from window")
	}
	countB, summaryB, _, _ := decodedSubagentMeta(t, anchorB)
	if countB != 2 {
		t.Errorf("anchor-b count: got %v, want 2", countB)
	}
	if summaryB != "did the thing" {
		t.Errorf("anchor-b summary: got %q, want non-empty terminal summary", summaryB)
	}
}

func TestDecorateSubagentAnchors_EmptySummariesOmitSummaryKey(t *testing.T) {
	// A just-launched group whose children haven't produced text yet:
	// the count decorates (the card shows "N entries") but no summary
	// key is written, so the frontend falls back to its own
	// "Initializing..." placeholder.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedAnchorItem(t, s, "t", "anchor", 0, 0)
	seedChildItem(t, s, "t", "c-blank", 0, 1, "anchor", "", "running")

	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	anchor, ok := itemByID(paged.Items, "anchor")
	if !ok {
		t.Fatal("anchor missing from window")
	}
	count, _, hasCount, hasSummary := decodedSubagentMeta(t, anchor)
	if !hasCount || count != 1 {
		t.Errorf("count: got %v (present=%v), want 1", count, hasCount)
	}
	if hasSummary {
		t.Error("summary key must be omitted when no child has summary text")
	}
}

func TestListSubagentDescendants_CapsAtMaxNewestWin(t *testing.T) {
	// The expansion load is a wire RPC; maxSubagentDescendants bounds one
	// call the way maxWindowItems bounds the window pagers. When the cap
	// binds, the newest rows win (tail-biased, like listTailSlice) and
	// the result stays in ascending timeline order.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedAnchorItem(t, s, "t", "anchor", 0, 0)
	total := maxSubagentDescendants + 5
	for i := 1; i <= total; i++ {
		seedChildItem(t, s, "t", fmt.Sprintf("c-%d", i), i, 0, "anchor", "", "completed")
	}

	descendants, err := s.ListSubagentDescendants("t", "anchor")
	if err != nil {
		t.Fatalf("list descendants: %v", err)
	}
	if len(descendants) != maxSubagentDescendants {
		t.Fatalf("len: got %d, want %d", len(descendants), maxSubagentDescendants)
	}
	wantFirst := fmt.Sprintf("c-%d", total-maxSubagentDescendants+1)
	if descendants[0].ID != wantFirst {
		t.Errorf("first: got %s, want %s (oldest rows beyond the cap drop)", descendants[0].ID, wantFirst)
	}
	if descendants[len(descendants)-1].ID != fmt.Sprintf("c-%d", total) {
		t.Errorf("last: got %s, want c-%d", descendants[len(descendants)-1].ID, total)
	}
	for i := 1; i < len(descendants); i++ {
		if descendants[i-1].TurnIndex > descendants[i].TurnIndex {
			t.Fatalf("not ascending at %d: %d > %d", i, descendants[i-1].TurnIndex, descendants[i].TurnIndex)
		}
	}

	// The collapsed-card badge still reports the full total — the cap
	// bounds one hydrate call, not the aggregate.
	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	anchor, ok := itemByID(paged.Items, "anchor")
	if !ok {
		t.Fatal("anchor missing from window")
	}
	count, _, hasCount, _ := decodedSubagentMeta(t, anchor)
	if !hasCount || int(count) != total {
		t.Errorf("decorated count: got %v (present=%v), want %d", count, hasCount, total)
	}
}

func TestMergeSubagentAnchorMeta_MalformedMetaLeftUntouched(t *testing.T) {
	// SQLite rejects malformed items.meta on write (the partial-index
	// predicates json_extract it), so this guards external corruption
	// only — but that failure mode must not be "the decorator quietly
	// emptied the row's meta". Direct unit test because the corrupt
	// bytes cannot be seeded through the insert path.
	const broken = `{"broken`
	got := mergeSubagentAnchorMeta(broken, subagentAnchorAggregate{
		descendantCount:    3,
		latestChildSummary: "did work",
	})
	if got != broken {
		t.Errorf("malformed meta must pass through unchanged, got %q", got)
	}
}

func TestDecorateSubagentAnchors_StaleStoredSummaryKeyDropped(t *testing.T) {
	// The decoration owns its keys. A same-named summary key already in
	// stored meta must not leak through when the computed summary is
	// empty — otherwise the collapsed card would render text the backend
	// never picked. Unrelated stored keys survive the merge.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "anchor", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", ToolName: "Task",
		Summary: "launch", CreatedAt: 0,
		Meta: `{"subagentLatestChildSummary":"stale text","input":{"description":"run tests"}}`,
	}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	seedChildItem(t, s, "t", "c-blank", 0, 1, "anchor", "", "running")

	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	anchor, ok := itemByID(paged.Items, "anchor")
	if !ok {
		t.Fatal("anchor missing from window")
	}
	count, _, hasCount, hasSummary := decodedSubagentMeta(t, anchor)
	if !hasCount || count != 1 {
		t.Errorf("count: got %v (present=%v), want 1", count, hasCount)
	}
	if hasSummary {
		t.Error("stale stored summary key must be dropped when computed summary is empty")
	}
	meta := map[string]any{}
	if err := json.Unmarshal([]byte(anchor.Meta), &meta); err != nil {
		t.Fatalf("unmarshal merged meta: %v", err)
	}
	if _, ok := meta["input"]; !ok {
		t.Error("unrelated stored meta keys must survive the merge")
	}
}

func TestDecorateSubagentAnchors_LeavesChildlessRowsUntouched(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// A tool_call with no children keeps its meta byte-identical — no
	// decoration keys, no JSON re-marshal churn.
	if err := s.InsertItem(Item{
		ID: "plain-tool", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", ToolName: "Read",
		Summary: "read a file", Meta: `{"filePath":"/tmp/x"}`, CreatedAt: 0,
	}); err != nil {
		t.Fatalf("seed plain tool: %v", err)
	}
	// A non-tool_call row is never a decoration candidate, even with
	// rows pointing at it (synthetic corrupt shape).
	seedItem(t, s, "t", "text-row", 1, 0, "")
	seedItem(t, s, "t", "text-child", 1, 1, "text-row")

	paged, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list slice: %v", err)
	}
	plain, ok := itemByID(paged.Items, "plain-tool")
	if !ok {
		t.Fatal("plain-tool missing from window")
	}
	if plain.Meta != `{"filePath":"/tmp/x"}` {
		t.Errorf("plain-tool meta changed: %s", plain.Meta)
	}
	textRow, ok := itemByID(paged.Items, "text-row")
	if !ok {
		t.Fatal("text-row missing from window")
	}
	_, _, hasCount, _ := decodedSubagentMeta(t, textRow)
	if hasCount {
		t.Error("non-tool_call row must not receive subagent decoration")
	}
}

// TestIsSubagentLaunch_StructuralNotToolName pins the provider-neutral
// launch predicate: a tool_call that other rows are attributed to is a
// launch, whatever it is called; a tool_call with nothing under it is
// not, however agent-shaped its name is. That is what keeps triage's
// final-progress persistence off ordinary tool calls without either
// package maintaining a list of launch tool names.
func TestIsSubagentLaunch_StructuralNotToolName(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// An anonymous MCP tool that spawned attributed rows IS a launch.
	seedAnchorItem(t, s, "t", "mcp__thing", 0, 0)
	seedChildItem(t, s, "t", "child", 0, 1, "mcp__thing", "child work", "completed")
	// A row NAMED Agent with nothing attributed is NOT.
	if err := s.InsertItem(Item{
		ID: "toolu_bare_agent", ThreadID: "t", TurnIndex: 0, ItemIndex: 2,
		Kind: "tool_call", Role: "assistant", Status: "running",
		Summary: "Agent: review", ToolName: "Agent", CreatedAt: 2,
	}); err != nil {
		t.Fatalf("seed bare agent: %v", err)
	}
	// A non-tool_call row that somehow anchors children is NOT.
	seedChildItem(t, s, "t", "text-parent", 0, 3, "", "text", "completed")
	seedChildItem(t, s, "t", "text-child", 0, 4, "text-parent", "nested", "completed")

	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"mcp__thing", true},
		{"toolu_bare_agent", false},
		{"text-parent", false},
		{"child", false},
		{"missing", false},
		{"", false},
	} {
		got, err := s.IsSubagentLaunch("t", tc.id)
		if err != nil {
			t.Fatalf("is subagent launch %q: %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("IsSubagentLaunch(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestIsSubagentLaunch_CrossThreadIsolation pins that a launch in one
// thread cannot be answered for by another thread's children — the
// EXISTS probe joins a second copy of `items`, and an unqualified
// column there would make the predicate vacuously true.
func TestIsSubagentLaunch_CrossThreadIsolation(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"t1", "t2"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}
	seedAnchorItem(t, s, "t1", "toolu_launch", 0, 0)
	seedAnchorItem(t, s, "t2", "toolu_launch", 0, 0)
	seedChildItem(t, s, "t2", "child", 0, 1, "toolu_launch", "child work", "completed")

	if got, err := s.IsSubagentLaunch("t1", "toolu_launch"); err != nil || got {
		t.Fatalf("t1 launch = %v (err %v), want false", got, err)
	}
	if got, err := s.IsSubagentLaunch("t2", "toolu_launch"); err != nil || !got {
		t.Fatalf("t2 launch = %v (err %v), want true", got, err)
	}
}
