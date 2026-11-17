package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/trackerapi"
)

// Worklog is a record of time spent on an issue in Jira's own time
// tracking. A site that uses Tempo keeps its full record there; this is
// what Jira holds.
type Worklog struct {
	ID           string
	IssueID      string
	Author       *User
	UpdateAuthor *User
	Comment      *adf.Node // nil when there is none
	Started      time.Time
	TimeSpent    time.Duration
	Created      time.Time
	Updated      time.Time
	// Visibility is a role or group the worklog is restricted to; nil
	// when everyone who can see the issue sees it.
	Visibility json.RawMessage
}

type worklogJSON struct {
	ID               string          `json:"id"`
	IssueID          string          `json:"issueId"`
	Author           *userJSON       `json:"author"`
	UpdateAuthor     *userJSON       `json:"updateAuthor"`
	Comment          json.RawMessage `json:"comment"`
	Started          string          `json:"started"`
	TimeSpentSeconds int64           `json:"timeSpentSeconds"`
	Created          string          `json:"created"`
	Updated          string          `json:"updated"`
	Visibility       json.RawMessage `json:"visibility"`
}

// worklog decodes w; a comment that is not a document or a start that is
// not a timestamp is an error, with the rest of the worklog returned.
func (w worklogJSON) worklog() (Worklog, error) {
	out := Worklog{
		ID: w.ID, IssueID: w.IssueID, Author: w.Author.userPtr(), UpdateAuthor: w.UpdateAuthor.userPtr(),
		TimeSpent: time.Duration(w.TimeSpentSeconds) * time.Second,
	}
	out.Created, _ = parseTime(w.Created)
	out.Updated, _ = parseTime(w.Updated)
	if !isNull(w.Visibility) {
		out.Visibility = w.Visibility
	}
	var err error
	if out.Started, err = parseTime(w.Started); err != nil {
		return out, fmt.Errorf("worklog %s: %w", w.ID, err)
	}
	if !isNull(w.Comment) {
		if err := decodeDoc(w.Comment, &out.Comment); err != nil {
			return out, fmt.Errorf("worklog %s: %w", w.ID, err)
		}
	}
	return out, nil
}

func worklogPath(issue, id string) (string, error) {
	path, err := issuePath(issue)
	if err != nil {
		return "", err
	}
	if err := checkID("worklog", id); err != nil {
		return "", err
	}
	return apiPath + path + "/worklog/" + url.PathEscape(id), nil
}

// WorklogQuery picks an issue's worklogs; a zero time bounds nothing.
type WorklogQuery struct {
	Issue         string
	StartedAfter  time.Time
	StartedBefore time.Time
}

// WorklogList is the worklogs a read returned, oldest first. Complete is
// false when the walk stopped early or, for WorklogsByID, when Jira
// returned fewer worklogs than were named.
type WorklogList struct {
	Worklogs []Worklog
	Complete bool
}

// Worklogs reads an issue's worklogs page by page. A failure after some
// pages returns them with the error.
func (c *Client) Worklogs(ctx context.Context, q WorklogQuery) (WorklogList, error) {
	var out WorklogList
	if err := c.admit(ctx); err != nil {
		return out, err
	}
	path, err := issuePath(q.Issue)
	if err != nil {
		return out, err
	}
	v := url.Values{"maxResults": {"1000"}}
	if !q.StartedAfter.IsZero() {
		v.Set("startedAfter", strconv.FormatInt(q.StartedAfter.UnixMilli(), 10))
	}
	if !q.StartedBefore.IsZero() {
		v.Set("startedBefore", strconv.FormatInt(q.StartedBefore.UnixMilli(), 10))
	}
	for start := 0; ; {
		v.Set("startAt", strconv.Itoa(start))
		var page struct {
			Worklogs []worklogJSON `json:"worklogs"`
			Total    *int          `json:"total"`
		}
		if err := c.get(ctx, path+"/worklog", v, &page); err != nil {
			return out, fmt.Errorf("jira: worklogs of %s: %w", q.Issue, err)
		}
		for _, j := range page.Worklogs {
			w, err := j.worklog()
			if err != nil {
				return out, fmt.Errorf("jira: worklogs of %s: %w", q.Issue, &AnswerError{Message: err.Error()})
			}
			out.Worklogs = append(out.Worklogs, w)
		}
		if page.Total == nil {
			// Only the total ends the walk; without it the end is unknown.
			return out, fmt.Errorf("jira: worklogs of %s: %w", q.Issue, &AnswerError{Message: "a worklog page names no total"})
		}
		start += len(page.Worklogs)
		switch {
		case start >= *page.Total:
			out.Complete = true
			return out, nil
		case len(page.Worklogs) == 0:
			return out, fmt.Errorf("jira: worklogs of %s: an empty page at %d of %d", q.Issue, start, *page.Total)
		}
	}
}

