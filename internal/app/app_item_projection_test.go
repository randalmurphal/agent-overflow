package app

import (
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/store"
)

// --- the projection preference ---------------------------------------

func TestItemWindow_ProjectionPreferenceRidesEachRequest(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	on, err := app.ListThreadSliceAround(thread.ID, "", 200, TimelinePageOptions{PageShape: PageShape{InlinePreviews: true, RunWindowRows: 30}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews on): %v", err)
	}
	off, err := app.ListThreadSliceAround(thread.ID, "", 200, TimelinePageOptions{PageShape: PageShape{RunWindowRows: 30}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews off): %v", err)
	}

	if countPreviewPatches(on.Items) == 0 {
		t.Fatal("a client that asked for inline previews received none")
	}
	if got := countPreviewPatches(off.Items); got != 0 {
		t.Errorf("%d preview patches rode to a client that renders none of them", got)
	}
	if got := countPreviewElided(off.Items); got == 0 {
		t.Error("previews were dropped without the marker that makes them recoverable")
	}
	if got := countPreviewElided(on.Items); got != 0 {
		t.Errorf("%d rows were marked elided for a client that got its previews", got)
	}
	for _, item := range off.Items {
		if item.PayloadMeta != "" && item.PayloadPreviewSpans != "" {
			t.Errorf("item %s kept preview spans with no patch left to highlight", item.ID)
			break
		}
	}
	// Two clients, two answers, from one backend that never read a
	// setting of its own.
	if len(on.Items) != len(off.Items) {
		t.Errorf("row counts diverged: %d vs %d — the preference must change fields, not history",
			len(on.Items), len(off.Items))
	}
}

