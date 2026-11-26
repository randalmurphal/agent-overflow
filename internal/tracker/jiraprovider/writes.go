package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// A write's parts follow the client's steps. A step the client did not
// reach is not sent; the step that failed is judged by outcome; the steps
// before it were applied. What a write's images stored is what the client
// reports stored, never inferred from its error: an applied upload part
// stands for every attachment the client names in its result, and a
// second upload part, with the failure's outcome, for the attach request
// that failed. Nothing is sent again: an unknown outcome is reported as
// it is.

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

// uploadParts are the upload parts of a write of issue that embeds
// images: one applied when the client reports an attachment stored, and
// one with the failure's outcome when an attach request failed.
func uploadParts(issue tracker.IssueID, stored []jira.Attachment, err error) []tracker.Part {
	var parts []tracker.Part
	if len(stored) > 0 {
		parts = append(parts, tracker.Part{Issue: issue, Step: tracker.StepUpload, Outcome: tracker.Applied(), Changed: issueResources(issue)})
	}
	if stepOf(err) == jira.StepUpload {
		parts = append(parts, tracker.Part{Issue: issue, Step: tracker.StepUpload, Outcome: outcome(err), Changed: issueResources(issue)})
	}
	return parts
}

// mainOutcome is the outcome of the write a write's images are for: not
// sent when it failed before its first request or at an image's upload or
// media ID read, and its own answer's otherwise.
func mainOutcome(err error) tracker.Outcome {
	switch stepOf(err) {
	case "", jira.StepCheck:
		if err != nil {
			return beforeSend(err)
		}
	case jira.StepUpload, jira.StepMediaID:
		return tracker.NotSent(fmt.Errorf("jiraprovider: not sent after an image's upload failed: %w", err))
	case jira.StepCreate, jira.StepEdit, jira.StepFinish, jira.StepTransition, jira.StepComment, jira.StepRemoteLink,
		jira.StepRank, jira.StepMove, jira.StepSprint, jira.StepEstimate, jira.StepWorklog:
		// The write's own step: its answer.
	}
	return outcome(err)
}

// stepParts are the parts of a write of issue whose main step is step:
// the upload parts of the images stored, which the client reports in
// stored, and of the attach that failed, then the main part.
func stepParts(issue tracker.IssueID, step tracker.Step, stored []jira.Attachment, err error) tracker.Result {
	main := tracker.Part{Issue: issue, Step: step, Outcome: mainOutcome(err), Changed: issueResources(issue)}
	return tracker.Result{Parts: append(uploadParts(issue, stored, err), main)}
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
	parts := []tracker.Part{{Issue: id, Step: tracker.StepCreate, Outcome: tracker.Applied(), Changed: issueResources(id), Key: res.Key}}
	if !images {
		return tracker.Result{Parts: parts}
	}
	parts = append(parts, uploadParts(id, res.Uploaded, err)...)
	finish := tracker.Part{Issue: id, Step: tracker.StepEdit, Outcome: mainOutcome(err), Changed: issueResources(id)}
	return tracker.Result{Parts: append(parts, finish)}
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
		return stepParts(in.Issue, tracker.StepEdit, nil, err)
	}
	plan, err := p.jira.PlanEdit(ctx, jira.EditInput{
		Issue: string(in.Issue), Fields: inputs, ImageDir: in.ImageDir, AcceptLoss: in.AcceptLoss,
	})
	if err != nil {
		return stepParts(in.Issue, tracker.StepEdit, nil, err)
	}
	res, err := p.jira.ApplyEdit(ctx, plan)
	return stepParts(in.Issue, tracker.StepEdit, res.Uploaded, err)
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
		return stepParts(in.Issue, tracker.StepMove, nil, err)
	}
	res, err := p.jira.Transition(ctx, jira.TransitionInput{
		Issue: string(in.Issue), Transition: string(in.Move), Fields: inputs, ImageDir: in.ImageDir, AcceptLoss: in.AcceptLoss,
	})
	return stepParts(in.Issue, tracker.StepMove, res.Uploaded, err)
}

// SetEstimate writes an issue's estimate through the board's estimation
// field, in the board's unit: points, or seconds for original time,
// which Jira takes in whole minutes. Zero is an estimate of zero, as the
// jira client writes it; a nil Value clears. An issue the board does not
// list is refused before anything is sent.
func (p *Provider) SetEstimate(ctx context.Context, in tracker.EstimateInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepEstimate, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepEstimate, err, in.Issue)
	}
	part := func(o tracker.Outcome) tracker.Result {
		return tracker.Result{Parts: []tracker.Part{{Issue: in.Issue, Step: tracker.StepEstimate, Outcome: o, Changed: boardResources(tracker.StepEstimate, in.Board, in.Issue)}}}
	}
	b, err := p.board(ctx, in.Board)
	if err != nil {
		return part(beforeSend(err))
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
	listed, err := p.onBoard(ctx, b, []tracker.IssueID{in.Issue})
	if err != nil {
		return part(beforeSend(err))
	}
	return boardParts(in.Board, tracker.StepEstimate, []tracker.IssueID{in.Issue}, listed, func([]tracker.IssueID) tracker.Result {
		_, _, err := p.jira.SetEstimate(ctx, jira.EstimateInput{Issue: string(in.Issue), Board: b.cfg.BoardID, Value: value})
		return part(outcome(err))
	})
}

