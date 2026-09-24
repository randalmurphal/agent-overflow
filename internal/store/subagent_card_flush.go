package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Flush.

var (
	// flushSubagentStampSQL writes an accumulator back to its stamp row
	// while the row is at the generation and state it was read at.
	flushSubagentStampSQL = `UPDATE subagent_aggregates SET ` +
		strings.Join(subagentAggregateValueColumns, " = ?, ") + ` = ?
 WHERE thread_id = ? AND item_id = ? AND gen = ? AND state = ` + aggCleanLiteral
	// flushSubagentStampInsertSQL writes the first stamp of an anchor
	// that had none, while it has none and the row still anchors.
	flushSubagentStampInsertSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, gen, ` +
		strings.Join(subagentAggregateValueColumns, ", ") + `)
SELECT ?1, ?2, ` + aggCleanLiteral + `, ?3` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `
 WHERE EXISTS (SELECT 1 FROM items a WHERE a.thread_id = ?1 AND a.id = ?2 AND ` + aggAnchorableSQL("a.") + `)
ON CONFLICT (thread_id, item_id) DO NOTHING`
	// flushSubagentCarrierInsertSQL writes the first stamp of a carrier a
	// resume prompt opened in memory (notePrompt), while the row is still
	// the carrier of the root the last parameter names, with no stamp and
	// no child of its own.
	flushSubagentCarrierInsertSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, gen, ` +
		strings.Join(subagentAggregateValueColumns, ", ") + `)
SELECT ?1, ?2, ` + aggCleanLiteral + `, ?3` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `
 WHERE EXISTS (SELECT 1 FROM items a WHERE a.thread_id = ?1 AND a.id = ?2 AND ` + aggAnchorableSQL("a.") + `
                 AND ` + aggTranscriptRootSQL("a.") + ` = ` + carrierRootParam + `
                 AND NOT ` + aggHasLocalChildSQL("a.thread_id", "a.id", "") + `)
ON CONFLICT (thread_id, item_id) DO NOTHING`
)

// carrierRootParam is flushSubagentCarrierInsertSQL's root parameter,
// after the thread, the id, the generation and the values.
var carrierRootParam = fmt.Sprintf("?%d", 4+len(subagentAggregateValueColumns))

// flushLocked writes the thread's pending accumulators. The caller holds
// t.mu. A thread with nothing pending runs no transaction. changed, when
// not nil, receives the anchors whose stamp changed.
func (s *Store) flushLocked(t *cardThread, changed *[]string) error {
	if !t.pending() {
		return nil
	}
	return s.cardTxLocked(t, "flush subagent cards", func(tx *sql.Tx) (func(), error) {
		return s.flushCardsTx(tx, t, changed, subagentBumpOnce(tx, t.id))
	})
}

