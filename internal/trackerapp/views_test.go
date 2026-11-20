package trackerapp

import (
	"bytes"
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
	inSprint := row("10001", 1_799_000_000_000, "a")
	five := 5.0
	inSprint.Card.Sprints, inSprint.Card.Parent, inSprint.Estimate = []tracker.SprintID{"1"}, "10004", &five
	inSprint.Card.Priority = tracker.Choice{ID: "3", Name: "Medium"}
	inSprint.Card.Labels = []tracker.Choice{{ID: "frontend", Name: "frontend"}}
	done := row("10003", 1_799_000_000_000, "c")
	done.Card.Status = "s3"
	m.p.changedRows = pagedDelta(t, []tracker.Row{inSprint, row("10002", 1_799_000_000_000, "b"), done, epic})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
}

func TestBoardView(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	if _, err := m.svc.Board(m.scope(), nil); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("board of a connection with no mirror: %v", err)
	}
	m.seedBoard(t)
	v := must[BoardView](t)(m.svc.Board(m.scope(), []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionBacklog}, {Kind: SectionColumn, Column: 2}}))
	if v.Name != "WEB board 1" || !v.PlansSprints || v.Estimate.Unit != "points" || v.All.Rows != 4 || v.Backlog.Rows != 2 || v.Sync.ReadAt == 0 {
		t.Fatalf("board %+v", v)
	}
	// The totals sum the estimates the store holds: 10001's 5, in sprint 1
	// and the first column.
	if v.All.Estimate != 5 || v.Backlog.Estimate != 0 || v.Sprints[0].Total != (SectionTotal{Rows: 1, Estimate: 5}) ||
		v.Columns[0].Total != (SectionTotal{Rows: 3, Estimate: 5}) || v.Columns[2].Total != (SectionTotal{Rows: 1}) {
		t.Fatalf("totals: all %+v, backlog %+v, sprints %+v, columns %+v", v.All, v.Backlog, v.Sprints, v.Columns)
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
	if _, err := m.svc.Board(Scope{Connection: m.conn.ID, Board: "9"}, nil); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("a board not in the mirror: %v", err)
	}
	for _, bad := range []Section{{Kind: "sprint"}, {Kind: "backlog", Sprint: "1"}, {Kind: "column", Column: -1}, {Kind: "other"}} {
		if _, err := m.svc.Board(m.scope(), []Section{bad}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("section %+v: %v", bad, err)
		}
	}
	if _, err := m.svc.Board(m.scope(), slices.Repeat([]Section{{Kind: SectionBacklog}}, maxViewSections+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("too many sections: %v", err)
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
	if out := must[Outcome](t)(m.svc.SyncBoard(t.Context(), m.scope(), BoardDue{Structure: true, Delta: true}, Background)); out.Failure != nil || out.Ended {
		t.Fatalf("tick %+v", out)
	}
	v := must[BoardView](t)(m.svc.Board(m.scope(), []Section{{Kind: SectionColumn}}))
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
	v := must[BoardView](t)(m.svc.Board(m.scope(), []Section{{Kind: SectionBacklog}}))
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
	first := must[SectionPage](t)(m.svc.SectionRows(m.scope(), backlog, nil))
	if len(first.Rows) != 100 || first.Next == nil || first.Next.Issue != "40099" {
		t.Fatalf("first page: %d rows, next %+v", len(first.Rows), first.Next)
	}
	second := must[SectionPage](t)(m.svc.SectionRows(m.scope(), backlog, first.Next))
	if len(second.Rows) != 50 || second.Next != nil || second.Rows[0].Card.ID != "40100" {
		t.Fatalf("second page: %d rows, next %+v", len(second.Rows), second.Next)
	}
	if _, err := m.svc.SectionRows(m.scope(), backlog, &SectionCursor{Order: "x"}); !errors.Is(err, ErrInvalidRequest) {
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
	if _, err := m.svc.SectionRows(m.scope(), backlog, first.Next); !errors.Is(err, ErrStalePage) || !errors.Is(err, store.ErrTrackerCacheStale) {
		t.Fatalf("a page after a stale cursor: %v", err)
	}
	again := must[SectionPage](t)(m.svc.SectionRows(m.scope(), backlog, nil))
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
	if c := must[BoardView](t)(m.svc.Board(m.scope(), sprint)).Sections[0].Rows[0].Card; c.StatusSince != 0 {
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
	if c := must[BoardView](t)(m.svc.Board(m.scope(), sprint)).Sections[0].Rows[0].Card; c.StatusSince != want {
		t.Fatalf("board card status since %d, want %d", c.StatusSince, want)
	}
	if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); v.Card.StatusSince != want {
		t.Fatalf("issue card status since %d", v.Card.StatusSince)
	}
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		c := card("10001", 1_799_000_000_000)
		c.Parent = "10004"
		return tracker.Detail{Card: c}, nil
	}
	for _, path := range []string{"read through", "checked", "the mirror's copy"} {
		var v DetailView
		var err error
		switch path {
		case "read through":
			v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		case "checked":
			m.p.updatedMovesWithDetail(cardsOf)
			v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		default:
			v, _, err = m.svc.MirroredDetail(m.conn.ID, "10001")
		}
		if err != nil || v.Card.StatusSince != want || v.Card.Parent == nil {
			t.Fatalf("detail %s: status since %d, parent %+v, %v", path, v.Card.StatusSince, v.Card.Parent, err)
		}
	}
	if m.p.count("Details") != 1 || m.p.count("Cards") != 1 {
		t.Fatalf("%d detail reads, %d card reads", m.p.count("Details"), m.p.count("Cards"))
	}
}

// A detail shows when its status began only when the mirror's time
// describes that status and stay: a detail read before the delta and
// history caught up shows it unknown, read through and as the mirror's
// copy, and so does a detail read before a newer card was stored,
// whatever its status.
func TestDetailShowsStatusSinceOnlyOfItsOwnCard(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		updated int64
		status  tracker.StatusID
	}{
		"another status":                  {updated: 1_799_000_100_000, status: "s2"},
		"the same status entered again":   {updated: 1_799_000_100_000, status: "s1"},
		"an older card of another status": {updated: 1_798_900_000_000, status: "s2"},
		// The issue may have left the status and come back between the two
		// versions, so the mirror's time is a later stay's.
		"an older card of the same status": {updated: 1_798_900_000_000, status: "s1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
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
			if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); v.Card.StatusSince != began.UnixMilli() {
				t.Fatalf("the mirror's card shows its status since %d", v.Card.StatusSince)
			}
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
				c := card("10001", tc.updated)
				c.Status = tc.status
				return tracker.Detail{Card: c}, nil
			}
			for _, mirrored := range []bool{false, true} {
				var v DetailView
				var err error
				if mirrored {
					v, _, err = m.svc.MirroredDetail(m.conn.ID, "10001")
				} else {
					v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
				}
				if err != nil || v.Card.StatusSince != 0 || v.Card.Updated != tc.updated {
					t.Fatalf("detail (mirror's copy %v): status %s since %d, updated %d, %v", mirrored, v.Card.Status.ID, v.Card.StatusSince, v.Card.Updated, err)
				}
			}
		})
	}
}

