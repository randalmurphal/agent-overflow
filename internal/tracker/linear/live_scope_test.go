//go:build linearlive

package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// TestLiveScope measures what the provider's pre-write checks, by-ID
// reads and create clears assume of Linear, each answer a MEASURE line.
// It creates three issues (a parent and two sub-issues) and trashes them
// when it ends. Run as TestLive is, with -run TestLiveScope; it makes
// under 60 requests.
func TestLiveScope(t *testing.T) {
	l := newLive(t)
	c := l.c
	var viewer *Viewer
	l.must("Viewer", func(ctx context.Context) (err error) { viewer, err = c.Viewer(ctx); return err })
	var teams []Team
	l.must("Teams", func(ctx context.Context) (err error) { teams, err = c.Teams(ctx); return err })
	if len(teams) == 0 {
		t.Fatal("the workspace has no team")
	}
	team := teams[0]
	l.sweep(team)
	var labels []Label
	l.step("Labels", func(ctx context.Context) (err error) { labels, err = c.Labels(ctx, team.ID); return err })
	var projects []Project
	l.step("Projects", func(ctx context.Context) (err error) { projects, err = c.Projects(ctx, team.ID, false); return err })
	l.measure("the workspace has %d teams, team 1 has %d labels and %d projects", len(teams), len(labels), len(projects))

	stamp := time.Now().UTC().Format("20060102T150405Z")
	parentIn := CreateIssueInput{
		TeamID: team.ID, Title: liveTitle + stamp + " scope parent",
		Priority: Set(PriorityHigh), AssigneeID: Set(viewer.User.ID), DueDate: Set(Date("2027-01-15")),
	}
	for _, lb := range labels[:min(len(labels), 3)] {
		parentIn.LabelIDs = append(parentIn.LabelIDs, lb.ID)
	}
	if len(projects) > 0 {
		parentIn.ProjectID = Set(projects[0].ID)
	}
	parent := l.create("parent", parentIn)
	l.measure("parent: priority %d assignee set %t due %q labels %d project set %t cycle set %t",
		parent.Priority, parent.AssigneeID != "", parent.DueDate, len(parent.LabelIDs), parent.ProjectID != "", parent.CycleID != "")

	bare := l.create("sub-issue naming only its parent", CreateIssueInput{
		TeamID: team.ID, Title: liveTitle + stamp + " scope child bare", ParentID: Set(parent.ID),
	})
	l.inherits("a sub-issue that names only its parent", bare)
	cleared := l.create("sub-issue with clears", CreateIssueInput{
		TeamID: team.ID, Title: liveTitle + stamp + " scope child cleared", ParentID: Set(parent.ID),
		AssigneeID: Null[string](), Priority: Set(PriorityNone), DueDate: Null[Date](), ProjectID: Null[string](),
	})
	l.inherits("a sub-issue sent assigneeId null, priority 0, dueDate null, projectId null", cleared)

	l.checkMissing()
	l.checkByID(team, viewer, parent)
	l.checkNestedLabelCost([]string{parent.ID, bare.ID})
	l.checkPageLimit([]string{parent.ID, bare.ID})
	l.checkForeignState(teams, bare)
}

// create creates an issue and registers its cleanup.
func (l *live) create(name string, in CreateIssueInput) *Issue {
	l.t.Helper()
	var issue *Issue
	l.must("CreateIssue "+name, func(ctx context.Context) (err error) { issue, err = l.c.CreateIssue(ctx, in); return err })
	l.t.Cleanup(func() { l.cleanup(issue) })
	return issue
}

func (l *live) inherits(what string, is *Issue) {
	l.measure("%s has: priority %d, assignee set %t, due %q, labels %d, project set %t, cycle set %t",
		what, is.Priority, is.AssigneeID != "", is.DueDate, len(is.LabelIDs), is.ProjectID != "", is.CycleID != "")
}

