package linearprovider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// issueTeamX expects the read of issue n with its team.
func issueTeamX(n int, team lineartest.Team, archived bool) lineartest.Exchange {
	return lineartest.Exchange{Op: "IssueTeam", Vars: `{"id":"` + issueID(n) + `"}`, Body: lineartest.Data(`"issue":` +
		js(map[string]any{"id": issueID(n), "archivedAt": nil, "trashed": archived, "team": json.RawMessage(team.JSON())}))}
}

// SetEstimate reads the issue with its team in one request, sends a
// value on the team's scale or none, and refuses an issue not on the
// board (gone, archived or of another team), a value off the scale and a
// team without estimates before the update.
func TestSetEstimate(t *testing.T) {
	t.Parallel()
	is := issue(1, 1)
	unused := lineartest.NewTeam("WEB")
	unused.Scale = "notUsed"
	id := tracker.IssueID(issueID(1))
	team := lineartest.NewTeam("WEB")
	p, _ := newProvider(t,
		issueTeamX(1, team, false), updateX(issueID(1), `{"estimate":5}`, applied(is.JSON())),
		issueTeamX(1, team, false), updateX(issueID(1), `{"estimate":null}`, applied(is.JSON())),
		issueTeamX(1, team, false),
		issueTeamX(1, team, false),
		lineartest.Exchange{Op: "IssueTeam", Vars: `{"id":"` + issueID(1) + `"}`, Body: notFound},
		issueTeamX(1, unused, false),
		issueTeamX(1, team, true),
		issueTeamX(1, lineartest.NewTeam("APP"), false),
	)
	ctx := context.Background()
	set := func(v *float64) tracker.Result {
		return p.SetEstimate(ctx, tracker.EstimateInput{Board: web, Issue: id, Value: v})
	}
	r := set(f64(5))
	expectResult(t, r, "1:estimate:applied")
	if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.RowOf(web, id), tracker.CardOf(id), tracker.DetailOf(id)}) {
		t.Errorf("changed %+v", r.Parts[0].Changed)
	}
	expectResult(t, set(nil), "1:estimate:applied")
	expectResult(t, set(f64(4)), "1:estimate:refused(invalid)")
	expectResult(t, set(f64(2.5)), "1:estimate:refused(invalid)")
	expectResult(t, set(f64(3)), "1:estimate:refused(stale)")
	expectResult(t, set(f64(3)), "1:estimate:refused(invalid)")
	expectResult(t, set(f64(3)), "1:estimate:refused(stale)")
	expectResult(t, set(f64(3)), "1:estimate:refused(stale)")
	expectResult(t, p.SetEstimate(ctx, tracker.EstimateInput{Board: web, Issue: "WEB-1", Value: f64(3)}), "WEB-1:estimate:refused(invalid)")
}

// cyclePositionsX expects the read of cycle c with the positions of ids
// on board web, answered with issues.
func cyclePositionsX(c lineartest.Cycle, ids []string, issues ...lineartest.Issue) lineartest.Exchange {
	var vars map[string]any
	if err := json.Unmarshal([]byte(byIDVars(ids, []string{web}, false)), &vars); err != nil {
		panic(err)
	}
	vars["id"] = c.ID
	var nodes []string
	for _, is := range issues {
		nodes = append(nodes, positionNode(is))
	}
	return lineartest.Exchange{Op: "CyclePositions", Vars: js(vars), Body: lineartest.Data(`"cycle":` + c.JSON() + `,` + lineartest.PageJSON(nodes, false, ""))}
}

// setCycleX expects the batch update of ids into cycle (nil: none),
// answered with each issue in it unless answer has a body.
func setCycleX(ns []int, cycle any, answer lineartest.Exchange) lineartest.Exchange {
	var nodes []string
	for _, n := range ns {
		is := issue(n, float64(n))
		is.Cycle, _ = cycle.(string)
		nodes = append(nodes, is.JSON())
	}
	answer.Op, answer.Vars = "UpdateIssues", js(map[string]any{"ids": ids(ns...), "input": map[string]any{"cycleId": cycle}})
	if answer.Body == "" {
		answer.Body = `{"data":{"result":{"success":true,"value":[` + strings.Join(nodes, ",") + `]}}}`
	}
	return answer
}