// A card whose blob this version cannot read shows its columns and is
// listed as stale.
func TestAStaleCardShowsItsColumns(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	bad := must[store.MirrorRow](t)(mirrorRow(row("10002", 1_799_000_000_001, "b")))
	bad.Card.Data = []byte(`{"v":0,"title":"old"}`)
	must[store.MirrorChanges](t)(m.st.WriteMirrorRows(m.root(), store.MirrorRowPage{BoardID: string(testBoard), Lifetime: m.sync(testBoard).Lifetime, Rows: []store.MirrorRow{bad}, ReadAt: m.now().UnixMilli() + 1}))
	page := must[SectionPage](t)(m.svc.SectionRows(m.scope(), Section{Kind: SectionBacklog}, nil))
	if !slices.Equal(page.Stale, []string{"10002"}) || page.Rows[0].Card.Key != "WEB-2" || page.Rows[0].Card.Title != "" {
		t.Fatalf("page %+v", page)
	}
	if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10002")); !v.Stale || v.Card.Status.Name != "To Do" {
		t.Fatalf("issue %+v", v)
	}
	// The next read of the row replaces it.
	m.p.set(func(p *scripted) { p.boardRows = boardOf(row("10002", 1_799_000_000_002, "b")) })
	r := m.begun(t, m.scope())
	must[Changes](t)(r.boardRows(testBoard, ids("10002")))
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

