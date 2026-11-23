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

// Lookup matches keys in one batch, case ignored, and reads each key the
// batch misses alone, which finds an identifier an issue had before
// it moved team; a trashed issue, an identifier Linear does not know and
// a key that is no identifier are unmatched.
func TestLookupMatches(t *testing.T) {
	t.Parallel()
	moved := issue(9, 1)
	trashed := issue(3, 1)
	trashed.Trashed = true
	p, srv := newProvider(t,
		issuesX(lookupVars(5, keyClause("WEB", 2, 1, 3, 7), keyClause("OPS", 4)), issue(1, 1), issue(2, 1), trashed),
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-3"}`, Body: `{"data":{"issue":` + trashed.JSON() + `}}`},
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"OPS-4"}`, Body: notFound},
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
	srv.Expect(issuesX(lookupVars(1, keyClause("WEB", 404))), lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-404"}`, Body: notFound})
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

// A batch read that fails fails the lookup, and so does a read of one
// key that fails other than by naming nothing.
func TestLookupFailure(t *testing.T) {
	t.Parallel()
	boom := `{"errors":[{"message":"boom"}]}`
	p, srv := newProvider(t, lineartest.Exchange{Op: "Issues", Vars: lookupVars(1, keyClause("WEB", 1)), Status: 500, Body: boom})
	if l, err := p.Lookup(context.Background(), []string{"WEB-1"}); err == nil || l.Found != nil {
		t.Errorf("lookup %+v %v", l, err)
	}
	srv.Expect(issuesX(lookupVars(1, keyClause("WEB", 1))), lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-1"}`, Status: 500, Body: boom})
	if l, err := p.Lookup(context.Background(), []string{"WEB-1"}); err == nil || l.Found != nil {
		t.Errorf("lookup of one key %+v %v", l, err)
	}
}
