package linearprovider

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// creationFold is how long after an issue's creation Linear folds
// changes into the creation instead of recording them as entries. The
// live checks (TestLiveHistoryFold, TestLiveHistoryFoldBisect) found a
// priority change at +2m30s never recorded and one at +3m recorded, with
// assignee and state changes agreeing, so the fold ends between 2m30s and
// 3m for every field tried; this is the upper bound. History is complete
// only from creation plus this much, so a value in that time is reported
// unknown rather than guessed.
const creationFold = 3 * time.Minute

// settleMargin is how long before a history read began its changes are
// taken as final. The live fold check saw an entry first recorded 5s
// after its change; the margin is 24 times that, which also covers a
// skew between this computer's clock and Linear's, since the caller
// compares CompleteTo with Linear's update times. A larger margin costs
// only a second read of an issue's history.
const settleMargin = 2 * time.Minute

// History is the status, cycle, estimate and assignee changes of the
// issues that exist and are visible, read ten issues per request. Each
// is complete from creation plus creationFold and final through
// settleMargin before the read began. An estimate change is reported on
// the board of the team the issue was in after the change, when that
// board is one q names. IDs that are not issue IDs name no issue and are
// left out.
func (p *Provider) History(ctx context.Context, q tracker.HistoryQuery) (tracker.History, error) {
	ids, _ := issueIDs(q.Issues)
	if len(ids) == 0 {
		return tracker.History{}, nil
	}
	completeTo := p.now().Add(-settleMargin)
	all, err := p.c.IssueHistories(ctx, ids)
	if err != nil {
		return tracker.History{}, fmt.Errorf("linearprovider: history: %w", err)
	}
	var out tracker.History
	for _, ic := range all {
		h := issueHistory(ic, q.Boards)
		h.CompleteTo = completeTo
		out.Issues = append(out.Issues, h)
	}
	return out, nil
}

// issueHistory is one issue's changes, oldest first; CompleteTo is the
// caller's to set.
func issueHistory(ic linear.IssueChanges, boards []tracker.BoardID) tracker.IssueHistory {
	h := tracker.IssueHistory{Issue: tracker.IssueID(ic.IssueID), CompleteFrom: ic.CreatedAt.Add(creationFold)}
	entries := slices.Clone(ic.Entries)
	slices.Reverse(entries)
	team := teamBefore(entries, ic.TeamID)
	for _, e := range entries {
		at := tracker.Change{At: e.CreatedAt, Author: tracker.UserID(e.ActorID)}
		if e.Team != nil {
			team = e.Team.To
		}
		if c := e.State; c != nil {
			h.Status = append(h.Status, tracker.StatusChange{Change: at, From: tracker.StatusID(c.From), To: tracker.StatusID(c.To)})
		}
		if c := e.Cycle; c != nil {
			h.Sprint = append(h.Sprint, tracker.SprintChange{Change: at, From: sprintSet(c.From), To: sprintSet(c.To)})
		}
		if c := e.Assignee; c != nil {
			h.Assignee = append(h.Assignee, tracker.AssigneeChange{Change: at, From: tracker.UserID(c.From), To: tracker.UserID(c.To)})
		}
		if c := e.Estimate; c != nil && slices.Contains(boards, tracker.BoardID(team)) {
			h.Estimate = append(h.Estimate, tracker.EstimateChange{Change: at, Board: tracker.BoardID(team), From: c.From, To: c.To})
		}
	}
	return h
}

// teamBefore is the team an issue was in before its oldest entry: the
// side its first team change left, or its team now when it never moved.
func teamBefore(oldestFirst []linear.HistoryEntry, now string) string {
	for _, e := range oldestFirst {
		if e.Team != nil {
			return e.Team.From
		}
	}
	return now
}

func sprintSet(id string) []tracker.SprintID {
	if id == "" {
		return nil
	}
	return []tracker.SprintID{tracker.SprintID(id)}
}
