package tracker

import (
	"context"
	"time"
)

// TimeTracker is a connection's logged time: Jira's own worklogs, or
// Tempo's when the Jira connection has a Tempo connection. Provider.Time
// returns it, or nil for a tracker with no time tracking (Linear).
// Writes are those the caller's posting path sends; each returns one
// StepWorklog part.
type TimeTracker interface {
	// Worklogs are one author's worklogs whose day is From to To, both
	// included.
	Worklogs(ctx context.Context, q WorklogQuery) ([]Worklog, error)
	AddWorklog(ctx context.Context, in WorklogInput) Result
	EditWorklog(ctx context.Context, in WorklogEdit) Result
	DeleteWorklog(ctx context.Context, in WorklogDelete) Result
}

// Worklog is time logged on an issue. Day is the calendar day the work
// started on in the author's own zone, which a timesheet groups by.
// Description is Markdown; a tracker that stores plain text stores it as
// written.
type Worklog struct {
	ID          WorklogID
	Issue       IssueID
	Author      UserID
	Day         Date
	Spent       time.Duration
	Description string
	Updated     time.Time
}

// WorklogQuery selects one author's worklogs by day.
type WorklogQuery struct {
	Author   UserID
	From, To Date
}

// Check refuses a query with no author, a missing day, or To before From.
func (q WorklogQuery) Check() error {
	if q.Author == "" || q.From.IsZero() || q.To.IsZero() {
		return invalidf("a worklog query needs an author and both days")
	}
	if q.To.Compare(q.From) < 0 {
		return invalidf("worklog days %s to %s run backwards", q.From, q.To)
	}
	return nil
}

// WorklogInput is a new worklog of the credential's own user.
type WorklogInput struct {
	Issue       IssueID
	Day         Date
	Spent       time.Duration
	Description string
}

// Check refuses a worklog with no issue or day, or a time spent that is
// not a positive whole number of seconds.
func (in WorklogInput) Check() error {
	if in.Issue == "" || in.Day.IsZero() {
		return invalidf("a worklog needs an issue and a day")
	}
	return checkSpent(in.Spent)
}

// WorklogEdit changes the members of a worklog that are set. From is the
// worklog as read when the edit was written: one that changed since is
// refused with ReasonStale. AcceptLoss is as on EditInput, for a
// description stored as rich text.
type WorklogEdit struct {
	From        Worklog
	Day         *Date
	Spent       *time.Duration
	Description *string
	AcceptLoss  bool
}

// Check refuses an edit of a worklog with no ID or issue, one that changes
// nothing, or one that sets no day or a time spent that is not a positive
// whole number of seconds.
func (in WorklogEdit) Check() error {
	if in.From.ID == "" || in.From.Issue == "" {
		return invalidf("a worklog edit needs the worklog as read")
	}
	if in.Day == nil && in.Spent == nil && in.Description == nil {
		return invalidf("a worklog edit changes nothing")
	}
	if in.Day != nil && in.Day.IsZero() {
		return invalidf("a worklog edit cannot clear the day")
	}
	if in.Spent != nil {
		return checkSpent(*in.Spent)
	}
	return nil
}

// WorklogDelete deletes a worklog, From as read.
type WorklogDelete struct {
	From Worklog
}

// Check refuses a delete of a worklog with no ID or issue.
func (in WorklogDelete) Check() error {
	if in.From.ID == "" || in.From.Issue == "" {
		return invalidf("a worklog delete needs the worklog as read")
	}
	return nil
}

func checkSpent(d time.Duration) error {
	if d <= 0 || d%time.Second != 0 {
		return invalidf("time spent %s is not a positive whole number of seconds", d)
	}
	return nil
}
