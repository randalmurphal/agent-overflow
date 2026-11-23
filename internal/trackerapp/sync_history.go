package trackerapp

import (
	"fmt"
	"slices"
	"time"

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

// history is the history unit (syncBoard, SyncLinked). Due issues are the
// connection's, not the scope's: the scope is where the outcome is
// recorded. Each read names the boards the issues are on now, as the due
// listing gives them, so their estimate changes are read, and the history
// written names the revision of that board set (store
// MirrorHistory.BoardsRev): an issue that gains a board is due again. An
// issue the tracker returns no history for
// (deleted or not visible) is stored with none known before the read, so
// it is not due again until its card moves. Which issues are due is the
// store's rule (store ListMirrorHistoryDue), on the steady clock: an issue
// that changed is read at once and, when that read is not final for its
// update (historyFinal), once more a settle margin after its answer. One
// listing gives the unit its issues, a page the size of historyBatches
// reads; each read either clears its issues' due state or is superseded
// by a write that did, so the next tick's listing reaches the issues
// behind them. The issues of first that are due are listed before the
// others (the shown tickets' issues whose status times no final read
// decided, roundHistory).
func (r *run) history(first ...string) (UnitResult, error) {
	var log changeLog
	more, err := r.historyReads(&log, first)
	return UnitResult{Changes: log.changes(), More: more}, err
}

// historyReads makes the reads of the history unit, first's due issues
// first, adding what each stored to log, and reports whether due issues
// are left.
func (r *run) historyReads(log *changeLog, first []string) (more bool, _ error) {
	due, more, err := r.s.store.ListMirrorHistoryDue(r.root, r.steadyAt(), first...)
	if err != nil {
		return false, fmt.Errorf("trackerapp: list the issues whose history is due: %w", err)
	}
	reads := 0
	for batch := range slices.Chunk(due, historyBatch) {
		if reads == historyBatches {
			return true, nil
		}
		reads++
		err := r.againIfSwept(func() error {
			ch, err := r.historyOf(batch)
			log.add(ch)
			return err
		})
		if err != nil {
			return false, err
		}
	}
	return more, nil
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
	read, err := r.beginRead()
	if err != nil {
		return Changes{}, err
	}
	began := r.steadyAt()
	r.calls++
	h, err := r.p.History(r.ctx, tracker.HistoryQuery{Issues: issues, Boards: boards})
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: read the history of %d issues: %w", len(issues), call(err))
	}
	// When the answers arrived, by the steady clock: a wait for a final
	// read counts from it (store MirrorHistory.AnsweredAt).
	answered, settle := r.steadyAt(), r.p.Capabilities().HistorySettle.Milliseconds()
	catalog, err := mirrorCatalog(h.Catalog)
	if err != nil {
		return Changes{}, err
	}
	byIssue := make(map[tracker.IssueID]int, len(h.Issues))
	for i, ih := range h.Issues {
		byIssue[ih.Issue] = i
	}
	histories := make([]store.MirrorHistory, len(issues))
	for i, id := range issues {
		d := due[i]
		final := true
		if j, ok := byIssue[id]; ok {
			histories[i] = mirrorHistory(h.Issues[j])
			final = r.historyFinal(d, h.Issues[j].CompleteTo, began, settle)
		} else {
			histories[i] = emptyHistory(id, d.UpdatedAt)
		}
		histories[i].For, histories[i].AnsweredAt = d.UpdatedAt, answered
		if !final {
			histories[i].Wait = settle
		}
		// The revision of the issue's boards the read was asked with: a
		// history of an older revision does not cover a board added since.
		// The receipt the read was listed with: an answer that arrives after
		// a later one was stored is superseded.
		histories[i].BoardsRev, histories[i].Receipt = d.BoardsRev, d.Receipt
	}
	var out Changes
	out.History, out.Catalog, err = r.storeHistories(histories, catalog, read)
	return out, err
}

// historyFinal reports whether a read of d's history that began at began
// (steady clock) with completeTo (the tracker's clock, zero for none) is
// final for d's update time, the version it was read for. Each comparison
// stays in one clock: completeTo with the update time, both the
// tracker's; began with the answer to the read before for the same
// version, both this process's steady clock. A read for the version a
// final read was stored for (one asked again for a board the issue
// gained) reads what had settled already. An answer stored by an earlier
// process (before steadyFrom) proves no elapsed time, since the wall
// clock may have stepped across the restart: the read after it is final
// only by completeTo, so a restart during a wait costs at most one more
// read. A provider with no settle margin waits no time either way.
// Trusting completeTo assumes the tracker's Date and its update times
// come from one clock, continuous to within the settle margin: a forward
// step of the tracker's clock by more than the margin between an update
// and the read makes a read final that has not settled.
func (r *run) historyFinal(d store.MirrorHistoryDue, completeTo time.Time, began, settle int64) bool {
	if millis(completeTo) >= d.UpdatedAt {
		return true
	}
	if d.For != d.UpdatedAt {
		return false
	}
	return d.Wait == 0 || (d.AnsweredAt >= r.s.steadyFrom && began-d.AnsweredAt >= settle)
}

// storeHistories stores hs, the whole history of each issue of one read,
// with the users and statuses the read named (catalog, the read begun at
// read), in one store call, and returns the issues whose stored history
// changed, sorted, and whether the catalog changed. A history the store
// finds superseded (a later read stored one) or of an issue it no longer
// keeps changes nothing and is no failure. A store call that failed part
// way changed what it reports, which is returned with its error.
func (r *run) storeHistories(hs []store.MirrorHistory, catalog store.MirrorCatalog, read store.MirrorRead) ([]tracker.IssueID, bool, error) {
	results, catalogChanged, err := r.s.store.ReplaceMirrorHistories(r.root, r.state.Sweep, hs, catalog, read)
	var changed []tracker.IssueID
	for _, res := range results {
		if res.Changed {
			changed = append(changed, tracker.IssueID(res.IssueID))
		}
	}
	slices.Sort(changed)
	if err != nil {
		return changed, catalogChanged, fmt.Errorf("trackerapp: store the history of %d issues: %w", len(hs), err)
	}
	return changed, catalogChanged, nil
}
