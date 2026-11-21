package linear

import (
	"context"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Sizes of the batched history read. A history page nested in an issues
// page is charged by its own first, not by the issues that carry it:
// 1,003 points at 250 for 1, 20 or 42 issues alike (measured). So one
// request reads the newest historyPage entries of historyBatchIssues
// issues for about 1,050 points, under Linear's 10,000-point cap, and
// the batch is as many issues as the answer bound holds at the
// worst-case entry: every change pair set, two 255-character titles of
// three-byte runes and ten labels each way, about 3.2 KB
// (TestHistoryBatchFitsItsBound), so 50 issues of 250 such entries fit
// in historyBodyBytes. An issue with more entries continues with
// IssueHistory pages.
const (
	historyBatchIssues = 50
	historyBodyBytes   = 40 << 20
)

// IssueChanges is one issue's whole recorded history: its team and
// creation time, and every entry, newest first. Changes made in a window
// after creation that ends between 2m30s and 3m (measured by
// TestLiveHistoryFoldBisect) are folded into the creation, not entries,
// and an entry can be recorded a few seconds after its change.
type IssueChanges struct {
	IssueID   string
	TeamID    string
	CreatedAt time.Time
	Entries   []HistoryEntry
	// Served is Linear's clock when it answered the read that held the
	// newest entries (its Date header), zero when that answer carried
	// none: an entry recorded later is not among them.
	Served time.Time
}

var opIssueHistories = newQuery("IssueHistories", "page", `query IssueHistories($filter: IssueFilter, $first: Int!, $entries: Int!) {
  page: issues(filter: $filter, includeArchived: true, first: $first) {
    nodes {
      id team { id } createdAt
      history(first: $entries) { nodes { ...HistoryFields } pageInfo { hasNextPage endCursor } }
    }
    pageInfo { hasNextPage endCursor }
  }
}
`+historyFields).withMaxBody(historyBodyBytes)

type historiesVars struct {
	Filter  *issueFilterInput `json:"filter"`
	First   int               `json:"first"`
	Entries int               `json:"entries"`
}

type wireIssueChanges struct {
	ID        string                  `json:"id"`
	Team      *ref                    `json:"team"`
	CreatedAt time.Time               `json:"createdAt"`
	History   connection[wireHistory] `json:"history"`
}

// IssueHistories returns the whole history of each issue ids names that
// exists and is visible, archived included, in no particular order; an
// issue it omits is missing or invisible. ids are issue IDs (IsIssueID);
// any other form is refused before sending. It reads
// historyBatchIssues issues per request, and pages one issue's longer
// history on its own, as one trackerapi.Walk. A failure on any request
// fails the call.
func (c *Client) IssueHistories(ctx context.Context, ids []string) ([]IssueChanges, error) {
	for i, id := range ids {
		if !IsIssueID(id) {
			return nil, invalid(opIssueHistories.name, "%q is not an issue ID (a lowercase UUID)", id)
		}
		if slices.Contains(ids[:i], id) {
			return nil, invalid(opIssueHistories.name, "%s is named twice", id)
		}
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) ([]IssueChanges, error) {
		var out []IssueChanges
		for chunk := range slices.Chunk(ids, historyBatchIssues) {
			filter, err := IssueFilter{IDs: chunk}.input()
			if err != nil {
				return nil, invalid(opIssueHistories.name, "%w", err)
			}
			vars := historiesVars{Filter: filter, First: len(chunk), Entries: historyPage}
			p, err := fetchPage(ctx, c, opIssueHistories, vars, same[wireIssueChanges])
			if err != nil {
				return nil, err
			}
			if p.HasNextPage && len(p.Items) < len(chunk) {
				return nil, fmt.Errorf("linear %s: %d IDs answered with more than one page", opIssueHistories.name, len(chunk))
			}
			for _, w := range p.Items {
				changes, err := c.issueChanges(ctx, w, p.Served)
				if err != nil {
					return nil, err
				}
				out = append(out, changes)
			}
		}
		return out, nil
	})
}

// issueChanges types one issue of a batch served at served, reading the
// rest of its history when the batch held only its newest entries.
func (c *Client) issueChanges(ctx context.Context, w wireIssueChanges, served time.Time) (IssueChanges, error) {
	first, err := page(opIssueHistories, w.History, Usage{}, wireHistory.typed)
	if err != nil {
		return IssueChanges{}, err
	}
	out := IssueChanges{IssueID: w.ID, TeamID: w.Team.id(), CreatedAt: w.CreatedAt, Entries: first.Items, Served: served}
	after := first.EndCursor
	for more := first.HasNextPage; more; {
		p, err := c.IssueHistory(ctx, w.ID, after)
		if err != nil {
			return IssueChanges{}, err
		}
		out.Entries = append(out.Entries, p.Items...)
		more, after = p.HasNextPage, p.EndCursor
	}
	return out, nil
}
