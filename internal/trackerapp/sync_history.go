package trackerapp

import (
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

const (
	// historyBatch is the most issues one history read names, and the
	// most one store write of histories takes (store
	// ReplaceMirrorHistories); historyBatches is the most reads one
	// history unit makes.
	historyBatch   = 50
	historyBatches = 4
)

// history is the history unit (SyncBoard, SyncLinked). Due issues are the
// connection's, not the scope's: the scope is where the outcome is
// recorded. Each read names the boards the issues are on now, as the due
// listing gives them, so their estimate changes are read, and the history
// written names the revision of that board set (store
// MirrorHistory.BoardsRev): an issue that gains a board is due again. An
// issue the tracker returns no history for
// (deleted or not visible) is stored with none known before the read, so
// it is not due again until its card moves.
func (r *run) history() (UnitResult, error) {
	var out UnitResult
	after, reads := "", 0
	for {
		due, more, err := r.s.store.ListMirrorHistoryDue(r.root, after)
		if err != nil {
			return out, fmt.Errorf("trackerapp: list the issues whose history is due: %w", err)
		}
		for batch := range slices.Chunk(due, historyBatch) {
			if reads == historyBatches {
				out.More = true
				return out, nil
			}
			reads++
			err := r.againIfSwept(func() error {
				ch, err := r.historyOf(batch)
				out.Changes.add(ch)
				return err
			})
			if err != nil {
				return out, err
			}
		}
		if !more || len(due) == 0 {
			return out, nil
		}
		if reads == historyBatches {
			// The next page is known to hold due issues: no read to learn it.
			out.More = true
			return out, nil
		}
		after = due[len(due)-1].IssueID
	}
}

// historyOf reads and stores the history of the issues of due.
func (r *run) historyOf(due []store.MirrorHistoryDue) (Changes, error) {
	issues := make([]tracker.IssueID, len(due))
	var boards []tracker.BoardID
	for i, d := range due {
		issues[i] = tracker.IssueID(d.IssueID)
		for _, b := range d.Boards {
			boards = append(boards, tracker.BoardID(b))
		}
	}
	slices.Sort(boards)
	boards = slices.Compact(boards)
	read := r.readAt()
	r.calls++
	h, err := r.p.History(r.ctx, tracker.HistoryQuery{Issues: issues, Boards: boards})
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: read the history of %d issues: %w", len(issues), call(err))
	}
	var out Changes
	if len(h.Users)+len(h.Statuses) > 0 {
		catalog, err := mirrorCatalog(h.Catalog)
		if err != nil {
			return out, err
		}
		stored, err := r.s.store.UpsertMirrorCards(r.root, r.state.Sweep, nil, catalog, read)
		if err != nil {
			return out, fmt.Errorf("trackerapp: store the users of a history read: %w", err)
		}
		out.Catalog = stored.Catalog
	}
	byIssue := make(map[tracker.IssueID]int, len(h.Issues))
	for i, ih := range h.Issues {
		byIssue[ih.Issue] = i
	}
	histories := make([]store.MirrorHistory, len(issues))
	for i, id := range issues {
		if j, ok := byIssue[id]; ok {
			histories[i] = mirrorHistory(h.Issues[j])
		} else {
			histories[i] = emptyHistory(id, read)
		}
		// The revision of the issue's boards the read was asked with: a
		// history of an older revision does not cover a board added since.
		histories[i].BoardsRev = due[i].BoardsRev
	}
	out.History, err = r.storeHistories(histories)
	return out, err
}

// storeHistories stores hs, the whole history of each issue of one read,
// in one store call, and returns the issues whose stored history changed,
// sorted. A history the store finds superseded (a later read stored one)
// or of an issue it no longer keeps changes nothing and is no failure.
func (r *run) storeHistories(hs []store.MirrorHistory) ([]tracker.IssueID, error) {
	results, err := r.s.store.ReplaceMirrorHistories(r.root, r.state.Sweep, hs)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: store the history of %d issues: %w", len(hs), err)
	}
	var changed []tracker.IssueID
	for _, res := range results {
		if res.Changed {
			changed = append(changed, tracker.IssueID(res.IssueID))
		}
	}
	slices.Sort(changed)
	return changed, nil
}
