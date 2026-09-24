package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// Subagent cards kept in memory between flushes.
//
// A writer that writes rows under a parent opens a card for that parent
// (OpenSubagentCard) and passes it with every row it writes there
// (Item.SubagentCard, ItemPartialUpdate.SubagentCard). The card holds one
// accumulator per stamp the rows reach: every local anchor up the
// parent's chain whose card counts them, with a resumed root's last round
// and whole-transcript counters, and the parent's tray. After a write
// commits, the store feeds the accumulators from the row it wrote, in Go,
// by the rules below; a write costs no statement for its cards. A flush
// writes each changed accumulator back with one keyed statement,
// optimistic on the stamp's generation, and recomputes a stamp whose
// generation moved. Every card of a thread shares one set of
// accumulators, so two agents under one root add to the same count.
//
// The rules, per stamp a counted row (a visible row with a parent)
// reaches:
//   - a root without resume rounds takes the row into its card: count,
//     newest position, and the preview when the row is previewable and
//     newer than the preview it has (betterSubagentPreview);
//   - a resumed root takes the row into its transcript count and newest
//     position, and its last round's carrier takes it into its card as
//     above; a row stored before that round's prompt makes the root
//     recompute at the next flush;
//   - a resume prompt directly under a root opens its next round
//     (notePrompt): the root keeps its count and takes the prompt into
//     its transcript, and the carrier the prompt names opens its card
//     with the prompt; a prompt stored before a row the root took, one
//     naming the root or a carrier the thread holds, and one under a
//     stamp that is not live recompute at the next flush. A prompt's
//     carrier counts only when the named row is anchorable and its
//     transcript root is the prompt's parent; otherwise the prompt cuts
//     the round with no card, and a prompt naming its root leaves its
//     family readTime (subagentResumeRounds, computeSubagentFamilies);
//   - the parent takes a toolable row into its tray when it is newer than
//     the tray row;
//   - a changed summary of the preview row or the tray row replaces it
//     while the row still qualifies, and otherwise makes that stamp
//     recompute; a changed summary of another row takes the preview or
//     the tray when it now qualifies and is newer.
//
// A carrier's own children count toward no card of the carrier, only
// toward its tray. A stamp the recompute left readTime is walked by every
// read and takes no value. A row that reaches a stamp and leaves its
// values, readTime or not, makes the next flush check that no recompute
// changed the stamp since: a writer that moves rows into the thread
// rewrites stamps outside the cards (recomputeLocalizedCardsTx). The item
// triggers serve every card a row changes anew: the anchors on its chain,
// and the carriers their prompts name, whose transcript root is on that
// chain.
//
// A write the rules do not follow recomputes the stamps it changed in its
// own transaction (recomputeSubagentChainsTx): a row inserted after rows
// already written under it, a carrier stored after the prompt that names
// it, a change of a row's parent, position, visibility, kind, tool or
// resume prompt identity, a row that starts or stops anchoring or changes
// its transcript root, a delete, and every bulk writer. Such a recompute
// retires the accumulators it covered and makes every card of the thread
// read its chain again at its next write.
//
// A card is exact at each flush; between flushes a read serves the stamp
// of the last one. A crash loses the rows written since the last flush
// from the stamps; the boot pass (RecoverSubagentCards, which
// RecoverCrashedTurns runs first) recomputes the stamps of every agent
// that was running. What the cards hold must stay within its reach
// (cardWrite.settle): a write with a card no running agent covers (live),
// and a write that stops an agent while a card it kept live holds rows no
// other live card reaches, flush the thread's cards in their own
// transaction. Whether a card is live is read when it resolves its chain;
// a write that can end or start an agent (a completion sibling, a change
// to a tool call row, a teardown) makes the cards it concerns resolve
// again (subagentCards.relive). A write that can stop an agent holds the
// lock of the thread's cards (writeItems, bulkWriteItems).

// ErrSubagentAnchor reports a write whose subagent card the store cannot
// accept: a visible row with a parent written without a card outside a
// bulk writer, a card opened for another parent or thread, or a closed
// card.
var ErrSubagentAnchor = errors.New("store: invalid subagent card")

