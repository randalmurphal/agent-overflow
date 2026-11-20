package linear

import "time"

// Page is one page of a connection, in the connection's order.
// EndCursor is passed as the next call's after while HasNextPage holds.
type Page[T any] struct {
	Items       []T
	EndCursor   string
	HasNextPage bool
	// Usage is what the answer reported about the rate limits.
	Usage Usage
	// Served is Linear's clock when it answered the page, from the
	// answer's Date header; zero when it carried none.
	Served time.Time
}

// User is a workspace member, or an app user.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	AvatarURL   string `json:"avatarUrl"`
	Active      bool   `json:"active"`
	Admin       bool   `json:"admin"`
	Guest       bool   `json:"guest"`
	App         bool   `json:"app"`
	IsMe        bool   `json:"isMe"`
}

// Organization is the workspace a key belongs to.
type Organization struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URLKey string `json:"urlKey"`
}

// Viewer is the key's user and its workspace.
type Viewer struct {
	User         User         `json:"viewer"`
	Organization Organization `json:"organization"`
}

// EstimateScale is a team's estimate scale
// (https://linear.app/docs/issue-property).
type EstimateScale string

const (
	EstimateNotUsed     EstimateScale = "notUsed"
	EstimateExponential EstimateScale = "exponential"
	EstimateFibonacci   EstimateScale = "fibonacci"
	EstimateLinear      EstimateScale = "linear"
	// EstimateTShirt stores XS through XL as 1, 2, 3, 5 and 8, with 13
	// and 21 on the extended scale.
	EstimateTShirt EstimateScale = "tShirt"
)

// Estimation is a team's estimate settings.
type Estimation struct {
	Scale     EstimateScale
	Extended  bool // the scale has its additional points
	AllowZero bool
	// Default is the estimate progress counts for an unestimated issue.
	Default float64
}

// Team is a team with the settings the issue views need.
type Team struct {
	ID          string
	Key         string
	Name        string
	Description string
	Timezone    string
	Estimation  Estimation
	// CyclesEnabled reports cycles; CycleWeeks is their length.
	CyclesEnabled bool
	CycleWeeks    float64
	ActiveCycleID string
	TriageEnabled bool
	// DefaultStateID is where a member's new issue starts;
	// TriageStateID where a non-member's does when triage is on.
	DefaultStateID string
	TriageStateID  string
	// SortOnStateChange is where an issue lands in its new status's
	// manual order: "first", "last" or "noAction".
	SortOnStateChange string
	ArchivedAt        *time.Time
	UpdatedAt         time.Time
}

// StateType is a workflow state's fixed type.
type StateType string

const (
	StateTriage    StateType = "triage"
	StateBacklog   StateType = "backlog"
	StateUnstarted StateType = "unstarted"
	StateStarted   StateType = "started"
	StateCompleted StateType = "completed"
	StateCanceled  StateType = "canceled"
	StateDuplicate StateType = "duplicate"
)

// WorkflowState is one status of a team's workflow. Position orders the
// states within their type, lower first.
type WorkflowState struct {
	ID          string
	Name        string
	Color       string
	Description string
	Type        StateType
	Position    float64
	TeamID      string
	ArchivedAt  *time.Time
}

// Cycle is one cycle of a team. Linear's per-day scope and completion
// histories are not read: they are team-wide and not a commitment
// snapshot, so nothing built on the client uses them.
type Cycle struct {
	ID          string
	Number      int
	Name        string
	Description string
	TeamID      string
	StartsAt    time.Time
	EndsAt      time.Time
	CompletedAt *time.Time
	ArchivedAt  *time.Time
	IsActive    bool
	IsFuture    bool
	IsPast      bool
	IsNext      bool
	IsPrevious  bool
	// Progress runs from 0 to 1 as Linear computes it.
	Progress float64
}

// Label is an issue label. TeamID is empty for a workspace label.
type Label struct {
	ID          string
	Name        string
	Color       string
	Description string
	IsGroup     bool
	ParentID    string
	TeamID      string
	ArchivedAt  *time.Time
}

// ProjectStatus is a project's status.
type ProjectStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Date is a calendar date, YYYY-MM-DD, without a time zone.
type Date string

// Project is a project.
type Project struct {
	ID          string
	Name        string
	URL         string
	Status      ProjectStatus
	LeadID      string
	StartDate   Date
	TargetDate  Date
	CompletedAt *time.Time
	CanceledAt  *time.Time
	ArchivedAt  *time.Time
	UpdatedAt   time.Time
}

// Priority is an issue's priority.
type Priority int

const (
	PriorityNone   Priority = 0
	PriorityUrgent Priority = 1
	PriorityHigh   Priority = 2
	PriorityMedium Priority = 3
	PriorityLow    Priority = 4
)

