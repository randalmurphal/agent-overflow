package jiraprovider

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// readsWorklogs reports that Jira's own worklogs are the connection's
// time, the one source Evidence reads worklogs from: while Tempo is the
// time, the worklogs that count are Tempo's, which Evidence does not read.
func (p *Provider) readsWorklogs() bool {
	_, ok := p.time.(*jiraTime)
	return ok
}

// Evidence reads the issues q names in one bulk fetch of their comments
// and update times, and their worklogs while Jira's own are the time
// (readsWorklogs), 100 a request. Jira embeds the first comments and
// worklogs of each issue with their total; an issue that holds more is
// read again whole, its comments through its comment list and its
// worklogs through its worklog list from the issue's bound. Only acts of
// a person's account count: an app's, a customer's and one of no account
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
	worklogs := p.readsWorklogs()
	read, err := p.jira.BulkActs(ctx, ids, worklogs)
	if err != nil {
		return tracker.Evidence{}, fmt.Errorf("jiraprovider: evidence: %w", err)
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
		if worklogs {
			bound, logs := from[is.ID], is.Worklogs.Acts
			if !is.Worklogs.Complete {
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
		}
		out.Issues = append(out.Issues, ie)
	}
	return out, nil
}