// flushCardsTx writes the thread's pending accumulators in tx. bump
// advances the thread stamp before the first stamp write; a flush inside
// an item write, whose own rows already did, passes one that does
// nothing.
func (s *Store) flushCardsTx(tx *sql.Tx, t *cardThread, changedOut *[]string, bump func() error) (func(), error) {
	ids := make([]string, 0, len(t.stamps))
	for id, st := range t.stamps {
		if st.pending() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	seeds := make([]string, 0, len(t.seeds))
	for id := range t.seeds {
		seeds = append(seeds, id)
	}
	type landed struct {
		st  *cardStamp
		gen int64
	}
	var wrote []landed
	// checked are the stamps a note reached and left as they were, whose
	// generation the flush checked: their reach is reset once it commits.
	var checked []*cardStamp
	var changed []string
	for _, id := range ids {
		st := t.stamps[id]
		unchanged := st.state == cardInert || st.values == st.stored
		switch {
		case st.state == cardRecompute:
			seeds = append(seeds, id)
		case unchanged && !st.exists && st.state == cardLive:
			checked = append(checked, st)
		case unchanged:
			want := int64(aggStateClean)
			if st.state == cardInert {
				want = aggStateReadTime
			}
			var gen, state int64
			err := tx.QueryRow(`SELECT gen, state FROM subagent_aggregates WHERE thread_id = ? AND item_id = ?`, t.id, id).Scan(&gen, &state)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				seeds = append(seeds, id)
			case err != nil:
				return nil, fmt.Errorf("store: check subagent stamp %s/%s: %w", t.id, id, err)
			case gen != st.gen || state != want:
				seeds = append(seeds, id)
			default:
				checked = append(checked, st)
			}
		default:
			if err := bump(); err != nil {
				return nil, err
			}
			var result sql.Result
			var err error
			gen := st.gen
			switch {
			case st.exists:
				args := append(st.values.args(), t.id, id, st.gen)
				result, err = tx.Exec(flushSubagentStampSQL, args...)
			case st.openedBy != "":
				gen = newSubagentGen()
				args := append(append([]any{t.id, id, gen}, st.values.args()...), st.openedBy)
				result, err = tx.Exec(flushSubagentCarrierInsertSQL, args...)
			default:
				gen = newSubagentGen()
				args := append([]any{t.id, id, gen}, st.values.args()...)
				result, err = tx.Exec(flushSubagentStampInsertSQL, args...)
			}
			if err != nil {
				return nil, fmt.Errorf("store: flush subagent stamp %s/%s: %w", t.id, id, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("store: count flushed subagent stamp %s/%s: %w", t.id, id, err)
			}
			if n == 0 {
				// The stamp moved, or the carrier a prompt opened is not
				// that root's childless carrier: the recompute decides,
				// the root's family with it.
				seeds = append(seeds, id)
				if st.openedBy != "" {
					seeds = append(seeds, st.openedBy)
				}
				continue
			}
			wrote = append(wrote, landed{st, gen})
			changed = append(changed, id)
		}
	}
	var members []subagentFamilyMember
	if len(seeds) > 0 {
		var err error
		if members, err = recomputeSubagentFamiliesTx(tx, t.id, seeds, bump); err != nil {
			return nil, err
		}
		for _, m := range members {
			if m.written {
				changed = append(changed, m.id)
			}
		}
	}
	if changedOut != nil {
		slices.Sort(changed)
		*changedOut = slices.Compact(changed)
	}
	return func() {
		for _, w := range wrote {
			w.st.stored, w.st.exists, w.st.gen, w.st.openedBy, w.st.reached = w.st.values, true, w.gen, "", false
		}
		for _, st := range checked {
			st.reached = false
		}
		fresh := make(map[string]*cardStamp, len(members))
		for _, m := range members {
			fresh[m.id] = cardStampOf(m)
		}
		var vanished []string
		for _, id := range seeds {
			if st := fresh[id]; st != nil {
				t.put(st)
			} else if t.stamps[id] != nil {
				vanished = append(vanished, id)
			}
		}
		for id, st := range fresh {
			if t.stamps[id] != nil {
				t.put(st)
			}
		}
		if len(vanished) > 0 {
			// A stamp the recompute found no anchor for: the row is gone,
			// stopped anchoring, or is a carrier a prompt named before it
			// was stored. Retired before the rounds link, so a root whose
			// last round names it links to no round card.
			t.retire(vanished, true)
		}
		for _, st := range t.stamps {
			if st.roundID != "" && t.stamps[st.roundID] == nil && fresh[st.roundID] != nil {
				t.put(fresh[st.roundID])
			}
		}
		t.linkRounds()
		clear(t.seeds)
	}, nil
}

// subagentBumpOnce advances the thread stamp before the first stamp write
// of a transaction: the stamp triggers stamp each anchor they write with
// the thread's history_rev, which must be one no reader has seen.
func subagentBumpOnce(tx *sql.Tx, threadID string) func() error {
	bumped := false
	return func() error {
		if bumped {
			return nil
		}
		if _, err := tx.Exec(bumpSubagentAggregateRevSQL, threadID); err != nil {
			return fmt.Errorf("store: bump history for subagent stamps in %s: %w", threadID, err)
		}
		bumped = true
		return nil
	}
}
