package linearprovider

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// recentVars are the variables of a page of team web's issues most
// recently updated first, from ceiling (empty for the first page)
// leaving out at.
func recentVars(limit int, ceiling string, at ...string) string {
	and := []any{map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}}}
	if ceiling != "" {
		and = append(and, map[string]any{"updatedAt": map[string]string{"lte": ceiling}})
		if len(at) > 0 {
			and = append(and, map[string]any{"or": []any{map[string]any{"updatedAt": map[string]string{"lt": ceiling}}, map[string]any{"id": map[string]any{"nin": at}}}})
		}
	}
	return js(map[string]any{"first": limit, "sort": []any{map[string]any{"updatedAt": map[string]string{"order": "Descending"}}}, "filter": map[string]any{"and": and}})
}

// Search sends the text as a term within the project and continues by
// Linear's cursor carried in its own token; with no text it lists the
// project's issues newest first and continues from the update time of
// the last issue shown, leaving out those shown at it. A token of
// another project, of the other kind of search or not one this package
// wrote is refused.
func TestSearch(t *testing.T) {
	t.Parallel()
	hit := `{"data":{"page":{"nodes":[` + issue(1, 1).JSON() + `],"pageInfo":{"hasNextPage":true,"endCursor":"s2"}}}}`
	more := lineartest.Data(lineartest.PageJSON([]string{issue(2, 1).JSON()}, true, "ignored"))
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login \"fails\" OR x","teamId":"` + web + `","first":50}`, Body: hit},
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login \"fails\" OR x","teamId":"` + web + `","first":50,"after":"s2"}`, Body: page()},
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login","first":5}`, Body: page()},
		lineartest.Exchange{Op: "Issues", Vars: recentVars(50, ""), Body: more},
		issuesX(recentVars(50, at, issueID(2)), issue(3, 1)),
	)
	ctx := context.Background()
	q := tracker.SearchQuery{Text: ` login "fails" OR x `, Project: web}
	pg, err := p.Search(ctx, q)
	if err != nil || len(pg.Cards) != 1 || pg.Cards[0].Key != "WEB-1" || pg.Next == "" {
		t.Fatalf("search %+v %v", pg, err)
	}
	textToken := pg.Next
	q.Page = textToken
	if pg, err := p.Search(ctx, q); err != nil || len(pg.Cards) != 0 || pg.Next != "" {
		t.Errorf("next search page %+v %v", pg, err)
	}
	if pg, err := p.Search(ctx, tracker.SearchQuery{Text: "login", Limit: 5}); err != nil || pg.Cards == nil || len(pg.Cards) != 0 || pg.Next != "" {
		t.Errorf("empty search %+v %v", pg, err)
	}
	pg, err = p.Search(ctx, tracker.SearchQuery{Project: web})
	if err != nil || pg.Cards[0].Key != "WEB-2" || pg.Next == "" {
		t.Fatalf("project issues %+v %v", pg, err)
	}
	if pg, err := p.Search(ctx, tracker.SearchQuery{Project: web, Page: pg.Next}); err != nil || pg.Cards[0].Key != "WEB-3" || pg.Next != "" {
		t.Errorf("next project page %+v %v", pg, err)
	}
	for name, sq := range map[string]tracker.SearchQuery{
		"control text":       {Text: "a\nb"},
		"a text token":       {Project: web, Page: textToken},
		"another project":    {Text: "login", Project: app, Page: textToken},
		"not a token":        {Project: web, Page: "s2"},
		"another version":    {Project: web, Page: `{"v":9,"team":"` + web + `","ceiling":"2026-11-01T00:00:00Z"}`},
		"an unknown member":  {Project: web, Page: `{"v":1,"team":"` + web + `","ceiling":"2026-11-01T00:00:00Z","x":1}`},
		"a token with no at": {Text: "login", Project: web, Page: `{"v":1,"team":"` + web + `"}`},
	} {
		if _, err := p.Search(ctx, sq); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The listing of a project's issues newest first, against the fake,
// with the last issue of a page updated before the next page: that issue
// moves to the front and is not shown again, and the unchanged issue
// after it is shown, once. A walk by Linear's cursor would show the
// first page's other issue again (the cursor follows its issue).
func TestSearchListingUpdatedBetweenPages(t *testing.T) {
	s := newScenario(t)
	q := tracker.SearchQuery{Project: tracker.ProjectID(scTeam), Limit: 2}
	first := scenarioMust[tracker.SearchPage](t)(s.p.Search(s.ctx, q))
	if got := cardKeys(first.Cards); !slices.Equal(got, []string{"WEB-1", "WEB-2"}) || first.Next == "" {
		t.Fatalf("first page %v next %q", got, first.Next)
	}
	s.clock.advance(time.Minute)
	allApplied(t, "edit", s.p.Edit(s.ctx, tracker.EditInput{Issue: tracker.IssueID(scIssue2), Fields: []tracker.FieldEdit{
		{Field: tracker.FieldTitle, Value: tracker.TypedValues("Add site search")},
	}}))
	q.Page = first.Next
	rest := scenarioMust[tracker.SearchPage](t)(s.p.Search(s.ctx, q))
	if got := cardKeys(rest.Cards); !slices.Equal(got, []string{"WEB-3"}) || rest.Next != "" {
		t.Errorf("next page %v next %q", got, rest.Next)
	}
}

func cardKeys(cs []tracker.Card) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Key
	}
	return out
}
