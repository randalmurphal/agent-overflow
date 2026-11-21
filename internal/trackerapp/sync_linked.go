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
		ch, _, err := r.cards(issueIDsInOrder(ids))
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
