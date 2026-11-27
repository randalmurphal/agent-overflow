package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

// Evidence reads the issues q names in one bulk fetch of their update
// times and the lists they ask for, and their worklogs from the
// connection's time source, 100 a request. Jira embeds the first
// comments and worklogs of each issue with their total; an issue that
// holds more, or whose answer was too large to embed them, is read again
// whole, its comments through its comment list and its worklogs through
// its worklog list from the issue's bound. While Tempo is the time, the
// bulk fetch asks for no worklog, and the worklogs are Tempo's, read
// through its worklog search by issue (tempoWorklogs).
//
// Only acts of a person's account count: an app's, a customer's and one
// of no account are left out. An act whose author Jira names with no
// account type cannot be told a person's, and fails its issue's source
// (evidence.add). A source fails for its issue alone when Jira did not
// return the issue or said it could not read it, when Jira withheld the
// issue's list or an author's account, or when a Tempo worklog's time
// cannot be placed; any other failure fails the read, with the sources
// read before it.
func (p *Provider) Evidence(ctx context.Context, q tracker.EvidenceQuery) (tracker.Evidence, error) {
	if err := q.Check(); err != nil {
		return tracker.Evidence{}, err
	}
	e := evidence{asked: make(map[string]tracker.EvidenceIssue, len(q.Issues)), failed: map[failedSource]bool{}, named: map[string]bool{}}
	ids := make([]string, len(q.Issues))
	comments, worklogs := false, false
	for i, in := range q.Issues {
		if err := checkIssue(in.Issue); err != nil {
			return tracker.Evidence{}, err
		}
		ids[i], e.asked[string(in.Issue)] = string(in.Issue), in
		comments, worklogs = comments || in.Comments, worklogs || in.Worklogs
	}
	tempoTime, fromTempo := p.time.(*tempoTime)
	read, failed, err := p.jira.BulkActs(ctx, ids, comments, worklogs && !fromTempo)
	if err != nil {
		return tracker.Evidence{}, fmt.Errorf("jiraprovider: evidence: %w", err)
	}
	for _, f := range failed {
		e.failAll(f.ID, fmt.Errorf("Jira could not read issue %s: %s", f.ID, f.Message))
	}
	returned := make(map[string]bool, len(read))
	for _, is := range read {
		returned[is.ID] = true
	}
	for _, id := range ids {
		if !returned[id] {
			e.failAll(id, fmt.Errorf("Jira did not return issue %s: it was deleted, or this account cannot see it", id))
		}
	}
	for _, is := range read {
		if !e.asked[is.ID].Comments {
			continue
		}
		list := is.Comments.Acts
		if !is.Comments.Complete {
			if list, err = p.jira.CommentActs(ctx, is.ID); err != nil {
				if !aloneFailure(err) {
					return e.out, fmt.Errorf("jiraprovider: evidence: %w", err)
				}
				e.fail(is.ID, tracker.EvidenceComments, fmt.Errorf("the comments of issue %s: %w", is.ID, err))
				continue
			}
		}
		e.add(is.ID, tracker.EvidenceComments, is.Updated, list, time.Time{})
	}
	switch {
	case !worklogs:
	case fromTempo:
		if err := p.tempoWorklogs(ctx, tempoTime.tempo, &e, read); err != nil {
			return e.out, fmt.Errorf("jiraprovider: evidence: %w", err)
		}
	default:
		for _, is := range read {
			in := e.asked[is.ID]
			if !in.Worklogs {
				continue
			}
			logs := is.Worklogs.Acts
			if !is.Worklogs.Complete {
				// Jira returns the worklogs started after startedAfter: a
				// millisecond before the bound reads those started at it.
				query := jira.WorklogQuery{Issue: is.ID}
				if !in.WorklogsFrom.IsZero() {
					query.StartedAfter = in.WorklogsFrom.Add(-time.Millisecond)
				}
				if logs, err = p.jira.WorklogActs(ctx, query); err != nil {
					if !aloneFailure(err) {
						return e.out, fmt.Errorf("jiraprovider: evidence: %w", err)
					}
					e.fail(is.ID, tracker.EvidenceWorklogs, fmt.Errorf("the worklogs of issue %s: %w", is.ID, err))
					continue
				}
			}
			e.add(is.ID, tracker.EvidenceWorklogs, is.Updated, logs, in.WorklogsFrom)
		}
	}
	return e.out, nil
}

