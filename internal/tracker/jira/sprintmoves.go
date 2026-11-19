package jira

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// SprintMove moves Issues into an open sprint.
type SprintMove struct {
	Sprint int64
	Issues []string
	// Position ranks the issues as they move; zero leaves their rank.
	Position RankPosition
	// RankFieldID is the rank field's number when Position is set; zero
	// uses Jira's default.
	RankFieldID int64
}

// MoveToSprint moves issues into a future or active sprint, in groups of
// 50 that chain as Rank's do. Jira documents only a 204 for it, so any
// other answer leaves a group's outcome unknown. The sprint is read first, and a closed one
// is refused before anything is sent. A failure is a *BatchError with the
// result saying what happened to each issue.
func (c *Client) MoveToSprint(ctx context.Context, in SprintMove) (BatchResult, error) {
	if err := c.admit(ctx); err != nil {
		return BatchResult{}, err
	}
	path, err := sprintPath(in.Sprint)
	if err != nil {
		return BatchResult{}, err
	}
	if err := checkBatch(in.Issues, in.Position, in.RankFieldID); err != nil {
		return BatchResult{}, err
	}
	sp, err := c.sprint(ctx, in.Sprint)
	if err != nil {
		return BatchResult{}, notSentErr(err)
	}
	if sp.State != SprintFuture && sp.State != SprintActive {
		return BatchResult{}, notSent("sprint %d (%s) is %s; issues move into a future or active sprint", sp.ID, sp.Name, sp.State)
	}
	return c.runBatch(ctx, batchWrite{
		step: StepMove, path: path + "/issue", method: http.MethodPost,
		issues: in.Issues, pos: in.Position, field: in.RankFieldID,
	})
}

// BacklogMove moves Issues to the backlog.
type BacklogMove struct {
	// Board names the board whose backlog the issues go to, which they
	// must be on already; zero takes them out of every future and active
	// sprint. A Position needs a Board.
	Board    int64
	Issues   []string
	Position RankPosition
	// RankFieldID is the rank field's number when Position is set.
	RankFieldID int64
}

// MoveToBacklog moves issues to the backlog, in groups of 50 that chain
// as Rank's do. This is also how an issue leaves a sprint: Jira has no
// other call that takes issues out of one. Issues keep their closed
// sprints. A failure is a *BatchError with the result saying what
// happened to each issue.
func (c *Client) MoveToBacklog(ctx context.Context, in BacklogMove) (BatchResult, error) {
	if err := c.admit(ctx); err != nil {
		return BatchResult{}, err
	}
	if err := checkBatch(in.Issues, in.Position, in.RankFieldID); err != nil {
		return BatchResult{}, err
	}
	path := agilePath + "backlog/issue"
	switch {
	case in.Board < 0:
		return BatchResult{}, checkAgileID("board", in.Board)
	case in.Board > 0:
		path = agilePath + "backlog/" + strconv.FormatInt(in.Board, 10) + "/issue"
	case in.Position.set():
		return BatchResult{}, notSent("a backlog position needs the board whose backlog it is")
	}
	// Only the board's backlog is documented to answer 207.
	return c.runBatch(ctx, batchWrite{
		step: StepMove, path: path, method: http.MethodPost,
		issues: in.Issues, pos: in.Position, field: in.RankFieldID, multi: in.Board > 0,
	})
}

// SprintCompletion closes an active sprint once its unfinished issues
// have moved on.
type SprintCompletion struct {
	Sprint int64
	// MoveTo is the future or active sprint unfinished issues move to;
	// zero moves them to the backlog.
	MoveTo int64
	// Board is the board whose done column says which issues are
	// finished; zero takes the sprint's own board.
	Board int64
}

// SprintIssue is an issue of a sprint by its numeric ID and its key.
type SprintIssue struct {
	ID  string
	Key string
}

// CompletionResult is how far a completion got: the unfinished issues
// found, what their moves did, the unfinished issues still in the sprint
// after the moves (which keep it open), and whether it was closed.
type CompletionResult struct {
	// Unfinished are the issues found unfinished, in rank order. The moves
	// send them by ID, so Moves reports each under its ID, in the same
	// order.
	Unfinished []SprintIssue
	Moves      BatchResult
	Remaining  []SprintIssue
	Closed     bool
	// Sprint is the sprint as Jira stored it once closed.
	Sprint *Sprint
}

// sprintIssueIDs are the IDs of issues.
func sprintIssueIDs(issues []SprintIssue) []string {
	out := make([]string, len(issues))
	for i, is := range issues {
		out[i] = is.ID
	}
	return out
}

