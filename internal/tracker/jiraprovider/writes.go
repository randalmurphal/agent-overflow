package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// A write's parts follow the client's steps. A step the client did not
// reach is not sent; the step that failed is judged by outcome; the steps
// before it were applied. An upload part stands for every image a
// Markdown body embeds and is reported when the write uploaded one or
// failed uploading. Nothing is sent again: an unknown outcome is reported
// as it is.

// issueResources are what an edit, a move or an image upload may change:
// the issue's card and its detail.
func issueResources(issue tracker.IssueID) []tracker.Resource {
	return []tracker.Resource{tracker.CardOf(issue), tracker.DetailOf(issue)}
}

// stepOf is the client step err failed at, or "" for a failure before the
// write's first request.
func stepOf(err error) jira.Step {
	if se, ok := errors.AsType[*jira.StepError](err); ok {
		return se.Step
	}
	return ""
}

// stepParts are the parts of a write of issue whose main step is step,
// which uploaded images first when uploaded is set or the write failed
// uploading.
func stepParts(issue tracker.IssueID, step tracker.Step, uploaded bool, err error) tracker.Result {
	main := tracker.Part{Issue: issue, Step: step, Outcome: outcome(err), Changed: issueResources(issue)}
	failed := stepOf(err)
	if failed == jira.StepCheck || failed == "" && err != nil {
		main.Outcome = beforeSend(err)
	}
	if !uploaded && failed != jira.StepUpload {
		return tracker.Result{Parts: []tracker.Part{main}}
	}
	upload := tracker.Part{Issue: issue, Step: tracker.StepUpload, Outcome: tracker.Applied(), Changed: issueResources(issue)}
	if failed == jira.StepUpload {
		upload.Outcome = outcome(err)
		main.Outcome = tracker.NotSent(fmt.Errorf("jiraprovider: not sent after the upload failed: %w", err))
	}
	return tracker.Result{Parts: []tracker.Part{upload, main}}
}

// Create makes an issue: the create, then, when its Markdown embeds local
// images, their upload to the new issue and the edit that writes them in.
func (p *Provider) Create(ctx context.Context, in tracker.CreateInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepCreate, err)
	}
	if err := checkProject(in.Project); err != nil {
		return tracker.Invalid(tracker.StepCreate, err)
	}
	if _, err := numericID("issue type", string(in.Type)); err != nil {
		return tracker.Invalid(tracker.StepCreate, err)
	}
	inputs, err := p.fieldInputs(ctx, "", in.Fields)
	if err != nil {
		return tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: beforeSend(err)}}}
	}
	inputs = append([]jira.FieldInput{{Field: jiraSummary, Value: jira.Text(in.Title)}}, inputs...)
	plan, err := p.jira.PlanCreate(ctx, jira.CreateInput{
		Project: string(in.Project), IssueType: string(in.Type), Fields: inputs, ImageDir: in.ImageDir,
	})
	if err != nil {
		return tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: beforeSend(err)}}}
	}
	res, err := p.jira.ApplyCreate(ctx, plan)
	return createParts(res, len(plan.Preview().Images) > 0, err)
}

// createParts are a create's parts from ApplyCreate's result: the
// create names the issue when Jira confirmed it, and none otherwise, as
// not sent, refused or of unknown outcome.
func createParts(res jira.CreateResult, images bool, err error) tracker.Result {
	failed := stepOf(err)
	if res.ID == "" {
		var o tracker.Outcome
		switch {
		case err == nil:
			o = tracker.Unknown(errors.New("jiraprovider: the create succeeded and named no issue"))
		case failed == jira.StepCreate:
			if o = outcome(err); o.State() == tracker.StateApplied {
				o = tracker.Unknown(err)
			}
		default:
			o = beforeSend(err)
		}
		return tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: o}}}
	}
	id := tracker.IssueID(res.ID)
	parts := []tracker.Part{{Issue: id, Step: tracker.StepCreate, Outcome: tracker.Applied(), Changed: issueResources(id)}}
	if !images {
		return tracker.Result{Parts: parts}
	}
	upload := tracker.Part{Issue: id, Step: tracker.StepUpload, Outcome: tracker.Applied(), Changed: issueResources(id)}
	finish := tracker.Part{Issue: id, Step: tracker.StepEdit, Outcome: tracker.Applied(), Changed: issueResources(id)}
	switch failed {
	case jira.StepUpload:
		upload.Outcome = outcome(err)
		finish.Outcome = tracker.NotSent(fmt.Errorf("jiraprovider: not sent after the upload failed: %w", err))
	case jira.StepFinish:
		finish.Outcome = outcome(err)
	}
	return tracker.Result{Parts: append(parts, upload, finish)}
}

