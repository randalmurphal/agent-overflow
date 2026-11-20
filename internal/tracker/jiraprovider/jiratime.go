package jiraprovider

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
)

// addHour is the hour of the author's day a new Jira worklog starts at:
// a neutral worklog has a day and no time.
const addHour = 9

// jiraTime is a Jira connection's logged time in Jira's own worklogs. A
// worklog's day is the day its start falls on in its author's time zone,
// which Jira's profile gives; its description is the comment as
// Markdown.
type jiraTime struct {
	jira *jira.Client
}

// zone is u's time zone; an account whose zone is hidden or unknown is an
// error, since the days and times read for it would be wrong.
func zone(u jira.User) (*time.Location, error) {
	if u.TimeZone == "" {
		return nil, fmt.Errorf("jiraprovider: Jira does not say which time zone %s is in", u.AccountID)
	}
	loc, err := time.LoadLocation(u.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: the time zone of %s: %w", u.AccountID, err)
	}
	return loc, nil
}

// Worklogs finds the issues the author logged time on around the days
// asked, then reads each issue's worklogs that started then and keeps the
// author's whose day is q.From to q.To. worklogDate is a date field, with
// no time to give as milliseconds, and JQL reads a date in the reader's
// zone, not the author's, so the search widens the days by one either
// side and the worklogs' start times decide.
func (t *jiraTime) Worklogs(ctx context.Context, q tracker.WorklogQuery) ([]tracker.Worklog, error) {
	if err := q.Check(); err != nil {
		return nil, err
	}
	author, err := t.jira.User(ctx, string(q.Author))
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: worklogs of %s: %w", q.Author, err)
	}
	loc, err := zone(author)
	if err != nil {
		return nil, err
	}
	from := time.Date(q.From.Year(), q.From.Month(), q.From.Day(), 0, 0, 0, 0, loc)
	to := time.Date(q.To.Year(), q.To.Month(), q.To.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	jql := "worklogAuthor = " + jira.QuoteJQL(author.AccountID) +
		" AND worklogDate >= " + jira.QuoteJQL(from.AddDate(0, 0, -1).Format(time.DateOnly)) +
		" AND worklogDate <= " + jira.QuoteJQL(to.Format(time.DateOnly))
	res, err := t.jira.SearchAll(ctx, jira.SearchQuery{JQL: jql, Fields: []string{"status"}}, 0)
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: issues %s logged time on: %w", q.Author, err)
	}
	if !res.Complete {
		return nil, fmt.Errorf("jiraprovider: issues %s logged time on: Jira warned that the search is incomplete", q.Author)
	}
	var out []tracker.Worklog
	for _, is := range res.Issues {
		list, err := t.jira.Worklogs(ctx, jira.WorklogQuery{Issue: is.ID, StartedAfter: from.Add(-time.Millisecond), StartedBefore: to})
		if err != nil {
			return nil, fmt.Errorf("jiraprovider: worklogs of %s: %w", is.Key, err)
		}
		for _, w := range list.Worklogs {
			if w.Author == nil || w.Author.AccountID != author.AccountID {
				continue
			}
			n, err := nativeWorklog(w, is.ID, loc)
			if err != nil {
				return nil, err
			}
			if n.Day.Compare(q.From) >= 0 && n.Day.Compare(q.To) <= 0 {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// AddWorklog logs time for the credential's own user, starting at
// addHour of the day in the user's zone.
func (t *jiraTime) AddWorklog(ctx context.Context, in tracker.WorklogInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.Issue)
	}
	if err := checkIssue(in.Issue); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.Issue)
	}
	me, err := t.jira.Myself(ctx)
	if err != nil {
		return worklogResult(in.Issue, "", beforeSend(fmt.Errorf("jiraprovider: who logs the time: %w", err)))
	}
	loc, err := zone(me)
	if err != nil {
		return worklogResult(in.Issue, tracker.UserID(me.AccountID), beforeSend(err), in.Day)
	}
	_, err = t.jira.AddWorklog(ctx, jira.WorklogInput{
		Issue: string(in.Issue), Started: time.Date(in.Day.Year(), in.Day.Month(), in.Day.Day(), addHour, 0, 0, 0, loc),
		TimeSpent: in.Spent, Markdown: in.Description, Estimate: jira.AdjustAuto(),
	})
	return worklogResult(in.Issue, tracker.UserID(me.AccountID), outcome(err), in.Day)
}

