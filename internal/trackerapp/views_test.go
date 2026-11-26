package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// seedBoard stores the structure and rows of the test board: 10001 in
// sprint 1, 10002 in the backlog, 10003 done, and 10004 the parent of
// 10001.
func (m *mirrorFixture) seedBoard(t *testing.T) {
	t.Helper()
	epic := row("10004", 1_799_000_000_000, "d")
	epic.Card.Title, epic.Card.Type = "The epic", tracker.IssueType{ID: "t1", Name: "Epic", Level: 1}
	five := 5.0
	inSprint := tracker.Row{Card: seededCard(), Order: "a", Estimate: &five}
	done := row("10003", 1_799_000_000_000, "c")
	done.Card.Status = "s3"
	m.p.changedRows = pagedDelta(t, []tracker.Row{inSprint, row("10002", 1_799_000_000_000, "b"), done, epic})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
}

// seededCard is issue 10001's card as seedBoard's board read finds it:
// in sprint 1, a child of 10004, of medium priority, labeled frontend.
func seededCard() tracker.Card {
	c := card("10001", 1_799_000_000_000)
	c.Sprints, c.Parent = []tracker.SprintID{"1"}, "10004"
	c.Priority = tracker.Choice{ID: "3", Name: "Medium"}
	c.Labels = []tracker.Choice{{ID: "frontend", Name: "frontend"}}
	return c
}

func TestBoardView(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	if _, err := m.svc.Board(m.scope(), StatusGroups{}, nil); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("board of a connection with no mirror: %v", err)
	}
	m.seedBoard(t)
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionBacklog}, {Kind: SectionColumn, Column: 2}}))
	if v.Name != "WEB board 1" || !v.PlansSprints || v.Estimate.Unit != "points" || v.All.Rows != 4 || v.Backlog.Rows != 2 || v.Sync.ReadAt == 0 {
		t.Fatalf("board %+v", v)
	}
	// The totals sum the estimates the store holds, 10001's 5, in sprint 1
	// and the first column, and count the rows with none.
	if v.All != (SectionTotal{Rows: 4, Estimate: 5, Unestimated: 3}) || v.Backlog != (SectionTotal{Rows: 2, Unestimated: 2}) ||
		v.Sprints[0].Total != (SectionTotal{Rows: 1, Estimate: 5}) ||
		v.Columns[0].Total != (SectionTotal{Rows: 3, Estimate: 5, Unestimated: 2}) || v.Columns[2].Total != (SectionTotal{Rows: 1, Unestimated: 1}) ||
		v.Pace != nil {
		t.Fatalf("totals: all %+v, backlog %+v, sprints %+v, columns %+v, pace %+v", v.All, v.Backlog, v.Sprints, v.Columns, v.Pace)
	}
	if len(v.Columns) != 3 || v.Columns[0].Total.Rows != 3 || v.Columns[2].Total.Rows != 1 || v.Columns[1].Statuses[0] != (StatusView{ID: "s2", Name: "In Progress", Group: "in_progress"}) {
		t.Fatalf("columns %+v", v.Columns)
	}
	if len(v.Sprints) != 2 || v.Sprints[0].State != "active" || v.Sprints[0].Total.Rows != 1 || v.Sprints[1].Total.Rows != 0 {
		t.Fatalf("sprints %+v", v.Sprints)
	}
	sprint := v.Sections[0]
	if len(sprint.Rows) != 1 || sprint.Next != nil {
		t.Fatalf("sprint section %+v", sprint)
	}
	c := sprint.Rows[0].Card
	if c.Title != "Issue 10001" || c.Status.Name != "To Do" || c.Assignee == nil || c.Assignee.Name != "User 1" ||
		c.Parent == nil || *c.Parent != (ParentView{ID: "10004", Key: "WEB-4", Title: "The epic"}) ||
		c.Priority == nil || c.Priority.Name != "Medium" || len(c.Labels) != 1 || *sprint.Rows[0].Estimate != 5 {
		t.Fatalf("sprint card %+v", sprint.Rows[0])
	}
	if got := rowIssues(v.Sections[1].Rows); !slices.Equal(got, []string{"10002", "10004"}) {
		t.Fatalf("backlog %v", got)
	}
	if got := rowIssues(v.Sections[2].Rows); !slices.Equal(got, []string{"10003"}) {
		t.Fatalf("done column %v", got)
	}
	if _, err := m.svc.Board(Scope{Connection: m.conn.ID, Board: "9"}, StatusGroups{}, nil); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("a board not in the mirror: %v", err)
	}
	for _, bad := range []Section{{Kind: "sprint"}, {Kind: "backlog", Sprint: "1"}, {Kind: "column", Column: -1}, {Kind: "other"}} {
		if _, err := m.svc.Board(m.scope(), StatusGroups{}, []Section{bad}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("section %+v: %v", bad, err)
		}
	}
	if _, err := m.svc.Board(m.scope(), StatusGroups{}, slices.Repeat([]Section{{Kind: SectionBacklog}}, maxViewSections+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("too many sections: %v", err)
	}
}

