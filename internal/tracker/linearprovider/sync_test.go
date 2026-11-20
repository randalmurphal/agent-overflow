package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// pollVars are the variables of a poll page of team web from floor,
// leaving out atFloor while they are still updated at it.
func pollVars(floor time.Time, atFloor ...string) string {
	return lineartest.PollVars(web, false, 250, floor, atFloor)
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// A delta page's rows carry the card with its labels named from the
// issue's nested list, and a label that list leaves out named by a read
// of exactly its ID; the order key of sortOrder and the estimate of a
// team whose scale the issue carries; its cursor continues the walk from
// the newest update, leaving out the issue delivered at it, and a later
// page ends it. No team or catalog is read.
func TestChangedRows(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	first := issue(1, -2.5)
	first.LabelIDs, first.Estimate, first.Cycle, first.Priority = []string{"label-bug", "label-gone"}, f64(3), "cycle-1", 1
	first.CompletedAt = "2026-11-02T11:00:00.000Z"
	second := issue(2, 7)
	second.UpdatedAt = "2026-11-02T11:00:00.000Z"
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Issues", Vars: pollVars(start), Body: lineartest.Data(lineartest.PageJSON([]string{first.JSON()}, true, "c1"))},
		labelsByIDX([]string{"label-gone"}),
		lineartest.Exchange{Op: "Issues", Vars: pollVars(mustTime(at), issueID(1)), Body: page(second.JSON())},
	)
	ctx := context.Background()
	pg, err := p.ChangedRows(ctx, tracker.DeltaQuery{Board: web, Start: start})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Rows) != 1 || !pg.More {
		t.Fatalf("page %+v", pg)
	}
	r := pg.Rows[0]
	key, _ := tracker.FloatOrderKey(-2.5)
	c := r.Card
	if r.Order != key || r.Estimate == nil || *r.Estimate != 3 || c.ID != tracker.IssueID(issueID(1)) || c.Key != "WEB-1" ||
		c.Project != web || c.Status != "state-todo" || c.Assignee != "user-1" || c.Priority != (tracker.Choice{ID: "1", Name: "Urgent"}) ||
		len(c.Labels) != 2 || c.Labels[0] != (tracker.Choice{ID: "label-bug", Name: "Bug"}) || c.Labels[1] != (tracker.Choice{ID: "label-gone"}) ||
		len(c.Sprints) != 1 || c.Sprints[0] != "cycle-1" || c.Resolved.IsZero() || c.Created.IsZero() {
		t.Errorf("row %+v", r)
	}
	pg, err = p.ChangedRows(ctx, tracker.DeltaQuery{Board: web, From: pg.Next, Start: start.Add(time.Hour)})
	if err != nil || len(pg.Rows) != 1 || pg.More || pg.Rows[0].Estimate != nil || pg.Rows[0].Card.Priority != (tracker.Choice{ID: "2", Name: "High"}) {
		t.Fatalf("second page %+v %v", pg, err)
	}
	// The walk read its first page long after the newest update (the
	// server's Date is this computer's clock), so the next walk resumes
	// exactly after it.
	var cur cursorJSON
	if err := json.Unmarshal(pg.Next, &cur); err != nil || !cur.Floor.Equal(mustTime(second.UpdatedAt)) ||
		!slices.Equal(cur.AtFloor, []string{issueID(2)}) || cur.Walk != nil || cur.Team != web {
		t.Errorf("next cursor %s %v", pg.Next, err)
	}
}

// A team without estimates gives rows no estimate even when an issue
// keeps one.
func TestChangedRowsNoEstimateUnit(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	is := issue(1, 1)
	is.Estimate, is.TeamScale = f64(5), "notUsed"
	p, _ := newProvider(t, lineartest.Exchange{Op: "Issues", Vars: pollVars(start), Body: page(is.JSON())})
	pg, err := p.ChangedRows(context.Background(), tracker.DeltaQuery{Board: web, Start: start})
	if err != nil || len(pg.Rows) != 1 || pg.Rows[0].Estimate != nil {
		t.Fatalf("page %+v %v", pg, err)
	}
}