// SubagentCard is a writer's handle on the cards the rows under one
// parent count toward. It is safe for concurrent use and valid until
// Close. The rows it counts reach the stored stamps at each flush (Flush,
// FlushSubagentCards) and at Close.
type SubagentCard struct {
	s        *Store
	t        *cardThread
	threadID string
	parentID string

	// Guarded by t.mu.
	closed   bool
	resolved bool
	// orphan reports a parent not stored yet: its rows count toward
	// nothing, and the card reads the chain again at every write until
	// the parent arrives.
	orphan bool
	// levels are the local anchors up the parent's chain whose card
	// counts the rows, parent first; tray is the parent's stamp when the
	// parent is a local anchorable row.
	levels []*cardStamp
	tray   *cardStamp
	// live reports that the boot pass would recover the stamps the rows
	// reach: the nearest anchor on the parent's chain, liveAnchor, is a
	// running agent or the transcript root of one (subagentCardLiveSQL).
	// A card with no anchor reaches no stamp and is live.
	live       bool
	liveAnchor string
}

// ThreadID is the thread the card was opened in.
func (c *SubagentCard) ThreadID() string { return c.threadID }

// ParentID is the parent of the rows the card counts.
func (c *SubagentCard) ParentID() string { return c.parentID }

// OpenSubagentCard opens the card for rows written under parentID. It
// reads the parent's chain once, makes an imported anchor on it local,
// and seeds an accumulator per stamp the rows reach from the stored stamp
// rows, recomputing one that is dirty or predates the stamps. A parent
// not stored yet gives a card that counts nothing until it arrives.
func (s *Store) OpenSubagentCard(threadID, parentID string) (*SubagentCard, error) {
	if threadID == "" || parentID == "" {
		return nil, fmt.Errorf("%w: a card needs a thread and a parent, got %q/%q", ErrSubagentAnchor, threadID, parentID)
	}
	t := s.cards.acquire(threadID, true)
	c := &SubagentCard{s: s, t: t, threadID: threadID, parentID: parentID}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handles == nil {
		t.stamps, t.handles, t.seeds = make(map[string]*cardStamp), make(map[*SubagentCard]struct{}), make(map[string]struct{})
	}
	t.handles[c] = struct{}{}
	if err := s.cardTxLocked(t, "open subagent card", func(tx *sql.Tx) (func(), error) {
		apply, _, _, err := s.resolveCardTx(tx, t, c)
		return apply, err
	}); err != nil {
		delete(t.handles, c)
		s.cards.release(t)
		return nil, err
	}
	return c, nil
}

// WithSubagentCard opens the card for parentID, runs fn with it, and
// closes it, which flushes what fn wrote. For a one-off writer.
func (s *Store) WithSubagentCard(threadID, parentID string, fn func(*SubagentCard) error) error {
	card, err := s.OpenSubagentCard(threadID, parentID)
	if err != nil {
		return err
	}
	return errors.Join(fn(card), card.Close())
}

// Flush writes the thread's changed accumulators to the stamps. It is
// FlushSubagentCards for the card's thread.
func (c *SubagentCard) Flush() ([]string, error) {
	return c.s.FlushSubagentCards(c.threadID)
}

// Close flushes the thread's changed accumulators and releases the card.
// A failed flush keeps them for the next FlushSubagentCards and is
// returned; the card is closed either way.
func (c *SubagentCard) Close() error {
	t := c.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	delete(t.handles, c)
	err := c.s.flushLocked(t, nil)
	t.collect()
	c.s.cards.release(t)
	return err
}

// FlushSubagentCards writes the thread's changed accumulators to their
// stamps, recomputing any stamp that moved since it was read, and returns
// the anchors whose stamp changed. A thread with nothing pending costs
// no statement.
func (s *Store) FlushSubagentCards(threadID string) ([]string, error) {
	t := s.cards.acquire(threadID, false)
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var changed []string
	err := s.flushLocked(t, &changed)
	t.collect()
	s.cards.release(t)
	return changed, err
}

