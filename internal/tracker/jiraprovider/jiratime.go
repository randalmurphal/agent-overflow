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

// zone is u's time zone; an account whose zone is hidden (matching
// tracker.ErrNoTimeZone) or unknown is an error, since the days and times
// read for it would be wrong.
func zone(u jira.User) (*time.Location, error) {
	if u.TimeZone == "" {
		return nil, fmt.Errorf("jiraprovider: %s: %w", u.AccountID, tracker.ErrNoTimeZone)
	}
	loc, err := time.LoadLocation(u.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: the time zone of %s: %w", u.AccountID, err)
	}
	return loc, nil
}

// Worklogs finds the issues the author logged time on around the days
// asked, with the worklogs Jira embeds in each, while it reads the
// author's time zone, and keeps the author's worklogs whose day is q.From
// to q.To. Jira embeds an issue's first 20 worklogs with their total
// (measured), so only an issue with more is read again, its worklogs
// that started around the days. worklogDate is a date field, with no
// time to give as milliseconds, and JQL reads a date in the reader's
// zone, not the author's, so the search widens the days by one either
// side and the worklogs' start times decide. Jira's worklog/updated feed
// is not used: it lists every worklog of the site changed since a time,
// not the author's of the days asked.
func (t *jiraTime) Worklogs(ctx context.Context, q tracker.WorklogQuery) ([]tracker.Worklog, error) {
	if err := q.Check(); err != nil {
		return nil, err
	}
	// The search names only the account and the days, a day either side,
	// so it runs while the author's zone is read.
	wide := func(d tracker.Date, days int) string {
		return jira.QuoteJQL(time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, days).Format(time.DateOnly))
	}
	jql := "worklogAuthor = " + jira.QuoteJQL(string(q.Author)) +
		" AND worklogDate >= " + wide(q.From, -1) + " AND worklogDate <= " + wide(q.To, 1) +
		// Paged in an order that cannot change between pages: Jira returns
		// an unordered search in no stable order (measured).
		" ORDER BY id ASC"
	// The lookup's failure wins over the search's: a person Jira will not
	// look up is the answer whichever request Jira answers first, so the
	// search's failure does not end the lookup, and the lookup's ends the
	// search.
	var (
		res       jira.SearchResult
		searchErr error
	)
	searchCtx, stopSearch := context.WithCancel(ctx)
	searched := make(chan struct{})
	go func() {
		defer close(searched)
		res, searchErr = t.jira.SearchAll(searchCtx, jira.SearchQuery{JQL: jql, Fields: []string{"worklog"}}, 0)
	}()
	author, err := t.jira.User(ctx, string(q.Author))
	if err != nil {
		stopSearch()
	}
	<-searched
	stopSearch()
	switch {
	case err != nil:
		return nil, fmt.Errorf("jiraprovider: worklogs of %s: %w: %w", q.Author, tracker.ErrWorklogAuthor, err)
	case searchErr != nil:
		return nil, fmt.Errorf("jiraprovider: issues %s logged time on: %w: %w", q.Author, tracker.ErrWorklogSearch, searchErr)
	}
	if !res.Complete {
		return nil, fmt.Errorf("jiraprovider: issues %s logged time on: Jira warned that the search is incomplete", q.Author)
	}
	loc, err := zone(author)
	if err != nil {
		return nil, err
	}
	from := time.Date(q.From.Year(), q.From.Month(), q.From.Day(), 0, 0, 0, 0, loc)
	to := time.Date(q.To.Year(), q.To.Month(), q.To.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	var out []tracker.Worklog
	for _, is := range res.Issues {
		list, err := is.Field("worklog").Worklogs()
		if err != nil {
			return nil, fmt.Errorf("jiraprovider: worklogs of %s: %w", is.Key, err)
		}
		if !list.Complete {
			if list, err = t.jira.Worklogs(ctx, jira.WorklogQuery{Issue: is.ID, StartedAfter: from.Add(-time.Millisecond), StartedBefore: to}); err != nil {
				return nil, fmt.Errorf("jiraprovider: worklogs of %s: %w", is.Key, err)
			}
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
	me, err := t.jira.Me(ctx)
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
// keeping its start's time of day when it moves to another day. The
// worklog's read that checks it is the last request before the write
// (see readWorklog); when another request came after it, the client
// reads the worklog again.
func (t *jiraTime) EditWorklog(ctx context.Context, in tracker.WorklogEdit) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	if err := checkWorklog(in.From); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	days := editDays(in)
	read, cur, loc, last, err := t.readWorklog(ctx, in.From)
	if err == nil && !sameWorklog(cur, in.From) {
		err = stale(string(in.From.Issue), "worklog "+string(in.From.ID))
	}
	if err != nil {
		return worklogResult(in.From.Issue, in.From.Author, beforeSend(err), days...)
	}
	edit := jira.WorklogEdit{Issue: string(in.From.Issue), ID: string(in.From.ID), Markdown: in.Description, AcceptLoss: in.AcceptLoss}
	if last {
		// Nothing was sent since the read, so the client takes it as it is
		// and reads it again only when the description names someone.
		edit.Current = &read
	} else {
		edit.From = &read
	}
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

// DeleteWorklog deletes a Jira worklog that still holds what From says,
// giving its time back to the issue's remaining estimate. The worklog is
// read first, so the result names its author's worklogs of its day as
// Jira holds them.
func (t *jiraTime) DeleteWorklog(ctx context.Context, in tracker.WorklogDelete) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	if err := checkWorklog(in.From); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	_, cur, _, _, err := t.readWorklog(ctx, in.From)
	if err == nil && !sameWorklog(cur, in.From) {
		err = stale(string(in.From.Issue), "worklog "+string(in.From.ID))
	}
	if err != nil {
		return worklogResult(in.From.Issue, in.From.Author, beforeSend(err), in.From.Day)
	}
	err = t.jira.DeleteWorklog(ctx, string(in.From.Issue), string(in.From.ID), jira.AdjustAuto())
	return worklogResult(in.From.Issue, in.From.Author, outcome(err), in.From.Day)
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
// whose own issue is not from's is refused. The zone of from's author is
// read first, so while the worklog is still that author's its read is
// the last request readWorklog makes, which last reports; another
// author's zone, or the zone when from names no author, is read after
// it.
func (t *jiraTime) readWorklog(ctx context.Context, from tracker.Worklog) (read jira.Worklog, cur tracker.Worklog, loc *time.Location, last bool, err error) {
	var named *time.Location
	if from.Author != "" {
		if named, err = t.authorZone(ctx, string(from.Author), from.ID); err != nil {
			return jira.Worklog{}, tracker.Worklog{}, nil, false, err
		}
	}
	read, err = t.jira.Worklog(ctx, string(from.Issue), string(from.ID))
	if err != nil {
		return jira.Worklog{}, tracker.Worklog{}, nil, false, fmt.Errorf("jiraprovider: read worklog %s: %w", from.ID, err)
	}
	if read.Author == nil || read.IssueID == "" {
		return jira.Worklog{}, tracker.Worklog{}, nil, false, fmt.Errorf("jiraprovider: worklog %s names no author or issue", from.ID)
	}
	loc, last = named, true
	if named == nil || read.Author.AccountID != string(from.Author) {
		if loc, err = t.authorZone(ctx, read.Author.AccountID, from.ID); err != nil {
			return jira.Worklog{}, tracker.Worklog{}, nil, false, err
		}
		last = false
	}
	cur, err = nativeWorklog(read, read.IssueID, loc)
	if err == nil {
		err = checkWorklogIssue(cur, from)
	}
	if err != nil {
		return jira.Worklog{}, tracker.Worklog{}, nil, false, err
	}
	return read, cur, loc, last, nil
}

// authorZone is the time zone of account, the author of worklog id.
func (t *jiraTime) authorZone(ctx context.Context, account string, id tracker.WorklogID) (*time.Location, error) {
	author, err := t.jira.User(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: the author of worklog %s: %w", id, err)
	}
	return zone(author)
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
