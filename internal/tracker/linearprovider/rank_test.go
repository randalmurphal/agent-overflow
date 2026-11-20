package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// neighbor is issue n as a rank's neighbor.
func neighbor(n int) tracker.IssueID { return tracker.IssueID(issueID(n)) }

// renumberX expects the aliased update of issues ns to sortOrders vs,
// answered with each issue at its value unless answer has a body.
func renumberX(ns []int, vs []float64, answer lineartest.Exchange) lineartest.Exchange {
	vars := map[string]any{}
	var parts []string
	for i, n := range ns {
		vars[fmt.Sprintf("id%d", i)] = issueID(n)
		vars[fmt.Sprintf("input%d", i)] = map[string]any{"sortOrder": vs[i]}
		parts = append(parts, fmt.Sprintf(`"i%d":{"success":true,"value":%s}`, i, issue(n, vs[i]).JSON()))
	}
	answer.Op, answer.Vars = "RenumberIssues", js(vars)
	if answer.Body == "" && !answer.Reset {
		answer.Body = `{"data":{` + strings.Join(parts, ",") + `}}`
	}
	return answer
}

// readBackX expects the read that settles a failed write of issues,
// answered with them as they stand.
func readBackX(issues ...lineartest.Issue) lineartest.Exchange {
	var ids []string
	for _, is := range issues {
		ids = append(ids, is.ID)
	}
	return issuesX(byIDVars(ids, nil, true), issues...)
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

// manualX is one page of the team's issues by ID and order alone, oldest
// created first.
func manualX(after string, more bool, cursor string, issues ...lineartest.Issue) lineartest.Exchange {
	v := map[string]any{"first": rankPage, "sort": []any{map[string]any{"createdAt": map[string]string{"order": "Ascending"}}},
		"filter": map[string]any{"and": []any{map[string]any{"team": map[string]any{"id": map[string]any{"in": []string{web}}}}}}}
	if after != "" {
		v["after"] = after
	}
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, positionJSON(is))
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

// Rank reads the issues and neighbors on the board with their current
// values, then sends the values the client spreads between the
// neighbors' current values in one aliased update, each issue its own
// part naming the issue's row and card. Neighbors a caller saw at 1 and
// 3 that now stand at 10 and 20 rank between 10 and 20. An ID that is
// not an issue ID, a neighbor's included, is refused before any request.
func TestRank(t *testing.T) {
	t.Parallel()
	vs := spread(t, f64(10), f64(20), 2)
	p, _ := newProvider(t,
		onBoardX(ids(1, 2, 8, 9), issue(1, 0), issue(2, 0), issue(8, 10), issue(9, 20)),
		renumberX([]int{1, 2}, vs, lineartest.Exchange{}),
		onBoardX(ids(1, 9), issue(1, 0), issue(9, -math.MaxFloat64)),
	)
	ctx := context.Background()
	in := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))}
	r := p.Rank(ctx, tracker.RankInput{Board: web, Issues: in, Prev: neighbor(8), Next: neighbor(9)})
	expectResult(t, r, "1:rank:applied 2:rank:applied")
	if !reflect.DeepEqual(r.Parts[1].Changed, []tracker.Resource{tracker.RowOf(web, in[1]), tracker.CardOf(in[1])}) {
		t.Errorf("changed %+v", r.Parts[1].Changed)
	}
	for _, bad := range []tracker.RankInput{
		{Board: web, Issues: in[:1], Prev: "WEB-8"},
		{Board: web, Issues: in[:1], Prev: neighbor(8), Next: "WEB-9"},
	} {
		r := p.Rank(ctx, bad)
		expectResult(t, r, "1:rank:refused(invalid)")
		named := bad.Prev
		if bad.Next != "" {
			named = bad.Next
		}
		ref, _ := r.Parts[0].Outcome.Refusal()
		if ref.Message != fmt.Sprintf("%q is not a Linear issue ID", named) || !errors.Is(r.Parts[0].Outcome.Err(), tracker.ErrInvalidInput) {
			t.Errorf("a neighbor that is no issue ID: %q %v", ref.Message, r.Parts[0].Outcome.Err())
		}
	}
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{"WEB-1"}, Prev: neighbor(8)}), "WEB-1:rank:refused(invalid)")
	// Before a first issue at the lowest value there is no room, and no
	// run to renumber.
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: in[:1], Next: neighbor(9)}), "1:rank:refused(invalid)")
}

// Neighbors whose values are swapped refuse the whole rank as stale with
// nothing sent. Neighbors at equal values stand in the order the input
// names: the run after Prev, Next included, is renumbered.
func TestRankNeighborOrder(t *testing.T) {
	t.Parallel()
	prev, next, moving := issue(8, 20), issue(9, 10), issue(1, 0)
	tiedPrev, tiedNext := issue(8, 5), issue(9, 5)
	vs := spread(t, f64(5), nil, 2)
	p, _ := newProvider(t,
		onBoardX(ids(1, 2, 8, 9), moving, issue(2, 0), prev, next),
		onBoardX(ids(1, 8, 9), moving, tiedPrev, tiedNext),
		manualX("", false, "", tiedNext, tiedPrev, moving),
		renumberX([]int{1, 9}, vs, lineartest.Exchange{}),
	)
	ctx := context.Background()
	in := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))}
	r := p.Rank(ctx, tracker.RankInput{Board: web, Issues: in, Prev: neighbor(8), Next: neighbor(9)})
	expectResult(t, r, "1:rank:refused(stale) 2:rank:refused(stale)")
	if ref, _ := r.Parts[0].Outcome.Refusal(); ref.Message != "issue "+issueID(8)+" no longer stands before issue "+issueID(9) {
		t.Errorf("refusal %q", ref.Message)
	}
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: in[:1], Prev: neighbor(8), Next: neighbor(9)}), "1:rank:applied 9:rank:applied")
}