// Rank places issues on a board next to the input's anchor, through the
// board's rank field: just after Prev, or just before Next when there is
// no Prev. A neighbor the board does not list, or two neighbors no longer
// in the order named, refuses the rank; an issue the board does not list
// is refused and the others are ranked. Each issue's
// outcome is the one the client's batch reports for it.
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
	anchor, after := in.Anchor()
	neighbors := slices.DeleteFunc([]tracker.IssueID{in.Prev, in.Next}, func(id tracker.IssueID) bool { return id == "" })
	listed, err := p.onBoard(ctx, b, append(neighbors, in.Issues...), b.rank)
	if err == nil {
		err = neighborsStand(in, b, listed)
	}
	if err != nil {
		return batchParts(in.Board, tracker.StepRank, in.Issues, jira.BatchResult{}, err)
	}
	pos := jira.RankBefore(string(anchor))
	if after {
		pos = jira.RankAfter(string(anchor))
	}
	return boardParts(in.Board, tracker.StepRank, in.Issues, listed, func(send []tracker.IssueID) tracker.Result {
		res, err := p.jira.Rank(ctx, jira.RankInput{Issues: refs(send), Position: pos, RankFieldID: b.cfg.RankFieldID})
		return batchParts(in.Board, tracker.StepRank, send, res, err)
	})
}

// neighborsStand refuses a rank whose neighbors board b does not list
// now, or, when both are named, whose Prev no longer sorts before its
// Next by the board's rank value, bytewise as the order key sorts: the
// board moved since it was read, and the issues would not land between
// them.
func neighborsStand(in tracker.RankInput, b board, listed map[tracker.IssueID]*jira.Issue) error {
	for _, n := range []tracker.IssueID{in.Prev, in.Next} {
		if n != "" && listed[n] == nil {
			return movedf("issue %s, which the issues go next to, is not on board %s now; read the board again", n, in.Board)
		}
	}
	if in.Prev == "" || in.Next == "" {
		return nil
	}
	prev, err := listed[in.Prev].Field(b.rank).Text()
	if err != nil {
		return fmt.Errorf("jiraprovider: the rank of %s on board %s: %w", in.Prev, in.Board, err)
	}
	next, err := listed[in.Next].Field(b.rank).Text()
	if err != nil {
		return fmt.Errorf("jiraprovider: the rank of %s on board %s: %w", in.Next, in.Board, err)
	}
	if prev >= next {
		return movedf("issues %s and %s, which the issues go between, no longer stand in that order on board %s; read the board again", in.Prev, in.Next, in.Board)
	}
	return nil
}

func rankChecks(in tracker.RankInput) error {
	if err := checkIssues(in.Issues); err != nil {
		return err
	}
	for _, n := range []tracker.IssueID{in.Prev, in.Next} {
		if n != "" {
			if err := checkIssue(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// MoveToSprint moves issues into an active or future sprint the board
// lists, or to the board's backlog when the input names no sprint. A
// sprint the board does not list open refuses the move; an issue the
// board does not list is refused and the others are moved. Each issue's
// outcome is the one the client's batch reports for it.
func (p *Provider) MoveToSprint(ctx context.Context, in tracker.SprintMoveInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepSprintMove, err, in.Issues...)
	}
	if err := checkIssues(in.Issues); err != nil {
		return tracker.Invalid(tracker.StepSprintMove, err, in.Issues...)
	}
	board, err := boardNumber(in.Board)
	if err == nil && in.Sprint != "" {
		_, err = sprintNumber(in.Sprint)
	}
	if err != nil {
		return tracker.Invalid(tracker.StepSprintMove, err, in.Issues...)
	}
	notSent := func(err error) tracker.Result {
		return batchParts(in.Board, tracker.StepSprintMove, in.Issues, jira.BatchResult{}, err)
	}
	b, err := p.board(ctx, in.Board)
	if err != nil {
		return notSent(err)
	}
	listed, err := p.onBoard(ctx, b, in.Issues)
	if err != nil {
		return notSent(err)
	}
	// The sprint's listing is the last read: the client takes it as read.
	var sprint *jira.Sprint
	if in.Sprint != "" {
		if sprint, err = p.boardSprint(ctx, in.Board, in.Sprint, jira.SprintActive, jira.SprintFuture); err != nil {
			return notSent(err)
		}
	}
	return boardParts(in.Board, tracker.StepSprintMove, in.Issues, listed, func(send []tracker.IssueID) tracker.Result {
		var res jira.BatchResult
		var err error
		if sprint == nil {
			res, err = p.jira.MoveToBacklog(ctx, jira.BacklogMove{Board: board, Issues: refs(send)})
		} else {
			res, err = p.jira.MoveToSprint(ctx, jira.SprintMove{Sprint: sprint.ID, Issues: refs(send), Current: sprint})
		}
		return batchParts(in.Board, tracker.StepSprintMove, send, res, err)
	})
}

// boardResources are what a write of an issue on a board may change: its
// row on the board, its card (read through every board the mirror has it
// on, which covers the boards sharing this one's rank or estimate field)
// and, but for a rank, its detail, which shows its sprints and estimate.
func boardResources(step tracker.Step, board tracker.BoardID, issue tracker.IssueID) []tracker.Resource {
	out := []tracker.Resource{tracker.RowOf(board, issue), tracker.CardOf(issue)}
	if step != tracker.StepRank {
		out = append(out, tracker.DetailOf(issue))
	}
	return out
}

// batchParts are one part per issue of a batch: the client's outcome for
// each issue it reports, and for a batch refused before any group was
// sent the outcome of that refusal for every issue. Each part names
// boardResources.
func batchParts(board tracker.BoardID, step tracker.Step, issues []tracker.IssueID, res jira.BatchResult, err error) tracker.Result {
	parts := make([]tracker.Part, len(issues))
	for i, id := range issues {
		parts[i] = tracker.Part{Issue: id, Step: step, Changed: boardResources(step, board, id)}
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
		case jira.OutcomeNotSent:
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
