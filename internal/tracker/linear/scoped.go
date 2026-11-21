package linear

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Reads of one issue together with a catalog of its team, for a caller
// that knows the issue and not its team: one request where reading the
// issue and then the catalog would be two. Each catalog is a nested
// connection charged by its page size, like the catalog reads
// (catalog.go). A catalog longer than its nested page is read to its end
// by the catalog's own call.

// IssueScope is an issue as a scoped read names it: its team and its
// current state.
type IssueScope struct {
	IssueID string
	TeamID  string
	StateID string
}

// scopedIssue is the issue part every scoped document selects, its team
// carrying the catalog page W.
type scopedIssue[W any] struct {
	ID    string `json:"id"`
	State *ref   `json:"state"`
	Team  *struct {
		ID   string        `json:"id"`
		Page connection[W] `json:"page"`
	} `json:"team"`
}

func (s *scopedIssue[W]) scope(op operation) (IssueScope, connection[W], error) {
	if s == nil || s.Team == nil {
		return IssueScope{}, connection[W]{}, fmt.Errorf("linear %s: the answer holds no issue with its team", op.name)
	}
	return IssueScope{IssueID: s.ID, TeamID: s.Team.ID, StateID: s.State.id()}, s.Team.Page, nil
}

var opIssueStates = newQuery("IssueStates", "issue", `query IssueStates($id: String!) {
  issue(id: $id) {
    id state { id }
    team { id page: states(first: 50) { nodes { ...StateFields } pageInfo { hasNextPage } } }
  }
}
`+stateFields)

// IssueStates returns an issue, by its ID or identifier, with its team's
// workflow states, archived ones excluded, in Linear's order: one
// request for a team of at most 50 states, which nearly every team is.
// A missing or invisible issue matches trackerapi.ErrNotFound.
func (c *Client) IssueStates(ctx context.Context, idOrIdentifier string) (IssueScope, []WorkflowState, error) {
	if idOrIdentifier == "" {
		return IssueScope{}, nil, invalid(opIssueStates.name, "an issue ID or identifier is required")
	}
	var data struct {
		Issue *scopedIssue[wireState] `json:"issue"`
	}
	if _, err := c.call(ctx, opIssueStates, idVars{idOrIdentifier}, &data); err != nil {
		return IssueScope{}, nil, err
	}
	scope, conn, err := data.Issue.scope(opIssueStates)
	if err != nil {
		return IssueScope{}, nil, err
	}
	states, more, err := firstPage(opIssueStates, conn, wireState.typed)
	if err == nil && more {
		states, err = c.WorkflowStates(ctx, scope.TeamID)
	}
	return scope, states, err
}

var opIssueMembers = newQuery("IssueMembers", "issue", `query IssueMembers($id: String!, $first: Int!) {
  issue(id: $id) {
    id state { id }
    team { id page: members(first: $first) { nodes { ...UserFields } pageInfo { hasNextPage endCursor } } }
  }
}
`+userFields)

type firstVars struct {
	ID    string `json:"id"`
	First int    `json:"first"`
}

// IssueMembers returns an issue, by its ID or identifier, with its
// team's members, read to the end as one trackerapi.Walk. A missing or
// invisible issue matches trackerapi.ErrNotFound.
func (c *Client) IssueMembers(ctx context.Context, idOrIdentifier string) (IssueScope, []User, error) {
	if idOrIdentifier == "" {
		return IssueScope{}, nil, invalid(opIssueMembers.name, "an issue ID or identifier is required")
	}
	r, err := trackerapi.Walk(ctx, func(ctx context.Context) (scopedMembers, error) {
		scope, members, err := c.issueMembers(ctx, idOrIdentifier)
		return scopedMembers{scope, members}, err
	})
	return r.scope, r.members, err
}

// scopedMembers is what IssueMembers returns, as one value for a walk.
type scopedMembers struct {
	scope   IssueScope
	members []User
}

// issueMembers is one walk of IssueMembers.
func (c *Client) issueMembers(ctx context.Context, idOrIdentifier string) (IssueScope, []User, error) {
	var data struct {
		Issue *scopedIssue[User] `json:"issue"`
	}
	if _, err := c.call(ctx, opIssueMembers, firstVars{idOrIdentifier, catalogPage}, &data); err != nil {
		return IssueScope{}, nil, err
	}
	scope, conn, err := data.Issue.scope(opIssueMembers)
	if err != nil {
		return IssueScope{}, nil, err
	}
	p, err := page(opIssueMembers, conn, Usage{}, same[User])
	if err != nil {
		return IssueScope{}, nil, err
	}
	members := p.Items
	for p.HasNextPage {
		if p, err = fetchPage(ctx, c, opTeamMembers, teamPageVars{pageVars{First: catalogPage, After: p.EndCursor}, scope.TeamID}, same[User]); err != nil {
			return IssueScope{}, nil, fmt.Errorf("%w (%d members were read before it)", err, len(members))
		}
		members = append(members, p.Items...)
	}
	return scope, members, nil
}

var opIssueLabels = newQuery("IssueLabels", "issue", `query IssueLabels($id: String!, $first: Int!) {
  issue(id: $id) {
    id state { id }
    team { id page: labels(first: $first) { nodes { ...LabelFields } pageInfo { hasNextPage endCursor } } }
  }
  workspace: issueLabels(filter: { team: { null: true } }, first: $first) {
    nodes { ...LabelFields } pageInfo { hasNextPage endCursor }
  }
}
`+labelFields)