func detailOfCard(c tracker.Card) tracker.Detail {
	return tracker.Detail{
		Card: c, URL: "https://example.atlassian.net/browse/" + c.Key, Description: "Body",
		Fields:   []tracker.Field{{ID: "customfield_10001", Name: "Team", Kind: tracker.KindOption, Editable: true, Value: tracker.OptionValue(tracker.Choice{ID: "o1", Name: "Web"})}},
		Comments: tracker.CommentPage{Comments: []tracker.Comment{{ID: "c1", Author: "acc-user2", Body: "Looks good", Created: c.Created}}, Next: "p2"},
		Links: []tracker.Link{{ID: "l1", Type: tracker.LinkType{ID: "10000", Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
			Issue: tracker.IssueRef{ID: "10002", Key: "WEB-2", Title: "Issue 10002", Status: "s1"}}},
		History: []tracker.Event{{At: c.Created, Author: "acc-user1", Field: "status", From: "To Do", To: "In Progress"}, {At: c.Created, Field: "resolution"}},
		Moves:   []tracker.Move{{ID: "11", Name: "Done", To: "s3"}},
		Catalog: tracker.Catalog{Users: []tracker.User{user2}},
	}
}

// openDetail opens issue's detail through IssueDetail and fails on an
// error.
func (m *mirrorFixture) openDetail(t *testing.T, issue tracker.IssueID) (DetailView, Changes) {
	t.Helper()
	v, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, issue, Interactive)
	if err != nil {
		t.Fatal(err)
	}
	return v, ch
}

// cardsAnswering answers a card read with *c for the issues asked that it
// is.
func cardsAnswering(c *tracker.Card) func([]tracker.IssueID) (tracker.Cards, error) {
	return func(asked []tracker.IssueID) (tracker.Cards, error) {
		var out tracker.Cards
		if slices.Contains(asked, c.ID) {
			out.Cards = append(out.Cards, *c)
		}
		return out, nil
	}
}

