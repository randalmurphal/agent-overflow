package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// Every write runs its input's Check first and refuses what it cannot
// send before any request. It reports each request's outcome by the
// client's mutation outcome table (outcome) and never sends one again.

func (ch changes) update() linear.UpdateIssueInput {
	return linear.UpdateIssueInput{
		Title: ch.title, Description: ch.description, AssigneeID: ch.assignee, Priority: ch.priority,
		LabelIDs: ch.labels, ParentID: ch.parent, ProjectID: ch.project, DueDate: ch.dueDate,
	}
}

// create is the create in team with title of ch. A clear is sent as a
// clear wherever Linear fills an omitted member: a sub-issue takes its
// parent's priority and project, and its assignee in some cases
// (https://linear.app/docs/parent-and-sub-issues), so those go as
// PriorityNone and null, and a due date as null, which Linear stores as
// none (measured). A clear of a member Linear leaves empty when omitted
// is omitted: the description, the labels ("Labels are not inherited",
// the same page) and the parent.
func (ch changes) create(team tracker.ProjectID, title string) linear.CreateIssueInput {
	in := linear.CreateIssueInput{
		TeamID: string(team), Title: title, Description: ch.description.Value(), AssigneeID: ch.assignee,
		Priority: ch.priority, LabelIDs: ch.labels.Value(), ProjectID: ch.project, DueDate: ch.dueDate,
	}
	if !ch.parent.IsNull() {
		in.ParentID = ch.parent
	}
	return in
}

// editRefusal is the result of a resolution that failed: refused when
// the edit was, else not sent because a read failed.
func editRefusal(step tracker.Step, err error, issues ...tracker.IssueID) tracker.Result {
	if r, ok := errors.AsType[*refusedEdit](err); ok {
		return refuse(step, r.Refusal, issues...)
	}
	return beforeWrite(step, err, issues...)
}

// Create creates an issue in the team in.Project. A confirmed create
// names the new issue and may have changed its card, its row on the
// team's board and its detail; a lost answer is a create part of unknown
// outcome naming no issue.
func (p *Provider) Create(ctx context.Context, in tracker.CreateInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepCreate, err)
	}
	if in.Type != "" {
		return refuse(tracker.StepCreate, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: "Linear issues have no type"})
	}
	if in.ImageDir != "" {
		return unsupported(tracker.StepCreate, "local images in Markdown")
	}
	r := &resolver{p: p, team: string(in.Project)}
	ch, err := r.resolve(ctx, in.Fields, true)
	if err != nil {
		return editRefusal(tracker.StepCreate, err)
	}
	is, err := p.c.CreateIssue(ctx, ch.create(in.Project, in.Title))
	if err != nil {
		return one("", tracker.StepCreate, err)
	}
	id := tracker.IssueID(is.ID)
	return one(id, tracker.StepCreate, nil, tracker.CardOf(id), tracker.RowOf(tracker.BoardID(is.TeamID), id), tracker.DetailOf(id))
}

// Edit changes fields of an issue with one update. A Markdown edit of the
// description is refused as stale when the description Linear holds is
// not the Markdown it was written from; the check reads the issue just
// before the update and is not atomic with it.
func (p *Provider) Edit(ctx context.Context, in tracker.EditInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepEdit, err, in.Issue)
	}
	if in.ImageDir != "" {
		return unsupported(tracker.StepEdit, "local images in Markdown", in.Issue)
	}
	r := &resolver{p: p}
	var current *linear.Issue
	if needsTeam(in.Fields) || slices.ContainsFunc(in.Fields, func(e tracker.FieldEdit) bool { return e.Value.Form() == tracker.EditMarkdown }) {
		is, err := p.summary(ctx, in.Issue)
		if err != nil {
			return beforeWrite(tracker.StepEdit, err, in.Issue)
		}
		current, r.team = is, is.TeamID
	}
	ch, err := r.resolve(ctx, in.Fields, false)
	if err != nil {
		return editRefusal(tracker.StepEdit, err, in.Issue)
	}
	if ch.descriptionFrom != nil && *ch.descriptionFrom != current.Description {
		return refuse(tracker.StepEdit, tracker.Refusal{Reason: tracker.ReasonStale, Message: "the description changed since the edit was written"}, in.Issue)
	}
	_, err = p.c.UpdateIssue(ctx, string(in.Issue), ch.update())
	return one(in.Issue, tracker.StepEdit, err, tracker.CardOf(in.Issue), tracker.DetailOf(in.Issue))
}

// Move sets an issue's workflow state, the move's ID. Linear's moves take
// no fields. It reads the issue and its team's states first and refuses
// a state of another team.
func (p *Provider) Move(ctx context.Context, in tracker.MoveInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepMove, err, in.Issue)
	}
	if len(in.Fields) > 0 || in.ImageDir != "" {
		return unsupported(tracker.StepMove, "fields on a move", in.Issue)
	}
	if err := p.teamState(ctx, in.Issue, in.Move); err != nil {
		return editRefusal(tracker.StepMove, err, in.Issue)
	}
	_, err := p.c.UpdateIssue(ctx, string(in.Issue), linear.UpdateIssueInput{StateID: linear.Set(string(in.Move))})
	return one(in.Issue, tracker.StepMove, err, tracker.CardOf(in.Issue), tracker.DetailOf(in.Issue))
}

