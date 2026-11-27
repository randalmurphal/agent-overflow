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
	// Problems names the fields whose value did not decode (started,
	// created, updated, comment); the rest of the worklog is usable. An
	// edit refuses to rewrite a comment that did not decode.
	Problems []FieldProblem
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

// worklog decodes w; a timestamp that does not parse or a comment that is
// not a document is one of its Problems.
func (w worklogJSON) worklog() Worklog {
	out := Worklog{
		ID: w.ID, IssueID: w.IssueID, Author: w.Author.userPtr(), UpdateAuthor: w.UpdateAuthor.userPtr(),
		TimeSpent: time.Duration(w.TimeSpentSeconds) * time.Second,
	}
	timeField("started", w.Started, &out.Started, &out.Problems)
	timeField("created", w.Created, &out.Created, &out.Problems)
	timeField("updated", w.Updated, &out.Updated, &out.Problems)
	if !isNull(w.Visibility) {
		out.Visibility = w.Visibility
	}
	if !isNull(w.Comment) {
		if err := decodeDoc(w.Comment, &out.Comment); err != nil {
			out.Comment = nil
			out.Problems = append(out.Problems, FieldProblem{Field: "comment", Err: err})
		}
	}
	return out
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

// Worklogs reads the worklog field of an issue read with it: the
// worklogs Jira embedded, oldest first. Complete reports that they are
// every worklog of the issue; Jira embeds the first 20 with the total
// (measured on Jira Cloud, docs/references/tracker-api-measurements.md),
// so an issue with more is read with Client.Worklogs.
func (v FieldValue) Worklogs() (WorklogList, error) {
	var page struct {
		StartAt  int           `json:"startAt"`
		Total    *int          `json:"total"`
		Worklogs []worklogJSON `json:"worklogs"`
	}
	if err := v.decode(&page); err != nil {
		return WorklogList{}, err
	}
	if page.Total == nil {
		return WorklogList{}, &AnswerError{Message: "the worklog field names no total"}
	}
	out := WorklogList{Worklogs: make([]Worklog, len(page.Worklogs))}
	for i, j := range page.Worklogs {
		out.Worklogs[i] = j.worklog()
	}
	out.Complete = page.StartAt == 0 && len(page.Worklogs) >= *page.Total
	return out, nil
}

// Worklogs reads an issue's worklogs page by page. A failure after some
// pages returns them with the error.
func (c *Client) Worklogs(ctx context.Context, q WorklogQuery) (WorklogList, error) {
	if err := c.admit(ctx); err != nil {
		return WorklogList{}, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (WorklogList, error) { return c.worklogs(ctx, q) })
}

// worklogs is one walk of Worklogs.
func (c *Client) worklogs(ctx context.Context, q WorklogQuery) (WorklogList, error) {
	var out WorklogList
	err := worklogPages(ctx, c, q, func(j worklogJSON) error {
		out.Worklogs = append(out.Worklogs, j.worklog())
		return nil
	})
	out.Complete = err == nil
	return out, err
}

// maxWorklogPage is the most worklogs a page asks for.
const maxWorklogPage = 1000

// worklogPages is one walk of the pages of q's issue's worklogs, each
// decoded as J and given to each in Jira's order, oldest first: the one
// walk every read of an issue's worklogs makes. It ends at the total the
// pages name, and fails at a page that names none, at an empty page
// before it, and at an error of each. A page over the transport's bound
// is asked again with half as many (trackerapi.Halved), and the pages
// after it keep the smaller size.
func worklogPages[J any](ctx context.Context, c *Client, q WorklogQuery, each func(J) error) error {
	path, err := issuePath(q.Issue)
	if err != nil {
		return err
	}
	v := url.Values{}
	if !q.StartedAfter.IsZero() {
		v.Set("startedAfter", strconv.FormatInt(q.StartedAfter.UnixMilli(), 10))
	}
	if !q.StartedBefore.IsZero() {
		v.Set("startedBefore", strconv.FormatInt(q.StartedBefore.UnixMilli(), 10))
	}
	size := maxWorklogPage
	for start := 0; ; {
		v.Set("startAt", strconv.Itoa(start))
		v.Set("maxResults", strconv.Itoa(size))
		var page struct {
			Worklogs []J  `json:"worklogs"`
			Total    *int `json:"total"`
		}
		err := c.get(ctx, path+"/worklog", v, &page)
		if half, ok := trackerapi.Halved(size, err); ok {
			size = half
			continue
		}
		if err != nil {
			return fmt.Errorf("jira: worklogs of %s: %w", q.Issue, err)
		}
		for _, j := range page.Worklogs {
			if err := each(j); err != nil {
				return fmt.Errorf("jira: worklogs of %s: %w", q.Issue, err)
			}
		}
		if page.Total == nil {
			// Only the total ends the walk; without it the end is unknown.
			return fmt.Errorf("jira: worklogs of %s: %w", q.Issue, &AnswerError{Message: "a worklog page names no total"})
		}
		start += len(page.Worklogs)
		switch {
		case start >= *page.Total:
			return nil
		case len(page.Worklogs) == 0:
			return fmt.Errorf("jira: worklogs of %s: an empty page at %d of %d", q.Issue, start, *page.Total)
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
	return j.worklog(), nil
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
	w, err := c.writeWorklog(ctx, http.MethodPost, apiPath+path+"/worklog", "", q, body)
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
	b := &builder{c: c, opts: ValueOptions{Issue: issue}, stage: newImageStage("", c.maxImages)}
	return b.markdown(ctx, md, base)
}

// writeWorklog sends a worklog write and decodes the worklog Jira stored:
// an add (id empty; documented: 201 with the new worklog) or an update of
// worklog id (200 with that worklog). A failure is a StepWorklog
// *StepError; an Applied one returns what of the worklog decoded, its ID
// at least, and any other returns the zero Worklog.
func (c *Client) writeWorklog(ctx context.Context, method, path, id string, q url.Values, body any) (Worklog, error) {
	want := bodyAnswer(namesID, http.StatusCreated)
	if id != "" {
		want = bodyAnswer(namesThisID(id), http.StatusOK)
	}
	var j worklogJSON
	w, err := c.write(ctx, trackerapi.Request{Method: method, Path: path, Query: q, Body: body}, want, &j)
	wl := j.worklog()
	wl.ID = w.id()
	if err != nil {
		return wl, &StepError{Step: StepWorklog, Err: err}
	}
	return wl, nil
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
	// Documented: 204.
	if _, err := c.write(ctx, trackerapi.Request{Method: http.MethodDelete, Path: path, Query: q}, emptyAnswer(http.StatusNoContent), nil); err != nil {
		return fmt.Errorf("jira: delete worklog %s on %s: %w", id, issue, err)
	}
	return nil
}