// A board's pace averages what its last finished sprints finished: the
// tickets done when each was completed, and their estimates.
func TestBoardPace(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	completed := time.UnixMilli(1_799_000_000_000).UTC()
	m.p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
		st := webStructure(b)
		st.Sprints = append(st.Sprints,
			tracker.Sprint{ID: "8", Board: b, Name: "Sprint 0", State: tracker.SprintClosed, Completed: completed.Add(-time.Hour)},
			tracker.Sprint{ID: "9", Board: b, Name: "Sprint 1", State: tracker.SprintClosed, Completed: completed})
		return st, nil
	}
	three, one := 3.0, 1.0
	member := func(id, status string, estimate *float64, sprint tracker.SprintID) tracker.Row {
		r := row(id, 1_799_000_000_000, id)
		r.Card.Status, r.Card.Sprints, r.Estimate = tracker.StatusID(status), []tracker.SprintID{sprint}, estimate
		return r
	}
	m.p.changedRows = pagedDelta(t, []tracker.Row{
		member("10001", "s3", &three, "9"), member("10002", "s1", &one, "9"), member("10003", "s3", nil, "9"),
		member("10004", "s3", &one, "8"),
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil))
	if v.Pace == nil || *v.Pace != (PaceView{Sprints: 2, Rows: 1.5, Estimate: 2}) {
		t.Fatalf("pace %+v; want 2 sprints averaging 1.5 tickets and 2 points", v.Pace)
	}
}

// No sections asks for the Board screen's, chosen from the structure the
// view reads: a sprint board's active sprints, a board of no sprints its
// columns, with pages of the first maxViewSections of them and the rest
// listed for the client to read by page. The Backlog screen's are listed
// beside them: a sprint board's active sprints, its future ones and the
// backlog, a board of no sprints the backlog alone.
func TestBoardScreen(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil))
	if !slices.Equal(v.Screen, []Section{{Kind: SectionSprint, Sprint: "1"}}) || len(v.Sections) != 1 ||
		v.Sections[0].Section != v.Screen[0] || !slices.Equal(rowIssues(v.Sections[0].Rows), []string{"10001"}) {
		t.Fatalf("sprint board: screen %+v, sections %+v", v.Screen, v.Sections)
	}
	if want := []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionSprint, Sprint: "2"}, {Kind: SectionBacklog}}; !slices.Equal(v.Plan, want) {
		t.Fatalf("sprint board plan %+v; want %+v", v.Plan, want)
	}
	// Asked sections are read as asked, and the screen is listed beside them.
	v = must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionBacklog}}))
	if len(v.Sections) != 1 || v.Sections[0].Section.Kind != SectionBacklog || len(v.Screen) != 1 {
		t.Fatalf("asked: screen %+v, sections %+v", v.Screen, v.Sections)
	}

	const columns = maxViewSections + 5
	m.p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
		st := webStructure(b)
		// A Linear team with cycles off still lists the cycles it had.
		st.Board.Sprints = false
		for i := len(st.Columns); i < columns; i++ {
			st.Columns = append(st.Columns, tracker.Column{Name: fmt.Sprintf("Column %d", i)})
		}
		return st, nil
	}
	if out := must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true}, Background)); out.Failure != nil || out.Ended {
		t.Fatalf("tick %+v", out)
	}
	v = must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil))
	if len(v.Screen) != columns || len(v.Sections) != maxViewSections {
		t.Fatalf("kanban board: %d screen sections, %d pages", len(v.Screen), len(v.Sections))
	}
	if !slices.Equal(v.Plan, []Section{{Kind: SectionBacklog}}) {
		t.Fatalf("kanban board plan %+v; want the backlog", v.Plan)
	}
	for i, sec := range v.Screen {
		if sec != (Section{Kind: SectionColumn, Column: i}) || (i < maxViewSections && v.Sections[i].Section != sec) {
			t.Fatalf("screen section %d: %+v, page %+v", i, sec, v.Sections[min(i, maxViewSections-1)].Section)
		}
	}
	if got := rowIssues(v.Sections[0].Rows); !slices.Equal(got, []string{"10001", "10002", "10004"}) {
		t.Fatalf("first column %v", got)
	}
	// A patch reads every section the screen holds, up to every section a
	// board can have.
	patch := must[BoardPatch](t)(m.svc.BoardPatch(m.scope(), StatusGroups{}, v.Screen, ids("10001"), TotalsAt{}))
	if len(patch.Sections) != columns || !slices.Equal(rowIssues(patch.Sections[0].Rows), []string{"10001"}) {
		t.Fatalf("patch of the screen: %+v", patch.Sections)
	}
	if _, err := m.svc.BoardPatch(m.scope(), StatusGroups{}, slices.Repeat([]Section{{Kind: SectionBacklog}}, store.MirrorMaxViewSections+1), nil, TotalsAt{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a patch past every section a board can have: %v", err)
	}
}