// checkMissing reads each single entity by an ID that names nothing.
func (l *live) checkMissing() {
	const none = "00000000-0000-4000-8000-000000000000"
	reads := map[string]func(context.Context) error{
		"comment":       func(ctx context.Context) error { _, err := l.c.Comment(ctx, none); return err },
		"issueRelation": func(ctx context.Context) error { _, err := l.c.Relation(ctx, none); return err },
		"cycle":         func(ctx context.Context) error { _, err := l.c.Cycle(ctx, none); return err },
		"team":          func(ctx context.Context) error { _, err := l.c.Team(ctx, none); return err },
	}
	for name, read := range reads {
		ctx, cancel := context.WithTimeout(trackerapi.WithInteractive(context.Background()), time.Minute)
		err := read(ctx)
		cancel()
		l.measure("%s for a missing ID: matches ErrNotFound %t, error %v", name, errors.Is(err, trackerapi.ErrNotFound), err)
	}
}

// checkByID runs the by-ID and by-key reads against what exists and logs
// each one's reported cost.
func (l *live) checkByID(team Team, viewer *Viewer, parent *Issue) {
	l.step("Team", func(ctx context.Context) error {
		got, err := l.c.Team(ctx, team.ID)
		if err == nil {
			l.measure("team by ID: same team %t, scale %q, cost %d", got.ID == team.ID, got.Estimation.Scale, l.c.Usage().Complexity)
		}
		return err
	})
	l.step("TeamsByKey", func(ctx context.Context) error {
		got, err := l.c.TeamsByKey(ctx, []string{strings.ToLower(team.Key), "zzznoteam"})
		if err == nil {
			l.measure("teams by lowercased key and an unknown key: %d found, the team %t, cost %d", len(got), len(got) == 1 && got[0].ID == team.ID, l.c.Usage().Complexity)
		}
		return err
	})
	l.step("TeamsByID", func(ctx context.Context) error {
		got, err := l.c.TeamsByID(ctx, []string{team.ID})
		if err == nil {
			l.measure("teams by ID: %d found, cost %d", len(got), l.c.Usage().Complexity)
		}
		return err
	})
	l.step("UsersByID", func(ctx context.Context) error {
		got, err := l.c.UsersByID(ctx, []string{viewer.User.ID})
		if err == nil {
			l.measure("users by ID: %d found, cost %d", len(got), l.c.Usage().Complexity)
		}
		return err
	})
	if len(parent.LabelIDs) > 0 {
		l.step("LabelsByID", func(ctx context.Context) error {
			got, err := l.c.LabelsByID(ctx, parent.LabelIDs)
			if err == nil {
				l.measure("labels by %d IDs: %d found, cost %d", len(parent.LabelIDs), len(got), l.c.Usage().Complexity)
			}
			return err
		})
	}
	l.step("Cycles kept since", func(ctx context.Context) error {
		got, err := l.c.Cycles(ctx, CycleQuery{TeamID: team.ID, KeptSince: time.Now().Add(-180 * 24 * time.Hour)})
		if err == nil {
			l.measure("cycles kept since 180 days ago: accepted, %d found (team cycles enabled %t), cost %d", len(got), team.CyclesEnabled, l.c.Usage().Complexity)
		}
		return err
	})
	for _, limit := range []int{50, 250} {
		l.step(fmt.Sprintf("ListIssuePositions %d", limit), func(ctx context.Context) error {
			p, err := l.c.ListIssuePositions(ctx, IssueQuery{Filter: IssueFilter{Teams: []string{team.ID}}, Limit: limit}, "")
			if err == nil {
				l.measure("issue positions with first %d: %d returned, cost %d", limit, len(p.Items), l.c.Usage().Complexity)
			}
			return err
		})
	}
}

