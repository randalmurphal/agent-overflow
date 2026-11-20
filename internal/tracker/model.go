// Package tracker is the tracker-neutral model the Issues pane draws and
// the local mirror stores, and the Provider interface one connection's
// tracker implements over its client. Jira and Linear both shape it: a
// concept only one tracker has is a capability, not a field that is empty
// for the other. The design is docs/architecture/tracker-backend.md.
//
// The package performs no I/O and imports no tracker client. It owns the
// neutral types, the write result with its per-part outcomes and the
// resources a write may have changed, the order key encoding a numeric
// order needs, and the input checks every provider shares. Providers live
// in tracker/jiraprovider and tracker/linearprovider; the application
// service that calls them is internal/trackerapp.
package tracker

import "time"

// IDs are the tracker's own identifiers as text, unique within one
// connection. A key such as WEB-1 is display and lookup text and never
// one of these: Jira changes a key when an issue moves project. Each kind
// of ID is its own type so one cannot be passed where another is wanted.
type (
	IssueID      string
	ProjectID    string
	BoardID      string
	SprintID     string
	StatusID     string
	UserID       string
	FieldID      string
	IssueTypeID  string
	CommentID    string
	AttachmentID string
	LinkID       string
	LinkTypeID   string
	MoveID       string
	WorklogID    string
)

// User is a person or an app account of the tracker.
type User struct {
	ID        UserID
	Name      string
	Email     string // empty when the tracker hides it
	AvatarURL string
	Active    bool
}

// Catalog is what a read learned about the users and statuses its
// records name, from a tracker that sends them with the records (Jira
// does; Linear names them by ID only, and Structure lists them). The
// mirror stores them beside the records.
type Catalog struct {
	Users    []User
	Statuses []Status
}

// Project is a tracker project: a Jira project or a Linear team.
type Project struct {
	ID   ProjectID
	Key  string
	Name string
	// ChoosesAudience reports a project whose new comments must say who
	// they reach (a Jira Service Management project, where a public
	// comment is emailed to the customer). A comment there with
	// AudienceUnset is refused.
	ChoosesAudience bool
}

// IssueType is an issue's type. The zero IssueType is no type, which is
// every issue of a tracker without Capabilities.IssueTypes.
type IssueType struct {
	ID   IssueTypeID
	Name string
	// Level is the type's place in the hierarchy: -1 a subtask, 0 a
	// standard issue, 1 an epic.
	Level int
}

// StatusGroup is the fixed group a status counts in. The provider maps the
// tracker's status category or state type to it; project setup may
// override a status's group.
type StatusGroup uint8

const (
	GroupBacklog StatusGroup = iota + 1
	GroupToDo
	GroupInProgress
	GroupDone
	GroupCanceled
)

// Status is a workflow status: a Jira status or a Linear workflow state.
type Status struct {
	ID    StatusID
	Name  string
	Group StatusGroup
}

// Choice is one value a field takes, or a value as read: an option, a
// priority, a label or a user, by ID with its display name.
type Choice struct {
	ID   string
	Name string
}

// Board is a board: a Jira board, or a Linear team's issues (one board per
// team, whose ID is the team's).
type Board struct {
	ID BoardID
	// Project is the project the board belongs to; empty when the tracker
	// does not say (a Jira board in a user's location).
	Project ProjectID
	Name    string
	// Sprints reports a board planned in sprints: a Jira scrum board, or a
	// Linear team with cycles on.
	Sprints bool
}

// Column is a board column and the statuses that put an issue in it, in
// board order. A Linear board has one column per workflow state.
type Column struct {
	Name     string
	Statuses []StatusID
}

// EstimateUnit is what a board's estimates count.
type EstimateUnit uint8

const (
	// UnitNone is a board with no estimate: it counts issues.
	UnitNone EstimateUnit = iota
	// UnitPoints is a number field, such as story points or Linear's
	// estimate.
	UnitPoints
	// UnitTime is an original time estimate, in seconds.
	UnitTime
)

// Estimate is a board's estimate: its unit, and the values a person picks
// from when the tracker fixes them (a Linear team's scale). No Choices
// means any number of the unit.
type Estimate struct {
	Unit    EstimateUnit
	Choices []EstimateChoice
}

// EstimateChoice is one value of a fixed estimate scale and its label
// ("M" for 3 on a T-shirt scale, "3" elsewhere).
type EstimateChoice struct {
	Value float64
	Label string
}

// SprintState is where a sprint is in its life.
type SprintState uint8

