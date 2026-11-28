package trackerfake

import (
	"encoding/json"
	"time"
)

// JiraSite is one Jira Cloud site: its people, workflow, fields,
// projects, boards, sprints and issues, and its Tempo account. It is
// both what a fixture seeds and what Snapshot returns, with every
// generated ID and default filled in and the tokens left out.
//
// IDs are Jira's numeric IDs as text (issue, comment, worklog, link,
// attachment IDs) or numbers (board and sprint IDs). A generated ID is
// above every seeded one and never repeats within an engine.
type JiraSite struct {
	// Site is the site's host, such as example.atlassian.net: the Origin
	// the site is served at.
	Site string `json:"site"`
	// Account is the account ID of the user the credential belongs to;
	// Token is the API token that user's email signs in with (Basic).
	Account string `json:"account"`
	Token   string `json:"token,omitempty"`
	// Members are the other people who sign in to the site, each with
	// their own token, which Snapshot leaves out.
	Members []JiraMember `json:"members,omitempty"`
	// CloudID is the site's cloud ID; empty seeds a made-up one.
	CloudID string `json:"cloudId,omitempty"`
	// Poker is the planning poker add-on; nil when the site has none.
	Poker *JiraPoker `json:"poker,omitempty"`

	Users      []JiraUser     `json:"users,omitempty"`
	Statuses   []JiraStatus   `json:"statuses,omitempty"`
	Priorities []JiraNamed    `json:"priorities,omitempty"`
	LinkTypes  []JiraLinkType `json:"linkTypes,omitempty"`
	// Resolutions are set on an issue moved to a done status; empty
	// seeds Jira's default "Done".
	Resolutions []JiraNamed `json:"resolutions,omitempty"`
	// Fields are the site's custom fields; the system fields are built in.
	Fields   []JiraField   `json:"fields,omitempty"`
	Projects []JiraProject `json:"projects,omitempty"`
	Boards   []JiraBoard   `json:"boards,omitempty"`
	Sprints  []JiraSprint  `json:"sprints,omitempty"`
	Issues   []JiraIssue   `json:"issues,omitempty"`
	// IndexLag is how long after a create searches and issue lists first
	// show the new issue (JiraIssue.Indexed); zero shows it at once.
	// Measured: 0.6 s, 0.7 to 1.6 s and 23 s in three runs. In JSON, a
	// number of nanoseconds.
	IndexLag time.Duration `json:"indexLag,omitempty"`
	Links    []JiraLink    `json:"links,omitempty"`
	// DeletedWorklogs feed worklog/deleted.
	DeletedWorklogs []JiraDeletion `json:"deletedWorklogs,omitempty"`

	Tempo *TempoAccount `json:"tempo,omitempty"`
}

// JiraUser is a Jira account.
type JiraUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	Inactive    bool   `json:"inactive,omitempty"`
	// AccountType is atlassian (the default), app or customer.
	AccountType string `json:"accountType,omitempty"`
	TimeZone    string `json:"timeZone,omitempty"`
	// HidesTimeZone is a profile whose privacy setting hides the time
	// zone: Jira answers it as null (documented: "Depending on the user's
	// privacy setting, this may be returned as null"). TimeZone still
	// places the user's Tempo days.
	HidesTimeZone bool `json:"hidesTimeZone,omitempty"`
	// OmitsAccountType is a user whose JSON names no accountType, as
	// Jira's published worklog example shows; the service, measured,
	// always names it.
	OmitsAccountType bool `json:"omitsAccountType,omitempty"`
}

// JiraStatus is a workflow status; Category is new, indeterminate or done.
type JiraStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// JiraNamed is a value named by ID: a priority, resolution or component.
type JiraNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// JiraLinkType is an issue link type.
type JiraLinkType struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Inward  string `json:"inward"`
	Outward string `json:"outward"`
}

// JiraSchema is a field's schema as the field catalog reports it.
type JiraSchema struct {
	Type     string `json:"type"`
	Items    string `json:"items,omitempty"`
	System   string `json:"system,omitempty"`
	Custom   string `json:"custom,omitempty"`
	CustomID int64  `json:"customId,omitempty"`
}

// JiraField is a custom field: its ID (customfield_<n>), name, schema and,
// for a select or cascading select, its options.
type JiraField struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Schema  JiraSchema   `json:"schema"`
	Options []JiraOption `json:"options,omitempty"`
}

// JiraOption is a select option; Children are a cascading select's.
type JiraOption struct {
	ID       string       `json:"id"`
	Value    string       `json:"value"`
	Disabled bool         `json:"disabled,omitempty"`
	Children []JiraOption `json:"children,omitempty"`
}

