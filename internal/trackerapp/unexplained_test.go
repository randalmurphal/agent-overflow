package trackerapp

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
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
// the structure once; a sprint created after that read reads it again.
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
	if set, _ := m.svc.unexplainedOf(m.scope()); !slices.Equal(sortedKeys(set.sprints), []string{"7"}) {
		t.Fatalf("unexplained sprints %v", sortedKeys(set.sprints))
	}
}

// A tick reads the structure at most once: a row its delta stores naming
// what the tick's structure read did not list joins the set unread.
func TestATickReadsTheStructureOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	r := row("10001", 1_799_000_000_000, "a")
	r.Card.Status = "s9"
	m.p.changedRows = pagedDelta(t, []tracker.Row{r})
	must[Outcome](t)(m.svc.SyncBoard(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background))
	if got := m.structureReads(); got != 1 {
		t.Fatalf("%d structure reads", got)
	}
	if set, _ := m.svc.unexplainedOf(m.scope()); !set.statuses["s9"] {
		t.Fatalf("unexplained %+v", set)
	}
}

// A structure tick rebuilds the set, and turning the connection off
// drops it.
func TestTheUnexplainedSetIsRebuiltAndDropped(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	d := &rowsDelta{}
	m.p.set(func(p *scripted) { p.changedRows = d.read })
	d.set(withSprint(row("10001", 1_799_000_000_000, "a"), 1_799_000_000_001, "7"))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if set, _ := m.svc.unexplainedOf(m.scope()); !set.sprints["7"] {
		t.Fatalf("unexplained %+v", set)
	}
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := webStructure(b)
			st.Sprints = append(st.Sprints, tracker.Sprint{ID: "7", Board: b, Name: "Sprint 7", State: tracker.SprintClosed})
			return st, nil
		}
	})
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	if set, _ := m.svc.unexplainedOf(m.scope()); !set.empty() {
		t.Fatalf("the structure tick kept %+v", set)
	}
	m.svc.setUnexplained(m.scope(), &refs{statuses: map[string]bool{"s9": true}, users: map[string]bool{}, sprints: map[string]bool{}})
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	m.svc.mu.Lock()
	defer m.svc.mu.Unlock()
	if len(m.svc.unexplained) != 0 {
		t.Fatalf("turned off, the service keeps %v", m.svc.unexplained)
	}
}

// Over the bound the set stops reading the structure for sprints, and
// still reads it for a user it does not hold.
func TestTheUnexplainedSetIsBounded(t *testing.T) {
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
	set, full := m.svc.unexplainedOf(m.scope())
	if got := m.structureReads(); got != 2 || !full || set.size() != maxUnexplained {
		t.Fatalf("%d structure reads, full %v, %d kept", got, full, set.size())
	}
	d := &rowsDelta{}
	m.p.set(func(p *scripted) { p.changedRows = d.read })
	d.set(withSprint(row("10001", 1_799_000_000_000, "a"), 1_799_000_000_001, "y"))
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.structureReads(); got != 2 {
		t.Fatalf("a full set read the structure for a sprint: %d reads", got)
	}
	u := row("10001", 1_799_000_000_002, "a")
	u.Card.Assignee = "acc-user9"
	d.set(u)
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := m.structureReads(); got != 3 {
		t.Fatalf("a full set did not read the structure for a user: %d reads", got)
	}
}

// The service keeps the sets of at most maxUnexplainedScopes scopes, the
// least recently written leaving first.
func TestUnexplainedScopesAreBounded(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	one := func() *refs {
		n := newRefs()
		n.sprints["7"] = true
		return n
	}
	for i := range maxUnexplainedScopes + 1 {
		m.svc.setUnexplained(Scope{Connection: "c", Board: tracker.BoardID(fmt.Sprint(i))}, one())
	}
	m.svc.mu.Lock()
	defer m.svc.mu.Unlock()
	if len(m.svc.unexplained) != maxUnexplainedScopes || m.svc.unexplained[Scope{Connection: "c", Board: "0"}] != nil {
		t.Fatalf("%d scopes kept, the first kept: %v", len(m.svc.unexplained), m.svc.unexplained[Scope{Connection: "c", Board: "0"}] != nil)
	}
}