// On a tracker whose update time moves with its detail, a stored detail is
// checked with one card read: served when the card's update time is the
// stored detail's, read again when it moved, the mirror's card refreshed
// by the check either way.
func TestIssueDetail(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.p.updatedMovesWithDetail(cardsAnswering(&c))
	v, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ch.Details, ids("10001")) || v.Description != "Body" || v.MoreComments != "p2" ||
		v.Comments[0].Author.Name != "User 2" || v.Card.Assignee.Name != "User 1" ||
		v.Links[0].Relation != "is blocked by" || v.Links[0].Issue.Status.Name != "To Do" ||
		v.History[0].Author.Name != "User 1" || v.History[1].Author != nil || v.Moves[0].To.Group != "done" ||
		v.Fields[0].Value.Kind != "option" || v.Fields[0].Value.Choices[0].Name != "Web" {
		t.Fatalf("detail %+v, changes %+v", v, ch)
	}
	if m.p.count("Cards") != 0 {
		t.Fatal("a detail the mirror did not hold was checked with a card read")
	}
	// The card did not move: the stored detail, no detail read.
	v, ch, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if err != nil || m.p.count("Details") != 1 || m.p.count("Cards") != 1 || len(ch.Details) != 0 || v.Description != "Body" {
		t.Fatalf("an unchanged card: %d detail reads, %d card reads, changes %+v, %v", m.p.count("Details"), m.p.count("Cards"), ch, err)
	}
	// The card moved: the detail is read again, ten minutes later (within
	// detailMaxAge), which marks it opened then, and the mirror's card is
	// the one the check read.
	later := m.now().Add(10 * time.Minute)
	m.svc.now = func() time.Time { return later }
	c.Updated, c.Title = c.Updated.Add(500*time.Millisecond), "Renamed"
	v, ch, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if err != nil || m.p.count("Details") != 2 || m.p.count("Cards") != 2 || v.Card.Title != "Renamed" ||
		!slices.Equal(ch.Details, ids("10001")) || !slices.Equal(ch.Cards, ids("10001")) {
		t.Fatalf("a moved card: %d detail reads, %d card reads, %q, changes %+v, %v", m.p.count("Details"), m.p.count("Cards"), v.Card.Title, ch, err)
	}
	if held := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); held.Card.Updated != millis(c.Updated) || held.Card.Title != "Renamed" {
		t.Fatalf("the mirror's card %+v", held.Card)
	}
	if d, ok, err := m.st.GetMirrorDetail(m.root(), "10001"); err != nil || !ok || d.OpenedAt != later.UnixMilli() || d.ReadAt != later.UnixMilli() {
		t.Fatalf("the detail read again: opened %d, read %d, want %d: %v", d.OpenedAt, d.ReadAt, later.UnixMilli(), err)
	}
	// A card the tracker does not return is no check: the detail is read.
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) { return tracker.Cards{}, nil }
	})
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil || m.p.count("Details") != 3 {
		t.Fatalf("a card not returned: %d detail reads, %v", m.p.count("Details"), err)
	}
	// An issue the mirror does not keep is read every time, never stored and
	// never checked.
	other := card("10009", 1_799_000_000_000)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(other), nil }
	})
	for range 2 {
		if _, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10009", Interactive); err != nil || !ch.Empty() {
			t.Fatalf("an unkept issue: %+v, %v", ch, err)
		}
	}
	if m.p.count("Details") != 5 || m.p.count("Cards") != 3 {
		t.Fatalf("%d detail reads, %d card reads", m.p.count("Details"), m.p.count("Cards"))
	}
	if _, _, err := m.svc.MirroredDetail(m.conn.ID, "10009"); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("the mirror's copy of an unkept issue: %v", err)
	}
}

// On a tracker whose update time does not move with its detail (Linear: a
// comment leaves it), every open reads the detail: a comment added since
// the last open shows though the card did not move, and an identical
// document changes nothing.
func TestADetailIsReadWhereItsUpdateTimeDoesNotMove(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	d := detailOfCard(card("10001", 1_799_000_000_000))
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return d, nil }
	open := func() (DetailView, Changes) { t.Helper(); return m.openDetail(t, "10001") }
	if _, ch := open(); !slices.Equal(ch.Details, ids("10001")) {
		t.Fatalf("the first open changed %+v", ch)
	}
	if _, ch := open(); !ch.Empty() {
		t.Fatalf("an identical document changed %+v", ch)
	}
	d.Comments.Comments = append([]tracker.Comment{{ID: "c2", Author: "acc-user1", Body: "A new comment", Created: d.Card.Updated}}, d.Comments.Comments...)
	v, ch := open()
	if len(v.Comments) != 2 || v.Comments[0].Body != "A new comment" || !slices.Equal(ch.Details, ids("10001")) {
		t.Fatalf("after a comment: %d comments, changes %+v", len(v.Comments), ch)
	}
	if m.p.count("Details") != 3 || m.p.count("Cards") != 0 {
		t.Fatalf("%d detail reads, %d card reads", m.p.count("Details"), m.p.count("Cards"))
	}
	if v, _, err := m.svc.MirroredDetail(m.conn.ID, "10001"); err != nil || len(v.Comments) != 2 {
		t.Fatalf("the mirror's copy: %d comments, %v", len(v.Comments), err)
	}
}

func TestIssueDetailFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 500}
	}
	_, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok || m.problem() != FailureTracker {
		t.Fatalf("a failed detail read: %v, problem %q", err, m.problem())
	}
	// An issue the tracker leaves out is that issue's failure, not the
	// connection's: the read succeeded.
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 404}
		}
	})
	_, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if !errors.Is(err, ErrIssueNotFound) || m.problem() != "" {
		t.Fatalf("an issue left out: %v, problem %q", err, m.problem())
	}
	// The tracker answering with another issue is refused, not stored.
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			return detailOfCard(card("10002", 1_799_000_000_000)), nil
		}
	})
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err == nil {
		t.Fatal("a detail of another issue was accepted")
	}
	if _, ok, _ := m.st.GetMirrorDetail(m.root(), "10002"); ok {
		t.Fatal("the other issue's detail was stored")
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a detail of a connection turned off: %v", err)
	}
}

// A check or a read that fails leaves the stored detail as it was and
// returns the tracker's failure, settled as the connection's problem; the
// mirror's copy still reads.
func TestAFailedCheckKeepsTheStoredDetail(t *testing.T) {
	t.Parallel()
	for name, moved := range map[string]bool{"the card read": false, "the detail read after a moved card": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			c := card("10001", 1_799_000_000_000)
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
			m.p.updatedMovesWithDetail(cardsAnswering(&c))
			m.openDetail(t, "10001")
			before, _, _ := m.storedDetail(t, "10001")
			unavailable := &trackerapi.StatusError{Tracker: "jira", Status: 503}
			m.p.set(func(p *scripted) {
				if moved {
					c.Updated = c.Updated.Add(time.Second)
					p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{}, unavailable }
					return
				}
				p.cards = func([]tracker.IssueID) (tracker.Cards, error) { return tracker.Cards{}, unavailable }
			})
			_, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
			if got, ok := errors.AsType[*trackerapi.StatusError](err); !ok || got.Status != 503 || m.problem() == "" {
				t.Fatalf("a failed check: %v, problem %q", err, m.problem())
			}
			after, _, ok := m.storedDetail(t, "10001")
			if !ok || after.ReadAt != before.ReadAt || !bytes.Equal(after.Data, before.Data) {
				t.Fatalf("the stored detail changed: read %d, was %d", after.ReadAt, before.ReadAt)
			}
			v, readAt, err := m.svc.MirroredDetail(m.conn.ID, "10001")
			if err != nil || readAt != before.ReadAt || v.Description != "Body" {
				t.Fatalf("the mirror's copy: read %d, %q, %v", readAt, v.Description, err)
			}
		})
	}
}

// A connection turned off while its tracker answers a check or a read
// ends the read: nothing is stored and the call fails wrapping
// ErrDisabled.
func TestATurnOffDuringADetailReadEndsIt(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"Cards", "Details"} {
		t.Run(call, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			c := card("10001", 1_799_000_000_000)
			off := func() {
				if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
					t.Error(err)
				}
			}
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
				if call == "Details" {
					off()
				}
				return detailOfCard(c), nil
			}
			if call == "Cards" {
				m.openDetail(t, "10001")
				m.p.updatedMovesWithDetail(func(asked []tracker.IssueID) (tracker.Cards, error) {
					off()
					return cardsAnswering(&c)(asked)
				})
			}
			reads := m.p.count("Details")
			if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); !errors.Is(err, ErrDisabled) {
				t.Fatalf("a read the turn off overtook: %v", err)
			}
			if call == "Cards" && m.p.count("Details") != reads {
				t.Fatal("the detail was read after the check's write was refused")
			}
			if m.root() != 0 {
				t.Fatal("the connection turned off kept a mirror")
			}
		})
	}
}