// IssueLabels returns an issue, by its ID or identifier, with the labels
// its team's issues can carry: the team's own and the workspace's, as
// Labels lists them, each once. A missing or invisible issue matches
// trackerapi.ErrNotFound.
func (c *Client) IssueLabels(ctx context.Context, idOrIdentifier string) (IssueScope, []Label, error) {
	if idOrIdentifier == "" {
		return IssueScope{}, nil, invalid(opIssueLabels.name, "an issue ID or identifier is required")
	}
	var data struct {
		Issue     *scopedIssue[wireLabel] `json:"issue"`
		Workspace *connection[wireLabel]  `json:"workspace"`
	}
	if _, err := c.call(ctx, opIssueLabels, firstVars{idOrIdentifier, catalogPage}, &data); err != nil {
		return IssueScope{}, nil, err
	}
	scope, conn, err := data.Issue.scope(opIssueLabels)
	if err == nil && data.Workspace == nil {
		err = fmt.Errorf("linear %s: the answer holds no workspace labels", opIssueLabels.name)
	}
	if err != nil {
		return IssueScope{}, nil, err
	}
	team, moreTeam, err := firstPage(opIssueLabels, conn, wireLabel.typed)
	if err != nil {
		return IssueScope{}, nil, err
	}
	workspace, moreWorkspace, err := firstPage(opIssueLabels, *data.Workspace, wireLabel.typed)
	if err != nil {
		return IssueScope{}, nil, err
	}
	if moreTeam || moreWorkspace {
		labels, err := c.Labels(ctx, scope.TeamID)
		return scope, labels, err
	}
	labels := team
	for _, l := range workspace {
		if !slices.ContainsFunc(labels, func(t Label) bool { return t.ID == l.ID }) {
			labels = append(labels, l)
		}
	}
	return scope, labels, nil
}

var opCyclePositions = newQuery("CyclePositions", "cycle", `query CyclePositions($id: String!, $filter: IssueFilter, $sort: [IssueSortInput!], $includeArchived: Boolean, $first: Int!) {
  cycle(id: $id) { ...CycleFields }
  page: issues(filter: $filter, sort: $sort, includeArchived: $includeArchived, first: $first) {
    nodes { ...PositionFields } pageInfo { hasNextPage endCursor }
  }
}
`+cycleFields+positionFields)

type cyclePositionsVars struct {
	issuesVars
	ID string `json:"id"`
}

// CyclePositions returns a cycle by its ID with the first page of the
// issue positions q selects: one request where Cycle and
// ListIssuePositions are two, for a write that checks the cycle and the
// issues before moving them into it. A missing or invisible cycle
// matches trackerapi.ErrNotFound and reads no issue.
func (c *Client) CyclePositions(ctx context.Context, cycleID string, q IssueQuery) (*Cycle, Page[IssuePosition], error) {
	if cycleID == "" {
		return nil, Page[IssuePosition]{}, invalid(opCyclePositions.name, "a cycle ID is required")
	}
	vars, err := issueVars(q, "")
	if err != nil {
		return nil, Page[IssuePosition]{}, invalid(opCyclePositions.name, "%w", err)
	}
	var data struct {
		Cycle *wireCycle                `json:"cycle"`
		Page  *connection[wirePosition] `json:"page"`
	}
	r, err := c.call(ctx, opCyclePositions, cyclePositionsVars{vars, cycleID}, &data)
	if err != nil {
		return nil, Page[IssuePosition]{}, err
	}
	if data.Cycle == nil || data.Page == nil {
		return nil, Page[IssuePosition]{}, fmt.Errorf("linear %s: the answer holds no cycle with its issues", opCyclePositions.name)
	}
	p, err := page(opCyclePositions, *data.Page, r.Usage, wirePosition.typed)
	if err != nil {
		return nil, Page[IssuePosition]{}, err
	}
	p.Served = r.Served
	cycle := data.Cycle.typed()
	return &cycle, p, nil
}

var opIssueTeam = newQuery("IssueTeam", "issue", `query IssueTeam($id: String!) {
  issue(id: $id) { id archivedAt trashed team { ...TeamFields } }
}
`+teamFields)

// IssueTeam is an issue with its whole team.
type IssueTeam struct {
	IssueID string
	// Archived reports an archived or trashed issue.
	Archived bool
	Team     Team
}

// IssueTeam returns an issue, by its ID or identifier, archived or not,
// with its team's settings: one request where IssueSummary and Team are
// two. A missing or invisible issue matches trackerapi.ErrNotFound.
func (c *Client) IssueTeam(ctx context.Context, idOrIdentifier string) (*IssueTeam, error) {
	if idOrIdentifier == "" {
		return nil, invalid(opIssueTeam.name, "an issue ID or identifier is required")
	}
	var data struct {
		Issue *struct {
			ID         string     `json:"id"`
			ArchivedAt *time.Time `json:"archivedAt"`
			Trashed    *bool      `json:"trashed"`
			Team       *wireTeam  `json:"team"`
		} `json:"issue"`
	}
	if _, err := c.call(ctx, opIssueTeam, idVars{idOrIdentifier}, &data); err != nil {
		return nil, err
	}
	if data.Issue == nil || data.Issue.Team == nil {
		return nil, fmt.Errorf("linear %s: the answer holds no issue with its team", opIssueTeam.name)
	}
	w := data.Issue
	archived := w.ArchivedAt != nil || (w.Trashed != nil && *w.Trashed)
	return &IssueTeam{IssueID: w.ID, Archived: archived, Team: w.Team.typed()}, nil
}