// SetEstimate sets the estimate of an issue of the team in.Board: a
// value on the team's scale, or none. It reads the issue within the
// board and the team first, and refuses an issue not on the board, a
// team without estimates and a value off its scale.
func (p *Provider) SetEstimate(ctx context.Context, in tracker.EstimateInput) tracker.Result {
	step := tracker.StepEstimate
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issue)
	}
	if !linear.IsIssueID(string(in.Issue)) {
		return notIssueID(step, in.Issue, in.Issue)
	}
	on, err := p.onBoard(ctx, in.Board, []string{string(in.Issue)})
	if err != nil {
		return beforeWrite(step, err, in.Issue)
	}
	if _, ok := on[string(in.Issue)]; !ok {
		return tracker.Refuse(step, notOnBoard(in.Issue, in.Board), in.Issue)
	}
	t, err := p.team(ctx, string(in.Board))
	if err != nil {
		return beforeWrite(step, err, in.Issue)
	}
	value := linear.Null[int]()
	if in.Value != nil {
		v, ok := onScale(estimate(t.Estimation), *in.Value)
		if !ok {
			return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: fmt.Sprintf("%v is not an estimate on board %s", *in.Value, in.Board)}, in.Issue)
		}
		value = linear.Set(v)
	}
	_, err = p.c.UpdateIssue(ctx, string(in.Issue), linear.UpdateIssueInput{Estimate: value})
	return one(in.Issue, step, err, tracker.RowOf(in.Board, in.Issue), tracker.CardOf(in.Issue), tracker.DetailOf(in.Issue))
}

// onScale is v as Linear's whole-number estimate when the board takes
// it: a board with estimates, a whole number, and one of its fixed
// choices when it has them.
func onScale(e tracker.Estimate, v float64) (int, bool) {
	if e.Unit == tracker.UnitNone || v != math.Trunc(v) || v > math.MaxInt32 {
		return 0, false
	}
	if len(e.Choices) > 0 && !slices.ContainsFunc(e.Choices, func(c tracker.EstimateChoice) bool { return c.Value == v }) {
		return 0, false
	}
	return int(v), true
}

// MoveToSprint moves issues into the cycle in.Sprint, or out of any cycle
// when it is empty, one update each in order. It reads the cycle and the
// issues within the board first: a cycle that is not the board's or is
// closed refuses the move, and an issue not on the board is refused while
// the others proceed. It stops at the first update not applied; the
// issues after it are not sent.
func (p *Provider) MoveToSprint(ctx context.Context, in tracker.SprintMoveInput) tracker.Result {
	step := tracker.StepSprintMove
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issues...)
	}
	ids, bad := issueIDs(in.Issues)
	if len(bad) > 0 {
		return notIssueID(step, bad[0], in.Issues...)
	}
	cycle := linear.Null[string]()
	if in.Sprint != "" {
		if err := p.openCycle(ctx, in.Board, in.Sprint); err != nil {
			return editRefusal(step, err, in.Issues...)
		}
		cycle = linear.Set(string(in.Sprint))
	}
	on, err := p.onBoard(ctx, in.Board, ids)
	if err != nil {
		return beforeWrite(step, err, in.Issues...)
	}
	var out tracker.Result
	var failed tracker.IssueID
	for _, id := range in.Issues {
		_, isOn := on[string(id)]
		switch {
		case !isOn:
			out.Parts = append(out.Parts, tracker.Part{Issue: id, Step: step, Outcome: tracker.Refused(notOnBoard(id, in.Board))})
		case failed != "":
			out.Parts = append(out.Parts, notSentAfter(step, failed, []tracker.IssueID{id})...)
		default:
			_, err := p.c.UpdateIssue(ctx, string(id), linear.UpdateIssueInput{CycleID: cycle})
			out.Parts = append(out.Parts, part(id, step, err, tracker.RowOf(in.Board, id), tracker.CardOf(id), tracker.DetailOf(id)))
			if err != nil {
				failed = id
			}
		}
	}
	return out
}

// notSentAfter are the parts of issues not sent because the part of
// failed was not applied.
func notSentAfter(step tracker.Step, failed tracker.IssueID, rest []tracker.IssueID) []tracker.Part {
	why := fmt.Errorf("linearprovider: not sent: the write of issue %s before it was not applied", failed)
	parts := make([]tracker.Part, 0, len(rest))
	for _, id := range rest {
		parts = append(parts, tracker.Part{Issue: id, Step: step, Outcome: tracker.NotSent(why)})
	}
	return parts
}