// pollStore is a store of team web whose walks end as nextWalk ends
// them, its Date header at *clock (none when clock is nil).
func pollStore(start time.Time, clock *time.Time) *lineartest.PollStore {
	s := lineartest.NewPollStore(web, false, 250, start)
	if clock != nil {
		s.Clock = func() time.Time { return *clock }
	}
	s.EndWalk = func(floor time.Time, atFloor []string, walkStart time.Time, delivered bool) (time.Time, []string) {
		if !delivered {
			return floor, atFloor
		}
		restart, exact := tracker.DeltaResume(floor, walkStart)
		if exact {
			return floor, atFloor
		}
		return restart, nil
	}
	return s
}

// syncer runs walks of ChangedRows against srv, each page through a new
// client and Provider as a process restarted after every stored page
// would, with only the cursor bytes carried over.
type syncer struct {
	t      *testing.T
	srv    *lineartest.Server
	start  time.Time
	cursor tracker.Cursor
}

// walk is one walk's rows by issue, and the requests it made.
func (s *syncer) walk() (map[tracker.IssueID]int, int) {
	s.t.Helper()
	seen := map[tracker.IssueID]int{}
	for n := 1; n <= 10; n++ {
		pg, err := providerOf(s.t, s.srv).ChangedRows(context.Background(), tracker.DeltaQuery{Board: web, From: s.cursor, Start: s.start})
		if err != nil {
			s.t.Fatal(err)
		}
		for _, r := range pg.Rows {
			seen[r.Card.ID]++
		}
		s.cursor = pg.Next
		if !pg.More {
			return seen, n
		}
	}
	s.t.Fatal("the walk did not end")
	return nil, 0
}

func (s *syncer) next() cursorJSON {
	s.t.Helper()
	var cur cursorJSON
	if err := json.Unmarshal(s.cursor, &cur); err != nil {
		s.t.Fatal(err)
	}
	return cur
}

// More issues sharing one update time than a page holds, polled across
// restarts, are each delivered once; the walk after delivers none, in
// one request.
func TestChangedRowsCohortAcrossRestarts(t *testing.T) {
	t.Parallel()
	cohort := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := cohort.Add(time.Hour)
	store := pollStore(cohort.Add(-time.Hour), &clock)
	for i := range 600 {
		store.Updated[issueID(i+1)] = cohort
	}
	s := &syncer{t: t, srv: lineartest.NewServer(t, store.Script(4)...), start: cohort.Add(-time.Hour)}
	seen, requests := s.walk()
	if len(seen) != 600 || requests != 3 {
		t.Fatalf("%d issues delivered in %d requests", len(seen), requests)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("issue %s delivered %d times in one walk", id, n)
		}
	}
	if cur := s.next(); !cur.Floor.Equal(cohort) || len(cur.AtFloor) != 600 || cur.Walk != nil {
		t.Fatalf("cursor after the walk: floor %v, %d left out, walk %+v", cur.Floor, len(cur.AtFloor), cur.Walk)
	}
	if seen, requests = s.walk(); len(seen) != 0 || requests != 1 {
		t.Errorf("the next walk delivered %d in %d requests", len(seen), requests)
	}
}

// Three idle ticks after a walk whose newest update is older than the
// overlap deliver nothing, at one request each.
func TestChangedRowsIdleTicks(t *testing.T) {
	t.Parallel()
	updated := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := updated.Add(time.Hour)
	store := pollStore(updated, &clock)
	store.Updated[issueID(1)] = updated
	s := &syncer{t: t, srv: lineartest.NewServer(t, store.Script(4)...), start: updated}
	if seen, _ := s.walk(); len(seen) != 1 {
		t.Fatalf("the first walk delivered %d", len(seen))
	}
	for tick := range 3 {
		clock = clock.Add(time.Minute)
		if seen, requests := s.walk(); len(seen) != 0 || requests != 1 {
			t.Errorf("tick %d delivered %d in %d requests", tick, len(seen), requests)
		}
	}
}

