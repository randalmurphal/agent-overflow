package jiraprovider

import (
	"context"
	"fmt"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
)

// tempoTime is a Jira connection's logged time in Tempo. A worklog's ID
// is Tempo's, its issue the Jira issue's ID, and its description plain
// text as written.
type tempoTime struct {
	jira  *jira.Client
	tempo *tempo.Client
}

// Worklogs are one author's Tempo worklogs whose start date is q.From to
// q.To; Tempo keeps each worklog's date on its author's own clock.
func (t *tempoTime) Worklogs(ctx context.Context, q tracker.WorklogQuery) ([]tracker.Worklog, error) {
	if err := q.Check(); err != nil {
		return nil, err
	}
	tq := tempo.WorklogQuery{Author: string(q.Author), From: tempoDate(q.From), To: tempoDate(q.To)}
	var out []tracker.Worklog
	err := t.tempo.WorklogPages(ctx, tq, func(page []tempo.Worklog) error {
		for _, w := range page {
			n, err := tempoWorklog(w)
			if err != nil {
				return err
			}
			out = append(out, n)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: Tempo worklogs of %s: %w", q.Author, err)
	}
	return out, nil
}

// AddWorklog logs time for the credential's own user.
func (t *tempoTime) AddWorklog(ctx context.Context, in tracker.WorklogInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.Issue)
	}
	issue, err := numericID("issue", string(in.Issue))
	if err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.Issue)
	}
	me, err := t.jira.Myself(ctx)
	if err != nil {
		return worklogResult(in.Issue, "", beforeSend(fmt.Errorf("jiraprovider: who logs the time: %w", err)))
	}
	_, err = t.tempo.Create(ctx, tempo.Draft{
		Issue: tempo.Issue{ID: issue}, Author: me.AccountID, Start: tempo.Start{Date: tempoDate(in.Day)},
		Spent: in.Spent, Description: in.Description,
	})
	return worklogResult(in.Issue, tracker.UserID(me.AccountID), outcome(err), in.Day)
}

// EditWorklog changes a Tempo worklog that still holds what From says;
// Tempo replaces the whole record, so the change starts from the record
// as read now.
func (t *tempoTime) EditWorklog(ctx context.Context, in tracker.WorklogEdit) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	id, err := numericID("worklog", string(in.From.ID))
	if err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	days := editDays(in)
	read, err := t.tempo.Worklog(ctx, id)
	if err == nil {
		err = staleWorklog(read, in.From)
	}
	if err != nil {
		return worklogResult(in.From.Issue, in.From.Author, beforeSend(err), days...)
	}
	ch := tempo.Change{From: read, Spent: in.Spent, Description: in.Description}
	if in.Day != nil {
		ch.Start = &tempo.Start{Date: tempoDate(*in.Day), Time: read.Start.Time}
	}
	_, err = t.tempo.Update(ctx, ch)
	return worklogResult(in.From.Issue, in.From.Author, outcome(err), days...)
}

// DeleteWorklog deletes a Tempo worklog.
func (t *tempoTime) DeleteWorklog(ctx context.Context, in tracker.WorklogDelete) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	id, err := numericID("worklog", string(in.From.ID))
	if err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	err = t.tempo.Delete(ctx, id)
	return worklogResult(in.From.Issue, in.From.Author, outcome(err), in.From.Day)
}

// staleWorklog refuses an edit whose worklog no longer holds what from
// says.
func staleWorklog(read tempo.Worklog, from tracker.Worklog) error {
	cur, err := tempoWorklog(read)
	if err != nil {
		return err
	}
	if !sameWorklog(cur, from) {
		return stale(string(from.Issue), "worklog "+string(from.ID))
	}
	return nil
}

// sameWorklog reports two reads of a worklog that hold the same issue,
// author, day, time and description.
func sameWorklog(a, b tracker.Worklog) bool {
	return a.ID == b.ID && a.Issue == b.Issue && a.Author == b.Author && a.Day == b.Day &&
		a.Spent == b.Spent && a.Description == b.Description
}

// editDays are the days an edit may change the author's worklogs on:
// the worklog's day, and the day it moves to.
func editDays(in tracker.WorklogEdit) []tracker.Date {
	days := []tracker.Date{in.From.Day}
	if in.Day != nil && *in.Day != in.From.Day {
		days = append(days, *in.Day)
	}
	return days
}

// worklogResult is a worklog write's one part, which may change the
// issue's detail and author's worklogs of days; author is empty when it
// is not known, and the write was then not sent.
func worklogResult(issue tracker.IssueID, author tracker.UserID, o tracker.Outcome, days ...tracker.Date) tracker.Result {
	changed := []tracker.Resource{tracker.DetailOf(issue)}
	if author != "" {
		for _, d := range days {
			changed = append(changed, tracker.WorklogsOf(author, d))
		}
	}
	return tracker.Result{Parts: []tracker.Part{{Issue: issue, Step: tracker.StepWorklog, Outcome: o, Changed: changed}}}
}

// tempoWorklog is w as a neutral worklog.
func tempoWorklog(w tempo.Worklog) (tracker.Worklog, error) {
	day, err := tracker.NewDate(w.Start.Date.Year(), w.Start.Date.Month(), w.Start.Date.Day())
	if err != nil {
		return tracker.Worklog{}, fmt.Errorf("jiraprovider: Tempo worklog %d: %w", w.ID, err)
	}
	return tracker.Worklog{
		ID: tracker.WorklogID(decimal(w.ID)), Issue: tracker.IssueID(decimal(w.Issue.ID)), Author: tracker.UserID(w.Author),
		Day: day, Spent: w.Spent, Description: w.Description, Updated: w.Updated,
	}, nil
}

// tempoDate is d as Tempo's date; every neutral day is one Tempo takes.
func tempoDate(d tracker.Date) tempo.Date {
	out, err := tempo.NewDate(d.Year(), d.Month(), d.Day())
	if err != nil {
		panic(fmt.Sprintf("jiraprovider: day %s: %v", d, err))
	}
	return out
}
