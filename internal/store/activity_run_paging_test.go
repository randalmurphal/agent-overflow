package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Page composition over activity runs
// (docs/architecture/timeline-window-pages.md §2, §2.1, §2.2). The rule
// every case here is about: a page is composed of whole units, so its
// range never splits a run, and every physical row in the range is either
// shipped or counted by exactly one stub.

// seedRunThread writes one turn of visible top-level rows from a compact
// spec: "p" is a prose row, "t" a tool call, "r" a still-running tool
// call, "k" a thinking row, "n" a notification, "c" the completion of the
// row before it. Ids are the spec letter plus the row index, so a failure
// names the row that broke.
func seedRunThread(t *testing.T, s *Store, threadID, spec string) []string {
	t.Helper()
	if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	ids := make([]string, 0, len(spec))
	for i, letter := range spec {
		item := Item{
			ID:        string(letter) + strconv.Itoa(i),
			ThreadID:  threadID,
			TurnIndex: 0,
			ItemIndex: i,
			Role:      "assistant",
			Status:    "completed",
			CreatedAt: int64(i + 1),
		}
		switch letter {
		case 'p':
			item.Kind = "assistant_text"
		case 't':
			item.Kind, item.ToolName = "tool_call", "Bash"
		case 'r':
			item.Kind, item.ToolName, item.Status = "tool_call", runningToolName(i), "running"
		case 'k':
			item.Kind = "thinking"
		case 'n':
			item.Kind, item.Role, item.ToolName = "notification", "system", "task_notification"
		case 'c':
			if i == 0 {
				t.Fatalf("seed letter c needs a row before it")
			}
			item.Kind, item.ToolName, item.CompletionOf = "tool_completion", "Bash", ids[i-1]
		default:
			t.Fatalf("unknown seed letter %q", letter)
		}
		item.Summary = item.ID
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert %s: %v", item.ID, err)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

// runningToolName is the tool name a running seed row carries, unique per
// row so a test can tell WHICH running row a stub named.
func runningToolName(i int) string { return "Run" + strconv.Itoa(i) }

// runSpec builds a spec string of one prose row, `members` tool rows and
// one trailing prose row.
func runSpec(members int) string {
	return "p" + strings.Repeat("t", members) + "p"
}

func pageItemIDs(page PagedItems) []string {
	return collectIDs(page.Items)
}

func onlyRun(t *testing.T, page PagedItems) ActivityRunStub {
	t.Helper()
	if len(page.Runs) != 1 {
		t.Fatalf("page carries %d runs, want 1", len(page.Runs))
	}
	return page.Runs[0]
}

// assertPageAccounts is the invariant every case re-checks: the page's
// range holds exactly the rows it shipped plus the rows its stubs count.
func assertPageAccounts(t *testing.T, s *Store, threadID string, page PagedItems, wantRange []string) {
	t.Helper()
	if page.OldestCursor.ItemID != wantRange[0] || page.NewestCursor.ItemID != wantRange[len(wantRange)-1] {
		t.Fatalf("page range = %s..%s, want %s..%s",
			page.OldestCursor.ItemID, page.NewestCursor.ItemID,
			wantRange[0], wantRange[len(wantRange)-1])
	}
	counted := len(page.Items)
	for _, stub := range page.Runs {
		counted += stub.UnshippedBefore + stub.UnshippedAfter
	}
	if counted != len(wantRange) {
		t.Errorf("page accounts for %d rows over a range of %d", counted, len(wantRange))
	}
	shipped := map[string]struct{}{}
	for _, item := range page.Items {
		shipped[item.ID] = struct{}{}
	}
	for _, stub := range page.Runs {
		for _, id := range stub.UnshippedPairedLaunchIDs {
			if _, ok := shipped[id]; ok {
				t.Errorf("stub %s names shipped row %s as an unshipped launch", stub.FirstItemID, id)
			}
		}
		for _, id := range stub.ShippedSupersededLaunchIDs {
			if _, ok := shipped[id]; !ok {
				t.Errorf("stub %s names unshipped row %s as a shipped launch", stub.FirstItemID, id)
			}
		}
	}
}

// TestTrimShippedTracksSplitPairsInFoldOrder: a trim can fold a
// completion before its launch (newer side, newest-first) or after it
// (older side, oldest-first). Either way the stub's two launch lists name
// the half still shipped, and name nothing once neither half is.
func TestTrimShippedTracksSplitPairsInFoldOrder(t *testing.T) {
	s := newTestStore(t)
	// A launch/completion pair at each end of the run: t1,c2 and t29,c30.
	spec := []byte(runSpec(30))
	spec[2], spec[30] = 'c', 'c'
	ids := seedRunThread(t, s, "t", string(spec))

	page, err := s.ListThreadSliceAround(context.Background(), "t", ids[15], 60, 30, TimelineSelection{})
	if err != nil {
		t.Fatalf("anchored slice: %v", err)
	}
	if got := pageItemIDs(page); got[0] != ids[1] || got[len(got)-1] != ids[31] {
		t.Fatalf("page = %v, want the whole run shipped", got)
	}
	// Items[0] is ids[1], so Items[i] is ids[i+1].
	// A newer-side trim drops the prose row after the run out of the range;
	// the run itself never leaves it while a member survives.
	cases := []struct {
		name           string
		from, to       int
		wantRange      []string
		wantPaired     []string
		wantSuperseded []string
	}{
		{"newer side folds the completion first", 0, 29, ids[1:31], nil, []string{ids[29]}},
		{"newer side then folds its launch", 0, 28, ids[1:31], nil, nil},
		{"older side folds the launch first", 1, 31, ids[1:], []string{ids[1]}, nil},
		{"older side then folds its completion", 2, 31, ids[1:], nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trimmed := page.TrimShipped(tc.from, tc.to)
			assertPageAccounts(t, s, "t", trimmed, tc.wantRange)
			folded := onlyRun(t, trimmed)
			if got := folded.UnshippedPairedLaunchIDs; !sameStrings(got, tc.wantPaired) {
				t.Errorf("unshipped paired = %v, want %v", got, tc.wantPaired)
			}
			if got := folded.ShippedSupersededLaunchIDs; !sameStrings(got, tc.wantSuperseded) {
				t.Errorf("shipped superseded = %v, want %v", got, tc.wantSuperseded)
			}
		})
	}
	// Two trims in sequence agree with one trim of the same span.
	staged := onlyRun(t, page.TrimShipped(0, 29).TrimShipped(0, 28))
	if len(staged.UnshippedPairedLaunchIDs) != 0 || len(staged.ShippedSupersededLaunchIDs) != 0 {
		t.Errorf("staged trims left %v / %v", staged.UnshippedPairedLaunchIDs, staged.ShippedSupersededLaunchIDs)
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTailPageShipsTheNewestMembersOfAWholeRun is the headline case: a
// run far larger than the ask is admitted whole, costs one stub, and
// ships only the members that would mount.
func TestTailPageShipsTheNewestMembersOfAWholeRun(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", "p"+strings.Repeat("t", 50))

	page, err := s.ListThreadSliceAround(context.Background(), "t", "", 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("tail slice: %v", err)
	}
	if got := len(page.Items); got != 10 {
		t.Fatalf("shipped %d rows, want the run window of 10: %v", got, pageItemIDs(page))
	}
	stub := onlyRun(t, page)
	if stub.MemberCount != 50 {
		t.Errorf("stub memberCount = %d, want 50", stub.MemberCount)
	}
	if stub.FirstItemID != ids[1] || stub.LastItemID != ids[50] {
		t.Errorf("stub edges = %s..%s, want %s..%s", stub.FirstItemID, stub.LastItemID, ids[1], ids[50])
	}
	if stub.LoadedFirstItemID != ids[41] || stub.LoadedLastItemID != ids[50] {
		t.Errorf("loaded span = %s..%s, want %s..%s",
			stub.LoadedFirstItemID, stub.LoadedLastItemID, ids[41], ids[50])
	}
	if stub.UnshippedBefore != 40 || stub.UnshippedAfter != 0 {
		t.Errorf("unshipped = %d/%d, want 40/0", stub.UnshippedBefore, stub.UnshippedAfter)
	}
	assertPageAccounts(t, s, "t", page, ids[1:])
	if !page.HasMoreOlder {
		t.Error("the prose row before the run is older history and must be reported")
	}
	if page.HasMoreNewer {
		t.Error("the run reaches the thread's newest row")
	}
}

// TestAnchoredPageCentersTheAnchorRun pins §2.1 step 2: the anchor's run
// ships its window centered on the anchor, so a jump lands on a mounted
// row instead of on the run's newest end.
func TestAnchoredPageCentersTheAnchorRun(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", runSpec(50))

	page, err := s.ListThreadSliceAround(context.Background(), "t", ids[26], 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("anchored slice: %v", err)
	}
	stub := onlyRun(t, page)
	if stub.LoadedFirstItemID != ids[21] || stub.LoadedLastItemID != ids[30] {
		t.Fatalf("loaded span = %s..%s, want the 10 rows centered on %s (%s..%s)",
			stub.LoadedFirstItemID, stub.LoadedLastItemID, ids[26], ids[21], ids[30])
	}
	if stub.UnshippedBefore != 20 || stub.UnshippedAfter != 20 {
		t.Errorf("unshipped = %d/%d, want 20/20", stub.UnshippedBefore, stub.UnshippedAfter)
	}
	found := false
	for _, item := range page.Items {
		if item.ID == ids[26] {
			found = true
		}
	}
	if !found {
		t.Errorf("the anchor %s is not in the shipped rows %v", ids[26], pageItemIDs(page))
	}
	// The anchor unit alone spends the at-or-before budget, so the prose
	// row before the run stays outside the range; the newer side has its
	// own budget and admits the prose row after it.
	assertPageAccounts(t, s, "t", page, ids[1:])
	if !page.HasMoreOlder {
		t.Error("the prose row before the run is older history and must be reported")
	}
	if page.HasMoreNewer {
		t.Error("the page reaches the thread's newest row")
	}
}

// TestCursorPagesResolveACursorOnAnUnshippedRow is §2's promise that a
// page edge may name a row the page never shipped: the next page in
// either direction has to start from it anyway.
func TestCursorPagesResolveACursorOnAnUnshippedRow(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", runSpec(50))

	page, err := s.ListThreadSliceAround(context.Background(), "t", ids[26], 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("anchored slice: %v", err)
	}
	// Dropping the trailing prose unit leaves the page ending on the
	// run's last member, which it never shipped — the shape the byte
	// backstop produces and the one a client pages onward from.
	page = page.TrimShipped(0, len(page.Items)-1)
	if page.OldestCursor.ItemID != ids[1] || page.NewestCursor.ItemID != ids[50] {
		t.Fatalf("page range = %s..%s, want the whole run %s..%s",
			page.OldestCursor.ItemID, page.NewestCursor.ItemID, ids[1], ids[50])
	}
	for _, edge := range []string{page.OldestCursor.ItemID, page.NewestCursor.ItemID} {
		for _, item := range page.Items {
			if item.ID == edge {
				t.Fatalf("edge %s was shipped; this case needs an unshipped edge", edge)
			}
		}
	}

	older, err := s.ListItemsBeforeCursor(context.Background(), "t", page.OldestCursor, 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("page before an unshipped cursor: %v", err)
	}
	if got := pageItemIDs(older); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("older page = %v, want [%s]", got, ids[0])
	}
	if len(older.Runs) != 0 {
		t.Errorf("older page carries %d runs, want none", len(older.Runs))
	}

	newer, err := s.ListItemsAfterCursor(context.Background(), "t", page.NewestCursor, 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("page after an unshipped cursor: %v", err)
	}
	if got := pageItemIDs(newer); len(got) != 1 || got[0] != ids[51] {
		t.Errorf("newer page = %v, want [%s]", got, ids[51])
	}
}

// TestCursorPagesAdmitWholeUnits: a cursor pager stops on a unit
// boundary, never inside a run, even when the run alone outruns the ask.
func TestCursorPagesAdmitWholeUnits(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", runSpec(40))
	tail := TimelineCursor{TurnIndex: 0, ItemIndex: 41, ItemID: ids[41]}

	older, err := s.ListItemsBeforeCursor(context.Background(), "t", tail, 5, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("older page: %v", err)
	}
	stub := onlyRun(t, older)
	if stub.MemberCount != 40 {
		t.Errorf("stub memberCount = %d, want the whole run of 40", stub.MemberCount)
	}
	if len(older.Items) != 10 {
		t.Errorf("shipped %d rows, want the run window of 10", len(older.Items))
	}
	assertPageAccounts(t, s, "t", older, ids[1:41])

	head := TimelineCursor{TurnIndex: 0, ItemIndex: 0, ItemID: ids[0]}
	newer, err := s.ListItemsAfterCursor(context.Background(), "t", head, 5, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("newer page: %v", err)
	}
	stub = onlyRun(t, newer)
	if stub.MemberCount != 40 {
		t.Errorf("forward stub memberCount = %d, want 40", stub.MemberCount)
	}
	assertPageAccounts(t, s, "t", newer, ids[1:41])
}

// TestForwardPageKeepsAGrownRunWhole: when a run grew past the caller's
// last page, the rows after its cursor continue that run. The page comes
// back describing the WHOLE run, so the client keeps one run record,
// while the budget still goes to rows the caller does not hold.
func TestForwardPageKeepsAGrownRunWhole(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", "p"+strings.Repeat("t", 20))
	cursor := TimelineCursor{TurnIndex: 0, ItemIndex: 10, ItemID: ids[10]}

	page, err := s.ListItemsAfterCursor(context.Background(), "t", cursor, 50, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("forward page: %v", err)
	}
	stub := onlyRun(t, page)
	if stub.FirstItemID != ids[1] || stub.MemberCount != 20 {
		t.Errorf("stub = %s with %d members, want %s with 20",
			stub.FirstItemID, stub.MemberCount, ids[1])
	}
	if stub.LoadedFirstItemID != ids[11] || stub.LoadedLastItemID != ids[20] {
		t.Errorf("loaded span = %s..%s, want the 10 rows the caller does not hold (%s..%s)",
			stub.LoadedFirstItemID, stub.LoadedLastItemID, ids[11], ids[20])
	}
	assertPageAccounts(t, s, "t", page, ids[1:])
}

// TestTrimShippedFoldsDroppedRowsIntoTheStub is §2.2: the byte backstop
// drops shipped rows, and every one of them stays accounted for.
func TestTrimShippedFoldsDroppedRowsIntoTheStub(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", runSpec(30))

	// The anchor unit spends the at-or-before budget on its own 20
	// centered members, so the page is t5..t24 plus the prose row after
	// the run, over a range of the whole run plus that row.
	page, err := s.ListThreadSliceAround(context.Background(), "t", ids[15], 40, 20, TimelineSelection{})
	if err != nil {
		t.Fatalf("anchored slice: %v", err)
	}
	stub := onlyRun(t, page)
	if got := pageItemIDs(page); len(got) != 21 || got[0] != ids[5] || got[20] != ids[31] {
		t.Fatalf("page = %v, want %s..%s plus %s", got, ids[5], ids[24], ids[31])
	}
	assertPageAccounts(t, s, "t", page, ids[1:])

	// Drop two rows off each end. On the older side both are members and
	// fold; on the newer side the prose unit leaves the range and one
	// member folds.
	trimmed := page.TrimShipped(2, len(page.Items)-2)
	if got := pageItemIDs(trimmed); len(got) != 17 || got[0] != ids[7] || got[16] != ids[23] {
		t.Fatalf("trimmed = %v, want %s..%s", got, ids[7], ids[23])
	}
	folded := onlyRun(t, trimmed)
	if folded.UnshippedBefore != stub.UnshippedBefore+2 || folded.UnshippedAfter != stub.UnshippedAfter+1 {
		t.Errorf("folded unshipped = %d/%d, want %d/%d",
			folded.UnshippedBefore, folded.UnshippedAfter,
			stub.UnshippedBefore+2, stub.UnshippedAfter+1)
	}
	if folded.LoadedFirstItemID != trimmed.Items[0].ID ||
		folded.LoadedLastItemID != trimmed.Items[len(trimmed.Items)-1].ID {
		t.Errorf("folded span = %s..%s, want %s..%s",
			folded.LoadedFirstItemID, folded.LoadedLastItemID,
			trimmed.Items[0].ID, trimmed.Items[len(trimmed.Items)-1].ID)
	}
	// The dropped prose row left the range; the run did not.
	assertPageAccounts(t, s, "t", trimmed, ids[1:31])
	if !trimmed.HasMoreOlder || !trimmed.HasMoreNewer {
		t.Errorf("has-more = %v/%v, want both after dropping the prose unit",
			trimmed.HasMoreOlder, trimmed.HasMoreNewer)
	}

	// The digest still describes exactly the unshipped members, which is
	// what makes the client's fold verifiable.
	rows := make([]WindowDigestRow, 0, 30)
	keptIDs := map[string]struct{}{}
	for _, item := range trimmed.Items {
		keptIDs[item.ID] = struct{}{}
	}
	for _, item := range page.Items {
		if _, ok := keptIDs[item.ID]; ok {
			continue
		}
		if item.Kind != "tool_call" {
			continue
		}
		rows = append(rows, WindowDigestRow{ID: item.ID, Rev: item.Rev})
	}
	wantBits := parseTestDigest(t, stub.UnshippedDigest) ^ parseTestDigest(t, WindowDigest(rows))
	if got := folded.UnshippedDigest; got != formatWindowDigest(wantBits) {
		t.Errorf("folded digest = %s, want %s", got, formatWindowDigest(wantBits))
	}
}

// TestTrimShippedDropsARunThatLosesItsWholeSpan: a run at a far end that
// keeps no shipped row leaves the range with its unit, and the cursor
// moves to the last surviving one.
func TestTrimShippedDropsARunThatLosesItsWholeSpan(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", "ttppp")

	page, err := s.ListThreadSliceAround(context.Background(), "t", "", 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("tail slice: %v", err)
	}
	if len(page.Items) != 5 || len(page.Runs) != 1 {
		t.Fatalf("page = %v with %d runs, want the whole thread and one run",
			pageItemIDs(page), len(page.Runs))
	}
	trimmed := page.TrimShipped(2, 5)
	if len(trimmed.Runs) != 0 {
		t.Errorf("run survived with no shipped rows: %+v", trimmed.Runs)
	}
	if trimmed.OldestCursor.ItemID != ids[2] {
		t.Errorf("oldest cursor = %s, want %s", trimmed.OldestCursor.ItemID, ids[2])
	}
	if !trimmed.HasMoreOlder {
		t.Error("the dropped run is older history and must be reported")
	}
}

func parseTestDigest(t *testing.T, digest string) uint64 {
	t.Helper()
	bits, err := strconv.ParseUint(digest, 16, 64)
	if err != nil {
		t.Fatalf("parse digest %q: %v", digest, err)
	}
	return bits
}

// TestImportedRowsClassifyThroughTheirOwnArm: run membership and the
// file-change count both come from the PAYLOAD, and imported history
// resolves its payload through a different physical arm with a local
// overlay on top. A page over imported rows must classify them exactly as
// it classifies local ones, through the same overlay rule.
func TestImportedRowsClassifyThroughTheirOwnArm(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "imp")

	const base = 1_700_000_000_000
	row := func(id string, index int, kind, tool string, payload *Payload) ImportRow {
		return ImportRow{
			Item: Item{
				ID: id, TurnIndex: 0, ItemIndex: index,
				Kind: kind, Role: "assistant", Status: "completed", Summary: id,
				ToolName: tool, CreatedAt: base + int64(index), UpdatedAt: base + int64(index),
				PayloadID: payloadIDOf(payload),
			},
			Payload: payload,
		}
	}
	diff := &Payload{
		ID: "pay-edit", Kind: "diff", CreatedAt: base,
		Meta: `{"inlineDiff":{"totalFiles":3}}`, Data: []byte("patch"),
	}
	plan := &Payload{ID: "pay-plan", Kind: "proposed_plan", CreatedAt: base, Meta: "{}", Data: []byte("plan")}

	// One long run whose OLDEST member is a file-change tool, so the run
	// window leaves it unshipped and its count has to reach the reader
	// through the stub; then a proposed_plan payload breaking the run.
	rows := []ImportRow{
		row("i-ask", 0, "user_text", "", nil),
		row("i-edit", 1, "tool_call", "Edit", diff),
	}
	for i := 0; i < 12; i++ {
		rows = append(rows, row(fmt.Sprintf("i-tool-%02d", i), 2+i, "tool_call", "Bash", nil))
	}
	rows = append(rows,
		row("i-plan", 14, "tool_call", "ExitPlanMode", plan),
		row("i-after", 15, "tool_call", "Grep", nil),
	)
	if err := s.ApplyImportBatch("imp", ImportBatch{
		Turns: []Turn{{TurnID: "imp:0", ThreadID: "imp", TurnIndex: 0, StartedAt: base}},
		Rows:  rows,
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}

	const runWindow = 10
	page, err := s.ListThreadSliceAround(context.Background(), "imp", "", 200, runWindow, TimelineSelection{})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("classified %d runs over imported rows, want 2: %+v", len(page.Runs), page.Runs)
	}
	if page.Runs[0].FirstItemID != "i-edit" || page.Runs[0].MemberCount != 13 {
		t.Fatalf("first run = %s with %d members, want i-edit with 13",
			page.Runs[0].FirstItemID, page.Runs[0].MemberCount)
	}
	if page.Runs[1].FirstItemID != "i-after" {
		t.Errorf("second run = %s, want i-after: the imported proposed_plan payload must break the run",
			page.Runs[1].FirstItemID)
	}
	if page.Runs[0].UnshippedBefore != 3 {
		t.Fatalf("unshipped before = %d, want the 3 members outside the run window",
			page.Runs[0].UnshippedBefore)
	}
	if got := groupRows(page.Runs[0], ActivityRunGroupKey{Kind: "tool_call", ToolName: "Edit"}); got != 3 {
		t.Errorf("imported Edit counted %d display rows, want 3 from its imported inlineDiff", got)
	}

	// A local payload row shadows the imported one; the scan resolves it
	// the same way queryHydratedTimelineItems does.
	if err := s.UpdatePayloadMeta("imp", "pay-edit", `{"inlineDiff":{"totalFiles":7}}`); err != nil {
		t.Fatalf("overlay the imported payload: %v", err)
	}
	page, err = s.ListThreadSliceAround(context.Background(), "imp", "", 200, runWindow, TimelineSelection{})
	if err != nil {
		t.Fatalf("window after the overlay: %v", err)
	}
	if got := groupRows(page.Runs[0], ActivityRunGroupKey{Kind: "tool_call", ToolName: "Edit"}); got != 7 {
		t.Errorf("after the overlay the Edit counted %d display rows, want 7", got)
	}
}

func payloadIDOf(payload *Payload) string {
	if payload == nil {
		return ""
	}
	return payload.ID
}

func groupRows(stub ActivityRunStub, key ActivityRunGroupKey) int {
	for _, group := range stub.UnshippedGroups {
		if group.ActivityRunGroupKey == key {
			return group.Rows
		}
	}
	return 0
}

// TestTrimShippedKeepsTheNewestRunningRowPerSide: the running edge a
// fold claims is decided by fold ORDER, and the rule is the same on both
// sides — the newest dropped running row is the one that stands, because
// that is the row the header would name if every row were loaded.
func TestTrimShippedKeepsTheNewestRunningRowPerSide(t *testing.T) {
	s := newTestStore(t)
	// Running rows at 3, 4 (older side of the kept span) and 26, 27
	// (newer side): the newest of each pair must win.
	spec := []byte(runSpec(30))
	for _, at := range []int{3, 4, 26, 27} {
		spec[at] = 'r'
	}
	ids := seedRunThread(t, s, "t", string(spec))

	page, err := s.ListThreadSliceAround(context.Background(), "t", ids[15], 60, 30, TimelineSelection{})
	if err != nil {
		t.Fatalf("anchored slice: %v", err)
	}
	if got := pageItemIDs(page); got[0] != ids[1] || got[len(got)-1] != ids[31] {
		t.Fatalf("page = %v, want the whole run shipped", got)
	}
	stub := onlyRun(t, page)
	if stub.RunningBefore != nil || stub.RunningAfter != nil {
		t.Fatalf("a fully shipped run has no unshipped running edge: %+v", stub)
	}

	// Keep t10..t20 (Items index 10..20 since ids[1] is Items[0]).
	trimmed := page.TrimShipped(9, 20)
	folded := onlyRun(t, trimmed)
	if folded.RunningBefore == nil || folded.RunningBefore.ToolName != runningToolName(4) {
		t.Errorf("running before = %+v, want the newest dropped older row %s", folded.RunningBefore, ids[4])
	}
	if folded.RunningAfter == nil || folded.RunningAfter.ToolName != runningToolName(27) {
		t.Errorf("running after = %+v, want the newest dropped newer row %s", folded.RunningAfter, ids[27])
	}
}

// TestAnchoredPageCentersOnTheRowAChildAnchorRendersInside: a subagent
// child is a valid anchor (a saved scroll position inside a card) but is
// never a page row, so the anchor run centers on the newest member at or
// before the child's coordinate — the row the card renders under.
func TestAnchoredPageCentersOnTheRowAChildAnchorRendersInside(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Members at even item indexes so a child can sit between two of them.
	var ids []string
	for i := 0; i < 40; i++ {
		item := Item{
			ID: "t" + strconv.Itoa(i), ThreadID: "t", TurnIndex: 0, ItemIndex: 2 * i,
			Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "completed",
			Summary: "t" + strconv.Itoa(i), CreatedAt: int64(i + 1),
		}
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert %s: %v", item.ID, err)
		}
		ids = append(ids, item.ID)
	}
	child := Item{
		ID: "child", ThreadID: "t", TurnIndex: 0, ItemIndex: 2*20 + 1, ParentID: ids[20],
		Kind: "tool_call", ToolName: "Read", Role: "assistant", Status: "completed",
		Summary: "child", CreatedAt: 100,
	}
	if err := s.InsertItem(child); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	page, err := s.ListThreadSliceAround(context.Background(), "t", child.ID, 10, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("slice around child: %v", err)
	}
	stub := onlyRun(t, page)
	if stub.LoadedFirstItemID != ids[15] || stub.LoadedLastItemID != ids[24] {
		t.Fatalf("span = %s..%s, want %s..%s centered on the child's parent",
			stub.LoadedFirstItemID, stub.LoadedLastItemID, ids[15], ids[24])
	}
	if len(page.Items) != 10 {
		t.Fatalf("page ships %d rows, want the 10 centered members and never the child", len(page.Items))
	}
}