// A walk whose newest update is inside the overlap of Linear's clock
// reads again from the overlap's start, and once the clock is past it
// resumes exactly; with no Date the walk after one that delivered steps
// back the overlap from the newest update.
func TestChangedRowsOverlap(t *testing.T) {
	t.Parallel()
	updated := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := updated.Add(10 * time.Second)
	store := pollStore(updated, &clock)
	store.Updated[issueID(1)] = updated
	s := &syncer{t: t, srv: lineartest.NewServer(t, store.Script(3)...), start: updated}
	s.walk()
	if cur := s.next(); !cur.Floor.Equal(clock.Truncate(time.Second).Add(-tracker.DeltaOverlap)) || cur.AtFloor != nil {
		t.Fatalf("cursor inside the overlap: %+v", cur)
	}
	clock = clock.Add(2 * time.Minute)
	if seen, _ := s.walk(); len(seen) != 1 {
		t.Errorf("the walk from the overlap delivered %d", len(seen))
	}
	if seen, _ := s.walk(); len(seen) != 0 {
		t.Errorf("the walk past the overlap delivered %d", len(seen))
	}

	noDate := pollStore(updated, nil)
	noDate.Updated[issueID(1)] = updated
	s = &syncer{t: t, srv: lineartest.NewServer(t, noDate.Script(2)...), start: updated}
	s.walk()
	if cur := s.next(); !cur.Floor.Equal(updated.Add(-tracker.DeltaOverlap)) || cur.AtFloor != nil {
		t.Fatalf("cursor without a Date: %+v", cur)
	}
	if seen, _ := s.walk(); len(seen) != 1 {
		t.Errorf("the walk after one without a Date delivered %d", len(seen))
	}
}

// An update Linear shows late, inside the overlap, is delivered by the
// next walk; the walk after, past the overlap, delivers nothing.
func TestChangedRowsLateUpdate(t *testing.T) {
	s := newScenario(t)
	ws := scenarioWorkspace()
	late := &ws.Issues[0]
	late.UpdatedAt, late.ShownAt = scStart.Add(30*time.Second), scStart.Add(90*time.Second)
	ws.Issues[1].UpdatedAt = scStart.Add(45 * time.Second)
	if err := s.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
		t.Fatal(err)
	}
	s.clock.advance(time.Minute)
	rows, next := s.drain(t, nil, scStart.Add(-30*24*time.Hour))
	if slices.Contains(rowIDs(rows), tracker.IssueID(scIssue1)) || len(rows) != 2 {
		t.Fatalf("the first walk delivered %v", rowIDs(rows))
	}
	s.clock.advance(time.Minute)
	rows, next = s.drain(t, next, time.Time{})
	if !slices.Contains(rowIDs(rows), tracker.IssueID(scIssue1)) {
		t.Errorf("the late update was not delivered: %v", rowIDs(rows))
	}
	s.clock.advance(time.Minute)
	if rows, _ = s.drain(t, next, time.Time{}); len(rows) != 0 {
		t.Errorf("the walk past the overlap delivered %v", rowIDs(rows))
	}
}