// FlushAllSubagentCards flushes every thread's accumulators: the store is
// about to close.
func (s *Store) FlushAllSubagentCards() error {
	s.cards.mu.Lock()
	threadIDs := make([]string, 0, len(s.cards.threads))
	for id := range s.cards.threads {
		threadIDs = append(threadIDs, id)
	}
	s.cards.mu.Unlock()
	slices.Sort(threadIDs)
	var errs []error
	for _, id := range threadIDs {
		if _, err := s.FlushSubagentCards(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SubagentCardPending reports whether the anchor's stamp holds changes
// no flush has written. It reads memory only.
func (s *Store) SubagentCardPending(threadID, anchorID string) bool {
	t := s.cards.acquire(threadID, false)
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stamps[anchorID]
	pending := st != nil && st.pending()
	s.cards.release(t)
	return pending
}

// cardTxLocked runs one card transaction: drain, fn, commit, the report
// of the pointer forks it moved (fork_moves.go), and fn's apply once
// committed. The caller holds t.mu.
func (s *Store) cardTxLocked(t *cardThread, label string, fn func(tx *sql.Tx) (func(), error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin %s in %s: %w", label, t.id, err)
	}
	defer tx.Rollback()
	defer dropForkMovesTx(tx)
	s.cards.drain(t)
	apply, err := fn(tx)
	if err != nil {
		return err
	}
	if err := s.commitReportingForks(tx); err != nil {
		return fmt.Errorf("store: commit %s in %s: %w", label, t.id, err)
	}
	if apply != nil {
		apply()
	}
	return nil
}

// subagentChainRow is one row on a parent chain, read over every arm.
// imported reports a row outside the local overlay: imported history, or
// a row a pointer fork reads from an ancestor.
type subagentChainRow struct {
	id, parentID, kind, toolName string
	imported, visible            bool
}

// subagentChainTx reads the logical parent chain from fromID upward, by
// primary key, fromID first. With visibleOnly the walk stops above the
// first hidden row, which a descendant walk does not pass. A missing row
// ends it.
func subagentChainTx(q sqlQueryer, threadID, fromID string, visibleOnly bool) ([]subagentChainRow, error) {
	var chain []subagentChainRow
	seen := make(map[string]bool)
	for id, depth := fromID, 0; id != "" && depth < 64 && !seen[id]; depth++ {
		seen[id] = true
		row := subagentChainRow{id: id}
		query, args, err := timelineArms(q, threadID, timelineSelection{
			Columns: func(_, rev string) string {
				return "items.parent_id, items.kind, items.tool_name, " + rev + " < 0, " + visibleItemsFilterFor("items.")
			},
			KeyFirst: true,
			Where:    "items.id = ?", WhereArgs: []any{id},
		})
		if err != nil {
			return nil, err
		}
		err = q.QueryRow(query, args...).Scan(&row.parentID, &row.kind, &row.toolName, &row.imported, &row.visible)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("store: walk subagent chain %s/%s: %w", threadID, id, err)
		}
		chain = append(chain, row)
		if visibleOnly && !row.visible {
			break
		}
		id = row.parentID
	}
	return chain, nil
}

// resolveCardTx reads the card's chain, seeds the accumulators it
// reaches that the thread does not hold yet, and reads whether the card
// is live. It returns the change to the registry, applied once the
// transaction commits, so a rollback leaves the registry as it was, and
// the card's liveness and the anchor that decides it.
func (s *Store) resolveCardTx(tx *sql.Tx, t *cardThread, c *SubagentCard) (func(), bool, string, error) {
	chain, err := subagentChainTx(tx, c.threadID, c.parentID, true)
	if err != nil {
		return nil, false, "", err
	}
	if len(chain) == 0 {
		return func() {
			c.levels, c.tray, c.orphan, c.resolved = nil, nil, true, true
			c.live, c.liveAnchor = true, ""
		}, true, "", nil
	}
	var anchors []string
	for _, row := range chain {
		if !SubagentAnchorable(row.kind, row.toolName) {
			continue
		}
		if row.imported {
			// An anchor outside the local overlay holds no stamp: an
			// imported one is localized, and one a pointer fork reads from
			// an ancestor is copied (shadowInheritedItemTx). Every anchor on
			// a resolved chain is then local, so no later copy can put a
			// stamp on the chain that no accumulator keeps.
			localized, err := localizeImportedItemTx(tx, c.threadID, row.id, "store: open subagent card")
			if err != nil {
				return nil, false, "", err
			}
			if !localized {
				if _, err := shadowInheritedItemTx(tx, c.threadID, row.id); err != nil {
					return nil, false, "", err
				}
			}
		}
		anchors = append(anchors, row.id)
	}

	fresh := make(map[string]*cardStamp)
	held := func(id string) *cardStamp {
		if st := fresh[id]; st != nil {
			return st
		}
		return t.stamps[id]
	}
	var missing []string
	for _, id := range anchors {
		if t.stamps[id] == nil {
			missing = append(missing, id)
		}
	}
	targets, err := subagentStampTargets(tx, c.threadID, missing)
	if err != nil {
		return nil, false, "", err
	}
	var recompute, liveRoots []string
	for _, id := range missing {
		target, ok := targets[id]
		if !ok {
			continue
		}
		st, err := seedCardStamp(tx, c.threadID, target)
		if err != nil {
			return nil, false, "", err
		}
		fresh[id] = st
		switch {
		case st.state == cardRecompute:
			recompute = append(recompute, id)
		case st.state == cardLive && !st.carrier:
			liveRoots = append(liveRoots, id)
		}
	}
	// A live root's rounds: its last round takes the rows, and a stamp
	// that disagrees with the prompts recomputes.
	if len(liveRoots) > 0 {
		rounds, err := subagentResumeRounds(tx, c.threadID, liveRoots)
		if err != nil {
			return nil, false, "", err
		}
		last := make(map[string]subagentRound, len(liveRoots))
		for _, round := range rounds {
			last[round.rootID] = round
		}
		for _, id := range liveRoots {
			st := fresh[id]
			round, resumed := last[id]
			consistent := resumed == st.stored.TranscriptCount.Valid
			if resumed && (round.imported || round.anchorID == id) {
				consistent = false
			}
			if resumed && consistent && round.anchorID != round.promptID {
				named := held(round.anchorID)
				if named == nil {
					targets, err := subagentStampTargets(tx, c.threadID, []string{round.anchorID})
					if err != nil {
						return nil, false, "", err
					}
					if target, ok := targets[round.anchorID]; ok && target.root == id {
						if named, err = seedCardStamp(tx, c.threadID, target); err != nil {
							return nil, false, "", err
						}
						fresh[round.anchorID] = named
					}
				}
				consistent = named != nil && named.carrier && named.state != cardRecompute
			}
			if !consistent {
				st.state = cardRecompute
				recompute = append(recompute, id)
				continue
			}
			if resumed {
				st.rounds = true
				st.lo = TimelineCursor{TurnIndex: round.turnIndex, ItemIndex: round.itemIndex}
				if round.anchorID != round.promptID {
					st.roundID = round.anchorID
				}
			}
		}
	}
	if len(recompute) > 0 {
		members, err := recomputeSubagentFamiliesTx(tx, c.threadID, recompute, nil)
		if err != nil {
			return nil, false, "", err
		}
		for _, id := range recompute {
			delete(fresh, id)
		}
		for _, m := range members {
			fresh[m.id] = cardStampOf(m)
		}
	}
	live, liveAnchor := true, ""
	if len(anchors) > 0 {
		liveAnchor = anchors[0]
		if err := tx.QueryRow(subagentCardLiveSQL, c.threadID, liveAnchor).Scan(&live); err != nil {
			return nil, false, "", fmt.Errorf("store: probe the agents under %s/%s: %w", c.threadID, liveAnchor, err)
		}
	}
	return func() {
		for _, st := range fresh {
			t.put(st)
		}
		t.linkRounds()
		c.levels, c.tray = nil, nil
		for i, id := range anchors {
			st := t.stamps[id]
			if st == nil {
				continue
			}
			if i == 0 && id == c.parentID {
				c.tray = st
			}
			if !st.carrier {
				c.levels = append(c.levels, st)
			}
		}
		c.orphan, c.resolved = false, true
		c.live, c.liveAnchor = live, liveAnchor
	}, live, liveAnchor, nil
}
