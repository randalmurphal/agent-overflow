package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/threadtools"
)

// A sidebar row's worker summary: the threads the open rows of its Agents
// listing went to, and those of them waiting on the person.

// ThreadWorkerSummary is what a thread's sidebar row says of its Agents
// listing: Open counts the threads its open rows went to, nested rows
// included, each once however many requests went to it, and NeedYou those
// of them blocked on a prompt no agent was given.
type ThreadWorkerSummary struct {
	ThreadID string `json:"threadId"`
	Open     int    `json:"open"`
	NeedYou  int    `json:"needYou"`
}

// threadWorkerSummaryPage bounds one page of ListThreadWorkerSummaries:
// each summary reads its thread's listing.
const threadWorkerSummaryPage = 64

// ThreadWorkerSummaryPage is one page of the threads whose Agents listing
// may show an open row, in thread id order. Summaries holds those that
// show one. Next continues after the last thread the page read; it is
// empty on the last page, and only the pages read up to it together say
// which threads have no open row.
type ThreadWorkerSummaryPage struct {
	Summaries []ThreadWorkerSummary `json:"summaries"`
	Next      string                `json:"next,omitempty"`
}

// ListThreadWorkerSummaries summarizes one page of the threads whose
// Agents listing shows an open row, after the thread id `after` (empty for
// the first page), by thread id. A thread with none is left out.
//
//ao:scope threads:read
//ao:route all
func (a *App) ListThreadWorkerSummaries(ctx context.Context, after string) (ThreadWorkerSummaryPage, error) {
	view := newThreadAgentView(a)
	view.counts = true
	ids, more, err := a.store.ThreadsWithOpenAgentRows(view.waiting, view.live, after, threadWorkerSummaryPage)
	if err != nil {
		return ThreadWorkerSummaryPage{}, fmt.Errorf("list worker summaries: %w", err)
	}
	page := ThreadWorkerSummaryPage{Summaries: make([]ThreadWorkerSummary, 0, len(ids))}
	for _, id := range ids {
		summary, err := a.threadWorkerSummary(ctx, view, id)
		if err != nil {
			return ThreadWorkerSummaryPage{}, fmt.Errorf("list worker summaries: %w", err)
		}
		if summary.Open > 0 {
			page.Summaries = append(page.Summaries, summary)
		}
	}
	if more {
		page.Next = ids[len(ids)-1]
	}
	return page, nil
}

// GetThreadWorkerSummary summarizes the thread's Agents listing as it stands
// now: zero counts when it shows no open row, or the thread is gone.
//
//ao:scope threads:read
func (a *App) GetThreadWorkerSummary(ctx context.Context, threadID string) (ThreadWorkerSummary, error) {
	if _, err := a.store.GetThread(threadID); errors.Is(err, sql.ErrNoRows) {
		// Deleted since the frame that asked: it has no open worker.
		return ThreadWorkerSummary{ThreadID: threadID}, nil
	} else if err != nil {
		return ThreadWorkerSummary{}, fmt.Errorf("read the thread: %w", err)
	}
	view := newThreadAgentView(a)
	view.counts = true
	return a.threadWorkerSummary(ctx, view, threadID)
}

// threadWorkerSummary counts the threads the open rows of the thread's
// listing went to, read as a pane reads it, through view, so the counts
// are the rows the pane shows.
func (a *App) threadWorkerSummary(ctx context.Context, view threadAgentView, threadID string) (ThreadWorkerSummary, error) {
	listing, _, err := a.threadAgentTreeDepth(ctx, view, threadID, threadAgentRowDepth, threadAgentPeersCached)
	if err != nil {
		return ThreadWorkerSummary{}, err
	}
	summary := ThreadWorkerSummary{ThreadID: threadID}
	open, needYou := map[string]bool{}, map[string]bool{}
	for _, row := range listing.Rows {
		if !row.open() {
			continue
		}
		open[row.worker()] = true
		if row.needsYou() {
			needYou[row.worker()] = true
		}
	}
	summary.Open, summary.NeedYou = len(open), len(needYou)
	return summary, nil
}

// worker names the thread the row went to, with its computer, so requests
// to one thread count it once; a row whose thread is not made yet is its
// own.
func (row ThreadAgentRow) worker() string {
	if row.ThreadID == "" {
		return "request:" + row.Token
	}
	return row.ComputerID + "/" + row.ThreadID
}

// open reports whether the row is open: its request has not settled, or
// the thread its spawn made is not at rest.
func (row ThreadAgentRow) open() bool {
	if row.Worker != "" {
		return true
	}
	switch row.State {
	case threadtools.RequestUnconfirmed, threadtools.RequestAccepted, threadtools.RequestRunning, threadtools.RequestBlocked:
		return true
	default:
		return false
	}
}

// needsYou reports whether the row waits on the user, as its blocked
// fields say: blocked on a prompt no agent was given.
func (row ThreadAgentRow) needsYou() bool {
	return row.BlockedOn != "" && row.BlockedRoutedTo == ""
}