// CompleteSprint completes an active sprint. Jira's API takes no
// destination for unfinished issues, so this moves them first: it reads
// the sprint, the destination, the board's done column and every issue of
// the sprint, refusing before any write when one of them is not as
// needed; moves the issues whose status is not in the done column to
// MoveTo or the backlog, by ID; lists the sprint's issues again; and closes the
// sprint only when every move was done and no unfinished issue is left
// in it. An unfinished issue added meanwhile is in Remaining, with the
// sprint left open, so the caller can run the completion again. Jira
// takes no precondition on the source sprint, so an issue someone else
// moved between the listing and the move is still moved by this call. A
// failure says how far it got; the close answer is read as every write's
// is (see write), so only an answer naming the sprint reports it closed.
func (c *Client) CompleteSprint(ctx context.Context, in SprintCompletion) (CompletionResult, error) {
	var res CompletionResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	path, err := sprintPath(in.Sprint)
	if err != nil {
		return res, err
	}
	done, err := c.completionChecks(ctx, in)
	if err == nil {
		res.Unfinished, err = c.unfinishedIssues(ctx, in.Sprint, done)
	}
	if err != nil {
		return res, notSentErr(fmt.Errorf("jira: complete sprint %d: %w", in.Sprint, err))
	}
	if len(res.Unfinished) > 0 {
		w := batchWrite{step: StepMove, path: agilePath + "backlog/issue", method: http.MethodPost, issues: sprintIssueIDs(res.Unfinished)}
		if in.MoveTo > 0 {
			w.path = agilePath + "sprint/" + strconv.FormatInt(in.MoveTo, 10) + "/issue"
		}
		res.Moves, err = c.runBatch(ctx, w)
		if err != nil {
			return res, fmt.Errorf("jira: complete sprint %d: the sprint stays open: %w", in.Sprint, err)
		}
		// The last read before the close.
		if res.Remaining, err = c.unfinishedIssues(ctx, in.Sprint, done); err == nil && len(res.Remaining) > 0 {
			err = fmt.Errorf("%d unfinished issues are in the sprint after the moves; run the completion again", len(res.Remaining))
		}
		if err != nil {
			return res, &StepError{Step: StepSprint, Err: notSentErr(fmt.Errorf("jira: complete sprint %d: the sprint stays open: %w", in.Sprint, err))}
		}
	}
	s, err := c.writeSprint(ctx, http.MethodPost, path, in.Sprint, map[string]any{"state": SprintClosed})
	if err != nil {
		if answerApplied(err) {
			res.Closed, res.Sprint = true, &s
		}
		return res, fmt.Errorf("jira: complete sprint %d: %w", in.Sprint, err)
	}
	res.Closed, res.Sprint = true, &s
	return res, nil
}

// completionChecks checks a completion can run: the sprint is active, the
// destination open, and the board has a done column, whose statuses it
// returns.
func (c *Client) completionChecks(ctx context.Context, in SprintCompletion) ([]string, error) {
	switch {
	case in.MoveTo < 0:
		return nil, checkAgileID("sprint", in.MoveTo)
	case in.MoveTo == in.Sprint:
		return nil, notSent("unfinished issues cannot move to the sprint being completed")
	}
	sp, err := c.sprint(ctx, in.Sprint)
	if err != nil {
		return nil, err
	}
	if sp.State != SprintActive {
		return nil, notSent("sprint %d is %s; only an active sprint completes", sp.ID, sp.State)
	}
	if in.MoveTo > 0 {
		to, err := c.sprint(ctx, in.MoveTo)
		if err != nil {
			return nil, err
		}
		if to.State != SprintFuture && to.State != SprintActive {
			return nil, notSent("sprint %d (%s) is %s; unfinished issues move to a future or active sprint", to.ID, to.Name, to.State)
		}
	}
	board := in.Board
	if board == 0 {
		board = sp.BoardID
	}
	cfg, err := c.boardConfig(ctx, board)
	if err != nil {
		return nil, err
	}
	done := cfg.DoneStatusIDs()
	if len(done) == 0 {
		return nil, notSent("board %d has no column with statuses, so no issue reads as done", board)
	}
	return done, nil
}

// unfinishedIssues lists every issue of sprint and returns those whose
// status is not in done, in rank order. A listing Jira warned about, or
// an issue listed without its status or numeric ID, is an *AnswerError:
// the unfinished issues are not known, or a move could not name one.
func (c *Client) unfinishedIssues(ctx context.Context, sprint int64, done []string) ([]SprintIssue, error) {
	list, err := c.listAll(ctx, ListQuery{List: SprintIssues(sprint), Fields: []string{"status"}}, 0)
	if err != nil {
		return nil, err
	}
	if !list.Complete {
		return nil, &AnswerError{Message: fmt.Sprintf("the sprint's issues were not all listed (%d warnings), so its unfinished issues are not known", len(list.Warnings))}
	}
	var out []SprintIssue
	for _, is := range list.Issues {
		if is.Status == nil {
			return nil, &AnswerError{Message: fmt.Sprintf("%s came without a status, so whether it is done is not known", is.Key)}
		}
		if !numericRE.MatchString(is.ID) {
			return nil, &AnswerError{Message: fmt.Sprintf("the sprint listed %s without its numeric ID", is.Key)}
		}
		if !slices.Contains(done, is.Status.ID) {
			out = append(out, SprintIssue{ID: is.ID, Key: is.Key})
		}
	}
	return out, nil
}
