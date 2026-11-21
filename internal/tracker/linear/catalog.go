package linear

import (
	"context"
	"time"
)

// Page sizes of the catalog reads, each well under Linear's 10,000-point
// cap per query: a node costs a point for each object it selects, so a
// team about 4, a state 2, a user 1, a label 3 and a cycle 2 (fitted to
// labels by ID and teams by key, measured). Linear charges a catalog
// connection by its page size, not by what it returns: a cycles read at
// 50 that returned nothing cost 441 points, when it also selected the
// cycles' scope histories (measured). A KeptSince read selects the active cycle, the upcoming
// ones and those inside the window, so it takes a smaller page; a longer
// list pages on.
const (
	catalogPage   = 100
	cyclePage     = 50
	keptCyclePage = 20
)

var opViewer = newQuery("Viewer", "viewer", `query Viewer {
  viewer { ...UserFields }
  organization { id name urlKey }
}
`+userFields)

// Viewer returns the key's user and its workspace. A bound client fails
// with a *WrongWorkspaceError when the answer names another workspace.
func (c *Client) Viewer(ctx context.Context) (*Viewer, error) {
	var v Viewer
	if _, err := c.call(ctx, opViewer, nil, &v); err != nil {
		return nil, err
	}
	if c.bound != nil && !sameWorkspace(v.Organization.URLKey, c.bound.want) {
		return nil, &WrongWorkspaceError{Want: c.bound.want, Got: v.Organization.URLKey}
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
	return collect(ctx, c, opWorkflowStates, statesPage(teamID), wireState.typed)
}

// statesPage builds a page of a team's states from its cursor.
func statesPage(teamID string) func(after string) any {
	return func(after string) any { return teamPageVars{pageVars{First: catalogPage, After: after}, teamID} }
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
	// KeptSince, when set, selects only the cycles that are active or
	// future or that ended or completed at or after it, so a read of the
	// cycles a window keeps does not grow with the team's age.
	KeptSince time.Time
}

var opCycles = newQuery("Cycles", "page", `query Cycles($filter: CycleFilter!, $includeArchived: Boolean, $first: Int!, $after: String) {
  page: cycles(filter: $filter, includeArchived: $includeArchived, first: $first, after: $after) {
    nodes { ...CycleFields } pageInfo { hasNextPage endCursor }
  }
}
`+cycleFields)

type cycleFilter struct {
	Team        *entityFilter   `json:"team,omitempty"`
	IsActive    *boolEq         `json:"isActive,omitempty"`
	IsFuture    *boolEq         `json:"isFuture,omitempty"`
	IsPast      *boolEq         `json:"isPast,omitempty"`
	EndsAt      *dateComparator `json:"endsAt,omitempty"`
	CompletedAt *dateComparator `json:"completedAt,omitempty"`
	Or          []cycleFilter   `json:"or,omitempty"`
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
	first, err := q.vars("Cycles")
	if err != nil {
		return nil, err
	}
	return collect(ctx, c, opCycles, first.after, wireCycle.typed)
}

// after is v for the page after cursor.
func (v cycleVars) after(cursor string) any {
	v.After = cursor
	return v
}

// vars are the variables of the first page of opCycles that q selects,
// or fail as op when q names no team or an unknown When.
func (q CycleQuery) vars(op string) (cycleVars, error) {
	if q.TeamID == "" {
		return cycleVars{}, invalid(op, "a team ID is required")
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
		return cycleVars{}, invalid(op, "unknown CycleWhen")
	}
	size := cyclePage
	if !q.KeptSince.IsZero() {
		since := &dateComparator{Gte: formatTime(q.KeptSince)}
		filter.Or = []cycleFilter{{IsActive: &boolEq{true}}, {IsFuture: &boolEq{true}}, {EndsAt: since}, {CompletedAt: since}}
		size = keptCyclePage
	}
	return cycleVars{pageVars{First: size}, filter, q.IncludeArchived}, nil
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
	return collect(ctx, c, opUsers, usersPage(includeDisabled), same[User])
}

// usersPage builds a page of the workspace's users from its cursor.
func usersPage(includeDisabled bool) func(after string) any {
	return func(after string) any { return usersVars{pageVars{First: catalogPage, After: after}, includeDisabled} }
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
