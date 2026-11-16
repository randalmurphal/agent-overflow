package linear

import (
	"context"
)

// Page sizes of the catalog reads, each well under Linear's 10,000-point
// cap per query: a team costs about 6 points, a state 2, a user 2, a
// label 3, a project 4 and a cycle 4 (its histories are scalar lists).
const (
	catalogPage = 100
	cyclePage   = 50
)

var opViewer = newQuery("Viewer", "viewer", `query Viewer {
  viewer { ...UserFields }
  organization { id name urlKey }
}
`+userFields)

// Viewer returns the key's user and its workspace.
func (c *Client) Viewer(ctx context.Context) (*Viewer, error) {
	var v Viewer
	if _, err := c.call(ctx, opViewer, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

var opTeams = newQuery("Teams", "page", `query Teams($first: Int!, $after: String) {
  page: teams(first: $first, after: $after) { nodes { ...TeamFields } pageInfo { hasNextPage endCursor } }
}
`+teamFields)

// Teams returns every team the key can see, paged to the end.
func (c *Client) Teams(ctx context.Context) ([]Team, error) {
	return collect(ctx, c, opTeams, func(after string) any {
		return pageVars{First: catalogPage, After: after}
	}, wireTeam.typed)
}

type teamPageVars struct {
	pageVars
	TeamID string `json:"teamId"`
}

var opWorkflowStates = newQuery("WorkflowStates", "page", `query WorkflowStates($teamId: ID!, $first: Int!, $after: String) {
  page: workflowStates(filter: { team: { id: { eq: $teamId } } }, first: $first, after: $after) {
    nodes { ...StateFields } pageInfo { hasNextPage endCursor }
  }
}
`+stateFields)

// WorkflowStates returns a team's workflow states, archived ones
// excluded, in Linear's order; sort by Type and Position to show them.
func (c *Client) WorkflowStates(ctx context.Context, teamID string) ([]WorkflowState, error) {
	if teamID == "" {
		return nil, invalid("WorkflowStates", "a team ID is required")
	}
	return collect(ctx, c, opWorkflowStates, func(after string) any {
		return teamPageVars{pageVars{First: catalogPage, After: after}, teamID}
	}, wireState.typed)
}

// CycleWhen picks cycles by time.
type CycleWhen int

const (
	CyclesAll CycleWhen = iota
	CyclesActive
	CyclesUpcoming // not started yet
	CyclesPast
)

// CycleQuery selects a team's cycles.
type CycleQuery struct {
	TeamID          string // required
	When            CycleWhen
	IncludeArchived bool
}

var opCycles = newQuery("Cycles", "page", `query Cycles($filter: CycleFilter!, $includeArchived: Boolean, $first: Int!, $after: String) {
  page: cycles(filter: $filter, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...CycleFields } pageInfo { hasNextPage endCursor }
  }
}
`+cycleFields)

type cycleFilter struct {
	Team     *entityFilter `json:"team"`
	IsActive *boolEq       `json:"isActive,omitempty"`
	IsFuture *boolEq       `json:"isFuture,omitempty"`
	IsPast   *boolEq       `json:"isPast,omitempty"`
}

type boolEq struct {
	Eq bool `json:"eq"`
}

type cycleVars struct {
	pageVars
	Filter          cycleFilter `json:"filter"`
	IncludeArchived bool        `json:"includeArchived,omitempty"`
}

// Cycles returns a team's cycles that q selects, paged to the end.
// Cycles are generated from the team's settings: the API cannot create
// one, and StartUpcomingCycle starts the next.
func (c *Client) Cycles(ctx context.Context, q CycleQuery) ([]Cycle, error) {
	if q.TeamID == "" {
		return nil, invalid("Cycles", "a team ID is required")
	}
	filter := cycleFilter{Team: idEq(q.TeamID)}
	switch q.When {
	case CyclesAll:
	case CyclesActive:
		filter.IsActive = &boolEq{true}
	case CyclesUpcoming:
		filter.IsFuture = &boolEq{true}
	case CyclesPast:
		filter.IsPast = &boolEq{true}
	default:
		return nil, invalid("Cycles", "unknown CycleWhen")
	}
	return collect(ctx, c, opCycles, func(after string) any {
		return cycleVars{pageVars{First: cyclePage, After: after}, filter, q.IncludeArchived}
	}, wireCycle.typed)
}

var opUsers = newQuery("Users", "page", `query Users($includeDisabled: Boolean, $first: Int!, $after: String) {
  page: users(includeDisabled: $includeDisabled, first: $first, after: $after) {
    nodes { ...UserFields } pageInfo { hasNextPage endCursor }
  }
}
`+userFields)

type usersVars struct {
	pageVars
	IncludeDisabled bool `json:"includeDisabled,omitempty"`
}

// Users returns the workspace's users, deactivated ones only with
// includeDisabled, paged to the end.
func (c *Client) Users(ctx context.Context, includeDisabled bool) ([]User, error) {
	return collect(ctx, c, opUsers, func(after string) any {
		return usersVars{pageVars{First: catalogPage, After: after}, includeDisabled}
	}, same[User])
}

var opTeamMembers = newQuery("TeamMembers", "node", `query TeamMembers($teamId: String!, $first: Int!, $after: String) {
  node: team(id: $teamId) {
    page: members(first: $first, after: $after) { nodes { ...UserFields } pageInfo { hasNextPage endCursor } }
  }
}
`+userFields)

// TeamMembers returns a team's members, paged to the end.
func (c *Client) TeamMembers(ctx context.Context, teamID string) ([]User, error) {
	if teamID == "" {
		return nil, invalid("TeamMembers", "a team ID is required")
	}
	return collect(ctx, c, opTeamMembers, func(after string) any {
		return teamPageVars{pageVars{First: catalogPage, After: after}, teamID}
	}, same[User])
}

var opLabels = newQuery("Labels", "page", `query Labels($filter: IssueLabelFilter, $first: Int!, $after: String) {
  page: issueLabels(filter: $filter, first: $first, after: $after) {
    nodes { ...LabelFields } pageInfo { hasNextPage endCursor }
  }
}
`+labelFields)

type labelFilter struct {
	Or []labelTeamFilter `json:"or"`
}

type labelTeamFilter struct {
	Team *entityFilter `json:"team"`
}

type labelsVars struct {
	pageVars
	Filter *labelFilter `json:"filter,omitempty"`
}

// Labels returns the labels a team's issues can carry (the team's own
// and the workspace's), or every label when teamID is empty, paged to
// the end.
func (c *Client) Labels(ctx context.Context, teamID string) ([]Label, error) {
	var filter *labelFilter
	if teamID != "" {
		yes := true
		filter = &labelFilter{Or: []labelTeamFilter{{Team: idEq(teamID)}, {Team: &entityFilter{Null: &yes}}}}
	}
	return collect(ctx, c, opLabels, func(after string) any {
		return labelsVars{pageVars{First: catalogPage, After: after}, filter}
	}, wireLabel.typed)
}

var opProjects = newQuery("Projects", "page", `query Projects($filter: ProjectFilter, $includeArchived: Boolean, $first: Int!, $after: String) {
  page: projects(filter: $filter, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...ProjectFields } pageInfo { hasNextPage endCursor }
  }
}
`+projectFields)

type projectFilter struct {
	AccessibleTeams struct {
		Some *entityFilter `json:"some"`
	} `json:"accessibleTeams"`
}

type projectsVars struct {
	pageVars
	Filter          *projectFilter `json:"filter,omitempty"`
	IncludeArchived bool           `json:"includeArchived,omitempty"`
}

// Projects returns the projects a team's issues can join, or every
// project when teamID is empty, paged to the end.
func (c *Client) Projects(ctx context.Context, teamID string, includeArchived bool) ([]Project, error) {
	var filter *projectFilter
	if teamID != "" {
		filter = &projectFilter{}
		filter.AccessibleTeams.Some = idEq(teamID)
	}
	return collect(ctx, c, opProjects, func(after string) any {
		return projectsVars{pageVars{First: catalogPage, After: after}, filter, includeArchived}
	}, wireProject.typed)
}
