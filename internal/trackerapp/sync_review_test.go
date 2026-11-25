package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// An issue the mirror holds that a second board lists has its history
// read again with both boards, so the second board's estimate changes are
// asked for: which estimate changes come back depends on the boards the
// query names, not on the issue's update time.
func TestHistoryOfAnIssueOnANewBoardIsReadWithThatBoard(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var mu sync.Mutex
	var queries []tracker.HistoryQuery
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		var h tracker.History
		for _, id := range q.Issues {
			h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now()})
		}
		return h, nil
	}
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	if len(queries) != 1 || !slices.Equal(queries[0].Boards, []tracker.BoardID{testBoard}) {
		t.Fatalf("the first history queries %+v", queries)
	}
	// Board 2 lists the issue, unchanged since its history was read.
	second := Scope{Connection: m.conn.ID, Board: "2"}
	m.p.set(func(p *scripted) {
		p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "b")})
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), second, Background))
	must[Outcome](t)(m.svc.tickHistory(t.Context(), second, Background))
	if !slices.ContainsFunc(queries[1:], func(q tracker.HistoryQuery) bool {
		return slices.Contains(q.Issues, "10001") && slices.Contains(q.Boards, "2") && slices.Contains(q.Boards, testBoard)
	}) {
		t.Fatalf("no history query named 10001 with boards 1 and 2: %+v", queries[1:])
	}
	// The history stored covers both boards: nothing is due after it.
	asked := len(queries)
	must[Outcome](t)(m.svc.tickHistory(t.Context(), second, Background))
	if len(queries) != asked {
		t.Fatalf("a history tick after both boards were read asked again: %+v", queries[asked:])
	}
}

// A structure read that a delta page's new sprint asked for and that
// failed after the page was stored is read again by the next tick, and
// the failure stands until it succeeds: the next page has no rows, so
// nothing in it asks again.
func TestAFailedCascadeStructureReadIsReadAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.structure = func(tracker.BoardID, time.Time) (tracker.Structure, error) {
			return tracker.Structure{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
		p.changedRows = pagedDelta(t, []tracker.Row{withSprint(row("10001", 1_799_000_000_000, "a"), 1_799_000_000_001, "3")})
	})
	if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); out.Failure == nil {
		t.Fatalf("the tick whose structure read failed: %+v", out)
	}
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "3", Board: b, Name: "Sprint 3", State: tracker.SprintActive})
			return st, nil
		}
	})
	reads := m.structureReads()
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil))
	if m.structureReads() != reads+1 || !slices.ContainsFunc(v.Sprints, func(s SprintView) bool { return s.ID == "3" }) {
		t.Fatalf("the next tick %+v read the structure %d times; sprints %+v", out, m.structureReads()-reads, v.Sprints)
	}
}

// A sprint created after the structure read is read for even when the
// scope has more unexplained references than any in-memory bound: the
// mirror lacking it is decided from durable state, not guessed old.
func TestANewSprintReadsTheStructurePastManyUnexplained(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	var pages [][]tracker.Row
	for p := range 3 {
		var page []tracker.Row
		for i := range readBatch {
			n := p*readBatch + i
			page = append(page, withSprint(row(fmt.Sprintf("2%04d", n), 1_799_000_000_000, fmt.Sprintf("o%04d", n)), 1_799_000_000_000, fmt.Sprintf("x%04d", n)))
		}
		pages = append(pages, page)
	}
	m.p.set(func(p *scripted) { p.changedRows = pagedDelta(t, pages...) })
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	// Sprint "new" is made after every read so far.
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "new", Board: b, Name: "Sprint new", State: tracker.SprintActive})
			return st, nil
		}
	})
	d := &rowsDelta{}
	m.p.set(func(p *scripted) { p.changedRows = d.read })
	d.set(withSprint(row("10001", 1_799_000_000_000, "a"), 1_799_000_000_001, "new"))
	reads := m.structureReads()
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil))
	if m.structureReads() != reads+1 || !slices.ContainsFunc(v.Sprints, func(s SprintView) bool { return s.ID == "new" }) {
		t.Fatalf("a new sprint read the structure %d times; sprints %d", m.structureReads()-reads, len(v.Sprints))
	}
}

// syncBetween is a store that runs between once, after the view's first
// board read or before its first section read, whichever comes first: a
// sync that commits while a board view is being read.
type syncBetween struct {
	Store
	once    sync.Once
	between func()
}

func (s *syncBetween) ReadMirrorBoardView(rootID int64, boardID string, regroup store.MirrorRegroup, sections []store.MirrorSection) (store.MirrorBoardView, bool, error) {
	v, ok, err := s.Store.ReadMirrorBoardView(rootID, boardID, regroup, sections)
	s.once.Do(s.between)
	return v, ok, err
}

func (s *syncBetween) GetMirrorBoard(rootID int64, boardID string) (store.MirrorStructure, bool, error) {
	st, ok, err := s.Store.GetMirrorBoard(rootID, boardID)
	s.once.Do(s.between)
	return st, ok, err
}