// A board of no one project (the tracker names none) syncs and shows
// with no project.
func TestBoardOfNoProject(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) {
		st := webStructure(b)
		st.Board.Project = ""
		return st, nil
	}
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	if out := must[Outcome](t)(m.svc.syncTick(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background)); out.Failure != nil || out.Ended {
		t.Fatalf("tick %+v", out)
	}
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionColumn}}))
	if v.Project != "" || v.All.Rows != 1 || len(v.Sections[0].Rows) != 1 {
		t.Fatalf("board %+v", v)
	}
}

func rowIssues(rows []RowView) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Card.ID)
	}
	return out
}

func TestBoardViewShowsTheScopesFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{}, &trackerapi.StatusError{Tracker: "jira", Status: 500}
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, []Section{{Kind: SectionBacklog}}))
	if v.Sync.Failure == nil || v.Sync.Failure.Kind != string(FailureTracker) || len(v.Sections[0].Rows) != 2 {
		t.Fatalf("a failing board %+v", v)
	}
}

func TestSectionPages(t *testing.T) {
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
	backlog := Section{Kind: SectionBacklog}
	first := must[SectionPage](t)(m.svc.SectionRows(m.scope(), StatusGroups{}, backlog, nil))
	if len(first.Rows) != 100 || first.Next == nil || first.Next.Issue != "40099" {
		t.Fatalf("first page: %d rows, next %+v", len(first.Rows), first.Next)
	}
	second := must[SectionPage](t)(m.svc.SectionRows(m.scope(), StatusGroups{}, backlog, first.Next))
	if len(second.Rows) != 50 || second.Next != nil || second.Rows[0].Card.ID != "40100" {
		t.Fatalf("second page: %d rows, next %+v", len(second.Rows), second.Next)
	}
	if _, err := m.svc.SectionRows(m.scope(), StatusGroups{}, backlog, &SectionCursor{Order: "x"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a cursor with no issue: %v", err)
	}
	// A row that moves after the first page was read makes its cursor
	// stale: the next page could repeat or skip rows.
	moved := slices.Clone(rows[:1])
	moved[0] = row(string(moved[0].Card.ID), 1_799_000_000_001, "zz")
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: moved, Next: cursorAfter("y")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if _, err := m.svc.SectionRows(m.scope(), StatusGroups{}, backlog, first.Next); !errors.Is(err, ErrStalePage) || !errors.Is(err, store.ErrTrackerCacheStale) {
		t.Fatalf("a page after a stale cursor: %v", err)
	}
	again := must[SectionPage](t)(m.svc.SectionRows(m.scope(), StatusGroups{}, backlog, nil))
	if again.Next == nil || again.Next.Revision == first.Next.Revision {
		t.Fatalf("the page read again: next %+v, first %+v", again.Next, first.Next)
	}
}

// A card shows when its current status began once its history is read;
// before, it shows none.
func TestCardShowsWhenItsStatusBegan(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	sprint := []Section{{Kind: SectionSprint, Sprint: "1"}}
	if c := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, sprint)).Sections[0].Rows[0].Card; c.StatusSince != 0 {
		t.Fatalf("a card with no history shows its status since %d", c.StatusSince)
	}
	began := time.UnixMilli(1_798_500_000_000).UTC()
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		var h tracker.History
		for _, id := range q.Issues {
			ih := tracker.IssueHistory{Issue: id, CompleteTo: m.now()}
			if id == "10001" {
				ih.Status = []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}
			}
			h.Issues = append(h.Issues, ih)
		}
		return h, nil
	}
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	want := began.UnixMilli()
	if c := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, sprint)).Sections[0].Rows[0].Card; c.StatusSince != want {
		t.Fatalf("board card status since %d, want %d", c.StatusSince, want)
	}
	if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); v.Card.StatusSince != want {
		t.Fatalf("issue card status since %d", v.Card.StatusSince)
	}
	c := seededCard()
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		return enteredAtStatusBegan(tracker.Detail{Card: c}), nil
	}
	for _, path := range []string{"read through", "checked", "the mirror's copy"} {
		var v DetailView
		var err error
		switch path {
		case "read through":
			v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		case "checked":
			m.p.updatedMovesWithDetail(cardsAnswering(&c))
			v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		default:
			v, _, err = m.svc.mirroredDetail(m.conn.ID, "10001")
		}
		if err != nil || v.Card.StatusSince != want || v.Card.Parent == nil {
			t.Fatalf("detail %s: status since %d, parent %+v, %v", path, v.Card.StatusSince, v.Card.Parent, err)
		}
	}
	if m.p.count("Details") != 1 || m.p.count("Cards") != 1 {
		t.Fatalf("%d detail reads, %d card reads", m.p.count("Details"), m.p.count("Cards"))
	}
}

