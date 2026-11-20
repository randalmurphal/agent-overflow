package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

// The checks a write makes before its first request, so that it changes
// only what its input names. Linear IDs are workspace-wide: nothing in a
// request ties the comment, relation, cycle, state or issue it names to
// the issue or board the input names, so the provider reads that tie
// first. Linear takes no precondition on a write, so each check narrows
// the window in which the tie can break and does not close it.

// onBoard maps each of ids that is an issue on board now to its current
// sortOrder: those a read by ID within the board's team finds, archived
// ones left out, in one request for up to idChunk IDs. An ID it lacks is
// not on the board.
func (p *Provider) onBoard(ctx context.Context, board tracker.BoardID, ids []string) (map[string]float64, error) {
	issues, err := p.issuesByID(ctx, ids, linear.IssueFilter{Teams: []string{string(board)}}, false)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: read the issues on board %s: %w", board, err)
	}
	on := make(map[string]float64, len(issues))
	for _, is := range issues {
		if is.TeamID == string(board) {
			on[is.ID] = is.SortOrder
		}
	}
	return on, nil
}

// notOnBoard refuses an issue onBoard did not find.
func notOnBoard(issue tracker.IssueID, board tracker.BoardID) tracker.Refusal {
	msg := fmt.Sprintf("issue %s is not on board %s now", issue, board)
	return tracker.Refusal{Reason: tracker.ReasonStale, Message: msg, Err: errors.New("linearprovider: " + msg)}
}

// openCycle refuses, as a *refusedEdit, a cycle that is not board's or
// is closed; any other error is a read that failed.
func (p *Provider) openCycle(ctx context.Context, board tracker.BoardID, sprint tracker.SprintID) error {
	c, err := p.c.Cycle(ctx, string(sprint))
	switch {
	case errors.Is(err, trackerapi.ErrNotFound):
		return refusedBy(tracker.ReasonStale, err, "cycle %s does not exist or is not visible", sprint)
	case err != nil:
		return fmt.Errorf("linearprovider: read cycle %s: %w", sprint, err)
	case c.TeamID != string(board):
		return refusedf(tracker.ReasonInvalid, "cycle %s is not a cycle of board %s", sprint, board)
	case !c.IsActive && !c.IsFuture:
		return refusedf(tracker.ReasonStale, "cycle %s is closed", sprint)
	}
	return nil
}

// issueComment is comment as Linear holds it, refused as a *refusedEdit
// when it is gone or on another issue than issue.
func (p *Provider) issueComment(ctx context.Context, issue tracker.IssueID, comment tracker.CommentID) (*linear.Comment, error) {
	c, err := p.c.Comment(ctx, string(comment))
	switch {
	case errors.Is(err, trackerapi.ErrNotFound):
		return nil, refusedBy(tracker.ReasonStale, err, "comment %s is no longer on the issue", comment)
	case err != nil:
		return nil, fmt.Errorf("linearprovider: read comment %s: %w", comment, err)
	case c.IssueID != string(issue):
		return nil, refusedf(tracker.ReasonInvalid, "comment %s is not on issue %s", comment, issue)
	}
	return c, nil
}

// linkBetween refuses, as a *refusedEdit, a relation that is gone or is
// not between in.Issue and in.Other, in either direction.
func (p *Provider) linkBetween(ctx context.Context, in tracker.UnlinkInput) error {
	r, err := p.c.Relation(ctx, string(in.Link))
	switch {
	case errors.Is(err, trackerapi.ErrNotFound):
		return refusedBy(tracker.ReasonStale, err, "link %s no longer exists", in.Link)
	case err != nil:
		return fmt.Errorf("linearprovider: read link %s: %w", in.Link, err)
	}
	sides := []string{r.Issue.ID, r.Related.ID}
	want := []string{string(in.Issue), string(in.Other)}
	if !slices.Equal(sides, want) && !slices.Equal(sides, []string{want[1], want[0]}) {
		return refusedf(tracker.ReasonInvalid, "link %s is not between issues %s and %s", in.Link, in.Issue, in.Other)
	}
	return nil
}

// teamState refuses, as a *refusedEdit, a state that is not one of
// issue's team's; any other error is a read that failed, a missing issue
// matching trackerapi.ErrNotFound.
func (p *Provider) teamState(ctx context.Context, issue tracker.IssueID, state tracker.MoveID) error {
	is, err := p.summary(ctx, issue)
	if err != nil {
		return err
	}
	states, err := p.teamStates(ctx, is.TeamID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(states, func(s linear.WorkflowState) bool { return s.ID == string(state) }) {
		return refusedf(tracker.ReasonInvalid, "status %s is not one of the statuses of issue %s's team", state, issue)
	}
	return nil
}