func (s *syncBetween) ListMirrorSection(rootID int64, boardID string, regroup store.MirrorRegroup, section store.MirrorSection, after store.MirrorRowKey) (store.MirrorSectionPage, error) {
	s.once.Do(s.between)
	return s.Store.ListMirrorSection(rootID, boardID, regroup, section, after)
}

// One board view's totals and rows describe one state of the mirror: a
// sync that commits while the view is read shows in both or in neither.
func TestBoardTotalsAndRowsAreOneState(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	before := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionBacklog}}))
	if before.Backlog.Rows != len(before.Sections[0].Rows) {
		t.Fatalf("a quiet board: backlog total %d, %d rows", before.Backlog.Rows, len(before.Sections[0].Rows))
	}
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{row("10005", 1_799_000_000_000, "e")}, Next: cursorAfter("10005")}, nil
		}
	})
	m.svc.store = &syncBetween{Store: m.svc.store, between: func() {
		must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	}}
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionBacklog}}))
	if v.Backlog.Rows != len(v.Sections[0].Rows) {
		t.Fatalf("a board read across a sync: backlog total %d, %d rows", v.Backlog.Rows, len(v.Sections[0].Rows))
	}
	after := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionBacklog}}))
	if after.Backlog.Rows != before.Backlog.Rows+1 || len(after.Sections[0].Rows) != after.Backlog.Rows {
		t.Fatalf("after the sync: backlog total %d, %d rows, before %d", after.Backlog.Rows, len(after.Sections[0].Rows), before.Backlog.Rows)
	}
}

// A row's estimate is shown only in the unit it was read in: after the
// board's estimate changes unit, a row read before shows no estimate until
// it is read again.
func TestARowEstimateShowsOnlyInItsOwnUnit(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	m.seedBoard(t)
	sprint := []Section{{Kind: SectionSprint, Sprint: "1"}}
	if v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, sprint)); v.Estimate.Unit != "points" || v.Sections[0].Rows[0].Estimate == nil {
		t.Fatalf("seeded: unit %s, row %+v", v.Estimate.Unit, v.Sections[0].Rows[0])
	}
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Estimate.Unit = tracker.UnitTime
			return st, nil
		}
	})
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, sprint))
	if r := v.Sections[0].Rows[0]; v.Estimate.Unit != "time" || r.Estimate != nil || v.Sprints[0].Total != (SectionTotal{Rows: 1}) {
		t.Fatalf("a row read in points shows %v in the board's unit %s; sprint total %+v", r.Estimate, v.Estimate.Unit, v.Sprints[0].Total)
	}
	if iv := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); iv.Boards[0].Estimate != nil {
		t.Fatalf("the issue's row shows estimate %v", *iv.Boards[0].Estimate)
	}
	// The unit change cleared the cursor: the next delta walks from the
	// window and reads the row again, in the new unit.
	if c := m.sync(testBoard).Cursor; len(c) != 0 {
		t.Fatalf("the cursor survived the unit change: %q", c)
	}
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			if q.From != nil {
				t.Errorf("the walk after the unit change started from %q", q.From)
			}
			r := row("10001", 1_799_000_000_000, "a")
			r.Card.Sprints = []tracker.SprintID{"1"}
			hour := 3600.0
			r.Estimate = &hour
			return tracker.RowPage{Rows: []tracker.Row{r}, Next: cursorAfter("10001")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	v = must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, sprint))
	if r := v.Sections[0].Rows[0]; r.Estimate == nil || *r.Estimate != 3600 || v.Sprints[0].Total.Estimate != 3600 {
		t.Fatalf("the row read again: %v, sprint total %+v", r.Estimate, v.Sprints[0].Total)
	}
}

// A section page asked for after a structure read moved the section's
// statuses to another column is stale: the column's rows changed under
// the cursor.
func TestAColumnCursorIsStaleAfterItsStatusesMove(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	var rows []tracker.Row
	for i := range 150 {
		id := fmt.Sprintf("%05d", 40000+i)
		rows = append(rows, row(id, 1_799_000_000_000, id))
	}
	m.p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
		return tracker.RowPage{Rows: rows, Next: cursorAfter("x")}, nil
	}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	first := must[SectionPage](t)(m.svc.SectionRows(m.scope(), StatusGroups{}, Section{Kind: SectionColumn, Column: 0}, nil))
	if len(first.Rows) != 100 || first.Next == nil {
		t.Fatalf("first page: %d rows, next %+v", len(first.Rows), first.Next)
	}
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Columns[0].Statuses, st.Columns[1].Statuses = st.Columns[1].Statuses, st.Columns[0].Statuses
			return st, nil
		}
	})
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	page, err := m.svc.SectionRows(m.scope(), StatusGroups{}, Section{Kind: SectionColumn, Column: 0}, first.Next)
	if !errors.Is(err, ErrStalePage) {
		t.Fatalf("the page after the columns changed: %d rows, %v", len(page.Rows), err)
	}
}
