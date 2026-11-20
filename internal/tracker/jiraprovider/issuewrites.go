package jiraprovider

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
)

// CreateSprint creates a future sprint on a board.
func (p *Provider) CreateSprint(ctx context.Context, in tracker.SprintInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	board, err := boardNumber(in.Board)
	if err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	_, err = p.jira.CreateSprint(ctx, jira.SprintInput{Board: board, Name: in.Name, Goal: in.Goal, Start: in.Start, End: in.End})
	return sprintPart(in.Board, outcome(err))
}

// StartSprint starts a future sprint of the board with its dates.
func (p *Provider) StartSprint(ctx context.Context, in tracker.SprintStart) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	if err := checkSprintIDs(in.Board, in.Sprint); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	sprint, err := p.boardSprint(ctx, in.Board, in.Sprint, jira.SprintFuture)
	if err != nil {
		return sprintPart(in.Board, beforeSend(err))
	}
	_, err = p.jira.StartSprint(ctx, jira.SprintStart{ID: sprint.ID, Start: in.Start, End: in.End, Current: sprint})
	return sprintPart(in.Board, outcome(err))
}

// EditSprint changes the name, goal or dates of a sprint of the board.
func (p *Provider) EditSprint(ctx context.Context, in tracker.SprintEdit) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	if err := checkSprintIDs(in.Board, in.Sprint); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	sprint, err := p.boardSprint(ctx, in.Board, in.Sprint)
	if err != nil {
		return sprintPart(in.Board, beforeSend(err))
	}
	_, err = p.jira.UpdateSprint(ctx, jira.SprintUpdate{ID: sprint.ID, Name: in.Name, Goal: in.Goal, Start: in.Start, End: in.End, Current: sprint})
	return sprintPart(in.Board, outcome(err))
}

// checkSprintIDs refuses a board or sprints that are not Jira IDs.
func checkSprintIDs(board tracker.BoardID, sprints ...tracker.SprintID) error {
	if _, err := boardNumber(board); err != nil {
		return err
	}
	for _, s := range sprints {
		if _, err := sprintNumber(s); err != nil {
			return err
		}
	}
	return nil
}

// CompleteSprint closes an active sprint through the jira client's
// completion, which moves the unfinished issues to MoveTo or the backlog
// first, lists the sprint again and closes it only when none is left;
// the moves and the close are separate writes, not one. The result has a
// StepSprintMove part for each unfinished issue, by ID, with the
// client's outcome for its move, then the StepSprint part of the close:
//
//   - not sent when a move failed or the listing after the moves did,
//     the moves' parts saying how far they got;
//   - refused as stale when unfinished issues arrived meanwhile, so the
//     caller runs the completion again;
//   - otherwise the outcome of the close's answer, unknown included.
//
// A completion refused before any write is the close part alone; so is one
// whose sprint is not an active sprint of the board, or whose MoveTo is
// not an open one.
func (p *Provider) CompleteSprint(ctx context.Context, in tracker.SprintCompletion) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	ids := []tracker.SprintID{in.Sprint}
	if in.MoveTo != "" {
		ids = append(ids, in.MoveTo)
	}
	if err := checkSprintIDs(in.Board, ids...); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	board, _ := boardNumber(in.Board)
	sprint, moveTo, err := p.completionSprints(ctx, in)
	if err != nil {
		return sprintPart(in.Board, beforeSend(err))
	}
	c := jira.SprintCompletion{Sprint: sprint.ID, Board: board, Current: sprint}
	if moveTo != nil {
		c.MoveTo, c.MoveToCurrent = moveTo.ID, moveTo
	}
	res, err := p.jira.CompleteSprint(ctx, c)
	moveErr, moveFailed := errors.AsType[*jira.BatchError](err)
	moved := make([]tracker.IssueID, len(res.Unfinished))
	for i, is := range res.Unfinished {
		moved[i] = tracker.IssueID(is.ID)
	}
	var batchErr error
	if moveFailed {
		batchErr = moveErr
	}
	parts := batchParts(in.Board, tracker.StepSprintMove, moved, res.Moves, batchErr).Parts
	closing := tracker.Part{Step: tracker.StepSprint, Changed: []tracker.Resource{tracker.StructureOf(in.Board)}}
	switch {
	case moveFailed:
		closing.Outcome = tracker.NotSent(fmt.Errorf("jiraprovider: the close of sprint %s was not sent: %w", in.Sprint, err))
	case len(res.Remaining) > 0:
		closing.Outcome = tracker.Refused(tracker.Refusal{
			Reason:  tracker.ReasonStale,
			Message: fmt.Sprintf("%d unfinished issues joined the sprint while it was completed; it stays open, so complete it again", len(res.Remaining)),
			Err:     err,
		})
	default:
		closing.Outcome = outcome(err)
	}
	return tracker.Result{Parts: append(parts, closing)}
}

// completionSprints are in's sprint and the sprint it moves issues to
// (nil for the backlog) as one listing of the board's open sprints holds
// them: the sprint must be an active one of the board, MoveTo an active or
// future one. The client takes both as read rather than reading them again.
func (p *Provider) completionSprints(ctx context.Context, in tracker.SprintCompletion) (sprint, moveTo *jira.Sprint, err error) {
	open, err := p.boardSprints(ctx, in.Board, jira.SprintActive, jira.SprintFuture)
	if err != nil {
		return nil, nil, err
	}
	n, _ := sprintNumber(in.Sprint)
	s, ok := open[n]
	if !ok || s.State != jira.SprintActive {
		return nil, nil, notBoardSprint(in.Board, in.Sprint, []jira.SprintState{jira.SprintActive})
	}
	sprint = &s
	if in.MoveTo != "" {
		n, _ = sprintNumber(in.MoveTo)
		to, ok := open[n]
		if !ok {
			return nil, nil, notBoardSprint(in.Board, in.MoveTo, []jira.SprintState{jira.SprintActive, jira.SprintFuture})
		}
		moveTo = &to
	}
	return sprint, moveTo, nil
}