// aloneFailure reports an error of a read of one issue's list, or of one
// account, that fails that issue's source alone: Jira withheld it (a 404
// or a 403), or one item of it is over the transport's bound. Any other
// is every issue's.
func aloneFailure(err error) bool {
	_, large := errors.AsType[*trackerapi.BodyTooLargeError](err)
	return large || jira.Withheld(err)
}

// failedSource is a source of an issue that failed.
type failedSource struct {
	issue  string
	source tracker.EvidenceSource
}

// evidence is an Evidence read as it is made: the query's issues by ID,
// the sources that failed, and the authors named.
type evidence struct {
	asked  map[string]tracker.EvidenceIssue
	out    tracker.Evidence
	failed map[failedSource]bool
	named  map[string]bool
}

// fail records source of issue failed for err, once.
func (e *evidence) fail(issue string, source tracker.EvidenceSource, err error) {
	k := failedSource{issue, source}
	if e.failed[k] {
		return
	}
	e.failed[k] = true
	e.out.Failed = append(e.out.Failed, tracker.SourceFailure{Issue: tracker.IssueID(issue), Source: source, Err: err})
}

// failAll records every source asked of issue failed for err.
func (e *evidence) failAll(issue string, err error) {
	in := e.asked[issue]
	if in.Comments {
		e.fail(issue, tracker.EvidenceComments, err)
	}
	if in.Worklogs {
		e.fail(issue, tracker.EvidenceWorklogs, err)
	}
}

// add records acts, those from bound, as source of issue read whole for
// its update updated: those of a person's account (an atlassian one),
// each author named once. An act whose author Jira named with no account
// type fails the source instead, since whether a person made it is not
// known.
func (e *evidence) add(issue string, source tracker.EvidenceSource, updated time.Time, acts []jira.Act, bound time.Time) {
	kept := make([]tracker.Act, 0, len(acts))
	for _, a := range acts {
		switch {
		case a.At.Before(bound) || a.Author == nil:
			continue
		case a.Author.AccountType == "":
			e.fail(issue, source, fmt.Errorf("Jira named no account type for %s, the author of %s %s of issue %s, so whether a person made it is not known",
				a.Author.AccountID, sourceNoun(source), a.ID, issue))
			return
		case a.Author.AccountType != "atlassian":
			continue
		}
		if !e.named[a.Author.AccountID] {
			e.named[a.Author.AccountID] = true
			e.out.Users = append(e.out.Users, userOf(*a.Author))
		}
		kept = append(kept, tracker.Act{ID: a.ID, Author: tracker.UserID(a.Author.AccountID), At: a.At, Spent: a.Spent})
	}
	e.out.Read = append(e.out.Read, tracker.SourceEvidence{Issue: tracker.IssueID(issue), Source: source, Updated: updated, Acts: kept})
}

// sourceNoun names one act of source.
func sourceNoun(source tracker.EvidenceSource) string {
	if source == tracker.EvidenceComments {
		return "comment"
	}
	return "worklog"
}