// An edit that leaves an issue's status as it was leaves the time the
// status began on its board card, before any history is read for the
// edit; an edit that changes the status shows none until one is.
func TestAnEditThatKeepsTheStatusKeepsWhenItBegan(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	began := time.UnixMilli(1_798_500_000_000).UTC()
	m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
		var h tracker.History
		for _, id := range q.Issues {
			h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now(),
				Status: []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}})
		}
		return h, nil
	}
	must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
	sprint := []Section{{Kind: SectionSprint, Sprint: "1"}}
	shown := func(what string, edit func(*tracker.Card), updated, want int64) {
		t.Helper()
		edited := row("10001", updated, "a")
		edited.Card.Sprints = []tracker.SprintID{"1"}
		edit(&edited.Card)
		stored := must[store.MirrorRow](t)(mirrorRow(edited))
		must[store.MirrorChanges](t)(m.st.WriteMirrorRows(m.root(), store.MirrorRowPage{BoardID: string(testBoard), Lifetime: m.sync(testBoard).Lifetime,
			Rows: []store.MirrorRow{stored}, ReadAt: beginReadAt(t, m.st, m.root(), m.now().UnixMilli()+updated)}))
		c := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, sprint)).Sections[0].Rows[0].Card
		if c.Updated != updated || c.StatusSince != want {
			t.Fatalf("%s: card updated %d shows its status since %d; want %d", what, c.Updated, c.StatusSince, want)
		}
	}
	shown("a new title", func(c *tracker.Card) { c.Title = "Renamed" }, 1_799_000_100_000, began.UnixMilli())
	shown("another status", func(c *tracker.Card) { c.Status = "s2" }, 1_799_000_200_000, 0)
	shown("the first status again", func(c *tracker.Card) {}, 1_799_000_300_000, 0)
}

