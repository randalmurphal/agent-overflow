package app

import (
	"context"
	"errors"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// The writes to a tracker, one bound method per trackerapp write a screen
// makes, each on the computer that holds the connection. Each answers the
// write's outcome (trackerapp.WriteView): a part the tracker refused, or
// whose answer was lost, is in the view and not an error. Its 3 minute
// budget is four tracker requests at the 45 second bound the tracker
// client gives each; what a write and its refresh send is counted in
// docs/architecture/tracker-backend.md (Methods and events). A run cut
// there answers each part as applied, unknown or not sent. The worklog
// reads a Time screen makes beside its writes close the file.

// trackerWriteBudget is every write's //ao:budget, which the attach's
// bound also gives its refresh.
const trackerWriteBudget = 3 * time.Minute

// trackerWrite runs write, a write of step, on connectionID with in, its
// whole run bounded by method's budget.
func trackerWrite[T any](a *App, ctx context.Context, method string, step tracker.Step, connectionID string, in T,
	write func(*trackerapp.Service, context.Context, string, T) (trackerapp.WriteView, error)) (trackerapp.WriteView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.WriteView{}, err
	}
	ctx, cancel := a.trackerRun(ctx, method, "The change to the tracker", trackerapp.LostWriteCheck(step))
	defer cancel()
	out, err := write(svc, ctx, connectionID, in)
	return out, trackerWriteError(ctx, step, err)
}

// trackerWriteError is a write of step's error as its caller reads it:
// the budget's when ctx ended at it, a result the provider may not give
// with where to look (the write may have been made), and otherwise its
// refusal's sentence.
func trackerWriteError(ctx context.Context, step tracker.Step, err error) error {
	if cut := budgetError(ctx, err); cut != err {
		return cut
	}
	if errors.Is(err, trackerapp.ErrBadResult) {
		return errorsx.Public("tracker_write_unclear",
			"the tracker's answer to this change could not be read, so it may or may not have been made; "+trackerapp.LostWriteCheck(step), err)
	}
	return trackerError(err)
}

// CreateTrackerIssue makes an issue from a section of a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) CreateTrackerIssue(ctx context.Context, connectionID string, in trackerapp.CreateRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "CreateTrackerIssue", tracker.StepCreate, connectionID, in, (*trackerapp.Service).Create)
}

// EditTrackerIssue changes fields of an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) EditTrackerIssue(ctx context.Context, connectionID string, in trackerapp.EditRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "EditTrackerIssue", tracker.StepEdit, connectionID, in, (*trackerapp.Service).Edit)
}

// MoveTrackerIssue moves an issue to the status of one of its moves.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) MoveTrackerIssue(ctx context.Context, connectionID string, in trackerapp.MoveRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "MoveTrackerIssue", tracker.StepMove, connectionID, in, (*trackerapp.Service).Move)
}

// SetTrackerEstimate sets an issue's estimate on a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) SetTrackerEstimate(ctx context.Context, connectionID string, in trackerapp.EstimateRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "SetTrackerEstimate", tracker.StepEstimate, connectionID, in, (*trackerapp.Service).SetEstimate)
}

// RankTrackerIssues places issues between two neighbors on a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) RankTrackerIssues(ctx context.Context, connectionID string, in trackerapp.RankRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "RankTrackerIssues", tracker.StepRank, connectionID, in, (*trackerapp.Service).Rank)
}

// MoveTrackerIssuesToSprint moves issues into a sprint or to a board's
// backlog.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) MoveTrackerIssuesToSprint(ctx context.Context, connectionID string, in trackerapp.SprintMoveRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "MoveTrackerIssuesToSprint", tracker.StepSprintMove, connectionID, in, (*trackerapp.Service).MoveToSprint)
}

// CreateTrackerSprint makes a future sprint on a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) CreateTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "CreateTrackerSprint", tracker.StepSprint, connectionID, in, (*trackerapp.Service).CreateSprint)
}

// StartTrackerSprint starts a future sprint.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) StartTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintStartRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "StartTrackerSprint", tracker.StepSprint, connectionID, in, (*trackerapp.Service).StartSprint)
}

// CompleteTrackerSprint closes an active sprint, moving its unfinished
// issues where the request says.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) CompleteTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintCompleteRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "CompleteTrackerSprint", tracker.StepSprint, connectionID, in, (*trackerapp.Service).CompleteSprint)
}

// EditTrackerSprint changes a sprint's name, goal or dates.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) EditTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintEditRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "EditTrackerSprint", tracker.StepSprint, connectionID, in, (*trackerapp.Service).EditSprint)
}

