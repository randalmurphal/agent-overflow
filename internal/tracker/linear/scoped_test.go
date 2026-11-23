package linear

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// scopedIssueJSON is an issue of team-web in state-todo whose team
// carries page.
func scopedIssueJSON(page string) string {
	return `{"id":"issue-1","state":{"id":"state-todo"},"team":{"id":"team-web","page":` + page + `}}`
}

var notFoundBody = `{"data":null,"errors":[{"message":"Entity not found: Issue","path":["issue"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`

// IssueStates answers a team of up to 50 states from the one request,
// and reads a longer team with WorkflowStates; a missing issue is not
// found.
func TestIssueStates(t *testing.T) {
	t.Parallel()
	state := lineartest.State{ID: "state-todo", Name: "Todo", Type: "unstarted", Team: "team-web"}.JSON()
	f := newFake(t,
		lineartest.Exchange{Op: "IssueStates", Vars: `{"id":"WEB-1"}`, Body: lineartest.Data(`"issue":` + scopedIssueJSON(lineartest.ConnectionJSON([]string{state}, false, "")))},
		lineartest.Exchange{Op: "IssueStates", Vars: `{"id":"WEB-2"}`, Body: lineartest.Data(`"issue":` + scopedIssueJSON(lineartest.ConnectionJSON([]string{state}, true, "")))},
		lineartest.Exchange{Op: "WorkflowStates", Vars: `{"teamId":"team-web","first":100}`, Body: lineartest.Data(lineartest.PageJSON([]string{state, state}, false, ""))},
		lineartest.Exchange{Op: "IssueStates", Vars: `{"id":"WEB-9"}`, Body: notFoundBody},
	)
	c := f.client(t)
	ctx := context.Background()
	scope, states, err := c.IssueStates(ctx, "WEB-1")
	if err != nil || scope != (IssueScope{IssueID: "issue-1", TeamID: "team-web", StateID: "state-todo"}) || len(states) != 1 {
		t.Fatalf("scope %+v states %+v %v", scope, states, err)
	}
	if _, states, err = c.IssueStates(ctx, "WEB-2"); err != nil || len(states) != 2 {
		t.Fatalf("a longer team: %+v %v", states, err)
	}
	if _, _, err = c.IssueStates(ctx, "WEB-9"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a missing issue: %v", err)
	}
	if _, _, err = c.IssueStates(ctx, ""); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("no ID: %v", err)
	}
}

// IssueMembers continues a longer team with TeamMembers from the nested
// page's cursor.
func TestIssueMembers(t *testing.T) {
	t.Parallel()
	user := func(id string) string { return lineartest.User{ID: id, Name: id, Active: true}.JSON() }
	f := newFake(t,
		lineartest.Exchange{Op: "IssueMembers", Vars: `{"id":"WEB-1","first":100}`, Body: lineartest.Data(`"issue":` + scopedIssueJSON(lineartest.ConnectionJSON([]string{user("user-1")}, true, "m1")))},
		lineartest.Exchange{Op: "TeamMembers", Vars: `{"teamId":"team-web","first":100,"after":"m1"}`, Body: `{"data":{` + lineartest.NodePageJSON([]string{user("user-2")}, false, "") + `}}`},
		lineartest.Exchange{Op: "IssueMembers", Vars: `{"id":"WEB-9","first":100}`, Body: notFoundBody},
	)
	c := f.client(t)
	ctx := context.Background()
	scope, users, err := c.IssueMembers(ctx, "WEB-1")
	if err != nil || scope.TeamID != "team-web" || len(users) != 2 || users[1].ID != "user-2" {
		t.Fatalf("scope %+v users %+v %v", scope, users, err)
	}
	if _, _, err = c.IssueMembers(ctx, "WEB-9"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a missing issue: %v", err)
	}
}

