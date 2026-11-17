package jira

import (
	"context"
	"errors"
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
// 50 that chain as Rank's do. The sprint is read first, and a closed one
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
	return c.runBatch(ctx, batchWrite{
		step: StepMove, path: path, method: http.MethodPost,
		issues: in.Issues, pos: in.Position, field: in.RankFieldID,
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

// CompletionResult is how far a completion got: the unfinished issues
// found, what their moves did, and whether the sprint was closed.
type CompletionResult struct {
	Unfinished []string
	Moves      BatchResult
	Closed     bool
	// Sprint is the sprint as Jira stored it once closed.
	Sprint *Sprint
}

// CompleteSprint completes an active sprint. Jira's API takes no
// destination for unfinished issues, so this moves them first: it reads
// the sprint, the destination, the board's done column and every issue of
// the sprint, refusing before any write when one of them is not as
// needed; moves the issues whose status is not in the done column to
// MoveTo or the backlog; and closes the sprint only when every move was
// done. A failure says how far it got: a move that failed or whose
// outcome is unknown leaves the sprint open, with the moves in Moves.
func (c *Client) CompleteSprint(ctx context.Context, in SprintCompletion) (CompletionResult, error) {
	var res CompletionResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	path, err := sprintPath(in.Sprint)
	if err != nil {
		return res, err
	}
	unfinished, err := c.unfinishedIssues(ctx, in)
	if err != nil {
		return res, notSentErr(fmt.Errorf("jira: complete sprint %d: %w", in.Sprint, err))
	}
	res.Unfinished = unfinished
	if len(unfinished) > 0 {
		w := batchWrite{step: StepMove, path: agilePath + "backlog/issue", method: http.MethodPost, issues: unfinished}
		if in.MoveTo > 0 {
			w.path = agilePath + "sprint/" + strconv.FormatInt(in.MoveTo, 10) + "/issue"
		}
		res.Moves, err = c.runBatch(ctx, w)
		if err != nil {
			return res, fmt.Errorf("jira: complete sprint %d: the sprint stays open: %w", in.Sprint, err)
		}
	}
	s, err := c.writeSprint(ctx, http.MethodPost, path, map[string]any{"state": SprintClosed})
	if err != nil {
		if se, ok := errors.AsType[*StepError](err); ok && se.Applied() {
			res.Closed, res.Sprint = true, &s
		}
		return res, fmt.Errorf("jira: complete sprint %d: %w", in.Sprint, err)
	}
	res.Closed, res.Sprint = true, &s
	return res, nil
}

// unfinishedIssues checks a completion can run and returns the keys of
// the sprint's issues outside the board's done column, in rank order.
func (c *Client) unfinishedIssues(ctx context.Context, in SprintCompletion) ([]string, error) {
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
	list, err := c.listAll(ctx, ListQuery{List: SprintIssues(in.Sprint), Fields: []string{"status"}}, 0)
	if err != nil {
		return nil, err
	}
	if !list.Complete {
		return nil, notSent("the sprint's issues were not all listed (%d warnings), so its unfinished issues are not known", len(list.Warnings))
	}
	var out []string
	for _, is := range list.Issues {
		if is.Status == nil {
			return nil, notSent("%s came without a status, so whether it is done is not known", is.Key)
		}
		if !slices.Contains(done, is.Status.ID) {
			out = append(out, is.Key)
		}
	}
	return out, nil
}
