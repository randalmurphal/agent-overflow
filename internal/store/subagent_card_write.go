package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// subagentCardLiveSQL reports whether the boot pass would recover the
// stamps a card's rows reach (RecoverSubagentCards): the card's nearest
// anchor ?2 is a running agent (liveSubagentAgentSQL), or a running
// agent's transcript root, a carrier it resumes (idx_items_transcript_root).
// The root matches as stored, where the boot pass trims it: under a root
// a carrier names with padding, which no provider writes, a card is not
// live and its writes flush in their own transaction.
var subagentCardLiveSQL = `SELECT EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND id = ?2 AND ` + liveSubagentAgentSQL("") + `)
 OR EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND ` + transcriptRootExpr + ` = ?2 AND ` + liveSubagentAgentSQL("") + `)`

// subagentPromptNamesSQL reports whether a local resume prompt directly
// under the root ?2 names the carrier ?3 (idx_items_subagent_resume_prompt).
var subagentPromptNamesSQL = `SELECT EXISTS (SELECT 1 FROM items p
 WHERE p.thread_id = ?1 AND p.parent_id = ?2 AND ` + aggPromptSQL("p.") + ` AND ` + aggPromptCarrierSQL("p.") + ` = ?3)`

// Writes.

// cardWrite is one write transaction's effect on the subagent cards: the
// rows its card counts, and what the rules do not follow, which the
// transaction recomputes before it commits (finish).
type cardWrite struct {
	s        *Store
	tx       *sql.Tx
	threadID string
	card     *SubagentCard
	// bulk permits counted rows without a card: the writer recomputes
	// every chain it touched instead. bump makes the recompute advance
	// the thread stamp, for a bulk writer none of whose writes did.
	bulk, bump bool

	inserts, changes []subagentRow
	chains, seeds    []string
	unanchor         []string
	// carriers are inserted carriers, whose root may hold a prompt that
	// named them before they arrived.
	carriers []subagentRow
	// relive names the anchors whose agents the write may have started
	// or stopped (subagentCards.relive).
	relive []string
	// stops name the agents the write stopped and the transcript roots
	// they kept live.
	stops []string
	// settled and revived name the launches of the completion siblings
	// the write inserted, whose trigger settles the launch, and of those
	// it deleted or re-pointed, whose trigger may revive it: finish reads
	// each launch's transcript root and records both (launches).
	settled, revived []string
	// placed are the counted rows the write inserted, moved or made
	// counted under a parent, whose readers may read them other than the
	// stamps above them count (markPlacedAnchorsTx).
	placed []placedRow
	// stale lists the stamps finish recomputed, once it has.
	stale   []string
	touched bool
	// levelsDropped is set by a write that removed lineage rows, which
	// can release a holder: writeItems reports it once the write commits
	// (holdersMayBeReleased).
	levelsDropped bool

	// t is the thread's cards, when the writer holds their lock
	// (writeItems, bulkWriteItems): finish then settles what the write
	// leaves in memory. A write without it may stop an agent only in a
	// thread whose cards hold nothing, or in a boot sweep (sweep), which
	// runs before any card is opened.
	t     *cardThread
	sweep bool
	// live and liveAnchor are the card's liveness as the transaction read
	// it; resolved applies the chain it read.
	live       bool
	liveAnchor string
	resolved   func()
	// undo puts back what settle applied before the commit.
	undo func()
}

// check refuses a row the card does not cover: one under another parent,
// or a counted row written without a card outside a bulk writer.
func (w *cardWrite) check(row subagentRow) error {
	if w.card != nil && w.card.parentID != row.parentID {
		return fmt.Errorf("%w: %s/%s is under %q, its card under %q", ErrSubagentAnchor, w.threadID, row.id, row.parentID, w.card.parentID)
	}
	if w.card == nil && !w.bulk && row.counts() {
		return fmt.Errorf("%w: %s/%s under %s was written without its card", ErrSubagentAnchor, w.threadID, row.id, row.parentID)
	}
	return nil
}

// inserted records an inserted row. hasChild reports rows already stored
// under it, which it adopts: its card and its chain's recompute.
func (w *cardWrite) inserted(row subagentRow, hasChild bool) {
	if row.completionOf != "" {
		w.settled = append(w.settled, row.completionOf)
	}
	if row.anchorable() {
		w.relive = append(w.relive, row.root)
	}
	if row.anchorable() && row.root != "" && !hasChild {
		w.carriers = append(w.carriers, row)
	}
	if hasChild {
		if row.anchorable() {
			w.seeds = append(w.seeds, row.id)
		}
		if row.counts() {
			w.chains = append(w.chains, row.parentID)
		}
	}
	if !row.counts() {
		return
	}
	w.place(row)
	if w.card == nil {
		w.chains = append(w.chains, row.parentID)
		if row.prompt {
			w.seeds = append(w.seeds, row.carrier)
		}
		return
	}
	w.inserts = append(w.inserts, row)
}