// A detail shows the issue record's card and status time, whichever
// version its read found. A read of a later version whose history is not
// final shows the time unknown whatever the status, as its latest change
// is older than the settle margin before the update (the Monday case),
// read through and as the mirror's copy. A read of an older version than
// the record's card is stored as the body, ordered by its version, and
// shows the record's card, its time and that the body is behind it.
func TestDetailShowsTheRecordsCardAndStatusTime(t *testing.T) {
	t.Parallel()
	began := time.UnixMilli(1_798_500_000_000).UTC()
	const record = 1_799_000_000_000
	for name, tc := range map[string]struct {
		updated      int64
		status       tracker.StatusID
		wantUpdated  int64
		wantSince    int64
		wantBehind   bool
		wantStatusOf tracker.StatusID
	}{
		"another status":                   {updated: 1_799_000_100_000, status: "s2", wantUpdated: 1_799_000_100_000, wantStatusOf: "s2"},
		"a later card of the same status":  {updated: 1_799_000_100_000, status: "s1", wantUpdated: 1_799_000_100_000, wantStatusOf: "s1"},
		"an older card of another status":  {updated: 1_798_900_000_000, status: "s2", wantUpdated: record, wantSince: began.UnixMilli(), wantBehind: true, wantStatusOf: "s1"},
		"an older card of the same status": {updated: 1_798_900_000_000, status: "s1", wantUpdated: record, wantSince: began.UnixMilli(), wantBehind: true, wantStatusOf: "s1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
				var h tracker.History
				for _, id := range q.Issues {
					ih := tracker.IssueHistory{Issue: id, CompleteTo: m.now()}
					if id == "10001" {
						ih.Status = []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}
					}
					h.Issues = append(h.Issues, ih)
				}
				return h, nil
			}
			must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
			if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); v.Card.StatusSince != began.UnixMilli() {
				t.Fatalf("the mirror's card shows its status since %d", v.Card.StatusSince)
			}
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
				c := card("10001", tc.updated)
				c.Status = tc.status
				return enteredAtStatusBegan(tracker.Detail{Card: c}), nil
			}
			answerCardsFromDetails(m.p)
			for _, mirrored := range []bool{false, true} {
				var v DetailView
				var err error
				if mirrored {
					v, _, err = m.svc.mirroredDetail(m.conn.ID, "10001")
				} else {
					v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
				}
				if err != nil || v.Card.StatusSince != tc.wantSince || v.Card.Updated != tc.wantUpdated ||
					v.Card.Status.ID != string(tc.wantStatusOf) || v.BodyBehind != tc.wantBehind {
					t.Fatalf("detail (mirror's copy %v): status %s since %d, updated %d, body behind %v, %v; want %s since %d, updated %d, behind %v",
						mirrored, v.Card.Status.ID, v.Card.StatusSince, v.Card.Updated, v.BodyBehind, err, tc.wantStatusOf, tc.wantSince, tc.wantUpdated, tc.wantBehind)
				}
			}
		})
	}
}

// A ticket read that sees the issue leave its status and then come back,
// with no card read between, shows no time for either: the mirror's time
// is the first stay's.
func TestATicketDoesNotShowAnEarlierStaysTime(t *testing.T) {
	t.Parallel()
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "refresh"}[refresh], func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			m.p.caps.DetailMovesUpdated = refresh
			answerCardsFromDetails(m.p)
			began := time.UnixMilli(1_798_500_000_000).UTC()
			m.p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
				var h tracker.History
				for _, id := range q.Issues {
					h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now(),
						Status: []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}})
				}
				return h, nil
			}
			must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
			detail := m.svc.IssueDetail
			if refresh {
				detail = func(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
					return m.svc.issueDetail(ctx, id, issue, u, true)
				}
			}
			for i, status := range []tracker.StatusID{"s2", "s1"} {
				m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
					c := card("10001", 1_799_000_100_000+int64(i)*100_000)
					c.Status = status
					return tracker.Detail{Card: c}, nil
				}
				v, _, err := detail(t.Context(), m.conn.ID, "10001", Interactive)
				if err != nil || v.Card.StatusSince != 0 {
					t.Fatalf("the ticket read in %s shows its status since %d, %v; want none", status, v.Card.StatusSince, err)
				}
			}
		})
	}
}

