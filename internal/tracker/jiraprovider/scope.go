package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// A write's input names an issue, a board, a sprint, a comment, a link or a
// neighbor by Jira's ID, which is the site's. Where Jira's request does not
// itself tie the thing written to the issue or board the input names, the
// provider reads that tie first and refuses, before sending, what is not
// tied (ReasonInvalid, naming it). The reads narrow the window in which a
// write reaches something that moved meanwhile and do not close it: Jira
// takes no precondition on a write.
//
// Tied by the request itself: a create, an edit, a move, a comment added,
// an attach (each under its issue's path), a link (both issues are its
// body), a sprint created (on its board) and a move to a board's backlog
// (Jira moves only issues the board has). Checked here: a rank's issues and
// the neighbor it ranks against, a sprint move's issues and sprint, an
// estimate's issue, a sprint started, edited or completed and the sprint a
// completion moves issues to, an unlink, and in the time trackers a
// worklog edited or deleted. A comment edited or deleted is read first
// through its issue's path, which Jira answers not found for another
// issue's comment (measured on reads; writes are not measured). A
// download is checked in Download.

// onBoard reports which of ids board b lists now, read through the board's
// issue list, a hundred IDs to a request.
func (p *Provider) onBoard(ctx context.Context, b board, ids []tracker.IssueID) (map[tracker.IssueID]bool, error) {
	listed := map[tracker.IssueID]bool{}
	for chunk := range slices.Chunk(distinct(ids), idChunk) {
		list, err := p.listByID(ctx, b, chunk, []string{"status"})
		if err != nil {
			return nil, fmt.Errorf("jiraprovider: which issues board %s lists: %w", b.id, err)
		}
		for _, is := range list {
			listed[tracker.IssueID(is.ID)] = true
		}
	}
	return listed, nil
}

// boardParts runs write on the issues of ids that listed holds, and
// refuses the others, which board does not list now, before anything is
// sent. The result has one part per issue of ids, in their order; write
// returns one part per issue it is given.
func boardParts(board tracker.BoardID, step tracker.Step, ids []tracker.IssueID, listed map[tracker.IssueID]bool, write func(send []tracker.IssueID) tracker.Result) tracker.Result {
	var send []tracker.IssueID
	for _, id := range ids {
		if listed[id] {
			send = append(send, id)
		}
	}
	sent := map[tracker.IssueID]tracker.Part{}
	if len(send) > 0 {
		for _, part := range write(send).Parts {
			sent[part.Issue] = part
		}
	}
	out := make([]tracker.Part, len(ids))
	for i, id := range ids {
		if listed[id] {
			out[i] = sent[id]
			continue
		}
		err := invalidf("issue %s is not on board %s now; read the board again", id, board)
		out[i] = tracker.Part{Issue: id, Step: step, Outcome: tracker.Refused(tracker.Refusal{Reason: tracker.ReasonInvalid, Message: err.Error(), Err: err})}
	}
	return tracker.Result{Parts: out}
}

// boardSprints are the sprints board lists in states, every state when
// none is named, by ID with their state.
func (p *Provider) boardSprints(ctx context.Context, board tracker.BoardID, states ...jira.SprintState) (map[int64]jira.SprintState, error) {
	n, err := boardNumber(board)
	if err != nil {
		return nil, err
	}
	list, err := p.jira.Sprints(ctx, jira.SprintQuery{Board: n, States: states})
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: sprints of board %s: %w", board, err)
	}
	out := make(map[int64]jira.SprintState, len(list.Sprints))
	for _, s := range list.Sprints {
		out[s.ID] = s.State
	}
	return out, nil
}

// boardSprint is sprint's number when board lists it in one of states,
// every state when none is named, and a refusal otherwise.
func (p *Provider) boardSprint(ctx context.Context, board tracker.BoardID, sprint tracker.SprintID, states ...jira.SprintState) (int64, error) {
	n, err := sprintNumber(sprint)
	if err != nil {
		return 0, err
	}
	listed, err := p.boardSprints(ctx, board, states...)
	if err != nil {
		return 0, err
	}
	if _, ok := listed[n]; !ok {
		return 0, notBoardSprint(board, sprint, states)
	}
	return n, nil
}

// notBoardSprint is the refusal of a sprint board does not list in
// states.
func notBoardSprint(board tracker.BoardID, sprint tracker.SprintID, states []jira.SprintState) error {
	if len(states) == 0 {
		return invalidf("sprint %s is not a sprint of board %s", sprint, board)
	}
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = string(s)
	}
	return invalidf("sprint %s is not a sprint of board %s that is %s", sprint, board, strings.Join(names, " or "))
}

// readComment reads comment through issue's path. Jira answers that read
// not found for a comment of another issue (measured), so a confirmed not
// found is a *notOnIssueError.
func (p *Provider) readComment(ctx context.Context, issue tracker.IssueID, comment tracker.CommentID) (jira.Comment, error) {
	c, err := p.jira.Comment(ctx, string(issue), string(comment))
	switch {
	case errors.Is(err, trackerapi.ErrNotFound):
		return jira.Comment{}, &notOnIssueError{what: "comment " + string(comment), issue: issue, err: err}
	case err != nil:
		return jira.Comment{}, fmt.Errorf("jiraprovider: read comment %s: %w", comment, err)
	}
	return c, nil
}

// notOnIssueError refuses a write of something Jira does not find
// through the issue its input names: it is gone, or another issue's. It
// matches tracker.ErrInvalidInput, and err, the client's not found, stays
// reachable.
type notOnIssueError struct {
	what  string
	issue tracker.IssueID
	err   error
}

func (e *notOnIssueError) Error() string {
	return fmt.Sprintf("jiraprovider: %s is not on issue %s: it is gone or belongs to another issue", e.what, e.issue)
}

func (e *notOnIssueError) Unwrap() []error { return []error{tracker.ErrInvalidInput, e.err} }

// checkLink refuses link unless it is one of issue's links and its other
// side is other: a link's ID is the site's, and its delete names nothing
// else.
func (p *Provider) checkLink(ctx context.Context, issue, other tracker.IssueID, link tracker.LinkID) error {
	is, err := p.jira.GetIssue(ctx, string(issue), "issuelinks")
	if err != nil {
		return fmt.Errorf("jiraprovider: links of %s: %w", issue, err)
	}
	for _, pr := range is.Problems {
		if pr.Field == "issuelinks" {
			return fmt.Errorf("jiraprovider: links of %s: %w", issue, pr.Err)
		}
	}
	if !slices.ContainsFunc(is.Links, func(l jira.IssueLink) bool { return l.ID == string(link) && l.Issue.ID == string(other) }) {
		return invalidf("link %s is not between issues %s and %s", link, issue, other)
	}
	return nil
}

// checkWorklogIssue refuses a worklog read as cur that is not on the
// issue from names: a worklog's ID is the site's (Tempo's), or names an
// issue Jira does not document that it checks (Jira's).
func checkWorklogIssue(cur, from tracker.Worklog) error {
	if cur.Issue != from.Issue {
		return invalidf("worklog %s is not issue %s's", from.ID, from.Issue)
	}
	return nil
}