// A cursor for another team, bytes that are not a cursor, a cursor of
// another version or with unknown members are refused before any
// request.
func TestChangedRowsRefusesForeignCursor(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	pc := linear.PollCursor{Floor: t0, AtFloor: []string{issueID(1)}, Walk: &linear.PollWalk{Start: t0, Delivered: true}}
	good, err := encodeCursor(web, pc)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := decodeCursor(web, good); err != nil || !reflect.DeepEqual(back, pc) {
		t.Fatalf("round trip %+v %v", back, err)
	}
	other, _ := encodeCursor(app, pc)
	for name, c := range map[string]tracker.Cursor{
		"other team":     other,
		"not JSON":       tracker.Cursor("not a cursor"),
		"empty":          tracker.Cursor{},
		"version 1":      tracker.Cursor(strings.Replace(string(good), `"v":2`, `"v":1`, 1)),
		"version 3":      tracker.Cursor(strings.Replace(string(good), `"v":2`, `"v":3`, 1)),
		"no version":     tracker.Cursor(strings.Replace(string(good), `"v":2,`, ``, 1)),
		"unknown member": tracker.Cursor(strings.Replace(string(good), `"v":2`, `"v":2,"x":1`, 1)),
		"trailing data":  append(append(tracker.Cursor{}, good...), []byte(`{}`)...),
		"version 1 form": tracker.Cursor(`{"v":1,"team":"` + web + `","since":"2026-11-01T00:00:00Z","after":"c1","floor":"2026-11-01T00:00:00Z"}`),
	} {
		_, err := p.ChangedRows(context.Background(), tracker.DeltaQuery{Board: web, From: c})
		if !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := p.ChangedRows(context.Background(), tracker.DeltaQuery{}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("no board: %v", err)
	}
}

// positionsX is one page of a listing's walk of issues, by ID and order.
// positionJSON is is as a positions read answers it.
func positionJSON(is lineartest.Issue) string {
	return js(map[string]any{"id": is.ID, "sortOrder": is.SortOrder, "team": map[string]string{"id": is.Team}})
}

func positionsX(vars string, more bool, cursor string, issues ...lineartest.Issue) lineartest.Exchange {
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, positionJSON(is))
	}
	return lineartest.Exchange{Op: "IssuePositions", Vars: vars, Body: lineartest.Data(lineartest.PageJSON(nodes, more, cursor))}
}

// listingVars are the variables of BoardIssues's walk of team web from
// window, in the cycles named, after cursor.
func listingVars(window, cursor string, cycles ...string) string {
	or := []any{
		map[string]any{"and": []any{map[string]any{"state": map[string]any{"type": map[string]any{"in": []string{"triage", "backlog", "unstarted", "started"}}}}}},
		map[string]any{"and": []any{map[string]any{"updatedAt": map[string]string{"gte": window}}}},
	}
	if len(cycles) > 0 {
		or = append(or, map[string]any{"and": []any{map[string]any{"cycle": map[string]any{"id": map[string]any{"in": cycles}}}}})
	}
	v := map[string]any{"first": 250, "sort": []any{map[string]any{"createdAt": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{
			map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}},
			map[string]any{"or": or},
		}}}
	if cursor != "" {
		v["after"] = cursor
	}
	return js(v)
}

const listingWindow = "2026-10-01T00:00:00.000Z"

// BoardIssues lists open issues, those updated in the window and those
// in the cycles Linear says the window keeps in one walk, oldest created
// first, each once, reading IDs alone; an archived or trashed issue is
// not listed, so a reconcile removes its row, nor one the answer names as
// another team's.
func TestBoardIssues(t *testing.T) {
	t.Parallel()
	window := mustTime(listingWindow)
	elsewhere := issue(4, 4)
	elsewhere.Team = app
	p, _ := newProvider(t,
		keptCyclesX(web, listingWindow,
			lineartest.Cycle{ID: "c-now", Number: 2, Team: web, StartsAt: "2026-11-16T00:00:00.000Z", EndsAt: "2026-11-30T00:00:00.000Z", Active: true}),
		positionsX(listingVars(listingWindow, "", "c-now"), true, "o1", issue(1, 1), issue(2, 2)),
		positionsX(listingVars(listingWindow, "o1", "c-now"), false, "", issue(2, 2), issue(3, 3), elsewhere),
	)
	l, err := p.BoardIssues(context.Background(), web, window)
	if err != nil {
		t.Fatal(err)
	}
	want := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2)), tracker.IssueID(issueID(3))}
	if !l.Complete || fmt.Sprint(l.IDs) != fmt.Sprint(want) {
		t.Errorf("listing %+v", l)
	}
}

