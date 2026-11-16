package linear

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

const teamJSON = `{"id":"team-%s","key":"%s","name":"Web","description":null,"timezone":"Europe/Dublin",` +
	`"issueEstimationType":"fibonacci","issueEstimationExtended":false,"issueEstimationAllowZero":true,"defaultIssueEstimate":1,` +
	`"cyclesEnabled":true,"cycleDuration":2,"activeCycle":{"id":"cycle-1"},"triageEnabled":true,"defaultIssueState":{"id":"state-backlog"},` +
	`"triageIssueState":{"id":"state-triage"},"setIssueSortOrderOnStateChange":"first","archivedAt":null,"updatedAt":"2026-11-01T09:00:00.000Z"}`

func team(key string) string { return strings.ReplaceAll(teamJSON, "%s", key) }

// A catalog read pages to the end with the cursor each page returns.
func TestTeamsPagesToTheEnd(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		exchange{op: "Teams", vars: `{"first":100}`, body: data(pageJSON([]string{team("WEB")}, true, "cursor-1"))},
		exchange{op: "Teams", vars: `{"first":100,"after":"cursor-1"}`, body: data(pageJSON([]string{team("APP")}, false, ""))},
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
		exchange{op: "Teams", vars: `{"first":100}`, body: data(pageJSON([]string{team("WEB")}, true, "cursor-1"))},
		exchange{op: "Teams", vars: `{"first":100,"after":"cursor-1"}`, status: 400, body: inputBody},
	)
	teams, err := f.client(t).Teams(context.Background())
	if teams != nil || !strings.Contains(err.Error(), "1 items were read before it") {
		t.Fatalf("teams %v err %v", teams, err)
	}
	if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok {
		t.Errorf("the transport error is lost: %v", err)
	}

	stuck := newFake(t,
		exchange{op: "Teams", vars: `{"first":100}`, body: data(pageJSON([]string{team("WEB")}, true, "cursor-1"))},
		exchange{op: "Teams", vars: `{"first":100,"after":"cursor-1"}`, body: data(pageJSON([]string{team("WEB")}, true, "cursor-1"))},
	)
	if _, err := stuck.client(t).Teams(context.Background()); err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("a repeated cursor: %v", err)
	}
	noCursor := newFake(t, exchange{op: "Teams", vars: `{"first":100}`, body: data(pageJSON(nil, true, ""))})
	if _, err := noCursor.client(t).Teams(context.Background()); err == nil || !strings.Contains(err.Error(), "without a cursor") {
		t.Errorf("a next page without a cursor: %v", err)
	}
}

func TestCatalogReads(t *testing.T) {
	t.Parallel()
	state := `{"id":"state-todo","name":"Todo","color":"#aaa","description":null,"type":"unstarted","position":1,"team":{"id":"team-web"},"archivedAt":null}`
	user := `{"id":"user-2","name":"User 2","displayName":"user2","email":"user2@example.com","avatarUrl":"https://example.com/a.png","active":true,"admin":true,"guest":false,"app":false,"isMe":false}`
	label := `{"id":"label-bug","name":"Bug","color":"#f00","description":null,"isGroup":false,"parent":{"id":"label-type"},"team":null,"archivedAt":null}`
	project := `{"id":"project-1","name":"Launch","url":"https://linear.app/example/project/launch","status":{"id":"ps-1","name":"In Progress","type":"started"},` +
		`"lead":{"id":"user-1"},"startDate":"2026-11-01","targetDate":null,"completedAt":null,"canceledAt":null,"archivedAt":null,"updatedAt":"2026-11-01T09:00:00.000Z"}`
	cycle := `{"id":"cycle-1","number":1,"name":"Sprint 1","description":null,"team":{"id":"team-web"},"startsAt":"2026-11-02T00:00:00.000Z","endsAt":"2026-11-16T00:00:00.000Z",` +
		`"completedAt":null,"archivedAt":null,"isActive":false,"isFuture":false,"isPast":true,"isNext":false,"isPrevious":true,"progress":0.5,` +
		`"scopeHistory":[3,5],"completedScopeHistory":[0,2],"inProgressScopeHistory":[1,1],"issueCountHistory":[2,3],"completedIssueCountHistory":[0,1]}`
	f := newFake(t,
		exchange{op: "WorkflowStates", vars: `{"teamId":"team-web","first":100}`, body: data(pageJSON([]string{state}, false, ""))},
		exchange{op: "Users", vars: `{"first":100}`, body: data(pageJSON([]string{user}, false, ""))},
		exchange{op: "Users", vars: `{"first":100,"includeDisabled":true}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "TeamMembers", vars: `{"teamId":"team-web","first":100}`, body: `{"data":{"node":{` + pageJSON([]string{user}, false, "") + `}}}`},
		exchange{op: "Labels", vars: `{"first":100,"filter":{"or":[{"team":{"id":{"eq":"team-web"}}},{"team":{"null":true}}]}}`, body: data(pageJSON([]string{label}, false, ""))},
		exchange{op: "Labels", vars: `{"first":100}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "Projects", vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"team-web"}}}}}`, body: data(pageJSON([]string{project}, false, ""))},
		exchange{op: "Projects", vars: `{"first":100,"includeArchived":true}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "Cycles", vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}},"isPast":{"eq":true}}}`, body: data(pageJSON([]string{cycle}, false, ""))},
		exchange{op: "Cycles", vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}},"isActive":{"eq":true}},"includeArchived":true}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "Cycles", vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}},"isFuture":{"eq":true}}}`, body: data(pageJSON(nil, false, ""))},
		exchange{op: "Cycles", vars: `{"first":50,"filter":{"team":{"id":{"eq":"team-web"}}}}`, body: data(pageJSON(nil, false, ""))},
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
