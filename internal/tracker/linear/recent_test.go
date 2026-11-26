package linear

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// recentVars are the variables of a newest-first page of team-web at
// ceiling (zero for the first page), leaving out at.
func recentVars(limit int, ceiling time.Time, at ...string) string {
	and := []any{map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{"team-web"}}}}}
	if !ceiling.IsZero() {
		c := lineartest.FormatTime(ceiling)
		and = append(and, map[string]any{"updatedAt": map[string]string{"lte": c}})
		if len(at) > 0 {
			and = append(and, map[string]any{"or": []any{
				map[string]any{"updatedAt": map[string]string{"lt": c}},
				map[string]any{"id": map[string]any{"nin": at}},
			}})
		}
	}
	b, err := json.Marshal(map[string]any{"first": limit, "sort": []any{map[string]any{"updatedAt": map[string]string{"order": "Descending"}}},
		"filter": map[string]any{"and": and}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func recentPage(more bool, issues ...lineartest.Issue) string {
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, is.JSON())
	}
	return lineartest.Data(lineartest.PageJSON(nodes, more, "never-used"))
}

// A newest-first walk asks each page from the ceiling, the update time
// of the last issue delivered, leaving out the issues delivered at
// exactly it, and never sends Linear's cursor; a cohort spanning pages
// grows across them and a newer time starts a new one.
func TestRecentIssuesPagesByPosition(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(m int) string { return lineartest.FormatTime(base.Add(time.Duration(m) * time.Minute)) }
	a := lineartest.NewIssue("issue-a", "WEB-1", at(3), 1)
	b := lineartest.NewIssue("issue-b", "WEB-2", at(2), 1)
	c := lineartest.NewIssue("issue-c", "WEB-3", at(2), 1)
	d := lineartest.NewIssue("issue-d", "WEB-4", at(1), 1)
	two := base.Add(2 * time.Minute)
	f := newFake(t,
		lineartest.Exchange{Op: "RecentIssues", Vars: recentVars(2, time.Time{}), Body: recentPage(true, a, b)},
		lineartest.Exchange{Op: "RecentIssues", Vars: recentVars(2, two, "issue-b"), Body: recentPage(true, c, d)},
		lineartest.Exchange{Op: "RecentIssues", Vars: recentVars(2, base.Add(time.Minute), "issue-d"), Body: recentPage(false)},
	)
	q := RecentQuery{Filter: IssueFilter{Teams: []string{"team-web"}}, Limit: 2}
	var got []string
	for range 3 {
		p, err := f.client(t).RecentIssues(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, identifiers(p.Items)...)
		q.From = p.Next
		if !p.HasNextPage {
			break
		}
	}
	if !slices.Equal(got, []string{"WEB-1", "WEB-2", "WEB-3", "WEB-4"}) || !q.From.Bound.Equal(base.Add(time.Minute)) || !slices.Equal(q.From.At, []string{"issue-d"}) {
		t.Errorf("walked %v, ended at %+v", got, q.From)
	}
}

// A page that breaks the newest-first order, one that lists an issue
// left out and one holding none that says more follow fail the call; a
// left-out ID that is not one is refused.
func TestRecentIssuesErrors(t *testing.T) {
	t.Parallel()
	ceiling := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	newer := lineartest.NewIssue("issue-b", "WEB-2", lineartest.FormatTime(ceiling.Add(time.Second)), 1)
	again := lineartest.NewIssue("issue-a", "WEB-1", lineartest.FormatTime(ceiling), 1)
	vars := recentVars(250, ceiling, "issue-a")
	f := newFake(t,
		lineartest.Exchange{Op: "RecentIssues", Vars: vars, Body: recentPage(false, newer)},
		lineartest.Exchange{Op: "RecentIssues", Vars: vars, Body: recentPage(false, again)},
		lineartest.Exchange{Op: "RecentIssues", Vars: vars, Body: recentPage(true)},
	)
	c := f.client(t)
	q := RecentQuery{Filter: IssueFilter{Teams: []string{"team-web"}}, From: Position{Bound: ceiling, At: []string{"issue-a"}}}
	for _, what := range []string{"newer", "left out", "empty with more"} {
		if _, err := c.RecentIssues(context.Background(), q); err == nil {
			t.Errorf("a page %s: no error", what)
		}
	}
	q.From.At = []string{""}
	if _, err := c.RecentIssues(context.Background(), q); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a bad left-out ID: %v", err)
	}
}