// sprintPart is a sprint write's one part, about no issue.
func sprintPart(board tracker.BoardID, o tracker.Outcome) tracker.Result {
	return tracker.Result{Parts: []tracker.Part{{Step: tracker.StepSprint, Outcome: o, Changed: []tracker.Resource{tracker.StructureOf(board)}}}}
}

// AddComment adds a comment, uploading the local images its Markdown
// embeds first.
func (p *Provider) AddComment(ctx context.Context, in tracker.CommentInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	aud := map[tracker.Audience]jira.Audience{
		tracker.AudienceUnset: jira.AudienceUnset, tracker.AudiencePublic: jira.AudiencePublic, tracker.AudienceInternal: jira.AudienceInternal,
	}[in.Audience]
	res, err := p.jira.AddComment(ctx, jira.CommentInput{Issue: string(in.Issue), Markdown: in.Markdown, Audience: aud, ImageDir: in.ImageDir})
	return commentParts(in.Issue, res.Uploaded, err)
}

// EditComment replaces a comment's body when it still renders to the
// Markdown the edit was written from. That read is the last before the
// first write: the client takes the comment as read (Current) and reads
// it again only when resolving a mention read Jira since.
func (p *Provider) EditComment(ctx context.Context, in tracker.CommentEdit) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	if _, err := numericID("comment", string(in.Comment)); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	read, err := p.readComment(ctx, in.Issue, in.Comment)
	if err == nil && adf.ToMarkdown(read.Body) != in.From {
		err = stale(string(in.Issue), "comment "+string(in.Comment))
	}
	if err != nil {
		return commentParts(in.Issue, nil, err)
	}
	res, err := p.jira.EditComment(ctx, jira.CommentEdit{
		Issue: string(in.Issue), ID: string(in.Comment), Markdown: in.Markdown, Current: &read,
		AcceptLoss: in.AcceptLoss, ImageDir: in.ImageDir,
	})
	return commentParts(in.Issue, res.Uploaded, err)
}

// DeleteComment deletes a comment of the issue, read first through the
// issue's path so a comment of another issue is refused before the
// delete.
func (p *Provider) DeleteComment(ctx context.Context, in tracker.CommentDelete) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	if _, err := numericID("comment", string(in.Comment)); err != nil {
		return tracker.Invalid(tracker.StepComment, err, in.Issue)
	}
	var o tracker.Outcome
	if _, err := p.readComment(ctx, in.Issue, in.Comment); err != nil {
		o = beforeSend(err)
	} else {
		o = outcome(p.jira.DeleteComment(ctx, string(in.Issue), string(in.Comment)))
	}
	return tracker.Result{Parts: []tracker.Part{{Issue: in.Issue, Step: tracker.StepComment, Outcome: o, Changed: []tracker.Resource{tracker.DetailOf(in.Issue)}}}}
}

// commentParts are a comment write's parts: its images' upload when it
// made one, then the comment. Either changes the issue's detail.
func commentParts(issue tracker.IssueID, stored []jira.Attachment, err error) tracker.Result {
	res := stepParts(issue, tracker.StepComment, stored, err)
	for i := range res.Parts {
		res.Parts[i].Changed = []tracker.Resource{tracker.DetailOf(issue)}
	}
	return res
}

// Link links two issues by a link type.
func (p *Provider) Link(ctx context.Context, in tracker.LinkInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Outward)
	}
	if err := checkIssues([]tracker.IssueID{in.Outward, in.Inward}); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Outward)
	}
	if _, err := numericID("link type", string(in.Type)); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Outward)
	}
	err := p.jira.LinkIssues(ctx, jira.LinkInput{Type: string(in.Type), Outward: string(in.Outward), Inward: string(in.Inward)})
	return linkPart(in.Outward, in.Inward, outcome(err))
}

// Unlink removes one link.
func (p *Provider) Unlink(ctx context.Context, in tracker.UnlinkInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Issue)
	}
	if err := checkIssues([]tracker.IssueID{in.Issue, in.Other}); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Issue)
	}
	if _, err := numericID("issue link", string(in.Link)); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Issue)
	}
	if err := p.checkLink(ctx, in.Issue, in.Other, in.Link); err != nil {
		return linkPart(in.Issue, in.Other, beforeSend(err))
	}
	return linkPart(in.Issue, in.Other, outcome(p.jira.UnlinkIssues(ctx, string(in.Link))))
}

// linkPart is a link write's part, about issue, which may change both
// issues' detail.
func linkPart(issue, other tracker.IssueID, o tracker.Outcome) tracker.Result {
	return tracker.Result{Parts: []tracker.Part{{
		Issue: issue, Step: tracker.StepLink, Outcome: o,
		Changed: []tracker.Resource{tracker.DetailOf(issue), tracker.DetailOf(other)},
	}}}
}

// Attach uploads files to an issue in one request.
func (p *Provider) Attach(ctx context.Context, in tracker.AttachInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepUpload, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepUpload, err, in.Issue)
	}
	files := make([]jira.File, len(in.Files))
	for i, f := range in.Files {
		files[i] = jira.File{Name: f.Name, Content: f.Content}
	}
	_, err := p.jira.Attach(ctx, string(in.Issue), files...)
	return tracker.Result{Parts: []tracker.Part{{Issue: in.Issue, Step: tracker.StepUpload, Outcome: outcome(err), Changed: []tracker.Resource{tracker.DetailOf(in.Issue)}}}}
}
