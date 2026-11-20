package linearprovider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// lookupVars are the variables of a batch read of identifiers: clauses
// are the or-ed team key and number clauses.
func lookupVars(n int, clauses ...map[string]any) string {
	return js(map[string]any{"first": n, "includeArchived": true, "sort": []any{map[string]any{"manual": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{map[string]any{"or": clauses}}}})
}

func keyClause(key string, numbers ...int) map[string]any {
	return map[string]any{"number": map[string]any{"in": numbers}, "team": map[string]any{"key": map[string]any{"eqIgnoreCase": key}}}
}

// teamsByKeyX expects the read of the teams keys name.
func teamsByKeyX(keys []string, teams ...lineartest.Team) lineartest.Exchange {
	var or []any
	for _, k := range keys {
		or = append(or, map[string]any{"key": map[string]string{"eqIgnoreCase": k}})
	}
	var nodes []string
	for _, t := range teams {
		nodes = append(nodes, t.JSON())
	}
	return lineartest.Exchange{Op: "TeamsByKey", Vars: js(map[string]any{"first": len(keys), "filter": map[string]any{"or": or}}), Body: page(nodes...)}
}

// Lookup matches keys in one batch, case ignored; a key the batch misses
// is read alone only when its team key names a visible team, which one
// read of the teams by the keys left tells; a trashed
// issue, an identifier Linear does not know and a key that is no
// identifier are unmatched.
func TestLookupMatches(t *testing.T) {
	t.Parallel()
	moved := issue(9, 1)
	trashed := issue(3, 1)
	trashed.Trashed = true
	p, srv := newProvider(t,
		issuesX(lookupVars(5, keyClause("WEB", 2, 1, 3, 7), keyClause("OPS", 4)), issue(1, 1), issue(2, 1), trashed),
		teamsByKeyX([]string{"WEB", "OPS"}, lineartest.NewTeam("WEB")),
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-3"}`, Body: `{"data":{"issue":` + trashed.JSON() + `}}`},
		summaryX("WEB-7", moved),
	)
	l, err := p.Lookup(context.Background(), []string{"web-2", "WEB-1", "WEB-3", "OPS-4", "WEB-7", "WEB_8"})
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, m := range l.Found {
		found = append(found, m.Key+"="+m.Card.Key)
	}
	if strings.Join(found, " ") != "web-2=WEB-2 WEB-1=WEB-1 WEB-7=WEB-9" || strings.Join(l.Unmatched, " ") != "WEB-3 OPS-4 WEB_8" {
		t.Errorf("found %v unmatched %v", found, l.Unmatched)
	}
	// An identifier Linear does not know is unmatched, not an error.
	srv.Expect(
		issuesX(lookupVars(1, keyClause("WEB", 404))),
		teamsByKeyX([]string{"WEB"}, lineartest.NewTeam("WEB")),
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-404"}`, Body: notFound},
	)
	if l, err := p.Lookup(context.Background(), []string{"WEB-404"}); err != nil || l.Unmatched[0] != "WEB-404" {
		t.Errorf("unknown identifier %+v %v", l, err)
	}
	// Keys that are no identifier send nothing.
	if l, err := p.Lookup(context.Background(), []string{"WEB", "1"}); err != nil || len(l.Unmatched) != 2 {
		t.Errorf("non-identifiers %+v %v", l, err)
	}
	if _, err := p.Lookup(context.Background(), nil); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("no keys: %v", err)
	}
}

// A batch read that fails fails the lookup.
func TestLookupFailure(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t, lineartest.Exchange{Op: "Issues", Vars: lookupVars(1, keyClause("WEB", 1)), Status: 500, Body: `{"errors":[{"message":"boom"}]}`})
	if l, err := p.Lookup(context.Background(), []string{"WEB-1"}); err == nil || l.Found != nil {
		t.Errorf("lookup %+v %v", l, err)
	}
}

// Search sends the text as a term within the project, or with no text
// lists the project's issues newest first; a page with more names the
// next.
func TestSearch(t *testing.T) {
	t.Parallel()
	hit := `{"data":{"page":{"nodes":[` + issue(1, 1).JSON() + `],"pageInfo":{"hasNextPage":true,"endCursor":"s2"}}}}`
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login \"fails\" OR x","teamId":"` + web + `","first":50,"after":"s1"}`, Body: hit},
		lineartest.Exchange{Op: "SearchIssues", Vars: `{"term":"login","first":5}`, Body: page()},
		issuesX(`{"first":50,"sort":[{"updatedAt":{"order":"Descending"}}],"filter":{"and":[{"team":{"id":{"in":["`+web+`"]}}}]}}`, issue(2, 1)),
	)
	ctx := context.Background()
	pg, err := p.Search(ctx, tracker.SearchQuery{Text: ` login "fails" OR x `, Project: web, Page: "s1"})
	if err != nil || len(pg.Cards) != 1 || pg.Cards[0].Key != "WEB-1" || pg.Next != "s2" {
		t.Fatalf("search %+v %v", pg, err)
	}
	if pg, err := p.Search(ctx, tracker.SearchQuery{Text: "login", Limit: 5}); err != nil || pg.Cards == nil || len(pg.Cards) != 0 || pg.Next != "" {
		t.Errorf("empty search %+v %v", pg, err)
	}
	if pg, err := p.Search(ctx, tracker.SearchQuery{Project: web}); err != nil || pg.Cards[0].Key != "WEB-2" {
		t.Errorf("project issues %+v %v", pg, err)
	}
	if _, err := p.Search(ctx, tracker.SearchQuery{Text: "a\nb"}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("control text: %v", err)
	}
}