// EditWorklog changes a Jira worklog that still holds what From says,
// keeping its start's time of day when it moves to another day; the
// client checks it again as its last read before the write.
func (t *jiraTime) EditWorklog(ctx context.Context, in tracker.WorklogEdit) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	if err := checkWorklog(in.From); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	days := editDays(in)
	read, cur, loc, err := t.readWorklog(ctx, in.From)
	if err == nil && !sameWorklog(cur, in.From) {
		err = stale(string(in.From.Issue), "worklog "+string(in.From.ID))
	}
	if err != nil {
		return worklogResult(in.From.Issue, in.From.Author, beforeSend(err), days...)
	}
	edit := jira.WorklogEdit{Issue: string(in.From.Issue), ID: string(in.From.ID), From: &read, Markdown: in.Description, AcceptLoss: in.AcceptLoss}
	if in.Spent != nil {
		edit.TimeSpent = *in.Spent
	}
	if d := in.Day; d != nil {
		s := read.Started.In(loc)
		edit.Started = time.Date(d.Year(), d.Month(), d.Day(), s.Hour(), s.Minute(), s.Second(), s.Nanosecond(), loc)
	}
	_, err = t.jira.UpdateWorklog(ctx, edit)
	return worklogResult(in.From.Issue, in.From.Author, outcome(err), days...)
}

// DeleteWorklog deletes a Jira worklog of the issue From names, giving its
// time back to the issue's remaining estimate. The worklog is read first,
// so the result names its author's worklogs of its day as Jira holds them.
func (t *jiraTime) DeleteWorklog(ctx context.Context, in tracker.WorklogDelete) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	if err := checkWorklog(in.From); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	_, cur, _, err := t.readWorklog(ctx, in.From)
	if err != nil {
		return worklogResult(in.From.Issue, in.From.Author, beforeSend(err), in.From.Day)
	}
	err = t.jira.DeleteWorklog(ctx, string(in.From.Issue), string(in.From.ID), jira.AdjustAuto())
	return worklogResult(in.From.Issue, cur.Author, outcome(err), deleteDays(cur, in.From)...)
}

func checkWorklog(w tracker.Worklog) error {
	if err := checkIssue(w.Issue); err != nil {
		return err
	}
	_, err := numericID("worklog", string(w.ID))
	return err
}

// readWorklog reads the worklog from names, as Jira holds it, and as a
// neutral worklog in its author's zone, which it returns too. A worklog
// whose own issue is not from's is refused.
func (t *jiraTime) readWorklog(ctx context.Context, from tracker.Worklog) (jira.Worklog, tracker.Worklog, *time.Location, error) {
	read, err := t.jira.Worklog(ctx, string(from.Issue), string(from.ID))
	if err != nil {
		return jira.Worklog{}, tracker.Worklog{}, nil, fmt.Errorf("jiraprovider: read worklog %s: %w", from.ID, err)
	}
	if read.Author == nil || read.IssueID == "" {
		return jira.Worklog{}, tracker.Worklog{}, nil, fmt.Errorf("jiraprovider: worklog %s names no author or issue", from.ID)
	}
	author, err := t.jira.User(ctx, read.Author.AccountID)
	if err != nil {
		return jira.Worklog{}, tracker.Worklog{}, nil, fmt.Errorf("jiraprovider: the author of worklog %s: %w", from.ID, err)
	}
	loc, err := zone(author)
	if err != nil {
		return jira.Worklog{}, tracker.Worklog{}, nil, err
	}
	cur, err := nativeWorklog(read, read.IssueID, loc)
	if err == nil {
		err = checkWorklogIssue(cur, from)
	}
	if err != nil {
		return jira.Worklog{}, tracker.Worklog{}, nil, err
	}
	return read, cur, loc, nil
}

// nativeWorklog is w, a worklog of issue, as a neutral worklog whose day
// is in loc. A start or comment that did not decode fails it.
func nativeWorklog(w jira.Worklog, issue string, loc *time.Location) (tracker.Worklog, error) {
	if len(w.Problems) > 0 || w.Started.IsZero() {
		return tracker.Worklog{}, fmt.Errorf("jiraprovider: worklog %s of %s did not read in full", w.ID, issue)
	}
	day, err := tracker.DateOf(w.Started.In(loc))
	if err != nil {
		return tracker.Worklog{}, fmt.Errorf("jiraprovider: worklog %s: %w", w.ID, err)
	}
	out := tracker.Worklog{
		ID: tracker.WorklogID(w.ID), Issue: tracker.IssueID(issue), Day: day,
		Spent: w.TimeSpent, Description: adf.ToMarkdown(w.Comment), Updated: w.Updated,
	}
	if w.Author != nil {
		out.Author = tracker.UserID(w.Author.AccountID)
	}
	return out, nil
}
