package trackerapp

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// timeStructure is the test board's structure with its estimate in time.
func timeStructure(b tracker.BoardID) tracker.Structure {
	st := webStructure(b)
	st.Estimate.Unit = tracker.UnitTime
	return st
}

// pointsStructure is the test board's structure estimating in the points
// field field.
func pointsStructure(b tracker.BoardID, field string) tracker.Structure {
	st := webStructure(b)
	st.Estimate.Field = field
	return st
}

// A board that switches from one points field to another, its issues
// unchanged, shows none of the old field's numbers as the new one's: its
// rows and totals show no estimate until the walk, which starts over,
// reads each row again in the new field, and each row read again makes
// its issue's history due, since estimate changes are read per field.
func TestAStructureThatSwitchesPointFieldsReadsTheRowsAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	m.p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
		return pointsStructure(b, "customfield_10016"), nil
	}
	five, eight := 5.0, 8.0
	r := row("10001", 1_799_000_000_000, "a")
	r.Card.Sprints, r.Estimate = []tracker.SprintID{"1"}, &five
	m.p.changedRows = pagedDelta(t, []tracker.Row{r})
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		return tracker.History{Issues: []tracker.IssueHistory{{Issue: "10001", CompleteTo: time.UnixMilli(1_799_900_000_000)}}}, nil
	}
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true, History: true}, Background))
	sprint := []Section{{Kind: SectionSprint, Sprint: "1"}}
	if v := must[BoardView](t)(m.svc.Board(m.scope(), sprint)); v.Sprints[0].Total.Estimate != 5 || len(m.due()) != 0 {
		t.Fatalf("seeded: sprint total %+v, due %v", v.Sprints[0].Total, m.due())
	}
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			return pointsStructure(b, "customfield_10026"), nil
		}
	})
	out := must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), sprint))
	if row := v.Sections[0].Rows[0]; !out.Changes().Structure || row.Estimate != nil || v.Sprints[0].Total != (SectionTotal{Rows: 1}) {
		t.Fatalf("after the switch: changed %v, row estimate %v, sprint total %+v; want the old field's number unknown", out.Changes().Structure, row.Estimate, v.Sprints[0].Total)
	}
	if c := m.sync(testBoard).Cursor; len(c) != 0 {
		t.Fatalf("the cursor survived the switch: %q", c)
	}
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			if q.From != nil {
				t.Errorf("the walk after the switch started from %q", q.From)
			}
			again := r
			again.Estimate = &eight
			return tracker.RowPage{Rows: []tracker.Row{again}, Next: cursorAfter("10001")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	v = must[BoardView](t)(m.svc.Board(m.scope(), sprint))
	if row := v.Sections[0].Rows[0]; row.Estimate == nil || *row.Estimate != eight || v.Sprints[0].Total.Estimate != eight {
		t.Fatalf("the row read again: %v, sprint total %+v", row.Estimate, v.Sprints[0].Total)
	}
	if due := m.due(); len(due) != 1 || due[0] != "10001" {
		t.Fatalf("due after the row was read again %v; want its history read again", due)
	}
}

// A tick that reads the structure and then the delta, where the structure
// changes the board's estimate unit, walks the delta from the start of the
// window, not from the cursor it read at its start: a row the old walk had
// passed is read again in the new unit.
func TestAStructureThatResetsTheEstimateWalksFromTheStart(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	five, hour := 5.0, 3600.0
	r := row("10001", 1_799_000_000_000, "a")
	r.Estimate = &five
	m.p.changedRows = pagedDelta(t, []tracker.Row{r})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if c := string(m.sync(testBoard).Cursor); c != string(cursorAfter("10001")) {
		t.Fatalf("seeded cursor %q", c)
	}
	var mu sync.Mutex
	var froms []string
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) { return timeStructure(b), nil }
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			mu.Lock()
			froms = append(froms, string(q.From))
			mu.Unlock()
			if q.From != nil {
				// Nothing changed after the old walk's cursor.
				return tracker.RowPage{Next: q.From}, nil
			}
			again := r
			again.Estimate = &hour
			return tracker.RowPage{Rows: []tracker.Row{again}, Next: cursorAfter("10001-time")}, nil
		}
	})
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background))
	mu.Lock()
	defer mu.Unlock()
	if len(froms) != 1 || froms[0] != "" {
		t.Fatalf("the delta after the unit change read from %q; want the start", froms)
	}
	v := must[BoardView](t)(m.svc.Board(m.scope(), []Section{{Kind: SectionBacklog}}))
	if rows := v.Sections[0].Rows; v.Estimate.Unit != "time" || len(rows) != 1 || rows[0].Estimate == nil || *rows[0].Estimate != hour {
		t.Fatalf("the row after the unit change: unit %s, rows %+v", v.Estimate.Unit, rows)
	}
	if c := string(m.sync(testBoard).Cursor); c != string(cursorAfter("10001-time")) {
		t.Fatalf("cursor %q after the new walk", c)
	}
}

// resetAfterPage is a store that runs reset once, after the first row page
// it stores: another run's structure write landing during a delta walk.
type resetAfterPage struct {
	Store
	once  sync.Once
	reset func()
}

