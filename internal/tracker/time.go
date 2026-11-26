package tracker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TimeTracker is a connection's logged time: Jira's own worklogs, or
// Tempo's when the Jira connection has a Tempo connection. Provider.Time
// returns it, or nil for a tracker with no time tracking (Linear).
// Writes are those the caller's posting path sends; each returns one
// StepWorklog part per worklog.
type TimeTracker interface {
	// Worklogs are one author's worklogs whose day is From to To, both
	// included.
	Worklogs(ctx context.Context, q WorklogQuery) ([]Worklog, error)
	// AddWorklogs logs each of ins, in order, as the credential's own
	// user: one part each, in ins's order, an applied one naming the
	// worklog it made (Part.Worklog). The checks and reads the batch
	// needs run once for it, and one input refused does not stop the
	// others. Inputs that CheckWorklogInputs refuses are refused whole,
	// with nothing sent.
	AddWorklogs(ctx context.Context, ins []WorklogInput) Result
	// EditWorklog and DeleteWorklog act on a worklog that still holds
	// what From says (issue, author, day, time and description) at the
	// tracker revision From carries (Updated); one that changed since it
	// was read, in a field From shows or not, is refused as stale with
	// nothing sent.
	EditWorklog(ctx context.Context, in WorklogEdit) Result
	DeleteWorklog(ctx context.Context, in WorklogDelete) Result
}

// ErrWorklogAuthor is matched by the error of a worklog read whose
// author the tracker would not look up, beside the tracker's own error:
// the read names the person, not the days.
var ErrWorklogAuthor = errors.New("tracker: the tracker did not look up the worklogs' author")

// ErrWorklogSearch is matched by the error of a worklog read whose search
// for the issues the author logged time on the tracker refused: the
// search names the person and the days.
var ErrWorklogSearch = errors.New("tracker: the tracker refused the search for the worklogs' issues")

// ErrNoTimeZone is matched by the error of a worklog read or write whose
// person's profile hides the time zone, which decides the day a worklog
// falls on: the tracker answered, and the person's privacy setting, not
// the connection, stops the read.
var ErrNoTimeZone = errors.New("tracker: the tracker does not show the person's time zone")

// Worklog is time logged on an issue. Day is the calendar day the work
// started on in the author's own zone, which a timesheet groups by.
// Description is Markdown; a tracker that stores plain text stores it as
// written. IssueKey and IssueTitle name the issue as the read found it,
// both empty when the tracker did not return the issue (one the user may
// no longer see); a write ignores them.
type Worklog struct {
	ID          WorklogID
	Issue       IssueID
	IssueKey    string
	IssueTitle  string
	Author      UserID
	Day         Date
	Spent       time.Duration
	Description string
	Updated     time.Time
}

// ErrWorklogNames is matched by the error of a worklog read whose request
// for its issues' keys and titles failed: a Jira connection's Tempo
// worklogs are named by Jira, not by Tempo.
var ErrWorklogNames = errors.New("tracker: the tracker did not name the worklogs' issues")

// ErrWorklogJira is matched by the failure of a request that a Jira
// connection's Tempo time makes to Jira: naming the worklogs' issues
// (ErrWorklogNames matches it too) and reading who logs the time. Every
// other failure of that time is Tempo's.
var ErrWorklogJira = errors.New("tracker: Jira, not Tempo, failed")

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

// MaxWorklogInputs is the most worklogs one AddWorklogs logs.
const MaxWorklogInputs = 250

// CheckWorklogInputs refuses a batch with no input, more than
// MaxWorklogInputs, or an input its Check refuses, naming its place.
func CheckWorklogInputs(ins []WorklogInput) error {
	if len(ins) == 0 || len(ins) > MaxWorklogInputs {
		return invalidf("a worklog batch holds 1 to %d worklogs, not %d", MaxWorklogInputs, len(ins))
	}
	for i, in := range ins {
		if err := in.Check(); err != nil {
			return fmt.Errorf("worklog %d: %w", i, err)
		}
	}
	return nil
}

// InvalidWorklogs is the result of a batch refused whole by its checks
// before anything was sent: one part per input, each about its issue.
func InvalidWorklogs(ins []WorklogInput, err error) Result {
	issues := make([]IssueID, len(ins))
	for i, in := range ins {
		issues[i] = in.Issue
	}
	return Invalid(StepWorklog, err, issues...)
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
