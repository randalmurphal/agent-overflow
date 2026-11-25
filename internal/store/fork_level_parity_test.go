package store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// forkLevelParityPath holds what the fork reads below returned when every
// unordered read of a fork rendered a pair of arms per lineage level, the
// form before renderTimelineArms read every level through one pair. It was
// recorded once from that form and is not regenerated: a fork read whose
// result differs from it changed what the fork shows.
const forkLevelParityPath = "testdata/fork_level_parity.json"

// A fork's reads return the same rows through one pair of lineage arms as
// they did through a pair per level, on a chain where nearer levels replace
// rows they inherit: an imported row the source overrode, rows a fork
// rewrote or hid, and a level that rewrote a row a nearer level had already
// rewritten.
func TestForkReadsMatchThePerLevelForm(t *testing.T) {
	raw, err := os.ReadFile(forkLevelParityPath)
	if err != nil {
		t.Fatalf("read the recorded results: %v", err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode the recorded results: %v", err)
	}
	got := forkLevelParityResults(t)
	if len(want) < 100 {
		t.Fatalf("the recorded results hold %d reads; the check proves nothing", len(want))
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got[key] != want[key] {
			t.Errorf("%s:\n got %s\nwant %s", key, got[key], want[key])
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s has no recorded result", key)
		}
	}
}

// forkLevelParityResults seeds the parity chain and returns each read's
// result by thread and read.
func forkLevelParityResults(t *testing.T) map[string]string {
	t.Helper()
	s := newTestStore(t)
	threads := seedForkLevelParityChain(t, s)
	ids := []string{
		"imp-user-0", "imp-launch-0", "imp-child-0", "imp-grandchild-0", "imp-plan-0", "imp-answer-0",
		"imp-user-1", "imp-answer-1", "loc-user-2", "loc-launch-2", "loc-child-2", "loc-grandchild-2",
		"loc-plan-2", "loc-answer-2", "loc-wire-3", "loc-user-3", "loc-answer-3", "par-own-3", "par-own-child-3",
	}
	ctx := context.Background()
	out := map[string]string{}
	record := func(key string, value any, err error) {
		if err != nil {
			out[key] = "error: " + err.Error()
			return
		}
		out[key] = parityValue(value)
	}
	for _, th := range threads {
		items, err := s.ListItems(th)
		record(th+"/ListItems", items, err)
		page, err := s.ListThreadSliceAround(ctx, th, "", 100, testRunWindowRows, TimelineSelection{}, AgentMessagesAreMembers)
		record(th+"/window", page.Items, err)
		for _, root := range []string{"imp-launch-0", "loc-launch-2"} {
			page, err := s.ListThreadSliceAround(ctx, th, "", 100, testRunWindowRows, TimelineSelection{ScopeRootID: root}, AgentMessagesAreMembers)
			record(th+"/agent scope "+root, page.Items, err)
		}
		hits, err := s.SearchThreadItems(ctx, th, "answer", TimelineSelection{}, 50, nil)
		record(th+"/search", hits.Items, err)
		for turn := range 4 {
			at := fmt.Sprintf("%s/turn %d/", th, turn)
			items, err := s.ListTurnItems(th, turn)
			record(at+"ListTurnItems", items, err)
			items, err = s.ListUnsettledTurnItems(th, turn)
			record(at+"ListUnsettledTurnItems", items, err)
			items, err = s.ListTurnItemsOfKind(th, turn, "user_text")
			record(at+"ListTurnItemsOfKind", items, err)
			prefixed, err := s.TurnItemIDsWithPrefix(th, turn, "loc-")
			record(at+"TurnItemIDsWithPrefix", prefixed, err)
			item, found, err := s.FindTurnItem(th, turn, "assistant_text")
			record(at+"FindTurnItem", []any{found, item}, err)
			index, found, err := s.MaxItemIndexForTurn(th, turn)
			record(at+"MaxItemIndexForTurn", []any{found, index}, err)
		}
		// Every turn the chain holds and five it does not: the bounds a new
		// row at the turn's head or tail is placed from.
		for turn := range 9 {
			for _, bound := range []struct{ name, query string }{{"MIN", minTurnItemIndexSQL}, {"MAX", maxTurnItemIndexSQL}} {
				index, err := turnItemIndexBound(s.reader(), bound.query, th, turn)
				record(fmt.Sprintf("%s/turn %d/%s item index", th, turn, bound.name), []any{index.Valid, index.Int64}, err)
			}
		}
		for _, id := range ids {
			item, found, err := s.GetThreadItem(th, id)
			record(th+"/GetThreadItem "+id, []any{found, item}, err)
		}
		wire, err := s.ListWireItems(th, ids)
		record(th+"/ListWireItems", wire, err)
		meta, count, err := s.SnapshotSubagentExecutionMeta(th, "loc-launch-2", "", "{}", 2, 1, 6)
		record(th+"/SnapshotSubagentExecutionMeta", []any{meta, count}, err)
		record(th+"/cards", subagentCardsForTest(t, s, s.reader(), th), nil)
	}
	return out
}

// seedForkLevelParityChain writes the parity thread and three pointer
// forks of it, each of the one before, and returns all four. The first
// fork rewrites a local and an imported row it inherits and hides one; the
// second rewrites the first's copy and an agent's child and hides a row
// the first inherited; the third is cut inside turn 2 and writes rows of
// its own past the cut.
func seedForkLevelParityChain(t *testing.T, s *Store) []string {
	t.Helper()
	seedTimelineParityThread(t, s)
	const f1, f2, f3 = "par-f1", "par-f2", "par-f3"
	rewrite := func(threadID, id, summary string) {
		t.Helper()
		if _, err := s.UpdateItemFields(threadID, id, ItemPartialUpdate{Summary: &summary}); err != nil {
			t.Fatalf("rewrite %s in %s: %v", id, threadID, err)
		}
	}
	hide := func(threadID, id string) {
		t.Helper()
		if err := s.DeleteThreadItem(threadID, id); err != nil {
			t.Fatalf("hide %s in %s: %v", id, threadID, err)
		}
	}
	mustPointerFork(t, s, timelineParityThreadID, f1, ForkCut{})
	rewrite(f1, "loc-answer-2", "answer f1 rewrote")
	rewrite(f1, "imp-answer-1", "imported answer f1 rewrote")
	hide(f1, "imp-answer-0")
	mustPointerFork(t, s, f1, f2, ForkCut{})
	rewrite(f2, "loc-answer-2", "answer f2 rewrote over f1")
	summary := "Grep f2 rewrote"
	if err := updateFieldsCarded(s, f2, "imp-launch-0", "imp-child-0", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatalf("rewrite imp-child-0 in %s: %v", f2, err)
	}
	hide(f2, "loc-answer-3")
	mustPointerFork(t, s, f2, f3, throughTurn(2))
	if err := s.InsertTurn(Turn{TurnID: f3 + ":3", ThreadID: f3, TurnIndex: 3, StartedAt: 1_700_000_000_300}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []Item{
		{ID: "par-own-3", ThreadID: f3, TurnIndex: 3, ItemIndex: 0, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "f3 answer", Meta: "{}"},
		{ID: "par-own-child-3", ThreadID: f3, TurnIndex: 3, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", ParentID: "loc-launch-2", Summary: "Bash f3", Meta: "{}"},
	} {
		item.CreatedAt, item.UpdatedAt = 1_700_000_000_300+int64(item.ItemIndex), 1_700_000_000_300+int64(item.ItemIndex)
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("insert %s: %v", item.ID, err)
		}
	}
	return []string{timelineParityThreadID, f1, f2, f3}
}

// parityValue renders a read's result with the fields a lineage form
// decides: which row, where, and what it holds. Write times are left out.
func parityValue(value any) string {
	render := func(item Item) string {
		return fmt.Sprintf("%s@%d.%d %s/%s/%s parent=%s %q meta=%s payload=%s input=%s",
			item.ID, item.TurnIndex, item.ItemIndex, item.Kind, item.ToolName, item.Status,
			item.ParentID, item.Summary, item.Meta, item.PayloadID, item.InputPayloadID)
	}
	switch v := value.(type) {
	case []Item:
		rows := make([]string, len(v))
		for i, item := range v {
			rows[i] = render(item)
		}
		return "[" + strings.Join(rows, "; ") + "]"
	case []any:
		parts := make([]string, len(v))
		for i, part := range v {
			if item, ok := part.(Item); ok {
				parts[i] = render(item)
				continue
			}
			parts[i] = fmt.Sprint(part)
		}
		return strings.Join(parts, " ")
	default:
		// fmt prints a map's keys in order.
		return fmt.Sprintf("%+v", v)
	}
}