// A walk that ends on an empty page after a page that promised more is
// complete with what the pages held; a walk whose later page fails fails
// the listing rather than report it complete.
func TestBoardIssuesEndOnEmptyPage(t *testing.T) {
	t.Parallel()
	window := mustTime(listingWindow)
	p, _ := newProvider(t,
		keptCyclesX(web, listingWindow),
		positionsX(listingVars(listingWindow, ""), true, "o1", issue(1, 1)),
		positionsX(listingVars(listingWindow, "o1"), false, ""),
		keptCyclesX(web, listingWindow),
		positionsX(listingVars(listingWindow, ""), true, "o1", issue(1, 1)),
		lineartest.Exchange{Op: "IssuePositions", Vars: listingVars(listingWindow, "o1"), Status: 400,
			Body: `{"errors":[{"message":"Invalid cursor","extensions":{"code":"INPUT_ERROR","userError":true}}]}`},
	)
	l, err := p.BoardIssues(context.Background(), web, window)
	if err != nil || !l.Complete || fmt.Sprint(l.IDs) != fmt.Sprint([]tracker.IssueID{tracker.IssueID(issueID(1))}) {
		t.Errorf("listing %+v %v", l, err)
	}
	if l, err := p.BoardIssues(context.Background(), web, window); err == nil {
		t.Errorf("a refused cursor: listing %+v", l)
	}
}

// A walk longer than its bound makes the listing incomplete, so the
// caller removes nothing.
func TestBoardIssuesIncompletePastBound(t *testing.T) {
	t.Parallel()
	window := mustTime(listingWindow)
	script := []lineartest.Exchange{keptCyclesX(web, listingWindow)}
	for i := range listingPages {
		cursor := ""
		if i > 0 {
			cursor = fmt.Sprintf("o%d", i)
		}
		script = append(script, positionsX(listingVars(listingWindow, cursor), true, fmt.Sprintf("o%d", i+1), issue(i+1, float64(i))))
	}
	p, _ := newProvider(t, script...)
	l, err := p.BoardIssues(context.Background(), web, window)
	if err != nil || l.Complete || len(l.IDs) != listingPages {
		t.Fatalf("listing complete %t with %d IDs, %v", l.Complete, len(l.IDs), err)
	}
}

// BoardRows reads by ID in requests of 250 within the team; an issue of
// another team, an archived or deleted one and an ID that is not an
// issue ID are missing, each once.
func TestBoardRows(t *testing.T) {
	t.Parallel()
	var asked []tracker.IssueID
	var first []string
	for i := 1; i <= 251; i++ {
		asked = append(asked, tracker.IssueID(issueID(i)))
		if i <= 250 {
			first = append(first, issueID(i))
		}
	}
	asked = append(asked, "WEB-1", tracker.IssueID(issueID(1)))
	var found []lineartest.Issue
	for i := 1; i <= 249; i++ {
		found = append(found, issue(i, float64(i)))
	}
	// An answer naming an issue outside the team is not a row of it.
	moved := issue(251, 1)
	moved.Team = app
	p, _ := newProvider(t,
		issuesX(byIDVars(first, []string{web}, false), found...),
		issuesX(byIDVars([]string{issueID(251)}, []string{web}, false), moved),
	)
	rows, err := p.BoardRows(context.Background(), web, asked)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 249 || fmt.Sprint(rows.Missing) != fmt.Sprint([]tracker.IssueID{tracker.IssueID(issueID(250)), tracker.IssueID(issueID(251)), "WEB-1"}) {
		t.Errorf("%d rows, missing %v", len(rows.Rows), rows.Missing)
	}
}

// Cards read by ID with archived issues; a trashed issue is missing.
func TestCards(t *testing.T) {
	t.Parallel()
	archived, trashed := issue(1, 1), issue(2, 2)
	archived.ArchivedAt = at
	trashed.ArchivedAt, trashed.Trashed = at, true
	p, _ := newProvider(t, issuesX(byIDVars([]string{issueID(1), issueID(2), issueID(3)}, nil, true), archived, trashed))
	cards, err := p.Cards(context.Background(), []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2)), tracker.IssueID(issueID(3))})
	if err != nil || len(cards.Cards) != 1 || cards.Cards[0].ID != tracker.IssueID(issueID(1)) ||
		fmt.Sprint(cards.Missing) != fmt.Sprint([]tracker.IssueID{tracker.IssueID(issueID(2)), tracker.IssueID(issueID(3))}) {
		t.Errorf("cards %+v %v", cards, err)
	}
}
