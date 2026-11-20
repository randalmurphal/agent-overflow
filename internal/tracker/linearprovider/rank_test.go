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

// manualX is one page of the team's manual order, by ID and order alone.
func manualX(after string, more bool, cursor string, issues ...lineartest.Issue) lineartest.Exchange {
	v := map[string]any{"first": rankPage, "sort": []any{map[string]any{"manual": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}}}}}
	if after != "" {
		v["after"] = after
	}
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, js(map[string]any{"id": is.ID, "sortOrder": is.SortOrder}))
	}
	return lineartest.Exchange{Op: "IssuePositions", Vars: js(v), Body: lineartest.Data(lineartest.PageJSON(nodes, more, cursor))}
}

// ids are the invented IDs of issues ns.
func ids(ns ...int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = issueID(n)
	}
	return out
}

// Rank reads the issues and neighbors on the board, then sends the
// values the client spreads between the neighbors' keys, one update per
// issue in order, each naming the issue's row and card; a key the
// provider did not make and an ID that is not an issue ID are refused
// before any request.
func TestRank(t *testing.T) {
	t.Parallel()
	vs := spread(t, f64(1), f64(3), 2)
	p, _ := newProvider(t,
		onBoardX(ids(1, 2, 8, 9), issue(1, 0), issue(2, 0), issue(8, 1), issue(9, 3)),
		orderX(1, vs[0], lineartest.Exchange{}), orderX(2, vs[1], lineartest.Exchange{}),
		onBoardX(ids(1, 9), issue(1, 0), issue(9, -math.MaxFloat64)),
	)
	ctx := context.Background()
	in := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))}
	r := p.Rank(ctx, tracker.RankInput{Board: web, Issues: in, Prev: neighbor(t, 8, 1), Next: neighbor(t, 9, 3)})
	expectResult(t, r, "1:rank:applied 2:rank:applied")
	if !reflect.DeepEqual(r.Parts[1].Changed, []tracker.Resource{tracker.RowOf(web, in[1]), tracker.CardOf(in[1])}) {
		t.Errorf("changed %+v", r.Parts[1].Changed)
	}
	jira := &tracker.Neighbor{Issue: tracker.IssueID(issueID(8)), Order: "0|hzzzzz:"}
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: in[:1], Prev: jira}), "1:rank:refused(invalid)")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{"WEB-1"}, Prev: neighbor(t, 8, 1)}), "WEB-1:rank:refused(invalid)")
	// Before a first issue at the lowest value there is no room, and no
	// run to renumber.
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: in[:1], Next: neighbor(t, 9, -math.MaxFloat64)}), "1:rank:refused(invalid)")
}

// An issue that left the board is refused as stale and the others are
// ranked; a neighbor that left it refuses the whole rank; a read that
// fails sends nothing. Prev alone and Next alone rank past that end.
func TestRankChecksTheBoard(t *testing.T) {
	t.Parallel()
	afterPrev := spread(t, f64(1), nil, 1)
	elsewhere := issue(1, 0)
	elsewhere.Team = app
	beforeNext := spread(t, nil, f64(3), 1)
	p, _ := newProvider(t,
		// Issue 2 moved to another team: only issue 1 is ranked.
		onBoardX(ids(1, 2, 8), issue(1, 0), issue(8, 1)),
		orderX(1, afterPrev[0], lineartest.Exchange{}),
		// The neighbor moved: nothing is sent.
		onBoardX(ids(1, 9), issue(1, 0)),
		// Next alone.
		onBoardX(ids(1, 9), issue(1, 0), issue(9, 3)),
		orderX(1, beforeNext[0], lineartest.Exchange{}),
		// Every issue moved; an answer naming an issue of another team
		// does not put it on the board.
		onBoardX(ids(1, 8), elsewhere, issue(8, 1)),
		// The read fails.
		lineartest.Exchange{Op: "Issues", Vars: byIDVars(ids(1, 8), []string{web}, false), Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
	)
	ctx := context.Background()
	one, two := tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))
	r := p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one, two}, Prev: neighbor(t, 8, 1)})
	expectResult(t, r, "2:rank:refused(stale) 1:rank:applied")
	if ref, _ := r.Parts[0].Outcome.Refusal(); ref.Message != "issue "+issueID(2)+" is not on board "+web+" now" {
		t.Errorf("refusal %q", ref.Message)
	}
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Next: neighbor(t, 9, 3)}), "1:rank:refused(stale)")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Next: neighbor(t, 9, 3)}), "1:rank:applied")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Prev: neighbor(t, 8, 1)}), "1:rank:refused(stale)")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Prev: neighbor(t, 8, 1)}), "1:rank:not sent")
}

// With no room between the neighbors, Rank reads the manual order to its
// end or the first issue with room, and renumbers the moving issues and
// every crowded issue after Prev, each a part naming its row and card;
// it stops at the first update not applied.
func TestRankNoRoom(t *testing.T) {
	t.Parallel()
	tight := math.Nextafter(1, 2)
	prev, crowded, roomy, moving := issue(8, 1), issue(9, tight), issue(7, 3), issue(1, 10)
	in := tracker.RankInput{Board: web, Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}, Prev: neighbor(t, 8, 1), Next: neighbor(t, 9, tight)}
	board := onBoardX(ids(1, 8, 9), moving, prev, crowded)
	vs := spread(t, f64(1), f64(3), 2)
	atEnd := spread(t, f64(1), nil, 2)
	p, _ := newProvider(t,
		// Renumbered up to the first issue with room, across pages.
		board, manualX("", true, "r1", prev, crowded), manualX("r1", false, "", roomy, moving),
		orderX(1, vs[0], lineartest.Exchange{}), orderX(9, vs[1], lineartest.Exchange{}),
		// The second update's answer is lost.
		board, manualX("", false, "", prev, crowded, roomy, moving),
		orderX(1, vs[0], lineartest.Exchange{}), orderX(9, vs[1], lineartest.Exchange{Status: 500, Body: `{"errors":[{"message":"boom"}]}`}),
		// The first is refused; the second is not sent.
		board, manualX("", false, "", prev, crowded, roomy, moving),
		orderX(1, vs[0], lineartest.Exchange{Status: 400, Body: inputBody}),
		// The crowded run reaches the end of the list.
		board, manualX("", false, "", moving, prev, crowded),
		orderX(1, atEnd[0], lineartest.Exchange{}), orderX(9, atEnd[1], lineartest.Exchange{}),
		// Prev left the board after the check.
		board, manualX("", false, "", crowded, roomy),
	)
	ctx := context.Background()
	r := p.Rank(ctx, in)
	expectResult(t, r, "1:rank:applied 9:rank:applied")
	nine := tracker.IssueID(issueID(9))
	if !reflect.DeepEqual(r.Parts[1].Changed, []tracker.Resource{tracker.RowOf(web, nine), tracker.CardOf(nine)}) {
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
	script := []lineartest.Exchange{onBoardX(ids(1, 8, 9), issue(1, 5), issue(8, 1), issue(9, math.Nextafter(1, 2)))}
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