// An issue that left the board is refused as stale and the others are
// ranked; a neighbor that left it refuses the whole rank; a read that
// fails sends nothing. Prev alone and Next alone rank past that end.
func TestRankChecksTheBoard(t *testing.T) {
	t.Parallel()
	afterPrev := spread(t, f64(1), nil, 1)
	beforeNext := spread(t, nil, f64(3), 1)
	elsewhere := issue(1, 0)
	elsewhere.Team = app
	p, _ := newProvider(t,
		// Issue 2 moved to another team: only issue 1 is ranked.
		onBoardX(ids(1, 2, 8), issue(1, 0), issue(8, 1)),
		renumberX([]int{1}, afterPrev, lineartest.Exchange{}),
		// The neighbor moved: nothing is sent.
		onBoardX(ids(1, 9), issue(1, 0)),
		// Next alone.
		onBoardX(ids(1, 9), issue(1, 0), issue(9, 3)),
		renumberX([]int{1}, beforeNext, lineartest.Exchange{}),
		// Every issue moved; an answer naming an issue of another team
		// does not put it on the board.
		onBoardX(ids(1, 8), elsewhere, issue(8, 1)),
		// The read fails.
		lineartest.Exchange{Op: "IssuePositions", Vars: byIDVars(ids(1, 8), []string{web}, false), Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
	)
	ctx := context.Background()
	one, two := tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))
	r := p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one, two}, Prev: neighbor(8)})
	expectResult(t, r, "2:rank:refused(stale) 1:rank:applied")
	if ref, _ := r.Parts[0].Outcome.Refusal(); ref.Message != "issue "+issueID(2)+" is not on board "+web+" now" {
		t.Errorf("refusal %q", ref.Message)
	}
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Next: neighbor(9)}), "1:rank:refused(stale)")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Next: neighbor(9)}), "1:rank:applied")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Prev: neighbor(8)}), "1:rank:refused(stale)")
	expectResult(t, p.Rank(ctx, tracker.RankInput{Board: web, Issues: []tracker.IssueID{one}, Prev: neighbor(8)}), "1:rank:not sent")
}

// With no room between the neighbors, Rank reads the team's order by
// creation, sorts it by value, and renumbers the moving issues and every
// crowded issue after Prev up to the first with room, in one request,
// each a part naming its row and card. A failed request is settled per
// issue by a read.
func TestRankNoRoom(t *testing.T) {
	t.Parallel()
	tight := math.Nextafter(1, 2)
	prev, crowded, roomy, moving := issue(8, 1), issue(9, tight), issue(7, 3), issue(1, 10)
	in := tracker.RankInput{Board: web, Issues: []tracker.IssueID{tracker.IssueID(issueID(1))}, Prev: neighbor(8), Next: neighbor(9)}
	board := onBoardX(ids(1, 8, 9), moving, prev, crowded)
	vs := spread(t, f64(1), f64(3), 2)
	atEnd := spread(t, f64(1), nil, 2)
	foreign := issue(6, tight)
	foreign.Team = app
	p, _ := newProvider(t,
		// Renumbered up to the first issue with room, the order read
		// across pages in creation order; an issue answered as another
		// team's is not in the run.
		board, manualX("", true, "r1", roomy, crowded, foreign), manualX("r1", false, "", moving, prev),
		renumberX([]int{1, 9}, vs, lineartest.Exchange{}),
		// The request fails; the read after it finds the first applied
		// and the second not.
		board, manualX("", false, "", prev, crowded, roomy, moving),
		renumberX([]int{1, 9}, vs, lineartest.Exchange{Status: 500, Body: `{"errors":[{"message":"boom"}]}`}),
		readBackX(issue(1, vs[0]), crowded),
		// Refused, and the read finds neither applied.
		board, manualX("", false, "", prev, crowded, roomy, moving),
		renumberX([]int{1, 9}, vs, lineartest.Exchange{Status: 400, Body: inputBody}),
		readBackX(moving, crowded),
		// The crowded run reaches the end of the list.
		board, manualX("", false, "", moving, prev, crowded),
		renumberX([]int{1, 9}, atEnd, lineartest.Exchange{}),
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
	expectResult(t, p.Rank(ctx, in), "1:rank:applied 9:rank:refused(tracker)")
	expectResult(t, p.Rank(ctx, in), "1:rank:refused(tracker) 9:rank:refused(tracker)")
	expectResult(t, p.Rank(ctx, in), "1:rank:applied 9:rank:applied")
	expectResult(t, p.Rank(ctx, in), "1:rank:refused(stale)")
}

// A crowded run in a team longer than the read's bound is refused, and
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
		Prev: neighbor(8), Next: neighbor(9)}
	expectResult(t, p.Rank(context.Background(), in), "1:rank:refused(invalid)")
}
