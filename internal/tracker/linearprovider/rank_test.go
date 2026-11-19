package linearprovider

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
)

func neighbor(t *testing.T, n int, v float64) *tracker.Neighbor {
	t.Helper()
	k, err := tracker.FloatOrderKey(v)
	if err != nil {
		t.Fatal(err)
	}
	return &tracker.Neighbor{Issue: tracker.IssueID(issueID(n)), Order: k}
}

// orderX expects the update of issue n to sortOrder v.
func orderX(n int, v float64, answer lineartest.Exchange) lineartest.Exchange {
	answer.Op, answer.Vars = "UpdateIssue", js(map[string]any{"id": issueID(n), "input": map[string]any{"sortOrder": v}})
	if answer.Body == "" && !answer.Reset {
		answer.Body = applied(issue(n, v).JSON())
	}
	return answer
}

// spread is linear.Spread for a test's expectations.
func spread(t *testing.T, lo, hi *float64, n int) []float64 {
	t.Helper()
	vs, err := linear.Spread(lo, hi, n)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// manualX is one page of the team's manual order.
func manualX(after string, more bool, cursor string, issues ...lineartest.Issue) lineartest.Exchange {
	v := map[string]any{"first": rankPage, "sort": []any{map[string]any{"manual": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}}}}}
	if after != "" {
		v["after"] = after
	}
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, is.JSON())
	}
	return lineartest.Exchange{Op: "Issues", Vars: js(v), Body: lineartest.Data(lineartest.PageJSON(nodes, more, cursor))}
}

// Rank sends the values the client spreads between the neighbors' keys,
// one update per issue in order, each naming the issue's row; a key the
// provider did not make and an ID that is not an issue ID are refused
// before any request.
func TestRank(t *testing.T) {
	t.Parallel()
	vs := spread(t, f64(1), f64(3), 2)
	p, _ := newProvider(t, orderX(1, vs[0], lineartest.Exchange{}), orderX(2, vs[1], lineartest.Exchange{}))
	ctx := context.Background()
	ids := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))}
	r := p.Rank(ctx, tracker.RankInput{Board: web, Issues: ids, Prev: neighbor(t, 8, 1), Next: neighbor(t, 9, 3)})
	expectResult(t, r, "1:rank:applied 2:rank:applied")
	if !reflect.DeepEqual(r.Parts[1].Changed, []tracker.Resource{tracker.RowOf(web, ids[1])}) {
		t.Errorf("changed %+v", r.Parts[1].Changed)
	}
	jira := &tracker.Neighbor{Issue: tracker.IssueID(issueID(8)), Order: "0|hzzzzz:"}
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: ids[:1], Prev: jira}), "1:rank:refused(invalid)")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{"WEB-1"}, Prev: neighbor(t, 8, 1)}), "WEB-1:rank:refused(invalid)")
	// Before a first issue at the lowest value there is no room, and no
	// run to renumber.
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: ids[:1], Next: neighbor(t, 9, -math.MaxFloat64)}), "1:rank:refused(invalid)")
}

// With no room between the neighbors, Rank reads the manual order to its
// end or the first issue with room, and renumbers the moving issues and
// every crowded issue after Prev, each a part naming its row; it stops
// at the first update not applied.
func TestRankNoRoom(t *testing.T) {
	t.Parallel()
	tight := math.Nextafter(1, 2)
	prev, crowded, roomy, moving := issue(8, 1), issue(9, tight), issue(7, 3), issue(1, 10)
	in := tracker.RankInput{Board: web, Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}, Prev: neighbor(t, 8, 1), Next: neighbor(t, 9, tight)}
	vs := spread(t, f64(1), f64(3), 2)
	atEnd := spread(t, f64(1), nil, 2)
	p, _ := newProvider(t,
		// Renumbered up to the first issue with room, across pages.
		manualX("", true, "r1", prev, crowded), manualX("r1", false, "", roomy, moving),
		orderX(1, vs[0], lineartest.Exchange{}), orderX(9, vs[1], lineartest.Exchange{}),
		// The second update's answer is lost.
		manualX("", false, "", prev, crowded, roomy, moving),
		orderX(1, vs[0], lineartest.Exchange{}), orderX(9, vs[1], lineartest.Exchange{Status: 500, Body: `{"errors":[{"message":"boom"}]}`}),
		// The first is refused; the second is not sent.
		manualX("", false, "", prev, crowded, roomy, moving),
		orderX(1, vs[0], lineartest.Exchange{Status: 400, Body: inputBody}),
		// The crowded run reaches the end of the list.
		manualX("", false, "", moving, prev, crowded),
		orderX(1, atEnd[0], lineartest.Exchange{}), orderX(9, atEnd[1], lineartest.Exchange{}),
		// Prev left the board.
		manualX("", false, "", crowded, roomy),
	)
	ctx := context.Background()
	r := p.Rank(ctx, in)
	expectResult(t, r, "1:rank:applied 9:rank:applied")
	if !reflect.DeepEqual(r.Parts[1].Changed, []tracker.Resource{tracker.RowOf(web, tracker.IssueID(issueID(9)))}) {
		t.Errorf("the renumbered neighbor's part names %+v", r.Parts[1].Changed)
	}
	expectResult(t, p.Rank(ctx, in), "1:rank:applied 9:rank:unknown")
	expectResult(t, p.Rank(ctx, in), "1:rank:refused(tracker) 9:rank:not sent")
	expectResult(t, p.Rank(ctx, in), "1:rank:applied 9:rank:applied")
	expectResult(t, p.Rank(ctx, in), "1:rank:refused(stale)")
}

// A crowded run that reaches past the read's bound is refused, and
// nothing is sent.
func TestRankNoRoomPastBound(t *testing.T) {
	t.Parallel()
	v := 1.0
	var script []lineartest.Exchange
	after := ""
	for page := range rankPages {
		var issues []lineartest.Issue
		if page == 0 {
			issues = append(issues, issue(8, v))
		}
		v = math.Nextafter(v, 2)
		issues = append(issues, issue(100+page, v))
		cursor := fmt.Sprintf("r%d", page)
		script = append(script, manualX(after, true, cursor, issues...))
		after = cursor
	}
	p, _ := newProvider(t, script...)
	in := tracker.RankInput{Board: web, Issues: []tracker.IssueID{tracker.IssueID(issueID(1))},
		Prev: neighbor(t, 8, 1), Next: neighbor(t, 9, math.Nextafter(1, 2))}
	expectResult(t, p.Rank(context.Background(), in), "1:rank:refused(invalid)")
}