// Worklog returns one worklog.
func (c *Client) Worklog(ctx context.Context, issue, id string) (Worklog, error) {
	if err := c.admit(ctx); err != nil {
		return Worklog{}, err
	}
	return c.worklog(ctx, issue, id)
}

func (c *Client) worklog(ctx context.Context, issue, id string) (Worklog, error) {
	path, err := worklogPath(issue, id)
	if err != nil {
		return Worklog{}, err
	}
	var j worklogJSON
	if _, err := c.call(ctx, trackerapi.Request{Path: path}, &j); err != nil {
		return Worklog{}, fmt.Errorf("jira: worklog %s on %s: %w", id, issue, err)
	}
	w, err := j.worklog()
	if err != nil {
		return Worklog{}, fmt.Errorf("jira: %w", &AnswerError{Message: err.Error()})
	}
	return w, nil
}

// EstimateAdjust is how a worklog write changes the issue's remaining
// estimate. The zero EstimateAdjust is Jira's default: AdjustAuto.
type EstimateAdjust struct {
	mode adjustMode
	by   time.Duration
}

type adjustMode uint8

const (
	adjustAuto adjustMode = iota
	adjustLeave
	adjustNew
	adjustReduce
	adjustIncrease
)

// AdjustAuto reduces the remaining estimate by the time logged (by the
// change in time on an update, and adds it back on a delete).
func AdjustAuto() EstimateAdjust { return EstimateAdjust{} }

// LeaveEstimate leaves the remaining estimate as it is.
func LeaveEstimate() EstimateAdjust { return EstimateAdjust{mode: adjustLeave} }

// SetRemaining sets the remaining estimate to d.
func SetRemaining(d time.Duration) EstimateAdjust { return EstimateAdjust{mode: adjustNew, by: d} }

// ReduceRemaining reduces the remaining estimate by d; adding a worklog
// only.
func ReduceRemaining(d time.Duration) EstimateAdjust {
	return EstimateAdjust{mode: adjustReduce, by: d}
}

// IncreaseRemaining increases the remaining estimate by d; deleting a
// worklog only.
func IncreaseRemaining(d time.Duration) EstimateAdjust {
	return EstimateAdjust{mode: adjustIncrease, by: d}
}

// query sets a's parameters on q for a write that allows the modes in
// allowed; a duration is written in whole minutes.
func (a EstimateAdjust) query(q url.Values, allowed ...adjustMode) error {
	if !slices.Contains(allowed, a.mode) {
		return notSent("this worklog write does not take that estimate adjustment")
	}
	minutes := func(least time.Duration) (string, error) {
		if a.by < least || a.by%time.Minute != 0 {
			return "", notSent("an estimate adjustment of %s is not a whole number of minutes from %s", a.by, least)
		}
		return strconv.FormatInt(int64(a.by/time.Minute), 10) + "m", nil
	}
	var err error
	var by string
	switch a.mode {
	case adjustAuto:
		q.Set("adjustEstimate", "auto")
	case adjustLeave:
		q.Set("adjustEstimate", "leave")
	case adjustNew:
		by, err = minutes(0)
		q.Set("adjustEstimate", "new")
		q.Set("newEstimate", by)
	case adjustReduce:
		by, err = minutes(time.Minute)
		q.Set("adjustEstimate", "manual")
		q.Set("reduceBy", by)
	case adjustIncrease:
		by, err = minutes(time.Minute)
		q.Set("adjustEstimate", "manual")
		q.Set("increaseBy", by)
	}
	return err
}