// updated records an update from old to row: a move between cards or
// rounds recomputes both chains, a row that starts or stops anchoring or
// changes its transcript root recomputes its family, and a summary change
// of a counted preview-kind row is a note for its card.
func (w *cardWrite) updated(old, row subagentRow) error {
	if w.card != nil && w.card.parentID != row.parentID {
		return fmt.Errorf("%w: %s/%s is under %q, its card under %q", ErrSubagentAnchor, w.threadID, row.id, row.parentID, w.card.parentID)
	}
	if old.anchorable() || row.anchorable() {
		// A tool call row's status, background flag or meta decides
		// whether its agent runs, for it and for its transcript root.
		w.relive = append(w.relive, row.id, old.root, row.root)
	}
	if old.running() && (!row.running() || old.root != row.root) {
		w.stops = append(w.stops, old.id, old.root)
	}
	if old.completionOf != row.completionOf {
		w.revived = append(w.revived, old.completionOf, row.completionOf)
	}
	structural := (old.counts() || row.counts()) && (old.parentID != row.parentID ||
		old.turn != row.turn || old.index != row.index || old.visible() != row.visible() ||
		old.kind != row.kind || old.toolName != row.toolName ||
		old.prompt != row.prompt || old.carrier != row.carrier)
	if structural {
		w.chains = append(w.chains, old.parentID, row.parentID)
		w.seeds = append(w.seeds, old.carrier, row.carrier)
		if row.counts() && (!old.counts() || old.parentID != row.parentID || old.turn != row.turn || old.index != row.index) {
			w.place(row)
		}
	}
	if old.anchorable() != row.anchorable() || old.root != row.root {
		w.seeds = append(w.seeds, row.id, old.root, row.root)
		if old.anchorable() && !row.anchorable() {
			w.unanchor = append(w.unanchor, row.id)
		}
	}
	if structural || !row.counts() || old.summary == row.summary || !previewKind(row.kind) {
		return nil
	}
	switch {
	case w.card != nil:
		w.changes = append(w.changes, row)
	case w.bulk:
		w.chains = append(w.chains, row.parentID)
	default:
		return fmt.Errorf("%w: %s/%s under %s changed its summary without its card", ErrSubagentAnchor, w.threadID, row.id, row.parentID)
	}
	return nil
}

// deleted records a deleted row: its chain loses it and its subtree, a
// prompt's carrier loses its round, and its own stamp goes with it.
func (w *cardWrite) deleted(old subagentRow) {
	if old.completionOf != "" {
		w.revived = append(w.revived, old.completionOf)
	}
	if old.anchorable() {
		w.relive = append(w.relive, old.id, old.root)
	}
	if old.running() {
		w.stops = append(w.stops, old.id, old.root)
	}
	if old.counts() {
		w.chains = append(w.chains, old.parentID)
	}
	if old.prompt {
		w.seeds = append(w.seeds, old.carrier)
	}
	if old.anchorable() {
		w.seeds = append(w.seeds, old.id, old.root)
	}
}

// subtreesChanged records anchors whose subtrees the write changed without
// writing a row of theirs: a pointer fork's copied anchors when the fork
// stops showing rows it inherits (forkViewChangedTx). finish recomputes
// them with their families.
func (w *cardWrite) subtreesChanged(ids []string) {
	w.seeds = append(w.seeds, ids...)
}

// place records a counted row the write put under its parent at its
// position.
func (w *cardWrite) place(row subagentRow) {
	w.placed = append(w.placed, placedRow{ID: row.id, Parent: row.parentID, Turn: row.turn, Item: row.index})
}

// finish records the markers the write's placed rows call for
// (markPlacedAnchorsTx), recomputes what the write changed that the rules
// do not follow, then settles what it leaves in memory (settle). A write
// that recomputes anything recomputes its noted rows' chains with it, and
// its card takes no note. A writer without the cards' lock has every
// accumulator of a stamp it recomputed retired at the next card
// operation.
func (w *cardWrite) finish() error {
	placed := w.placed
	w.placed = nil
	if err := markPlacedAnchorsTx(w.tx, w.threadID, placed); err != nil {
		return err
	}
	if err := w.launches(); err != nil {
		return err
	}
	if len(w.relive) > 0 {
		w.s.cards.relive(w.threadID, w.relive)
	}
	w.relive = nil
	if err := w.recompute(); err != nil {
		return err
	}
	return w.settle()
}