func TestItemWindow_EveryPathProjectsTheSameWay(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	slice, err := app.ListThreadSliceAround(thread.ID, "", 200, TimelinePageOptions{PageShape: PageShape{RunWindowRows: 30}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	synced, err := app.SyncThreadWindow(thread.ID, SyncThreadWindowRequest{
		ItemBudget: 200, HaveEpoch: -1, HaveRev: -1, RunWindowRows: 30,
	})
	if err != nil {
		t.Fatalf("SyncThreadWindow: %v", err)
	}
	if synced.Page == nil {
		t.Fatal("SyncThreadWindow answered without a page")
	}
	// A cold open reaches one of these and a gap refresh reaches the
	// other. If they shaped a row differently, one window would end up
	// holding both shapes.
	if len(synced.Page.Items) != len(slice.Items) {
		t.Fatalf("page sizes differ: sync %d, slice %d", len(synced.Page.Items), len(slice.Items))
	}
	for i := range slice.Items {
		if synced.Page.Items[i].PayloadMeta != slice.Items[i].PayloadMeta {
			t.Fatalf("row %s is shaped differently by the two window paths", slice.Items[i].ID)
		}
		if synced.Page.Items[i].Meta != slice.Items[i].Meta {
			t.Fatalf("row %s has a different meta on the two window paths", slice.Items[i].ID)
		}
	}

	// The pagers, the single-row read and the unwindowed list are the
	// same surface and must not leak an unprojected row.
	older, err := app.ListItemsBeforeCursor(thread.ID, slice.NewestCursor, 50, TimelinePageOptions{PageShape: PageShape{RunWindowRows: 30}})
	if err != nil {
		t.Fatalf("ListItemsBeforeCursor: %v", err)
	}
	if got := countPreviewPatches(older.Items); got != 0 {
		t.Errorf("the older pager shipped %d unprojected previews into the same window", got)
	}
	all, err := app.ListItems(thread.ID, false)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if got := countPreviewPatches(all); got != 0 {
		t.Errorf("ListItems shipped %d unprojected previews", got)
	}
}

func countPreviewPatches(items []store.Item) int {
	count := 0
	for _, item := range items {
		count += strings.Count(item.PayloadMeta, `"previewPatch"`)
	}
	return count
}

func countPreviewElided(items []store.Item) int {
	count := 0
	for _, item := range items {
		count += strings.Count(item.PayloadMeta, `"previewElided"`)
	}
	return count
}

// --- the byte backstop ------------------------------------------------

func TestAdmittedRange_GrowsOutwardFromTheAnchor(t *testing.T) {
	// Rows just over a tenth of the budget each: ten fit, the eleventh
	// cannot. From a mid-page anchor the range takes rows from both sides.
	rowBytes := itemWindowMaxBytes/10 + 1
	items := make([]store.Item, 30)
	for i := range items {
		items[i] = store.Item{ID: fmt.Sprintf("item-%02d", i), Summary: strings.Repeat("x", rowBytes)}
	}

	from, to := admittedRange(items, 15, itemWindowMaxBytes)
	if from > 15 || to <= 15 {
		t.Fatalf("range [%d,%d) does not contain the anchor at 15", from, to)
	}
	if from == 0 || to == len(items) {
		t.Fatalf("range [%d,%d) reached an end; the fixture is not over budget", from, to)
	}
	if 15-from != to-1-15 {
		t.Errorf("range [%d,%d) is not balanced around the anchor", from, to)
	}
	if spent := spentBytes(items[from:to]); spent > itemWindowMaxBytes {
		t.Errorf("admitted %d bytes over the %d budget", spent, itemWindowMaxBytes)
	}

	// The two cursor-page shapes: anchored at an end, the range is one-sided.
	if from, to := admittedRange(items, newestIndex(items), itemWindowMaxBytes); to != len(items) || from == 0 {
		t.Errorf("newest anchor: range [%d,%d), want the newest rows only", from, to)
	}
	if from, to := admittedRange(items, 0, itemWindowMaxBytes); from != 0 || to == len(items) {
		t.Errorf("oldest anchor: range [%d,%d), want the oldest rows only", from, to)
	}
}

func TestAdmittedRange_OneSideExhaustedSpendsTheRestOnTheOther(t *testing.T) {
	rowBytes := itemWindowMaxBytes/10 + 1
	items := make([]store.Item, 30)
	for i := range items {
		items[i] = store.Item{ID: fmt.Sprintf("item-%02d", i), Summary: strings.Repeat("x", rowBytes)}
	}
	// Two rows older than the anchor: both fit, and the budget the older
	// side no longer needs goes to the newer side.
	from, to := admittedRange(items, 2, itemWindowMaxBytes)
	if from != 0 {
		t.Errorf("range [%d,%d) did not reach the oldest row", from, to)
	}
	if to-from != 9 {
		t.Errorf("admitted %d rows, want 9 (the budget's worth)", to-from)
	}
}

func TestAdmittedRange_AlwaysAdmitsTheAnchorEvenOversized(t *testing.T) {
	huge := store.Item{ID: "huge", Summary: strings.Repeat("x", itemWindowMaxBytes*3)}
	if from, to := admittedRange([]store.Item{huge}, 0, itemWindowMaxBytes); to-from != 1 {
		t.Fatal("refused the only row on the page; pagination would stall on it forever")
	}
	// And it is still exactly one: the oversized anchor does not license
	// the rest of the page, whichever side it sits on.
	items := []store.Item{huge, huge, huge}
	for anchor := range items {
		if from, to := admittedRange(items, anchor, itemWindowMaxBytes); to-from != 1 || from != anchor {
			t.Errorf("anchor %d: range [%d,%d), want exactly the anchor", anchor, from, to)
		}
	}
}

func TestAdmittedRange_ClampsAnOutOfRangeAnchor(t *testing.T) {
	items := []store.Item{{ID: "a"}, {ID: "b"}}
	if from, to := admittedRange(items, 7, itemWindowMaxBytes); from != 0 || to != 2 {
		t.Errorf("anchor past the end: range [%d,%d), want [0,2)", from, to)
	}
	if from, to := admittedRange(items, -3, itemWindowMaxBytes); from != 0 || to != 2 {
		t.Errorf("anchor before the start: range [%d,%d), want [0,2)", from, to)
	}
	if from, to := admittedRange(nil, 0, itemWindowMaxBytes); from != 0 || to != 0 {
		t.Errorf("empty page: range [%d,%d), want [0,0)", from, to)
	}
}

// The anchor a caller names need not be among the rows the page ships:
// a subagent child anchors a window it can never appear in, and a run
// member outside the run window is counted by a stub instead. The byte
// trim still has to grow from where the READER is, which is the newest
// shipped row at or before the anchor's coordinate.
func TestPageAnchorIndex_ResolvesAnAnchorThePageDidNotShip(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-anchor-coord", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	shipped := make([]store.Item, 0, 6)
	for i := range 6 {
		item := store.Item{
			ID: fmt.Sprintf("row-%02d", i), ThreadID: thread.ID,
			TurnIndex: i, ItemIndex: i * 10,
			Kind: "assistant_text", Role: "assistant", Status: "completed",
		}
		if err := app.store.InsertItem(item); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
		shipped = append(shipped, item)
	}
	child := store.Item{
		ID: "child", ThreadID: thread.ID, TurnIndex: 3, ItemIndex: 35, ParentID: "row-03",
		Kind: "assistant_text", Role: "assistant", Status: "completed",
	}
	if err := app.store.InsertItem(child); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	cases := []struct {
		name   string
		anchor string
		items  []store.Item
		want   int
	}{
		{name: "shipped anchor", anchor: "row-01", items: shipped, want: 1},
		{name: "unshipped child anchor", anchor: "child", items: shipped, want: 3},
		{name: "anchor older than every shipped row", anchor: "child", items: shipped[4:], want: 0},
		{name: "tail read", anchor: "", items: shipped, want: 5},
		{name: "vanished anchor", anchor: "gone", items: shipped, want: 5},
		{name: "empty page", anchor: "row-01", items: nil, want: -1},
	}
	for _, tc := range cases {
		got, err := app.pageAnchorIndex(thread.ID, tc.anchor, tc.items)
		if err != nil {
			t.Fatalf("%s: pageAnchorIndex: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: pageAnchorIndex = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestProjectPage_ReportsWhatItDroppedOnBothSides(t *testing.T) {
	rowBytes := itemWindowMaxBytes/4 + 1
	items := make([]store.Item, 12)
	for i := range items {
		items[i] = store.Item{
			ID: fmt.Sprintf("item-%02d", i), TurnIndex: i, ItemIndex: i,
			Summary: strings.Repeat("x", rowBytes),
		}
	}
	page := store.PagedItems{
		Items:        items,
		OldestCursor: store.TimelineCursor{TurnIndex: 0, ItemIndex: 0, ItemID: "item-00"},
		NewestCursor: store.TimelineCursor{TurnIndex: 11, ItemIndex: 11, ItemID: "item-11"},
	}
	trimmed := projectPage(page, PageShape{}.normalize(), 6)
	if len(trimmed.Items) == len(items) {
		t.Fatal("fixture is not over budget")
	}
	if !containsItem(trimmed.Items, "item-06") {
		t.Fatal("the anchor was trimmed off its own page")
	}
	if !trimmed.HasMoreOlder || !trimmed.HasMore || !trimmed.HasMoreNewer {
		t.Errorf("rows were dropped on both sides without saying so: older=%v newer=%v",
			trimmed.HasMoreOlder, trimmed.HasMoreNewer)
	}
	if trimmed.OldestCursor.ItemID != trimmed.Items[0].ID {
		t.Errorf("OldestCursor = %q, want the surviving oldest row %q",
			trimmed.OldestCursor.ItemID, trimmed.Items[0].ID)
	}
	if trimmed.NewestCursor.ItemID != trimmed.Items[len(trimmed.Items)-1].ID {
		t.Errorf("NewestCursor = %q, want the surviving newest row %q",
			trimmed.NewestCursor.ItemID, trimmed.Items[len(trimmed.Items)-1].ID)
	}
}

// The page a jump to the first message loads: the anchor is the oldest
// row of a turn heavy enough to blow the byte budget on its own. The
// anchor must survive the backstop or the jump has nothing to land on
// (this is the failure the message-nav rail hit on a real thread).
func TestListThreadSliceAround_KeepsTheAnchorOnAnOverBudgetPage(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-slice-anchor", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "t0", ThreadID: thread.ID, TurnIndex: 0}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	const rows = 40
	rowBytes := itemWindowMaxBytes/20 + 1
	for i := range rows {
		// Prose, so every row is a unit of its own: a row this trim
		// drops leaves the page's RANGE and moves its cursor. A run
		// member instead folds into its stub and the range stays put,
		// which is TestListThreadSliceAround_AnchorInsideALongRun.
		kind, role := "assistant_text", "assistant"
		if i == 0 {
			kind, role = "user_text", "user"
		}
		if err := app.store.InsertItem(store.Item{
			ID: fmt.Sprintf("row-%02d", i), ThreadID: thread.ID, TurnIndex: 0, ItemIndex: i,
			Kind: kind, Role: role, Status: "completed",
			Summary: strings.Repeat("x", rowBytes),
		}); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	page, err := app.ListThreadSliceAround(thread.ID, "row-00", rows, TimelinePageOptions{PageShape: PageShape{RunWindowRows: 200}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	if len(page.Items) == rows {
		t.Fatal("fixture is not over budget; the test proves nothing")
	}
	if !containsItem(page.Items, "row-00") {
		t.Fatalf("anchor row-00 missing from the page; oldest shipped row is %q", page.Items[0].ID)
	}
	if page.HasMoreOlder {
		t.Error("HasMoreOlder reported with the thread's first row on the page")
	}
	if !page.HasMoreNewer {
		t.Error("rows were trimmed from the newer side without HasMoreNewer")
	}
	// The tail fallback still anchors at the newest end.
	tail, err := app.ListThreadSliceAround(thread.ID, "", rows, TimelinePageOptions{PageShape: PageShape{RunWindowRows: 200}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround tail: %v", err)
	}
	if !containsItem(tail.Items, fmt.Sprintf("row-%02d", rows-1)) {
		t.Error("tail slice dropped the newest row")
	}
}

func containsItem(items []store.Item, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func spentBytes(items []store.Item) int {
	total := 0
	for _, item := range items {
		total += itemwire.EncodedBytes(item)
	}
	return total
}

// --- the recovery route -----------------------------------------------

func TestGetThreadItemProjectionSource_ReturnsWhatTheProjectionRemoved(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	// A wide run window, because this is a test of the recovery ROUTE:
	// the fixture's elided rows are members of its long run, and the
	// default window would leave them off the page instead of eliding
	// anything on them.
	page, err := app.ListThreadSliceAround(thread.ID, "", 200, TimelinePageOptions{PageShape: PageShape{RunWindowRows: 200}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}

	var elidedPreview, elidedInput store.Item
	for _, item := range page.Items {
		if strings.Contains(item.PayloadMeta, `"previewElided"`) && elidedPreview.ID == "" {
			elidedPreview = item
		}
		if strings.Contains(item.Meta, itemwire.MarkerKey) && elidedInput.ID == "" {
			elidedInput = item
		}
	}
	if elidedPreview.ID == "" || elidedInput.ID == "" {
		t.Fatal("fixture produced no elided rows to recover")
	}

	for _, item := range []store.Item{elidedPreview, elidedInput} {
		source, err := app.GetThreadItemProjectionSource(thread.ID, item.ID)
		if err != nil {
			t.Fatalf("GetThreadItemProjectionSource(%s): %v", item.ID, err)
		}
		if source.ItemID != item.ID {
			t.Fatalf("route answered for %q, asked for %q", source.ItemID, item.ID)
		}
		stored, found, err := app.store.GetThreadItem(thread.ID, item.ID)
		if err != nil || !found {
			t.Fatalf("stored row %s missing: %v", item.ID, err)
		}
		if source.Meta != stored.Meta || source.PayloadMeta != stored.PayloadMeta ||
			source.PayloadPreviewSpans != stored.PayloadPreviewSpans {
			t.Errorf("row %s: the route did not return the complete stored fields", item.ID)
		}
	}

	// The route recovers the exact values the projection removed.
	source, err := app.GetThreadItemProjectionSource(thread.ID, elidedPreview.ID)
	if err != nil {
		t.Fatalf("GetThreadItemProjectionSource: %v", err)
	}
	if !strings.Contains(source.PayloadMeta, `"previewPatch"`) {
		t.Error("recovered payloadMeta carries no patch text")
	}
	if source.PayloadPreviewSpans == "" {
		t.Error("recovered preview spans are empty")
	}
	if strings.Contains(source.PayloadMeta, `"previewElided"`) {
		t.Error("the recovery route returned a projected value; it must be the stored one")
	}

	missing, err := app.GetThreadItemProjectionSource(thread.ID, "no-such-item")
	if err != nil {
		t.Fatalf("GetThreadItemProjectionSource(missing): %v", err)
	}
	if missing.ItemID != "" {
		t.Errorf("missing item answered %#v, want the zero value", missing)
	}
}

func TestScopedPageByteTrimPreservesContextAndEveryHistoryRow(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-scoped-budget", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatal(err)
	}
	root := store.Item{ID: "scope", ThreadID: thread.ID, Kind: "tool_call", ToolName: "Agent", Role: "assistant", Status: "completed", Summary: strings.Repeat("identity", 80)}
	if err := app.store.InsertItem(root); err != nil {
		t.Fatal(err)
	}
	const count = 600
	for i := 0; i < count; i++ {
		item := store.Item{ID: fmt.Sprintf("child-%04d", i), ThreadID: thread.ID, TurnIndex: 0, ItemIndex: i + 1, ParentID: root.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: strings.Repeat("x", 2048)}
		if err := app.store.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	selection := store.TimelineSelection{ScopeRootID: root.ID}
	shape := PageShape{MaxBytes: 12 << 10, RunWindowRows: 30}
	page, err := app.ListThreadSliceAround(thread.ID, "", 200, TimelinePageOptions{PageShape: shape, Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		if page.Scope == nil || page.Scope.Root.ID != root.ID {
			t.Fatal("byte trim lost scope identity")
		}
		cost := scopeContextBytes(page.Scope)
		for _, item := range page.Items {
			seen[item.ID] = true
			cost += itemwire.EncodedBytes(item)
		}
		if cost > shape.MaxBytes {
			t.Fatalf("scope and page cost %d exceeds %d", cost, shape.MaxBytes)
		}
		if !page.HasMoreOlder {
			break
		}
		cursor := page.OldestCursor
		page, err = app.ListItemsBeforeCursor(thread.ID, cursor, 200, TimelinePageOptions{PageShape: shape, Selection: selection})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 || page.OldestCursor.ItemIndex >= cursor.ItemIndex {
			t.Fatal("byte-limited paging did not advance")
		}
	}
	if len(seen) != count {
		t.Fatalf("reached %d/%d history rows", len(seen), count)
	}
}