// WorklogInput is a new worklog.
type WorklogInput struct {
	Issue string
	// Started is when the work began. It is sent as its instant with its
	// own UTC offset (2026-11-16T09:30:00.000+0100); Jira keeps the
	// instant, and a worklog read back carries the offset Jira answers
	// with. This is the one time zone rule of worklogs.
	Started time.Time
	// TimeSpent is the time worked, in whole seconds.
	TimeSpent time.Duration
	// Markdown is the comment, empty for none. It names no local images.
	Markdown string
	Estimate EstimateAdjust
}

// AddWorklog adds a worklog. Its Markdown is converted and its mentions
// resolved before anything is sent. A worklog stored whose answer did
// not decode is a StepWorklog *StepError whose Applied reports it, with
// what of the worklog decoded.
func (c *Client) AddWorklog(ctx context.Context, in WorklogInput) (Worklog, error) {
	if err := c.admit(ctx); err != nil {
		return Worklog{}, err
	}
	path, err := issuePath(in.Issue)
	if err != nil {
		return Worklog{}, err
	}
	q := url.Values{}
	if err := in.Estimate.query(q, adjustAuto, adjustLeave, adjustNew, adjustReduce); err != nil {
		return Worklog{}, err
	}
	if in.Started.IsZero() {
		return Worklog{}, notSent("a worklog on %s needs its start", in.Issue)
	}
	if err := checkTimeSpent(in.TimeSpent); err != nil {
		return Worklog{}, err
	}
	body := map[string]any{
		"started":          in.Started.Format(jiraTimeLayout),
		"timeSpentSeconds": int64(in.TimeSpent / time.Second),
	}
	if in.Markdown != "" {
		doc, _, err := c.worklogDoc(ctx, in.Issue, in.Markdown, nil)
		if err != nil {
			return Worklog{}, notSentErr(fmt.Errorf("jira: worklog on %s: %w", in.Issue, err))
		}
		body["comment"] = doc
	}
	w, err := c.writeWorklog(ctx, http.MethodPost, apiPath+path+"/worklog", q, body)
	if err != nil {
		return w, fmt.Errorf("jira: worklog on %s: %w", in.Issue, err)
	}
	return w, nil
}

func checkTimeSpent(d time.Duration) error {
	if d <= 0 || d%time.Second != 0 {
		return notSent("a worklog's time spent of %s is not a positive whole number of seconds", d)
	}
	return nil
}

// worklogDoc converts a worklog comment, editing base when it is set.
// Mentions resolve against the issue; local images are refused, since a
// worklog write is one request.
func (c *Client) worklogDoc(ctx context.Context, issue, md string, base *adf.Node) (*adf.Node, []adf.Warning, error) {
	b := &builder{c: c, opts: ValueOptions{Issue: issue}, stage: newImageStage("", c.maxUpload)}
	return b.markdown(ctx, md, base)
}

// writeWorklog sends a worklog write and decodes the worklog Jira stored.
// A failure is a StepWorklog *StepError; an answer that does not decode
// is one whose Applied reports it.
func (c *Client) writeWorklog(ctx context.Context, method, path string, q url.Values, body any) (Worklog, error) {
	var j worklogJSON
	ids, err := c.write(ctx, trackerapi.Request{Method: method, Path: path, Query: q, Body: body}, &j)
	if err != nil {
		known := Worklog{}
		if numericRE.MatchString(ids.id) {
			known.ID = ids.id
		}
		return known, &StepError{Step: StepWorklog, Err: err}
	}
	w, err := j.worklog()
	switch {
	case err != nil:
		return w, &StepError{Step: StepWorklog, Err: &AnswerError{Message: "the worklog was stored, but its answer did not decode: " + err.Error()}}
	case !numericRE.MatchString(w.ID):
		return w, &StepError{Step: StepWorklog, Err: &AnswerError{Message: "the worklog's answer names no worklog ID"}}
	}
	return w, nil
}

