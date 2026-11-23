package trackerfake

import "time"

// LinearWorkspace is one Linear workspace: its people, teams, workflow
// states, cycles, labels, projects, issues and what hangs off them. It is
// both what a fixture seeds and what Snapshot returns, with every
// generated ID and default filled in and the key left out. IDs are
// lowercase UUIDs; a generated one is above every seeded one.
type LinearWorkspace struct {
	// URLKey is the workspace's URL key, which names it as a tenant.
	URLKey string `json:"urlKey"`
	Name   string `json:"name"`
	ID     string `json:"id,omitempty"`
	// APIKey is the personal API key, sent bare; Viewer is the user it
	// belongs to.
	APIKey string `json:"apiKey,omitempty"`
	Viewer string `json:"viewer"`

	Users       []LinearUser       `json:"users,omitempty"`
	Teams       []LinearTeam       `json:"teams,omitempty"`
	States      []LinearState      `json:"states,omitempty"`
	Cycles      []LinearCycle      `json:"cycles,omitempty"`
	Labels      []LinearLabel      `json:"labels,omitempty"`
	Projects    []LinearProject    `json:"projects,omitempty"`
	Issues      []LinearIssue      `json:"issues,omitempty"`
	Comments    []LinearComment    `json:"comments,omitempty"`
	Attachments []LinearAttachment `json:"attachments,omitempty"`
	Relations   []LinearRelation   `json:"relations,omitempty"`
}

// LinearUser is a workspace member; Disabled is a suspended one.
type LinearUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Admin       bool   `json:"admin,omitempty"`
	Guest       bool   `json:"guest,omitempty"`
	App         bool   `json:"app,omitempty"`
}

// LinearTeam is a team. Estimation is notUsed (the default),
// exponential, fibonacci, linear or tShirt. DefaultState is where a new
// issue starts; empty is the team's first backlog or unstarted state.
type LinearTeam struct {
	ID                 string    `json:"id"`
	Key                string    `json:"key"`
	Name               string    `json:"name"`
	Description        string    `json:"description,omitempty"`
	Timezone           string    `json:"timezone,omitempty"`
	Estimation         string    `json:"estimation,omitempty"`
	EstimationExtended bool      `json:"estimationExtended,omitempty"`
	EstimationZero     bool      `json:"estimationZero,omitempty"`
	DefaultEstimate    float64   `json:"defaultEstimate,omitempty"`
	CyclesEnabled      bool      `json:"cyclesEnabled,omitempty"`
	CycleWeeks         float64   `json:"cycleWeeks,omitempty"`
	TriageEnabled      bool      `json:"triageEnabled,omitempty"`
	DefaultState       string    `json:"defaultState,omitempty"`
	TriageState        string    `json:"triageState,omitempty"`
	SortOnStateChange  string    `json:"sortOnStateChange,omitempty"`
	Members            []string  `json:"members,omitempty"`
	ArchivedAt         time.Time `json:"archivedAt,omitzero"`
	UpdatedAt          time.Time `json:"updatedAt,omitzero"`
}

// LinearState is a workflow state of a team; Type is triage, backlog,
// unstarted, started, completed or canceled.
type LinearState struct {
	ID          string    `json:"id"`
	Team        string    `json:"team"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Color       string    `json:"color,omitempty"`
	Description string    `json:"description,omitempty"`
	Position    float64   `json:"position,omitempty"`
	ArchivedAt  time.Time `json:"archivedAt,omitzero"`
}

// LinearCycle is a cycle of a team. Whether it is active, future or
// past follows from its dates and the engine's clock.
type LinearCycle struct {
	ID          string    `json:"id"`
	Team        string    `json:"team"`
	Number      float64   `json:"number"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	StartsAt    time.Time `json:"startsAt"`
	EndsAt      time.Time `json:"endsAt"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
	ArchivedAt  time.Time `json:"archivedAt,omitzero"`
	// The histories are Linear's daily team-wide counts, as seeded.
	ScopeHistory               []float64 `json:"scopeHistory,omitempty"`
	CompletedScopeHistory      []float64 `json:"completedScopeHistory,omitempty"`
	InProgressScopeHistory     []float64 `json:"inProgressScopeHistory,omitempty"`
	IssueCountHistory          []float64 `json:"issueCountHistory,omitempty"`
	CompletedIssueCountHistory []float64 `json:"completedIssueCountHistory,omitempty"`
}

// LinearLabel is an issue label: a team's, or the workspace's when Team
// is empty.
type LinearLabel struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Color       string    `json:"color,omitempty"`
	Description string    `json:"description,omitempty"`
	IsGroup     bool      `json:"isGroup,omitempty"`
	Parent      string    `json:"parent,omitempty"`
	Team        string    `json:"team,omitempty"`
	ArchivedAt  time.Time `json:"archivedAt,omitzero"`
}

// LinearProject is a project the issues of Teams can join. StatusType
// is backlog, planned, started, paused, completed or canceled.
type LinearProject struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Teams       []string  `json:"teams"`
	StatusID    string    `json:"statusId,omitempty"`
	StatusName  string    `json:"statusName,omitempty"`
	StatusType  string    `json:"statusType,omitempty"`
	Lead        string    `json:"lead,omitempty"`
	StartDate   string    `json:"startDate,omitempty"`
	TargetDate  string    `json:"targetDate,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
	CanceledAt  time.Time `json:"canceledAt,omitzero"`
	ArchivedAt  time.Time `json:"archivedAt,omitzero"`
	UpdatedAt   time.Time `json:"updatedAt,omitzero"`
}