func (s *resetAfterPage) WriteMirrorRowPage(rootID int64, page store.MirrorRowPage, cursor []byte) (store.MirrorChanges, error) {
	ch, err := s.Store.WriteMirrorRowPage(rootID, page, cursor)
	s.once.Do(s.reset)
	return ch, err
}

// A delta walk whose board's estimate definition changes under it stops at
// the first page whose cursor the store refuses, with More, and stores no
// cursor of the old walk; the next tick walks from the start.
func TestADeltaStopsWhenAResetRefusesItsCursor(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	pages := [][]tracker.Row{{row("10001", 1_799_000_000_000, "a")}, {row("10002", 1_799_000_000_000, "b")}, {row("10003", 1_799_000_000_000, "c")}}
	var mu sync.Mutex
	var froms []string
	paged := pagedDelta(t, pages...)
	m.p.set(func(p *scripted) {
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			mu.Lock()
			froms = append(froms, string(q.From))
			mu.Unlock()
			return paged(q)
		}
	})
	m.svc.store = &resetAfterPage{Store: m.svc.store, reset: func() {
		st, err := mirrorStructure(timeStructure(testBoard), m.now().UnixMilli())
		if err != nil {
			t.Error(err)
			return
		}
		st.Lifetime = m.sync(testBoard).Lifetime
		if _, err := m.st.ReplaceMirrorStructure(m.root(), st); err != nil {
			t.Error(err)
		}
	}}
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	mu.Lock()
	read := len(froms)
	mu.Unlock()
	if !out.More() || read != 2 {
		t.Fatalf("the walk under a reset: more %v after %d pages; want more after 2", out.More(), read)
	}
	if c := m.sync(testBoard).Cursor; c != nil {
		t.Fatalf("the old walk left cursor %q", c)
	}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	mu.Lock()
	defer mu.Unlock()
	if len(froms) < 3 || froms[2] != "" {
		t.Fatalf("the next tick read from %q; want the start", froms[2:])
	}
	if c := string(m.sync(testBoard).Cursor); c != string(cursorAfter("10003")) {
		t.Fatalf("the next walk's cursor %q", c)
	}
}

// syncAfterRead is a store that runs between once, after the first board
// view or section page it reads: a sync that commits right after the read
// of the rows.
type syncAfterRead struct {
	Store
	once    sync.Once
	between func()
}

func (s *syncAfterRead) ReadMirrorBoardView(rootID int64, boardID string, sections []store.MirrorSection) (store.MirrorBoardView, bool, error) {
	v, ok, err := s.Store.ReadMirrorBoardView(rootID, boardID, sections)
	s.once.Do(s.between)
	return v, ok, err
}

func (s *syncAfterRead) ListMirrorSection(rootID int64, boardID string, section store.MirrorSection, after store.MirrorRowKey) (store.MirrorSectionPage, error) {
	page, err := s.Store.ListMirrorSection(rootID, boardID, section, after)
	s.once.Do(s.between)
	return page, err
}

// A board view and a section page name their cards' statuses, users and
// parents from the state their rows came from: a sync that commits right
// after the rows were read, moving a status to done and renaming a
// parent, changes neither, so no backlog card shows as done.
func TestABoardsNamesComeFromItsRowsState(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	m.seedBoard(t)
	base := m.svc.store
	regroup := func(group tracker.StatusGroup) {
		m.p.set(func(p *scripted) {
			p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
				st := webStructure(b)
				st.Catalog.Statuses[0].Group = group
				return st, nil
			}
		})
		must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	}
	m.svc.store = &syncAfterRead{Store: base, between: func() {
		regroup(tracker.GroupDone)
		epic := row("10004", 1_799_000_000_001, "d")
		epic.Card.Title, epic.Card.Type = "The renamed epic", tracker.IssueType{ID: "t1", Name: "Epic", Level: 1}
		m.p.set(func(p *scripted) { p.changedRows = pagedDelta(t, []tracker.Row{epic}) })
		must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	}}
	v := must[BoardView](t)(m.svc.Board(m.scope(), []Section{{Kind: SectionBacklog}, {Kind: SectionSprint, Sprint: "1"}}))
	backlog := v.Sections[0].Rows
	if v.Backlog.Rows != len(backlog) || len(backlog) != 2 {
		t.Fatalf("backlog total %d, %d rows", v.Backlog.Rows, len(backlog))
	}
	for _, r := range backlog {
		if r.Card.Status.Group != "todo" {
			t.Fatalf("backlog card %s shows status %+v; want the rows' state", r.Card.ID, r.Card.Status)
		}
	}
	if p := v.Sections[1].Rows[0].Card.Parent; p == nil || p.Title != "The epic" {
		t.Fatalf("parent %+v; want the rows' state", p)
	}
	// A section page, read before s1 leaves the done group.
	m.svc.store = &syncAfterRead{Store: base, between: func() { regroup(tracker.GroupInProgress) }}
	page := must[SectionPage](t)(m.svc.SectionRows(m.scope(), Section{Kind: SectionColumn, Column: 0}, nil))
	if len(page.Rows) == 0 {
		t.Fatal("the first column is empty")
	}
	for _, r := range page.Rows {
		if r.Card.Status.Group != "done" {
			t.Fatalf("column card %s shows status %+v; want the rows' state", r.Card.ID, r.Card.Status)
		}
	}
}
