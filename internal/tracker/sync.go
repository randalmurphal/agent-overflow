package tracker

import "time"

// Cursor is where a board's delta resumes, in the provider's own bytes.
// The caller stores what a page returns, in the same transaction as that
// page's rows, and hands it back; it never builds or adjusts one. A
// cursor resumes after the page that returned it, inside a run of issues
// sharing one update time included; overlap for the tracker's time
// resolution is the provider's business.
type Cursor []byte

// DeltaQuery asks for the next page of a board's changed rows.
type DeltaQuery struct {
	Board BoardID
	// From is the cursor the last stored page returned; nil starts a walk
	// at Start.
	From Cursor
	// Start is where a walk with no cursor begins: the rows of issues
	// updated at or after it. Ignored when From is set.
	Start time.Time
}

// RowPage is one page of a board's changed rows, oldest update first,
// with the users and statuses they name. Rows may repeat a row an earlier
// page delivered. Next is the cursor to store with the page; More reports
// that another page follows now, and when it is false Next is where the
// next tick's walk starts.
type RowPage struct {
	Rows []Row
	Next Cursor
	More bool
	Catalog
}

// Listing is the IDs of the issues on a board within a history window:
// those not done, those done at or after the window's start, and those in
// a sprint that is open or closed at or after it. Complete is false when
// the tracker could not list them all; the caller then adds what it
// lists and removes nothing.
type Listing struct {
	IDs      []IssueID
	Complete bool
}

// Rows are a board's rows for a set of issues. Missing are the issues
// asked for that the board does not list now: moved off it, deleted, or
// not visible.
type Rows struct {
	Rows    []Row
	Missing []IssueID
	Catalog
}

// Cards are the cards of a set of issues, read with no board. Missing are
// the issues asked for that the tracker says do not exist or are not
// visible.
type Cards struct {
	Cards   []Card
	Missing []IssueID
	Catalog
}

// HistoryQuery asks for the status, sprint, estimate and assignee changes
// of a set of issues. Boards are the boards whose estimates the estimate
// changes are read for, since a Jira board names its own estimate field.
type HistoryQuery struct {
	Issues []IssueID
	Boards []BoardID
}

// History is the changes of the issues a HistoryQuery named, one
// IssueHistory for each that exists and is visible, with the users their
// changes name.
type History struct {
	Issues []IssueHistory
	Catalog
}

// IssueHistory is one issue's status, sprint, estimate and assignee
// changes, each list oldest first.
//
// They are complete from CompleteFrom, zero meaning from the issue's
// creation: a value before that time is unknown, never inferred from the
// absence of a change.
//
// They are final through CompleteTo, which a provider always sets: a
// tracker may record a change some time after it is made, so a change made
// after CompleteTo may be missing. A caller reads an issue's history again
// while the issue's update time is later than the CompleteTo it holds.
type IssueHistory struct {
	Issue        IssueID
	CompleteFrom time.Time
	CompleteTo   time.Time
	Status       []StatusChange
	Sprint       []SprintChange
	Estimate     []EstimateChange
	Assignee     []AssigneeChange
}

// Change is when a change was made and who made it; Author is empty for a
// change the tracker made itself.
type Change struct {
	At     time.Time
	Author UserID
}

// StatusChange moves an issue between statuses.
type StatusChange struct {
	Change
	From, To StatusID
}

// SprintChange changes the set of sprints an issue is in; each side is
// the whole set, sorted, empty for none.
type SprintChange struct {
	Change
	From, To []SprintID
}

// EstimateChange changes an issue's estimate on Board, in that board's
// unit; nil is no estimate.
type EstimateChange struct {
	Change
	Board    BoardID
	From, To *float64
}

// AssigneeChange changes an issue's assignee; empty is unassigned.
type AssigneeChange struct {
	Change
	From, To UserID
}