var (
	opLivePlain = newQuery("LivePlainIDs", "page", `query LivePlainIDs($ids: [ID!]!, $first: Int!) {
  page: issues(filter: { id: { in: $ids } }, first: $first) { nodes { id } pageInfo { hasNextPage endCursor } }
}
`)
	opLiveLabels = newQuery("LiveNestedLabels", "page", `query LiveNestedLabels($ids: [ID!]!, $first: Int!) {
  page: issues(filter: { id: { in: $ids } }, first: $first) {
    nodes { id labels { nodes { id name } } } pageInfo { hasNextPage endCursor }
  }
}
`)
	opLiveLabels5 = newQuery("LiveNestedLabels5", "page", `query LiveNestedLabels5($ids: [ID!]!, $first: Int!) {
  page: issues(filter: { id: { in: $ids } }, first: $first) {
    nodes { id labels(first: 5) { nodes { id name } } } pageInfo { hasNextPage endCursor }
  }
}
`)
)

// checkNestedLabelCost reads the same two issues plain, with nested
// labels at the default page size, and with nested labels at five, at
// two outer page sizes, so the charge for a nested list is told apart
// from the charge for the results.
func (l *live) checkNestedLabelCost(ids []string) {
	for _, first := range []int{2, 50} {
		for _, op := range []operation{opLivePlain, opLiveLabels, opLiveLabels5} {
			l.step(op.name, func(ctx context.Context) error {
				var data map[string]json.RawMessage
				r, err := l.c.call(ctx, op, idsVars{pageVars{First: first}, ids}, &data)
				if err == nil {
					l.measure("%s for 2 issues with first %d: cost %d", op.name, first, r.Usage.Complexity)
				}
				return err
			})
		}
	}
}

var (
	opLiveLabels250 = newQuery("LiveNestedLabels250", "page", `query LiveNestedLabels250($ids: [ID!]!, $first: Int!) {
  page: issues(filter: { id: { in: $ids } }, first: $first) {
    nodes { id labels(first: 250) { nodes { id name } } } pageInfo { hasNextPage endCursor }
  }
}
`)
	opLiveLabels251 = newQuery("LiveNestedLabels251", "page", `query LiveNestedLabels251($ids: [ID!]!, $first: Int!) {
  page: issues(filter: { id: { in: $ids } }, first: $first) {
    nodes { id labels(first: 251) { nodes { id name } } } pageInfo { hasNextPage endCursor }
  }
}
`)
)

// checkPageLimit asks for a nested page of 250 and of 251, and an outer
// page of 251, to learn the largest page Linear serves.
func (l *live) checkPageLimit(ids []string) {
	for _, tc := range []struct {
		op    operation
		first int
	}{{opLiveLabels250, 2}, {opLiveLabels251, 2}, {opLivePlain, 250}, {opLivePlain, 251}} {
		ctx, cancel := context.WithTimeout(trackerapi.WithInteractive(context.Background()), time.Minute)
		var data map[string]json.RawMessage
		r, err := l.c.call(ctx, tc.op, idsVars{pageVars{First: tc.first}, ids}, &data)
		cancel()
		l.measure("%s with outer first %d: error %v, cost %d", tc.op.name, tc.first, err, r.Usage.Complexity)
	}
}

// checkForeignState moves an issue to a status of another team, when the
// workspace has one.
func (l *live) checkForeignState(teams []Team, issue *Issue) {
	if len(teams) < 2 {
		l.measure("a status of another team: not measurable, the workspace has one team")
		return
	}
	var states []WorkflowState
	if !l.step("WorkflowStates other team", func(ctx context.Context) (err error) {
		states, err = l.c.WorkflowStates(ctx, teams[1].ID)
		return err
	}) || len(states) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(trackerapi.WithInteractive(context.Background()), time.Minute)
	defer cancel()
	got, err := l.c.UpdateIssue(ctx, issue.ID, UpdateIssueInput{StateID: Set(states[0].ID)})
	if err != nil {
		l.measure("issueUpdate with another team's status: refused %t, error %v", true, err)
		logBody(l.t, err)
		return
	}
	l.measure("issueUpdate with another team's status: applied, issue's team unchanged %t, status is the other team's %t",
		got.TeamID == issue.TeamID, got.StateID == states[0].ID)
}

var _ = testing.Short
