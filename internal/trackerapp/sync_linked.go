package trackerapp

import "fmt"

// linkedCards is the cards unit (SyncLinked): the cards of the
// connection's linked issues that no board the mirror shows has a row of
// (store ListMirrorOffBoardLinkedIssues), since no board read keeps those
// fresh; every page of the store's listing, readBatch issues a read.
func (r *run) linkedCards() (UnitResult, error) {
	var out UnitResult
	for after := ""; ; {
		ids, more, err := r.s.store.ListMirrorOffBoardLinkedIssues(r.root, after)
		if err != nil {
			return out, fmt.Errorf("trackerapp: list the linked issues of %s: %w", r.row.ID, err)
		}
		ch, err := r.cards(issueIDsInOrder(ids))
		out.Changes.add(ch)
		if err != nil {
			return out, err
		}
		if !more || len(ids) == 0 {
			return out, nil
		}
		after = ids[len(ids)-1]
	}
}
