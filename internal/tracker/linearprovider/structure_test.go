package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func TestIdentityProjectsBoards(t *testing.T) {
	t.Parallel()
	viewer := `{"data":{"viewer":` + lineartest.User{ID: "user-1", Name: "", DisplayName: "user1", Email: "user1@example.com", Active: true}.JSON() +
		`,"organization":{"id":"org-1","name":"Example","urlKey":"example"}}}`
	noCycles := lineartest.NewTeam("APP")
	noCycles.Cycles = false
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "Viewer", Body: viewer},
		teamsX(lineartest.NewTeam("WEB"), noCycles),
		teamX(noCycles),
		lineartest.Exchange{Op: "Team", Vars: `{"id":"team-gone"}`,
			Body: `{"data":null,"errors":[{"message":"Entity not found: Team","path":["team"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`},
	)
	ctx := context.Background()
	me, err := p.Identity(ctx)
	if err != nil || me != (tracker.User{ID: "user-1", Name: "user1", Email: "user1@example.com", AvatarURL: "https://example.com/user-1.png", Active: true}) {
		t.Fatalf("identity %+v %v", me, err)
	}
	projects, err := p.Projects(ctx)
	if err != nil || len(projects) != 2 || projects[1] != (tracker.Project{ID: app, Key: "APP", Name: "Web"}) {
		t.Fatalf("projects %+v %v", projects, err)
	}
	boards, err := p.Boards(ctx, app)
	if err != nil || len(boards) != 1 || boards[0] != (tracker.Board{ID: app, Project: app, Name: "Web", Sprints: false}) {
		t.Fatalf("boards %+v %v", boards, err)
	}
	if boards, err := p.Boards(ctx, "team-gone"); err != nil || boards != nil {
		t.Errorf("an invisible team's boards %+v %v", boards, err)
	}
	if types, err := p.IssueTypes(ctx, web); err != nil || types != nil {
		t.Errorf("issue types %+v %v", types, err)
	}
	if lt, _ := p.LinkTypes(ctx); len(lt) != 3 || lt[0].Outward != "blocks" || lt[0].Inward != "is blocked by" {
		t.Errorf("link types %+v", lt)
	}
	if p.Capabilities() != (tracker.Capabilities{}) || p.Time() != nil {
		t.Error("Linear claims a capability or time tracking")
	}
	if _, err := New(nil); err == nil {
		t.Error("New accepted no client")
	}
}

