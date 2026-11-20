package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// Each direct read asks for one entity by ID and types it; one that
// does not exist matches ErrNotFound, and an empty ID is refused before
// sending.
func TestReadOne(t *testing.T) {
	t.Parallel()
	ref := func(id, key string) lineartest.IssueRef {
		return lineartest.IssueRef{ID: id, Identifier: key, Title: "T", State: "state-todo"}
	}
	cycle := lineartest.Cycle{ID: "cycle-1", Number: 3, Team: "team-web", StartsAt: "2026-11-02T00:00:00.000Z", EndsAt: "2026-11-16T00:00:00.000Z", Active: true}
	notFound := `{"data":null,"errors":[{"message":"Entity not found: Comment","path":["comment"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	f := newFake(t,
		lineartest.Exchange{Op: "Comment", Vars: `{"id":"comment-2"}`, Body: `{"data":{"comment":` + commentJSON + `}}`},
		lineartest.Exchange{Op: "Comment", Vars: `{"id":"comment-9"}`, Body: notFound},
		lineartest.Exchange{Op: "Relation", Vars: `{"id":"rel-1"}`,
			Body: `{"data":{"issueRelation":` + lineartest.RelationJSON("rel-1", "blocks", ref("issue-1", "WEB-1"), ref("issue-2", "WEB-2")) + `}}`},
		lineartest.Exchange{Op: "Cycle", Vars: `{"id":"cycle-1"}`, Body: `{"data":{"cycle":` + cycle.JSON() + `}}`},
		lineartest.Exchange{Op: "Team", Vars: `{"id":"team-WEB"}`, Body: `{"data":{"team":` + team("WEB") + `}}`},
		lineartest.Exchange{Op: "Team", Vars: `{"id":"team-NONE"}`, Body: `{"data":{"team":null}}`},
	)
	c := f.client(t)
	ctx := context.Background()
	cm, err := c.Comment(ctx, "comment-2")
	if err != nil || cm.IssueID != "issue-1" || cm.UserID != "user-1" {
		t.Errorf("comment %+v %v", cm, err)
	}
	if _, err := c.Comment(ctx, "comment-9"); !errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("missing comment: %v", err)
	}
	rel, err := c.Relation(ctx, "rel-1")
	if err != nil || rel.Issue.ID != "issue-1" || rel.Related.ID != "issue-2" || rel.Type != RelationBlocks {
		t.Errorf("relation %+v %v", rel, err)
	}
	cy, err := c.Cycle(ctx, "cycle-1")
	if err != nil || cy.TeamID != "team-web" || !cy.IsActive {
		t.Errorf("cycle %+v %v", cy, err)
	}
	tm, err := c.Team(ctx, "team-WEB")
	if err != nil || tm.Key != "WEB" || tm.Estimation.Scale != EstimateFibonacci {
		t.Errorf("team %+v %v", tm, err)
	}
	if _, err := c.Team(ctx, "team-NONE"); err == nil || !strings.Contains(err.Error(), "holds no team") {
		t.Errorf("an answer without the team: %v", err)
	}
	for name, err := range map[string]error{
		"comment":  func() error { _, err := c.Comment(ctx, ""); return err }(),
		"relation": func() error { _, err := c.Relation(ctx, ""); return err }(),
		"cycle":    func() error { _, err := c.Cycle(ctx, ""); return err }(),
		"team":     func() error { _, err := c.Team(ctx, ""); return err }(),
	} {
		if !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%s with no ID: %v", name, err)
		}
	}
}

func idsVarsJSON(ids []string, after string) string {
	v := map[string]any{"ids": ids, "first": len(ids)}
	if after != "" {
		v["after"] = after
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// A by-ID read names each ID once, asks for a page of exactly that many,
// pages to the end, splits past 250 IDs, and sends nothing for no IDs.
func TestByIDReads(t *testing.T) {
	t.Parallel()
	user := func(id string) string { return lineartest.User{ID: id, Name: "User " + id, Active: true}.JSON() }
	many := make([]string, 251)
	for i := range many {
		many[i] = fmt.Sprintf("user-%d", i)
	}
	label := lineartest.Label{ID: "label-bug", Name: "Bug"}.JSON()
	project := lineartest.Project{ID: "project-1", Name: "Launch"}.JSON()
	cycle := lineartest.Cycle{ID: "cycle-1", Number: 1, Team: "team-app", StartsAt: "2026-11-02T00:00:00.000Z", EndsAt: "2026-11-16T00:00:00.000Z", Past: true}.JSON()
	f := newFake(t,
		lineartest.Exchange{Op: "UsersByID", Vars: idsVarsJSON([]string{"user-1", "user-2"}, ""),
			Body: lineartest.Data(lineartest.PageJSON([]string{user("user-1")}, true, "u1"))},
		lineartest.Exchange{Op: "UsersByID", Vars: idsVarsJSON([]string{"user-1", "user-2"}, "u1"),
			Body: lineartest.Data(lineartest.PageJSON([]string{user("user-2")}, false, ""))},
		lineartest.Exchange{Op: "UsersByID", Vars: idsVarsJSON(many[:250], ""), Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "UsersByID", Vars: idsVarsJSON(many[250:], ""), Body: lineartest.Data(lineartest.PageJSON([]string{user(many[250])}, false, ""))},
		lineartest.Exchange{Op: "LabelsByID", Vars: idsVarsJSON([]string{"label-bug"}, ""), Body: lineartest.Data(lineartest.PageJSON([]string{label}, false, ""))},
		lineartest.Exchange{Op: "ProjectsByID", Vars: idsVarsJSON([]string{"project-1"}, ""), Body: lineartest.Data(lineartest.PageJSON([]string{project}, false, ""))},
		lineartest.Exchange{Op: "CyclesByID", Vars: idsVarsJSON([]string{"cycle-1"}, ""), Body: lineartest.Data(lineartest.PageJSON([]string{cycle}, false, ""))},
		lineartest.Exchange{Op: "TeamsByID", Vars: idsVarsJSON([]string{"team-APP"}, ""), Body: lineartest.Data(lineartest.PageJSON([]string{team("APP")}, false, ""))},
	)
	c := f.client(t)
	ctx := context.Background()
	users, err := c.UsersByID(ctx, []string{"user-1", "user-2", "user-1"})
	if err != nil || len(users) != 2 || users[1].ID != "user-2" {
		t.Fatalf("users %+v %v", users, err)
	}
	if users, err := c.UsersByID(ctx, many); err != nil || len(users) != 1 {
		t.Fatalf("251 users %+v %v", users, err)
	}
	if users, err := c.UsersByID(ctx, nil); err != nil || users != nil {
		t.Errorf("no IDs: %+v %v", users, err)
	}
	if _, err := c.UsersByID(ctx, []string{""}); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("an empty ID: %v", err)
	}
	labels, err := c.LabelsByID(ctx, []string{"label-bug"})
	if err != nil || len(labels) != 1 || labels[0].Name != "Bug" {
		t.Errorf("labels %+v %v", labels, err)
	}
	projects, err := c.ProjectsByID(ctx, []string{"project-1"})
	if err != nil || len(projects) != 1 || projects[0].Name != "Launch" {
		t.Errorf("projects %+v %v", projects, err)
	}
	cycles, err := c.CyclesByID(ctx, []string{"cycle-1"})
	if err != nil || len(cycles) != 1 || cycles[0].TeamID != "team-app" {
		t.Errorf("cycles %+v %v", cycles, err)
	}
	teams, err := c.TeamsByID(ctx, []string{"team-APP"})
	if err != nil || len(teams) != 1 || teams[0].Key != "APP" {
		t.Errorf("teams %+v %v", teams, err)
	}
}

// TeamsByKey matches each key once, case ignored, and sends nothing for
// no keys.
func TestTeamsByKey(t *testing.T) {
	t.Parallel()
	f := newFake(t, lineartest.Exchange{Op: "TeamsByKey",
		Vars: `{"first":2,"filter":{"or":[{"key":{"eqIgnoreCase":"web"}},{"key":{"eqIgnoreCase":"APP"}}]}}`,
		Body: lineartest.Data(lineartest.PageJSON([]string{team("WEB")}, false, ""))})
	c := f.client(t)
	ctx := context.Background()
	teams, err := c.TeamsByKey(ctx, []string{"web", "APP", "WEB"})
	if err != nil || len(teams) != 1 || teams[0].Key != "WEB" {
		t.Fatalf("teams %+v %v", teams, err)
	}
	if teams, err := c.TeamsByKey(ctx, nil); err != nil || teams != nil {
		t.Errorf("no keys: %+v %v", teams, err)
	}
	if _, err := c.TeamsByKey(ctx, []string{""}); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("an empty key: %v", err)
	}
}

// The single-issue read pages each direction of relations to its end
// with the issue's ID, so the detail holds all of them, and fails when a
// later page fails; it carries the project's name.
func TestIssueRelationsReadToTheEnd(t *testing.T) {
	t.Parallel()
	ref := func(n int) lineartest.IssueRef {
		return lineartest.IssueRef{ID: fmt.Sprintf("issue-%d", n), Identifier: fmt.Sprintf("WEB-%d", n), Title: "T", State: "state-todo"}
	}
	rel := func(id string, from, to int) string { return lineartest.RelationJSON(id, "blocks", ref(from), ref(to)) }
	detail := lineartest.DetailJSON(lineartest.NewIssue("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1), lineartest.Detail{
		ProjectName: "Launch",
		Relations:   []string{rel("rel-1", 1, 2)}, RelationCursor: "r1",
		InverseRelations: []string{rel("rel-3", 4, 1)}, InverseRelationCursor: "i1",
	})
	pageOf := func(more bool, cursor string, nodes ...string) string {
		return `{"data":{` + lineartest.NodePageJSON(nodes, more, cursor) + `}}`
	}
	f := newFake(t,
		lineartest.Exchange{Op: "Issue", Vars: `{"id":"WEB-1"}`, Body: `{"data":{"issue":` + detail + `}}`},
		lineartest.Exchange{Op: "IssueRelations", Vars: `{"id":"issue-1","first":100,"after":"r1"}`, Body: pageOf(true, "r2", rel("rel-2", 1, 3))},
		lineartest.Exchange{Op: "IssueRelations", Vars: `{"id":"issue-1","first":100,"after":"r2"}`, Body: pageOf(false, "")},
		lineartest.Exchange{Op: "IssueInverseRelations", Vars: `{"id":"issue-1","first":100,"after":"i1"}`, Body: pageOf(false, "", rel("rel-4", 5, 1))},
		// A later page that fails fails the read.
		lineartest.Exchange{Op: "Issue", Vars: `{"id":"issue-1"}`, Body: `{"data":{"issue":` + detail + `}}`},
		lineartest.Exchange{Op: "IssueRelations", Vars: `{"id":"issue-1","first":100,"after":"r1"}`, Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
	)
	c := f.client(t)
	d, err := c.Issue(context.Background(), "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range d.Relations {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, " ") != "rel-1 rel-2 rel-3 rel-4" || d.ProjectID != "project-1" || d.ProjectName != "Launch" {
		t.Errorf("relations %v project %q %q", ids, d.ProjectID, d.ProjectName)
	}
	if d, err := c.Issue(context.Background(), "issue-1"); err == nil || !strings.Contains(err.Error(), "1 relations were read before it") {
		t.Errorf("a failed relation page: %+v %v", d, err)
	}
}

// IssueDetails reads its issues by ID in one page as large as the IDs, and
// each issue's relations past the first page with its ID; an ID the page
// leaves out is not returned. A page reporting more fails the read.
func TestIssueDetailsReadsByID(t *testing.T) {
	t.Parallel()
	id := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }
	ref := func(n int) lineartest.IssueRef {
		return lineartest.IssueRef{ID: id(n), Identifier: fmt.Sprintf("WEB-%d", n), Title: "T", State: "state-todo"}
	}
	rel := func(rid string, from, to int) string {
		return lineartest.RelationJSON(rid, "blocks", ref(from), ref(to))
	}
	one := lineartest.DetailJSON(lineartest.NewIssue(id(1), "WEB-1", "2026-11-02T10:00:00.000Z", 1), lineartest.Detail{
		Relations: []string{rel("rel-1", 1, 2)}, RelationCursor: "r1",
	})
	two := lineartest.DetailJSON(lineartest.NewIssue(id(2), "WEB-2", "2026-11-02T10:00:00.000Z", 2), lineartest.Detail{})
	vars := func(ids ...int) string {
		var in []string
		for _, n := range ids {
			in = append(in, `"`+id(n)+`"`)
		}
		return fmt.Sprintf(`{"filter":{"and":[{"id":{"in":[%s]}}]},"first":%d}`, strings.Join(in, ","), len(ids))
	}
	f := newFake(t,
		lineartest.Exchange{Op: "IssueDetails", Vars: vars(1, 2, 3), Body: lineartest.Data(lineartest.PageJSON([]string{one, two}, false, ""))},
		lineartest.Exchange{Op: "IssueRelations", Vars: `{"id":"` + id(1) + `","first":100,"after":"r1"}`,
			Body: `{"data":{` + lineartest.NodePageJSON([]string{rel("rel-2", 1, 3)}, false, "") + `}}`},
		lineartest.Exchange{Op: "IssueDetails", Vars: vars(2), Body: lineartest.Data(lineartest.PageJSON([]string{two}, true, "p1"))},
	)
	c := f.client(t)
	ds, err := c.IssueDetails(context.Background(), []string{id(1), id(2), id(3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].ID != id(1) || len(ds[0].Relations) != 2 || ds[1].ID != id(2) {
		t.Fatalf("details %+v", ds)
	}
	if _, err := c.IssueDetails(context.Background(), []string{id(2)}); err == nil {
		t.Error("a page reporting more was taken")
	}
	for _, ids := range [][]string{nil, make([]string, MaxIssueDetails+1), {"WEB-1"}} {
		if _, err := c.IssueDetails(context.Background(), ids); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%d IDs %v: %v", len(ids), ids, err)
		}
	}
}

// The lean list sends ListIssues's variables and types IDs, orders and
// teams.
func TestListIssuePositions(t *testing.T) {
	t.Parallel()
	f := newFake(t, lineartest.Exchange{Op: "IssuePositions", Vars: `{"first":250,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"team":{"id":{"in":["team-web"]}}}]}}`,
		Body: lineartest.Data(lineartest.PageJSON([]string{`{"id":"issue-1","sortOrder":-2.5,"team":{"id":"team-web"}}`}, true, "p1"))})
	p, err := f.client(t).ListIssuePositions(context.Background(), IssueQuery{Filter: IssueFilter{Teams: []string{"team-web"}}, Limit: 250}, "")
	if err != nil || len(p.Items) != 1 || p.Items[0] != (IssuePosition{ID: "issue-1", SortOrder: -2.5, TeamID: "team-web"}) || !p.HasNextPage || p.EndCursor != "p1" {
		t.Fatalf("page %+v %v", p, err)
	}
	if _, err := f.client(t).ListIssuePositions(context.Background(), IssueQuery{Limit: 251}, ""); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a page too large: %v", err)
	}
}

// KeptSince selects the cycles a window keeps on the server: active,
// future, or ended or completed at or after it, at the smaller page
// (Linear charges the connection by its page size) and paged to the end.
func TestCyclesKeptSince(t *testing.T) {
	t.Parallel()
	since := `{"gte":"2026-05-01T00:00:00.000Z"}`
	filter := `"filter":{"team":{"id":{"eq":"team-web"}},"or":[{"isActive":{"eq":true}},{"isFuture":{"eq":true}},{"endsAt":` + since + `},{"completedAt":` + since + `}]}`
	cycle := func(id string) string {
		return lineartest.Cycle{ID: id, Number: 1, Name: id, Team: "team-web", StartsAt: "2026-11-02T00:00:00.000Z", EndsAt: "2026-11-16T00:00:00.000Z"}.JSON()
	}
	f := newFake(t,
		lineartest.Exchange{Op: "Cycles", Vars: `{"first":20,` + filter + `}`,
			Body: lineartest.Data(lineartest.PageJSON([]string{cycle("cycle-1")}, true, "c1"))},
		lineartest.Exchange{Op: "Cycles", Vars: `{"first":20,"after":"c1",` + filter + `}`,
			Body: lineartest.Data(lineartest.PageJSON([]string{cycle("cycle-2")}, false, ""))})
	q := CycleQuery{TeamID: "team-web", KeptSince: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	cycles, err := f.client(t).Cycles(context.Background(), q)
	if err != nil || len(cycles) != 2 || cycles[0].ID != "cycle-1" || cycles[1].ID != "cycle-2" {
		t.Fatalf("cycles %+v %v", cycles, err)
	}
}
