package linear

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"

	"agent-overflow/internal/tracker/linear/lineartest"
)

func team(key string) string { return lineartest.NewTeam(key).JSON() }

// A catalog read pages to the end with the cursor each page returns.
func TestTeamsPagesToTheEnd(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON([]string{team("WEB")}, true, "cursor-1"))},
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100,"after":"cursor-1"}`, Body: lineartest.Data(lineartest.PageJSON([]string{team("APP")}, false, ""))},
	)
	teams, err := f.client(t).Teams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 2 || teams[0].Key != "WEB" || teams[1].Key != "APP" {
		t.Fatalf("teams %+v", teams)
	}
	want := Estimation{Scale: EstimateFibonacci, AllowZero: true, Default: 1}
	got := teams[0]
	if got.Estimation != want || !got.CyclesEnabled || got.CycleWeeks != 2 || got.ActiveCycleID != "cycle-1" ||
		!got.TriageEnabled || got.DefaultStateID != "state-backlog" || got.TriageStateID != "state-triage" || got.SortOnStateChange != "first" {
		t.Errorf("team %+v", got)
	}
}

// A failure on a later page fails the whole read and says how much was
// read; a cursor that does not advance is refused rather than looped on.
func TestCatalogMidWalkFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON([]string{team("WEB")}, true, "cursor-1"))},
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100,"after":"cursor-1"}`, Status: 400, Body: inputBody},
	)
	teams, err := f.client(t).Teams(context.Background())
	if teams != nil || !strings.Contains(err.Error(), "1 items were read before it") {
		t.Fatalf("teams %v err %v", teams, err)
	}
	if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok {
		t.Errorf("the transport error is lost: %v", err)
	}

	stuck := newFake(t,
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON([]string{team("WEB")}, true, "cursor-1"))},
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100,"after":"cursor-1"}`, Body: lineartest.Data(lineartest.PageJSON([]string{team("WEB")}, true, "cursor-1"))},
	)
	if _, err := stuck.client(t).Teams(context.Background()); err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("a repeated cursor: %v", err)
	}
	noCursor := newFake(t, lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON(nil, true, ""))})
	if _, err := noCursor.client(t).Teams(context.Background()); err == nil || !strings.Contains(err.Error(), "without a cursor") {
		t.Errorf("a next page without a cursor: %v", err)
	}
}

func TestCatalogReads(t *testing.T) {
	t.Parallel()
	state := lineartest.State{ID: "state-todo", Name: "Todo", Type: "unstarted", Position: 1, Team: "team-web"}.JSON()
	user := lineartest.User{ID: "user-2", Name: "User 2", DisplayName: "user2", Email: "user2@example.com", Active: true, Admin: true}.JSON()
	label := lineartest.Label{ID: "label-bug", Name: "Bug", Parent: "label-type"}.JSON()
	project := lineartest.Project{ID: "project-1", Name: "Launch", Lead: "user-1", StartDate: "2026-11-01"}.JSON()
	cycle := lineartest.Cycle{ID: "cycle-1", Number: 1, Name: "Sprint 1", Team: "team-web", StartsAt: "2026-11-02T00:00:00.000Z",
		EndsAt: "2026-11-16T00:00:00.000Z", Past: true, Previous: true}.JSON()
	f := newFake(t,
		lineartest.Exchange{Op: "WorkflowStates", Vars: `{"teamId":"team-web","first":100}`, Body: lineartest.Data(lineartest.PageJSON([]string{state}, false, ""))},
		lineartest.Exchange{Op: "Users", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON([]string{user}, false, ""))},
		lineartest.Exchange{Op: "Users", Vars: `{"first":100,"includeDisabled":true}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "TeamMembers", Vars: `{"teamId":"team-web","first":100}`, Body: `{"data":{"node":{` + lineartest.PageJSON([]string{user}, false, "") + `}}}`},
		lineartest.Exchange{Op: "Labels", Vars: `{"first":100,"filter":{"or":[{"team":{"id":{"eq":"team-web"}}},{"team":{"null":true}}]}}`, Body: lineartest.Data(lineartest.PageJSON([]string{label}, false, ""))},
		lineartest.Exchange{Op: "Labels", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"team-web"}}}}}`, Body: lineartest.Data(lineartest.PageJSON([]string{project}, false, ""))},
		lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"includeArchived":true}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Cycles", Vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}},"isPast":{"eq":true}}}`, Body: lineartest.Data(lineartest.PageJSON([]string{cycle}, false, ""))},
		lineartest.Exchange{Op: "Cycles", Vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}},"isActive":{"eq":true}},"includeArchived":true}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Cycles", Vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}},"isFuture":{"eq":true}}}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "Cycles", Vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}}}}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
	)
	c := f.client(t)
	ctx := context.Background()
	states, err := c.WorkflowStates(ctx, "team-web")
	if err != nil || len(states) != 1 || states[0].Type != StateUnstarted || states[0].TeamID != "team-web" || states[0].Position != 1 {
		t.Fatalf("states %+v %v", states, err)
	}
	users, err := c.Users(ctx, false)
	if err != nil || len(users) != 1 || !users[0].Admin || users[0].AvatarURL == "" {
		t.Fatalf("users %+v %v", users, err)
	}
	if _, err := c.Users(ctx, true); err != nil {
		t.Fatal(err)
	}
	if members, err := c.TeamMembers(ctx, "team-web"); err != nil || len(members) != 1 || members[0].ID != "user-2" {
		t.Fatalf("members %+v %v", members, err)
	}
	labels, err := c.Labels(ctx, "team-web")
	if err != nil || len(labels) != 1 || labels[0].TeamID != "" || labels[0].ParentID != "label-type" {
		t.Fatalf("labels %+v %v", labels, err)
	}
	if _, err := c.Labels(ctx, ""); err != nil {
		t.Fatal(err)
	}
	projects, err := c.Projects(ctx, "team-web", false)
	if err != nil || len(projects) != 1 || projects[0].Status.Type != "started" || projects[0].LeadID != "user-1" || projects[0].StartDate != "2026-11-01" {
		t.Fatalf("projects %+v %v", projects, err)
	}
	if _, err := c.Projects(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	cycles, err := c.Cycles(ctx, CycleQuery{TeamID: "team-web", When: CyclesPast})
	if err != nil || len(cycles) != 1 || !cycles[0].IsPast || cycles[0].Name != "Sprint 1" || len(cycles[0].CompletedIssueCountHistory) != 2 {
		t.Fatalf("cycles %+v %v", cycles, err)
	}
	for _, q := range []CycleQuery{{TeamID: "team-web", When: CyclesActive, IncludeArchived: true}, {TeamID: "team-web", When: CyclesUpcoming}, {TeamID: "team-web"}} {
		if _, err := c.Cycles(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for name, err := range map[string]error{
		"states":  func() error { _, err := c.WorkflowStates(ctx, ""); return err }(),
		"members": func() error { _, err := c.TeamMembers(ctx, ""); return err }(),
		"cycles":  func() error { _, err := c.Cycles(ctx, CycleQuery{}); return err }(),
		"when":    func() error { _, err := c.Cycles(ctx, CycleQuery{TeamID: "t", When: 9}); return err }(),
	} {
		if err == nil {
			t.Errorf("%s: a missing argument was accepted", name)
		}
	}
}
