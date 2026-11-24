package app

import (
	"context"

	"agent-overflow/internal/trackerapp"
)

// The writes to a tracker, one bound method per trackerapp write a screen
// makes, each on the computer that holds the connection. Each answers the
// write's outcome (trackerapp.WriteView): a part the tracker refused, or
// whose answer was lost, is in the view and not an error. Its 3 minute
// budget is four tracker requests at the 45 second bound the tracker
// client gives each; what a write and its refresh send is counted in
// docs/architecture/tracker-backend.md (Methods and events). A run cut
// there answers each part as applied, unknown or not sent.

// trackerWrite runs write on connectionID with in, its whole run bounded
// by method's budget.
func trackerWrite[T any](a *App, ctx context.Context, method, connectionID string, in T,
	write func(*trackerapp.Service, context.Context, string, T) (trackerapp.WriteView, error)) (trackerapp.WriteView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.WriteView{}, err
	}
	ctx, cancel := a.trackerRun(ctx, method, "The change to the tracker", "look at the ticket before you try again")
	defer cancel()
	out, err := write(svc, ctx, connectionID, in)
	return out, trackerRunError(ctx, err)
}

// CreateTrackerIssue makes an issue from a section of a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) CreateTrackerIssue(ctx context.Context, connectionID string, in trackerapp.CreateRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "CreateTrackerIssue", connectionID, in, (*trackerapp.Service).Create)
}

// EditTrackerIssue changes fields of an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) EditTrackerIssue(ctx context.Context, connectionID string, in trackerapp.EditRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "EditTrackerIssue", connectionID, in, (*trackerapp.Service).Edit)
}

// MoveTrackerIssue moves an issue to the status of one of its moves.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) MoveTrackerIssue(ctx context.Context, connectionID string, in trackerapp.MoveRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "MoveTrackerIssue", connectionID, in, (*trackerapp.Service).Move)
}

// SetTrackerEstimate sets an issue's estimate on a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) SetTrackerEstimate(ctx context.Context, connectionID string, in trackerapp.EstimateRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "SetTrackerEstimate", connectionID, in, (*trackerapp.Service).SetEstimate)
}

// RankTrackerIssues places issues between two neighbors on a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) RankTrackerIssues(ctx context.Context, connectionID string, in trackerapp.RankRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "RankTrackerIssues", connectionID, in, (*trackerapp.Service).Rank)
}

// MoveTrackerIssuesToSprint moves issues into a sprint or to a board's
// backlog.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) MoveTrackerIssuesToSprint(ctx context.Context, connectionID string, in trackerapp.SprintMoveRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "MoveTrackerIssuesToSprint", connectionID, in, (*trackerapp.Service).MoveToSprint)
}

// CreateTrackerSprint makes a future sprint on a board.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) CreateTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "CreateTrackerSprint", connectionID, in, (*trackerapp.Service).CreateSprint)
}

// StartTrackerSprint starts a future sprint.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) StartTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintStartRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "StartTrackerSprint", connectionID, in, (*trackerapp.Service).StartSprint)
}

// CompleteTrackerSprint closes an active sprint, moving its unfinished
// issues where the request says.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) CompleteTrackerSprint(ctx context.Context, connectionID string, in trackerapp.SprintCompleteRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "CompleteTrackerSprint", connectionID, in, (*trackerapp.Service).CompleteSprint)
}

// AddTrackerComment adds a comment to an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) AddTrackerComment(ctx context.Context, connectionID string, in trackerapp.CommentRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "AddTrackerComment", connectionID, in, (*trackerapp.Service).AddComment)
}

// EditTrackerComment replaces a comment's body.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) EditTrackerComment(ctx context.Context, connectionID string, in trackerapp.CommentEditRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "EditTrackerComment", connectionID, in, (*trackerapp.Service).EditComment)
}

// DeleteTrackerComment deletes a comment.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) DeleteTrackerComment(ctx context.Context, connectionID string, in trackerapp.CommentDeleteRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "DeleteTrackerComment", connectionID, in, (*trackerapp.Service).DeleteComment)
}

// LinkTrackerIssues links two issues.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) LinkTrackerIssues(ctx context.Context, connectionID string, in trackerapp.LinkRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "LinkTrackerIssues", connectionID, in, (*trackerapp.Service).Link)
}

// UnlinkTrackerIssues removes one link between two issues.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) UnlinkTrackerIssues(ctx context.Context, connectionID string, in trackerapp.UnlinkRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "UnlinkTrackerIssues", connectionID, in, (*trackerapp.Service).Unlink)
}

// AddTrackerWorklog logs time on an issue, as the person's own action.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) AddTrackerWorklog(ctx context.Context, connectionID string, in trackerapp.WorklogRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "AddTrackerWorklog", connectionID, in, (*trackerapp.Service).AddWorklog)
}
