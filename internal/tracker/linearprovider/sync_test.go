package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// pollVars are the variables of a poll page of team web.
func pollVars(since time.Time, after string) string {
	v := map[string]any{"first": 100, "sort": []any{map[string]any{"updatedAt": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{
			map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}},
			map[string]any{"updatedAt": map[string]string{"gte": lineartest.FormatTime(since)}},
		}}}
	if after != "" {
		v["after"] = after
	}
	return js(v)
}

// A delta page's rows carry the card with its labels named from the
// issue's nested list, and a label that list leaves out named by a read
// of exactly its ID; the order key of sortOrder and the estimate of a
// team whose scale the issue carries; its cursor continues the walk and
// a later page ends it. No team or catalog is read.
func TestChangedRows(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	first := issue(1, -2.5)
	first.LabelIDs, first.Estimate, first.Cycle, first.Priority = []string{"label-bug", "label-gone"}, f64(3), "cycle-1", 1
	first.CompletedAt = "2026-11-02T11:00:00.000Z"
	second := issue(2, 7)
	second.UpdatedAt = "2026-11-02T11:00:00.000Z"
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Issues", Vars: pollVars(start, ""), Body: lineartest.Data(lineartest.PageJSON([]string{first.JSON()}, true, "c1"))},
		labelsByIDX([]string{"label-gone"}),
		lineartest.Exchange{Op: "Issues", Vars: pollVars(start, "c1"), Body: page(second.JSON())},
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
	var cur cursorJSON
	if err := json.Unmarshal(pg.Next, &cur); err != nil || cur.Since.Format(time.RFC3339Nano) != "2026-11-02T11:00:00Z" || cur.After != "" || cur.Team != web {
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
	p, _ := newProvider(t, lineartest.Exchange{Op: "Issues", Vars: pollVars(start, ""), Body: page(is.JSON())})
	pg, err := p.ChangedRows(context.Background(), tracker.DeltaQuery{Board: web, Start: start})
	if err != nil || len(pg.Rows) != 1 || pg.Rows[0].Estimate != nil {
		t.Fatalf("page %+v %v", pg, err)
	}
}

// A walk of 101 issues sharing one update time, more than a page, run
// through a new client and Provider for every page as a process
// restarted after each stored page would, with only the cursor bytes
// carried over, delivers every issue without looping, and the next walk
// starts at that time.
func TestChangedRowsRestartInsideSameTimeRun(t *testing.T) {
	t.Parallel()
	cohort := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	start := cohort.Add(-time.Hour)
	store := lineartest.NewPollStore(web, false, 100, start)
	for i := range 101 {
		store.Updated[issueID(i+1)] = cohort
	}
	srv := lineartest.NewServer(t, store.Script(4)...)
	seen := map[tracker.IssueID]int{}
	var cursor tracker.Cursor
	walk := func() {
		for range 5 {
			pg, err := providerOf(t, srv).ChangedRows(context.Background(), tracker.DeltaQuery{Board: web, From: cursor, Start: start})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range pg.Rows {
				seen[r.Card.ID]++
			}
			cursor = pg.Next
			if !pg.More {
				return
			}
		}
		t.Fatal("the walk did not end")
	}
	walk()
	if len(seen) != 101 {
		t.Fatalf("%d issues delivered, want 101", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("issue %s delivered %d times in one walk", id, n)
		}
	}
	var cur cursorJSON
	if err := json.Unmarshal(cursor, &cur); err != nil || !cur.Since.Equal(cohort) || !cur.Floor.Equal(cohort) || cur.After != "" {
		t.Fatalf("cursor after the walk %s", cursor)
	}
	// The next walk starts at the run's time, inclusive, and ends.
	clear(seen)
	walk()
	if len(seen) != 101 {
		t.Errorf("the next walk delivered %d issues", len(seen))
	}
}

// A cursor for another team, bytes that are not a cursor, a cursor of
// another version or with unknown members are refused before any
// request.
func TestChangedRowsRefusesForeignCursor(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	pc := linear.PollCursor{Since: t0, After: "c1", Floor: t0}
	good, err := encodeCursor(web, pc)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := decodeCursor(web, good); err != nil || back != pc {
		t.Fatalf("round trip %+v %v", back, err)
	}
	other, _ := encodeCursor(app, pc)
	for name, c := range map[string]tracker.Cursor{
		"other team":     other,
		"not JSON":       tracker.Cursor("not a cursor"),
		"empty":          tracker.Cursor{},
		"version 2":      tracker.Cursor(strings.Replace(string(good), `"v":1`, `"v":2`, 1)),
		"no version":     tracker.Cursor(strings.Replace(string(good), `"v":1,`, ``, 1)),
		"unknown member": tracker.Cursor(strings.Replace(string(good), `"v":1`, `"v":1,"x":1`, 1)),
		"trailing data":  append(append(tracker.Cursor{}, good...), []byte(`{}`)...),
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
func positionsX(vars string, more bool, cursor string, issues ...lineartest.Issue) lineartest.Exchange {
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, js(map[string]any{"id": is.ID, "sortOrder": is.SortOrder}))
	}
	return lineartest.Exchange{Op: "IssuePositions", Vars: vars, Body: lineartest.Data(lineartest.PageJSON(nodes, more, cursor))}
}

// The variables of BoardIssues's walks of team web.
var (
	openVars = `{"first":250,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"team":{"id":{"in":["` + web + `"]}}},` +
		`{"state":{"type":{"in":["triage","backlog","unstarted","started"]}}}]}}`
	recentVars = `{"first":250,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"team":{"id":{"in":["` + web + `"]}}},` +
		`{"updatedAt":{"gte":"2026-10-01T00:00:00.000Z"}}]}}`
)

func afterVars(vars, cursor string) string {
	return strings.Replace(vars, `{"first"`, `{"after":"`+cursor+`","first"`, 1)
}

// BoardIssues lists open issues, those updated in the window and those
// in the cycles Linear says the window keeps, each once, reading IDs
// alone; an archived or trashed issue is not listed, so a reconcile
// removes its row.
func TestBoardIssues(t *testing.T) {
	t.Parallel()
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	inCycle := `{"first":250,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"team":{"id":{"in":["` + web + `"]}}},` +
		`{"cycle":{"id":{"in":["c-now"]}}}]}}`
	p, _ := newProvider(t,
		keptCyclesX(web, "2026-10-01T00:00:00.000Z",
			lineartest.Cycle{ID: "c-now", Number: 2, Team: web, StartsAt: "2026-11-16T00:00:00.000Z", EndsAt: "2026-11-30T00:00:00.000Z", Active: true}),
		positionsX(openVars, true, "o1", issue(1, 1)),
		positionsX(afterVars(openVars, "o1"), false, "", issue(2, 2)),
		positionsX(recentVars, false, "", issue(2, 2), issue(3, 3)),
		positionsX(inCycle, false, "", issue(4, 4), issue(1, 1)),
	)
	l, err := p.BoardIssues(context.Background(), web, window)
	if err != nil {
		t.Fatal(err)
	}
	want := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2)), tracker.IssueID(issueID(3)), tracker.IssueID(issueID(4))}
	if !l.Complete || fmt.Sprint(l.IDs) != fmt.Sprint(want) {
		t.Errorf("listing %+v", l)
	}
}

// A walk that ends on an empty page after a page that promised more is
// complete with what the pages held; a walk whose later page fails fails
// the listing rather than report it complete.
func TestBoardIssuesEndOnEmptyPage(t *testing.T) {
	t.Parallel()
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p, _ := newProvider(t,
		keptCyclesX(web, "2026-10-01T00:00:00.000Z"),
		positionsX(openVars, true, "o1", issue(1, 1)),
		positionsX(afterVars(openVars, "o1"), false, ""),
		positionsX(recentVars, false, ""),
		keptCyclesX(web, "2026-10-01T00:00:00.000Z"),
		positionsX(openVars, true, "o1", issue(1, 1)),
		lineartest.Exchange{Op: "IssuePositions", Vars: afterVars(openVars, "o1"), Status: 400,
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
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	script := []lineartest.Exchange{keptCyclesX(web, "2026-10-01T00:00:00.000Z"), positionsX(recentVars, false, "")}
	for i := range listingPages {
		vars := openVars
		if i > 0 {
			vars = afterVars(openVars, fmt.Sprintf("o%d", i))
		}
		script = append(script, positionsX(vars, true, fmt.Sprintf("o%d", i+1), issue(i+1, float64(i))))
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
