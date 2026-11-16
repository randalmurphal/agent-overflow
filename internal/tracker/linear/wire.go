package linear

import "time"

// Each fragment below and the wire type after it are kept in step: the
// type has a field for each selection, and the schema test validates the
// fragment in every document that uses it.

const userFields = `fragment UserFields on User {
  id name displayName email avatarUrl active admin guest app isMe
}`

const teamFields = `fragment TeamFields on Team {
  id key name description timezone
  issueEstimationType issueEstimationExtended issueEstimationAllowZero defaultIssueEstimate
  cyclesEnabled cycleDuration activeCycle { id }
  triageEnabled defaultIssueState { id } triageIssueState { id }
  setIssueSortOrderOnStateChange archivedAt updatedAt
}`

// ref is a related entity selected as { id }.
type ref struct {
	ID string `json:"id"`
}

func (r *ref) id() string {
	if r == nil {
		return ""
	}
	return r.ID
}

type wireTeam struct {
	ID                       string     `json:"id"`
	Key                      string     `json:"key"`
	Name                     string     `json:"name"`
	Description              string     `json:"description"`
	Timezone                 string     `json:"timezone"`
	IssueEstimationType      string     `json:"issueEstimationType"`
	IssueEstimationExtended  bool       `json:"issueEstimationExtended"`
	IssueEstimationAllowZero bool       `json:"issueEstimationAllowZero"`
	DefaultIssueEstimate     float64    `json:"defaultIssueEstimate"`
	CyclesEnabled            bool       `json:"cyclesEnabled"`
	CycleDuration            float64    `json:"cycleDuration"`
	ActiveCycle              *ref       `json:"activeCycle"`
	TriageEnabled            bool       `json:"triageEnabled"`
	DefaultIssueState        *ref       `json:"defaultIssueState"`
	TriageIssueState         *ref       `json:"triageIssueState"`
	SortOnStateChange        string     `json:"setIssueSortOrderOnStateChange"`
	ArchivedAt               *time.Time `json:"archivedAt"`
	UpdatedAt                time.Time  `json:"updatedAt"`
}

func (w wireTeam) typed() Team {
	return Team{
		ID: w.ID, Key: w.Key, Name: w.Name, Description: w.Description, Timezone: w.Timezone,
		Estimation: Estimation{
			Scale: EstimateScale(w.IssueEstimationType), Extended: w.IssueEstimationExtended,
			AllowZero: w.IssueEstimationAllowZero, Default: w.DefaultIssueEstimate,
		},
		CyclesEnabled: w.CyclesEnabled, CycleWeeks: w.CycleDuration, ActiveCycleID: w.ActiveCycle.id(),
		TriageEnabled: w.TriageEnabled, DefaultStateID: w.DefaultIssueState.id(), TriageStateID: w.TriageIssueState.id(),
		SortOnStateChange: w.SortOnStateChange, ArchivedAt: w.ArchivedAt, UpdatedAt: w.UpdatedAt,
	}
}

const stateFields = `fragment StateFields on WorkflowState {
  id name color description type position team { id } archivedAt
}`