// launches records the agents the write's completion siblings settle or
// revive: each launch and the transcript root it keeps live, read once
// per launch, as the cards they decide. A thread whose cards hold nothing
// has no card to tell, and reads nothing.
func (w *cardWrite) launches() error {
	settled, revived := w.settled, w.revived
	w.settled, w.revived = nil, nil
	if len(settled)+len(revived) == 0 || (w.t == nil && !w.s.cards.holds(w.threadID)) {
		return nil
	}
	read := make(map[string][]string, len(settled)+len(revived))
	agents := func(id string) ([]string, error) {
		if got, ok := read[id]; ok || id == "" {
			return got, nil
		}
		row, err := scanSubagentRow(w.tx.QueryRow(subagentRowSQL, w.threadID, id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			read[id] = []string{id}
		case err != nil:
			return nil, fmt.Errorf("store: read the launch %s/%s a completion names: %w", w.threadID, id, err)
		default:
			read[id] = []string{id, row.root}
		}
		return read[id], nil
	}
	for _, id := range settled {
		ids, err := agents(id)
		if err != nil {
			return err
		}
		w.relive = append(w.relive, ids...)
		w.stops = append(w.stops, ids...)
	}
	for _, id := range revived {
		ids, err := agents(id)
		if err != nil {
			return err
		}
		w.relive = append(w.relive, ids...)
	}
	return nil
}

// recompute is finish's recompute of what the rules do not follow.
func (w *cardWrite) recompute() error {
	// A carrier stored after the prompt that names it: the root's last
	// round has had no card (linkRounds), so the family is recomputed.
	for _, row := range w.carriers {
		var named bool
		if err := w.tx.QueryRow(subagentPromptNamesSQL, w.threadID, row.root, row.id).Scan(&named); err != nil {
			return fmt.Errorf("store: probe the prompts naming %s/%s: %w", w.threadID, row.id, err)
		}
		if named {
			w.seeds = append(w.seeds, row.id)
		}
	}
	w.carriers = nil
	if len(w.chains) == 0 && len(w.seeds) == 0 && len(w.unanchor) == 0 {
		return nil
	}
	w.touched = true
	for _, row := range append(w.inserts, w.changes...) {
		w.chains = append(w.chains, row.parentID)
		if row.prompt {
			w.seeds = append(w.seeds, row.carrier)
		}
	}
	w.inserts, w.changes = nil, nil
	// A writer whose own writes advanced the thread stamp needs no bump.
	bump := func() error { return nil }
	if w.bump {
		bump = subagentBumpOnce(w.tx, w.threadID)
	}
	stale, err := w.s.recomputeSubagentChainsTx(w.tx, w.threadID, w.chains, w.seeds, w.unanchor, bump)
	if err != nil {
		return err
	}
	w.chains, w.seeds, w.unanchor = nil, nil, nil
	w.stale = append(w.stale, stale...)
	if w.t == nil {
		w.s.cards.invalidate(w.threadID, stale)
	}
	return nil
}

// settle keeps what the write leaves in memory recoverable by the boot
// pass. The notes of a card that is not live, or whose agent the write
// stopped, and when it stopped agents the pending accumulators their
// cards reach or no card reaches (cardThread.stopped), with the thread's
// seeds, are written to their stamps in this transaction: settle applies
// the notes before the commit, and undo puts them back if it does not
// commit. The cards of agents the write did not stop keep theirs for
// their own flush. The rows advanced the thread stamp in it, so the flush
// adds no bump.
func (w *cardWrite) settle() error {
	stops := slices.DeleteFunc(w.stops, func(id string) bool { return id == "" })
	w.stops = nil
	t := w.t
	if t == nil {
		// Without the lock, only a thread whose cards hold nothing can
		// lose no accumulator: a card changes them in a transaction of
		// its own, and holds the thread's entry until it has applied them.
		if len(stops) > 0 && !w.sweep && w.s.cards.holds(w.threadID) {
			return fmt.Errorf("store: a write that stops an agent of %s holds no lock on its subagent cards", w.threadID)
		}
		return nil
	}
	noted := len(w.inserts)+len(w.changes) > 0
	// The notes of a card that stays live reach stamps its agent keeps
	// recoverable; they are applied once the write commits.
	kept := !noted || (w.live && !slices.Contains(stops, w.liveAnchor))
	if kept && (len(stops) == 0 || !t.pending(t.stopped(stops))) {
		return nil
	}
	if w.undo == nil {
		w.undo = t.snapshot(w.card)
	}
	w.apply()
	only := make(map[*cardStamp]struct{})
	if len(stops) > 0 {
		only = t.stopped(stops)
	}
	if !kept && w.card != nil {
		t.reach(w.card, only)
	}
	flushed, err := w.s.flushCardsTx(w.tx, t, only, nil, func() error { return nil })
	if err != nil {
		return err
	}
	flushed()
	return nil
}

// apply feeds the cards what the write left them, once: the chain the
// card's transaction read, then its notes, or after a recompute the
// retire of the accumulators it rewrote. It runs when the write commits,
// or before from settle, whose undo puts it back.
func (w *cardWrite) apply() {
	if w.resolved != nil {
		w.resolved()
		w.resolved = nil
	}
	if w.touched {
		w.touched = false
		w.t.retire(w.stale, true)
		w.stale = nil
		return
	}
	for _, row := range w.inserts {
		w.card.noteInsert(row)
	}
	for _, row := range w.changes {
		w.card.noteChange(row)
	}
	w.inserts, w.changes = nil, nil
}

// writeItems runs one item write transaction under the lock of the
// thread's cards. card is the card the write carries, or nil. fn records
// what it wrote through w; the transaction recomputes what the rules do
// not follow and settles what no boot pass would recover before it
// commits, and the cards take the rest once it has.
func (s *Store) writeItems(threadID string, card *SubagentCard, label string, fn func(tx *sql.Tx, w *cardWrite) error) error {
	return s.itemWriteTx(threadID, card, false, label, fn)
}

// bulkWriteItems is writeItems for a bulk writer of one thread: counted
// rows need no card, and fn recomputes every chain it touched.
func (s *Store) bulkWriteItems(threadID, label string, fn func(tx *sql.Tx, w *cardWrite) error) error {
	return s.itemWriteTx(threadID, nil, true, label, fn)
}

func (s *Store) itemWriteTx(threadID string, card *SubagentCard, bulk bool, label string, fn func(tx *sql.Tx, w *cardWrite) error) error {
	t := s.cards.acquire(threadID, true)
	t.mu.Lock()
	defer t.mu.Unlock()
	defer s.cards.release(t)
	if card != nil {
		if card.s != s || card.threadID != threadID {
			return fmt.Errorf("%w: a card of %s/%s written in %s", ErrSubagentAnchor, card.threadID, card.parentID, threadID)
		}
		// An open card keeps its thread's entry: another entry means the
		// card was closed.
		if card.t != t || card.closed {
			return fmt.Errorf("%w: the card of %s/%s is closed", ErrSubagentAnchor, card.threadID, card.parentID)
		}
	}
	w := &cardWrite{s: s, threadID: threadID, card: card, bulk: bulk, t: t, live: true}
	err := s.cardTxLocked(t, label, func(tx *sql.Tx) (func(), error) {
		w.tx = tx
		if card != nil {
			w.live, w.liveAnchor = card.live, card.liveAnchor
			if !card.resolved || card.orphan {
				var err error
				if w.resolved, w.live, w.liveAnchor, err = s.resolveCardTx(tx, t, card); err != nil {
					return nil, err
				}
			}
		}
		if err := fn(tx, w); err != nil {
			return nil, err
		}
		if err := w.finish(); err != nil {
			return nil, err
		}
		return w.apply, nil
	})
	if err != nil && w.undo != nil {
		w.undo()
	}
	if err == nil && w.levelsDropped {
		s.holdersMayBeReleased()
	}
	return err
}

// bulkItemWrites is the cardWrite of a bulk writer's transaction that
// does not hold the lock of the thread's cards: counted rows need no
// card, and the writer calls finish before it commits. It stops an agent
// only in a thread whose cards hold nothing (settle). bump is for a
// writer whose own writes do not advance the thread stamp.
func (s *Store) bulkItemWrites(tx *sql.Tx, threadID string, bump bool) *cardWrite {
	return &cardWrite{s: s, tx: tx, threadID: threadID, bulk: true, bump: bump}
}

// sweepItemWrites is bulkItemWrites for a boot sweep of every thread,
// which stops agents: it runs before any card is opened, once
// FlushAllSubagentCards has written any the store holds.
func (s *Store) sweepItemWrites(tx *sql.Tx, threadID string) *cardWrite {
	return &cardWrite{s: s, tx: tx, threadID: threadID, bulk: true, sweep: true}
}