// IssueLabels joins the team's labels and the workspace's, each once,
// and reads both with Labels when either has more.
func TestIssueLabels(t *testing.T) {
	t.Parallel()
	team := lineartest.Label{ID: "label-bug", Name: "Bug", Team: "team-web"}.JSON()
	ws := lineartest.Label{ID: "label-ws", Name: "Feature"}.JSON()
	answer := func(more bool) string {
		return lineartest.Data(`"issue":` + scopedIssueJSON(lineartest.ConnectionJSON([]string{team}, more, "l1")) +
			`,"workspace":` + lineartest.ConnectionJSON([]string{ws, team}, false, ""))
	}
	f := newFake(t,
		lineartest.Exchange{Op: "IssueLabels", Vars: `{"id":"WEB-1","first":100}`, Body: answer(false)},
		lineartest.Exchange{Op: "IssueLabels", Vars: `{"id":"WEB-2","first":100}`, Body: answer(true)},
		lineartest.Exchange{Op: "Labels", Vars: `{"first":100,"filter":{"or":[{"team":{"id":{"eq":"team-web"}}},{"team":{"null":true}}]}}`,
			Body: lineartest.Data(lineartest.PageJSON([]string{team, ws, ws}, false, ""))},
		lineartest.Exchange{Op: "IssueLabels", Vars: `{"id":"WEB-9","first":100}`, Body: notFoundBody},
	)
	c := f.client(t)
	ctx := context.Background()
	scope, labels, err := c.IssueLabels(ctx, "WEB-1")
	if err != nil || scope.TeamID != "team-web" || len(labels) != 2 || labels[0].ID != "label-bug" || labels[1].ID != "label-ws" {
		t.Fatalf("scope %+v labels %+v %v", scope, labels, err)
	}
	if _, labels, err = c.IssueLabels(ctx, "WEB-2"); err != nil || len(labels) != 3 {
		t.Fatalf("a longer catalog: %+v %v", labels, err)
	}
	if _, _, err = c.IssueLabels(ctx, "WEB-9"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a missing issue: %v", err)
	}
}

// IssueTeam reports an archived or trashed issue with its team's
// settings.
func TestIssueTeam(t *testing.T) {
	t.Parallel()
	team := lineartest.NewTeam("WEB").JSON()
	f := newFake(t,
		lineartest.Exchange{Op: "IssueTeam", Vars: `{"id":"WEB-1"}`, Body: lineartest.Data(`"issue":{"id":"issue-1","archivedAt":null,"trashed":null,"team":` + team + `}`)},
		lineartest.Exchange{Op: "IssueTeam", Vars: `{"id":"WEB-2"}`, Body: lineartest.Data(`"issue":{"id":"issue-2","archivedAt":null,"trashed":true,"team":` + team + `}`)},
		lineartest.Exchange{Op: "IssueTeam", Vars: `{"id":"WEB-9"}`, Body: notFoundBody},
	)
	c := f.client(t)
	ctx := context.Background()
	it, err := c.IssueTeam(ctx, "WEB-1")
	if err != nil || it.IssueID != "issue-1" || it.Archived || it.Team.ID != "team-WEB" || it.Team.Estimation.Scale != EstimateFibonacci {
		t.Fatalf("issue team %+v %v", it, err)
	}
	if it, err = c.IssueTeam(ctx, "WEB-2"); err != nil || !it.Archived {
		t.Fatalf("a trashed issue: %+v %v", it, err)
	}
	if _, err = c.IssueTeam(ctx, "WEB-9"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a missing issue: %v", err)
	}
}

// CyclePositions reads a cycle and the first page of positions in one
// request; a missing cycle is not found.
func TestCyclePositions(t *testing.T) {
	t.Parallel()
	cycle := lineartest.Cycle{ID: "cycle-2", Number: 2, Team: "team-web", StartsAt: "2026-11-16T00:00:00.000Z", EndsAt: "2026-11-30T00:00:00.000Z", Future: true}.JSON()
	vars := `{"id":"cycle-2","first":2,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"id":{"in":["` + uuid1 + `","` + uuid2 + `"]}},{"team":{"id":{"in":["team-web"]}}}]}}`
	f := newFake(t,
		lineartest.Exchange{Op: "CyclePositions", Vars: vars, Body: lineartest.Data(`"cycle":` + cycle + `,` + lineartest.PageJSON([]string{`{"id":"` + uuid1 + `","sortOrder":3,"team":{"id":"team-web"}}`}, false, ""))},
		lineartest.Exchange{Op: "CyclePositions", Vars: strings.Replace(vars, "cycle-2", "cycle-9", 1),
			Body: `{"data":null,"errors":[{"message":"Entity not found: Cycle","path":["cycle"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`},
	)
	c := f.client(t)
	ctx := context.Background()
	q := IssueQuery{Filter: IssueFilter{IDs: []string{uuid1, uuid2}, Teams: []string{"team-web"}}, Limit: 2}
	cy, p, err := c.CyclePositions(ctx, "cycle-2", q)
	if err != nil || cy.ID != "cycle-2" || !cy.IsFuture || len(p.Items) != 1 || p.Items[0].SortOrder != 3 || p.Items[0].TeamID != "team-web" {
		t.Fatalf("cycle %+v positions %+v %v", cy, p, err)
	}
	if _, _, err = c.CyclePositions(ctx, "cycle-9", q); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("a missing cycle: %v", err)
	}
	if _, _, err = c.CyclePositions(ctx, "", q); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("no cycle: %v", err)
	}
}