// Edit changes fields of an issue through a plan the client checks and
// then applies.
func (p *Provider) Edit(ctx context.Context, in tracker.EditInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepEdit, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepEdit, err, in.Issue)
	}
	inputs, err := p.fieldInputs(ctx, in.Issue, in.Fields)
	if err != nil {
		return stepParts(in.Issue, tracker.StepEdit, false, err)
	}
	plan, err := p.jira.PlanEdit(ctx, jira.EditInput{
		Issue: string(in.Issue), Fields: inputs, ImageDir: in.ImageDir, AcceptLoss: in.AcceptLoss,
	})
	if err != nil {
		return stepParts(in.Issue, tracker.StepEdit, false, err)
	}
	res, err := p.jira.ApplyEdit(ctx, plan)
	return stepParts(in.Issue, tracker.StepEdit, len(res.Uploaded) > 0, err)
}

// Move moves an issue by one of its transitions, with the fields its
// screen takes.
func (p *Provider) Move(ctx context.Context, in tracker.MoveInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepMove, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepMove, err, in.Issue)
	}
	if _, err := numericID("transition", string(in.Move)); err != nil {
		return tracker.Invalid(tracker.StepMove, err, in.Issue)
	}
	inputs, err := p.fieldInputs(ctx, in.Issue, in.Fields)
	if err != nil {
		return stepParts(in.Issue, tracker.StepMove, false, err)
	}
	res, err := p.jira.Transition(ctx, jira.TransitionInput{
		Issue: string(in.Issue), Transition: string(in.Move), Fields: inputs, ImageDir: in.ImageDir, AcceptLoss: in.AcceptLoss,
	})
	return stepParts(in.Issue, tracker.StepMove, len(res.Uploaded) > 0, err)
}

// SetEstimate writes an issue's estimate through the board's estimation
// field, in the board's unit: points, or seconds for original time,
// which Jira takes in whole minutes. Zero is an estimate of zero, as the
// jira client writes it; a nil Value clears.
func (p *Provider) SetEstimate(ctx context.Context, in tracker.EstimateInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepEstimate, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepEstimate, err, in.Issue)
	}
	part := tracker.Part{Issue: in.Issue, Step: tracker.StepEstimate, Changed: []tracker.Resource{tracker.RowOf(in.Board, in.Issue), tracker.DetailOf(in.Issue)}}
	b, err := p.board(ctx, in.Board)
	if err != nil {
		part.Outcome = beforeSend(err)
		return tracker.Result{Parts: []tracker.Part{part}}
	}
	value := jira.NoEstimate()
	switch v := in.Value; {
	case v == nil:
	case estimateOf(b.cfg).Unit == tracker.UnitTime:
		if *v >= math.MaxInt64/float64(time.Second) {
			return tracker.Invalid(tracker.StepEstimate, invalidf("%v seconds is not a time estimate", *v), in.Issue)
		}
		value = jira.TimeEstimate(time.Duration(*v * float64(time.Second)))
	default:
		value = jira.Points(*v)
	}
	_, _, err = p.jira.SetEstimate(ctx, jira.EstimateInput{Issue: string(in.Issue), Board: b.cfg.BoardID, Value: value})
	part.Outcome = outcome(err)
	return tracker.Result{Parts: []tracker.Part{part}}
}