// JiraProject is a project with its issue types, their screens, and its
// workflow's transitions.
type JiraProject struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
	// TypeKey is software (the default), service_desk, business or
	// product_discovery. A service_desk project's comments are internal
	// or public.
	TypeKey    string `json:"typeKey,omitempty"`
	Simplified bool   `json:"simplified,omitempty"`
	// Statuses are the statuses the project's workflow uses; empty is
	// every site status.
	Statuses    []string         `json:"statuses,omitempty"`
	IssueTypes  []JiraIssueType  `json:"issueTypes"`
	Transitions []JiraTransition `json:"transitions,omitempty"`
	Components  []JiraNamed      `json:"components,omitempty"`
	// Assignable are the account IDs that can be assigned the project's
	// issues; empty is every active atlassian account.
	Assignable []string `json:"assignable,omitempty"`
}

// JiraIssueType is an issue type of a project. Screen is its create and
// edit screen: the fields a create may set (summary is always there and
// required) and an edit may change.
type JiraIssueType struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Subtask        bool              `json:"subtask,omitempty"`
	HierarchyLevel int               `json:"hierarchyLevel,omitempty"`
	Screen         []JiraScreenField `json:"screen,omitempty"`
}

// JiraScreenField is a field on a screen.
type JiraScreenField struct {
	Field      string `json:"field"`
	Required   bool   `json:"required,omitempty"`
	HasDefault bool   `json:"hasDefault,omitempty"`
}

// JiraTransition is a workflow move: from the statuses From (none for a
// global transition, available from every status) to To, taking the
// fields of its Screen.
type JiraTransition struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	To     string            `json:"to"`
	From   []string          `json:"from,omitempty"`
	Screen []JiraScreenField `json:"screen,omitempty"`
}

// JiraBoard is a Jira Software board, located in Project. Its saved
// filter selects the issues of Project and of Also, the other projects it
// names; it lists those whose status a column maps, ranked by the site's
// rank field. Estimation is the field it estimates in ("" estimates by
// issue count).
type JiraBoard struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Project    string       `json:"project"`
	Also       []string     `json:"also,omitempty"`
	Columns    []JiraColumn `json:"columns"`
	Estimation string       `json:"estimation,omitempty"`
	FilterID   string       `json:"filterId,omitempty"`
	// SubQuery is a kanban board's sub-filter, JQL in the forms the fake
	// reads; its list holds only the issues it matches (unverified).
	SubQuery string `json:"subQuery,omitempty"`
}

// JiraColumn is a board column and the statuses it maps.
type JiraColumn struct {
	Name     string   `json:"name"`
	Statuses []string `json:"statuses"`
	Min      int      `json:"min,omitempty"`
	Max      int      `json:"max,omitempty"`
}

// JiraSprint is a sprint of a board.
type JiraSprint struct {
	ID    int64 `json:"id"`
	Board int64 `json:"board"`
	// Shown are the other boards that list the sprint, as a board lists
	// a sprint of issues its filter also holds.
	Shown    []int64   `json:"shown,omitempty"`
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Goal     string    `json:"goal,omitempty"`
	Start    time.Time `json:"start,omitzero"`
	End      time.Time `json:"end,omitzero"`
	Complete time.Time `json:"complete,omitzero"`
}