// AddTrackerComment adds a comment to an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) AddTrackerComment(ctx context.Context, connectionID string, in trackerapp.CommentRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "AddTrackerComment", tracker.StepComment, connectionID, in, (*trackerapp.Service).AddComment)
}

// EditTrackerComment replaces a comment's body.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) EditTrackerComment(ctx context.Context, connectionID string, in trackerapp.CommentEditRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "EditTrackerComment", tracker.StepComment, connectionID, in, (*trackerapp.Service).EditComment)
}

// DeleteTrackerComment deletes a comment.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) DeleteTrackerComment(ctx context.Context, connectionID string, in trackerapp.CommentDeleteRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "DeleteTrackerComment", tracker.StepComment, connectionID, in, (*trackerapp.Service).DeleteComment)
}

// LinkTrackerIssues links two issues.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) LinkTrackerIssues(ctx context.Context, connectionID string, in trackerapp.LinkRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "LinkTrackerIssues", tracker.StepLink, connectionID, in, (*trackerapp.Service).Link)
}

// UnlinkTrackerIssues removes one link between two issues.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) UnlinkTrackerIssues(ctx context.Context, connectionID string, in trackerapp.UnlinkRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "UnlinkTrackerIssues", tracker.StepLink, connectionID, in, (*trackerapp.Service).Unlink)
}

// AddTrackerWorklog logs time on an issue, as the person's own action.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) AddTrackerWorklog(ctx context.Context, connectionID string, in trackerapp.WorklogRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "AddTrackerWorklog", tracker.StepWorklog, connectionID, in, (*trackerapp.Service).AddWorklog)
}

// EditTrackerWorklog changes a worklog, as read, on its issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) EditTrackerWorklog(ctx context.Context, connectionID string, in trackerapp.WorklogEditRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "EditTrackerWorklog", tracker.StepWorklog, connectionID, in, (*trackerapp.Service).EditWorklog)
}

// DeleteTrackerWorklog deletes a worklog, as read.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) DeleteTrackerWorklog(ctx context.Context, connectionID string, in trackerapp.WorklogDeleteRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "DeleteTrackerWorklog", tracker.StepWorklog, connectionID, in, (*trackerapp.Service).DeleteWorklog)
}

// TrackerWorklogs is an author's worklogs on a connection whose day is
// from to to (YYYY-MM-DD, both included, at most 366 days) as this
// computer last read them, with no tracker call, and the days of the
// range it has never read (unread).
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerWorklogs(ctx context.Context, connectionID, author, from, to string) (trackerapp.WorklogDays, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.WorklogDays{}, err
	}
	out, err := svc.Worklogs(connectionID, author, from, to)
	return out, trackerTimeError(ctx, err)
}

// RefreshTrackerWorklogs reads an author's worklogs of at most 31 days
// from the connection's time tracker, then answers them as
// TrackerWorklogs does. A read that failed is the answer's failure, with
// the worklogs as this computer held them before. It runs at the default
// budget, as a form read does. Its requests grow with the time logged:
// Tempo answers in pages, and Jira looks the author up (unless its client
// has the user cached), pages the issues the author logged time on with the
// worklogs each embeds (up to 20), and reads the worklogs of an issue
// with more in pages of their own. On jiraprovider's cost fixture,
// a month of 20 worklogs on 20 issues, the read is 1 Tempo request of
// about 11.8 KB (TestRequestCost) or 2 Jira requests of about 28.0 KB
// (TestRequestCostJiraWorklogs).
//
//ao:scope tracker:read
//ao:route home
func (a *App) RefreshTrackerWorklogs(ctx context.Context, connectionID, author, from, to string) (trackerapp.WorklogDays, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.WorklogDays{}, err
	}
	ctx, cancel := a.trackerRun(ctx, "RefreshTrackerWorklogs", "Reading the logged time", trackerSlow)
	defer cancel()
	out, err := svc.RefreshWorklogs(ctx, connectionID, author, from, to)
	return out, trackerTimeError(ctx, err)
}

// trackerTimeError is a worklog read's error with its sentence: the
// budget's when ctx ended at it, and a tracker with no time tracking.
func trackerTimeError(ctx context.Context, err error) error {
	if cut := budgetError(ctx, err); cut != err {
		return cut
	}
	if errors.Is(err, tracker.ErrUnsupported) {
		return errorsx.Public("tracker_unsupported", "this tracker has no time tracking", err)
	}
	return trackerError(err)
}
