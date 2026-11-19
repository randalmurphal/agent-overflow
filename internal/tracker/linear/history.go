package linear

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Sizes of the batched history read. One request reads the newest
// historyBatchEntries entries of historyBatchIssues issues: about 3,200
// points at the measured 641 points per 100 entries, under Linear's
// 10,000-point cap per query. An issue with more entries continues with
// IssueHistory pages.
const (
	historyBatchIssues  = 10
	historyBatchEntries = 50
)

// IssueChanges is one issue's whole recorded history: its team and
// creation time, and every entry, newest first. Changes made in a window
// after creation that ends between 2m30s and 3m (measured by
// TestLiveHistoryFoldBisect) are folded into the creation, not entries, and an
// entry can be recorded a few seconds after its change.
type IssueChanges struct {
	IssueID   string
	TeamID    string
	CreatedAt time.Time
	Entries   []HistoryEntry
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
`+historyFields)

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
// any other form is refused before sending. It reads ten issues per
// request, and pages one issue's longer history on its own. A failure on
// any request fails the call.
func (c *Client) IssueHistories(ctx context.Context, ids []string) ([]IssueChanges, error) {
	for i, id := range ids {
		if !IsIssueID(id) {
			return nil, invalid(opIssueHistories.name, "%q is not an issue ID (a lowercase UUID)", id)
		}
		if slices.Contains(ids[:i], id) {
			return nil, invalid(opIssueHistories.name, "%s is named twice", id)
		}
	}
	var out []IssueChanges
	for chunk := range slices.Chunk(ids, historyBatchIssues) {
		filter, err := IssueFilter{IDs: chunk}.input()
		if err != nil {
			return nil, invalid(opIssueHistories.name, "%w", err)
		}
		vars := historiesVars{Filter: filter, First: historyBatchIssues, Entries: historyBatchEntries}
		p, err := fetchPage(ctx, c, opIssueHistories, vars, same[wireIssueChanges])
		if err != nil {
			return nil, err
		}
		if p.HasNextPage {
			return nil, fmt.Errorf("linear %s: %d IDs answered with more than one page", opIssueHistories.name, len(chunk))
		}
		for _, w := range p.Items {
			changes, err := c.issueChanges(ctx, w)
			if err != nil {
				return nil, err
			}
			out = append(out, changes)
		}
	}
	return out, nil
}

// issueChanges types one issue of a batch, reading the rest of its
// history when the batch held only its newest entries.
func (c *Client) issueChanges(ctx context.Context, w wireIssueChanges) (IssueChanges, error) {
	first, err := page(opIssueHistories, w.History, Usage{}, wireHistory.typed)
	if err != nil {
		return IssueChanges{}, err
	}
	out := IssueChanges{IssueID: w.ID, TeamID: w.Team.id(), CreatedAt: w.CreatedAt, Entries: first.Items}
	after := first.EndCursor
	for more := first.HasNextPage; more; {
		p, err := c.IssueHistory(ctx, w.ID, after)
		if err != nil {
			return IssueChanges{}, err
		}
		out.Entries = append(out.Entries, p.Items...)
		if p.HasNextPage && p.EndCursor == after {
			return IssueChanges{}, fmt.Errorf("linear %s: the page cursor did not advance", opIssueHistory.name)
		}
		more, after = p.HasNextPage, p.EndCursor
	}
	return out, nil
}