// An edit that keeps the issue's status makes a new version whose status
// time no read has decided: the open ticket's read of it, whose history is
// not final through the edit, cannot tell a stay the issue left and came
// back to since, and its latest change is older than the settle margin
// before the edit, so the ticket shows the time unknown (the Monday case),
// on Jira after the check stored the card and on Linear from the full read
// alone. The history read final through the edit decides it again.
func TestAnEditOfAnOpenTicketShowsItsStatusTimeOnceAFinalReadDecidesIt(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 1, jira)
			began := time.UnixMilli(1_798_500_000_000).UTC()
			m.p.set(func(p *scripted) {
				p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
					var h tracker.History
					for _, id := range q.Issues {
						h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now(),
							Status: []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}})
					}
					return h, nil
				}
			})
			must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
			quiet(t, m.svc)
			if since := m.ticketView(t, subs[0].ID).Detail.Card.StatusSince; since != began.UnixMilli() {
				t.Fatalf("the ticket shows its status since %d before the edit; want %d", since, began.UnixMilli())
			}
			st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
			if res := m.round(t); res.failed {
				t.Fatalf("round %+v", res)
			}
			if c := m.ticketView(t, subs[0].ID).Detail.Card; c.Title != "Renamed" || c.StatusSince != 0 {
				t.Fatalf("after the edit the ticket shows %q, in its status since %d; want unknown", c.Title, c.StatusSince)
			}
			if r := m.recordOf(t, string(issues[0])); r.StatusFinal {
				t.Fatal("the record's status time is final for the edit before any final read")
			}
			must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
			quiet(t, m.svc)
			if c := m.ticketView(t, subs[0].ID).Detail.Card; c.StatusSince != began.UnixMilli() {
				t.Fatalf("after the history read the ticket shows its status since %d; want %d", c.StatusSince, began.UnixMilli())
			}
		})
	}
}

// A card whose blob this version cannot read shows its columns, its
// title among them, and is listed as stale.
func TestAStaleCardShowsItsColumns(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	bad := must[store.MirrorRow](t)(mirrorRow(row("10002", 1_799_000_000_001, "b")))
	bad.Card.Data = []byte(`{"v":0,"title":"old"}`)
	must[store.MirrorChanges](t)(m.st.WriteMirrorRows(m.root(), store.MirrorRowPage{BoardID: string(testBoard), Lifetime: m.sync(testBoard).Lifetime, Rows: []store.MirrorRow{bad}, ReadAt: beginReadAt(t, m.st, m.root(), m.now().UnixMilli()+1)}))
	page := must[SectionPage](t)(m.svc.SectionRows(m.scope(), StatusGroups{}, Section{Kind: SectionBacklog}, nil))
	if !slices.Equal(page.Stale, []string{"10002"}) || page.Rows[0].Card.Key != "WEB-2" || page.Rows[0].Card.Title != "Issue 10002" || page.Rows[0].Card.Labels != nil {
		t.Fatalf("page %+v", page)
	}
	if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10002")); !v.Stale || v.Card.Status.Name != "To Do" {
		t.Fatalf("issue %+v", v)
	}
	// The next read of the row replaces it.
	m.p.set(func(p *scripted) { p.boardRows = boardOf(row("10002", 1_799_000_000_002, "b")) })
	r := m.begun(t, m.scope())
	if _, _, _, err := r.boardRows(testBoard, ids("10002"), tracker.Eligibility{}); err != nil {
		t.Fatal(err)
	}
	r.finish()
	if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10002")); v.Stale || v.Card.Title != "Issue 10002" {
		t.Fatalf("issue after a read %+v", v)
	}
}

func TestIssueView(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001"))
	if v.Card.Key != "WEB-1" || len(v.Boards) != 1 || v.Boards[0] != (IssueRowView{Board: "1", Order: "a", Estimate: v.Boards[0].Estimate}) || *v.Boards[0].Estimate != 5 {
		t.Fatalf("issue %+v", v)
	}
	if _, err := m.svc.Issue(m.conn.ID, "19999"); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("an issue not in the mirror: %v", err)
	}
	if _, err := m.svc.Issue(m.conn.ID, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no issue: %v", err)
	}
}