// inCycle is issue n in cycle.
func inCycle(n int, cycle string) lineartest.Issue {
	is := issue(n, float64(n))
	is.Cycle = cycle
	return is
}

// MoveToSprint reads the cycle with the issues on the board in one
// request and moves the issues on the board with one batch update; an
// issue not on the board, one the answer names as another team's
// included, is refused as stale, first. A failed update is
// settled per issue by a read: applied, not applied, or unknown when the
// read fails too. An empty sprint clears the cycle without reading one;
// a cycle of another board, a closed one and one that is gone refuse the
// move before any update.
func TestMoveToSprint(t *testing.T) {
	t.Parallel()
	open := lineartest.Cycle{ID: "c1", Number: 1, Team: web, StartsAt: at, EndsAt: at, Active: true}
	future, other, closed := open, open, open
	future.ID, future.Active, future.Future = "c2", false, true
	other.ID, other.Team = "c-app", app
	closed.ID, closed.Active, closed.Past = "c-old", false, true
	gone := `{"data":null,"errors":[{"message":"Entity not found: Cycle","path":["cycle"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	boom := lineartest.Exchange{Status: 500, Body: `{"errors":[{"message":"boom"}]}`}
	failedRead := readBackX(issue(1, 1), issue(2, 2))
	failedRead.Status, failedRead.Body = 500, boom.Body
	elsewhere := issue(5, 5)
	elsewhere.Team = app
	p, _ := newProvider(t,
		cyclePositionsX(open, ids(1, 2, 4, 5), issue(1, 1), issue(2, 2), issue(4, 4), elsewhere),
		setCycleX([]int{1, 2, 4}, "c1", lineartest.Exchange{}),
		cyclePositionsX(open, ids(1, 2), issue(1, 1), issue(2, 2)),
		setCycleX([]int{1, 2}, "c1", boom), readBackX(inCycle(1, "c1"), issue(2, 2)),
		cyclePositionsX(open, ids(1, 2), issue(1, 1), issue(2, 2)),
		setCycleX([]int{1, 2}, "c1", boom), failedRead,
		onBoardX(ids(3), issue(3, 3)), setCycleX([]int{3}, nil, lineartest.Exchange{}),
		cyclePositionsX(future, ids(3), issue(3, 3)), setCycleX([]int{3}, "c2", lineartest.Exchange{}),
		cyclePositionsX(other, ids(3), issue(3, 3)), cyclePositionsX(closed, ids(3), issue(3, 3)),
		lineartest.Exchange{Op: "CyclePositions", Vars: strings.Replace(cyclePositionsX(open, ids(3)).Vars, `"c1"`, `"c-gone"`, 1), Body: gone},
	)
	ctx := context.Background()
	in := []tracker.IssueID{tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2)), tracker.IssueID(issueID(4)), tracker.IssueID(issueID(5))}
	r := p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c1", Issues: in})
	expectResult(t, r, "5:sprint move:refused(stale) 1:sprint move:applied 2:sprint move:applied 4:sprint move:applied")
	if !reflect.DeepEqual(r.Parts[1].Changed, []tracker.Resource{tracker.RowOf(web, in[0]), tracker.CardOf(in[0]), tracker.DetailOf(in[0])}) {
		t.Errorf("changed %+v", r.Parts[1].Changed)
	}
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c1", Issues: in[:2]}), "1:sprint move:applied 2:sprint move:refused(tracker)")
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c1", Issues: in[:2]}), "1:sprint move:unknown 2:sprint move:unknown")
	three := []tracker.IssueID{tracker.IssueID(issueID(3))}
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Issues: three}), "3:sprint move:applied")
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c2", Issues: three}), "3:sprint move:applied")
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c-app", Issues: three}), "3:sprint move:refused(invalid)")
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c-old", Issues: three}), "3:sprint move:refused(stale)")
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Sprint: "c-gone", Issues: three}), "3:sprint move:refused(stale)")
	expectResult(t, p.MoveToSprint(ctx, tracker.SprintMoveInput{Board: web, Issues: []tracker.IssueID{"WEB-3"}}), "WEB-3:sprint move:refused(invalid)")
}
