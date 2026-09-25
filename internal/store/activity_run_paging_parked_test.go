package store

import (
	"context"
	"testing"
)

// A background agent's parked stops pair with its launch for the counts
// but do not supersede its status (activityScanRow.endsLaunch): the run
// names the launch running until its ending completion is a member. These
// cases hold that across the byte trim (§2.2), which folds shipped
// members back into the stub in either order.

var parkedAgentKey = ActivityRunGroupKey{Kind: "tool_call", ToolName: "Agent"}

// seedParkedAgentRun writes p0 L1 t2 P3 t4 X5 t6 p7: prose, a running
// background Agent launch L1, Bash calls, L1's parked stop P3, and at 5
// a second parked stop of L1, or its ending completion when ends is set.
func seedParkedAgentRun(t *testing.T, s *Store, threadID string, ends bool) []string {
	t.Helper()
	if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	fifth := Item{ID: "P5", Kind: "tool_completion", ToolName: "Agent", Status: ItemStatusParked, CompletionOf: "L1"}
	if ends {
		fifth = Item{ID: "E5", Kind: "tool_completion", ToolName: "Agent", Status: "completed", CompletionOf: "L1"}
	}
	rows := []Item{
		{ID: "p0", Kind: "assistant_text", Status: "completed"},
		{ID: "L1", Kind: "tool_call", ToolName: "Agent", Status: "running", IsBackground: true},
		{ID: "t2", Kind: "tool_call", ToolName: "Bash", Status: "completed"},
		{ID: "P3", Kind: "tool_completion", ToolName: "Agent", Status: ItemStatusParked, CompletionOf: "L1"},
		{ID: "t4", Kind: "tool_call", ToolName: "Bash", Status: "completed"},
		fifth,
		{ID: "t6", Kind: "tool_call", ToolName: "Bash", Status: "completed"},
		{ID: "p7", Kind: "assistant_text", Status: "completed"},
	}
	ids := make([]string, 0, len(rows))
	for i, item := range rows {
		item.ThreadID = threadID
		item.ItemIndex = i
		item.Role = "assistant"
		item.Summary = item.ID
		item.CreatedAt = int64(i + 1)
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("insert %s: %v", item.ID, err)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

// wholeParkedAgentPage reads the thread's tail page, which ships every row:
// Items[i] is ids[i].
func wholeParkedAgentPage(t *testing.T, s *Store, threadID string, ids []string) PagedItems {
	t.Helper()
	page, err := s.ListThreadSliceAround(context.Background(), threadID, "", 20, 20, TimelineSelection{})
	if err != nil {
		t.Fatalf("tail slice: %v", err)
	}
	if got := pageItemIDs(page); !sameStrings(got, ids) {
		t.Fatalf("page = %v, want every row %v", got, ids)
	}
	return page
}

type parkedTrimCase struct {
	name           string
	from, to       int
	wantRange      []string
	wantPaired     []string
	wantSuperseded []string
	wantBefore     *ActivityRunGroupKey
	wantAfter      *ActivityRunGroupKey
}

func assertParkedTrim(t *testing.T, s *Store, threadID string, page PagedItems, tc parkedTrimCase) {
	t.Helper()
	trimmed := page.TrimShipped(tc.from, tc.to)
	assertPageAccounts(t, s, threadID, trimmed, tc.wantRange)
	folded := onlyRun(t, trimmed)
	if got := folded.UnshippedPairedLaunchIDs; !sameStrings(got, tc.wantPaired) {
		t.Errorf("unshipped paired = %v, want %v", got, tc.wantPaired)
	}
	if got := folded.ShippedSupersededLaunchIDs; !sameStrings(got, tc.wantSuperseded) {
		t.Errorf("shipped superseded = %v, want %v", got, tc.wantSuperseded)
	}
	if !sameGroupKey(folded.RunningBefore, tc.wantBefore) {
		t.Errorf("running before = %+v, want %+v", folded.RunningBefore, tc.wantBefore)
	}
	if !sameGroupKey(folded.RunningAfter, tc.wantAfter) {
		t.Errorf("running after = %+v, want %+v", folded.RunningAfter, tc.wantAfter)
	}
}

func sameGroupKey(got, want *ActivityRunGroupKey) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// TestTrimShippedKeepsAParkedLaunchRunning: with only parked stops of it
// in the run, a folded launch is the running member before the span, a
// shipped launch is never named superseded, and the launch stays paired
// while any parked stop of it is still shipped.
func TestTrimShippedKeepsAParkedLaunchRunning(t *testing.T) {
	s := newTestStore(t)
	ids := seedParkedAgentRun(t, s, "t", false)
	page := wholeParkedAgentPage(t, s, "t", ids)
	if stub := onlyRun(t, page); len(stub.ShippedSupersededLaunchIDs) != 0 || len(stub.UnshippedPairedLaunchIDs) != 0 {
		t.Fatalf("a wholly shipped run names no launch: %+v", stub)
	}
	cases := []parkedTrimCase{
		{name: "older side folds the launch", from: 3, to: 8, wantRange: ids[1:8],
			wantPaired: []string{"L1"}, wantBefore: &parkedAgentKey},
		{name: "older side folds the launch and one parked stop", from: 4, to: 8, wantRange: ids[1:8],
			wantPaired: []string{"L1"}, wantBefore: &parkedAgentKey},
		{name: "newer side folds both parked stops", from: 0, to: 3, wantRange: ids[0:7]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertParkedTrim(t, s, "t", page, tc) })
	}
	// Two trims in sequence agree with one trim of the same span.
	staged := onlyRun(t, page.TrimShipped(3, 8).TrimShipped(1, 4))
	if !sameStrings(staged.UnshippedPairedLaunchIDs, []string{"L1"}) || len(staged.ShippedSupersededLaunchIDs) != 0 {
		t.Errorf("staged trims left %v / %v, want [L1] / []", staged.UnshippedPairedLaunchIDs, staged.ShippedSupersededLaunchIDs)
	}
}

// TestTrimShippedSupersedesALaunchOnlyByItsEndingCompletion: once the
// ending completion is a member, it alone supersedes the launch; a parked
// stop still pairs.
func TestTrimShippedSupersedesALaunchOnlyByItsEndingCompletion(t *testing.T) {
	s := newTestStore(t)
	ids := seedParkedAgentRun(t, s, "t", true)
	page := wholeParkedAgentPage(t, s, "t", ids)
	cases := []parkedTrimCase{
		{name: "newer side folds the ending completion", from: 0, to: 5, wantRange: ids[0:7],
			wantSuperseded: []string{"L1"}},
		{name: "older side folds the launch", from: 3, to: 8, wantRange: ids[1:8],
			wantPaired: []string{"L1"}},
		{name: "both sides fold, keeping neither", from: 4, to: 5, wantRange: ids[1:7]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertParkedTrim(t, s, "t", page, tc) })
	}
}
