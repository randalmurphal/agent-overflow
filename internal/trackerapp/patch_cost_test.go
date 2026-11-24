package trackerapp

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// A one-row change of a realistic board costs a client holding three
// sections and the board's totals one event naming the issue and one
// patch read of two store calls (the root, then one read transaction),
// with no totals for a change that moved none; the bytes of both, and of
// a patch with the totals, are pinned so a change that grows them shows.
func TestOneRowChangeCost(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cs := newCountingStore(m.st)
	m.svc.store = cs
	scope := m.scope()
	at := time.UnixMilli(1_799_000_000_000).UTC()
	rows := realisticRows(0, readBatch, at)
	m.tracks(rows)
	sections := []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionBacklog}, {Kind: SectionColumn}}
	if _, err := m.svc.SubscribeBoard(m.project(), scope, sections, untied); err != nil {
		t.Fatal(err)
	}
	first := len(m.events.boardEvents(t, 1))
	// The client holds the totals of the board it read.
	v := must[BoardView](t)(m.svc.Board(scope, sections))
	held := TotalsAt{Lifetime: v.Lifetime, Revision: v.TotalsRevision}

	changed := slices.Clone(rows)
	changed[3] = realisticRow(3, at.Add(time.Hour))
	changed[3].Card.Title = "Make the checkout page load faster, part 3, now with a cache"
	m.tracks(changed)
	if out := must[Outcome](t)(m.svc.syncTick(t.Context(), scope, BoardDue{Delta: true}, Background)); out.Changes().Empty() {
		t.Fatalf("the change changed nothing: %+v", out)
	}
	events := m.events.boardEvents(t, first+1)
	ev := events[len(events)-1]
	if !slices.Equal(ev.Changed, []tracker.IssueID{changed[3].Card.ID}) || len(ev.Removed) != 0 || ev.ReadAgain || ev.Truncated {
		t.Fatalf("the event of a one-row change: %+v", ev)
	}

	cs.take()
	patch := must[BoardPatch](t)(m.svc.BoardPatch(scope, sections, ev.Changed, held))
	if calls, want := cs.take(), map[string]int{"GetMirrorRoot": 1, "ReadMirrorBoardPatch": 1}; !maps.Equal(calls, want) {
		t.Fatalf("a patch read made %s; pinned %s", describeCounts(calls), describeCounts(want))
	}
	if len(patch.Sections[0].Rows) != 1 || patch.Sections[0].Rows[0].Card.Title != changed[3].Card.Title {
		t.Fatalf("the patch's sprint section: %+v", patch.Sections[0])
	}
	// A title moves no total: the patch has none.
	if patch.Totals != nil {
		t.Fatalf("a title change's patch has totals %+v", patch.Totals)
	}
	totals := must[BoardPatch](t)(m.svc.BoardPatch(scope, sections, ev.Changed, TotalsAt{}))
	sizes := map[string]int{"event": len(must[[]byte](t)(json.Marshal(ev))), "patch": len(must[[]byte](t)(json.Marshal(patch))),
		"patch with totals": len(must[[]byte](t)(json.Marshal(totals)))}
	t.Logf("one-row change bytes: %s", describeCounts(sizes))
	if want := map[string]int{"event": 166, "patch": 1174, "patch with totals": 1487}; !maps.Equal(sizes, want) {
		t.Errorf("one-row change bytes %s, pinned %s", describeCounts(sizes), describeCounts(want))
	}
}