// JiraIssue is an issue. Typed members hold the fields with behavior;
// Values holds every other field's value by field ID, as Jira reads it
// back. Sprints are every sprint the issue was or is in; Rank its value
// of the site's rank field, which orders every board.
type JiraIssue struct {
	ID  string `json:"id"`
	Key string `json:"key"`
	// FormerKeys are the keys the issue had before it moved to its
	// project. A read by one answers the issue under its key (measured
	// for GET issue, documented for the bulk fetch).
	FormerKeys  []string        `json:"formerKeys,omitempty"`
	Project     string          `json:"project"`
	Type        string          `json:"type"`
	Status      string          `json:"status"`
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	Reporter    string          `json:"reporter,omitempty"`
	Priority    string          `json:"priority,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	Components  []string        `json:"components,omitempty"`
	// Parent is the parent issue's ID.
	Parent     string    `json:"parent,omitempty"`
	DueDate    string    `json:"dueDate,omitempty"`
	Resolution string    `json:"resolution,omitempty"`
	Created    time.Time `json:"created,omitzero"`
	Updated    time.Time `json:"updated,omitzero"`
	// Indexed, when set, is when searches and issue lists first show the
	// issue: before it they leave it out, as an update Jira's index has
	// not taken yet. Reads by key show it all along. How long Jira's
	// index lags is unverified; a fixture picks the lag.
	Indexed  time.Time `json:"indexed,omitzero"`
	Resolved time.Time `json:"resolved,omitzero"`
	Sprints  []int64   `json:"sprints,omitempty"`
	Rank     string    `json:"rank,omitempty"`
	// OriginalEstimate and RemainingEstimate are in seconds; nil is none.
	OriginalEstimate  *int64                     `json:"originalEstimate,omitempty"`
	RemainingEstimate *int64                     `json:"remainingEstimate,omitempty"`
	Values            map[string]json.RawMessage `json:"values,omitempty"`
	Comments          []JiraComment              `json:"comments,omitempty"`
	Worklogs          []JiraWorklog              `json:"worklogs,omitempty"`
	// BulkError, when set, is the message a bulk fetch answers for the
	// issue in issueErrors in place of the issue, as Jira answers an
	// issue it could not read ("Jira is busy").
	BulkError   string           `json:"bulkError,omitempty"`
	Attachments []JiraAttachment `json:"attachments,omitempty"`
	RemoteLinks []JiraRemoteLink `json:"remoteLinks,omitempty"`
	Watchers    []string         `json:"watchers,omitempty"`
	// Changelog is the issue's history, oldest first; every write that
	// changes a field adds an entry.
	Changelog []JiraChange `json:"changelog,omitempty"`
}

// JiraComment is a comment; Internal marks a service desk comment only
// agents see.
type JiraComment struct {
	ID           string          `json:"id"`
	Author       string          `json:"author"`
	UpdateAuthor string          `json:"updateAuthor,omitempty"`
	Body         json.RawMessage `json:"body"`
	Created      time.Time       `json:"created,omitzero"`
	Updated      time.Time       `json:"updated,omitzero"`
	Internal     bool            `json:"internal,omitempty"`
	Visibility   json.RawMessage `json:"visibility,omitempty"`
}

// JiraWorklog is one of Jira's own worklogs.
type JiraWorklog struct {
	ID           string          `json:"id"`
	Author       string          `json:"author"`
	UpdateAuthor string          `json:"updateAuthor,omitempty"`
	Started      time.Time       `json:"started"`
	TimeSpent    int64           `json:"timeSpentSeconds"`
	Comment      json.RawMessage `json:"comment,omitempty"`
	Created      time.Time       `json:"created,omitzero"`
	Updated      time.Time       `json:"updated,omitzero"`
	Visibility   json.RawMessage `json:"visibility,omitempty"`
}

// JiraDeletion is a deleted worklog and when it went.
type JiraDeletion struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
}

// JiraAttachment is a file attached to an issue; MediaID is the media
// service's ID its content redirects to.
type JiraAttachment struct {
	ID       string    `json:"id"`
	Filename string    `json:"filename"`
	MimeType string    `json:"mimeType,omitempty"`
	Content  []byte    `json:"content"`
	Author   string    `json:"author,omitempty"`
	Created  time.Time `json:"created,omitzero"`
	MediaID  string    `json:"mediaId,omitempty"`
}

// JiraRemoteLink is a link from an issue to a page outside Jira.
type JiraRemoteLink struct {
	ID           string `json:"id"`
	GlobalID     string `json:"globalId,omitempty"`
	Relationship string `json:"relationship,omitempty"`
	URL          string `json:"url"`
	Title        string `json:"title"`
	Summary      string `json:"summary,omitempty"`
	Application  string `json:"application,omitempty"`
}

// JiraLink links two issues by their IDs: Outward relates to Inward by
// the type's outward phrase.
type JiraLink struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Outward string `json:"outward"`
	Inward  string `json:"inward"`
}

// JiraChange is one changelog entry.
type JiraChange struct {
	ID      string           `json:"id"`
	Author  string           `json:"author,omitempty"`
	Created time.Time        `json:"created"`
	Items   []JiraChangeItem `json:"items"`
}

// JiraChangeItem is one field's change: raw values (IDs, account IDs,
// numbers) in From and To, what a person reads in FromString and
// ToString; nil is none. FieldID is empty, and left out of the wire,
// for the items Jira writes without one (Link, IssueParentAssociation).
type JiraChangeItem struct {
	FieldID    string  `json:"fieldId,omitempty"`
	Field      string  `json:"field"`
	FieldType  string  `json:"fieldtype"`
	From       *string `json:"from"`
	To         *string `json:"to"`
	FromString *string `json:"fromString"`
	ToString   *string `json:"toString"`
}
