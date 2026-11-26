package trackerapp

import (
	"fmt"
	"slices"
)

// linkedCards is the cards unit (SyncLinked): the cards of the
// connection's linked issues that no polled board has a row of (store
// ListMirrorOffBoardLinkedIssues), since only a board read that runs keeps
// those fresh; every page of the store's listing, readBatch issues a
// read. Which boards are polled is taken again for each page, so a pump
// that stops or pauses during the unit leaves the pages after it to this
// read and those before it to the next round's.
func (r *run) linkedCards() (UnitResult, error) {
	var log changeLog
	err := r.linkedPages(&log)
	return UnitResult{Changes: log.changes()}, err
}

// linkedPages reads the pages of linkedCards, adding what each stored to
// log.
func (r *run) linkedPages(log *changeLog) error {
	for after := ""; ; {
		ids, more, err := r.s.store.ListMirrorOffBoardLinkedIssues(r.root, after, r.s.polledBoards(r.row.ID, r.gen))
		if err != nil {
			return fmt.Errorf("trackerapp: list the linked issues of %s: %w", r.row.ID, err)
		}
		ch, _, _, err := r.cards(issueIDsInOrder(ids))
		log.add(ch)
		if err != nil || !more || len(ids) == 0 {
			return err
		}
		after = ids[len(ids)-1]
	}
}

// polledBoards are the boards of connection conn whose pumps, of mirror
// generation gen, keep their rows' cards fresh now: a pump that is not
// over, not paused (every handle hidden), and whose subscribers are told
// no failure, so its delta runs every interval. Sorted.
func (s *Service) polledBoards(conn string, gen uint64) []string {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	var out []string
	for sc, p := range s.subs.boards {
		if sc.Connection == conn && p.gen == gen && !p.over && !p.core.DeadLocked() && !p.core.Paused() && p.failure == nil {
			out = append(out, string(sc.Board))
		}
	}
	slices.Sort(out)
	return out
}

// namesBatches is the most reads of unnamed parents one names unit makes.
const namesBatches = 10

// unnamedParents is the names unit: the parents an upgraded mirror's
// stored issues name and no read named (v172; store
// ListMirrorUnnamedParents), read by their own cards, a batch at a time,
// up to namesBatches a tick (More). The card write names each parent from
// its own card whether or not the mirror keeps it, and stores the card
// only if it does. Each batch passes the worklist's cursor after its read
// is stored, so a parent the tracker did not return, or returned with no
// name, is asked for once; the worklist ends once no parent is left after
// it. A tracker failure leaves the cursor, and the next tick asks again.
// The issues it did not return are not recorded missing: the mark is the
// links'.
func (r *run) unnamedParents() (UnitResult, error) {
	var log changeLog
	for range namesBatches {
		batch, err := r.s.store.ListMirrorUnnamedParents(r.root)
		if err != nil {
			return UnitResult{Changes: log.changes()}, fmt.Errorf("trackerapp: read the unnamed parents of %s: %w", r.row.ID, err)
		}
		if !batch.Owed {
			return UnitResult{Changes: log.changes()}, nil
		}
		parents := batch.Parents
		if len(parents) > 0 {
			err = r.againIfSwept(func() error {
				ch, _, _, err := r.cardsOf(issueIDsInOrder(parents))
				log.add(ch)
				return err
			})
			if err == nil {
				err = r.s.store.PassMirrorUnnamedParents(r.root, parents[len(parents)-1])
			}
			if err != nil {
				return UnitResult{Changes: log.changes()}, err
			}
		}
		if !batch.More {
			if err := r.s.store.EndMirrorUnnamedParents(r.root); err != nil {
				return UnitResult{Changes: log.changes()}, err
			}
			return UnitResult{Changes: log.changes()}, nil
		}
	}
	return UnitResult{Changes: log.changes(), More: true}, nil
}