// Columns are the states in board order (type, then position), one
// status each; each type counts in its group; the cycles are those
// Linear says the window keeps; users are the workspace's.
func TestStructure(t *testing.T) {
	t.Parallel()
	window := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	states := []lineartest.State{
		{ID: "s-done", Name: "Done", Type: "completed", Position: 1, Team: web},
		{ID: "s-dup", Name: "Duplicate", Type: "duplicate", Position: 0, Team: web},
		{ID: "s-review", Name: "In Review", Type: "started", Position: 2, Team: web},
		{ID: "s-progress", Name: "In Progress", Type: "started", Position: 1, Team: web},
		{ID: "s-todo", Name: "Todo", Type: "unstarted", Position: 0, Team: web},
		{ID: "s-canceled", Name: "Canceled", Type: "canceled", Position: 0, Team: web},
		{ID: "s-backlog", Name: "Backlog", Type: "backlog", Position: 0, Team: web},
		{ID: "s-triage", Name: "Triage", Type: "triage", Position: 0, Team: web},
	}
	cycles := []lineartest.Cycle{
		{ID: "c-ended-in", Number: 2, Team: web, StartsAt: "2026-09-20T00:00:00.000Z", EndsAt: "2026-10-04T00:00:00.000Z", CompletedAt: "2026-10-04T00:00:00.000Z", Past: true},
		{ID: "c-now", Number: 3, Name: "Sprint 3", Team: web, StartsAt: "2026-11-16T00:00:00.000Z", EndsAt: "2026-11-30T00:00:00.000Z", Active: true},
		{ID: "c-next", Number: 4, Team: web, StartsAt: "2026-11-30T00:00:00.000Z", EndsAt: "2026-12-14T00:00:00.000Z", Future: true},
	}
	var cycleNodes []string
	for _, c := range cycles {
		cycleNodes = append(cycleNodes, c.JSON())
	}
	tshirt := lineartest.NewTeam("WEB")
	tshirt.Scale, tshirt.Extended, tshirt.AllowZero = "tShirt", true, false
	p, _ := newProvider(t,
		teamX(tshirt),
		statesX(web, states...),
		lineartest.Exchange{Op: "Cycles", Vars: keptCyclesX(web, "2026-10-01T00:00:00.000Z").Vars, Body: page(cycleNodes...)},
		lineartest.Exchange{Op: "Users", Vars: `{"first":100,"includeDisabled":true}`, Body: page(
			lineartest.User{ID: "user-1", Name: "User 1", Active: true}.JSON(), lineartest.User{ID: "user-2", Name: "User 2"}.JSON())},
	)
	s, err := p.Structure(context.Background(), web, window)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, c := range s.Columns {
		if len(c.Statuses) != 1 || string(c.Statuses[0]) == "" {
			t.Errorf("column %+v", c)
		}
		cols = append(cols, string(c.Statuses[0]))
	}
	want := []string{"s-triage", "s-backlog", "s-todo", "s-progress", "s-review", "s-done", "s-canceled", "s-dup"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns %v, want %v", cols, want)
	}
	groups := map[tracker.StatusID]tracker.StatusGroup{}
	for _, st := range s.Statuses {
		groups[st.ID] = st.Group
	}
	wantGroups := map[tracker.StatusID]tracker.StatusGroup{
		"s-triage": tracker.GroupBacklog, "s-backlog": tracker.GroupBacklog, "s-todo": tracker.GroupToDo,
		"s-progress": tracker.GroupInProgress, "s-review": tracker.GroupInProgress, "s-done": tracker.GroupDone,
		"s-canceled": tracker.GroupCanceled, "s-dup": tracker.GroupCanceled,
	}
	if !reflect.DeepEqual(groups, wantGroups) {
		t.Errorf("groups %v", groups)
	}
	var kept []string
	for _, sp := range s.Sprints {
		kept = append(kept, string(sp.ID)+":"+sp.Name)
	}
	if !reflect.DeepEqual(kept, []string{"c-ended-in:Cycle 2", "c-now:Sprint 3", "c-next:Cycle 4"}) {
		t.Errorf("sprints %v", kept)
	}
	if s.Sprints[1].State != tracker.SprintActive || s.Sprints[2].State != tracker.SprintFuture || s.Sprints[0].State != tracker.SprintClosed ||
		s.Sprints[0].Completed.IsZero() || !s.Sprints[1].Completed.IsZero() || s.Sprints[1].Board != web {
		t.Errorf("sprint states %+v", s.Sprints)
	}
	if s.Board != (tracker.Board{ID: web, Project: web, Name: "Web", Sprints: true}) || len(s.Users) != 2 || s.Users[1].Active {
		t.Errorf("structure %+v", s)
	}
	var labels []string
	for _, c := range s.Estimate.Choices {
		labels = append(labels, c.Label)
	}
	if s.Estimate.Unit != tracker.UnitPoints || strings.Join(labels, " ") != "XS S M L XL XXL XXXL" || s.Estimate.Choices[6].Value != 21 {
		t.Errorf("estimate %+v", s.Estimate)
	}
}

// A state of a type Linear did not document fails the read rather than
// count in a guessed group; an invisible team is not found.
func TestStructureRefusesUnknownState(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t,
		teamX(lineartest.NewTeam("WEB")),
		statesX(web, lineartest.State{ID: "s-x", Name: "X", Type: "paused", Team: web}),
		lineartest.Exchange{Op: "Team", Vars: `{"id":"team-gone"}`,
			Body: `{"data":null,"errors":[{"message":"Entity not found: Team","path":["team"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`},
	)
	if _, err := p.Structure(context.Background(), web, time.Time{}); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("unknown state type: %v", err)
	}
	if _, err := p.Structure(context.Background(), "team-gone", time.Time{}); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("invisible team: %v", err)
	}
}

func TestEstimateScales(t *testing.T) {
	t.Parallel()
	values := func(e tracker.Estimate) []float64 {
		var out []float64
		for _, c := range e.Choices {
			out = append(out, c.Value)
		}
		return out
	}
	for _, tc := range []struct {
		in   linear.Estimation
		unit tracker.EstimateUnit
		want []float64
	}{
		{linear.Estimation{Scale: linear.EstimateNotUsed}, tracker.UnitNone, nil},
		{linear.Estimation{Scale: linear.EstimateExponential}, tracker.UnitPoints, []float64{1, 2, 4, 8, 16}},
		{linear.Estimation{Scale: linear.EstimateExponential, Extended: true, AllowZero: true}, tracker.UnitPoints, []float64{0, 1, 2, 4, 8, 16, 32, 64}},
		{linear.Estimation{Scale: linear.EstimateFibonacci, Extended: true}, tracker.UnitPoints, []float64{1, 2, 3, 5, 8, 13, 21}},
		{linear.Estimation{Scale: linear.EstimateLinear, Extended: true}, tracker.UnitPoints, []float64{1, 2, 3, 4, 5, 6, 7}},
		{linear.Estimation{Scale: linear.EstimateTShirt}, tracker.UnitPoints, []float64{1, 2, 3, 5, 8}},
		{linear.Estimation{Scale: "custom"}, tracker.UnitPoints, nil},
	} {
		got := estimate(tc.in)
		if got.Unit != tc.unit || !reflect.DeepEqual(values(got), tc.want) {
			t.Errorf("%+v: %+v", tc.in, got)
		}
	}
}
