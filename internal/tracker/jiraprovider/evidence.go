package jiraprovider

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
)

// Evidence reads the issues q names in one bulk fetch of their comments
// and update times, and their worklogs from the connection's time
// source, 100 a request. Jira embeds the first comments and worklogs of
// each issue with their total; an issue that holds more is read again
// whole, its comments through its comment list and its worklogs through
// its worklog list from the issue's bound. While Tempo is the time, the
// bulk fetch asks for no worklog, and the worklogs are Tempo's, read
// through its worklog search by issue (tempoActs). Only acts of a
// person's account count: an app's, a customer's and one of no account
// are left out.
func (p *Provider) Evidence(ctx context.Context, q tracker.EvidenceQuery) (tracker.Evidence, error) {
	if err := q.Check(); err != nil {
		return tracker.Evidence{}, err
	}
	ids := make([]string, len(q.Issues))
	from := make(map[string]time.Time, len(q.Issues))
	for i, in := range q.Issues {
		if err := checkIssue(in.Issue); err != nil {
			return tracker.Evidence{}, err
		}
		ids[i], from[string(in.Issue)] = string(in.Issue), in.WorklogsFrom
	}
	tempoTime, fromTempo := p.time.(*tempoTime)
	read, err := p.jira.BulkActs(ctx, ids, !fromTempo)
	if err != nil {
		return tracker.Evidence{}, fmt.Errorf("jiraprovider: evidence: %w", err)
	}
	var tempoLogs map[string][]jira.Act
	if fromTempo {
		if tempoLogs, err = p.tempoActs(ctx, tempoTime.tempo, read, from); err != nil {
			return tracker.Evidence{}, fmt.Errorf("jiraprovider: evidence: %w", err)
		}
	}
	var out tracker.Evidence
	named := map[string]bool{}
	acts := func(in []jira.Act, from time.Time) []tracker.Act {
		kept := make([]tracker.Act, 0, len(in))
		for _, a := range in {
			if a.Author == nil || a.Author.AccountType == "app" || a.Author.AccountType == "customer" || a.At.Before(from) {
				continue
			}
			if !named[a.Author.AccountID] {
				named[a.Author.AccountID] = true
				out.Users = append(out.Users, userOf(*a.Author))
			}
			kept = append(kept, tracker.Act{ID: a.ID, Author: tracker.UserID(a.Author.AccountID), At: a.At, Spent: a.Spent})
		}
		return kept
	}
	for _, is := range read {
		comments := is.Comments.Acts
		if !is.Comments.Complete {
			if comments, err = p.jira.CommentActs(ctx, is.ID); err != nil {
				return tracker.Evidence{}, fmt.Errorf("jiraprovider: evidence: %w", err)
			}
		}
		ie := tracker.IssueEvidence{Issue: tracker.IssueID(is.ID), Updated: is.Updated, Comments: acts(comments, time.Time{})}
		bound, logs := from[is.ID], is.Worklogs.Acts
		switch {
		case fromTempo:
			logs = tempoLogs[is.ID]
		case !is.Worklogs.Complete:
			// Jira returns the worklogs started after startedAfter: a
			// millisecond before the bound reads those started at it.
			query := jira.WorklogQuery{Issue: is.ID}
			if !bound.IsZero() {
				query.StartedAfter = bound.Add(-time.Millisecond)
			}
			if logs, err = p.jira.WorklogActs(ctx, query); err != nil {
				return tracker.Evidence{}, fmt.Errorf("jiraprovider: evidence: %w", err)
			}
		}
		ie.Worklogs = acts(logs, bound)
		out.Issues = append(out.Issues, ie)
	}
	return out, nil
}

// tempoActs are the Tempo worklogs of the issues read, by issue ID, each
// with its author as Jira names it: one search for the issues of each
// bound, from the day before the bound, since Tempo dates a worklog on
// its author's clock (Evidence keeps those from the bound). Each author
// is looked up once a call, through the client's user cache; a lookup
// that fails fails the read.
func (p *Provider) tempoActs(ctx context.Context, tc *tempo.Client, read []jira.IssueActs, from map[string]time.Time) (map[string][]jira.Act, error) {
	byBound := map[time.Time][]int64{}
	var bounds []time.Time
	for _, is := range read {
		n, err := strconv.ParseInt(is.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("issue %q is not a number: %w", is.ID, err)
		}
		bound := from[is.ID]
		if _, ok := byBound[bound]; !ok {
			bounds = append(bounds, bound)
		}
		byBound[bound] = append(byBound[bound], n)
	}
	authors := map[string]*jira.User{}
	out := map[string][]jira.Act{}
	for _, bound := range bounds {
		q := tempo.IssueWorklogQuery{Issues: byBound[bound]}
		if !bound.IsZero() {
			q.From = tempo.DateOf(bound.UTC().AddDate(0, 0, -1))
		}
		logs, err := tc.IssueWorklogs(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, w := range logs {
			author, ok := authors[w.Author]
			if !ok {
				u, err := p.jira.User(ctx, w.Author)
				if err != nil {
					return nil, fmt.Errorf("the author of Tempo worklog %d: %w", w.ID, err)
				}
				author = &u
				authors[w.Author] = author
			}
			at := w.StartUTC
			if at.IsZero() {
				at = w.Start.Date.In(time.UTC).Add(w.Start.Time.SinceMidnight())
			}
			issue := decimal(w.Issue.ID)
			out[issue] = append(out[issue], jira.Act{ID: decimal(w.ID), Author: author, At: at, Spent: w.Spent})
		}
	}
	return out, nil
}