// CreateSprint is refused: Linear generates cycles from team settings.
func (p *Provider) CreateSprint(_ context.Context, in tracker.SprintInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	return unsupported(tracker.StepSprint, "creating cycles; they follow the team's settings")
}

// StartSprint is refused: Linear starts cycles on their schedule.
func (p *Provider) StartSprint(_ context.Context, in tracker.SprintStart) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	return unsupported(tracker.StepSprint, "starting cycles; they follow the team's settings")
}

// CompleteSprint is refused: Linear completes cycles and rolls their
// unfinished issues over itself.
func (p *Provider) CompleteSprint(_ context.Context, in tracker.SprintCompletion) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	return unsupported(tracker.StepSprint, "completing cycles; they follow the team's settings")
}

// EditSprint is refused: a cycle's dates follow the team's settings.
func (p *Provider) EditSprint(_ context.Context, in tracker.SprintEdit) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprint, err)
	}
	return unsupported(tracker.StepSprint, "editing cycles; they follow the team's settings")
}

// AddComment adds a Markdown comment. Linear comments have no audience.
func (p *Provider) AddComment(ctx context.Context, in tracker.CommentInput) tracker.Result {
	step := tracker.StepComment
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issue)
	}
	if in.Audience != tracker.AudienceUnset {
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: "Linear comments have no audience"}, in.Issue)
	}
	if in.ImageDir != "" {
		return unsupported(step, "local images in Markdown", in.Issue)
	}
	_, err := p.c.CreateComment(ctx, linear.CreateCommentInput{IssueID: string(in.Issue), Body: in.Markdown})
	return one(in.Issue, step, err, tracker.DetailOf(in.Issue))
}

// EditComment replaces a comment's body. It reads the comment first and
// refuses one on another issue, and as stale one that is gone or whose
// body is no longer in.From.
func (p *Provider) EditComment(ctx context.Context, in tracker.CommentEdit) tracker.Result {
	step := tracker.StepComment
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issue)
	}
	if in.ImageDir != "" {
		return unsupported(step, "local images in Markdown", in.Issue)
	}
	c, err := p.issueComment(ctx, in.Issue, in.Comment)
	if err != nil {
		return editRefusal(step, err, in.Issue)
	}
	if c.Body != in.From {
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonStale, Message: "the comment changed since the edit was written"}, in.Issue)
	}
	_, err = p.c.UpdateComment(ctx, string(in.Comment), in.Markdown)
	return one(in.Issue, step, err, tracker.DetailOf(in.Issue))
}

// DeleteComment deletes a comment. It reads the comment first and refuses
// one on another issue, and as stale one that is gone.
func (p *Provider) DeleteComment(ctx context.Context, in tracker.CommentDelete) tracker.Result {
	step := tracker.StepComment
	if err := in.Check(); err != nil {
		return tracker.Invalid(step, err, in.Issue)
	}
	if _, err := p.issueComment(ctx, in.Issue, in.Comment); err != nil {
		return editRefusal(step, err, in.Issue)
	}
	err := p.c.DeleteComment(ctx, string(in.Comment))
	return one(in.Issue, step, err, tracker.DetailOf(in.Issue))
}

// Link relates two issues by one of the relations LinkTypes lists.
func (p *Provider) Link(ctx context.Context, in tracker.LinkInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Outward)
	}
	if !slices.ContainsFunc(linkTypes, func(t tracker.LinkType) bool { return t.ID == in.Type }) {
		return refuse(tracker.StepLink, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: fmt.Sprintf("Linear has no link type %q to create", in.Type)}, in.Outward)
	}
	_, err := p.c.CreateRelation(ctx, linear.CreateRelationInput{
		IssueID: string(in.Outward), RelatedIssueID: string(in.Inward), Type: linear.RelationType(in.Type),
	})
	return one(in.Outward, tracker.StepLink, err, tracker.DetailOf(in.Outward), tracker.DetailOf(in.Inward))
}

// Unlink deletes one relation. It reads the relation first and refuses
// one that is not between in.Issue and in.Other, and as stale one that is
// gone.
func (p *Provider) Unlink(ctx context.Context, in tracker.UnlinkInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepLink, err, in.Issue)
	}
	if err := p.linkBetween(ctx, in); err != nil {
		return editRefusal(tracker.StepLink, err, in.Issue)
	}
	err := p.c.DeleteRelation(ctx, string(in.Link))
	return one(in.Issue, tracker.StepLink, err, tracker.DetailOf(in.Issue), tracker.DetailOf(in.Other))
}

// Attach is refused: Linear's attachments are links, not files.
func (p *Provider) Attach(_ context.Context, in tracker.AttachInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepUpload, err, in.Issue)
	}
	return unsupported(tracker.StepUpload, "file attachments", in.Issue)
}

// Download fails wrapping tracker.ErrUnsupported: Linear holds no files.
func (p *Provider) Download(_ context.Context, in tracker.DownloadInput) error {
	if err := in.Check(); err != nil {
		return err
	}
	return fmt.Errorf("linearprovider: download: %w", tracker.ErrUnsupported)
}
