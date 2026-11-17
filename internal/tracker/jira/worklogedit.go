package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"agent-overflow/internal/tracker/adf"
)

// WorklogEdit changes a worklog; a zero or nil field keeps the stored
// value. The edit says what it is based on, and has no default: From is
// the worklog as the caller read it, so a worklog changed since (its
// comment, an absent one included, its start or its time spent) is a
// *StaleError; Unchecked edits whatever is stored, with no check. One of
// the two is required.
type WorklogEdit struct {
	Issue     string
	ID        string
	From      *Worklog
	Unchecked bool
	Started   time.Time
	TimeSpent time.Duration
	// Markdown replaces the comment, edited from the base comment (From's,
	// or the stored one when Unchecked) so what Markdown cannot show
	// survives where the text is unchanged.
	Markdown *string
	// AcceptLoss is the user's acceptance that the edit loses content
	// Markdown cannot show; without it such an edit is a *adf.LossError.
	AcceptLoss bool
	// Estimate takes AdjustAuto, LeaveEstimate or SetRemaining. SetRemaining
	// is a change on its own; AdjustAuto and LeaveEstimate change nothing
	// without another field.
	Estimate EstimateAdjust
}

// WorklogUpdate is what UpdateWorklog did: the worklog as Jira stored it,
// or as read when Unchanged reports that it already held every value
// asked for, so nothing was sent.
type WorklogUpdate struct {
	Worklog   Worklog
	Unchanged bool
}

// UpdateWorklog converts the Markdown (its mentions read first), then
// reads the worklog as the last read before the write, refuses a stale
// base, and writes the fields that change, keeping the worklog's
// visibility. The check narrows the window for a lost update and does not
// close it: Jira takes no precondition on the write, so a change between
// that read and the write is overwritten. An edit that asks for nothing is
// refused before anything is sent.
func (c *Client) UpdateWorklog(ctx context.Context, in WorklogEdit) (WorklogUpdate, error) {
	var res WorklogUpdate
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	path, err := worklogPath(in.Issue, in.ID)
	if err != nil {
		return res, err
	}
	if err := in.check(); err != nil {
		return res, err
	}
	q := url.Values{}
	if err := in.Estimate.query(q, adjustAuto, adjustLeave, adjustNew); err != nil {
		return res, err
	}
	base, cur := in.From, (*Worklog)(nil)
	if base == nil {
		if cur, err = c.readWorklog(ctx, in.Issue, in.ID); err != nil {
			return res, err
		}
		base = cur
	}
	var doc *adf.Node
	if in.Markdown != nil {
		if doc, err = c.worklogEditDoc(ctx, in, base); err != nil {
			return res, err
		}
	}
	if cur == nil {
		if cur, err = c.readWorklog(ctx, in.Issue, in.ID); err != nil {
			return res, err
		}
		if !sameWorklog(base, cur) {
			return res, notSentErr(&StaleError{Issue: in.Issue, What: "worklog " + in.ID})
		}
	}
	body := in.changes(cur, doc)
	if len(body) == 0 {
		return WorklogUpdate{Worklog: *cur, Unchanged: true}, nil
	}
	if cur.Visibility != nil {
		// A write without it may show a restricted worklog to everyone.
		body["visibility"] = cur.Visibility
	}
	res.Worklog, err = c.writeWorklog(ctx, http.MethodPut, path, in.ID, q, body)
	if err != nil {
		return res, fmt.Errorf("jira: edit worklog %s on %s: %w", in.ID, in.Issue, err)
	}
	return res, nil
}

// check refuses an edit without a base or that asks for nothing.
func (in WorklogEdit) check() error {
	switch {
	case (in.From == nil) == !in.Unchecked:
		return notSent("edit worklog %s: give From, the worklog the edit is based on, or Unchecked, not both", in.ID)
	case in.From != nil && in.From.ID != in.ID:
		return notSent("edit worklog %s: From is worklog %s", in.ID, in.From.ID)
	case in.Started.IsZero() && in.TimeSpent == 0 && in.Markdown == nil && in.Estimate.mode != adjustNew:
		return notSent("edit worklog %s: nothing to change", in.ID)
	case in.TimeSpent != 0:
		return checkTimeSpent(in.TimeSpent)
	}
	return nil
}

// readWorklog reads a worklog an edit depends on; a failure is the edit's
// refusal before sending.
func (c *Client) readWorklog(ctx context.Context, issue, id string) (*Worklog, error) {
	w, err := c.worklog(ctx, issue, id)
	if err != nil {
		return nil, notSentErr(err)
	}
	return &w, nil
}

// worklogEditDoc converts the edit's Markdown against base's comment.
func (c *Client) worklogEditDoc(ctx context.Context, in WorklogEdit, base *Worklog) (*adf.Node, error) {
	if err := problemIn(base.Problems, "comment"); err != nil {
		return nil, notSentErr(fmt.Errorf("jira: edit worklog %s on %s: its comment did not decode, so an edit cannot keep it: %w", in.ID, in.Issue, err))
	}
	doc, ws, err := c.worklogDoc(ctx, in.Issue, *in.Markdown, base.Comment)
	if err == nil {
		err = adf.CheckLoss(ws, in.AcceptLoss)
	}
	if err != nil {
		return nil, notSentErr(fmt.Errorf("jira: edit worklog %s on %s: %w", in.ID, in.Issue, err))
	}
	return doc, nil
}

// sameWorklog reports a stored worklog that still holds what an edit
// based on base can overwrite: the comment (an absent one included), the
// start and the time spent.
func sameWorklog(base, cur *Worklog) bool {
	return adf.Equal(base.Comment, cur.Comment) && base.Started.Equal(cur.Started) && base.TimeSpent == cur.TimeSpent &&
		(problemIn(base.Problems, "comment") == nil) == (problemIn(cur.Problems, "comment") == nil)
}

// changes is the body of an edit against cur: the fields that differ, and
// for an edit that sets the remaining estimate on its own, the time spent
// as stored, so the write carries the adjustment.
func (in WorklogEdit) changes(cur *Worklog, doc *adf.Node) map[string]any {
	body := map[string]any{}
	if !in.Started.IsZero() && !in.Started.Equal(cur.Started) {
		body["started"] = in.Started.Format(jiraTimeLayout)
	}
	if in.TimeSpent != 0 && in.TimeSpent != cur.TimeSpent {
		body["timeSpentSeconds"] = int64(in.TimeSpent / time.Second)
	}
	if in.Markdown != nil && !adf.Equal(doc, cur.Comment) {
		body["comment"] = doc
	}
	if len(body) == 0 && in.Estimate.mode == adjustNew {
		body["timeSpentSeconds"] = int64(cur.TimeSpent / time.Second)
	}
	return body
}