type wireState struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Color       string     `json:"color"`
	Description string     `json:"description"`
	Type        string     `json:"type"`
	Position    float64    `json:"position"`
	Team        *ref       `json:"team"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

func (w wireState) typed() WorkflowState {
	return WorkflowState{
		ID: w.ID, Name: w.Name, Color: w.Color, Description: w.Description, Type: StateType(w.Type),
		Position: w.Position, TeamID: w.Team.id(), ArchivedAt: w.ArchivedAt,
	}
}

const cycleFields = `fragment CycleFields on Cycle {
  id number name description team { id } startsAt endsAt completedAt archivedAt
  isActive isFuture isPast isNext isPrevious progress
  scopeHistory completedScopeHistory inProgressScopeHistory issueCountHistory completedIssueCountHistory
}`

type wireCycle struct {
	ID                         string     `json:"id"`
	Number                     float64    `json:"number"`
	Name                       string     `json:"name"`
	Description                string     `json:"description"`
	Team                       *ref       `json:"team"`
	StartsAt                   time.Time  `json:"startsAt"`
	EndsAt                     time.Time  `json:"endsAt"`
	CompletedAt                *time.Time `json:"completedAt"`
	ArchivedAt                 *time.Time `json:"archivedAt"`
	IsActive                   bool       `json:"isActive"`
	IsFuture                   bool       `json:"isFuture"`
	IsPast                     bool       `json:"isPast"`
	IsNext                     bool       `json:"isNext"`
	IsPrevious                 bool       `json:"isPrevious"`
	Progress                   float64    `json:"progress"`
	ScopeHistory               []float64  `json:"scopeHistory"`
	CompletedScopeHistory      []float64  `json:"completedScopeHistory"`
	InProgressScopeHistory     []float64  `json:"inProgressScopeHistory"`
	IssueCountHistory          []float64  `json:"issueCountHistory"`
	CompletedIssueCountHistory []float64  `json:"completedIssueCountHistory"`
}

func (w wireCycle) typed() Cycle {
	return Cycle{
		ID: w.ID, Number: int(w.Number), Name: w.Name, Description: w.Description, TeamID: w.Team.id(),
		StartsAt: w.StartsAt, EndsAt: w.EndsAt, CompletedAt: w.CompletedAt, ArchivedAt: w.ArchivedAt,
		IsActive: w.IsActive, IsFuture: w.IsFuture, IsPast: w.IsPast, IsNext: w.IsNext, IsPrevious: w.IsPrevious,
		Progress: w.Progress, ScopeHistory: w.ScopeHistory, CompletedScopeHistory: w.CompletedScopeHistory,
		InProgressScopeHistory: w.InProgressScopeHistory, IssueCountHistory: w.IssueCountHistory,
		CompletedIssueCountHistory: w.CompletedIssueCountHistory,
	}
}

const labelFields = `fragment LabelFields on IssueLabel {
  id name color description isGroup parent { id } team { id } archivedAt
}`

type wireLabel struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Color       string     `json:"color"`
	Description string     `json:"description"`
	IsGroup     bool       `json:"isGroup"`
	Parent      *ref       `json:"parent"`
	Team        *ref       `json:"team"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

func (w wireLabel) typed() Label {
	return Label{
		ID: w.ID, Name: w.Name, Color: w.Color, Description: w.Description, IsGroup: w.IsGroup,
		ParentID: w.Parent.id(), TeamID: w.Team.id(), ArchivedAt: w.ArchivedAt,
	}
}

const projectFields = `fragment ProjectFields on Project {
  id name url status { id name type } lead { id } startDate targetDate
  completedAt canceledAt archivedAt updatedAt
}`

type wireProject struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	URL         string        `json:"url"`
	Status      ProjectStatus `json:"status"`
	Lead        *ref          `json:"lead"`
	StartDate   Date          `json:"startDate"`
	TargetDate  Date          `json:"targetDate"`
	CompletedAt *time.Time    `json:"completedAt"`
	CanceledAt  *time.Time    `json:"canceledAt"`
	ArchivedAt  *time.Time    `json:"archivedAt"`
	UpdatedAt   time.Time     `json:"updatedAt"`
}

func (w wireProject) typed() Project {
	return Project{
		ID: w.ID, Name: w.Name, URL: w.URL, Status: w.Status, LeadID: w.Lead.id(),
		StartDate: w.StartDate, TargetDate: w.TargetDate, CompletedAt: w.CompletedAt,
		CanceledAt: w.CanceledAt, ArchivedAt: w.ArchivedAt, UpdatedAt: w.UpdatedAt,
	}
}

// pageInfo is a connection's paging state.
type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// connection is a page as a document selects it:
// { nodes { ... } pageInfo { hasNextPage endCursor } }.
type connection[W any] struct {
	Nodes    []W      `json:"nodes"`
	PageInfo pageInfo `json:"pageInfo"`
}

// page converts a connection with each node's typed form.
func page[W any, T any](c connection[W], usage Usage, typed func(W) T) Page[T] {
	items := make([]T, len(c.Nodes))
	for i, n := range c.Nodes {
		items[i] = typed(n)
	}
	return Page[T]{Items: items, EndCursor: c.PageInfo.EndCursor, HasNextPage: c.PageInfo.HasNextPage, Usage: usage}
}

func same[T any](v T) T { return v }