// Rank places issues on a board just after Prev, or just before Next when
// there is no Prev, through the board's rank field. Each issue's outcome
// is the one the client's batch reports for it.
func (p *Provider) Rank(ctx context.Context, in tracker.RankInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepRank, err, in.Issues...)
	}
	if err := rankChecks(in); err != nil {
		return tracker.Invalid(tracker.StepRank, err, in.Issues...)
	}
	b, err := p.board(ctx, in.Board)
	if err != nil {
		return batchParts(in.Board, tracker.StepRank, in.Issues, jira.BatchResult{}, err)
	}
	pos := jira.RankBefore(string(in.Next.Issue))
	if in.Prev != nil {
		pos = jira.RankAfter(string(in.Prev.Issue))
	}
	res, err := p.jira.Rank(ctx, jira.RankInput{Issues: refs(in.Issues), Position: pos, RankFieldID: b.cfg.RankFieldID})
	return batchParts(in.Board, tracker.StepRank, in.Issues, res, err)
}

func rankChecks(in tracker.RankInput) error {
	if err := checkIssues(in.Issues); err != nil {
		return err
	}
	for _, n := range []*tracker.Neighbor{in.Prev, in.Next} {
		if n != nil {
			if err := checkIssue(n.Issue); err != nil {
				return err
			}
		}
	}
	return nil
}

// MoveToSprint moves issues into a sprint, or to the board's backlog when
// the input names no sprint. Each issue's outcome is the one the client's
// batch reports for it.
func (p *Provider) MoveToSprint(ctx context.Context, in tracker.SprintMoveInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprintMove, err, in.Issues...)
	}
	if err := checkIssues(in.Issues); err != nil {
		return tracker.Invalid(tracker.StepSprintMove, err, in.Issues...)
	}
	board, err := boardNumber(in.Board)
	if err != nil {
		return tracker.Invalid(tracker.StepSprintMove, err, in.Issues...)
	}
	var res jira.BatchResult
	if in.Sprint == "" {
		res, err = p.jira.MoveToBacklog(ctx, jira.BacklogMove{Board: board, Issues: refs(in.Issues)})
	} else {
		sprint, serr := sprintNumber(in.Sprint)
		if serr != nil {
			return tracker.Invalid(tracker.StepSprintMove, serr, in.Issues...)
		}
		res, err = p.jira.MoveToSprint(ctx, jira.SprintMove{Sprint: sprint, Issues: refs(in.Issues)})
	}
	return batchParts(in.Board, tracker.StepSprintMove, in.Issues, res, err)
}

// batchParts are one part per issue of a batch: the client's outcome for
// each issue it reports, and for a batch refused before any group was
// sent the outcome of that refusal for every issue. A rank or sprint move
// may change the issue's row on the board and its card on every board.
func batchParts(board tracker.BoardID, step tracker.Step, issues []tracker.IssueID, res jira.BatchResult, err error) tracker.Result {
	parts := make([]tracker.Part, len(issues))
	for i, id := range issues {
		parts[i] = tracker.Part{Issue: id, Step: step, Changed: []tracker.Resource{tracker.RowOf(board, id), tracker.CardOf(id)}}
	}
	if len(res.Issues) != len(issues) {
		if err == nil {
			err = fmt.Errorf("jiraprovider: the %s reported %d of %d issues", step, len(res.Issues), len(issues))
		}
		for i := range parts {
			parts[i].Outcome = beforeSend(err)
		}
		return tracker.Result{Parts: parts}
	}
	for i, o := range res.Issues {
		switch o.Outcome {
		case jira.OutcomeDone:
			parts[i].Outcome = tracker.Applied()
		case jira.OutcomeRefused:
			parts[i].Outcome = tracker.Refused(tracker.Refusal{Reason: tracker.ReasonTracker, Message: o.Reason, Err: err})
		case jira.OutcomeUnknown:
			parts[i].Outcome = tracker.Unknown(err)
		default:
			parts[i].Outcome = tracker.NotSent(err)
		}
	}
	return tracker.Result{Parts: parts}
}

func refs(ids []tracker.IssueID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
