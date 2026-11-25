package trackerapp

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// rowsDelta scripts ChangedRows to answer what rows holds now, as one
// page.
type rowsDelta struct {
	mu   sync.Mutex
	rows []tracker.Row
}

func (d *rowsDelta) set(rows ...tracker.Row) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows = rows
}

func (d *rowsDelta) read(tracker.DeltaQuery) (tracker.RowPage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return tracker.RowPage{Rows: slices.Clone(d.rows), Next: cursorAfter("last")}, nil
}

// withSprint is r edited at updated to be in sprint sp.
func withSprint(r tracker.Row, updated int64, sp string) tracker.Row {
	r.Card.Updated = time.UnixMilli(updated).UTC()
	r.Card.Sprints = []tracker.SprintID{tracker.SprintID(sp)}
	return r
}

func (m *mirrorFixture) structureReads() int { return m.p.count("Structure") }

// An edited issue that names a sprint the structure does not list reads
// the structure once: the complete read marks the sprint unexplained, so
// an edit naming it again reads nothing. A sprint created after that read
// reads it again.
func TestAnUnlistedSprintReadsTheStructureOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	d := &rowsDelta{}
	m.p.set(func(p *scripted) { p.changedRows = d.read })
	base := row("10001", 1_799_000_000_000, "a")
	d.set(withSprint(base, 1_799_000_000_001, "7"))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.structureReads(); got != 2 {
		t.Fatalf("%d structure reads, want the first and one for sprint 7", got)
	}
	d.set(withSprint(base, 1_799_000_000_002, "7"))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.structureReads(); got != 2 {
		t.Fatalf("an edit naming sprint 7 again read the structure: %d reads", got)
	}
	// Sprint 3 is made after the last structure read.
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "3", Board: b, Name: "Sprint 3", State: tracker.SprintFuture})
			return st, nil
		}
	})
	d.set(withSprint(base, 1_799_000_000_003, "3"))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.structureReads(); got != 3 {
		t.Fatalf("a new sprint did not read the structure: %d reads", got)
	}
}

// A row a tick stores after the tick's structure read, naming what that
// read did not list, reads the structure again in the same tick, once;
// the next edit naming it reads nothing.
func TestARowAfterTheTicksStructureReadReadsItAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	r := row("10001", 1_799_000_000_000, "a")
	r.Card.Status = "s9"
	d := &rowsDelta{}
	m.p.changedRows = d.read
	d.set(r)
	must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background))
	if got := m.structureReads(); got != 2 {
		t.Fatalf("%d structure reads", got)
	}
	r.Card.Updated = r.Card.Updated.Add(time.Second)
	d.set(r)
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.structureReads(); got != 2 {
		t.Fatalf("an edit naming the unexplained status read the structure: %d reads", got)
	}
}

// Rows a reconcile reads that name a sprint the mirror lacks read the
// structure after them, as a delta's do.
func TestReconcileCascadesToTheStructure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10001")
		p.boardRows = boardOf(withSprint(row("10001", 1_799_000_000_000, "a"), 1_799_000_000_000, "3"))
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "3", Board: b, Name: "Sprint 3", State: tracker.SprintActive})
			return st, nil
		}
	})
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil))
	if m.structureReads() != 2 || !slices.ContainsFunc(v.Sprints, func(s SprintView) bool { return s.ID == "3" }) {
		t.Fatalf("%d structure reads, %d sprints", m.structureReads(), len(v.Sprints))
	}
}

// A structure due from an earlier tick is read once in a tick that runs
// the structure unit too: the unit's read answers for it.
func TestADueStructureIsReadOnceATick(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.structure = func(tracker.BoardID, time.Time) (tracker.Structure, error) {
			return tracker.Structure{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
		p.changedRows = pagedDelta(t, []tracker.Row{withSprint(row("10001", 1_799_000_000_000, "a"), 1_799_000_000_001, "3")})
	})
	if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); out.Failure == nil || m.sync(testBoard).StructureDueAt == 0 {
		t.Fatalf("the failed cascade: %+v, due at %d", out, m.sync(testBoard).StructureDueAt)
	}
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) { return webStructure(b), nil }
	})
	reads := m.structureReads()
	out := must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background))
	if out.Failure != nil || m.structureReads() != reads+1 {
		t.Fatalf("tick %+v, %d structure reads", out, m.structureReads()-reads)
	}
}