// LinearIssue is an issue. Its identifier is its team's key and Number.
// Priority is 0 (none) to 4 (low). History is its change history, oldest
// first; writes add to it as Linear does (see AGENTS.md).
type LinearIssue struct {
	ID                string    `json:"id"`
	Team              string    `json:"team"`
	Number            float64   `json:"number,omitempty"`
	Title             string    `json:"title"`
	Description       string    `json:"description,omitempty"`
	Priority          float64   `json:"priority,omitempty"`
	Estimate          *float64  `json:"estimate,omitempty"`
	SortOrder         float64   `json:"sortOrder,omitempty"`
	PrioritySortOrder float64   `json:"prioritySortOrder,omitempty"`
	SubIssueSortOrder *float64  `json:"subIssueSortOrder,omitempty"`
	State             string    `json:"state,omitempty"`
	Assignee          string    `json:"assignee,omitempty"`
	Creator           string    `json:"creator,omitempty"`
	Cycle             string    `json:"cycle,omitempty"`
	Project           string    `json:"project,omitempty"`
	Parent            string    `json:"parent,omitempty"`
	Labels            []string  `json:"labels,omitempty"`
	DueDate           string    `json:"dueDate,omitempty"`
	CreatedAt         time.Time `json:"createdAt,omitzero"`
	UpdatedAt         time.Time `json:"updatedAt,omitzero"`
	StartedAt         time.Time `json:"startedAt,omitzero"`
	CompletedAt       time.Time `json:"completedAt,omitzero"`
	CanceledAt        time.Time `json:"canceledAt,omitzero"`
	ArchivedAt        time.Time `json:"archivedAt,omitzero"`
	Trashed           bool      `json:"trashed,omitempty"`
	// FormerKeys are the identifiers the issue had in the teams it moved
	// from, oldest first; a move adds one. issue(id:) still answers one
	// and an identifier filter does not match it (unverified: inferred
	// from Linear's app, which redirects an old identifier).
	FormerKeys []string        `json:"formerKeys,omitempty"`
	History    []LinearHistory `json:"history,omitempty"`
	// ShownAt, when set, is when lists (issues, searchIssues) first show
	// the issue: before it they leave it out, as a search index that
	// lists an update late would. A read by ID shows it at once. This
	// models the lag tracker.DeltaOverlap allows for; how long Linear's
	// lists lag, if at all, is unverified.
	ShownAt time.Time `json:"shownAt,omitzero"`
}

// LinearHistory is one entry of an issue's history: the actor and each
// value that changed, before and after. Empty strings and nil are none.
type LinearHistory struct {
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"createdAt"`
	Actor         string    `json:"actor,omitempty"`
	FromState     string    `json:"fromState,omitempty"`
	ToState       string    `json:"toState,omitempty"`
	FromAssignee  string    `json:"fromAssignee,omitempty"`
	ToAssignee    string    `json:"toAssignee,omitempty"`
	FromCycle     string    `json:"fromCycle,omitempty"`
	ToCycle       string    `json:"toCycle,omitempty"`
	FromProject   string    `json:"fromProject,omitempty"`
	ToProject     string    `json:"toProject,omitempty"`
	FromParent    string    `json:"fromParent,omitempty"`
	ToParent      string    `json:"toParent,omitempty"`
	FromTeam      string    `json:"fromTeam,omitempty"`
	ToTeam        string    `json:"toTeam,omitempty"`
	FromEstimate  *float64  `json:"fromEstimate,omitempty"`
	ToEstimate    *float64  `json:"toEstimate,omitempty"`
	FromPriority  *float64  `json:"fromPriority,omitempty"`
	ToPriority    *float64  `json:"toPriority,omitempty"`
	FromTitle     string    `json:"fromTitle,omitempty"`
	ToTitle       string    `json:"toTitle,omitempty"`
	AddedLabels   []string  `json:"addedLabels,omitempty"`
	RemovedLabels []string  `json:"removedLabels,omitempty"`
	Archived      *bool     `json:"archived,omitempty"`
}

// LinearComment is a comment on an issue; Parent is the comment it
// answers.
type LinearComment struct {
	ID         string    `json:"id"`
	Issue      string    `json:"issue"`
	Parent     string    `json:"parent,omitempty"`
	User       string    `json:"user"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt,omitzero"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
	EditedAt   time.Time `json:"editedAt,omitzero"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`
}

// LinearAttachment is a link attached to an issue.
type LinearAttachment struct {
	ID         string    `json:"id"`
	Issue      string    `json:"issue"`
	Title      string    `json:"title"`
	Subtitle   string    `json:"subtitle,omitempty"`
	URL        string    `json:"url"`
	SourceType string    `json:"sourceType,omitempty"`
	Creator    string    `json:"creator,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitzero"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
}

// LinearRelation relates Issue to Related; Type is blocks, duplicate,
// related or similar.
type LinearRelation struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Issue   string `json:"issue"`
	Related string `json:"related"`
}
