package linear

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// A page size is sent as first; zero is the default and anything outside
// 1 to 250 is refused before sending.
func TestListIssuesLimit(t *testing.T) {
	t.Parallel()
	empty := lineartest.Data(lineartest.PageJSON(nil, false, ""))
	f := newFake(t,
		lineartest.Exchange{Op: "Issues", Vars: `{"first":250,"sort":[{"manual":{"order":"Ascending"}}]}`, Body: empty},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":1,"sort":[{"manual":{"order":"Ascending"}}]}`, Body: empty},
	)
	c := f.client(t)
	for _, limit := range []int{250, 1} {
		if _, err := c.ListIssues(context.Background(), IssueQuery{Limit: limit}, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{-1, 251} {
		if _, err := c.ListIssues(context.Background(), IssueQuery{Limit: limit}, ""); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}

// IssueSummary reads the issue fields alone, by ID or identifier.
func TestIssueSummary(t *testing.T) {
	t.Parallel()
	missing := `{"data":null,"errors":[{"message":"Entity not found: Issue","path":["issue"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	f := newFake(t,
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-1"}`, Body: `{"data":{"issue":` + lineartest.IssueJSON(uuid1, "WEB-1", "2026-11-02T10:00:00.000Z", 2) + `}}`},
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-404"}`, Body: missing},
	)
	c := f.client(t)
	got, err := c.IssueSummary(context.Background(), "WEB-1")
	if err != nil || got.ID != uuid1 || got.SortOrder != 2 || got.TeamID != "team-web" {
		t.Fatalf("summary %+v %v", got, err)
	}
	if _, err := c.IssueSummary(context.Background(), "WEB-404"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("missing issue: %v", err)
	}
	if _, err := c.IssueSummary(context.Background(), ""); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("empty ID: %v", err)
	}
}

// SearchIssues sends the term and team as variables, never as document
// text, pages with the cursor, and decodes the search result type as an
// issue.
func TestSearchIssues(t *testing.T) {
	t.Parallel()
	hit := lineartest.IssueJSON(uuid1, "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	f := newFake(t,
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login \"page\" OR *","teamId":"team-web","first":50}`,
			Body: lineartest.Data(lineartest.PageJSON([]string{hit}, true, "s1"))},
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login","first":100,"after":"s1","includeArchived":true}`,
			Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
	)
	c := f.client(t)
	p, err := c.SearchIssues(context.Background(), SearchQuery{Term: `login "page" OR *`, TeamID: "team-web", Limit: 50}, "")
	if err != nil || len(p.Items) != 1 || p.Items[0].Identifier != "WEB-1" || !p.HasNextPage || p.EndCursor != "s1" {
		t.Fatalf("page %+v %v", p, err)
	}
	if _, err := c.SearchIssues(context.Background(), SearchQuery{Term: "login", IncludeArchived: true}, "s1"); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]SearchQuery{"no term": {}, "blank term": {Term: " \t"}, "limit": {Term: "x", Limit: 300}} {
		if _, err := c.SearchIssues(context.Background(), q, ""); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