// tempoWorklogs adds the Tempo worklogs of the issues read that ask for
// them: one search for the issues of each bound, from the day before the
// bound, since Tempo dates a worklog on its author's clock. A worklog
// before its bound is dropped before its author is looked up. Each
// author is looked up once a call, through the client's user cache: one
// Jira withholds fails the worklogs of each issue it logged on, naming
// the permission a lookup needs. A worklog Tempo gave no UTC start is
// placed on its author's clock, and fails its issue's worklogs when Jira
// shows no time zone for the author.
func (p *Provider) tempoWorklogs(ctx context.Context, tc *tempo.Client, e *evidence, read []jira.IssueActs) error {
	byBound := map[time.Time][]int64{}
	var bounds []time.Time
	for _, is := range read {
		in := e.asked[is.ID]
		if !in.Worklogs {
			continue
		}
		n, err := strconv.ParseInt(is.ID, 10, 64)
		if err != nil {
			return fmt.Errorf("issue %q is not a number: %w", is.ID, err)
		}
		if _, ok := byBound[in.WorklogsFrom]; !ok {
			bounds = append(bounds, in.WorklogsFrom)
		}
		byBound[in.WorklogsFrom] = append(byBound[in.WorklogsFrom], n)
	}
	authors := map[string]*jira.User{}
	withheld := map[string]error{}
	logs := map[string][]jira.Act{}
	for _, bound := range bounds {
		q := tempo.IssueWorklogQuery{Issues: byBound[bound]}
		if !bound.IsZero() {
			q.From = tempo.DateOf(bound.UTC().AddDate(0, 0, -1))
		}
		found, err := tc.IssueWorklogs(ctx, q)
		if err != nil {
			return err
		}
		for _, w := range found {
			issue := decimal(w.Issue)
			if e.failed[failedSource{issue, tracker.EvidenceWorklogs}] || before(w, bound) {
				continue
			}
			author, err := p.tempoAuthor(ctx, w.Author, authors, withheld)
			if err != nil {
				if !jira.Withheld(err) {
					return fmt.Errorf("the author of Tempo worklog %d: %w", w.ID, err)
				}
				e.fail(issue, tracker.EvidenceWorklogs, fmt.Errorf("Jira did not show who logged Tempo worklog %d on issue %s: looking up a person needs Jira's Browse users and groups permission: %w",
					w.ID, issue, err))
				continue
			}
			at := w.StartUTC
			if at.IsZero() {
				loc, err := time.LoadLocation(author.TimeZone)
				if author.TimeZone == "" || err != nil {
					e.fail(issue, tracker.EvidenceWorklogs, fmt.Errorf("Tempo gave worklog %d on issue %s no UTC start, and Jira shows no time zone for %s to place it by",
						w.ID, issue, author.AccountID))
					continue
				}
				d := w.Start.Date
				at = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, int(w.Start.Time.SinceMidnight()/time.Second), 0, loc)
			}
			logs[issue] = append(logs[issue], jira.Act{ID: decimal(w.ID), Author: author, At: at, Spent: w.Spent})
		}
	}
	for _, is := range read {
		in := e.asked[is.ID]
		if in.Worklogs && !e.failed[failedSource{is.ID, tracker.EvidenceWorklogs}] {
			e.add(is.ID, tracker.EvidenceWorklogs, is.Updated, logs[is.ID], in.WorklogsFrom)
		}
	}
	return nil
}

// before reports a Tempo worklog that started before bound on any clock:
// by its UTC start, or with none, by its date and time 14 hours behind
// UTC, the farthest a clock is.
func before(w tempo.IssueWorklog, bound time.Time) bool {
	if !w.StartUTC.IsZero() {
		return w.StartUTC.Before(bound)
	}
	d := w.Start.Date
	latest := time.Date(d.Year(), d.Month(), d.Day(), 14, 0, int(w.Start.Time.SinceMidnight()/time.Second), 0, time.UTC)
	return latest.Before(bound)
}

// tempoAuthor is the Jira account of a Tempo worklog's author, looked up
// once a call: authors holds those found, withheld the lookups Jira
// withheld.
func (p *Provider) tempoAuthor(ctx context.Context, account string, authors map[string]*jira.User, withheld map[string]error) (*jira.User, error) {
	if u, ok := authors[account]; ok {
		return u, nil
	}
	if err, ok := withheld[account]; ok {
		return nil, err
	}
	u, err := p.jira.User(ctx, account)
	switch {
	case err == nil:
		authors[account] = &u
		return &u, nil
	case jira.Withheld(err):
		withheld[account] = err
	}
	return nil, err
}