// Issue is an issue as lists, polls and writes carry it: everything but
// its description, which DescribedIssue adds. Related entities are named
// by ID; empty means none.
type Issue struct {
	ID string
	// Identifier, such as WEB-1, changes when the issue moves team.
	Identifier string
	Number     int
	Title      string
	URL        string
	BranchName string
	Priority   Priority
	// Estimate is nil when the issue is not estimated; zero is an
	// estimate.
	Estimate *float64
	// SortOrder orders manually sorted lists (board columns and the
	// backlog), PrioritySortOrder issues within a priority group, and
	// SubIssueSortOrder (nil for a top-level issue) a parent's
	// sub-issues; lower first.
	SortOrder         float64
	PrioritySortOrder float64
	SubIssueSortOrder *float64
	TeamID            string
	// TeamEstimateScale is the team's estimate scale as of the read, so
	// a list can tell an estimate the team counts without reading the
	// team.
	TeamEstimateScale EstimateScale
	StateID           string
	AssigneeID        string
	CreatorID         string
	CycleID           string
	ProjectID         string
	ParentID          string
	// LabelIDs are every label the issue carries. Labels names those
	// Linear listed with the issue, at most 250 and archived ones left
	// out; LabelsByID names the rest.
	LabelIDs    []string
	Labels      []LabelRef
	DueDate     Date
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	CanceledAt  *time.Time
	ArchivedAt  *time.Time
	// Trashed marks an archived issue awaiting permanent deletion.
	Trashed bool
}

// DescribedIssue is an issue with its Markdown description, as the reads
// of one issue carry it.
type DescribedIssue struct {
	Issue
	Description string
}

// LabelRef is a label as an issue names it.
type LabelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IssuePosition is an issue's place in the manual order, as the lean
// list ListIssuePositions reads it.
type IssuePosition struct {
	ID        string
	SortOrder float64
	// TeamID is the issue's team, for a caller that checks the issue is
	// still the team's.
	TeamID string
}

// IssueRef names a related issue.
type IssueRef struct {
	ID         string
	Identifier string
	Title      string
	StateID    string
}

// IssueDetail is one issue with what its view shows. Comments, children,
// attachments, history and its team's states hold their first page, and
// a More flag or HasNextPage reports that there are more: further
// comments come from IssueComments, history from IssueHistory, states
// from WorkflowStates and children from ListIssues with a Parent filter.
// Relations are complete.
type IssueDetail struct {
	DescribedIssue
	// ProjectName is the name of the issue's project; empty for none.
	ProjectName     string
	Parent          *IssueRef
	Children        []IssueRef
	MoreChildren    bool
	Comments        Page[Comment]
	Attachments     []Attachment
	MoreAttachments bool
	// Relations holds every relation of the issue in both directions,
	// each read to its end: Issue is the side the relation was created
	// from.
	Relations []Relation
	// History is the newest page of the issue's history, newest first.
	History Page[HistoryEntry]
	// States are the workflow states of the issue's team, archived ones
	// left out, in Linear's order; MoreStates reports a team with more
	// than the page holds.
	States     []WorkflowState
	MoreStates bool
}

// Comment is a comment on an issue. ParentID names the comment it
// replies to.
type Comment struct {
	ID         string
	IssueID    string
	ParentID   string
	UserID     string
	Body       string // Markdown
	URL        string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	EditedAt   *time.Time
	ResolvedAt *time.Time
}

// Attachment is a link recorded on an issue, such as a pull request.
type Attachment struct {
	ID         string
	IssueID    string
	Title      string
	Subtitle   string
	URL        string
	SourceType string
	CreatorID  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// RelationType is how two issues relate.
type RelationType string

const (
	RelationBlocks    RelationType = "blocks"
	RelationDuplicate RelationType = "duplicate"
	RelationRelated   RelationType = "related"
	// RelationSimilar is deprecated by Linear: it can be read but
	// CreateRelation refuses it.
	RelationSimilar RelationType = "similar"
)

// Relation is a relation between two issues: Issue blocks, duplicates,
// or relates to Related.
type Relation struct {
	ID      string
	Type    RelationType
	Issue   IssueRef
	Related IssueRef
}

// Change is a change of a related entity by ID; an empty side is none.
type Change struct{ From, To string }

// NumberChange is a change of a number; a nil side is no value.
type NumberChange struct{ From, To *float64 }

// HistoryEntry is one recorded change of an issue. Each field is set
// only when the entry records that change.
type HistoryEntry struct {
	ID        string
	CreatedAt time.Time
	// UpdatedAt is later than CreatedAt when Linear changed the entry
	// after writing it.
	UpdatedAt time.Time
	ActorID   string
	State     *Change
	Assignee  *Change
	Cycle     *Change
	Project   *Change
	Parent    *Change
	Team      *Change
	Estimate  *NumberChange
	Priority  *NumberChange
	// Title holds the old and new title.
	Title           *Change
	AddedLabelIDs   []string
	RemovedLabelIDs []string
	// Archived is true for an archive, false for an unarchive.
	Archived *bool
}

// StateSpan is a period an issue spent in one status. EndedAt is nil
// for the open span.
type StateSpan struct {
	ID        string
	StateID   string
	StartedAt time.Time
	EndedAt   *time.Time
}