// WorklogEdit changes a worklog; a zero or nil field keeps the stored
// value.
type WorklogEdit struct {
	Issue     string
	ID        string
	Started   time.Time
	TimeSpent time.Duration
	// Markdown replaces the comment, edited from the stored one so what
	// Markdown cannot show survives where the text is unchanged.
	Markdown *string
	// From is the stored comment the Markdown was written from; when set,
	// a worklog that holds another comment by now is a *StaleError.
	From *adf.Node
	// AcceptLoss is the user's acceptance that the edit loses content
	// Markdown cannot show; without it such an edit is a *adf.LossError.
	AcceptLoss bool
	// Estimate takes AdjustAuto, LeaveEstimate or SetRemaining.
	Estimate EstimateAdjust
}

// UpdateWorklog reads the worklog, checks the edit against it and writes
// the fields that change, keeping its visibility. An edit that changes
// nothing sends nothing and returns the worklog as read.
func (c *Client) UpdateWorklog(ctx context.Context, in WorklogEdit) (Worklog, error) {
	if err := c.admit(ctx); err != nil {
		return Worklog{}, err
	}
	path, err := worklogPath(in.Issue, in.ID)
	if err != nil {
		return Worklog{}, err
	}
	q := url.Values{}
	if err := in.Estimate.query(q, adjustAuto, adjustLeave, adjustNew); err != nil {
		return Worklog{}, err
	}
	if in.TimeSpent != 0 {
		if err := checkTimeSpent(in.TimeSpent); err != nil {
			return Worklog{}, err
		}
	}
	cur, err := c.worklog(ctx, in.Issue, in.ID)
	if err != nil {
		return Worklog{}, notSentErr(err)
	}
	if in.From != nil && !adf.Equal(in.From, cur.Comment) {
		return Worklog{}, notSentErr(&StaleError{Issue: in.Issue, What: "worklog " + in.ID})
	}
	body := map[string]any{}
	if !in.Started.IsZero() && !in.Started.Equal(cur.Started) {
		body["started"] = in.Started.Format(jiraTimeLayout)
	}
	if in.TimeSpent != 0 && in.TimeSpent != cur.TimeSpent {
		body["timeSpentSeconds"] = int64(in.TimeSpent / time.Second)
	}
	if in.Markdown != nil {
		doc, ws, err := c.worklogDoc(ctx, in.Issue, *in.Markdown, cur.Comment)
		if err == nil {
			err = adf.CheckLoss(ws, in.AcceptLoss)
		}
		if err != nil {
			return Worklog{}, notSentErr(fmt.Errorf("jira: edit worklog %s on %s: %w", in.ID, in.Issue, err))
		}
		if !adf.Equal(doc, cur.Comment) {
			body["comment"] = doc
		}
	}
	if len(body) == 0 {
		return cur, nil
	}
	if cur.Visibility != nil {
		// A write without it may show a restricted worklog to everyone.
		body["visibility"] = cur.Visibility
	}
	w, err := c.writeWorklog(ctx, http.MethodPut, path, q, body)
	if err != nil {
		return w, fmt.Errorf("jira: edit worklog %s on %s: %w", in.ID, in.Issue, err)
	}
	return w, nil
}

// DeleteWorklog deletes a worklog. estimate takes AdjustAuto,
// LeaveEstimate, SetRemaining or IncreaseRemaining.
func (c *Client) DeleteWorklog(ctx context.Context, issue, id string, estimate EstimateAdjust) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	path, err := worklogPath(issue, id)
	if err != nil {
		return err
	}
	q := url.Values{}
	if err := estimate.query(q, adjustAuto, adjustLeave, adjustNew, adjustIncrease); err != nil {
		return err
	}
	if _, err := c.call(ctx, trackerapi.Request{Method: http.MethodDelete, Path: path, Query: q}, nil); err != nil {
		return fmt.Errorf("jira: delete worklog %s on %s: %w", id, issue, err)
	}
	return nil
}
