package jira

import (
	"context"
	"encoding/json"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
)

// The allocation bounds of the client's hot reads: each is the count
// testing.AllocsPerRun measured through the exported call on a client
// New built, with the headroom stated beside it, so a change that makes
// the read allocate more fails here.

// catalogClient is a signed-in client whose site has a 1,500-field
// catalog, read once so its cache holds it.
func catalogClient(t testing.TB) *Client {
	t.Helper()
	body, err := json.Marshal(costCatalog(1500))
	if err != nil {
		t.Fatal(err)
	}
	f := jiratest.NewServer(t)
	f.On("GET", "/rest/api/3/field", jiratest.Reply(200, string(body)))
	c := signedIn(t, f)
	if _, err := c.Fields(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCatalogLookupAllocs bounds a cached Fields call on a 1,500-field
// catalog and a lookup by ID in what it returns, which every provider
// read that needs the sprint field makes. Measured: 0 allocations;
// bound 0.
func TestCatalogLookupAllocs(t *testing.T) {
	plainBuild(t)
	c := catalogClient(t)
	ctx := context.Background()
	n := testing.AllocsPerRun(100, func() {
		cat, err := c.Fields(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cat.Field("customfield_11499"); !ok {
			t.Fatal("lookup")
		}
	})
	if n > 0 {
		t.Errorf("%.1f allocations a cached Fields call and lookup, at most 0", n)
	}
}

// TestIssueDecodeAllocs bounds what a Search page costs per issue: the
// allocations of a 100-issue row page less those of a 1-issue one, over
// the 99 issues between, so the request's own cost (the transport's and
// the test server's, the same for both) drops out; what grows with the
// answer's size stays in, on both sides of the connection, since
// AllocsPerRun counts every goroutine. Measured: 51.1 allocations an
// issue; bound 54 (6% headroom).
func TestIssueDecodeAllocs(t *testing.T) {
	plainBuild(t)
	per := func(n int) float64 {
		f := jiratest.NewServer(t)
		f.On("POST", "/rest/api/3/search/jql", jiratest.Reply(200, string(costRowPage(n))))
		c := signedIn(t, f)
		ctx := context.Background()
		search := func() {
			page, err := c.Search(ctx, SearchQuery{JQL: "project = WEB"})
			if err != nil || len(page.Issues) != n {
				t.Fatalf("%d issues, %v; want %d", len(page.Issues), err, n)
			}
		}
		search()
		return testing.AllocsPerRun(20, search)
	}
	if got := (per(100) - per(1)) / 99; got > 54 {
		t.Errorf("%.1f allocations an issue, at most 54", got)
	}
}

// plainBuild skips an allocation bound under the race detector, whose
// instrumentation changes what escapes; the bounds hold for a plain
// build, which make go-test without -race runs.
func plainBuild(t *testing.T) {
	t.Helper()
	if raceOn {
		t.Skip("allocation counts differ under the race detector")
	}
}
