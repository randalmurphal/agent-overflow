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

// DeltaOverlap bounds how late a tracker may first show an update: a
// search index that lags, or a write that commits after a later one. A
// delta reads again every row updated within it before a walk began.
const DeltaOverlap = time.Minute

// DeltaResume decides where the delta walk after a finished one starts.
// newest is the update time of the newest row delivered so far, and
// walkStart the tracker's own clock when the finished walk read its first
// page, zero when the tracker gave none. Both are the tracker's times, so
// this computer's clock has no part in it.
//
// Every update made before walkStart-DeltaOverlap was visible to the whole
// walk. When newest is no later than that, exact is true: the next walk
// resumes exactly after the last row delivered, so a board nobody changed
// delivers no row. Otherwise the next walk starts at restart, inclusive,
// and delivers again the rows updated since. With no walkStart that is
// DeltaOverlap before newest after every walk that delivered a row.
func DeltaResume(newest, walkStart time.Time) (restart time.Time, exact bool) {
	if walkStart.IsZero() {
		return newest.Add(-DeltaOverlap), false
	}
	visible := walkStart.Add(-DeltaOverlap)
	if !newest.After(visible) {
		return newest, true
	}
	return visible, false
}

// RowPage is one page of a board's changed rows, oldest update first,
// with the users and statuses they name. Rows may repeat a row an earlier
// page delivered, within DeltaOverlap of a change; a walk of a board
// nobody changed since delivers none. Next is the cursor to store with the
// page; More reports that another page follows now, and when it is false
// Next is where the next tick's walk starts.
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

// Placement is where a just-created issue stands on a set of boards, by
// the tracker's own word: its row on each board that holds it, the
// boards that do not, and the boards the tracker could not judge now,
// each with why. Every board asked is in exactly one of the three.
type Placement struct {
	Rows    []BoardRow
	Off     []BoardID
	Unknown []UnjudgedBoard
	Catalog
}

// BoardRow is an issue's row on Board.
type BoardRow struct {
	Board BoardID
	Row   Row
}

// UnjudgedBoard is a board a Placement could not judge, and why.
type UnjudgedBoard struct {
	Board BoardID
	Err   error
}

// CheckNewRows refuses a NewRows call that names no issue, no board, or
// a board twice.
func CheckNewRows(issue IssueID, boards []BoardID) error {
	if issue == "" || len(boards) == 0 {
		return invalidf("new rows need an issue and at least one board")
	}
	seen := make(map[BoardID]bool, len(boards))
	for _, b := range boards {
		if b == "" || seen[b] {
			return invalidf("board %q is empty or named twice", b)
		}
		seen[b] = true
	}
	return nil
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
// They are final through CompleteTo, by the tracker's own clock: a tracker
// may record a change some time after it is made, so a change made after
// CompleteTo may be missing. A provider takes it as its
// Capabilities.HistorySettle before the tracker's time on the answer that
// carried them, and leaves it zero when the answer named no time: this
// computer's clock never stands in for the tracker's. A caller that needs
// a read final for an update without it reads once more, at least
// HistorySettle after that answer arrived.
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