const (
	SprintFuture SprintState = iota + 1
	SprintActive
	SprintClosed
)

// Sprint is a Jira sprint or a Linear cycle. A time is zero when the
// sprint has not reached it.
type Sprint struct {
	ID        SprintID
	Board     BoardID
	Name      string
	Goal      string
	State     SprintState
	Start     time.Time
	End       time.Time
	Completed time.Time
}

// Structure is a board's structure as one read returns it: the board,
// its columns, its estimate, its sprints that are active or future or
// closed at or after the window the read named, and the users and
// statuses its issues can name (Catalog). A tracker that sends users with
// its records lists in Catalog only the users it lists for the board.
type Structure struct {
	Board    Board
	Columns  []Column
	Estimate Estimate
	Sprints  []Sprint
	Catalog
}

// Card is an issue as a board or backlog row draws it, the same on every
// board. Related things are named by ID; Catalog and Structure name them.
type Card struct {
	ID      IssueID
	Key     string
	Project ProjectID
	Type    IssueType
	Title   string
	Status  StatusID
	// Assignee is empty for an unassigned issue.
	Assignee UserID
	Priority Choice
	Labels   []Choice
	// Parent is empty for a top-level issue.
	Parent IssueID
	// Sprints are the sprints the card read names: Jira every sprint the
	// issue is or was in, Linear the cycle it is in now. Sprint changes in
	// History complete the set.
	Sprints  []SprintID
	Created  time.Time
	Updated  time.Time
	Resolved time.Time // zero when the issue is not resolved
}

// Row is an issue on one board: its card with that board's order key and
// estimate.
type Row struct {
	Card  Card
	Order OrderKey
	// Estimate is in the board's unit; nil when the issue has none or the
	// board has no estimate.
	Estimate *float64
}

// IssueRef is another issue as an issue names it: a linked issue.
type IssueRef struct {
	ID     IssueID
	Key    string
	Title  string
	Status StatusID
}

// LinkType is a kind of link between two issues: the outward issue
// relates to the inward one by Outward ("blocks"), the inward to the
// outward by Inward ("is blocked by").
type LinkType struct {
	ID      LinkTypeID
	Name    string
	Outward string
	Inward  string
}

// Link is a link from the issue that holds it to Issue. Outward reports
// that the holding issue is the outward side.
type Link struct {
	ID      LinkID
	Type    LinkType
	Outward bool
	Issue   IssueRef
}

// Attachment is a file attached to an issue.
type Attachment struct {
	ID       AttachmentID
	Name     string
	MimeType string
	Size     int64
	Created  time.Time
	Author   UserID
}

// Comment is a comment on an issue, its body as Markdown.
type Comment struct {
	ID      CommentID
	Author  UserID
	Body    string
	Created time.Time
	Updated time.Time
	// Internal reports a comment only the tracker's agents see (a Jira
	// Service Management internal note).
	Internal bool
}

// PageToken is a provider's own position in a list of pages. The caller
// passes back the token a page returned; empty asks for the first page.
type PageToken string

// CommentPage is one page of an issue's comments, newest first, and the
// users they name. Next is empty on the last page.
type CommentPage struct {
	Comments []Comment
	Next     PageToken
	Catalog
}

// Move is a move an issue's workflow allows now: a Jira transition, or a
// Linear workflow state. Fields are the fields the move takes; those it
// requires are marked Required.
type Move struct {
	ID     MoveID
	Name   string
	To     StatusID
	Fields []Field
}

// Event is one recorded change of an issue as the ticket's history shows
// it: who changed which field when, from what to what, as a person reads
// it.
type Event struct {
	At     time.Time
	Author UserID // empty for a change the tracker made itself
	Field  string
	From   string
	To     string
}

// Detail is one issue as its ticket shows it. Fields are every field the
// issue shows other than the card's and the description, with their
// metadata; Comments is the first page; History is complete from
// HistoryFrom (zero: from creation), and an earlier value is unknown.
type Detail struct {
	Card        Card
	URL         string
	Description string // Markdown
	Fields      []Field
	Comments    CommentPage
	Links       []Link
	Attachments []Attachment
	History     []Event
	HistoryFrom time.Time
	Moves       []Move
	Catalog
}

// Details are the details of several issues read together (Provider.Details):
// one Detail for each issue the tracker returned, in no order, and the
// issues it did not return, deleted or not visible to the user, in Missing.
type Details struct {
	Details []Detail
	Missing []IssueID
}
