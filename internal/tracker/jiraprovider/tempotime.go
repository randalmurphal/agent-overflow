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
	if err := t.name(ctx, out); err != nil {
		return nil, fmt.Errorf("jiraprovider: the issues of %s's Tempo worklogs: %w: %w: %w", q.Author, tracker.ErrWorklogNames, tracker.ErrWorklogJira, err)
	}
	return out, nil
}

// name sets each worklog's issue key and title from Jira, which Tempo
// does not give: a bulk read of the distinct issues with one field, in
// the client's batches (500 issues a request). An issue Jira does not
// return, or lists as failed (one the user may no longer see), keeps no
// name; a request that fails fails the read.
func (t *tempoTime) name(ctx context.Context, logs []tracker.Worklog) error {
	if len(logs) == 0 {
		return nil
	}
	ids := make([]string, len(logs))
	for i, w := range logs {
		ids[i] = string(w.Issue)
	}
	res, err := t.jira.BulkFetchByID(ctx, ids, "summary")
	if err != nil {
		return err
	}
	names := make(map[tracker.IssueID]*jira.Issue, len(res.Issues))
	for _, is := range res.Issues {
		names[tracker.IssueID(is.ID)] = is
	}
	for i := range logs {
		if is := names[logs[i].Issue]; is != nil {
			logs[i].IssueKey, logs[i].IssueTitle = is.Key, is.Summary
		}
	}
	return nil
}

// AddWorklogs logs each of ins for the credential's own user: one read
// of who the user is from Jira, then one Tempo batch (tempo.Client.Apply),
// whose checks run once for every worklog and which sends one request
// per worklog, in order.
func (t *tempoTime) AddWorklogs(ctx context.Context, ins []tracker.WorklogInput) tracker.Result {
	if err := tracker.CheckWorklogInputs(ins); err != nil {
		return tracker.InvalidWorklogs(ins, err)
	}
	me, err := t.jira.Me(ctx)
	if err != nil {
		return worklogResults(ins, "", beforeSend(fmt.Errorf("jiraprovider: who logs the time: %w: %w", tracker.ErrWorklogJira, err)))
	}
	author := tracker.UserID(me.AccountID)
	out := tracker.Result{Parts: make([]tracker.Part, len(ins))}
	var ops []tempo.Op
	var sent []int // the place in ins of each op
	for i, in := range ins {
		issue, err := numericID("issue", string(in.Issue))
		if err != nil {
			out.Parts[i] = worklogPart(in, author, tracker.Refused(refusal(err)), "")
			continue
		}
		ops = append(ops, tempo.CreateOp(tempo.Draft{
			Issue: tempo.Issue{ID: issue}, Author: me.AccountID, Start: tempo.Start{Date: tempoDate(in.Day)},
			Spent: in.Spent, Description: in.Description,
		}))
		sent = append(sent, i)
	}
	if len(ops) == 0 {
		return out
	}
	for j, o := range t.tempo.Apply(ctx, ops) {
		var made tracker.WorklogID
		if o.Status == tempo.Applied {
			made = tracker.WorklogID(decimal(o.Stored.ID))
		}
		out.Parts[sent[j]] = worklogPart(ins[sent[j]], author, outcome(o.Err), made)
	}
	return out
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
	read, cur, err := t.readWorklog(ctx, id, in.From)
	if err == nil && !sameWorklog(cur, in.From) {
		err = stale(string(in.From.Issue), "worklog "+string(in.From.ID))
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

// DeleteWorklog deletes a Tempo worklog that still holds what From says.
// The worklog is read first, so the result names its author's worklogs of
// its day as Tempo holds them.
func (t *tempoTime) DeleteWorklog(ctx context.Context, in tracker.WorklogDelete) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	id, err := numericID("worklog", string(in.From.ID))
	if err != nil {
		return tracker.Invalid(tracker.StepWorklog, err, in.From.Issue)
	}
	_, cur, err := t.readWorklog(ctx, id, in.From)
	if err == nil && !sameWorklog(cur, in.From) {
		err = stale(string(in.From.Issue), "worklog "+string(in.From.ID))
	}
	if err != nil {
		return worklogResult(in.From.Issue, in.From.Author, beforeSend(err), in.From.Day)
	}
	err = t.tempo.Delete(ctx, id)
	return worklogResult(in.From.Issue, in.From.Author, outcome(err), in.From.Day)
}

// readWorklog reads Tempo worklog id, as Tempo holds it and as a neutral
// worklog, refusing one whose issue is not from's: a Tempo worklog ID is
// the site's, and nothing in its write names the issue.
func (t *tempoTime) readWorklog(ctx context.Context, id int64, from tracker.Worklog) (tempo.Worklog, tracker.Worklog, error) {
	read, err := t.tempo.Worklog(ctx, id)
	if err != nil {
		return tempo.Worklog{}, tracker.Worklog{}, fmt.Errorf("jiraprovider: read Tempo worklog %d: %w", id, err)
	}
	cur, err := tempoWorklog(read)
	if err == nil {
		err = checkWorklogIssue(cur, from)
	}
	if err != nil {
		return tempo.Worklog{}, tracker.Worklog{}, err
	}
	return read, cur, nil
}

// sameWorklog reports two reads of a worklog that hold the same issue,
// author, day, time and description at the same tracker revision. The
// revision is compared at the millisecond, the precision a copy keeps
// through the mirror and the wire (Jira answers milliseconds, Tempo
// seconds), so a change the copy does not show (a start moved within its
// day, Tempo's billable time) still makes it stale. A worklog read with
// no revision has none in either copy.
func sameWorklog(a, b tracker.Worklog) bool {
	return a.ID == b.ID && a.Issue == b.Issue && a.Author == b.Author && a.Day == b.Day &&
		a.Spent == b.Spent && a.Description == b.Description && a.Updated.UnixMilli() == b.Updated.UnixMilli()
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

// worklogPart is the part of in, one worklog of a batch added for author
// with outcome o; made names the worklog an applied add made.
func worklogPart(in tracker.WorklogInput, author tracker.UserID, o tracker.Outcome, made tracker.WorklogID) tracker.Part {
	p := worklogResult(in.Issue, author, o, in.Day).Parts[0]
	if o.State() == tracker.StateApplied {
		p.Worklog = made
	}
	return p
}

// worklogResults is a batch whose every input has outcome o.
func worklogResults(ins []tracker.WorklogInput, author tracker.UserID, o tracker.Outcome) tracker.Result {
	out := tracker.Result{Parts: make([]tracker.Part, len(ins))}
	for i, in := range ins {
		out.Parts[i] = worklogPart(in, author, o, "")
	}
	return out
}

// worklogResult is a worklog write's one part, which may change the
// issue's detail and author's worklogs of days. It names worklogs only of
// an author and a day that are known: a write that failed before it
// learned them, from its input or a read, names none for them.
func worklogResult(issue tracker.IssueID, author tracker.UserID, o tracker.Outcome, days ...tracker.Date) tracker.Result {
	changed := []tracker.Resource{tracker.DetailOf(issue)}
	for _, d := range days {
		if author != "" && !d.IsZero() {
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
		Day: day, Spent: w.Spent, Description: w.Description, Created: w.Created, Updated: w.Updated,
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
